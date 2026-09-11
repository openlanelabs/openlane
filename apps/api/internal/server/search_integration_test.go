//go:build integration

package server

import (
	"encoding/json"
	"strings"
	"testing"
)

const searchPortalToken = "search-portal-token-1234567890abcdefghij"

func TestSearch(t *testing.T) {
	_, _, h := filesTestStack(t)
	// portal link via staff API
	code, body := h.do("POST", "/v1/portal-links", map[string]any{
		"project_id": projA, "contact_id": contactID,
	})
	var link struct {
		Token string `json:"token"`
	}
	if code != 201 || json.Unmarshal(body, &link) != nil || link.Token == "" {
		t.Fatalf("link = %d %s", code, body)
	}

	// seed searchable content: docs + files (projA already named
	// "Files Project"); tasks seeded by filesTestStack? Create a couple:
	mkTask := func(title string, visible bool) {
		if code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
			"title": title, "owner_type": "customer", "customer_visible": visible,
		}); code != 201 {
			t.Fatalf("task %q = %d %s", title, code, body)
		}
	}
	mkTask("Provision SSO tenant", true)
	mkTask("Internal margin review", false)
	h.do("POST", "/v1/projects/"+projA+"/docs", map[string]any{
		"title": "SSO runbook", "content_md": "steps", "customer_visible": true,
	})
	h.do("POST", "/v1/projects/"+projA+"/docs", map[string]any{
		"title": "Internal pricing notes", "content_md": "secret", "customer_visible": false,
	})

	// staff search: typo-tolerant ("runbok"), hits doc
	code, body = h.do("GET", "/v1/search?q=runbok", nil)
	if code != 200 || !strings.Contains(string(body), "SSO runbook") {
		t.Fatalf("staff typo search = %d %s", code, body)
	}
	var hits []searchHit
	json.Unmarshal(body, &hits)
	if len(hits) < 1 || hits[0].Type != "doc" {
		t.Fatalf("hits: %s", body)
	}

	// staff sees internal rows too
	if code, body = h.do("GET", "/v1/search?q=Internal", nil); code != 200 ||
		!strings.Contains(string(body), "Internal pricing notes") {
		t.Fatalf("staff internal = %d %s", code, body)
	}

	// multi-type: "SSO" hits task + doc
	if code, body = h.do("GET", "/v1/search?q=SSO", nil); code != 200 ||
		!strings.Contains(string(body), `"task"`) || !strings.Contains(string(body), `"doc"`) {
		t.Fatalf("multi-type = %d %s", code, body)
	}

	// ranking: closer match first ("Provision SSO" over "SSO runbook" for q=provision)
	if code, body = h.do("GET", "/v1/search?q=provision", nil); code != 200 ||
		!strings.Contains(string(body), "Provision SSO tenant") {
		t.Fatalf("provision = %d %s", code, body)
	}

	// no results → []
	if code, body = h.do("GET", "/v1/search?q=zzzznothing", nil); code != 200 ||
		string(body) == "null" || strings.Contains(string(body), "title") {
		t.Fatalf("empty = %d %s", code, body)
	}
	// bad q → 400
	if code, _ = h.do("GET", "/v1/search?q=", nil); code != 400 {
		t.Fatal("empty q should 400")
	}

	// ---- portal search: scoped to shared rows only
	pURL := "/p/" + link.Token
	if code, body = h.doPortal("GET", pURL+"/search?q=SSO"); code != 200 ||
		!strings.Contains(string(body), "Provision SSO tenant") ||
		!strings.Contains(string(body), "SSO runbook") {
		t.Fatalf("portal search = %d %s", code, body)
	}
	// internal rows NEVER leak into portal results
	if code, body = h.doPortal("GET", pURL+"/search?q=Internal"); code != 200 ||
		strings.Contains(string(body), "Internal pricing notes") ||
		strings.Contains(string(body), "Internal margin review") {
		t.Fatalf("portal internal leak = %d %s", code, body)
	}
	// portal bad q → 400
	if code, _ := h.doPortal("GET", pURL+"/search?q="); code != 400 {
		t.Fatal("portal empty q should 400")
	}
}
