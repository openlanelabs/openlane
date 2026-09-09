// Package server wires the HTTP mux. Routes mirror packages/contracts/openapi.yaml.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server holds the pool + staff token; handlers live in portal.go.
type Server struct {
	pool       *pgxpool.Pool
	staffToken string // empty = staff endpoints return 503
}

// ConfigFromEnv reads operator config. DATABASE_URL must be the openlane_app
// role (non-owner) — RLS does not apply to table owners.
func ConfigFromEnv() (addr, dsn, staffToken string) {
	addr = os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return addr, os.Getenv("DATABASE_URL"), os.Getenv("OPENLANE_STAFF_TOKEN")
}

// New builds the mux; caller closes the pool.
func New(ctx context.Context, dsn, staffToken string) (*http.ServeMux, *pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	s := &Server{pool: pool, staffToken: staffToken}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// public auth endpoints
	mux.HandleFunc("POST /v1/auth/magic-link", s.requestMagicLink)
	mux.HandleFunc("GET /v1/auth/magic-link/consume", s.consumeMagicLink)
	mux.HandleFunc("POST /v1/auth/refresh", s.refreshSession)
	authed := s.auth
	mux.Handle("POST /v1/auth/logout", authed(s.logout))
	mux.Handle("GET /v1/auth/me", authed(s.me))
	mux.Handle("GET /v1/auth/workspaces", authed(s.myWorkspaces))

	// staff routes: JWT (static token only when OPENLANE_ALLOW_STATIC_TOKEN=1)
	mux.Handle("POST /v1/portal-links", authed(s.createPortalLink))
	mux.Handle("GET /v1/portal-links", authed(s.listPortalLinks))
	mux.Handle("POST /v1/portal-links/{id}/revoke", authed(s.revokePortalLink))
	mux.Handle("POST /v1/imports", authed(s.createImport))
	mux.Handle("POST /v1/projects", authed(s.createProject))
	mux.Handle("GET /v1/projects", authed(s.listProjects))
	mux.Handle("GET /v1/projects/{id}", authed(s.getProject))
	mux.Handle("PATCH /v1/projects/{id}", authed(s.patchProject))
	mux.Handle("DELETE /v1/projects/{id}", authed(s.deleteProject))
	mux.Handle("POST /v1/projects/from-template", authed(s.createProjectFromTemplate))
	mux.Handle("POST /v1/templates", authed(s.createTemplate))
	mux.Handle("GET /v1/templates", authed(s.listTemplates))
	mux.Handle("GET /v1/templates/{id}", authed(s.getTemplate))
	mux.Handle("POST /v1/templates/{id}/new-version", authed(s.newTemplateVersion))
	mux.Handle("GET /v1/projects/{id}/tasks", authed(s.listProjectTasks))
	mux.Handle("POST /v1/projects/{id}/tasks", authed(s.createTask))
	mux.Handle("PATCH /v1/tasks/{id}", authed(s.patchTask))
	mux.Handle("DELETE /v1/tasks/{id}", authed(s.deleteTask))

	mux.HandleFunc("GET /v1/portal/{token}/session", s.portal(s.getPortalSession))
	mux.HandleFunc("GET /v1/portal/{token}/tasks", s.portal(s.listPortalTasks))
	mux.HandleFunc("POST /v1/portal/{token}/tasks/{task_id}/complete", s.portal(s.completePortalTask))

	return mux, pool, nil
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   fmt.Sprintf("https://openlane.dev/errors/%d", status),
		"title":  title,
		"status": status,
	})
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

type wsKey struct{}

func workspaceFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(wsKey{}).(string); ok {
		return v
	}
	return ""
}

// staff: bearer + X-Workspace-Id. RLS enforces whatever the header claims.
// ponytail: static bearer = P0 staff auth; the auth slice swaps in real
// sessions later — handler SQL never changes, only this middleware does.
func (s *Server) staff() func(http.HandlerFunc) http.Handler {
	return func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.staffToken == "" {
				problem(w, http.StatusServiceUnavailable, "staff auth not configured")
				return
			}
			var got string
			if _, err := fmt.Sscanf(r.Header.Get("Authorization"), "Bearer %s", &got); err != nil ||
				subtle.ConstantTimeCompare([]byte(got), []byte(s.staffToken)) != 1 {
				problem(w, http.StatusUnauthorized, "invalid staff credentials")
				return
			}
			ws := r.Header.Get("X-Workspace-Id")
			if ws == "" {
				problem(w, http.StatusBadRequest, "X-Workspace-Id required")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), wsKey{}, ws)))
		})
	}
}

// portal: validate token shape, open tx, set RLS scope, run handler.
// Handler returns (status, err): 0 = response written; errQuiet = plain
// problem(status); anything else = 500 (logged).
func (s *Server) portal(next func(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if len(token) < 43 || len(token) > 128 {
			problem(w, http.StatusUnauthorized, "invalid portal token")
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
			"SELECT set_config('app.workspace_id', '', true), set_config('app.portal_token_hash', $1, true)",
			hashToken(token)); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		// Resolve the session ONCE here: every portal handler then runs with a
		// guaranteed-valid link. 410 (revoked/expired) vs 401 (never valid) per contract.
		sess, err := resolvePortalSession(ctx, tx, hashToken(token))
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if sess == nil {
			var st string
			if err2 := tx.QueryRow(ctx, `SELECT portal_link_status($1)`, hashToken(token)).Scan(&st); err2 == nil && (st == "revoked" || st == "expired") {
				problem(w, http.StatusGone, http.StatusText(http.StatusGone))
			} else {
				problem(w, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
			}
			return
		}
		ctx = context.WithValue(ctx, sessionKey{}, *sess)
		status, err := next(ctx, tx, w, r)
		switch {
		case status == 0 && err == nil:
			// handler already wrote + committed
		case err == errQuiet:
			problem(w, status, http.StatusText(status))
		default:
			problem(w, http.StatusInternalServerError, "internal error")
		}
	}
}

type sessionKey struct{}

func sessionFromCtx(ctx context.Context) portalSessionOut {
	if v, ok := ctx.Value(sessionKey{}).(portalSessionOut); ok {
		return v
	}
	return portalSessionOut{}
}

// resolvePortalSession returns nil (not error) when the token has no live link.
func resolvePortalSession(ctx context.Context, tx pgx.Tx, tokenHash string) (*portalSessionOut, error) {
	var out portalSessionOut
	var wsID string
	err := tx.QueryRow(ctx, `SELECT * FROM portal_session($1)`, tokenHash).
		Scan(&wsID, &out.ProjectID, &out.ProjectName, &out.CustomerName, &out.ContactID, &out.ContactName, &out.ExpiresAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
