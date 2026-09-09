package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

// Auth core (issue #39, spec §16): magic-link → JWT(15m) + rotating refresh(30d).
// JWT carries sub(user_id), ws(workspace_id), role — middleware sets
// app.user_id/app.workspace_id so RLS + audit attribution work unchanged.

const (
	accessTTL  = 15 * time.Minute
	refreshTTL = 30 * 24 * time.Hour
	loginTTL   = 15 * time.Minute
)

func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func jwtSecret() []byte {
	s := os.Getenv("OPENLANE_JWT_SECRET")
	if s == "" {
		// ponytail: dev fallback — CI/tests only; prod requires real secret (fail closed at startup in prod profile later)
		return []byte("dev-secret-change-me")
	}
	return []byte(s)
}

type sessionTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	UserID       string `json:"user_id"`
	WorkspaceID  string `json:"workspace_id"`
	Role         string `json:"role"`
}

func issueTokens(userID, workspaceID, role string) (sessionTokens, string, error) {
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  userID,
		"ws":   workspaceID,
		"role": role,
		"exp":  time.Now().Add(accessTTL).Unix(),
		"iat":  time.Now().Unix(),
	}).SignedString(jwtSecret())
	if err != nil {
		return sessionTokens{}, "", err
	}
	refresh, err := randToken()
	if err != nil {
		return sessionTokens{}, "", err
	}
	family := newUUID()
	return sessionTokens{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		UserID:       userID,
		WorkspaceID:  workspaceID,
		Role:         role,
	}, family, nil
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hashTok: hex sha256 of the token string (same scheme as portal links).
func hashTok(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Server) requestMagicLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email         string `json:"email"`
		WorkspaceSlug string `json:"workspace_slug"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		!strings.Contains(req.Email, "@") || req.WorkspaceSlug == "" {
		problem(w, http.StatusBadRequest, "email and workspace_slug required")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 404-no-oracle: unknown email/slug still returns 202. Membership check
	// happens at consume; probing gives nothing away here.
	// workspaces has no RLS (root tenant table, slug lookups); memberships
	// is RLS'd — so set the workspace ctx BEFORE querying it.
	var wsID string
	_ = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE slug = $1`, req.WorkspaceSlug).Scan(&wsID)

	// §17-2 brute-force throttle: 10/IP/hr, 5/email/hr (§7.4-E2 resend limit).
	// login_tokens has no RLS — counts work without tenant ctx.
	ipTries, emailTries := 0, 0
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = h // ports churn per connection; throttle the host
	}
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM login_tokens
		WHERE created_ip = $1 AND created_at > now() - interval '1 hour'`, ip).Scan(&ipTries)
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM login_tokens
		WHERE email = $1 AND created_at > now() - interval '1 hour'`, req.Email).Scan(&emailTries)
	if ipTries >= 10 || emailTries >= 5 {
		w.Header().Set("Retry-After", "3600")
		problem(w, http.StatusTooManyRequests, "too many login requests — try again later")
		return
	}

	// Always mint + store a token (member or not) so probing consumes the
	// rate-limit buckets identically; non-members' tokens simply never
	// consume (no membership row to join). No response oracle either way.
	resp := map[string]any{}
	if wsID != "" {
		if _, err := tx.Exec(ctx,
			"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", wsID); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		var userID string
		var role string
		isMember := tx.QueryRow(ctx, `
			SELECT m.user_id, m.role FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE lower(u.email) = lower($1) AND m.workspace_id = $2`, req.Email, wsID).Scan(&userID, &role) == nil

		tok, err2 := randToken()
		if err2 == nil {
			if _, err3 := tx.Exec(ctx, `
				INSERT INTO login_tokens (email, workspace_id, token_hash, expires_at, created_ip)
				VALUES ($1, $2, $3, now() + interval '15 minutes', $4)`,
				req.Email, wsID, hashTok(tok), ip); err3 == nil && isMember {
				if os.Getenv("OPENLANE_DEV_LOGIN") == "1" {
					resp["dev_token"] = tok // no SMTP at P0 — the email pillar wires real sending
				}
				// ponytail: real email send lands with the notifications pillar; log line stands in
				fmt.Fprintf(os.Stderr, "MAGIC LINK for %s [%s]: /v1/auth/magic-link/consume?token=%s\n", req.Email, req.WorkspaceSlug, tok)
			}
		}
	}
	_ = tx.Commit(ctx)

	// Opportunistic housekeeping (~1% of requests): expired login tokens
	// and long-revoked sessions. ponytail: in-request sweep; a cron/worker
	// takes over when River lands.
	if seedRand()%100 == 0 {
		_, _ = s.pool.Exec(ctx, `DELETE FROM login_tokens WHERE expires_at < now() - interval '1 day'`)
		_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() - interval '7 days'`)
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func seedRand() int {
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	return int(b[0])
}

func (s *Server) consumeMagicLink(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if len(tok) < 43 || len(tok) > 128 {
		problem(w, http.StatusUnauthorized, "invalid token")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// login_tokens has no RLS (system table) — resolve the token's workspace
	// first, then set ctx so the memberships join (RLS'd) can see rows.
	var wsID string
	err = tx.QueryRow(ctx, `
		SELECT workspace_id FROM login_tokens
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`,
		hashTok(tok)).Scan(&wsID)
	if err == pgx.ErrNoRows {
		problem(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", wsID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	var userID, role string
	err = tx.QueryRow(ctx, `
		UPDATE login_tokens lt SET used_at = now()
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE lt.token_hash = $1
		  AND m.workspace_id = lt.workspace_id AND m.user_id = u.id
		  AND lower(u.email) = lower(lt.email)
		RETURNING m.user_id, m.role`, hashTok(tok)).Scan(&userID, &role)
	if err == pgx.ErrNoRows {
		problem(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	st, family, err := issueTokens(userID, wsID, role)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (user_id, workspace_id, refresh_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4, now() + interval '30 days')`,
		userID, wsID, hashTok(st.RefreshToken), family); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// refreshSession rotates; reuse of a rotated token kills the family (§17-6).
