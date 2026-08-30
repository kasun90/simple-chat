// Package api serves the JSON endpoints the UI needs outside the real-time
// path: the contact list and conversation history.
package api

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kasun90/simple-chat/internal/auth"
	"github.com/kasun90/simple-chat/internal/store"
)

// Presence is the one thing the API needs from the hub. Keeping it an
// interface means api does not import hub, so the two packages cannot end up
// depending on each other.
type Presence interface {
	Online(userID int64) bool
}

type Handlers struct {
	Store    store.Store
	Presence Presence
}

type contact struct {
	store.User
	Online bool `json:"online"`
}

// Register mounts the routes; all of them require a session.
func (h *Handlers) Register(mux *http.ServeMux, sessions *auth.Sessions) {
	mux.Handle("GET /api/users", sessions.Middleware(http.HandlerFunc(h.listUsers)))
	mux.Handle("GET /api/conversations/{userID}/messages", sessions.Middleware(http.HandlerFunc(h.listMessages)))
}

// listUsers returns everyone except the caller. The whole table is returned:
// with a handful of demo users that is right, and OBSTACLES.md covers what
// changes (search + pagination) when it is not.
func (h *Handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	me, _ := auth.UserID(r.Context())
	users, err := h.Store.ListUsers(r.Context())
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	out := make([]contact, 0, len(users))
	for _, u := range users {
		if u.ID == me {
			continue
		}
		out = append(out, contact{User: u, Online: h.Presence.Online(u.ID)})
	}
	writeJSON(w, http.StatusOK, out)
}

// listMessages returns history between the caller and {userID}, oldest
// first, with ID > after. The client passes the last ID it has seen, which is
// also how it fills gaps after a reconnect.
func (h *Handlers) listMessages(w http.ResponseWriter, r *http.Request) {
	me, _ := auth.UserID(r.Context())
	other, err := strconv.ParseInt(r.PathValue("userID"), 10, 64)
	if err != nil || other == me {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	msgs, err := h.Store.ListMessages(r.Context(), me, other, after, limit)
	if err != nil {
		log.Printf("list messages %d<->%d: %v", me, other, err)
		writeError(w, http.StatusInternalServerError, "could not list messages")
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// Logging is a minimal request log: method, path, status, duration. It is
// deliberately plain text to stdout so `docker compose logs` is readable; a
// structured logger is a production concern.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter records the status code. It must keep exposing Hijacker or the
// WebSocket upgrade behind this middleware would fail.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	s.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
