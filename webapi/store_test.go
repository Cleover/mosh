package webapi

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestAdminMemberKeepsStoredHostVisible(t *testing.T) {
	api := &API{store: &Store{sessions: map[string]*Session{
		"room": {
			ID: "room",
			Members: map[string]Member{
				"host": {ID: "host", Username: "Host", Host: true, LastSeen: time.Now().Add(-memberIdleTimeout - time.Second)},
			},
		},
	}}}
	admin, session, ok := api.adminMember("room")
	if !ok || !admin.Host || admin.ID != "admin" {
		t.Fatalf("expected owner credential, got %#v (ok=%v)", admin, ok)
	}
	view := api.view(session)
	if len(view.Members) != 1 || view.Members[0].ID != "host" {
		t.Fatalf("expected refreshed host in room view, got %#v", view.Members)
	}
}

func TestStoreMigratesLibraryPermissionOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	legacy, err := json.Marshal(persistedState{Sessions: map[string]*Session{
		"room": {ID: "room", Members: map[string]Member{"guest": {ID: "guest", Username: "Guest"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !store.sessions["room"].Members["guest"].Permissions.CanLibrary || store.sessions["room"].PermissionsVersion != 1 {
		t.Fatal("legacy member did not receive the library browsing default")
	}

	room := store.sessions["room"]
	guest := room.Members["guest"]
	guest.Permissions.CanLibrary = false
	room.Members["guest"] = guest
	if err := store.saveLocked(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.sessions["room"].Members["guest"].Permissions.CanLibrary {
		t.Fatal("an explicit library permission change was overwritten on reload")
	}
}

func TestUniqueSearchTermsOmitsDuplicateAndEmptySortKeys(t *testing.T) {
	terms := uniqueSearchTerms(" Romanized title ", "romanized title", "", "Romanized artist")
	if len(terms) != 2 || terms[0] != "Romanized title" || terms[1] != "Romanized artist" {
		t.Fatalf("unexpected search terms: %#v", terms)
	}
}

func TestPublicTrackIncludesLibraryNavigationIDs(t *testing.T) {
	view := publicTrackFor(Track{ID: "track", ArtistID: "artist", AlbumID: "album"})
	if view.ArtistID != "artist" || view.AlbumID != "album" {
		t.Fatalf("navigation IDs were not exposed: %#v", view)
	}
}
