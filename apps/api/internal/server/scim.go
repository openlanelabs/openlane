package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SCIM 2.0 user provisioning (§406 Phase2, completes §590 P1
// 'SCIM/SSO'). The 4 endpoints IdPs actually call; Bearer-token
// auth where the token maps to the tenant (no session, no
// workspace header — the IdP is the caller). Deprovisioning is the
// half that matters: ex-employees keeping access is the classic
// PSA breach, so active:false revokes membership NOW.
//
// SCIM id = users.id (stable, opaque, global). Deactivation =
// memberships.deactivated_at (role untouched — reactivation is a
// no-op beyond clearing the stamp; role CHECK stays intact).
// Users are never hard-deleted (audit trail) — RFC 7644
// soft-delete semantics.

// scimUser: RFC 7643 core User schema (the fields IdPs sync).
type scimUser struct {
	Schemas  []string `json:"schemas,omitempty"`
	ID       string   `json:"id,omitempty"`
	UserName string   `json:"userName"`
	Active   *bool    `json:"active,omitempty"`
	Name     struct {
		GivenName  string `json:"givenName,omitempty"`
		FamilyName string `json:"familyName,omitempty"`
	} `json:"name,omitempty"`
	Emails []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	} `json:"emails,omitempty"`
	Meta struct {
		ResourceType string `json:"resourceType,omitempty"`
		Created      string `json:"created,omitempty"`
		LastModified string `json:"lastModified,omitempty"`
	} `json:"meta,omitempty"`
}

func scimErr(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"},
		"status":  status,
		"detail":  detail,
	})
}

// scimBearer: resolve workspace from the Authorization header.
// Constant-time compare on the unsealed token.
func (s *Server) scimBearer(ctx context.Context, r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return "", false
	}
	presented := strings.TrimPrefix(h, p)

	aead, err := sfSecretAEAD()
	if err != nil {
		return "", false
	}
	rows, err := s.pool.Query(ctx, `SELECT workspace_id::text, token_enc FROM scim_resolve($1)`, presented)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	type cand struct {
		ws     string
		sealed []byte
	}
	cands := []cand{}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.ws, &c.sealed); err != nil {
			continue
		}
		cands = append(cands, c)
	}
	for _, c := range cands {
		plain, err := openSecret(aead, c.sealed)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(plain), []byte(presented)) == 1 {
			return c.ws, true
		}
	}
	return "", false
}

