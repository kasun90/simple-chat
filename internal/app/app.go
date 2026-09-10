// Package app assembles the HTTP handler from its parts. main and the
// end-to-end tests both use it, so the tests exercise exactly what ships.
package app

import (
	"context"
	"net/http"
	"time"

	"github.com/kasun90/simple-chat/internal/api"
	"github.com/kasun90/simple-chat/internal/auth"
	"github.com/kasun90/simple-chat/internal/hub"
	"github.com/kasun90/simple-chat/internal/store"
	"github.com/kasun90/simple-chat/web"
)

// New wires store, sessions, hub, API and frontend into one handler.
func New(st store.Store, seedUsers []string, groupName string, groupMembers []string) (http.Handler, error) {
	for _, name := range seedUsers {
		if _, err := st.UpsertUser(context.Background(), name); err != nil {
			return nil, err
		}
	}

	if groupName != "" && len(groupMembers) > 0 {
		if err := st.UpsertGroup(context.Background(), groupName); err != nil {
			return nil, err
		}
	}

	sessions := auth.NewSessions(24 * time.Hour)
	h := hub.New(st)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	(&auth.Handlers{Sessions: sessions, Store: st}).Register(mux)
	mux.Handle("GET /ws", sessions.Middleware(h.Handler()))
	(&api.Handlers{Store: st, Presence: h}).Register(mux, sessions)
	mux.Handle("/", web.Handler())
	return api.Logging(mux), nil
}
