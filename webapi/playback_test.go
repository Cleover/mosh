package webapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moshconfig "github.com/adamrdrew/mosh/config"
	moshserver "github.com/adamrdrew/mosh/server"
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

	if _, advanced := api.advanceStreamNext("room", "last", 4); advanced {
		t.Fatal("final track unexpectedly advanced")
	}

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

func TestNextOnFinalTrackClearsPlaybackAndQueue(t *testing.T) {
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {
			ID: "room", Queue: []Track{{ID: "last"}}, CurrentIndex: 0,
			IsPlaying: false, StreamVersion: 4, Members: map[string]Member{},
		},
	}}
	api := &API{store: store, streams: NewStreamHub("unused", "320k")}
	member := Member{ID: "host", Host: true}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", nil)

	api.control(recorder, request, store.sessions["room"], member, "next")
	if recorder.Code != http.StatusOK {
		t.Fatalf("next on final track returned %d: %s", recorder.Code, recorder.Body.String())
	}

	session, ok := store.snapshot("room")
	if !ok || session.IsPlaying || session.CurrentIndex != -1 || len(session.Queue) != 0 || session.PositionMS != 0 {
		t.Fatalf("expected next on final track to leave an empty idle queue, got %#v", session)
	}
}

func TestNextPromotesFirstShuffledTrack(t *testing.T) {
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {
			ID: "room", Queue: []Track{{ID: "now"}}, CurrentIndex: 0,
			ShuffleEnabled: true, ShuffleQueue: []Track{{ID: "suggestion"}},
			IsPlaying: false, StreamVersion: 4, Members: map[string]Member{},
		},
	}}
	api := &API{store: store, streams: NewStreamHub("unused", "320k")}
	member := Member{ID: "host", Host: true}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", nil)

	api.control(recorder, request, store.sessions["room"], member, "next")
	if recorder.Code != http.StatusOK {
		t.Fatalf("next into shuffle returned %d: %s", recorder.Code, recorder.Body.String())
	}

	session, _ := store.snapshot("room")
	if session.CurrentIndex != 1 || len(session.Queue) != 2 || session.Queue[1].ID != "suggestion" {
		t.Fatalf("expected shuffled suggestion to be promoted, got %#v", session)
	}
	if len(session.ShuffleQueue) != 0 {
		t.Fatalf("expected promoted suggestion to leave shuffled queue, got %#v", session.ShuffleQueue)
	}
}

func TestRepeatRestartsCurrentTrackAtStreamBoundary(t *testing.T) {
	now := time.Now()
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {
			ID: "room", Queue: []Track{{ID: "now", PartPath: "/library/parts/1"}}, CurrentIndex: 0,
			RepeatCurrent: true, IsPlaying: true, PositionMS: 183_000, PositionAt: now, StreamVersion: 4, Members: map[string]Member{},
		},
	}}
	plex := moshserver.GetServer(&moshconfig.Config{Token: "token", Address: "localhost", Port: "32400", Scheme: "http", Library: "1"})
	api := &API{store: store, plex: plex, streams: NewStreamHub("unused", "320k")}

	next, advanced := api.advanceStreamNext("room", "now", 4)
	if !advanced || next.Track.ID != "now" || next.SourceURL == "" {
		t.Fatalf("expected repeat to restart the current stream track, got %#v advanced=%v", next, advanced)
	}
	session, _ := store.snapshot("room")
	if !session.RepeatCurrent || session.CurrentIndex != 0 || session.PositionMS != 0 || !session.IsPlaying {
		t.Fatalf("expected repeat state to remain at the current track, got %#v", session)
	}
}

func TestQueueEditsOnlyMoveUpcomingTracks(t *testing.T) {
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {
			ID: "room", CurrentIndex: 0, Members: map[string]Member{},
			Queue: []Track{{ID: "now"}, {ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
	}}
	api := &API{store: store}
	member := Member{ID: "host", Host: true}

	reorder := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"fromIndex":1,"toIndex":4}`))
	recorder := httptest.NewRecorder()
	api.reorderQueue(recorder, reorder, store.sessions["room"], member)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reorder returned %d: %s", recorder.Code, recorder.Body.String())
	}
	session, _ := store.snapshot("room")
	if got := []string{session.Queue[0].ID, session.Queue[1].ID, session.Queue[2].ID, session.Queue[3].ID}; strings.Join(got, ",") != "now,b,c,a" {
		t.Fatalf("unexpected queue after reorder: %v", got)
	}

	remove := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"index":2}`))
	recorder = httptest.NewRecorder()
	api.removeFromQueue(recorder, remove, session, member)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove returned %d: %s", recorder.Code, recorder.Body.String())
	}
	session, _ = store.snapshot("room")
	if got := []string{session.Queue[0].ID, session.Queue[1].ID, session.Queue[2].ID}; strings.Join(got, ",") != "now,b,a" {
		t.Fatalf("unexpected queue after removal: %v", got)
	}

	cannotRemoveCurrent := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"index":0}`))
	recorder = httptest.NewRecorder()
	api.removeFromQueue(recorder, cannotRemoveCurrent, session, member)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("current-track removal returned %d", recorder.Code)
	}
}
