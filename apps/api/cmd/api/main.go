// OpenLane API — see docs/openlane_spec.md and docs/adr/.
//
// P0 scaffold: health endpoint only. Routes grow from packages/contracts/openapi.yaml.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// ponytail: ADDR is operator-controlled config, not user input; G706 taint is
	// overcautious here. Fixed value, not log-injectable in practice.
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("openlane api listening")
	log.Fatal(srv.ListenAndServe())
}
