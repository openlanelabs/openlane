package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Jira two-way task sync v1 (P1 §336). Status map is the spine:
// Jira "category" style names → OpenLane todo/in_progress/done.
// Conflicts: Jira wins for status (spec's hard call, v1 not configurable).
// Outbound: task PATCH status → Jira transition by name.

var jiraStatusToOpenLane = map[string]string{
	"to do":       "todo",
	"open":        "todo",
	"backlog":     "todo",
	"in progress": "in_progress",
	"in review":   "in_progress",
	"review":      "in_progress",
	"done":        "done",
	"closed":      "done",
	"complete":    "done",
	"completed":   "done",
	"resolved":    "done",
}

// openLaneToJiraNames: transition names to look for when pushing out.
var openLaneToJiraNames = map[string][]string{
	"in_progress": {"In Progress", "Start Progress"},
	"done":        {"Done", "Close Issue"},
	"todo":        {"To Do", "Reopen"},
}

// putJiraSettings: PUT /v1/jira/settings
func (s *Server) putJiraSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceURL   string `json:"instance_url"`
		WebhookSecret string `json:"webhook_secret"`
		PAT           string `json:"pat"` // Jira personal access token — sealed
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if len(req.WebhookSecret) < 16 {
		problem(w, http.StatusBadRequest, "webhook_secret required (min 16 chars)")
		return
	}
	if !validJiraInstanceURL(req.InstanceURL) {
		problem(w, http.StatusBadRequest, "instance_url must be an https URL")
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	secretSealed, err := sealSecret(aead, req.WebhookSecret)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	patSealed := []byte(nil)
	if req.PAT != "" {
		if patSealed, err = sealSecret(aead, req.PAT); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_integrations (workspace_id, provider, instance_url, webhook_secret_enc)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'jira', $1, $2)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET
		  instance_url = $1, webhook_secret_enc = $2, updated_at = now()`,
		req.InstanceURL, secretSealed); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// PAT lives in external_refs — the extensible slot. Sealed bytes are
	// base64 so jsonb stays text-only.
	if patSealed != nil {
		patB64 := base64.StdEncoding.EncodeToString(patSealed)
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_integrations (workspace_id, provider, instance_url, webhook_secret_enc, external_refs)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'jira', $1, $2, jsonb_build_object('pat_enc', $3::text))
			ON CONFLICT (workspace_id, provider) DO UPDATE SET external_refs = jsonb_build_object('pat_enc', $3::text), updated_at = now()`,
			req.InstanceURL, secretSealed, patB64); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'integration', NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'jira.settings_updated', 'api', '{}')`,
	); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "instance_url": req.InstanceURL})
}

// validJiraInstanceURL: https in prod; plain loopback http allowed so
// integration tests and local dev can run a stub.
func validJiraInstanceURL(u string) bool {
	if strings.HasPrefix(u, "https://") {
		return true
	}
	return strings.HasPrefix(u, "http://127.0.0.1:") || strings.HasPrefix(u, "http://localhost:")
}

// getJiraSettings: GET /v1/jira/settings — never echoes secrets.
func (s *Server) getJiraSettings(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var instanceURL *string
	err := tx.QueryRow(r.Context(), `
		SELECT instance_url
		FROM workspace_integrations
		WHERE provider = 'jira'
		  AND workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&instanceURL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if instanceURL == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "instance_url": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "instance_url": *instanceURL})
}

// linkTask: POST /v1/tasks/{id}/link {issue_key}
// Validates the issue exists in the workspace's Jira before linking.
func (s *Server) linkTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IssueKey string `json:"issue_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.IssueKey == "" || len(req.IssueKey) > 60 {
		problem(w, http.StatusBadRequest, "issue_key required (1-60 chars)")
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

	// resolve integration + PAT for validation
	var instanceURL, patB64 sql.NullString
	if err := tx.QueryRow(ctx, `
		SELECT instance_url::text, COALESCE(external_refs->>'pat_enc','') FROM workspace_integrations
		WHERE provider = 'jira' AND workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&instanceURL, &patB64); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusBadRequest, "jira not configured for this workspace")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	var issueID, url string
	if instanceURL.Valid && instanceURL.String != "" {
		var jiraErr error
		issueID, url, jiraErr = s.jiraValidateIssue(ctx, instanceURL.String, patB64.String, req.IssueKey)
		if jiraErr != nil {
			problem(w, http.StatusBadRequest, "issue not found in jira: "+req.IssueKey)
			return
		}
	}

	var linkID string
	err = tx.QueryRow(ctx, `
		INSERT INTO task_links (workspace_id, task_id, provider, issue_key, issue_id, url)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, t.id, 'jira', $2, $3, $4
		FROM tasks t
		WHERE t.id = $1::uuid AND t.deleted_at IS NULL
		ON CONFLICT (task_id, provider) DO UPDATE SET
		  issue_key = $2, issue_id = $3, url = $4, updated_at = now()
		RETURNING id`,
		r.PathValue("id"), req.IssueKey, issueID, url).Scan(&linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'task.jira_linked', 'api', $2)`,
		r.PathValue("id"), mustJSON(map[string]string{"issue_key": req.IssueKey})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"issue_key": req.IssueKey, "url": url})
}

// unlinkTask: DELETE /v1/tasks/{id}/link
func (s *Server) unlinkTask(w http.ResponseWriter, r *http.Request) {
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
	ct, err := tx.Exec(ctx, `DELETE FROM task_links WHERE task_id = $1::uuid AND provider = 'jira'`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ct.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "no jira link on this task")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'task.jira_unlinked', 'api', '{}')`,
		r.PathValue("id")); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jiraValidateIssue: GET /rest/api/3/issue/{key} with PAT bearer.
