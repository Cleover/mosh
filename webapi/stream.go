package webapi

import (
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const streamPreloadLead = 2500 * time.Millisecond

// streamTrack is the private audio input for one queue entry. It deliberately
// keeps Plex URLs out of API responses while letting the stream runner prepare
// the following track before the current one ends.
type streamTrack struct {
	Track     Track
	SourceURL string
	StartMS   int64
}

type streamCallbacks struct {
	// Peek returns the current queue's next entry without changing playback
	// state. It is used only to warm a decoder shortly before a transition.
	Peek func(finishedTrackID string) (streamTrack, bool)
	// Advance atomically moves the room to the next entry at the real decoder
	// boundary. It returns false when the queue has ended or control changed.
	Advance func(finishedTrackID string) (streamTrack, bool)
	// Failure records an asynchronous decoder/encoder error against the room.
	Failure func(trackID string)
}

type decoderProcess struct {
	cmd    *exec.Cmd
	output io.ReadCloser
	track  streamTrack
}

type liveStream struct {
	mu           sync.Mutex
	cancel       context.CancelFunc
	encoder      *exec.Cmd
	encoderInput io.WriteCloser
	decoder      *exec.Cmd
	trackID      string
	paused       bool
	closed       bool
	subs         map[chan []byte]struct{}
}

type StreamHub struct {
	mu      sync.Mutex
	streams map[string]*liveStream
	ffmpeg  string
	bitrate string
}

func NewStreamHub(ffmpegPath, bitrate string) *StreamHub {
	return &StreamHub{streams: map[string]*liveStream{}, ffmpeg: ffmpegPath, bitrate: bitrate}
}

// Start builds a room-local radio stream. Individual tracks are decoded to a
// fixed PCM format, but one long-lived FFmpeg encoder produces the MP3 output.
// Keeping that output connection open across ordinary track boundaries is the
// important distinction from repeatedly replacing an <audio> element source.
func (h *StreamHub) Start(sessionID string, initial streamTrack, callbacks streamCallbacks) error {
	if initial.Track.ID == "" || initial.SourceURL == "" {
		return errors.New("stream track is incomplete")
	}
	h.Stop(sessionID)

	ctx, cancel := context.WithCancel(context.Background())
	encoderArgs := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
		"-c:a", "libmp3lame", "-b:a", h.bitrate,
		"-flush_packets", "1",
		"-f", "mp3", "pipe:1",
	}
	encoder := exec.CommandContext(ctx, h.ffmpeg, encoderArgs...)
	encoderInput, err := encoder.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	output, err := encoder.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := encoder.Start(); err != nil {
		cancel()
		return err
	}

	stream := &liveStream{
		cancel: cancel, encoder: encoder, encoderInput: encoderInput,
		trackID: initial.Track.ID, subs: map[chan []byte]struct{}{},
	}
	h.mu.Lock()
	h.streams[sessionID] = stream
	h.mu.Unlock()

	go h.fanout(sessionID, stream, output)
	go h.run(sessionID, ctx, stream, initial, callbacks)
	return nil
}

func (h *StreamHub) run(sessionID string, ctx context.Context, stream *liveStream, initial streamTrack, callbacks streamCallbacks) {
	defer stream.encoderInput.Close()

	current := initial
	decoder, err := h.startDecoder(ctx, current)
	if err != nil {
		h.fail(stream, callbacks, current.Track.ID)
		return
	}

	for {
		h.setDecoder(stream, decoder, current.Track.ID)
		cancelPreload, prepared := h.preloadNext(ctx, current, callbacks)
		copyErr := h.copyDecoder(stream, decoder)
		waitErr := decoder.cmd.Wait()
		h.clearDecoder(stream, decoder.cmd)

		if ctx.Err() != nil {
			cancelPreload()
			h.discardPrepared(prepared)
			return
		}
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			cancelPreload()
			h.discardPrepared(prepared)
			log.Printf("shared stream decode error for %s: %v", sessionID, copyErr)
			h.fail(stream, callbacks, current.Track.ID)
			return
		}
		if waitErr != nil {
			cancelPreload()
			h.discardPrepared(prepared)
			log.Printf("shared stream decoder exited for %s: %v", sessionID, waitErr)
			h.fail(stream, callbacks, current.Track.ID)
			return
		}

		next, ok := callbacks.Advance(current.Track.ID)
		candidate := h.takePrepared(cancelPreload, prepared)
		if !ok {
			h.stopDecoder(candidate)
			return
		}
		if candidate != nil && candidate.track.Track.ID == next.Track.ID {
			decoder = candidate
		} else {
			h.stopDecoder(candidate)
			decoder, err = h.startDecoder(ctx, next)
			if err != nil {
				h.fail(stream, callbacks, next.Track.ID)
				return
			}
		}
		current = next
	}
}

func (h *StreamHub) startDecoder(ctx context.Context, input streamTrack) (*decoderProcess, error) {
	args := []string{"-hide_banner", "-loglevel", "error"}
	if input.StartMS > 0 {
		args = append(args, "-ss", formatSeconds(input.StartMS))
	}
	args = append(args,
		"-re", "-i", input.SourceURL,
		"-vn", "-map", "0:a:0", "-ac", "2", "-ar", "48000",
		"-f", "s16le", "pipe:1",
	)
	cmd := exec.CommandContext(ctx, h.ffmpeg, args...)
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &decoderProcess{cmd: cmd, output: output, track: input}, nil
}

func (h *StreamHub) copyDecoder(stream *liveStream, decoder *decoderProcess) error {
	_, err := io.Copy(stream.encoderInput, decoder.output)
	_ = decoder.output.Close()
	return err
}

