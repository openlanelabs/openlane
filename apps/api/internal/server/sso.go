package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

// OIDC SSO v1 (P1 §406). One IdP per workspace, PKCE S256, JIT
// provisioning with viewer role (§424). SCIM stays Phase2 per spec.
// Secrets sealed like the other integration credentials.

// ---- discovery + JWKS (admin-configured issuer, in-memory cache) ----

type oidcDiscovery struct {
	Issuer   string `json:"issuer"`
	AuthURL  string `json:"authorization_endpoint"`
	TokenURL string `json:"token_endpoint"`
	JWKSURL  string `json:"jwks_uri"`
}

type oidcJWK struct {
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kty string `json:"kty"`
}

type oidcJWKS struct {
	Keys []oidcJWK `json:"keys"`
}

type jwkCacheEntry struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var (
	jwksCacheMu  sync.Mutex
	jwksCache    = map[string]jwkCacheEntry{} // keyed by issuer
	discoCacheMu sync.Mutex
	discoCache   = map[string]oidcDiscovery{}
	discoCacheAt = map[string]time.Time{}
)

// fetchDiscovery: {issuer}/.well-known/openid-configuration, cached 1h.
func fetchDiscovery(ctx context.Context, issuer string) (oidcDiscovery, error) {
	discoCacheMu.Lock()
	if d, ok := discoCache[issuer]; time.Since(discoCacheAt[issuer]) < time.Hour && ok {
		discoCacheMu.Unlock()
		return d, nil
	}
	discoCacheMu.Unlock()

	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return oidcDiscovery{}, err
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return oidcDiscovery{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return oidcDiscovery{}, errors.New("discovery fetch failed")
	}
	var d oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&d); err != nil {
		return oidcDiscovery{}, err
	}
	if d.Issuer == "" || d.AuthURL == "" || d.TokenURL == "" || d.JWKSURL == "" {
		return oidcDiscovery{}, errors.New("discovery incomplete")
	}
	discoCacheMu.Lock()
	discoCache[issuer] = d
	discoCacheAt[issuer] = time.Now()
	discoCacheMu.Unlock()
	return d, nil
}

// fetchJWKS: keys cached 1h per issuer.
func fetchJWKS(ctx context.Context, issuer, jwksURL string) (map[string]*rsa.PublicKey, error) {
	jwksCacheMu.Lock()
	if e, ok := jwksCache[issuer]; ok && time.Since(e.fetched) < time.Hour {
		jwksCacheMu.Unlock()
		return e.keys, nil
	}
	jwksCacheMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("jwks fetch failed")
	}
	var ks oidcJWKS
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&ks); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range ks.Keys {
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		// JWK e is the big-endian exponent (usually AQAB)
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e == 0 || len(nb) == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	jwksCacheMu.Lock()
	jwksCache[issuer] = jwkCacheEntry{keys: keys, fetched: time.Now()}
	jwksCacheMu.Unlock()
	return keys, nil
}

// ---- settings ----

// validOIDCIssuer: https in prod; plain loopback for tests/dev stubs.
func validOIDCIssuer(u string) bool {
	if strings.HasPrefix(u, "https://") {
		return true
	}
	return strings.HasPrefix(u, "http://127.0.0.1") || strings.HasPrefix(u, "http://localhost")
}

