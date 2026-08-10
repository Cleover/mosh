package webapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moshconfig "github.com/adamrdrew/mosh/config"
	moshserver "github.com/adamrdrew/mosh/server"
)

func TestArtworkOnlyFetchesCachedArtworkForKnownItem(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/library/metadata/track-1/thumb/1" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("image"))
	}))
	defer upstream.Close()

	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	plex := moshserver.GetServer(&moshconfig.Config{Token: "token", Address: host, Port: port, Scheme: parsed.Scheme, Library: "1"})
	store := &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
		"room": {ID: "room", Members: map[string]Member{"member": {ID: "member", Username: "Guest", Permissions: Permissions{CanLibrary: true}, LastSeen: time.Now()}}},
	}}
	api := &API{
		config: AppConfig{SigningSecret: "01234567890123456789012345678901"},
		store:  store,
		plex:   plex,
		http:   upstream.Client(),
		library: &libraryCache{ready: true, artworkByKind: map[string]map[string]string{
			"track": {"track-1": "/library/metadata/track-1/thumb/1"},
		}},
	}
	cookieWriter := httptest.NewRecorder()
	api.issueCookie(cookieWriter, memberCookie, claims{Role: "member", SessionID: "room", MemberID: "member", Exp: time.Now().Add(time.Hour).Unix()})
	cookie := cookieWriter.Result().Cookies()[0]

	valid := httptest.NewRequest(http.MethodGet, "/api/art?kind=track&id=track-1", nil)
	valid.AddCookie(cookie)
	validResponse := httptest.NewRecorder()
	api.handleArtwork(validResponse, valid)
	if validResponse.Code != http.StatusOK || validResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("expected cached artwork response, got %d %q", validResponse.Code, validResponse.Header().Get("Content-Type"))
	}

	unsafe := httptest.NewRequest(http.MethodGet, "/api/art?path=%2Flibrary%2Fsections%2F1%2Fall", nil)
	unsafe.AddCookie(cookie)
	unsafeResponse := httptest.NewRecorder()
	api.handleArtwork(unsafeResponse, unsafe)
	if unsafeResponse.Code != http.StatusBadRequest {
		t.Fatalf("expected arbitrary Plex path to be rejected, got %d: %s", unsafeResponse.Code, unsafeResponse.Body.String())
	}

	unknown := httptest.NewRequest(http.MethodGet, "/api/art?kind=track&id=not-in-cache", nil)
	unknown.AddCookie(cookie)
	unknownResponse := httptest.NewRecorder()
	api.handleArtwork(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound {
		t.Fatalf("expected unknown artwork id to be rejected, got %d: %s", unknownResponse.Code, unknownResponse.Body.String())
	}
	if requests != 1 {
		t.Fatalf("unexpected upstream requests: got %d, want 1", requests)
	}
}

func TestPublicLibraryModelsDoNotExposePlexArtworkPaths(t *testing.T) {
	track := publicTrackFor(Track{ID: "track-1", Artwork: "/library/metadata/track-1/thumb/1"})
	payload, err := json.Marshal(track)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "/library/") || strings.Contains(string(payload), `"artwork"`) {
		t.Fatalf("public track leaked Plex artwork path: %s", payload)
	}
	if !track.HasArtwork {
		t.Fatal("public track should retain its artwork availability flag")
	}
}
