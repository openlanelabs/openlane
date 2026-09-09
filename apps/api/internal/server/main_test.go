//go:build integration

package server

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// CI/e2e static bearer: integration tests authenticate with the static
	// staff token (documented escape hatch — OPENLANE_ALLOW_STATIC_TOKEN=1).
	os.Setenv("OPENLANE_ALLOW_STATIC_TOKEN", "1")
	os.Setenv("OPENLANE_JWT_SECRET", "test-jwt-secret")
	os.Exit(m.Run())
}
