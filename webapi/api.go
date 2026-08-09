package webapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	moshconfig "github.com/adamrdrew/mosh/config"
	"github.com/adamrdrew/mosh/responses"
	moshserver "github.com/adamrdrew/mosh/server"
)

type API struct {
	config    AppConfig
	store     *Store
	plex      moshserver.Server
	library   *libraryCache
	waveforms *waveformCache
	streams   *StreamHub
	limits    *rateLimiter
	http      *http.Client
}

const memberIdleTimeout = 45 * time.Second

func New(config AppConfig) (*API, error) {
	plexConfig := moshconfig.GetConfig()
	if plexConfig.Token == moshconfig.UNINITIALIZED || plexConfig.Address == moshconfig.UNINITIALIZED || plexConfig.Library == moshconfig.UNINITIALIZED {
		return nil, errors.New("PLEX_TOKEN, PLEX_BASE_URL, and PLEX_LIBRARY_SECTION must be configured")
	}
	store, err := NewStore(config.DataPath)
	if err != nil {
		return nil, err
	}
	api := &API{config: config, store: store, plex: moshserver.GetServer(&plexConfig), library: newLibraryCache(), waveforms: newWaveformCache(), streams: NewStreamHub(config.FFmpegPath, config.Bitrate), limits: newRateLimiter(), http: &http.Client{Timeout: 15 * time.Second}}
	if _, err := api.refreshLibrary(); err != nil {
		log.Printf("initial Plex library cache load failed: %v", err)
	}
	return api, nil
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", requireMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/api/auth/admin/login", requireMethod(http.MethodPost, a.handleAdminLogin))
	mux.HandleFunc("/api/auth/admin/logout", requireMethod(http.MethodPost, a.handleAdminLogout))
	mux.HandleFunc("/api/auth/logout", requireMethod(http.MethodPost, a.handleLogout))
	mux.HandleFunc("/api/public/sessions", requireMethod(http.MethodGet, a.handlePublicSessions))
	mux.HandleFunc("/api/admin/library/artists", requireMethod(http.MethodGet, a.handleArtists))
	mux.HandleFunc("/api/admin/library/albums", requireMethod(http.MethodGet, a.handleAlbums))
	mux.HandleFunc("/api/admin/library/tracks", requireMethod(http.MethodGet, a.handleTracks))
	mux.HandleFunc("/api/admin/library/refresh", requireMethod(http.MethodPost, a.handleRefreshLibrary))
	mux.HandleFunc("/api/admin/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			a.handleCreateSession(w, r)
		case http.MethodGet:
			a.handleListSessions(w, r)
		default:
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
	mux.HandleFunc("/api/admin/sessions/", requireMethod(http.MethodDelete, a.handleCloseSession))
	mux.HandleFunc("/api/sessions/", a.handleSessionRoutes)
	mux.HandleFunc("/api/art", requireMethod(http.MethodGet, a.handleArtwork))
	return a.internalOnly(mux)
}

func requireMethod(expected string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handler(w, r)
	}
}

func (a *API) internalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-API-Secret")), []byte(a.config.InternalAPISecret)) != 1 {
			respondError(w, http.StatusForbidden, "proxy authentication failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limits.allow(clientIP(r)+":admin", 5) {
		respondError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	var input struct {
		Secret string `json:"secret"`
	}
	if !decode(r, &input) || subtle.ConstantTimeCompare([]byte(input.Secret), []byte(a.config.AdminSecret)) != 1 {
		respondError(w, http.StatusUnauthorized, "invalid admin secret")
		return
	}
	a.issueCookie(w, adminCookie, claims{Role: "admin", Exp: time.Now().Add(12 * time.Hour).Unix()})
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, adminCookie, a.config.SecureCookies)
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout signs a browser out of either type of room identity. Marking a
// member as departed keeps the room roster accurate immediately instead of
// waiting for the normal inactivity timeout.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if member, ok := a.readCookie(r, memberCookie); ok && member.Role == "member" {
		a.store.mu.Lock()
		if session := a.store.sessions[member.SessionID]; session != nil {
			if current, exists := session.Members[member.MemberID]; exists {
				current.LastSeen = time.Time{}
				session.Members[member.MemberID] = current
				if err := a.store.saveLocked(); err != nil {
					log.Printf("could not persist member logout: %v", err)
				}
			}
		}
		a.store.mu.Unlock()
	}
	clearCookie(w, memberCookie, a.config.SecureCookies)
	clearCookie(w, adminCookie, a.config.SecureCookies)
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) admin(r *http.Request) bool {
	c, ok := a.readCookie(r, adminCookie)
	return ok && c.Role == "admin"
}

