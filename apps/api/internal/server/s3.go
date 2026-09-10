package server

// SigV4 presigned URLs for VaultS3 (and any S3-compatible store) —
// implemented with stdlib only (hmac/sha256), per ADR-0001's no-heavy-deps
// rule. Supports exactly what files v1 needs: GET and PUT object, 15-min
// expiry, no session-token (VaultS3 root keys).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type s3Config struct {
	Endpoint string // e.g. http://localhost:9000
	Key      string
	Secret   string
	Bucket   string
	Region   string // VaultS3 accepts any; keep constant
}

func s3ConfigFromEnv(getenv func(string) string) s3Config {
	return s3Config{
		Endpoint: getenv("OPENLANE_S3_ENDPOINT"),
		Key:      getenv("OPENLANE_S3_KEY"),
		Secret:   getenv("OPENLANE_S3_SECRET"),
		Bucket:   getenv("OPENLANE_S3_BUCKET"),
		Region:   "us-east-1",
	}
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// presignS3 produces a SigV4 presigned URL for method/host/objectKey,
// valid for ttl. Query-encoding follows AWS rules: every reserved char
// percent-encoded except unreserved A-Za-z0-9-._~.
func presignS3(cfg s3Config, method, objectKey string, ttl time.Duration, now time.Time) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid S3 endpoint %q", cfg.Endpoint)
	}
	host := u.Host
	// virtual-host style would need bucket in the host; path-style keeps it
	// simple and VaultS3 defaults to it.
	canonicalURI := "/" + cfg.Bucket + "/" + encodeKeyPath(objectKey)

	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	credentialScope := dateStamp + "/" + cfg.Region + "/s3/aws4_request"

	// canonical query: only X-Amz-* params, sorted
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", cfg.Key+"/"+credentialScope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	// url.Values.Encode sorts by key and percent-encodes exactly the AWS set
	// for these ASCII-only values.
	canonicalQuery := q.Encode()

	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	scope := credentialScope
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hashSHA256([]byte(canonicalRequest))),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+cfg.Secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(cfg.Region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	signed := canonicalQuery + "&X-Amz-Signature=" + sig
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + host + canonicalURI + "?" + signed, nil
}

func hashSHA256(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// encodeKeyPath encodes each path segment like S3 clients do: safe chars
// stay, everything else percent-encoded. Server-generated keys are
// UUID-ish so this is belt-and-braces, but the object key is only ever
// produced here — never from client input.
func encodeKeyPath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// verifyPresign recomputes the signature for a presigned URL and reports
// whether it matches (query params intact). Test hook for CI (no live
// VaultS3 there) and a guard against tampering.
func verifyPresign(cfg s3Config, method, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return false
	}
	cred := q.Get("X-Amz-Credential")
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[0] != cfg.Key {
		return false
	}
	amzDate := q.Get("X-Amz-Date")
	if len(amzDate) != 16 {
		return false
	}
	// re-sign the same inputs minus the signature
	q.Del("X-Amz-Signature")
	canonicalQuery := q.Encode()
	canonicalURI := u.EscapedPath()
	canonicalHeaders := "host:" + u.Host + "\n"
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		strings.Join(parts[1:], "/"),
		hex.EncodeToString(hashSHA256([]byte(canonicalRequest))),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+cfg.Secret), []byte(parts[1]))
	kRegion := hmacSHA256(kDate, []byte(cfg.Region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	expected := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
	return u.Query().Get("X-Amz-Signature") == expected
}
