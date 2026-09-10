package server

// Prod-config hardening (issue #56, spec §17-6/§420): fail-closed
// production profile + throttle IP extraction behind proxies.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

func prodProfile() bool {
	switch os.Getenv("OPENLANE_ENV") {
	case "production", "prod":
		return true
	}
	return false
}

// validateProdConfig: the dev/test escape hatches (JWT fallback secret,
// static staff token, magic-link dev tokens, slack allow-any) break auth
// integrity in production — the server refuses to start with any active.
func validateProdConfig() error {
	if !prodProfile() {
		return nil
	}
	var findings []string
	if s := os.Getenv("OPENLANE_JWT_SECRET"); s == "" || s == "dev-secret-change-me" {
		findings = append(findings, "OPENLANE_JWT_SECRET must be set to a real secret (not the dev fallback)")
	}
	if os.Getenv("OPENLANE_ALLOW_STATIC_TOKEN") == "1" {
		findings = append(findings, "OPENLANE_ALLOW_STATIC_TOKEN=1 is a CI-only escape hatch and must be off in production")
	}
	if os.Getenv("OPENLANE_DEV_LOGIN") == "1" {
		findings = append(findings, "OPENLANE_DEV_LOGIN=1 returns dev tokens from the magic-link endpoint and must be off in production")
	}
	if os.Getenv("OPENLANE_SLACK_ALLOW_ANY") == "1" {
		findings = append(findings, "OPENLANE_SLACK_ALLOW_ANY=1 disables the webhook allowlist and must be off in production")
	}
	if len(findings) == 0 {
		return nil
	}
	return fmt.Errorf("production profile refused to start: %s", findings)
}

// trustProxy: honor X-Forwarded-For for throttle keying. Off by default —
// the header is client-spoofable unless the operator says a proxy sets it.
func trustProxy() bool {
	return os.Getenv("OPENLANE_TRUST_PROXY") == "1"
}

// clientIP: throttle key source. With trustProxy, take the LAST
// X-Forwarded-For entry — the single trusted hop our proxy appended.
// A forged client-supplied prefix just means we throttle the proxy's
// view of the client, which is the only address it controls.
func clientIP(r *http.Request) string {
	if trustProxy() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h // ports churn per connection; throttle the host
	}
	return r.RemoteAddr
}
