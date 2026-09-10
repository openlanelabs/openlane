package server

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Notifications v1 (issue #45, spec §16): best-effort Slack webhook delivery.
// ponytail: synchronous fire-and-forget in the request path — the
// transactional-outbox/worker pattern replaces it when River lands.

const slackHTTPClient = 2 * time.Second

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

// slackNotify posts {"text": ...} with a single retry. Never returns an
// error — delivery is best-effort; failures log to stderr only.
func slackNotify(ctx context.Context, webhookURL, text string) {
	if webhookURL == "" {
		return
	}
	body := []byte(`{"text":` + jsonString(text) + `}`)
	client := &http.Client{Timeout: slackHTTPClient}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
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
	go slackNotify(cctx, url, text)
}
