//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const chatPortalToken = "chat-portal-token-1234567890abcdefghijkXYZ1"

func chatStack(t *testing.T) (*httptestSrv, string, string, *pgxpool.Pool) {
	t.Helper()
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, encode(sha256($4::bytea),'hex'), now() + interval '7 days')`,
		wsA, projA, contactID, chatPortalToken); err != nil {
		t.Fatalf("link: %v", err)
	}
	code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "SSO setup", "owner_type": "customer", "customer_visible": true,
	})
	if code != 201 {
		t.Fatalf("task = %d %s", code, body)
	}
	var vis struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &vis)
	code, body = h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Margin analysis", "owner_type": "internal", "customer_visible": false,
	})
	if code != 201 {
		t.Fatalf("task2 = %d %s", code, body)
	}
	var internal struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &internal)
	// real JWT identity for staff posts (static token = system, no
	// user id — chat needs a human author)
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO users (id, email, display_name) VALUES ($1, 'asha@acme.test', 'Asha') ON CONFLICT (id) DO NOTHING`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, 'admin') ON CONFLICT DO NOTHING`, wsA, userID); err != nil {
		t.Fatal(err)
	}
	return h, vis.ID, internal.ID, ap
}

func TestChatLite(t *testing.T) {
	h, visID, internalID, ap := chatStack(t)
	pURL := "/p/" + chatPortalToken

	// staff posts with a mention (JWT author)
	code, body, tok := devLogin(h)
	if code != 200 || tok == "" {
		t.Fatalf("devLogin = %d %s", code, body)
	}
	code, body = h.doJWT("POST", "/v1/tasks/"+visID+"/messages", map[string]any{
		"body": "ping @asha@acme.test — SSO tenant ready for review",
	}, tok)
	if code != http.StatusCreated {
		t.Fatalf("staff post = %d %s", code, body)
	}
	var m messageOut
	json.Unmarshal(body, &m)
	if m.AuthorType != "user" || !strings.Contains(string(m.Mentions), "asha@acme.test") {
		t.Fatalf("mention extraction: %s", body)
	}

	// portal (contact) replies on the customer-visible task
	if code, body = h.doPortalJSON("POST", pURL+"/tasks/"+visID+"/messages",
		map[string]any{"body": "looks good, proceeding"}); code != http.StatusCreated ||
		!strings.Contains(string(body), `"contact"`) {
		t.Fatalf("portal post = %d %s", code, body)
	}

	// thread: both messages, ordered
	code, body = h.doJWT("GET", "/v1/tasks/"+visID+"/messages", nil, tok)
	if code != 200 || strings.Count(string(body), `"body"`) != 2 {
		t.Fatalf("thread = %d %s", code, body)
	}
	var thread []messageOut
	json.Unmarshal(body, &thread)
	if thread[0].AuthorType != "user" || thread[1].AuthorType != "contact" {
		t.Fatalf("order: %s", body)
	}

	// ?after_id= cursor: only messages after the first (uuid cursor —
	// RFC3339 truncates microseconds and two posts share a second)
	if code, body = h.doJWT("GET", "/v1/tasks/"+visID+"/messages?after_id="+thread[0].ID, nil, tok); code != 200 ||
		strings.Count(string(body), `"body"`) != 1 {
		t.Fatalf("after cursor = %d %s", code, body)
	}

	// portal reads the same thread
	if code, body = h.doPortal("GET", pURL+"/tasks/"+visID+"/messages"); code != 200 ||
		strings.Count(string(body), `"body"`) != 2 {
		t.Fatalf("portal thread = %d %s", code, body)
	}

	// portal CANNOT post to the internal task (404, no oracle)
	if code, _ = h.doPortalJSON("POST", pURL+"/tasks/"+internalID+"/messages",
		map[string]any{"body": "sneak"}); code != 404 {
		t.Fatalf("internal post = %d, want 404", code)
	}
	// portal CANNOT read the internal task's thread
	if code, _ = h.doPortal("GET", pURL+"/tasks/"+internalID+"/messages"); code != 404 {
		t.Fatalf("internal read = %d, want 404", code)
	}

	// staff posts to the internal task fine (author user)
	if code, _ = h.doJWT("POST", "/v1/tasks/"+internalID+"/messages",
		map[string]any{"body": "internal note"}, tok); code != http.StatusCreated {
		t.Fatal("staff internal post failed")
	}

	// validation: empty + oversize bodies
	if code, _ = h.doJWT("POST", "/v1/tasks/"+visID+"/messages", map[string]any{"body": "  "}, tok); code != 400 {
		t.Fatal("empty body should 400")
	}
	if code, _ = h.doJWT("POST", "/v1/tasks/"+visID+"/messages",
		map[string]any{"body": strings.Repeat("x", 2001)}, tok); code != 400 {
		t.Fatal("oversize should 400")
	}
	// ghost task 404
	if code, _ = h.doJWT("POST", "/v1/tasks/99999999-9999-9999-9999-999999999999/messages",
		map[string]any{"body": "hi"}, tok); code != 404 {
		t.Fatal("ghost task should 404")
	}

	// audits: message.created user + contact actors
	var au, co int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='message.created' AND actor_type='user'`).Scan(&au)
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='message.created' AND actor_type='contact'`).Scan(&co)
	if au < 2 || co != 1 {
		t.Fatalf("audits user=%d contact=%d", au, co)
	}
}

func TestChatThrottle(t *testing.T) {
	h, visID, _, _ := chatStack(t)
	pURL := "/p/" + chatPortalToken

	limited := false
	for i := 0; i < 32; i++ {
		code, _ := h.doPortalJSON("POST", pURL+"/tasks/"+visID+"/messages",
			map[string]any{"body": "spam " + strconv.Itoa(i)})
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if code != http.StatusCreated {
			t.Fatalf("post %d = %d", i, code)
		}
	}
	if !limited {
		t.Fatal("30/min limit never hit")
	}
	// staff unaffected (different author bucket)
	code, body, tok := devLogin(h)
	if code != 200 || tok == "" {
		t.Fatalf("devLogin = %d %s", code, body)
	}
	if code, _ := h.doJWT("POST", "/v1/tasks/"+visID+"/messages",
		map[string]any{"body": "staff still fine"}, tok); code != http.StatusCreated {
		t.Fatal("staff throttled by contact spam")
	}
}
