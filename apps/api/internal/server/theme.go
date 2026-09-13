package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Portal white-label theming (§199/§499): per-workspace logo_url +
// brand_color + no_branding. Portal reads go through the SECURITY
// DEFINER helper (portal sessions carry no workspace ctx). Host
// resolution: verified workspace domains → workspace, else null —
// unresolved hosts simply serve the standard portal (§229 fallback).

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type portalTheme struct {
	LogoURL    *string `json:"logo_url"`
	BrandColor *string `json:"brand_color"`
	NoBranding bool    `json:"no_branding"`
}

func (s *Server) putTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req portalTheme
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.BrandColor != nil && !hexColorRe.MatchString(*req.BrandColor) {
		problem(w, http.StatusBadRequest, "brand_color must be a 6-digit hex like #2563eb")
		return
	}
	if req.LogoURL != nil && len(*req.LogoURL) > 2048 {
		problem(w, http.StatusBadRequest, "logo_url too long")
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_themes (workspace_id, logo_url, brand_color, no_branding, updated_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3, now())
		ON CONFLICT (workspace_id) DO UPDATE
		SET logo_url = $1, brand_color = $2, no_branding = $3, updated_at = now()`,
		req.LogoURL, req.BrandColor, req.NoBranding); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'workspace_theme',
		        NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'theme.saved', 'ui', $1)`,
		mustJSON(req)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) getTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	out := portalTheme{}
	var logo, color *string
	var noBrand bool
	err = tx.QueryRow(ctx, `SELECT logo_url, brand_color, no_branding FROM workspace_themes
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&logo, &color, &noBrand)
	if err == nil {
		out = portalTheme{LogoURL: logo, BrandColor: color, NoBranding: noBrand}
	}
	_ = tx.Commit(ctx)
	writeJSON(w, http.StatusOK, out)
}

// portalHostResolve: GET /v1/portal/host?host=<hostname> —
// unauthenticated, read-only. Verified workspace domains resolve;
// anything else → {workspace_id: null} and the caller serves the
// standard portal (§229: never 404).
func (s *Server) portalHostResolve(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("host")))
	host = regexp.MustCompile(`:[0-9]+$`).ReplaceAllString(host, "") // strip port
	if host == "" || len(host) > 253 {
		writeJSON(w, http.StatusOK, map[string]any{"workspace_id": nil, "theme": nil})
		return
	}
	ctx := r.Context()
	// SECURITY DEFINER routing table (§229): verified domains only;
	// cross-tenant read is the design — a host either routes or it doesn't.
	var resolved json.RawMessage
	_ = s.pool.QueryRow(ctx, `SELECT workspace_for_host($1)`, host).Scan(&resolved)
	var out struct {
		WorkspaceID *string         `json:"workspace_id"`
		Theme       json.RawMessage `json:"theme"`
	}
	if err := json.Unmarshal(resolved, &out); err != nil || out.WorkspaceID == nil {
		writeJSON(w, http.StatusOK, map[string]any{"workspace_id": nil, "theme": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace_id": *out.WorkspaceID, "theme": out.Theme})
}

// themeForSession: inside a portal tx, resolve the workspace's theme
// (SECURITY DEFINER helper — portal ctx has no workspace id).
func themeForSession(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, ws string) json.RawMessage {
	var theme json.RawMessage
	_ = q.QueryRow(ctx, `SELECT COALESCE(workspace_theme_for($1::uuid), 'null'::jsonb)`, ws).Scan(&theme)
	if string(theme) == "null" {
		return nil // omit when never themed (omitempty drops nil RawMessage)
	}
	return theme
}
