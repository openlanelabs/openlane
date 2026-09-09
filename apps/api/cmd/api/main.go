// OpenLane API — see docs/openlane_spec.md and docs/adr/.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openlanelabs/openlane/apps/api/internal/server"
)

func main() {
	addr, dsn, staffToken := server.ConfigFromEnv()
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (must be the openlane_app role, not the owner)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux, pool, err := server.New(ctx, dsn, staffToken)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	defer pool.Close()

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("openlane api listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
