// Command server is the single binary for the chat app: it serves the HTTP
// API, the WebSocket endpoint and the static frontend.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kasun90/simple-chat/internal/app"
	"github.com/kasun90/simple-chat/internal/config"
	"github.com/kasun90/simple-chat/internal/store"
)

func main() {
	cfg := config.FromEnv()

	// `server -healthcheck` is used by the compose healthcheck: the distroless
	// runtime image has no curl/wget, so the binary probes itself.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck("http://127.0.0.1" + cfg.Addr + "/healthz"))
	}

	st, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	handler, err := app.New(st, cfg.SeedUsers)
	if err != nil {
		log.Fatalf("wire app: %v", err)
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// ReadHeaderTimeout defends against slowloris on the plain HTTP side.
		// No global ReadTimeout/WriteTimeout: those would kill long-lived
		// WebSocket connections, which manage their own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown so `docker compose down` (SIGTERM) finishes in-flight
	// requests instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func healthcheck(url string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	resp.Body.Close()
	return 0
}