func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !a.admin(r) {
		respondError(w, http.StatusUnauthorized, "admin authentication required")
		return false
	}
	return true
}

func (a *API) handleArtists(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	search, ok := librarySearch(w, r)
	if !ok {
		return
	}
	items, ready := a.library.searchArtists(search)
	if !ready {
		respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
		return
	}
	respond(w, http.StatusOK, map[string]any{"artists": items})
}

func (a *API) handleAlbums(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	search, ok := librarySearch(w, r)
	if !ok {
		return
	}
	items, ready := a.library.searchAlbums(search)
	if !ready {
		respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
		return
	}
	respond(w, http.StatusOK, map[string]any{"albums": items})
}

func (a *API) handleTracks(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	search, ok := librarySearch(w, r)
	if !ok {
		return
	}
	items, ready := a.library.searchTracks(search)
	if !ready {
		respondError(w, http.StatusServiceUnavailable, "library cache is not ready")
		return
	}
	respond(w, http.StatusOK, map[string]any{"tracks": publicTracks(items)})
}

func (a *API) handleRefreshLibrary(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	status, err := a.refreshLibrary()
	if err != nil {
		if errors.Is(err, errLibraryRefreshInProgress) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusBadGateway, "could not refresh Plex library cache")
		return
	}
	respond(w, http.StatusOK, status)
}

func (a *API) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var input struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Password string `json:"password"`
		Public   bool   `json:"public"`
	}
	if !decode(r, &input) || len(strings.TrimSpace(input.Name)) == 0 || !validUsername(input.Username) {
		respondError(w, http.StatusBadRequest, "name and a 2-24 character username are required")
		return
	}
	if len(input.Password) > 0 && len(input.Password) < 4 {
		respondError(w, http.StatusBadRequest, "session password must be at least 4 characters")
		return
	}
	now := time.Now()
	session := &Session{
		ID: id("session"), Name: strings.TrimSpace(input.Name), IsPublic: input.Public, ShareSecret: id("share"), CurrentIndex: -1,
		Members: map[string]Member{}, CreatedAt: now, PositionAt: now,
	}
	if input.Password != "" {
		session.PasswordHash = hashPassword(input.Password)
	}
	host := Member{ID: id("member"), Username: strings.TrimSpace(input.Username), Host: true, Permissions: Permissions{CanControl: true, CanQueue: true, CanLibrary: true}, LastSeen: now}
	session.Members[host.ID] = host
	a.store.mu.Lock()
	a.store.sessions[session.ID] = session
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to persist session")
		return
	}
	a.issueCookie(w, memberCookie, claims{Role: "member", SessionID: session.ID, MemberID: host.ID, Exp: now.Add(24 * time.Hour).Unix()})
	respond(w, http.StatusCreated, map[string]any{"session": a.view(session), "shareToken": a.shareToken(session.ID, session.ShareSecret)})
}

func (a *API) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	a.store.mu.RLock()
	defer a.store.mu.RUnlock()
	items := make([]adminSessionView, 0, len(a.store.sessions))
	for _, session := range a.store.sessions {
		items = append(items, adminSessionView{sessionView: a.view(session), ShareToken: a.shareToken(session.ID, session.ShareSecret)})
	}
	respond(w, http.StatusOK, map[string]any{"sessions": items})
}

// handlePublicSessions intentionally exposes only discovery information. The
// queue, stream URL, member identities, and invite secret remain private until
// someone has joined a room.
func (a *API) handlePublicSessions(w http.ResponseWriter, r *http.Request) {
	a.store.mu.RLock()
	items := make([]publicRoomView, 0, len(a.store.sessions))
	for _, session := range a.store.sessions {
		if session.IsPublic {
			items = append(items, a.publicRoom(session))
		}
	}
	a.store.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	respond(w, http.StatusOK, map[string]any{"sessions": items})
}