// putSSOConfig: PUT /v1/sso — admin only (§127 ops admin owns SSO).
func (s *Server) putSSOConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Issuer         string   `json:"issuer"`
		ClientID       string   `json:"client_id"`
		ClientSecret   string   `json:"client_secret"`
		AllowedDomains []string `json:"allowed_domains"`
		Enforce        bool     `json:"enforce"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if !validOIDCIssuer(req.Issuer) {
		problem(w, http.StatusBadRequest, "issuer must be an https URL")
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		problem(w, http.StatusBadRequest, "client_id and client_secret required")
		return
	}
	for _, d := range req.AllowedDomains {
		if d == "" || strings.Contains(d, "@") || len(d) > 200 {
			problem(w, http.StatusBadRequest, "allowed_domains must be bare domains")
			return
		}
	}
	if req.AllowedDomains == nil {
		req.AllowedDomains = []string{}
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	sealed, err := sealSecret(aead, req.ClientSecret)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// admin gate
	if !isAdminRole(ctx, tx) {
		problem(w, http.StatusForbidden, "admin role required")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sso_configs (workspace_id, issuer, client_id, client_secret_enc, allowed_domains, enforce)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id) DO UPDATE SET
		  issuer = $1, client_id = $2, client_secret_enc = $3, allowed_domains = $4, enforce = $5, updated_at = now()`,
		req.Issuer, req.ClientID, sealed, req.AllowedDomains, req.Enforce); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'sso_config', NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'sso.configured', 'api', $1)`,
		mustJSON(map[string]any{"issuer": req.Issuer, "enforce": req.Enforce, "allowed_domains": req.AllowedDomains})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "issuer": req.Issuer, "enforce": req.Enforce})
}

// getSSOConfig: GET /v1/sso — masked.
func (s *Server) getSSOConfig(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var issuer string
	var domains []string
	var enforce bool
	err := tx.QueryRow(r.Context(), `
		SELECT issuer, allowed_domains, enforce FROM sso_configs
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&issuer, &domains, &enforce)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if domains == nil {
		domains = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "issuer": issuer, "allowed_domains": domains, "enforce": enforce,
	})
}

// isAdminRole: the caller's membership role is owner/admin.
func isAdminRole(ctx context.Context, tx pgx.Tx) bool {
	var role string
	if err := tx.QueryRow(ctx, `
		SELECT m.role FROM memberships m
		WHERE m.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid`).
		Scan(&role); err != nil {
		return false
	}
	return role == "owner" || role == "admin"
}

// ---- the flow ----

