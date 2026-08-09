package webapi

import "testing"

func TestWaveformCacheCachesAvailableAndUnavailableResults(t *testing.T) {
	cache := newWaveformCache()
	loads := 0
	load := func() ([]float64, bool) {
		loads++
		return []float64{-18.2, -11.4}, true
	}

	levels, available := cache.get("track", load)
	if !available || len(levels) != 2 || loads != 1 {
		t.Fatalf("first waveform result = %#v, %t after %d loads", levels, available, loads)
	}
	levels[0] = 0
	levels, available = cache.get("track", load)
	if !available || levels[0] != -18.2 || loads != 1 {
		t.Fatalf("cached waveform result = %#v, %t after %d loads", levels, available, loads)
	}

	missingLoads := 0
	missing := func() ([]float64, bool) {
		missingLoads++
		return nil, false
	}
	_, available = cache.get("missing", missing)
	if available {
		t.Fatal("missing waveform was reported as available")
	}
	_, available = cache.get("missing", missing)
	if available || missingLoads != 1 {
		t.Fatalf("missing waveform was not cached; available %t loads %d", available, missingLoads)
	}
}
