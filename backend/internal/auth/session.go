// Package auth handles players, logins and session cookies.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	CookieName = "ctc_session"
	sessionTTL = 30 * 24 * time.Hour
)

// Sessions issues and verifies stateless, HMAC-signed session cookies.
// Cookie value: base64(userID:expiryUnix) + "." + base64(hmac-sha256).
type Sessions struct {
	secret []byte
	secure bool // set the Secure flag (HTTPS only) in production
	now    func() time.Time
}

// NewSessions creates a session manager.
func NewSessions(secret string, secure bool) *Sessions {
	return &Sessions{secret: []byte(secret), secure: secure, now: time.Now}
}

// Issue sets a session cookie for userID.
func (s *Sessions) Issue(w http.ResponseWriter, userID int64) {
	exp := s.now().Add(sessionTTL)
	payload := fmt.Sprintf("%d:%d", userID, exp.Unix())
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    enc(payload) + "." + enc(string(s.sign(payload))),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear removes the session cookie.
func (s *Sessions) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// UserID returns the authenticated user from the request cookie, if valid and unexpired.
func (s *Sessions) UserID(r *http.Request) (int64, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return 0, false
	}
	payloadB64, sigB64, ok := strings.Cut(c.Value, ".")
	if !ok {
		return 0, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(payloadB64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sigB64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, s.sign(string(payload))) {
		return 0, false
	}
	idStr, expStr, ok := strings.Cut(string(payload), ":")
	if !ok {
		return 0, false
	}
	id, err1 := strconv.ParseInt(idStr, 10, 64)
	exp, err2 := strconv.ParseInt(expStr, 10, 64)
	if err1 != nil || err2 != nil || id <= 0 || s.now().Unix() > exp {
		return 0, false
	}
	return id, true
}

func (s *Sessions) sign(payload string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
