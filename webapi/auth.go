package webapi

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const adminCookie = "pm_admin"
const memberCookie = "pm_member"

type claims struct {
	Role      string `json:"role"`
	SessionID string `json:"sessionId,omitempty"`
	MemberID  string `json:"memberId,omitempty"`
	Exp       int64  `json:"exp"`
}

func (a *API) issueCookie(w http.ResponseWriter, name string, c claims) {
	payload, _ := json.Marshal(c)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(a.config.SigningSecret))
	mac.Write([]byte(encoded))
	value := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(time.Until(time.Unix(c.Exp, 0)).Seconds()),
		HttpOnly: true,
		Secure:   a.config.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *API) readCookie(r *http.Request, name string) (claims, bool) {
	cookie, err := r.Cookie(name)
	if err != nil {
		return claims{}, false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return claims{}, false
	}
	mac := hmac.New(sha256.New, []byte(a.config.SigningSecret))
	mac.Write([]byte(parts[0]))
	expected := mac.Sum(nil)
	actual, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(expected, actual) != 1 {
		return claims{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims{}, false
	}
	var c claims
	if json.Unmarshal(decoded, &c) != nil || c.Exp < time.Now().Unix() {
		return claims{}, false
	}
	return c, true
}

func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// Passwords are salted and deliberately expensive. This keeps the project
// dependency-free while avoiding plaintext or a fast unsalted digest in its
// small JSON state file.
func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	digest := passwordDigest(password, salt)
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(digest)
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, ":")
	if len(parts) != 2 {
		return false
	}
	salt, saltErr := hex.DecodeString(parts[0])
	expected, hashErr := hex.DecodeString(parts[1])
	if saltErr != nil || hashErr != nil {
		return false
	}
	return subtle.ConstantTimeCompare(passwordDigest(password, salt), expected) == 1
}

func passwordDigest(password string, salt []byte) []byte {
	digest, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if err != nil {
		// The fixed parameters above are valid; fail closed if the standard
		// library ever rejects them rather than accepting a weak fallback.
		return nil
	}
	return digest
}