type ssoState struct {
	WorkspaceID string `json:"ws"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	Expires     int64  `json:"exp"`
}

// ssoStateMAC: HMAC over the state JSON with the JWT secret — the
// browser carries it; we verify on the way back (no server session).
func ssoStateMAC(b []byte) []byte {
	m := hmacSHA256(jwtSecret(), b)
	return m
}

// ssoAuthorize: GET /v1/sso/authorize?ws=<slug>
// Unauthenticated endpoint (login flow starts pre-session). 302 to IdP.
func (s *Server) ssoAuthorize(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("ws")
	if slug == "" {
		problem(w, http.StatusBadRequest, "ws query parameter required")
		return
	}
	ctx := r.Context()
	wsID, issuer, clientID, err := ssoAuthorizeConfigRow(ctx, s.pool, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "no sso configured")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	disco, err := fetchDiscovery(ctx, issuer)
	if err != nil {
		problem(w, http.StatusInternalServerError, "identity provider unreachable")
		return
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	verifier := make([]byte, 32)
	if _, err := rand.Read(verifier); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	st := ssoState{
		WorkspaceID: wsID,
		Nonce:       base64.RawURLEncoding.EncodeToString(nonce),
		Verifier:    base64.RawURLEncoding.EncodeToString(verifier),
		Expires:     time.Now().Add(10 * time.Minute).Unix(),
	}
	raw, _ := json.Marshal(st)
	enc := base64.RawURLEncoding.EncodeToString(raw)
	mac := base64.RawURLEncoding.EncodeToString(ssoStateMAC(raw))

	verSum := sha256.Sum256([]byte(st.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(verSum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", externalBaseURL(r)+"/v1/sso/callback")
	q.Set("scope", "openid email profile")
	q.Set("state", enc+"."+mac)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	http.Redirect(w, r, disco.AuthURL+"?"+q.Encode(), http.StatusFound)
}

// externalBaseURL: the API's own reachable base (deployed behind the
// web proxy in prod). v1: env-configured; dev default localhost.
func externalBaseURL(r *http.Request) string {
	if b := r.Header.Get("X-Forwarded-Base"); b != "" {
		return b
	}
	if b := os.Getenv("OPENLANE_EXTERNAL_URL"); b != "" {
		return b
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ssoCallback: GET /v1/sso/callback?code&state — verifies state MAC,
// exchanges the code, validates the ID token (iss/aud/exp/nonce per
// §424), links/JITs the user, issues a session, 302 to the web app.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	stateRaw := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	parts := strings.Split(stateRaw, ".")
	if len(parts) != 2 || code == "" {
		problem(w, http.StatusBadRequest, "malformed callback")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		problem(w, http.StatusBadRequest, "malformed state")
		return
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		problem(w, http.StatusBadRequest, "malformed state")
		return
	}
	if !hmac.Equal(ssoStateMAC(raw), mac) {
		problem(w, http.StatusBadRequest, "state verification failed")
		return
	}
	var st ssoState
	if err := json.Unmarshal(raw, &st); err != nil {
		problem(w, http.StatusBadRequest, "malformed state")
		return
	}
	if time.Now().Unix() > st.Expires {
		problem(w, http.StatusBadRequest, "state expired")
		return
	}

	ctx := r.Context()
	var (
		issuer, clientID string
		secretEnc        []byte
		domains          []string
		enforce          bool
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT issuer, client_id, client_secret_enc, allowed_domains, enforce
		FROM sso_config_by_ws($1::uuid)`, st.WorkspaceID).
		Scan(&issuer, &clientID, &secretEnc, &domains, &enforce); err != nil {
		problem(w, http.StatusNotFound, "no sso configured")
		return
	}
	d, err := fetchDiscovery(ctx, issuer)
	if err != nil {
		problem(w, http.StatusInternalServerError, "identity provider unreachable")
		return
	}

	// exchange
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", externalBaseURL(r)+"/v1/sso/callback")
	form.Set("client_id", clientID)
	form.Set("code_verifier", st.Verifier)
	tokReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRes, err := (&http.Client{Timeout: 10 * time.Second}).Do(tokReq)
	if err != nil {
		problem(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	defer func() { _ = tokRes.Body.Close() }()
	if tokRes.StatusCode != http.StatusOK {
		problem(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tokRes.Body, 1<<20)).Decode(&tok); err != nil || tok.IDToken == "" {
		problem(w, http.StatusBadGateway, "no id_token")
		return
	}

	// verify id token (§424: signature, issuer, audience, expiry, nonce)
	keys, err := fetchJWKS(ctx, issuer, d.JWKSURL)
	if err != nil {
		problem(w, http.StatusBadGateway, "jwks fetch failed")
		return
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok.IDToken, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, errors.New("unexpected signing method")
		}
		kid, _ := t.Header["kid"].(string)
		k, ok := keys[kid]
		if !ok {
			return nil, errors.New("unknown kid")
		}
		return k, nil
	}, jwt.WithIssuer(issuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		problem(w, http.StatusUnauthorized, "id token invalid")
		return
	}
	if n, _ := claims["nonce"].(string); n != st.Nonce {
		problem(w, http.StatusUnauthorized, "nonce mismatch")
		return
	}
	email, _ := claims["email"].(string)
	sub, _ := claims["sub"].(string)
	name, _ := claims["name"].(string)
	if email == "" || sub == "" {
		problem(w, http.StatusUnauthorized, "id token missing email/sub")
		return
	}

	// domain gate
	if len(domains) > 0 && !domainAllowed(email, domains) {
		problem(w, http.StatusForbidden, "email domain not allowed for this workspace")
		return
	}

	// link or JIT (viewer per §424), issue session — all in one tx
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", st.WorkspaceID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var (
		userID, role string
	)
	// 1) existing link by subject
	err = tx.QueryRow(ctx, `
		SELECT u.id::text, m.role FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
		WHERE u.sso_subject = $2`, st.WorkspaceID, sub).Scan(&userID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		// 2) link by email
		err = tx.QueryRow(ctx, `
			SELECT u.id::text, m.role FROM users u
			JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
			WHERE lower(u.email) = lower($2)`, st.WorkspaceID, email).Scan(&userID, &role)
		if err == nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET sso_subject = $1, updated_at = now() WHERE id = $2::uuid`, sub, userID); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// 3) JIT: user + viewer membership (§424)
		displayName := name
		if displayName == "" {
			displayName = strings.Split(email, "@")[0]
		}
		err = tx.QueryRow(ctx, `
			WITH u AS (
				INSERT INTO users (email, display_name, sso_subject)
				VALUES (lower($2), $3, $4)
				ON CONFLICT (email) DO UPDATE SET sso_subject = $4, updated_at = now()
				RETURNING id, email, display_name
			)
			INSERT INTO memberships (workspace_id, user_id, role)
			SELECT $1::uuid, u.id, 'viewer' FROM u
			ON CONFLICT DO NOTHING
			RETURNING (SELECT u.id::text FROM u), 'viewer'`, st.WorkspaceID, email, displayName, sub).Scan(&userID, &role)
		if err == nil {
			role = "viewer"
		} else if errors.Is(err, pgx.ErrNoRows) {
			// membership existed without subject link (rare): resolve role
			if err2 := tx.QueryRow(ctx, `
				SELECT u.id::text, m.role FROM users u
				JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
				WHERE lower(u.email) = lower($2)`, st.WorkspaceID, email).Scan(&userID, &role); err2 != nil {
				problem(w, http.StatusForbidden, "not a member of this workspace")
				return
			}
		} else {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// session tokens (issueTokens is the magic-link path's own fn)
	// role ctx for audit
	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES ($1, 'user', $2, 'user', $2, 'sso.login', 'api', $3)`,
		st.WorkspaceID, userID, mustJSON(map[string]string{"email": email, "role": role})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	toks, _, err := issueTokens(userID, st.WorkspaceID, role)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// hand tokens to the web app via short-lived redirect cookies
	http.SetCookie(w, &http.Cookie{Name: "ol_sso_at", Value: toks.AccessToken, Path: "/", MaxAge: 60, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "ol_sso_rt", Value: toks.RefreshToken, Path: "/", MaxAge: 60, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/sso/finish", http.StatusFound)
}

// domainAllowed: email domain ∈ allowed_domains (case-insensitive).
func domainAllowed(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	d := strings.ToLower(email[at+1:])
	for _, a := range domains {
		if strings.ToLower(a) == d {
			return true
		}
	}
	return false
}

// ssoAuthorizeConfig: slug → config row for the authorize redirect
// (cross-tenant pre-auth lookup, SECURITY DEFINER, no secret).
func ssoAuthorizeConfigRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, slug string) (wsID, issuer, clientID string, err error) {
	err = q.QueryRow(ctx, `SELECT workspace_id::text, issuer, client_id FROM sso_authorize_for($1)`, slug).
		Scan(&wsID, &issuer, &clientID)
	return
}

// ssoStatus: GET /v1/sso/status?ws=<slug> — unauthenticated: does this
// workspace have SSO (login page shows the button) + is this email
// domain enforced. No secrets in the response.
func (s *Server) ssoStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("ws")
	if slug == "" {
		problem(w, http.StatusBadRequest, "ws query parameter required")
		return
	}
	var enforce bool
	var domains []string
	err := s.pool.QueryRow(r.Context(),
		`SELECT enforce, allowed_domains FROM sso_status_for($1)`, slug).
		Scan(&enforce, &domains)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if domains == nil {
		domains = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "enforce": enforce, "allowed_domains": domains,
	})
}