// scimTx: a tx with the resolved tenant in RLS context — every SCIM
// membership query runs tenant-scoped like an authed request.
func (s *Server) scimTx(ctx context.Context, ws string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", ws); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// putSCIMConfig: PUT /v1/scim (admin) — set or clear the bearer
// token. Random 32B, shown ONCE in the response.
func (s *Server) putSCIMConfig(w http.ResponseWriter, r *http.Request) {
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
	if !isAdminRole(ctx, tx) {
		problem(w, http.StatusForbidden, "admin role required")
		return
	}

	var req struct {
		Clear bool `json:"clear"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}

	if req.Clear {
		if _, err := tx.Exec(ctx, `DELETE FROM scim_configs
			WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": nil})
		return
	}

	// generate + seal
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	token := "scim_" + hex.EncodeToString(raw)
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	sealed, err := sealSecret(aead, token)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO scim_configs (workspace_id, token_enc)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1)
		ON CONFLICT (workspace_id) DO UPDATE SET token_enc = $1, updated_at = now()`,
		sealed); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token}) // shown once
}

// scimCreate: POST /scim/v2/Users — upsert-by-email provision.
func (s *Server) scimCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.scimBearer(ctx, r)
	if !ok {
		scimErr(w, http.StatusUnauthorized, "invalid SCIM bearer token")
		return
	}

	tx, txErr := s.scimTx(ctx, ws)
	if txErr != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var u scimUser
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&u); err != nil {
		scimErr(w, http.StatusBadRequest, "invalid SCIM payload")
		return
	}
	email := strings.ToLower(strings.TrimSpace(u.UserName))
	if email == "" || !strings.Contains(email, "@") {
		scimErr(w, http.StatusBadRequest, "userName must be an email")
		return
	}

	// existing user by email?
	var uid string
	err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE lower(email) = $1`, email).Scan(&uid)
	if err == nil {
		// ensure membership exists (or reactivate if deactivated)
		if _, err := tx.Exec(ctx, `
			INSERT INTO memberships (workspace_id, user_id, role)
			VALUES ($1::uuid, $2::uuid, 'viewer')
			ON CONFLICT DO NOTHING`, ws, uid); err != nil {
			scimErr(w, http.StatusInternalServerError, "provision failed")
			return
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET deactivated_at = NULL
			WHERE workspace_id = $1::uuid AND user_id = $2::uuid AND deactivated_at IS NOT NULL`,
			ws, uid); err != nil {
			scimErr(w, http.StatusInternalServerError, "provision failed")
			return
		}
		_ = tx.Commit(ctx)
		w.WriteHeader(http.StatusConflict) // IdPs expect 409 on dup
		writeScimUser(w, s, ctx, ws, uid, email)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		scimErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	// create user + viewer membership (§424 JIT parity)
	name := u.Name.GivenName + " " + u.Name.FamilyName
	if strings.TrimSpace(name) == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (email, display_name)
		VALUES ($1, $2) RETURNING id::text`, email, strings.TrimSpace(name)).Scan(&uid); err != nil {
		scimErr(w, http.StatusInternalServerError, "provision failed")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1::uuid, $2::uuid, 'viewer')`,
		ws, uid); err != nil {
		scimErr(w, http.StatusInternalServerError, "provision failed")
		return
	}
	_ = tx.Commit(ctx)
	w.WriteHeader(http.StatusCreated)
	writeScimUser(w, s, ctx, ws, uid, email)
}

// writeScimUser: render the RFC 7643 shape from live rows. Owns its
// read tx (the caller's tx may already be committed).
func writeScimUser(w http.ResponseWriter, s *Server, ctx context.Context, ws, uid, email string) {
	tx, txErr := s.scimTx(ctx, ws)
	if txErr != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	active := true
	var created time.Time
	var dn string
	_ = tx.QueryRow(ctx, `
		SELECT u.created_at, u.display_name,
		       (m.deactivated_at IS NULL) FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
		WHERE u.id = $2::uuid`, ws, uid).Scan(&created, &dn, &active)
	out := scimUser{UserName: email}
	out.Schemas = []string{"urn:ietf:params:scim:schemas:core:2.0:User"}
	out.ID = uid
	out.Active = &active
	out.Name.FamilyName = dn
	out.Meta.ResourceType = "User"
	out.Meta.Created = created.UTC().Format(time.RFC3339)
	out.Meta.LastModified = time.Now().UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/scim+json")
	_ = json.NewEncoder(w).Encode(out)
}

// scimGetUser: GET /scim/v2/Users?filter=userName eq "x" — the
// IdP reconciliation query. Only eq on userName is supported v1.
func (s *Server) scimGetUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.scimBearer(ctx, r)
	if !ok {
		scimErr(w, http.StatusUnauthorized, "invalid SCIM bearer token")
		return
	}

	tx, txErr := s.scimTx(ctx, ws)
	if txErr != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f := r.URL.Query().Get("filter")
	const p = `userName eq "`
	i := strings.Index(f, p)
	if i < 0 || !strings.HasSuffix(f, `"`) {
		scimErr(w, http.StatusBadRequest, "only userName eq \"email\" filters are supported")
		return
	}
	email := strings.ToLower(f[i+len(p) : len(f)-1])

	var uid string
	err := tx.QueryRow(ctx, `SELECT u.id::text FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
		WHERE lower(u.email) = $2`, ws, email).Scan(&uid)
	if err != nil {
		scimErr(w, http.StatusNotFound, "user not found")
		return
	}
	writeScimUser(w, s, ctx, ws, uid, email)
}

