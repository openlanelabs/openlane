// OpenLane API — see docs/openlane_spec.md and docs/adr/.
package main

import (
	"context"
	"fmt"
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
		Handler:           requestLog(mux),
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

// requestLog: one structured line per request — method, path, status,
// duration, request-id (echoed via X-Request-Id). P0 observability baseline
// (spec §26): no metrics stack; ingress + this line cover the basics.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// hex-only: a client-forged X-Request-Id must not become fake log
		// lines (gosec G706 log injection) — regenerate on anything else
		reqID := r.Header.Get("X-Request-Id")
		if !isHex(reqID) || len(reqID) > 64 {
			reqID = fmt.Sprintf("%x", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-Id", reqID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("req=%s method=%s path=%s status=%d dur=%dms",
			reqID, r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// isHex: accept only hex request ids — a client-forged X-Request-Id with
// newlines must never become fake log lines (gosec G706).
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
