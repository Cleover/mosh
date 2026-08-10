package webapi

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var errLibraryRefreshInProgress = errors.New("library refresh is already in progress")

// libraryStatus is intentionally small: the browser receives only the one
// collection it is displaying, while the terminal can report useful progress
// after an explicit refresh.
type libraryStatus struct {
	Tracks      int       `json:"tracks"`
	Albums      int       `json:"albums"`
	Artists     int       `json:"artists"`
	RefreshedAt time.Time `json:"refreshedAt"`
}

// libraryCache is the web player's read model of the Plex library. Keeping a
// complete snapshot here means browsing never turns a fast scroll into a burst
// of Plex requests. A refresh builds a new snapshot first, then swaps it in as
// one operation so readers either see the old complete library or the new one.
type libraryCache struct {
	mu         sync.RWMutex
	ready      bool
	refreshing bool

	tracks         []Track
	albums         []publicAlbum
	artists        []publicArtist
	tracksByID     map[string]Track
	tracksByAlbum  map[string][]Track
	tracksByArtist map[string][]Track
	refreshedAt    time.Time
}

func newLibraryCache() *libraryCache {
	return &libraryCache{
		tracksByID:     make(map[string]Track),
		tracksByAlbum:  make(map[string][]Track),
		tracksByArtist: make(map[string][]Track),
	}
}

func (a *API) refreshLibrary() (libraryStatus, error) {
	cache := a.library
	if cache == nil {
		return libraryStatus{}, errors.New("library cache is unavailable")
	}

	cache.mu.Lock()
	if cache.refreshing {
		cache.mu.Unlock()
		return libraryStatus{}, errLibraryRefreshInProgress
	}
	cache.refreshing = true
	cache.mu.Unlock()

	defer func() {
		cache.mu.Lock()
		cache.refreshing = false
		cache.mu.Unlock()
	}()

	// These calls are deliberately serial. Refreshing may be a few dozen Plex
	// pages for a large library, but it is a rare administrative action and
	// should not compete with playback or trip Plex's request limits.
	rawArtists, err := a.plex.GetAllArtists()
	if err != nil {
		return libraryStatus{}, err
	}
	rawAlbums, err := a.plex.GetAllAlbums()
	if err != nil {
		return libraryStatus{}, err
	}
	// Plex only exposes these classifications through its music-format
	// filters, not on the ordinary all-albums response. Resolve them once here
	// and cache the resulting IDs so opening an artist never triggers Plex
	// requests in the browser.
	singleAlbums, err := a.plex.GetAllAlbumsByPlexFilter("format=EP%2CSingle")
	if err != nil {
		return libraryStatus{}, fmt.Errorf("load Plex singles and EPs: %w", err)
	}
	soundtrackAlbums, err := a.plex.GetAllAlbumsByPlexFilter("format!=EP%2CSingle&subformat=Soundtrack")
	if err != nil {
		return libraryStatus{}, fmt.Errorf("load Plex soundtracks: %w", err)
	}
	compilationAlbums, err := a.plex.GetAllAlbumsByPlexFilter("format!=EP%2CSingle&subformat=Compilation&subformat!=Live%2CSoundtrack")
	if err != nil {
		return libraryStatus{}, fmt.Errorf("load Plex compilations: %w", err)
	}
	rawTracks, err := a.plex.GetAllTracks()
	if err != nil {
		return libraryStatus{}, err
	}

	artists := publicArtists(rawArtists)
	albumCategories := make(map[string]string, len(singleAlbums)+len(soundtrackAlbums)+len(compilationAlbums))
	for _, album := range singleAlbums {
		albumCategories[album.RatingKey] = "singles"
	}
	for _, album := range soundtrackAlbums {
		albumCategories[album.RatingKey] = "soundtracks"
	}
	for _, album := range compilationAlbums {
		albumCategories[album.RatingKey] = "compilations"
	}

	albums := publicAlbums(rawAlbums, albumCategories)
	tracks := normalizeTracks(rawTracks)
	sort.Slice(artists, func(i, j int) bool {
		return compareLibraryText(artists[i].Title, artists[j].Title, artists[i].ID, artists[j].ID)
	})
	sort.Slice(albums, func(i, j int) bool {
		return compareLibraryText(albums[i].Title, albums[j].Title, albums[i].ID, albums[j].ID)
	})
	sort.Slice(tracks, func(i, j int) bool {
		return compareLibraryText(tracks[i].Title, tracks[j].Title, tracks[i].ID, tracks[j].ID)
	})

	tracksByID := make(map[string]Track, len(tracks))
	tracksByAlbum := make(map[string][]Track)
	tracksByArtist := make(map[string][]Track)
	for _, track := range tracks {
		tracksByID[track.ID] = track
		if track.AlbumID != "" {
			tracksByAlbum[track.AlbumID] = append(tracksByAlbum[track.AlbumID], track)
		}
		if track.ArtistID != "" {
			tracksByArtist[track.ArtistID] = append(tracksByArtist[track.ArtistID], track)
		}
	}
	for albumID := range tracksByAlbum {
		sort.SliceStable(tracksByAlbum[albumID], func(i, j int) bool {
			left, right := tracksByAlbum[albumID][i], tracksByAlbum[albumID][j]
			leftDisc, rightDisc := left.DiscIndex, right.DiscIndex
			if leftDisc <= 0 {
				leftDisc = 1
			}
			if rightDisc <= 0 {
				rightDisc = 1
			}
			if leftDisc != rightDisc {
				return leftDisc < rightDisc
			}
			if left.TrackIndex > 0 && right.TrackIndex > 0 && left.TrackIndex != right.TrackIndex {
				return left.TrackIndex < right.TrackIndex
			}
			return compareLibraryText(left.Title, right.Title, left.ID, right.ID)
		})
	}
	refreshedAt := time.Now().UTC()
	status := libraryStatus{Tracks: len(tracks), Albums: len(albums), Artists: len(artists), RefreshedAt: refreshedAt}

	cache.mu.Lock()
	cache.tracks = tracks
	cache.albums = albums
	cache.artists = artists
	cache.tracksByID = tracksByID
	cache.tracksByAlbum = tracksByAlbum
	cache.tracksByArtist = tracksByArtist
	cache.refreshedAt = refreshedAt
	cache.ready = true
	cache.mu.Unlock()
	if a.waveforms != nil {
		a.waveforms.reset()
	}

	return status, nil
}