// scimPatch: PATCH /scim/v2/Users/{id} — the lifecycle verb.
// active:false deactivates (deactivated_at stamp — role untouched);
// true restores. Other operations → 400 (honest unsupported).
func (s *Server) scimPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.scimBearer(ctx, r)
	if !ok {
		scimErr(w, http.StatusUnauthorized, "invalid SCIM bearer token")
		return
	}

	tx, txErr := s.scimTx(ctx, ws)
	if txErr != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	uid := r.PathValue("id")
	var patch struct {
		Operations []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"Operations"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&patch); err != nil {
		scimErr(w, http.StatusBadRequest, "invalid SCIM patch")
		return
	}
	activeSet := false
	activeVal := false
	for _, op := range patch.Operations {
		if !strings.EqualFold(op.Op, "replace") || !strings.EqualFold(strings.TrimSpace(op.Path), "active") {
			scimErr(w, http.StatusBadRequest, "only replace active is supported")
			return
		}
		// value may be bare bool or {"active": bool} per IdP quirks
		var b bool
		if err := json.Unmarshal(op.Value, &b); err == nil {
			activeVal, activeSet = b, true
			continue
		}
		var wrap struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(op.Value, &wrap); err == nil {
			activeVal, activeSet = wrap.Active, true
			continue
		}
		scimErr(w, http.StatusBadRequest, "active value must be boolean")
		return
	}
	if !activeSet {
		scimErr(w, http.StatusBadRequest, "no active operation")
		return
	}

	// membership must exist in this tenant
	var email string
	err := tx.QueryRow(ctx, `SELECT lower(u.email) FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = $1::uuid
		WHERE u.id = $2::uuid`, ws, uid).Scan(&email)
	if err != nil {
		scimErr(w, http.StatusNotFound, "user not found")
		return
	}

	if !activeVal {
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET deactivated_at = now()
			WHERE workspace_id = $1::uuid AND user_id = $2::uuid AND deactivated_at IS NULL`,
			ws, uid); err != nil {
			scimErr(w, http.StatusInternalServerError, "deactivate failed")
			return
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET deactivated_at = NULL
			WHERE workspace_id = $1::uuid AND user_id = $2::uuid AND deactivated_at IS NOT NULL`,
			ws, uid); err != nil {
			scimErr(w, http.StatusInternalServerError, "reactivate failed")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeScimUser(w, s, ctx, ws, uid, email)
}

// scimDelete: DELETE /scim/v2/Users/{id} — soft-delete semantics.
func (s *Server) scimDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.scimBearer(ctx, r)
	if !ok {
		scimErr(w, http.StatusUnauthorized, "invalid SCIM bearer token")
		return
	}

	tx, txErr := s.scimTx(ctx, ws)
	if txErr != nil {
		scimErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	uid := r.PathValue("id")
	res, err := tx.Exec(ctx, `
		UPDATE memberships SET deactivated_at = now()
		WHERE workspace_id = $1::uuid AND user_id = $2::uuid AND deactivated_at IS NULL`,
		ws, uid)
	if err != nil {
		scimErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if res.RowsAffected() == 0 {
		// already deactivated OR not a member — distinguish
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM memberships
			WHERE workspace_id = $1::uuid AND user_id = $2::uuid`, ws, uid).Scan(&n)
		if n == 0 {
			scimErr(w, http.StatusNotFound, "user not found")
			return
		}
	}
	_ = tx.Commit(ctx)
	w.WriteHeader(http.StatusNoContent)
}

// scimSPConfig: GET /scim/v2/ServiceProviderConfig — what we
// support, honestly declared.
func (s *Server) scimSPConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/scim+json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemas":        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":          map[string]any{"supported": true},
		"bulk":           map[string]any{"supported": false},
		"filter":         map[string]any{"supported": true, "maxResults": 1},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "SCIM Bearer Token",
			"description": "Per-workspace token set by the workspace admin",
		}},
	})
}
