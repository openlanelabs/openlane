//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const docsPortalToken = "docs-portal-token-1234567890abcdefghijkQRSTU"

func docsTestStack(t *testing.T) *httptestSrv {
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
		wsA, projA, contactID, docsPortalToken); err != nil {
		t.Fatalf("link: %v", err)
	}
	return h
}

func TestDocsFlow(t *testing.T) {
	h := docsTestStack(t)

	// create two docs: visible + internal
	mk := func(title string, visible bool, content string) string {
		code, body := h.do("POST", "/v1/projects/"+projA+"/docs", map[string]any{
			"title": title, "content_md": content, "customer_visible": visible,
		})
		if code != http.StatusCreated {
			t.Fatalf("create %q = %d %s", title, code, body)
		}
		var d docOut
		json.Unmarshal(body, &d)
		if d.LatestVersion != 1 {
			t.Fatalf("v1 expected: %s", body)
		}
		return d.ID
	}
	visibleID := mk("SOW draft", true, "v1 scope text")
	internalID := mk("Internal notes", false, "secret v1")

	// ghost project → 404
	if code, _ := h.do("POST", "/v1/projects/99999999-9999-9999-9999-999999999999/docs",
		map[string]any{"title": "x"}); code != 404 {
		t.Fatal("ghost project should 404")
	}
	// bad title → 400
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/docs", map[string]any{"title": ""}); code != 400 {
		t.Fatal("empty title should 400")
	}

	// staff get current: content v1
	code, body := h.do("GET", "/v1/docs/"+visibleID, nil)
	if code != 200 || !strings.Contains(string(body), "v1 scope text") {
		t.Fatalf("get = %d %s", code, body)
	}

	// edit visible doc: new content → version 2
	code, body = h.do("PUT", "/v1/docs/"+visibleID, map[string]any{"content_md": "v2 scope text"})
	if code != 200 {
		t.Fatalf("edit = %d %s", code, body)
	}
	var d docOut
	json.Unmarshal(body, &d)
	if d.LatestVersion != 2 {
		t.Fatalf("edit should bump to v2: %s", body)
	}

	// title-only edit → v3 carries content forward
	code, body = h.do("PUT", "/v1/docs/"+visibleID, map[string]any{"title": "SOW final"})
	if code != 200 {
		t.Fatalf("title edit = %d %s", code, body)
	}
	json.Unmarshal(body, &d)
	if d.LatestVersion != 3 || d.Title != "SOW final" {
		t.Fatalf("title edit: %s", body)
	}

	// get current: title updated + content carried (v2 text)
	code, body = h.do("GET", "/v1/docs/"+visibleID, nil)
	if code != 200 || !strings.Contains(string(body), "v2 scope text") ||
		!strings.Contains(string(body), "SOW final") {
		t.Fatalf("current after edits: %d %s", code, body)
	}

	// version history: 3 versions descending, v1 has old content
	code, body = h.do("GET", "/v1/docs/"+visibleID+"/versions", nil)
	if code != 200 || strings.Count(string(body), `"version"`) != 3 {
		t.Fatalf("versions = %d %s", code, body)
	}
	if !strings.Contains(string(body), "v1 scope text") {
		t.Fatal("v1 content should be in history")
	}

	// staff list shows both docs
	code, body = h.do("GET", "/v1/projects/"+projA+"/docs", nil)
	if code != 200 || strings.Count(string(body), `"title"`) != 2 {
		t.Fatalf("list = %d %s", code, body)
	}

	// edit ghost doc → 404
	if code, _ := h.do("PUT", "/v1/docs/99999999-9999-9999-9999-999999999999",
		map[string]any{"content_md": "x"}); code != 404 {
		t.Fatal("ghost doc edit should 404")
	}

	// ---- portal: sees ONLY customer-visible docs with latest content
	code, body = h.doPortal("GET", "/p/"+docsPortalToken+"/docs")
	if code != 200 || strings.Contains(string(body), "Internal notes") {
		t.Fatalf("portal docs leak internal: %d %s", code, body)
	}
	if !strings.Contains(string(body), "SOW final") || !strings.Contains(string(body), "v2 scope text") {
		t.Fatalf("portal should show latest visible doc: %s", body)
	}

	// ---- audits
	admin := adminDSN(t)
	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()
	var created, updated int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='doc.created'`).Scan(&created)
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='doc.updated'`).Scan(&updated)
	if created != 2 || updated != 2 {
		t.Fatalf("audits created=%d updated=%d", created, updated)
	}

	_ = internalID
}