func (a *API) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions/")
	if sessionID == "" || strings.Contains(sessionID, "/") {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	a.store.mu.Lock()
	session := a.store.sessions[sessionID]
	if session == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	delete(a.store.sessions, sessionID)
	if err := a.store.saveLocked(); err != nil {
		a.store.sessions[sessionID] = session
		a.store.mu.Unlock()
		respondError(w, http.StatusInternalServerError, "failed to close session")
		return
	}
	a.store.mu.Unlock()
	a.streams.Stop(sessionID)
	if claim, ok := a.readCookie(r, memberCookie); ok && claim.SessionID == sessionID {
		clearCookie(w, memberCookie, a.config.SecureCookies)
	}
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleSessionRoutes(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		respondError(w, http.StatusNotFound, "not found")
		return
	}
	sessionID := parts[0]
	if len(parts) == 2 && parts[1] == "preview" && r.Method == http.MethodGet {
		a.preview(w, r, sessionID)
		return
	}
	if len(parts) == 2 && parts[1] == "join" && r.Method == http.MethodPost {
		a.join(w, r, sessionID)
		return
	}
	member, session, ok := a.member(r, sessionID)
	if !ok && a.admin(r) {
		session, ok = a.store.snapshot(sessionID)
		if ok {
			// The admin secret is an out-of-band owner credential. It may manage
			// any session without being represented as a guest member.
			member = Member{ID: "admin", Username: "Admin", Host: true, Permissions: Permissions{CanControl: true, CanQueue: true, CanLibrary: true}}
		}
	}
	if !ok {
		respondError(w, http.StatusUnauthorized, "session membership required")
		return
	}
	if len(parts) == 2 && parts[1] == "state" && r.Method == http.MethodGet {
		respond(w, http.StatusOK, map[string]any{"session": a.view(session), "you": member})
		return
	}
	if len(parts) == 2 && parts[1] == "stream" && r.Method == http.MethodGet {
		a.stream(w, r, session, member)
		return
	}
	if len(parts) == 2 && parts[1] == "library" && r.Method == http.MethodGet {
		a.sessionLibrary(w, r, member)
		return
	}
	if len(parts) == 2 && parts[1] == "waveform" && r.Method == http.MethodGet {
		a.waveform(w, r, session)
		return
	}
	if len(parts) == 3 && parts[1] == "queue" && parts[2] == "add" && r.Method == http.MethodPost {
		a.addQueue(w, r, session, member)
		return
	}
	if len(parts) == 3 && parts[1] == "queue" && parts[2] == "reorder" && r.Method == http.MethodPost {
		a.reorderQueue(w, r, session, member)
		return
	}
	if len(parts) == 3 && parts[1] == "queue" && parts[2] == "remove" && r.Method == http.MethodPost {
		a.removeFromQueue(w, r, session, member)
		return
	}
	if len(parts) == 2 && parts[1] == "leave" && r.Method == http.MethodPost {
		a.leave(w, r, session, member)
		return
	}
	if len(parts) == 3 && parts[1] == "control" && r.Method == http.MethodPost {
		a.control(w, r, session, member, parts[2])
		return
	}
	if len(parts) == 4 && parts[1] == "members" && parts[3] == "permissions" && r.Method == http.MethodPatch {
		a.permissions(w, r, session, member, parts[2])
		return
	}
	respondError(w, http.StatusNotFound, "not found")
}

func (a *API) preview(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, ok := a.store.snapshot(sessionID)
	if !ok || (!session.IsPublic && !a.validShare(sessionID, r.URL.Query().Get("t"), session.ShareSecret)) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": session.ID, "name": session.Name, "requiresPassword": session.PasswordHash != ""})
}

func (a *API) join(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !a.limits.allow(clientIP(r)+":join", 10) {
		respondError(w, http.StatusTooManyRequests, "too many join attempts")
		return
	}
	var input struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		ShareToken string `json:"shareToken"`
	}
	if !decode(r, &input) || !validUsername(input.Username) {
		respondError(w, http.StatusBadRequest, "a valid username is required")
		return
	}
	a.store.mu.Lock()
	session := a.store.sessions[sessionID]
	if session == nil || (!session.IsPublic && !a.validShare(sessionID, input.ShareToken, session.ShareSecret)) {
		a.store.mu.Unlock()
		respondError(w, http.StatusUnauthorized, "invalid share link")
		return
	}
	if session.PasswordHash != "" && !verifyPassword(input.Password, session.PasswordHash) {
		a.store.mu.Unlock()
		respondError(w, http.StatusUnauthorized, "incorrect session password")
		return
	}
	pruneInactiveMembers(session, time.Now())
	for _, existing := range session.Members {
		if strings.EqualFold(existing.Username, strings.TrimSpace(input.Username)) {
			a.store.mu.Unlock()
			respondError(w, http.StatusConflict, "username is already in use")
			return
		}
	}
	member := Member{ID: id("member"), Username: strings.TrimSpace(input.Username), Permissions: Permissions{CanLibrary: true}, LastSeen: time.Now()}
	session.Members[member.ID] = member
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save membership")
		return
	}
	a.issueCookie(w, memberCookie, claims{Role: "member", SessionID: session.ID, MemberID: member.ID, Exp: time.Now().Add(24 * time.Hour).Unix()})
	view, _ := a.store.snapshot(session.ID)
	respond(w, http.StatusOK, map[string]any{"session": a.view(view), "you": member})
}