func (s *Server) refreshSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || len(req.RefreshToken) < 43 {
		problem(w, http.StatusBadRequest, "refresh_token required")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sessID, userID, wsID, family string
	var role string
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, workspace_id, family_id FROM sessions
		WHERE refresh_hash = $1 AND revoked_at IS NULL AND expires_at > now()`,
		hashTok(req.RefreshToken)).Scan(&sessID, &userID, &wsID, &family)
	if err == pgx.ErrNoRows {
		// unknown or REVOKED. If the hash matches a REVOKED row → reuse of a
		// rotated token → theft: revoke the whole family.
		var revFamily string
		err2 := tx.QueryRow(ctx, `SELECT family_id FROM sessions WHERE refresh_hash = $1 AND revoked_at IS NOT NULL`, hashTok(req.RefreshToken)).Scan(&revFamily)
		if err2 == nil {
			_, _ = tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, revFamily)
			_ = tx.Commit(ctx)
			problem(w, http.StatusUnauthorized, "token reuse detected — session family revoked")
			return
		}
		problem(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// memberships is RLS'd — set the session's workspace ctx before the role lookup
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", wsID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.QueryRow(ctx, `SELECT role FROM memberships WHERE user_id = $1 AND workspace_id = $2`, userID, wsID).Scan(&role); err != nil {
		problem(w, http.StatusUnauthorized, "membership revoked")
		return
	}

	st, _, err := issueTokens(userID, wsID, role)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// rotate: revoke old, insert new (same family)
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now(), last_used_at = now() WHERE id = $1`, sessID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (user_id, workspace_id, refresh_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4, now() + interval '30 days')`,
		userID, wsID, hashTok(st.RefreshToken), family); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || len(req.RefreshToken) < 43 {
		problem(w, http.StatusBadRequest, "refresh_token required")
		return
	}
	ctx := r.Context()
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE refresh_hash = $1 AND revoked_at IS NULL`, hashTok(req.RefreshToken))
	if err != nil || tag.RowsAffected() == 0 {
		problem(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	// revoke whole family on explicit logout (belt and braces)
	_, _ = s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now()
		WHERE family_id = (SELECT family_id FROM sessions WHERE refresh_hash = $1) AND revoked_at IS NULL`, hashTok(req.RefreshToken))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	uid, ws := userFromCtx(r.Context()), workspaceFromCtx(r.Context())
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", ws); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var out struct {
		UserID        string `json:"user_id"`
		Email         string `json:"email"`
		DisplayName   string `json:"display_name"`
		WorkspaceID   string `json:"workspace_id"`
		WorkspaceName string `json:"workspace_name"`
		Role          string `json:"role"`
	}
	err = tx.QueryRow(ctx, `
		SELECT u.id, u.email, u.display_name, w.id, w.name, m.role
		FROM memberships m JOIN users u ON u.id = m.user_id JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = $1 AND m.workspace_id = $2`, uid, ws).
		Scan(&out.UserID, &out.Email, &out.DisplayName, &out.WorkspaceID, &out.WorkspaceName, &out.Role)
	if err != nil {
		problem(w, http.StatusUnauthorized, "no membership")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) myWorkspaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// memberships is RLS'd per-workspace; a cross-workspace list needs the
	// ctx cleared per row — use SECURITY DEFINER view-free approach: query as
	// system via the login path. Simplest P0: workspaces join via a lateral
	// on memberships WITH ctx set per candidate... actually the honest P0:
	// memberships rows are user-owned; grant the app role a helper.
	// KISS: query via a SECURITY DEFINER function user_memberships(uid).
	rows, err := s.pool.Query(ctx, `SELECT * FROM user_memberships($1)`, userFromCtx(ctx))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, slug, role string
		if err := rows.Scan(&id, &name, &slug, &role); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, map[string]any{"workspace_id": id, "name": name, "slug": slug, "role": role})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- JWT middleware (replaces static bearer for staff routes) ----

type userKey struct{}

func userFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(userKey{}).(string); ok {
		return v
	}
	return ""
}

var errBadToken = fmt.Errorf("bad token")

func (s *Server) parseAccess(tokenStr string) (userID, wsID, role string, err error) {
	tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errBadToken
		}
		return jwtSecret(), nil
	})
	if err != nil || !tok.Valid {
		return "", "", "", errBadToken
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", "", errBadToken
	}
	userID, _ = claims["sub"].(string)
	wsID, _ = claims["ws"].(string)
	role, _ = claims["role"].(string)
	if userID == "" || wsID == "" {
		return "", "", "", errBadToken
	}
	return userID, wsID, role, nil
}

// auth wraps staff handlers: JWT bearer (or static token when
// OPENLANE_ALLOW_STATIC_TOKEN=1 for CI/e2e). Sets app.user_id +
// app.workspace_id so every downstream query + audit row is attributed.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var got string
		_, err := fmt.Sscanf(r.Header.Get("Authorization"), "Bearer %s", &got)
		if err != nil || got == "" {
			problem(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		userID, wsID, role := "", "", ""
		if s.staticAllowed() && subtle.ConstantTimeCompare([]byte(got), []byte(s.staffToken)) == 1 && s.staffToken != "" {
			// CI/e2e static identity: full scope, attributed as system
			userID, wsID, role = "", r.Header.Get("X-Workspace-Id"), "admin"
			if wsID == "" {
				problem(w, http.StatusBadRequest, "X-Workspace-Id required with static token")
				return
			}
		} else {
			userID, wsID, role, err = s.parseAccess(got)
			if err != nil {
				problem(w, http.StatusUnauthorized, "invalid or expired token")
				return
			}
		}
		ctx := r.Context()
		ctx = context.WithValue(ctx, userKey{}, userID)
		ctx = context.WithValue(ctx, wsKey{}, wsID)
		ctx = context.WithValue(ctx, roleKey{}, role)
		next(w, r.WithContext(ctx))
	}
}

type roleKey struct{}

func (s *Server) staticAllowed() bool {
	return os.Getenv("OPENLANE_ALLOW_STATIC_TOKEN") == "1" && s.staffToken != ""
}
