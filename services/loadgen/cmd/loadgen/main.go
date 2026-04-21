// Command loadgen is the HTTP sidecar k6 hits to generate INSERT/UPDATE/
// DELETE load against Postgres. It exists because the k6 script in
// load/k6/write-mix.js assumes an HTTP API; we translate those calls into
// SQL via pgx. See docs/plan.md (Week 3) and ADR-005 for the load story.
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

	"loadgen/internal/api"
	"loadgen/internal/db"
)

func main() {
	uri := env("PG_URI", "postgres://app:app@postgres:5432/app?sslmode=disable")
	addr := env("ADDR", ":8080")

	log.Printf("loadgen starting: pg=%s addr=%s", uri, addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := db.New(ctx, uri)
	if err != nil {
		// Boot-time fatal. Process exit lets the OS reclaim the signal
		// channel; there is no useful cleanup at this point in startup.
		log.Fatalf("db: %v", err) //nolint:gocritic
	}
	defer store.Close()

	mux := http.NewServeMux()
	h := &api.Handler{Store: store}
	h.Register(mux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("loadgen shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