func (a *API) member(r *http.Request, sessionID string) (Member, *Session, bool) {
	claims, ok := a.readCookie(r, memberCookie)
	if !ok || claims.Role != "member" || claims.SessionID != sessionID {
		return Member{}, nil, false
	}
	a.store.mu.Lock()
	session := a.store.sessions[sessionID]
	if session == nil {
		a.store.mu.Unlock()
		return Member{}, nil, false
	}
	member, exists := session.Members[claims.MemberID]
	if exists {
		member.LastSeen = time.Now()
		session.Members[claims.MemberID] = member
	}
	copy := cloneSession(session)
	a.store.mu.Unlock()
	return member, copy, exists
}

func (a *API) addQueue(w http.ResponseWriter, r *http.Request, session *Session, member Member) {
	if !member.Host && !member.Permissions.CanQueue {
		respondError(w, http.StatusForbidden, "queue permission required")
		return
	}
	var input struct {
		TrackID  string `json:"trackId"`
		AlbumID  string `json:"albumId"`
		ArtistID string `json:"artistId"`
	}
	if !decode(r, &input) || (input.TrackID == "" && input.AlbumID == "" && input.ArtistID == "") {
		respondError(w, http.StatusBadRequest, "trackId, albumId, or artistId is required")
		return
	}
	var tracks []Track
	if input.AlbumID != "" {
		tracks, _ = a.library.tracksForAlbum(input.AlbumID)
	} else if input.ArtistID != "" {
		tracks, _ = a.library.tracksForArtist(input.ArtistID)
	} else if track, found := a.library.track(input.TrackID); found {
		tracks = []Track{track}
	}
	if len(tracks) == 0 {
		respondError(w, http.StatusNotFound, "no playable tracks found")
		return
	}
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	_, hasCurrent := current.current()
	shouldStart := false
	var track Track
	var position, version int64
	if !hasCurrent {
		// A room with no current track is either brand new or has naturally
		// exhausted its queue. Start a fresh queue instead of retaining the
		// finished tracks, and begin streaming the first new addition.
		current.Queue = append([]Track(nil), tracks...)
		current.CurrentIndex = 0
		current.PositionMS = 0
		current.PositionAt = time.Now()
		current.IsPlaying = true
		current.StreamVersion++
		track, _ = current.current()
		position = current.PositionMS
		version = current.StreamVersion
		shouldStart = true
	} else {
		current.Queue = append(current.Queue, tracks...)
	}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save queue")
		return
	}
	if shouldStart {
		if err := a.startStream(session.ID, track, position); err != nil {
			a.pauseForStreamFailure(session.ID, version)
			respondError(w, http.StatusBadGateway, "could not start Plex stream")
			return
		}
	}
	view, _ := a.store.snapshot(current.ID)
	respond(w, http.StatusOK, map[string]any{"session": a.view(view)})
}

// reorderQueue accepts positions in the full queue. The current track and
// playback history are deliberately immutable; only upcoming tracks can move.
func (a *API) reorderQueue(w http.ResponseWriter, r *http.Request, session *Session, member Member) {
	if !member.Host && !member.Permissions.CanQueue {
		respondError(w, http.StatusForbidden, "queue permission required")
		return
	}
	var input struct {
		FromIndex int `json:"fromIndex"`
		ToIndex   int `json:"toIndex"`
	}
	if !decode(r, &input) {
		respondError(w, http.StatusBadRequest, "fromIndex and toIndex are required")
		return
	}
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if input.FromIndex <= current.CurrentIndex || input.FromIndex >= len(current.Queue) || input.ToIndex <= current.CurrentIndex || input.ToIndex > len(current.Queue) {
		a.store.mu.Unlock()
		respondError(w, http.StatusBadRequest, "only upcoming tracks can be reordered")
		return
	}
	if input.FromIndex != input.ToIndex && input.FromIndex+1 != input.ToIndex {
		queue := append([]Track(nil), current.Queue...)
		track := queue[input.FromIndex]
		queue = append(queue[:input.FromIndex], queue[input.FromIndex+1:]...)
		insertAt := input.ToIndex
		if input.FromIndex < insertAt {
			insertAt--
		}
		queue = append(queue, Track{})
		copy(queue[insertAt+1:], queue[insertAt:])
		queue[insertAt] = track
		current.Queue = queue
	}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save queue")
		return
	}
	view, _ := a.store.snapshot(session.ID)
	respond(w, http.StatusOK, map[string]any{"session": a.view(view)})
}