// preloadNext opens and probes the next Plex input before the current decoder
// finishes. Its PCM stdout naturally blocks after a small kernel buffer; that
// gives the transition a warm process and a little audio headroom without
// unbounded memory or moving room state ahead of the audible track change.
func (h *StreamHub) preloadNext(ctx context.Context, current streamTrack, callbacks streamCallbacks) (context.CancelFunc, <-chan *decoderProcess) {
	waitCtx, cancel := context.WithCancel(ctx)
	ready := make(chan *decoderProcess, 1)
	go func() {
		defer close(ready)
		remaining := time.Duration(current.Track.DurationMS-current.StartMS) * time.Millisecond
		if delay := remaining - streamPreloadLead; delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-waitCtx.Done():
				return
			}
		}
		next, ok := callbacks.Peek(current.Track.ID)
		if !ok || waitCtx.Err() != nil {
			return
		}
		decoder, err := h.startDecoder(ctx, next)
		if err != nil {
			return
		}
		select {
		case ready <- decoder:
		case <-ctx.Done():
			h.stopDecoder(decoder)
		}
	}()
	return cancel, ready
}

func (h *StreamHub) takePrepared(cancel context.CancelFunc, ready <-chan *decoderProcess) *decoderProcess {
	cancel()
	select {
	case decoder, open := <-ready:
		if open {
			return decoder
		}
	default:
		go h.discardPrepared(ready)
	}
	return nil
}

func (h *StreamHub) discardPrepared(ready <-chan *decoderProcess) {
	if ready == nil {
		return
	}
	for decoder := range ready {
		h.stopDecoder(decoder)
	}
}

func (h *StreamHub) stopDecoder(decoder *decoderProcess) {
	if decoder == nil {
		return
	}
	_ = decoder.output.Close()
	if decoder.cmd.Process != nil {
		_ = decoder.cmd.Process.Kill()
	}
	_ = decoder.cmd.Wait()
}

func (h *StreamHub) setDecoder(stream *liveStream, decoder *decoderProcess, trackID string) {
	stream.mu.Lock()
	stream.decoder = decoder.cmd
	stream.trackID = trackID
	paused := stream.paused
	stream.mu.Unlock()
	if paused && decoder.cmd.Process != nil {
		_ = decoder.cmd.Process.Signal(syscall.SIGSTOP)
	}
}

func (h *StreamHub) clearDecoder(stream *liveStream, decoder *exec.Cmd) {
	stream.mu.Lock()
	if stream.decoder == decoder {
		stream.decoder = nil
	}
	stream.mu.Unlock()
}

func (h *StreamHub) fail(stream *liveStream, callbacks streamCallbacks, trackID string) {
	if callbacks.Failure != nil {
		callbacks.Failure(trackID)
	}
	stream.cancel()
}

func (h *StreamHub) fanout(sessionID string, stream *liveStream, output io.ReadCloser) {
	defer output.Close()
	buffer := make([]byte, 32*1024)
	for {
		count, err := output.Read(buffer)
		if count > 0 {
			chunk := append([]byte(nil), buffer[:count]...)
			stream.mu.Lock()
			for subscriber := range stream.subs {
				select {
				case subscriber <- chunk:
				default:
					// A slow browser falls behind the radio live edge instead of
					// unboundedly buffering data or delaying everyone else.
				}
			}
			stream.mu.Unlock()
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("shared stream read error for %s: %v", sessionID, err)
			}
			break
		}
	}
	_ = stream.encoder.Wait()
	stream.mu.Lock()
	stream.closed = true
	for subscriber := range stream.subs {
		close(subscriber)
		delete(stream.subs, subscriber)
	}
	stream.mu.Unlock()
	h.mu.Lock()
	if h.streams[sessionID] == stream {
		delete(h.streams, sessionID)
	}
	h.mu.Unlock()
}

func (h *StreamHub) Stop(sessionID string) {
	h.mu.Lock()
	stream := h.streams[sessionID]
	delete(h.streams, sessionID)
	h.mu.Unlock()
	if stream != nil {
		stream.cancel()
	}
}

func (h *StreamHub) Pause(sessionID string, paused bool) {
	h.mu.Lock()
	stream := h.streams[sessionID]
	h.mu.Unlock()
	if stream == nil {
		return
	}
	stream.mu.Lock()
	if stream.paused == paused || stream.decoder == nil || stream.decoder.Process == nil {
		stream.paused = paused
		stream.mu.Unlock()
		return
	}
	stream.paused = paused
	decoder := stream.decoder
	stream.mu.Unlock()
	if paused {
		_ = decoder.Process.Signal(syscall.SIGSTOP)
	} else {
		_ = decoder.Process.Signal(syscall.SIGCONT)
	}
}

func (h *StreamHub) Subscribe(sessionID string) (<-chan []byte, func(), bool) {
	h.mu.Lock()
	stream := h.streams[sessionID]
	h.mu.Unlock()
	if stream == nil {
		return nil, nil, false
	}
	// Keep only a small, fixed jitter window. Letting a slow browser retain a
	// large backlog would make its audible track drift far behind room state.
	subscriber := make(chan []byte, 8)
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return nil, nil, false
	}
	stream.subs[subscriber] = struct{}{}
	stream.mu.Unlock()
	return subscriber, func() {
		stream.mu.Lock()
		delete(stream.subs, subscriber)
		stream.mu.Unlock()
	}, true
}

func formatSeconds(milliseconds int64) string {
	seconds := float64(milliseconds) / 1000
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}
