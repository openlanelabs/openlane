package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateProdConfig(t *testing.T) {
	// dev profile: escape hatches tolerated
	t.Setenv("OPENLANE_ENV", "")
	t.Setenv("OPENLANE_JWT_SECRET", "")
	t.Setenv("OPENLANE_ALLOW_STATIC_TOKEN", "1")
	if err := validateProdConfig(); err != nil {
		t.Fatalf("dev profile must tolerate escape hatches: %v", err)
	}

	// prod profile with clean config: boots
	t.Setenv("OPENLANE_ENV", "production")
	t.Setenv("OPENLANE_JWT_SECRET", "a-real-secret-32-bytes-long-at-least!")
	t.Setenv("OPENLANE_ALLOW_STATIC_TOKEN", "")
	t.Setenv("OPENLANE_DEV_LOGIN", "")
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "")
	if err := validateProdConfig(); err != nil {
		t.Fatalf("prod profile with clean config must boot: %v", err)
	}

	// each hatch individually refuses startup
	cases := map[string]struct{ k, v string }{
		"default jwt secret": {"OPENLANE_JWT_SECRET", "dev-secret-change-me"},
		"missing jwt secret": {"OPENLANE_JWT_SECRET", ""},
		"static token":       {"OPENLANE_ALLOW_STATIC_TOKEN", "1"},
		"dev login":          {"OPENLANE_DEV_LOGIN", "1"},
		"slack allow-any":    {"OPENLANE_SLACK_ALLOW_ANY", "1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// reset all hatches to safe values, then break one
			t.Setenv("OPENLANE_JWT_SECRET", "a-real-secret-32-bytes-long-at-least!")
			t.Setenv("OPENLANE_ALLOW_STATIC_TOKEN", "")
			t.Setenv("OPENLANE_DEV_LOGIN", "")
			t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "")
			t.Setenv(c.k, c.v)
			if err := validateProdConfig(); err == nil {
				t.Fatalf("%s=%s must refuse startup in prod", c.k, c.v)
			}
		})
	}

	// alias "prod" also triggers the profile
	t.Setenv("OPENLANE_ENV", "prod")
	t.Setenv("OPENLANE_JWT_SECRET", "")
	if err := validateProdConfig(); err == nil {
		t.Fatal("OPENLANE_ENV=prod must also refuse a missing secret")
	}
}

func TestClientIP(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// untrusted (default): RemoteAddr only, header ignored
	t.Setenv("OPENLANE_TRUST_PROXY", "")
	if got := clientIP(mk("203.0.113.9:5555", "1.2.3.4")); got != "203.0.113.9" {
		t.Fatalf("untrusted xff must be ignored, got %s", got)
	}

	// trusted: last hop wins
	t.Setenv("OPENLANE_TRUST_PROXY", "1")
	if got := clientIP(mk("10.0.0.5:4444", "1.2.3.4, 5.6.7.8")); got != "5.6.7.8" {
		t.Fatalf("trusted xff must use last entry, got %s", got)
	}
	if got := clientIP(mk("203.0.113.9:5555", "")); got != "203.0.113.9" {
		t.Fatalf("no xff falls back to RemoteAddr, got %s", got)
	}
	// single-entry xff
	if got := clientIP(mk("10.0.0.5:4444", "9.9.9.9")); got != "9.9.9.9" {
		t.Fatalf("single-entry xff, got %s", got)
	}
}
