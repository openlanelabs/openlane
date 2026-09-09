// OpenLane API — see docs/openlane_spec.md and docs/adr/.
//
// P0 scaffold: health endpoint only. Routes grow from packages/contracts/openapi.yaml.
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("openlane api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
