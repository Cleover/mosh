package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adamrdrew/mosh/responses"
)

func TestLibraryCacheServesSearchesAndCopies(t *testing.T) {
	cache := newLibraryCache()
	cache.ready = true
	cache.tracks = []Track{
		{ID: "track-1", Title: "Blue Sky", Artist: "The Echoes", ArtistID: "artist-1", Album: "First Light", AlbumID: "album-1", TrackIndex: 2},
		{ID: "track-2", Title: "Morning Glow", Artist: "The Echoes", ArtistID: "artist-1", Album: "First Light", AlbumID: "album-1", TrackIndex: 1},
	}
	cache.albums = []publicAlbum{{ID: "album-1", Title: "First Light", Artist: "The Echoes", ReleaseType: "album;compilation", Formats: []string{"Album", "Compilation"}}}
	cache.artists = []publicArtist{{ID: "artist-1", Title: "The Echoes"}}
	cache.tracksByID = map[string]Track{"track-1": cache.tracks[0], "track-2": cache.tracks[1]}
	cache.tracksByAlbum = map[string][]Track{"album-1": {cache.tracks[1], cache.tracks[0]}}
	cache.tracksByArtist = map[string][]Track{"artist-1": {cache.tracks[0], cache.tracks[1]}}

	tracks, ready := cache.searchTracks("echo")
	if !ready || len(tracks) != 2 {
		t.Fatalf("searchTracks() = %d items, ready %t; want 2 items and ready", len(tracks), ready)
	}
	albums, ready := cache.searchAlbums("light")
	if !ready || len(albums) != 1 || albums[0].ID != "album-1" {
		t.Fatalf("searchAlbums() = %#v, ready %t", albums, ready)
	}
	artists, ready := cache.searchArtists("echo")
	if !ready || len(artists) != 1 || artists[0].ID != "artist-1" {
		t.Fatalf("searchArtists() = %#v, ready %t", artists, ready)
	}

	albumTracks, found := cache.tracksForAlbum("album-1")
	if !found || len(albumTracks) != 2 || albumTracks[0].ID != "track-2" {
		t.Fatalf("tracksForAlbum() = %#v, found %t", albumTracks, found)
	}
	albumTracks[0].Title = "changed"
	if cache.tracksByAlbum["album-1"][0].Title == "changed" {
		t.Fatal("tracksForAlbum returned a mutable cache slice")
	}
	artistTracks, found := cache.tracksForArtist("artist-1")
	if !found || len(artistTracks) != 2 {
		t.Fatalf("tracksForArtist() = %#v, found %t", artistTracks, found)
	}
	artistTracks[0].Title = "changed"
	if cache.tracksByArtist["artist-1"][0].Title == "changed" {
		t.Fatal("tracksForArtist returned a mutable cache slice")
	}

	allTracks, allAlbums, allArtists, status, ready := cache.all()
	if !ready || len(allTracks) != 2 || len(allAlbums) != 1 || len(allArtists) != 1 {
		t.Fatalf("all() returned tracks=%d albums=%d artists=%d ready=%t; want 2, 1, 1, true", len(allTracks), len(allAlbums), len(allArtists), ready)
	}
	if status.Tracks != 2 || status.Albums != 1 || status.Artists != 1 {
		t.Fatalf("all() status = %#v; want 2 tracks, 1 album, 1 artist", status)
	}
	allTracks[0].Title = "changed again"
	allAlbums[0].Title = "changed again"
	allArtists[0].Title = "changed again"
	if cache.tracks[0].Title == "changed again" || cache.albums[0].Title == "changed again" || cache.artists[0].Title == "changed again" {
		t.Fatal("all returned mutable cache slices")
	}
}

func TestPublicAlbumsUsesCachedPlexCategory(t *testing.T) {
	albums := publicAlbums([]responses.ResponseAlbumDirectory{{RatingKey: "album-1", Title: "First Light"}}, map[string]string{"album-1": "soundtracks"})
	if len(albums) != 1 || albums[0].Category != "soundtracks" {
		t.Fatalf("publicAlbums() = %#v; expected a cached soundtrack category", albums)
	}
}

func TestSessionLibraryAllReturnsOneCachedSnapshot(t *testing.T) {
	api := &API{library: newLibraryCache()}
	api.library.ready = true
	api.library.tracks = []Track{{ID: "track-1", Title: "Blue Sky", DurationMS: 1}}
	api.library.albums = []publicAlbum{{ID: "album-1", Title: "First Light", ReleaseType: "single", Formats: []string{"Single"}}}
	api.library.artists = []publicArtist{{ID: "artist-1", Title: "The Echoes"}}

	request := httptest.NewRequest(http.MethodGet, "/api/sessions/room/library?kind=all", nil)
	recorder := httptest.NewRecorder()
	api.sessionLibrary(recorder, request, Member{ID: "guest", Permissions: Permissions{CanLibrary: true}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("all library response returned %d; want %d", recorder.Code, http.StatusOK)
	}

	var response struct {
		Tracks  []publicTrack  `json:"tracks"`
		Albums  []publicAlbum  `json:"albums"`
		Artists []publicArtist `json:"artists"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode all library response: %v", err)
	}
	if len(response.Tracks) != 1 || len(response.Albums) != 1 || len(response.Artists) != 1 {
		t.Fatalf("all library response = %#v; want one item of each kind", response)
	}
	if response.Albums[0].ReleaseType != "single" {
		t.Fatalf("all library releaseType = %q; want %q", response.Albums[0].ReleaseType, "single")
	}
	if len(response.Albums[0].Formats) != 1 || response.Albums[0].Formats[0] != "Single" {
		t.Fatalf("all library formats = %#v; want [Single]", response.Albums[0].Formats)
	}
}

func TestSessionLibraryRequiresLibraryPermission(t *testing.T) {
	api := &API{library: newLibraryCache()}
	api.library.ready = true

	request := httptest.NewRequest(http.MethodGet, "/api/sessions/room/library?kind=albums", nil)
	denied := httptest.NewRecorder()
	api.sessionLibrary(denied, request, Member{ID: "guest"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("library without permission returned %d; want %d", denied.Code, http.StatusForbidden)
	}

	allowed := httptest.NewRecorder()
	api.sessionLibrary(allowed, request, Member{ID: "guest", Permissions: Permissions{CanLibrary: true}})
	if allowed.Code != http.StatusOK {
		t.Fatalf("library with permission returned %d; want %d", allowed.Code, http.StatusOK)
	}
}