// removeFromQueue intentionally excludes the currently playing item. That
// keeps a drag-and-drop edit from unexpectedly interrupting the shared stream.
func (a *API) removeFromQueue(w http.ResponseWriter, r *http.Request, session *Session, member Member) {
	if !member.Host && !member.Permissions.CanQueue {
		respondError(w, http.StatusForbidden, "queue permission required")
		return
	}
	var input struct {
		Index int `json:"index"`
	}
	if !decode(r, &input) {
		respondError(w, http.StatusBadRequest, "index is required")
		return
	}
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if input.Index <= current.CurrentIndex || input.Index >= len(current.Queue) {
		a.store.mu.Unlock()
		respondError(w, http.StatusBadRequest, "only upcoming tracks can be removed")
		return
	}
	current.Queue = append(current.Queue[:input.Index], current.Queue[input.Index+1:]...)
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save queue")
		return
	}
	view, _ := a.store.snapshot(session.ID)
	respond(w, http.StatusOK, map[string]any{"session": a.view(view)})
}

func (a *API) leave(w http.ResponseWriter, r *http.Request, session *Session, member Member) {
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if _, exists := current.Members[member.ID]; !exists {
		a.store.mu.Unlock()
		respondError(w, http.StatusUnauthorized, "session membership required")
		return
	}
	member.LastSeen = time.Time{}
	current.Members[member.ID] = member
	a.store.mu.Unlock()
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) control(w http.ResponseWriter, r *http.Request, session *Session, member Member, action string) {
	if !member.Host && !member.Permissions.CanControl {
		respondError(w, http.StatusForbidden, "playback permission required")
		return
	}
	var input struct {
		PositionMS int64 `json:"positionMs"`
	}
	if action == "seek" && !decode(r, &input) {
		respondError(w, http.StatusBadRequest, "positionMs is required")
		return
	}
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	track, hasTrack := current.current()
	now := time.Now()
	shouldStart, shouldPause, stop := false, false, false
	switch action {
	case "play":
		if !hasTrack {
			a.store.mu.Unlock()
			respondError(w, http.StatusBadRequest, "queue is empty")
			return
		}
		if !current.IsPlaying {
			current.IsPlaying = true
			current.PositionAt = now
			current.StreamVersion++
			shouldStart = true
		}
	case "pause":
		if current.IsPlaying {
			current.PositionMS = current.position(now)
			current.PositionAt = now
			current.IsPlaying = false
			shouldPause = true
		}
	case "stop":
		current.IsPlaying = false
		current.PositionMS = 0
		current.PositionAt = now
		current.StreamVersion++
		stop = true
	case "next", "back":
		if !hasTrack {
			a.store.mu.Unlock()
			respondError(w, http.StatusBadRequest, "queue is empty")
			return
		}
		delta := 1
		if action == "back" {
			delta = -1
		}
		next := current.CurrentIndex + delta
		if next < 0 || next >= len(current.Queue) {
			a.store.mu.Unlock()
			respondError(w, http.StatusBadRequest, "no track in that direction")
			return
		}
		current.CurrentIndex = next
		current.PositionMS = 0
		current.PositionAt = now
		current.StreamVersion++
		track, _ = current.current()
		shouldStart = current.IsPlaying
		stop = !current.IsPlaying
	case "seek":
		if !hasTrack {
			a.store.mu.Unlock()
			respondError(w, http.StatusBadRequest, "queue is empty")
			return
		}
		if input.PositionMS < 0 {
			input.PositionMS = 0
		}
		if track.DurationMS > 0 && input.PositionMS > track.DurationMS {
			input.PositionMS = track.DurationMS
		}
		current.PositionMS = input.PositionMS
		current.PositionAt = now
		current.StreamVersion++
		shouldStart = current.IsPlaying
		stop = !current.IsPlaying
	default:
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "unsupported control action")
		return
	}
	position := current.PositionMS
	version := current.StreamVersion
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save playback state")
		return
	}
	if stop {
		a.streams.Stop(session.ID)
	}
	if shouldPause {
		a.streams.Pause(session.ID, true)
	}
	if shouldStart {
		if err := a.startStream(session.ID, track, position); err != nil {
			a.pauseForStreamFailure(session.ID, version)
			respondError(w, http.StatusBadGateway, "could not start Plex stream")
			return
		}
	}
	view, _ := a.store.snapshot(current.ID)
	respond(w, http.StatusOK, map[string]any{"session": a.view(view)})
}

