//go:build integration

package server

import (
	"testing"
)

func TestMagicLinkRateLimit(t *testing.T) {
	h, _ := authTestStack(t)

	// per-EMAIL throttle (5/hr): same email repeatedly → 429 by the 6th.
	lastCode := 0
	for i := 0; i < 8; i++ {
		code, _ := authJSON(t, h, "POST", "/v1/auth/magic-link",
			map[string]string{"email": "asha@acme.test", "workspace_slug": "acme"}, nil)
		lastCode = code
		if code == 429 {
			break
		}
	}
	if lastCode != 429 {
		t.Fatalf("email throttle: no 429 after 8 same-email attempts (last=%d)", lastCode)
	}

	// per-IP throttle (10/hr): different emails from the same client IP
	// (httptest = constant 127.0.0.1) — ~5 rows exist already, 6 more unique
	// emails push the IP bucket past 10 → 429 even for a fresh email.
	saw429 := false
	for i := 0; i < 7; i++ {
		code, _ := authJSON(t, h, "POST", "/v1/auth/magic-link",
			map[string]string{"email": "u" + string(rune('a'+i)) + "@acme.test", "workspace_slug": "acme"}, nil)
		if code == 429 {
			saw429 = true
			break
		}
	}
	if !saw429 {
		t.Fatal("IP throttle: no 429 after 12 total requests from one IP")
	}
}
