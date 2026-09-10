package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestLogQuotesPath: a %0A in a request path must NOT split the log
// line (CWE-117 / gosec G706). Guards the #nosec in requestLog — if the %q
// verbs are ever changed back to %s, this fails.
func TestRequestLogQuotesPath(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)

	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	// raw newline smuggled in the path
	req := httptest.NewRequest(http.MethodGet, "http://x/v1/auth/magic-link%0AFAKELOG", nil)
	req.URL.Path = "/v1/auth/magic-link\nFAKELOG"
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Count(out, "\nFAKELOG") != 0 || !strings.Contains(out, "status=418") {
		t.Fatalf("log line split or missing: %q", out)
	}
	if strings.Contains(out, "\nFAKELOG status=418") {
		t.Fatalf("injection: newline reached log raw: %q", out)
	}
	// the quoted form renders the newline escaped instead
	if !strings.Contains(out, "FAKELOG") {
		t.Fatalf("path value lost: %q", out)
	}
}
