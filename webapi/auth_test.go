package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash := hashPassword("correct horse battery staple")
	if !verifyPassword("correct horse battery staple", hash) {
		t.Fatal("expected password verification to succeed")
	}
	if verifyPassword("wrong password", hash) {
		t.Fatal("expected incorrect password verification to fail")
	}
}

func TestSignedCookieRejectsTampering(t *testing.T) {
	api := &API{config: AppConfig{SigningSecret: "01234567890123456789012345678901"}}
	recorder := httptest.NewRecorder()
	api.issueCookie(recorder, adminCookie, claims{Role: "admin", Exp: time.Now().Add(time.Hour).Unix()})
	cookie := recorder.Result().Cookies()[0]

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	if claims, ok := api.readCookie(request, adminCookie); !ok || claims.Role != "admin" {
		t.Fatal("expected signed cookie to be accepted")
	}

	cookie.Value += "x"
	tampered := httptest.NewRequest(http.MethodGet, "/", nil)
	tampered.AddCookie(cookie)
	if _, ok := api.readCookie(tampered, adminCookie); ok {
		t.Fatal("expected tampered cookie to be rejected")
	}
}

func TestHandlerRoutesDoNotDependOnMethodQualifiedMuxPatterns(t *testing.T) {
	api := &API{config: AppConfig{InternalAPISecret: "internal-secret", SigningSecret: "01234567890123456789012345678901"}, limits: newRateLimiter()}

	health := httptest.NewRecorder()
	api.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health route returned %d", health.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/admin/sessions", nil)
	request.Header.Set("X-Internal-API-Secret", "internal-secret")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected matched admin route to reject missing login with 401, got %d", response.Code)
	}
}

func TestRoomViewUsesEmptyMemberArray(t *testing.T) {
	view := (&API{}).view(&Session{ID: "room", Name: "Room", CurrentIndex: -1})
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"members":[]`) {
		t.Fatalf("empty members should serialize as an array, got %s", payload)
	}
}

func TestAdminCanCloseSession(t *testing.T) {
	api := &API{
		config:  AppConfig{InternalAPISecret: "internal-secret", SigningSecret: "01234567890123456789012345678901"},
		limits:  newRateLimiter(),
		streams: NewStreamHub("unused", "320k"),
		store: &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
			"room": {ID: "room", Members: map[string]Member{}},
		}},
	}
	cookieRecorder := httptest.NewRecorder()
	api.issueCookie(cookieRecorder, adminCookie, claims{Role: "admin", Exp: time.Now().Add(time.Hour).Unix()})

	request := httptest.NewRequest(http.MethodDelete, "/api/admin/sessions/room", nil)
	request.Header.Set("X-Internal-API-Secret", "internal-secret")
	request.AddCookie(cookieRecorder.Result().Cookies()[0])
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("close returned %d", response.Code)
	}
	if _, exists := api.store.sessions["room"]; exists {
		t.Fatal("closed session remained in store")
	}
}

func TestAdminCanUpdateRoomAccess(t *testing.T) {
	api := &API{
		config: AppConfig{InternalAPISecret: "internal-secret", SigningSecret: "01234567890123456789012345678901"},
		limits: newRateLimiter(),
		store: &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
			"room": {ID: "room", Members: map[string]Member{}},
		}},
	}
	cookieRecorder := httptest.NewRecorder()
	api.issueCookie(cookieRecorder, adminCookie, claims{Role: "admin", Exp: time.Now().Add(time.Hour).Unix()})

	request := httptest.NewRequest(http.MethodPatch, "/api/admin/sessions/room", strings.NewReader(`{"public":true,"password":"secret"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Internal-API-Secret", "internal-secret")
	request.AddCookie(cookieRecorder.Result().Cookies()[0])
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", response.Code, response.Body.String())
	}
	room := api.store.sessions["room"]
	if !room.IsPublic || !verifyPassword("secret", room.PasswordHash) {
		t.Fatalf("room access settings were not persisted: %#v", room)
	}

	clearPassword := httptest.NewRequest(http.MethodPatch, "/api/admin/sessions/room", strings.NewReader(`{"password":""}`))
	clearPassword.Header.Set("Content-Type", "application/json")
	clearPassword.Header.Set("X-Internal-API-Secret", "internal-secret")
	clearPassword.AddCookie(cookieRecorder.Result().Cookies()[0])
	clearResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(clearResponse, clearPassword)
	if clearResponse.Code != http.StatusOK || api.store.sessions["room"].PasswordHash != "" {
		t.Fatalf("room password was not cleared: status=%d", clearResponse.Code)
	}
}

func TestPublicRoomsCanBeDiscoveredAndJoinedWithoutAnInvite(t *testing.T) {
	api := &API{
		config:  AppConfig{InternalAPISecret: "internal-secret", SigningSecret: "01234567890123456789012345678901"},
		limits:  newRateLimiter(),
		streams: NewStreamHub("unused", "320k"),
		store: &Store{path: filepath.Join(t.TempDir(), "sessions.json"), sessions: map[string]*Session{
			"public": {
				ID: "public", Name: "Public room", IsPublic: true, ShareSecret: "public-share", CurrentIndex: -1,
				Members: map[string]Member{"host": {ID: "host", Username: "Host", Host: true}}, CreatedAt: time.Now(),
			},
			"private": {
				ID: "private", Name: "Private room", ShareSecret: "private-share", CurrentIndex: -1,
				Members: map[string]Member{}, CreatedAt: time.Now(),
			},
		}},
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/public/sessions", nil)
	listRequest.Header.Set("X-Internal-API-Secret", "internal-secret")
	listResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("public list returned %d", listResponse.Code)
	}
	payload := listResponse.Body.Bytes()
	if strings.Contains(string(payload), "share") {
		t.Fatal("public listing leaked invite data")
	}
	var listing struct {
		Sessions []publicRoomView `json:"sessions"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 || listing.Sessions[0].ID != "public" {
		t.Fatalf("unexpected public listing: %#v", listing.Sessions)
	}

	joinRequest := httptest.NewRequest(http.MethodPost, "/api/sessions/public/join", strings.NewReader(`{"username":"Guest"}`))
	joinRequest.Header.Set("Content-Type", "application/json")
	joinRequest.Header.Set("X-Internal-API-Secret", "internal-secret")
	joinResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(joinResponse, joinRequest)
	if joinResponse.Code != http.StatusOK {
		t.Fatalf("public join returned %d: %s", joinResponse.Code, joinResponse.Body.String())
	}

	privateJoin := httptest.NewRequest(http.MethodPost, "/api/sessions/private/join", strings.NewReader(`{"username":"Guest"}`))
	privateJoin.Header.Set("Content-Type", "application/json")
	privateJoin.Header.Set("X-Internal-API-Secret", "internal-secret")
	privateResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(privateResponse, privateJoin)
	if privateResponse.Code != http.StatusUnauthorized {
		t.Fatalf("private join without an invite returned %d", privateResponse.Code)
	}
}
