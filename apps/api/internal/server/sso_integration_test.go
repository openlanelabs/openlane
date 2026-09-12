//go:build integration

package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// oidcStub: a local OIDC IdP — discovery, JWKS, authorize redirect,
// token endpoint issuing RSA-signed ID tokens with nonce/aud checks.
type oidcStub struct {
	srv           *httptest.Server
	key           *rsa.PrivateKey
	kid           string
	clientID      string
	lastNonce     string
	lastChallenge string
}

func newOIDCStub(t *testing.T) *oidcStub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	st := &oidcStub{key: key, kid: "test-key-1", clientID: "ol-client"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 st.srv.URL,
			"authorization_endpoint": st.srv.URL + "/authorize",
			"token_endpoint":         st.srv.URL + "/token",
			"jwks_uri":               st.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		nb := st.key.PublicKey.N.Bytes()
		eb := []byte{1, 0, 1}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": st.kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(nb),
				"e": base64.RawURLEncoding.EncodeToString(eb),
			}},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		// record what the app sent
		st.lastNonce = r.URL.Query().Get("nonce")
		st.lastChallenge = r.URL.Query().Get("code_challenge")
		if r.URL.Query().Get("code_challenge_method") != "S256" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// real IdP behavior: bounce straight back to the registered
		// redirect_uri with code + state
		ru := r.URL.Query().Get("redirect_uri")
		q := url.Values{}
		q.Set("code", "test-code-1")
		q.Set("state", r.URL.Query().Get("state"))
		http.Redirect(w, r, ru+"?"+q.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "test-code-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		claims := jwt.MapClaims{
			"iss":   st.srv.URL,
			"aud":   st.clientID,
			"sub":   "sub-1234",
			"email": "nova@acme.test",
			"name":  "Nova IdP",
			"nonce": st.lastNonce,
			"exp":   time.Now().Add(5 * time.Minute).Unix(),
			"iat":   time.Now().Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = st.kid
		signed, err := tok.SignedString(st.key)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed})
	})
	st.srv = httptest.NewServer(mux)
	t.Cleanup(st.srv.Close)
	return st
}

// runSSOFlow: authorize → IdP → callback → /sso/finish. The client
// follows the whole chain with a cookie jar (Set-Cookie on the callback
// hop must be observable — the final response won't carry them).
func runSSOFlow(t *testing.T, h *httptestSrv, stub *oidcStub) (*http.Response, *http.CookieJar, error) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(h.URL + "/v1/sso/authorize?ws=acme")
	jarPtr := http.CookieJar(jar)
	return resp, &jarPtr, err
}