func (a *API) stream(w http.ResponseWriter, r *http.Request, session *Session, member Member) {
	if !session.IsPlaying {
		respondError(w, http.StatusConflict, "session is paused")
		return
	}
	track, ok := session.current()
	if !ok {
		respondError(w, http.StatusNotFound, "queue is empty")
		return
	}
	chunks, unsubscribe, exists := a.streams.Subscribe(session.ID)
	if !exists {
		if err := a.startStream(session.ID, track, session.position(time.Now())); err != nil {
			a.pauseForStreamFailure(session.ID, session.StreamVersion)
			respondError(w, http.StatusBadGateway, "could not start Plex stream")
			return
		}
		chunks, unsubscribe, exists = a.streams.Subscribe(session.ID)
	}
	if !exists {
		respondError(w, http.StatusServiceUnavailable, "stream is starting")
		return
	}
	defer unsubscribe()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case chunk, open := <-chunks:
			if !open {
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (a *API) startStream(sessionID string, track Track, position int64) error {
	if track.PartPath == "" {
		return errors.New("track has no Plex media path")
	}
	if err := a.streams.Start(sessionID, track, a.plex.MakeURL(track.PartPath), position); err != nil {
		return err
	}
	a.store.mu.RLock()
	session := a.store.sessions[sessionID]
	version := int64(0)
	shouldSchedule := false
	if session != nil {
		current, ok := session.current()
		shouldSchedule = ok && session.IsPlaying && current.ID == track.ID
		version = session.StreamVersion
	}
	a.store.mu.RUnlock()
	if shouldSchedule && track.DurationMS > position {
		go a.advanceAtEnd(sessionID, track.ID, version, track.DurationMS-position)
	}
	return nil
}

func (a *API) pauseForStreamFailure(sessionID string, expectedVersion int64) {
	a.store.mu.Lock()
	session := a.store.sessions[sessionID]
	if session == nil || session.StreamVersion != expectedVersion {
		a.store.mu.Unlock()
		return
	}
	now := time.Now()
	session.PositionMS = session.position(now)
	session.PositionAt = now
	session.IsPlaying = false
	session.StreamVersion++
	if err := a.store.saveLocked(); err != nil {
		log.Printf("could not persist failed shared stream %s: %v", sessionID, err)
	}
	a.store.mu.Unlock()
	a.streams.Stop(sessionID)
}

// advanceAtEnd owns normal end-of-track progression for a single shared
// session. Each start/seek bumps StreamVersion, so stale timers can never
// advance a track after a new control action.
func (a *API) advanceAtEnd(sessionID, trackID string, version, remainingMS int64) {
	timer := time.NewTimer(time.Duration(remainingMS) * time.Millisecond)
	defer timer.Stop()
	<-timer.C

	a.store.mu.Lock()
	session := a.store.sessions[sessionID]
	if session == nil || !session.IsPlaying || session.StreamVersion != version {
		a.store.mu.Unlock()
		return
	}
	current, ok := session.current()
	if !ok || current.ID != trackID {
		a.store.mu.Unlock()
		return
	}
	now := time.Now()
	nextIndex := session.CurrentIndex + 1
	if nextIndex >= len(session.Queue) {
		// A completed room has no now-playing track and no remaining queue.
		// Clearing both avoids a stale final track in clients and lets the next
		// queued item start as a fresh shared stream.
		session.Queue = nil
		session.CurrentIndex = -1
		session.IsPlaying = false
		session.PositionMS = 0
		session.PositionAt = now
		session.StreamVersion++
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err == nil {
			a.streams.Stop(sessionID)
		}
		return
	}
	session.CurrentIndex = nextIndex
	session.PositionMS = 0
	session.PositionAt = now
	session.StreamVersion++
	nextVersion := session.StreamVersion
	next, _ := session.current()
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if err == nil {
		if err := a.startStream(sessionID, next, 0); err != nil {
			log.Printf("could not advance shared session %s: %v", sessionID, err)
			a.pauseForStreamFailure(sessionID, nextVersion)
		}
	}
}

func (a *API) permissions(w http.ResponseWriter, r *http.Request, session *Session, member Member, targetID string) {
	if !member.Host && !a.admin(r) {
		respondError(w, http.StatusForbidden, "host permission required")
		return
	}
	var input Permissions
	if !decode(r, &input) {
		respondError(w, http.StatusBadRequest, "invalid permissions")
		return
	}
	a.store.mu.Lock()
	current := a.store.sessions[session.ID]
	if current == nil {
		a.store.mu.Unlock()
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	target, exists := current.Members[targetID]
	if exists && !target.Host {
		target.Permissions = input
		current.Members[targetID] = target
	}
	err := a.store.saveLocked()
	a.store.mu.Unlock()
	if !exists {
		respondError(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save permissions")
		return
	}
	respond(w, http.StatusOK, map[string]any{"member": target})
}

func (a *API) handleArtwork(w http.ResponseWriter, r *http.Request) {
	if !a.admin(r) {
		if _, _, ok := a.anyMember(r); !ok {
			respondError(w, http.StatusUnauthorized, "authentication required")
			return
		}
	}
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/library/") {
		respondError(w, http.StatusBadRequest, "invalid artwork path")
		return
	}
	upstream, err := a.http.Get(a.plex.MakeURL(path))
	if err != nil {
		respondError(w, http.StatusBadGateway, "Plex artwork unavailable")
		return
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		respondError(w, upstream.StatusCode, "artwork unavailable")
		return
	}
	w.Header().Set("Content-Type", upstream.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = io.Copy(w, upstream.Body)
}

func (a *API) anyMember(r *http.Request) (Member, *Session, bool) {
	c, ok := a.readCookie(r, memberCookie)
	if !ok || c.Role != "member" {
		return Member{}, nil, false
	}
	return a.member(r, c.SessionID)
}

type publicTrack struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	ArtistID    string   `json:"artistId,omitempty"`
	Album       string   `json:"album"`
	AlbumID     string   `json:"albumId,omitempty"`
	Artwork     string   `json:"artwork,omitempty"`
	BlurHash    string   `json:"blurHash,omitempty"`
	SearchTerms []string `json:"searchTerms,omitempty"`
	DurationMS  int64    `json:"durationMs"`
}

type publicArtist struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Artwork     string   `json:"artwork,omitempty"`
	BlurHash    string   `json:"blurHash,omitempty"`
	SearchTerms []string `json:"searchTerms,omitempty"`
}

type publicAlbum struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	ArtistID    string   `json:"artistId,omitempty"`
	Year        int      `json:"year,omitempty"`
	SubType     string   `json:"subtype,omitempty"`
	Artwork     string   `json:"artwork,omitempty"`
	BlurHash    string   `json:"blurHash,omitempty"`
	SearchTerms []string `json:"searchTerms,omitempty"`
}
type sessionView struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	IsPublic      bool          `json:"isPublic"`
	Queue         []publicTrack `json:"queue"`
	CurrentIndex  int           `json:"currentIndex"`
	Current       *publicTrack  `json:"currentTrack,omitempty"`
	IsPlaying     bool          `json:"isPlaying"`
	PositionMS    int64         `json:"positionMs"`
	StreamVersion int64         `json:"streamVersion"`
	Members       []Member      `json:"members"`
	CreatedAt     time.Time     `json:"createdAt"`
}

