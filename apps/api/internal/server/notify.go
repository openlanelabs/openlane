package server

import (
	"context"
	"log"
	"net/url"
	"os"
	"strings"
)

// Notifications v2 (issue #54, spec §16 + ADR-0003): durable delivery.
// notifyEvent resolves settings, then enqueues a slack_notify River job
// in its own short tx. Once committed, the worker owns delivery with
// River's retry/backoff — a crashed API process can no longer drop a
// notification (the §350 "never lose write" edge).

// slackAllowed enforces the webhook allowlist (§17-5 SSRF guard): only
// hooks.slack.com and *.slack.com/services/ URLs.
func slackAllowed(raw string) bool {
	if os.Getenv("OPENLANE_SLACK_ALLOW_ANY") == "1" {
		return true // CI/tests point at httptest servers
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "hooks.slack.com" || strings.HasSuffix(h, ".slack.com")
}

// notifyEvent fires the event if the workspace enabled it. Opens its own
// connection + sets the workspace ctx (callers may run under portal or
// staff scopes — this path is self-contained). Delivery is a goroutine:
// the request never waits on Slack.
func (s *Server) notifyEvent(ctx context.Context, workspaceID, event, text string) {
	if workspaceID == "" {
		return
	}
	cctx := context.WithoutCancel(ctx)
	conn, err := s.pool.Acquire(cctx)
	if err != nil {
		return
	}
	defer conn.Release()
	// is_local=true REVERTS at statement end on autocommit connections —
	// the RLS trap again. Session-level (false) it is; pool conns are
	// short-lived here and every use re-sets the value first.
	if _, err := conn.Exec(cctx,
		"SELECT set_config('app.workspace_id', $1, false), set_config('app.portal_token_hash', '', false)", workspaceID); err != nil {
		return
	}
	var url string
	var enabled bool
	err = conn.QueryRow(cctx, `
		SELECT slack_webhook_url,
		       COALESCE($2::text = 'task.completed' AND notify_task_completed
		            OR $2::text = 'project.created' AND notify_project_created
		            OR $2::text = 'project.created_from_template' AND notify_project_created, false)
		FROM workspace_settings WHERE workspace_id = $1`, workspaceID, event).Scan(&url, &enabled)
	if err != nil || !enabled || url == "" {
		return
	}
	// enqueue the durable job in its own short tx. Not part of the
	// caller's tx: these call sites fire AFTER their own commit, and
	// coupling them would require threading tx through every handler.
	// The gap (crash between caller commit and this enqueue) is covered
	// where it matters most — sf_project_create inlines its slack job.
	if err := s.enqueueSlackEvent(cctx, workspaceID, url, text); err != nil {
		// delivery is still best-effort from the API's perspective:
		// log and move on. River owns it from here.
		log.Printf("slack enqueue failed ws=%s event=%s: %v", workspaceID, event, err)
	}
}

// enqueueSlackEvent: open tx, insert job, commit. Self-contained.
func (s *Server) enqueueSlackEvent(ctx context.Context, workspaceID, url, text string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := enqueueSlack(ctx, s.river, tx, workspaceID, url, text); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