// Returns issue id + browse URL.
func (s *Server) jiraValidateIssue(ctx context.Context, instanceURL, patEnc, issueKey string) (string, string, error) {
	if patEnc == "" {
		// configured without PAT: accept the link without remote validation
		return "", instanceURL + "/browse/" + issueKey, nil
	}
	patSealed, err := base64.StdEncoding.DecodeString(patEnc)
	if err != nil {
		return "", "", err
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		return "", "", err
	}
	pat, err := openSecret(aead, patSealed)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		strings.TrimSuffix(instanceURL, "/")+"/rest/api/3/issue/"+issueKey+"?fields=key", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+string(pat))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("jira api status " + resp.Status)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", err
	}
	return out.ID, instanceURL + "/browse/" + issueKey, nil
}

// jiraWebhook: POST /v1/jira/webhook?ws=slug — HMAC-verified like SF/HS.
// Events: issue updated (status/assignee) + comment created.
type jiraWebhookEvent struct {
	WebhookEvent string `json:"webhookEvent"`
	Issue        struct {
		Key    string `json:"key"`
		Fields struct {
			Status struct {
				Name string `json:"name"`
			} `json:"status"`
			Assignee *struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"assignee"`
		} `json:"fields"`
	} `json:"issue"`
	Comment *struct {
		ID     string `json:"id"`
		Author struct {
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
		} `json:"author"`
		Body string `json:"body"`
	} `json:"comment"`
}

func (s *Server) jiraWebhook(w http.ResponseWriter, r *http.Request) {
	raw := readBody1MB(w, r)
	if raw == nil {
		return
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-OpenLane-Signature"))
	if err != nil || len(sig) == 0 {
		problem(w, http.StatusUnauthorized, "missing or malformed X-OpenLane-Signature")
		return
	}
	var ev jiraWebhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		problem(w, http.StatusBadRequest, "malformed event")
		return
	}
	slug := r.URL.Query().Get("ws")
	if slug == "" {
		problem(w, http.StatusBadRequest, "ws query parameter required")
		return
	}

	ctx := r.Context()
	var wsID string
	var enc []byte
	if err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, webhook_secret_enc FROM jira_webhook_secret($1)`, slug).Scan(&wsID, &enc); err != nil {
		problem(w, http.StatusNotFound, "not found")
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	secret, err := openSecret(aead, enc)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), sig) {
		problem(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// unknown events: acknowledge + ignore
	kind := ev.WebhookEvent
	if kind != "jira:issue_updated" && kind != "comment_created" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', '', true)", wsID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// resolve the linked task (no oracle: unknown key = ignored)
	var taskID, curStatus string
	err = tx.QueryRow(ctx, `
		SELECT t.id::text, t.status FROM task_links tl
		JOIN tasks t ON t.id = tl.task_id
		WHERE tl.issue_key = $1 AND tl.provider = 'jira' AND tl.workspace_id = $2::uuid`,
		ev.Issue.Key, wsID).Scan(&taskID, &curStatus)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no link"})
		return
	}

	switch kind {
	case "jira:issue_updated":
		newStatus := jiraStatusToOpenLane[strings.ToLower(strings.TrimSpace(ev.Issue.Fields.Status.Name))]
		if newStatus != "" && newStatus != curStatus {
			// valid OpenLane transition or ignore (todo→done blocked in UI too)
			if transitionOK(curStatus, newStatus) {
				if _, err := tx.Exec(ctx, `
					UPDATE tasks SET
					  status = $2,
					  completed_at = CASE WHEN $2 = 'done' AND completed_at IS NULL THEN now()
					                      WHEN $2 = 'todo' THEN NULL
					                      ELSE completed_at END,
					  updated_at = now()
					WHERE id = $1::uuid`, taskID, newStatus); err != nil {
					problem(w, http.StatusInternalServerError, "internal error")
					return
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
					VALUES ($1, 'task', $2, 'system', NULL, 'task.jira_sync_in', 'api', $3)`,
					wsID, taskID, mustJSON(map[string]string{"from": curStatus, "to": newStatus})); err != nil {
					problem(w, http.StatusInternalServerError, "internal error")
					return
				}
			}
		}
	case "comment_created":
		if ev.Comment != nil && strings.TrimSpace(ev.Comment.Body) != "" {
			// dedupe: jira comment id in task_messages refs — v1 checks existence
			var exists bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM task_messages WHERE task_id = $1::uuid
					AND refs->>'jira_comment_id' = $2)`, taskID, ev.Comment.ID).Scan(&exists); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if !exists {
				body := strings.TrimSpace(ev.Comment.Body)
				if len(body) > 2000 {
					body = body[:2000]
				}
				var authorID *string
				if ev.Comment.Author.EmailAddress != "" {
					var uid string
					if err := tx.QueryRow(ctx, `
						SELECT m.user_id::text FROM memberships m JOIN users u ON u.id = m.user_id
						WHERE lower(u.email) = lower($1) AND m.workspace_id = $2::uuid LIMIT 1`,
						ev.Comment.Author.EmailAddress, wsID).Scan(&uid); err == nil {
						authorID = &uid
					}
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO task_messages (workspace_id, task_id, author_type, author_id, body, refs)
					VALUES ($1, $2::uuid, 'user', $3, $4, $5)`,
					wsID, taskID, authorID, body, mustJSON(map[string]string{"jira_comment_id": ev.Comment.ID})); err != nil {
					problem(w, http.StatusInternalServerError, "internal error")
					return
				}
			}
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE task_links SET last_synced_at = now() WHERE task_id = $1::uuid AND provider = 'jira'`, taskID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "synced"})
}

// pushJiraStatus: outbound transition (called by the River worker).
// Guard: skip when last_synced_at is newer than the task's updated_at
// (that update CAME from jira — pushing it back would loop).
func (s *Server) pushJiraStatus(ctx context.Context, wsID, taskID, issueKey, newStatus string) error {
	var instanceURL, patB64 string
	if err := s.pool.QueryRow(ctx, `
		SELECT instance_url, COALESCE(external_refs->>'pat_enc','') FROM workspace_integrations
		WHERE provider = 'jira' AND workspace_id = $1::uuid`, wsID).Scan(&instanceURL, &patB64); err != nil {
		return err // not configured: silently skip (link stays, sync pauses)
	}
	if instanceURL == "" || patB64 == "" {
		return nil
	}
	patSealed, err := base64.StdEncoding.DecodeString(patB64)
	if err != nil {
		return err
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		return err
	}
	pat, err := openSecret(aead, patSealed)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(instanceURL, "/")

	// loop guard: if jira told us about this task very recently, skip
	var lastSync, taskUpdated time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(tl.last_synced_at, to_timestamp(0)), t.updated_at
		FROM tasks t LEFT JOIN task_links tl ON tl.task_id = t.id AND tl.provider = 'jira'
		WHERE t.id = $1::uuid`, taskID).Scan(&lastSync, &taskUpdated); err != nil {
		return err
	}
	if lastSync.After(taskUpdated) || lastSync.Equal(taskUpdated) {
		return nil // came from jira
	}

	// find the matching transition id
	names := openLaneToJiraNames[newStatus]
	if len(names) == 0 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		base+"/rest/api/3/issue/"+issueKey+"/transitions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(pat))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("jira transitions status " + resp.Status)
	}
	var tr struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return err
	}
	transitionID := ""
	for _, t := range tr.Transitions {
		for _, want := range names {
			if strings.EqualFold(t.Name, want) {
				transitionID = t.ID
			}
		}
	}
	if transitionID == "" {
		return nil // no matching transition (e.g. already done)
	}

	post, err := http.NewRequestWithContext(ctx, "POST",
		base+"/rest/api/3/issue/"+issueKey+"/transitions",
		bytes.NewReader(mustJSON(map[string]any{"transition": map[string]string{"id": transitionID}})))
	if err != nil {
		return err
	}
	post.Header.Set("Authorization", "Bearer "+string(pat))
	post.Header.Set("Content-Type", "application/json")
	presp, err := http.DefaultClient.Do(post)
	if err != nil {
		return err
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent && presp.StatusCode != http.StatusOK {
		return errors.New("jira transition status " + presp.Status)
	}

	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES ($1, 'task', $2, 'system', NULL, 'task.jira_sync_out', 'api', $3)`,
		wsID, taskID, mustJSON(map[string]string{"status": newStatus}))
	_, _ = s.pool.Exec(ctx, `UPDATE task_links SET last_synced_at = now() WHERE task_id = $1::uuid AND provider = 'jira'`, taskID)
	return nil
}
