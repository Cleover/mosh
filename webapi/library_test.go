package webapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLibraryCacheServesSearchesAndCopies(t *testing.T) {
	cache := newLibraryCache()
	cache.ready = true
	cache.tracks = []Track{
		{ID: "track-1", Title: "Blue Sky", Artist: "The Echoes", Album: "First Light", AlbumID: "album-1", TrackIndex: 2},
		{ID: "track-2", Title: "Morning Glow", Artist: "The Echoes", Album: "First Light", AlbumID: "album-1", TrackIndex: 1},
	}
	cache.albums = []publicAlbum{{ID: "album-1", Title: "First Light", Artist: "The Echoes"}}
	cache.artists = []publicArtist{{ID: "artist-1", Title: "The Echoes"}}
	cache.tracksByID = map[string]Track{"track-1": cache.tracks[0], "track-2": cache.tracks[1]}
	cache.tracksByAlbum = map[string][]Track{"album-1": {cache.tracks[1], cache.tracks[0]}}

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
