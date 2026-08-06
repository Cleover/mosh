package webapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
