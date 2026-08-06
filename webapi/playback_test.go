package webapi

import (
	"path/filepath"
	"testing"
	"time"
)

func TestQueueEndClearsPlaybackAndQueue(t *testing.T) {
	now := time.Now()
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {
			ID: "room", Queue: []Track{{ID: "last", DurationMS: 1}}, CurrentIndex: 0,
			IsPlaying: true, PositionAt: now, StreamVersion: 4, Members: map[string]Member{},
		},
	}}
	api := &API{store: store, streams: NewStreamHub("unused", "320k")}

	api.advanceAtEnd("room", "last", 4, 0)

	session, ok := store.snapshot("room")
	if !ok {
		t.Fatal("expected room to remain")
	}
	if session.IsPlaying || session.CurrentIndex != -1 || len(session.Queue) != 0 || session.PositionMS != 0 {
		t.Fatalf("expected an empty idle queue after final track, got %#v", session)
	}
	if _, hasCurrent := session.current(); hasCurrent {
		t.Fatal("completed queue still has a current track")
	}
}