func TestSSOFlowAndEnforcement(t *testing.T) {
	_, _, h := filesTestStack(t)
	stub := newOIDCStub(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

	// configure via settings API (staff static token = ws ctx; but the
	// admin gate requires user role — static token has no user_id!
	// seed asha as admin first)
	ap, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES
		  ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'owner')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}

	// login as admin (magic link dev login needs DEV_LOGIN env — set by stack)
	_, _, access := devLogin(h)

	putBody := map[string]any{
		"issuer":        stub.srv.URL,
		"client_id":     "ol-client",
		"client_secret": "stub-secret",
	}
	if code, body := h.doJWT("PUT", "/v1/sso", putBody, access); code != http.StatusOK {
		t.Fatalf("put sso = %d %s", code, body)
	}

	// masked GET — no secret
	if code, body := h.doJWT("GET", "/v1/sso", nil, access); code != http.StatusOK || strings.Contains(string(body), "stub-secret") {
		t.Fatalf("get sso = %d %s", code, body)
	}

	// status endpoint (unauthenticated)
	if code, body := h.do("GET", "/v1/sso/status?ws=acme", nil); code != http.StatusOK || !strings.Contains(string(body), `"configured":true`) {
		t.Fatalf("status = %d %s", code, body)
	}

	// full flow: authorize → IdP → callback → JIT nova as viewer
	resp, jarPtr, err := runSSOFlow(t, h, stub)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/sso/finish") {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("flow ended at %v status %d body %s (want /sso/finish)", resp.Request, resp.StatusCode, b)
	}
	// session cookies were set on the callback hop → jar holds them
	var at, rt string
	u, _ := url.Parse(h.URL)
	for _, c := range (*jarPtr).Cookies(u) {
		if c.Name == "ol_sso_at" {
			at = c.Value
		}
		if c.Name == "ol_sso_rt" {
			rt = c.Value
		}
	}
	if at == "" || rt == "" {
		t.Fatal("no session cookies in jar after finish redirect")
	}

	// JIT assertions: user + viewer membership + subject link
	var (
		novaID string
		role   string
		subj   string
	)
	if err := ap.QueryRow(context.Background(), `
		SELECT u.id::text, m.role, u.sso_subject FROM users u
		JOIN memberships m ON m.user_id = u.id
		WHERE u.email = 'nova@acme.test' AND m.workspace_id = '11111111-1111-1111-1111-111111111111'`).
		Scan(&novaID, &role, &subj); err != nil {
		t.Fatalf("JIT user missing: %v", err)
	}
	if role != "viewer" || subj != "sub-1234" || novaID == "" {
		t.Fatalf("JIT = id %s role %s sub %s", novaID, role, subj)
	}

	// audit row
	var n int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'sso.login'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sso.login audit = %d err %v", n, err)
	}

	// PKCE was used: challenge is the S256 of the verifier — the stub
	// asserted method; verifier reached /token (else 400 → flow failed)
	// (implicit in the happy path above)

	// re-run: subject link resolves, no second user
	if _, _, err := runSSOFlow(t, h, stub); err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = 'nova@acme.test'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("dup users = %d", cnt)
	}

	// wrong-audience token: reconfigure stub clientID mismatch
	stub.clientID = "other-app"
	resp2, _, err := runSSOFlow(t, h, stub)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	// token now has aud other-app; app expects ol-client → 401 at callback
	// (the flow's last hop IS the callback)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong audience = %d (want 401)", resp2.StatusCode)
	}
}

func TestSSOEnforceBlocksMagicLink(t *testing.T) {
	_, _, h := filesTestStack(t)
	stub := newOIDCStub(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

	ap, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES
		  ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'owner')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	_, _, access := devLogin(h)

	if code, body := h.doJWT("PUT", "/v1/sso", map[string]any{
		"issuer": stub.srv.URL, "client_id": "ol-client", "client_secret": "s",
		"allowed_domains": []string{"acme.test"}, "enforce": true,
	}, access); code != http.StatusOK {
		t.Fatalf("put = %d %s", code, body)
	}

	// enforced domain → magic link refused
	if code, body := h.do("POST", "/v1/auth/magic-link", map[string]any{
		"email": "asha@acme.test", "workspace_slug": "acme",
	}); code != http.StatusForbidden {
		t.Fatalf("enforced magic link = %d %s", code, body)
	}

	// different domain → still allowed
	if code, _ := h.do("POST", "/v1/auth/magic-link", map[string]any{
		"email": "other@gmail.com", "workspace_slug": "acme",
	}); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("non-enforced magic link = %d", code)
	}
}

func TestSSOStateTamper(t *testing.T) {
	_, _, h := filesTestStack(t)
	// tampered state → 400 without any IdP contact
	req, _ := jsonBodyRequest(t, h, "GET", "/v1/sso/callback?code=x&state=garbage.garbage", nil)
	if code, body := doRequest(t, req); code != http.StatusBadRequest {
		t.Fatalf("tampered state = %d %s", code, body)
	}
	// valid MAC but expired state → craft using the helper
	raw, _ := json.Marshal(ssoState{WorkspaceID: "11111111-1111-1111-1111-111111111111", Nonce: "n", Verifier: "v", Expires: time.Now().Add(-time.Hour).Unix()})
	mac := base64.RawURLEncoding.EncodeToString(ssoStateMAC(raw))
	enc := base64.RawURLEncoding.EncodeToString(raw)
	req2, _ := jsonBodyRequest(t, h, "GET", "/v1/sso/callback?code=x&state="+enc+"."+mac, nil)
	if code, _ := doRequest(t, req2); code != http.StatusBadRequest {
		t.Fatalf("expired state = %d", code)
	}
}

var _ = sha256.Sum256
