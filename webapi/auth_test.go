package webapi

import (
	"net/http"
	"net/http/httptest"
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
