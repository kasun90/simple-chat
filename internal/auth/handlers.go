package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/kasun90/simple-chat/internal/store"
)

// Handlers exposes login/logout/me. It depends on the Store only to upsert
// and read users.
type Handlers struct {
	Sessions *Sessions
	Store    store.Store
}

// Register mounts the auth routes. /api/me is protected by the middleware so
// the frontend can use it as "am I logged in?" on page load.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", h.login)
	mux.HandleFunc("POST /api/logout", h.logout)
	mux.Handle("GET /api/me", h.Sessions.Middleware(http.HandlerFunc(h.me)))
}

// usernameRe is a strict ASCII whitelist. Usernames are the whole identity in
// this app, so look-alike names must be impossible: a zero-width space or a
// Cyrillic "а" would otherwise let an impostor sit in the contact list as an
// indistinguishable "alice".
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	// Login needs no cookie, so SameSite cannot protect it: a cross-site form
	// post could log the victim in as an attacker-controlled account and then
	// read everything they type. Origin + a JSON content type (which HTML
	// forms cannot send) close that hole.
	if !SameOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusForbidden, "cross-origin or non-JSON login rejected")
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(body.Username)
	if !usernameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "username must be 1-32 letters, digits, _ . or -")
		return
	}
	user, err := h.Store.UpsertUser(r.Context(), name)
	if err != nil {
		log.Printf("upsert user %q: %v", name, err)
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	h.Sessions.SetCookie(w, h.Sessions.Create(user.ID))
	writeJSON(w, http.StatusOK, user)
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	if !SameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin logout rejected")
		return
	}
	if c, err := r.Cookie(CookieName); err == nil {
		h.Sessions.Delete(c.Value)
	}
	ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) {
	id, _ := UserID(r.Context())
	user, err := h.Store.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unknown user")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
