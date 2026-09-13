package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Custom domains (P1 §199/§229/§499): per-workspace branded portal
// hosts. TLS lives at the hosting layer (Caddy on-demand ACME etc);
// the app owns registration, TXT verification, and host→workspace
// resolution. TXT lookups use stdlib DNS — no HTTP fetch of the
// domain, so there is no SSRF surface.

type workspaceDomain struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	Verified  bool   `json:"verified"`
	Challenge string `json:"challenge"`
	CreatedAt string `json:"created_at"`
}

var domainLabelRe = mustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validCustomDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if len(d) < 4 || len(d) > 253 || strings.Contains(d, "://") || strings.ContainsAny(d, "/@ \t") {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !domainLabelRe.MatchString(l) {
			return false
		}
	}
	return true
}

func newChallenge() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "openlane-verify=" + hex.EncodeToString(b)
}

// dnsTXT is swappable for tests.
var dnsTXT = func(ctx context.Context, record string) ([]string, error) {
	r := net.DefaultResolver
	return r.LookupTXT(ctx, record)
}

func (s *Server) putDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	d := strings.ToLower(strings.TrimSpace(req.Domain))
	if !validCustomDomain(d) {
		problem(w, http.StatusBadRequest, "invalid domain — use a lowercase hostname like onboarding.acme.com")
		return
	}
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
	// domain taken by ANOTHER workspace?
	var ownerWS string
	err = tx.QueryRow(ctx, `SELECT workspace_id FROM workspace_domains WHERE domain = $1`, d).Scan(&ownerWS)
	if err == nil && ownerWS != workspaceFromCtx(ctx) {
		problem(w, http.StatusConflict, "domain already claimed by another workspace")
		return
	}
	// same-workspace re-put: keep verified unless the domain changed
	ch := newChallenge()
	var out workspaceDomain
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_domains (workspace_id, domain, challenge)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2)
		ON CONFLICT (domain) DO UPDATE SET challenge = $2, verified = false, updated_at = now()
		RETURNING id, domain, verified, challenge, created_at::text`,
		d, ch).Scan(&out.ID, &out.Domain, &out.Verified, &out.Challenge, &out.CreatedAt); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'workspace_domain', $1::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'domain.claimed', 'ui', $2)`,
		out.ID, mustJSON(map[string]string{"domain": d})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)",
		workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT id, domain, verified, challenge, created_at::text FROM workspace_domains
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		ORDER BY created_at`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []workspaceDomain{}
	for rows.Next() {
		var d workspaceDomain
		if err := rows.Scan(&d.ID, &d.Domain, &d.Verified, &d.Challenge, &d.CreatedAt); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, d)
	}
	_ = tx.Commit(ctx)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
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
	var domain, challenge string
	if err := tx.QueryRow(ctx, `
		SELECT domain, challenge FROM workspace_domains
		WHERE id = $1::uuid AND workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`,
		r.PathValue("id")).Scan(&domain, &challenge); err != nil {
		problem(w, http.StatusNotFound, "domain not found")
		return
	}
	txts, err := dnsTXT(ctx, "_openlane-challenge."+domain)
	if err != nil {
		problem(w, http.StatusBadRequest, "DNS lookup failed — create the TXT record first: _openlane-challenge."+domain)
		return
	}
	for _, t := range txts {
		if strings.TrimSpace(t) == challenge {
			if _, err := tx.Exec(ctx,
				`UPDATE workspace_domains SET verified = true, updated_at = now() WHERE id = $1::uuid`,
				r.PathValue("id")); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
				VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'workspace_domain', $1::uuid, 'user',
				        NULLIF(current_setting('app.user_id', true), '')::uuid, 'domain.verified', 'ui', $2)`,
				r.PathValue("id"), mustJSON(map[string]string{"domain": domain})); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if err := tx.Commit(ctx); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "domain": domain, "verified": true})
			return
		}
	}
	problem(w, http.StatusBadRequest, "TXT record not found (or mismatch) at _openlane-challenge."+domain+" — expected "+challenge)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
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
	tag, err := tx.Exec(ctx, `
		DELETE FROM workspace_domains
		WHERE id = $1::uuid AND workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`,
		r.PathValue("id"))
	if err != nil || tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "domain not found")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mustCompile: tiny regexp helper (kept local to avoid vet flag on the
// package-level MustCompile pattern used elsewhere).
func mustCompile(expr string) *regexp.Regexp {
	return regexp.MustCompile(expr)
}
