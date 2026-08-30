// Package auth identifies users with an HttpOnly session cookie.
//
// The MVP has no passwords: knowing a username logs you in. That is enough to
// demonstrate two identities in two browser windows, and the cookie plumbing
// is the part that stays when real credentials are added.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const CookieName = "chat_session"

// Sessions is an in-memory session store. It lives in one process, so a
// restart logs everyone out and a second replica would not recognise the
// cookie — acceptable for the MVP, and the reason the type sits behind a
// small interface-shaped API (Create/Lookup/Delete) that a Redis/DB-backed
// version can replace.
type Sessions struct {
	mu   sync.Mutex
	ttl  time.Duration
	byID map[string]session
}

type session struct {
	userID  int64
	expires time.Time
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{ttl: ttl, byID: make(map[string]session)}
}

// Create issues a new opaque token for the user. 32 random bytes is far more
// entropy than needed; the token is a lookup key, never decoded.
func (s *Sessions) Create(userID int64) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.byID[token] = session{userID: userID, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return token
}

// Lookup returns the user for a token. Expired sessions are removed lazily on
// access rather than by a sweeper goroutine: at MVP scale the map cannot grow
// meaningfully, and one less goroutine is one less thing to reason about.
func (s *Sessions) Lookup(token string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[token]
	if !ok {
		return 0, false
	}
	if time.Now().After(sess.expires) {
		delete(s.byID, token)
		return 0, false
	}
	return sess.userID, true
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	delete(s.byID, token)
	s.mu.Unlock()
}

// SetCookie writes the session cookie. HttpOnly keeps it away from page
// scripts (XSS cannot steal it); SameSite=Lax means a cross-site form POST
// will not carry it, which is our CSRF defence for a same-origin app.
// Secure is deliberately not set: the demo runs on plain http://localhost,
// where browsers would drop a Secure cookie. Set it behind TLS in production.
func (s *Sessions) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.ttl / time.Second),
	})
}

func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

type ctxKey struct{}

// UserID extracts the authenticated user placed by Middleware.
func UserID(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok
}

// Middleware rejects requests without a valid session and stores the user ID
// in the request context. It is used for both the JSON API and the WebSocket
// upgrade: browsers attach cookies to the upgrade request, so the socket is
// authenticated by the same mechanism as everything else.
func (s *Sessions) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		userID, ok := s.Lookup(c.Value)
		if !ok {
			ClearCookie(w)
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, userID)))
	})
}

// SameOrigin reports whether the request's Origin header (if any) matches the
// Host. Browsers set Origin on cross-site POSTs and on every WebSocket
// upgrade, and page script cannot forge it, which makes this the CSRF check
// for both. A missing Origin means a non-browser client (curl, tests) — the
// session cookie is still required for anything that matters.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
