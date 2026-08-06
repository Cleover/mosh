package webapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStoreSnapshotIsIndependent(t *testing.T) {
	store := &Store{sessions: map[string]*Session{
		"room": {
			ID: "room", Queue: []Track{{ID: "track"}}, Members: map[string]Member{"host": {ID: "host", Username: "Host"}},
		},
	}}
	copy, ok := store.snapshot("room")
	if !ok {
		t.Fatal("expected session snapshot")
	}
	copy.Queue[0].Title = "changed"
	copy.Members["host"] = Member{ID: "host", Username: "Changed"}
	if store.sessions["room"].Queue[0].Title != "" || store.sessions["room"].Members["host"].Username != "Host" {
		t.Fatal("snapshot mutated stored session")
	}
}

func TestAdminSessionViewFlattensPublicFieldsOnly(t *testing.T) {
	view := adminSessionView{
		sessionView: sessionView{ID: "room", Queue: []publicTrack{{ID: "track"}}},
		ShareToken:  "invite-token",
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != "room" || decoded["shareToken"] != "invite-token" {
		t.Fatalf("unexpected admin view: %s", data)
	}
	if _, found := decoded["partPath"]; found {
		t.Fatal("private Plex media path leaked into API response")
	}
}

func TestSessionViewOrdersMembersStably(t *testing.T) {
	api := &API{}
	now := time.Now()
	view := api.view(&Session{Members: map[string]Member{
		"z":    {ID: "z", Username: "Zelda", LastSeen: now},
		"host": {ID: "host", Username: "Host", Host: true, LastSeen: now},
		"a":    {ID: "a", Username: "alice", LastSeen: now},
	}})
	if len(view.Members) != 3 || view.Members[0].Username != "Host" || view.Members[1].Username != "alice" || view.Members[2].Username != "Zelda" {
		t.Fatalf("members were not sorted predictably: %#v", view.Members)
	}
}

func TestSessionViewHidesInactiveMembers(t *testing.T) {
	api := &API{}
	now := time.Now()
	view := api.view(&Session{Members: map[string]Member{
		"active": {ID: "active", Username: "Active", LastSeen: now},
		"stale":  {ID: "stale", Username: "Stale", LastSeen: now.Add(-memberIdleTimeout - time.Second)},
	}})
	if len(view.Members) != 1 || view.Members[0].Username != "Active" {
		t.Fatalf("expected only active listeners, got %#v", view.Members)
	}
}

func TestSessionPositionAdvancesOnlyWhilePlaying(t *testing.T) {
	now := time.Now()
	session := Session{IsPlaying: true, PositionMS: 1000, PositionAt: now.Add(-1500 * time.Millisecond)}
	if got := session.position(now); got < 2400 || got > 2600 {
		t.Fatalf("unexpected playing position: %d", got)
	}
	session.IsPlaying = false
	if got := session.position(now); got != 1000 {
		t.Fatalf("unexpected paused position: %d", got)
	}
}