type publicRoomTrack struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
}

type publicRoomView struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	RequiresPassword bool             `json:"requiresPassword"`
	Current          *publicRoomTrack `json:"currentTrack,omitempty"`
	IsPlaying        bool             `json:"isPlaying"`
	ListenerCount    int              `json:"listenerCount"`
	CreatedAt        time.Time        `json:"createdAt"`
}

// The share token is deliberately only included in the authenticated admin
// listing. Normal session views never disclose it to guests.
type adminSessionView struct {
	sessionView
	ShareToken string `json:"shareToken"`
}

func (a *API) view(session *Session) sessionView {
	now := time.Now()
	view := sessionView{ID: session.ID, Name: session.Name, IsPublic: session.IsPublic, Queue: publicTracks(session.Queue), CurrentIndex: session.CurrentIndex, IsPlaying: session.IsPlaying, PositionMS: session.position(now), StreamVersion: session.StreamVersion, Members: make([]Member, 0), CreatedAt: session.CreatedAt}
	if current, ok := session.current(); ok {
		public := publicTrackFor(current)
		view.Current = &public
	}
	for _, member := range session.Members {
		if memberIsActive(member, now) {
			view.Members = append(view.Members, member)
		}
	}
	sort.Slice(view.Members, func(i, j int) bool {
		if view.Members[i].Host != view.Members[j].Host {
			return view.Members[i].Host
		}
		return strings.ToLower(view.Members[i].Username) < strings.ToLower(view.Members[j].Username)
	})
	return view
}

func (a *API) publicRoom(session *Session) publicRoomView {
	view := publicRoomView{
		ID: session.ID, Name: session.Name, RequiresPassword: session.PasswordHash != "", IsPlaying: session.IsPlaying,
		ListenerCount: activeMemberCount(session, time.Now()), CreatedAt: session.CreatedAt,
	}
	if current, ok := session.current(); ok {
		view.Current = &publicRoomTrack{Title: current.Title, Artist: current.Artist}
	}
	return view
}

func memberIsActive(member Member, now time.Time) bool {
	return !member.LastSeen.IsZero() && now.Sub(member.LastSeen) <= memberIdleTimeout
}

func activeMemberCount(session *Session, now time.Time) int {
	count := 0
	for _, member := range session.Members {
		if memberIsActive(member, now) {
			count++
		}
	}
	return count
}

