package hub

import (
	"net/http"

	"github.com/kasun90/simple-chat/internal/auth"
	"github.com/kasun90/simple-chat/internal/ws"
)

// Handler upgrades an authenticated request to a WebSocket and serves it.
// Mount it behind auth.Middleware.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserID(r.Context())
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		// Browsers send cookies on cross-site WebSocket upgrades and SameSite
		// does not protect them, so a page on evil.example could open a
		// socket as the victim. The Origin header is the defence: it is set
		// by the browser and cannot be forged by page script.
		if !auth.SameOrigin(r) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		conn, err := ws.Upgrade(w, r)
		if err != nil {
			return // Upgrade already wrote the HTTP error
		}
		conn.ReadTimeout = ReadTimeout
		go conn.KeepAlive(KeepAliveInterval)
		h.Serve(r.Context(), userID, conn)
	})
}
