package webapi

import (
	"net/http"
	"sync"
	"time"
)

const unavailableWaveformRetryAfter = 10 * time.Minute

type waveformCacheEntry struct {
	levels    []float64
	available bool
	expiresAt time.Time
}

// waveformCache coalesces a room's simultaneous requests for the same track.
// This keeps a shared room from multiplying one Plex loudness query by its
// listener count, and also caches an unavailable response for a short period.
type waveformCache struct {
	mu      sync.Mutex
	entries map[string]waveformCacheEntry
	loading map[string]chan struct{}
}

func newWaveformCache() *waveformCache {
	return &waveformCache{entries: make(map[string]waveformCacheEntry), loading: make(map[string]chan struct{})}
}

func (c *waveformCache) reset() {
	c.mu.Lock()
	c.entries = make(map[string]waveformCacheEntry)
	c.mu.Unlock()
}

func (c *waveformCache) get(trackID string, load func() ([]float64, bool)) ([]float64, bool) {
	for {
		c.mu.Lock()
		if entry, exists := c.entries[trackID]; exists && time.Now().Before(entry.expiresAt) {
			levels := append([]float64(nil), entry.levels...)
			c.mu.Unlock()
			return levels, entry.available
		}
		if done, loading := c.loading[trackID]; loading {
			c.mu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		c.loading[trackID] = done
		c.mu.Unlock()

		levels, available := load()
		if !available {
			levels = nil
		}
		entry := waveformCacheEntry{levels: append([]float64(nil), levels...), available: available, expiresAt: time.Now().Add(unavailableWaveformRetryAfter)}
		if available {
			// Loudness analysis is tied to the track file, so a successful result
			// is safe to retain until this backend restarts or the track changes.
			entry.expiresAt = time.Now().Add(24 * time.Hour)
		}

		c.mu.Lock()
		c.entries[trackID] = entry
		delete(c.loading, trackID)
		close(done)
		c.mu.Unlock()
		return append([]float64(nil), entry.levels...), entry.available
	}
}

func (a *API) waveform(w http.ResponseWriter, _ *http.Request, session *Session) {
	track, ok := session.current()
	if !ok {
		respond(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	if a.waveforms == nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "waveform cache unavailable"})
		return
	}
	levels, available := a.waveforms.get(track.ID, func() ([]float64, bool) {
		return a.plex.GetLoudnessLevels(track.ID)
	})
	respond(w, http.StatusOK, map[string]any{"trackId": track.ID, "available": available, "levels": levels})
}