// Departed and timed-out members remain in the stored membership map, which
// makes a page reload race-safe. Prune them when a new join occurs so old
// browser sessions cannot reserve names indefinitely.
func pruneInactiveMembers(session *Session, now time.Time) {
	for id, member := range session.Members {
		if !memberIsActive(member, now) {
			delete(session.Members, id)
		}
	}
}

func normalizeTracks(items []responses.ResponseTrack) []Track {
	tracks := make([]Track, 0, len(items))
	for _, item := range items {
		if item.GetPath() != "" {
			trackIndex, _ := strconv.Atoi(item.Index)
			tracks = append(tracks, Track{ID: item.RatingKey, Title: item.Title, Artist: item.GrandParentTitle, ArtistID: item.GrandParentRatingKey, Album: item.ParentTitle, AlbumID: item.ParentRatingKey, TrackIndex: trackIndex, PartPath: item.GetPath(), Artwork: item.Image, BlurHash: item.ThumbBlurHash, SearchTerms: uniqueSearchTerms(item.TitleSort, item.GrandParentTitleSort, item.ParentTitleSort), DurationMS: item.Duration})
		}
	}
	return tracks
}
func publicTrackFor(track Track) publicTrack {
	return publicTrack{ID: track.ID, Title: track.Title, Artist: track.Artist, ArtistID: track.ArtistID, Album: track.Album, AlbumID: track.AlbumID, Artwork: track.Artwork, BlurHash: track.BlurHash, SearchTerms: track.SearchTerms, DurationMS: track.DurationMS}
}
func publicTracks(tracks []Track) []publicTrack {
	result := make([]publicTrack, 0, len(tracks))
	for _, track := range tracks {
		result = append(result, publicTrackFor(track))
	}
	return result
}

func publicArtists(items []responses.ResponseArtistDirectory) []publicArtist {
	result := make([]publicArtist, 0, len(items))
	for _, item := range items {
		result = append(result, publicArtist{ID: item.RatingKey, Title: item.Title, Artwork: item.Thumb, BlurHash: item.ThumbBlurHash, SearchTerms: uniqueSearchTerms(item.TitleSort)})
	}
	return result
}

func publicAlbums(items []responses.ResponseAlbumDirectory) []publicAlbum {
	result := make([]publicAlbum, 0, len(items))
	for _, item := range items {
		result = append(result, publicAlbum{ID: item.RatingKey, Title: item.Title, Artist: item.ParentTitle, ArtistID: item.ParentRatingKey, Year: item.Year, SubType: item.SubType, Artwork: item.Thumb, BlurHash: item.ThumbBlurHash, SearchTerms: uniqueSearchTerms(item.TitleSort, item.ParentTitleSort)})
	}
	return result
}

func uniqueSearchTerms(values ...string) []string {
	terms := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		terms = append(terms, value)
	}
	return terms
}

func (a *API) shareToken(sessionID, secret string) string {
	data := sessionID + "." + secret
	mac := hmac.New(sha256.New, []byte(a.config.SigningSecret))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *API) validShare(sessionID, token, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.shareToken(sessionID, secret))) == 1
}
func validUsername(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 2 || len(value) > 24 {
		return false
	}
	for _, char := range value {
		if !(char == ' ' || char == '-' || char == '_' || char >= '0' && char <= '9' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z') {
			return false
		}
	}
	return true
}

// Library calls are search-only and capped at the Plex side. This keeps a
// large library from turning one page view into thousands of metadata calls.
func librarySearch(w http.ResponseWriter, r *http.Request) (string, bool) {
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len([]rune(search)) < 2 {
		respondError(w, http.StatusBadRequest, "enter at least two characters to search the library")
		return "", false
	}
	if len([]rune(search)) > 120 {
		respondError(w, http.StatusBadRequest, "search is too long")
		return "", false
	}
	return search, true
}
func decode(r *http.Request, target any) bool {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target) == nil
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func respondError(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func clientIP(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-For"); value != "" {
		return strings.TrimSpace(strings.Split(value, ",")[0])
	}
	return r.RemoteAddr
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]rateEntry
}
type rateEntry struct {
	count int
	reset time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{entries: map[string]rateEntry{}} }
func (l *rateLimiter) allow(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	item := l.entries[key]
	now := time.Now()
	if item.reset.Before(now) {
		item = rateEntry{reset: now.Add(time.Minute)}
	}
	item.count++
	l.entries[key] = item
	return item.count <= max
}

func (a *API) Run() error {
	log.Printf("plexamp-mosh web API listening on %s", a.config.Addr)
	return http.ListenAndServe(a.config.Addr, a.Handler())
}
