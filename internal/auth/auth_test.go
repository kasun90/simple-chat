package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kasun90/simple-chat/internal/store"
)

func newServer(t *testing.T, ttl time.Duration) (*httptest.Server, *Sessions) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sessions := NewSessions(ttl)
	mux := http.NewServeMux()
	(&Handlers{Sessions: sessions, Store: st}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sessions
}

func TestLoginMeLogoutFlow(t *testing.T) {
	srv, _ := newServer(t, time.Hour)

	// Unauthenticated /api/me is rejected.
	resp, _ := http.Get(srv.URL + "/api/me")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me before login: %d", resp.StatusCode)
	}

	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"alice"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %v %d", err, resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("expected HttpOnly session cookie")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.AddCookie(cookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me after login: %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", srv.URL+"/api/logout", nil)
	req.AddCookie(cookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.AddCookie(cookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", resp.StatusCode)
	}
}

func TestLoginValidation(t *testing.T) {
	srv, _ := newServer(t, time.Hour)
	for _, body := range []string{`{}`, `{"username":"  "}`, `{"username":"has space"}`, `not json`, `{"username":"` + strings.Repeat("a", 33) + `"}`, `{"username":"alice\u200b"}`, `{"username":"аlice"}`} {
		resp, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", body, resp.StatusCode)
		}
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessions(time.Millisecond)
	tok := s.Create(7)
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("expired session should not resolve")
	}
	if _, ok := s.Lookup("garbage"); ok {
		t.Fatal("unknown token should not resolve")
	}
}

func TestLoginRejectsCrossSiteForm(t *testing.T) {
	srv, _ := newServer(t, time.Hour)
	// A cross-site HTML form can only send text/plain or form encodings, and
	// the browser stamps a foreign Origin on it.
	req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(`{"username":"mallory"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("text/plain: want 403, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(`{"username":"mallory"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: want 403, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/logout", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout foreign origin: want 403, got %d", resp.StatusCode)
	}
}