func (a *API) sessionLibrary(w http.ResponseWriter, r *http.Request, member Member) {
	if !member.Host && !member.Permissions.CanLibrary {
		respondError(w, http.StatusForbidden, "library permission required")
		return
	}
	if a.library == nil {
		respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
		return
	}

	switch r.URL.Query().Get("kind") {
	case "all":
		tracks, albums, artists, status, ready := a.library.all()
		if !ready {
			respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
			return
		}
		respond(w, http.StatusOK, map[string]any{
			"tracks": publicTracks(tracks), "albums": albums, "artists": artists, "refreshedAt": status.RefreshedAt,
		})
	case "tracks":
		tracks, status, ready := a.library.allTracks()
		if !ready {
			respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
			return
		}
		respond(w, http.StatusOK, map[string]any{"tracks": publicTracks(tracks), "refreshedAt": status.RefreshedAt})
	case "albums":
		albums, status, ready := a.library.allAlbums()
		if !ready {
			respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
			return
		}
		respond(w, http.StatusOK, map[string]any{"albums": albums, "refreshedAt": status.RefreshedAt})
	case "artists":
		artists, status, ready := a.library.allArtists()
		if !ready {
			respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
			return
		}
		respond(w, http.StatusOK, map[string]any{"artists": artists, "refreshedAt": status.RefreshedAt})
	default:
		respondError(w, http.StatusBadRequest, "library kind must be all, tracks, albums, or artists")
	}
}

func (c *libraryCache) searchTracks(query string) ([]Track, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil, false
	}
	items := make([]Track, 0, 50)
	for _, track := range c.tracks {
		if matchesLibraryQuery(query, track.Title, track.Artist, track.Album) {
			items = append(items, track)
			if len(items) == cap(items) {
				break
			}
		}
	}
	return items, true
}

func (c *libraryCache) searchAlbums(query string) ([]publicAlbum, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil, false
	}
	items := make([]publicAlbum, 0, 50)
	for _, album := range c.albums {
		if matchesLibraryQuery(query, album.Title, album.Artist) {
			items = append(items, album)
			if len(items) == cap(items) {
				break
			}
		}
	}
	return items, true
}

func (c *libraryCache) searchArtists(query string) ([]publicArtist, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil, false
	}
	items := make([]publicArtist, 0, 50)
	for _, artist := range c.artists {
		if matchesLibraryQuery(query, artist.Title) {
			items = append(items, artist)
			if len(items) == cap(items) {
				break
			}
		}
	}
	return items, true
}

func (c *libraryCache) track(id string) (Track, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return Track{}, false
	}
	track, found := c.tracksByID[id]
	return track, found
}

func (c *libraryCache) tracksForAlbum(id string) ([]Track, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil, false
	}
	return append([]Track(nil), c.tracksByAlbum[id]...), true
}

func (c *libraryCache) tracksForArtist(id string) ([]Track, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil, false
	}
	return append([]Track(nil), c.tracksByArtist[id]...), true
}

func (c *libraryCache) allTracks() ([]Track, libraryStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Track(nil), c.tracks...), c.statusLocked(), c.ready
}

// all returns one internally consistent snapshot for the unified search UI.
// It is still served from the local cache, so typing never causes Plex calls.
func (c *libraryCache) all() ([]Track, []publicAlbum, []publicArtist, libraryStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Track(nil), c.tracks...), append([]publicAlbum(nil), c.albums...), append([]publicArtist(nil), c.artists...), c.statusLocked(), c.ready
}

// randomTracks draws from the in-memory library snapshot. Shuffle never asks
// Plex at playback time, which keeps it as cheap as normal queue navigation.
func (c *libraryCache) randomTracks(excluded map[string]struct{}, count int) []Track {
	if count <= 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return nil
	}
	candidates := make([]Track, 0, len(c.tracks))
	for _, track := range c.tracks {
		if track.PartPath == "" {
			continue
		}
		if _, used := excluded[track.ID]; used {
			continue
		}
		candidates = append(candidates, track)
	}
	if len(candidates) == 0 {
		return nil
	}
	if count > len(candidates) {
		count = len(candidates)
	}
	permutation := rand.Perm(len(candidates))
	result := make([]Track, 0, count)
	for _, index := range permutation[:count] {
		result = append(result, candidates[index])
	}
	return result
}

func (c *libraryCache) allAlbums() ([]publicAlbum, libraryStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]publicAlbum(nil), c.albums...), c.statusLocked(), c.ready
}

func (c *libraryCache) allArtists() ([]publicArtist, libraryStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]publicArtist(nil), c.artists...), c.statusLocked(), c.ready
}

func (c *libraryCache) statusLocked() libraryStatus {
	return libraryStatus{Tracks: len(c.tracks), Albums: len(c.albums), Artists: len(c.artists), RefreshedAt: c.refreshedAt}
}

func compareLibraryText(left, right, leftID, rightID string) bool {
	left, right = strings.ToLower(left), strings.ToLower(right)
	if left == right {
		return leftID < rightID
	}
	return left < right
}

func matchesLibraryQuery(query string, values ...string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}
