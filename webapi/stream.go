package webapi

import (
	"context"
	"io"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

type liveStream struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	trackID string
	paused  bool
	closed  bool
	subs    map[chan []byte]struct{}
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

func (h *StreamHub) Start(sessionID string, track Track, sourceURL string, startMS int64) error {
	h.Stop(sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{"-hide_banner", "-loglevel", "error"}
	if startMS > 0 {
		args = append(args, "-ss", formatSeconds(startMS))
	}
	args = append(args, "-re", "-i", sourceURL, "-vn", "-map", "0:a:0", "-c:a", "libmp3lame", "-b:a", h.bitrate, "-f", "mp3", "pipe:1")
	cmd := exec.CommandContext(ctx, h.ffmpeg, args...)
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}

	stream := &liveStream{cancel: cancel, cmd: cmd, trackID: track.ID, subs: map[chan []byte]struct{}{}}
	h.mu.Lock()
	h.streams[sessionID] = stream
	h.mu.Unlock()

	go h.fanout(sessionID, stream, output)
	return nil
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
	_ = stream.cmd.Wait()
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
	defer stream.mu.Unlock()
	if stream.paused == paused || stream.cmd.Process == nil {
		return
	}
	stream.paused = paused
	if paused {
		_ = stream.cmd.Process.Signal(syscall.SIGSTOP)
	} else {
		_ = stream.cmd.Process.Signal(syscall.SIGCONT)
	}
}

func (h *StreamHub) Subscribe(sessionID string) (<-chan []byte, func(), bool) {
	h.mu.Lock()
	stream := h.streams[sessionID]
	h.mu.Unlock()
	if stream == nil {
		return nil, nil, false
	}
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
