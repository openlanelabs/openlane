// Package server wires the HTTP mux. Routes mirror packages/contracts/openapi.yaml.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// Server holds the pool + staff token; handlers live in portal.go.
type Server struct {
	pool       *pgxpool.Pool
	staffToken string // empty = staff endpoints return 503
	s3         s3Config
	http       *http.Client          // for S3 HEAD (uploaded-verify); short timeout
	river      *river.Client[pgx.Tx] // enqueue-only job producer (ADR-0003)
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
func New(ctx context.Context, dsn, staffToken string) (http.Handler, *pgxpool.Pool, error) {
	// fail closed: prod profile refuses to boot with dev escape hatches
	if err := validateProdConfig(); err != nil {
		return nil, nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	rc, err := newRiverClient(pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	s := &Server{
		pool:       pool,
		staffToken: staffToken,
		s3:         s3ConfigFromEnv(os.Getenv),
		http:       &http.Client{Timeout: 5 * time.Second},
		river:      rc,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			problem(w, http.StatusServiceUnavailable, "db unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// public auth endpoints
	mux.HandleFunc("POST /v1/auth/magic-link", s.requestMagicLink)
	mux.HandleFunc("GET /v1/auth/magic-link/consume", s.consumeMagicLink)
	mux.HandleFunc("POST /v1/auth/refresh", s.refreshSession)
	authed := s.auth
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
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
	mux.Handle("POST /v1/tasks/{id}/messages", authed(s.createTaskMessage))
	mux.Handle("GET /v1/tasks/{id}/messages", authed(s.listTaskMessages))
	mux.Handle("DELETE /v1/tasks/{id}", authed(s.deleteTask))
	mux.Handle("PUT /v1/settings/slack", authed(s.putSlackSettings))
	mux.Handle("GET /v1/settings/slack", authed(s.getSlackSettings))
	mux.Handle("DELETE /v1/settings/slack", authed(s.deleteSlackSettings))

	mux.Handle("POST /v1/projects/{id}/files", authed(s.createProjectFile))
	mux.Handle("POST /v1/files/{id}/uploaded", authed(s.confirmFileUploaded))
	mux.Handle("GET /v1/projects/{id}/files", authed(s.listProjectFiles))
	mux.Handle("DELETE /v1/files/{id}", authed(s.deleteFile))
	mux.Handle("GET /v1/files/{id}/url", authed(s.fileDownloadURL))
	mux.Handle("POST /v1/projects/{id}/approvals", authed(s.createApproval))
	mux.Handle("GET /v1/projects/{id}/approvals", authed(s.listProjectApprovals))
	mux.Handle("POST /v1/approvals/{id}/reopen", authed(s.reopenApproval))
	mux.Handle("POST /v1/projects/{id}/forms", authed(s.createForm))
	mux.Handle("GET /v1/projects/{id}/forms", authed(s.listProjectForms))
	mux.Handle("GET /v1/forms/{id}/responses.csv", authed(s.exportFormResponsesCSV))
	mux.Handle("GET /v1/search", authed(s.staffSearch))
	mux.Handle("POST /v1/automations", authed(s.createAutomation))
	mux.Handle("GET /v1/automations", authed(s.listAutomations))
	mux.Handle("PATCH /v1/automations/{id}", authed(s.patchAutomation))
	mux.Handle("DELETE /v1/automations/{id}", authed(s.deleteAutomation))
	mux.Handle("GET /v1/automations/{id}/runs", authed(s.listAutomationRuns))
	mux.Handle("POST /v1/projects/{id}/docs", authed(s.createDoc))
	mux.Handle("GET /v1/projects/{id}/docs", authed(s.listProjectDocs))
	mux.Handle("GET /v1/docs/{id}", authed(s.getDoc))
	mux.Handle("PUT /v1/docs/{id}", authed(s.updateDoc))
	mux.Handle("GET /v1/docs/{id}/versions", authed(s.listDocVersions))
	mux.Handle("GET /v1/projects/{id}/csat", authed(s.listProjectCSAT))
	mux.Handle("POST /v1/tasks/{id}/time", authed(s.logTaskTime))
	mux.Handle("POST /v1/projects/{id}/time", authed(s.logProjectTime))
	mux.Handle("GET /v1/projects/{id}/time", authed(s.listProjectTime))
	mux.Handle("GET /v1/me/time", authed(s.listMyTime))
	mux.Handle("PUT /v1/integrations/salesforce", authed(s.putSFSettings))
	mux.Handle("GET /v1/integrations/salesforce", authed(s.getSFSettings))
	mux.Handle("PUT /v1/integrations/hubspot", authed(s.putHSSettings))
	mux.Handle("GET /v1/integrations/hubspot", authed(s.getHSSettings))
	mux.HandleFunc("POST /v1/integrations/hubspot/webhook", s.hsWebhook)
	mux.HandleFunc("POST /v1/integrations/salesforce/webhook", s.sfWebhook)

	mux.HandleFunc("GET /v1/portal/{token}/session", s.portal(s.getPortalSession))
	mux.HandleFunc("GET /v1/portal/{token}/tasks", s.portal(s.listPortalTasks))
	mux.HandleFunc("POST /v1/portal/{token}/tasks/{task_id}/complete", s.portal(s.completePortalTask))
	mux.HandleFunc("GET /v1/portal/{token}/files", s.portal(s.listPortalFiles))
	mux.HandleFunc("GET /v1/portal/{token}/approvals", s.portal(s.listPortalApprovals))
	mux.HandleFunc("POST /v1/portal/{token}/approvals/{id}/decide", s.portal(s.decidePortalApproval))
	mux.HandleFunc("POST /v1/portal/{token}/csat", s.portal(s.submitPortalCSAT))
	mux.HandleFunc("GET /v1/portal/{token}/docs", s.portal(s.listPortalDocs))
	mux.HandleFunc("GET /v1/portal/{token}/search", s.portal(s.portalSearch))
	mux.HandleFunc("POST /v1/portal/{token}/tasks/{task_id}/messages", s.portal(s.portalPostMessage))
	mux.HandleFunc("GET /v1/portal/{token}/tasks/{task_id}/messages", s.portal(s.portalListMessages))
	mux.HandleFunc("GET /v1/portal/{token}/forms", s.portal(s.listPortalForms))
	mux.HandleFunc("POST /v1/portal/{token}/forms/{id}/submit", s.portal(s.submitPortalForm))
	mux.HandleFunc("GET /v1/portal/{token}/files/{file_id}/url", s.portal(s.portalFileURL))

	mux.Handle("GET /v1/metrics", authed(s.getMetrics))
	mux.Handle("POST /v1/rate-cards", authed(s.createRateCard))
	mux.Handle("GET /v1/rate-cards", authed(s.listRateCards))
	mux.Handle("GET /v1/rate-cards/{id}", authed(s.getRateCard))
	mux.Handle("PATCH /v1/rate-cards/{id}", authed(s.patchRateCard))
	mux.Handle("DELETE /v1/rate-cards/{id}", authed(s.deleteRateCard))
	// Prometheus scrape endpoint — same auth shape as staff (scrapers send
	// the bearer); no portal route exists (§207: portal never sees ops data).
	mux.Handle("GET /metrics", authed(s.prometheus))

	// metrics middleware wraps everything (§588 p95) — returns an http.Handler
	return recordMetrics(mux), pool, nil
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

func sessionWorkspaceFromCtx(ctx context.Context) string {
	return sessionFromCtx(ctx).WorkspaceID
}

// resolvePortalSession returns nil (not error) when the token has no live link.
func resolvePortalSession(ctx context.Context, tx pgx.Tx, tokenHash string) (*portalSessionOut, error) {
	var out portalSessionOut
	err := tx.QueryRow(ctx, `SELECT * FROM portal_session($1)`, tokenHash).
		Scan(&out.WorkspaceID, &out.ProjectID, &out.ProjectName, &out.CustomerName, &out.ContactID, &out.ContactName, &out.ExpiresAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
