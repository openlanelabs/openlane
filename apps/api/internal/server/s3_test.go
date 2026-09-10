package server

import (
	"strings"
	"testing"
	"time"
)

func testS3() s3Config {
	return s3Config{
		Endpoint: "http://localhost:9000",
		Key:      "openlane",
		Secret:   "openlane-secret",
		Bucket:   "openlane-files",
		Region:   "us-east-1",
	}
}

// Round-trip: a presigned URL must verify against the same config.
func TestPresignRoundTrip(t *testing.T) {
	cfg := testS3()
	now := time.Now()
	for _, m := range []string{"GET", "PUT"} {
		u, err := presignS3(cfg, m, "ws/proj/uuid.pdf", 15*time.Minute, now)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if !strings.HasPrefix(u, "http://localhost:9000/openlane-files/ws/proj/uuid.pdf?") {
			t.Fatalf("%s: url shape: %s", m, u)
		}
		if !verifyPresign(cfg, m, u) {
			t.Fatalf("%s: signature does not verify", m)
		}
	}
}

// Wrong secret must NOT verify — this is the whole point of presigning.
func TestPresignWrongSecret(t *testing.T) {
	cfg := testS3()
	u, _ := presignS3(cfg, "PUT", "ws/proj/uuid.pdf", 15*time.Minute, time.Now())
	bad := cfg
	bad.Secret = "attacker"
	if verifyPresign(bad, "PUT", u) {
		t.Fatal("signature verified with wrong secret")
	}
}

// Tampering with the query (e.g. bumping the expiry) must break the sig.
func TestPresignTamperedQuery(t *testing.T) {
	cfg := testS3()
	u, _ := presignS3(cfg, "GET", "ws/proj/uuid.pdf", 15*time.Minute, time.Now())
	if i := strings.Index(u, "X-Amz-Expires="); i > 0 {
		u = u[:i] + "X-Amz-Expires=99999" + strings.TrimPrefix(u[i:i+len("X-Amz-Expires=900")], "X-Amz-Expires=900")
	} else {
		t.Fatal("no expiry in url")
	}
	if verifyPresign(cfg, "GET", u) {
		t.Fatal("tampered query verified")
	}
}

// Method swap must not verify (a presigned PUT can't be reused as GET).
func TestPresignMethodSwap(t *testing.T) {
	cfg := testS3()
	u, _ := presignS3(cfg, "PUT", "ws/proj/uuid.pdf", 15*time.Minute, time.Now())
	if verifyPresign(cfg, "GET", u) {
		t.Fatal("PUT url verified as GET")
	}
}

// Object-key safety: keys are SERVER-generated (ws/project/uuid) so path
// traversal is structurally impossible (spec edge: file name
// ../../etc/passwd never reaches storage). The encoder must still not
// open a double-decode hole: a '%2F' in a segment re-escapes to %252F,
// never decoding into a real '/'.
func TestEncodeKeyPath(t *testing.T) {
	got := encodeKeyPath("ws/proj/a%2Fb.pdf")
	if got != "ws/proj/a%252Fb.pdf" {
		t.Fatalf("percent not re-escaped (double-decode hole): %q", got)
	}
}
