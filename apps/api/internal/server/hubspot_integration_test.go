//go:build integration

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func hsSignedRequest(t *testing.T, h *httptestSrv, secret string, ev map[string]any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(ev)
	req, err := jsonBodyRequest(t, h, "POST", "/v1/integrations/hubspot/webhook?ws=acme", raw)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	req.Header.Set("X-OpenLane-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return doRequest(t, req)
}

func TestHubspotSettingsRoundTrip(t *testing.T) {
	h, _ := riverTestStack(t)

	code, body := h.do("GET", "/v1/integrations/hubspot", nil)
	if code != 200 || !strings.Contains(string(body), `"configured":false`) {
		t.Fatalf("unconfigured = %d %s", code, body)
	}
	if code, _ := h.do("PUT", "/v1/integrations/hubspot", map[string]any{
		"instance_url": "https://app.hubspot.com", "webhook_secret": "hs-test-secret-123456",
	}); code != 204 {
		t.Fatal("settings PUT failed")
	}
	code, body = h.do("GET", "/v1/integrations/hubspot", nil)
	if code != 200 || !strings.Contains(string(body), `"configured":true`) ||
		strings.Contains(string(body), "hs-test-secret-123456") {
		t.Fatalf("settings echo must never include the secret: %d %s", code, body)
	}
	// SF + HS coexist (PK change): configure SF too, both readable
	h.do("PUT", "/v1/integrations/salesforce", map[string]any{
		"webhook_secret": "sf-coexist-secret-1234",
	})
	if code, body = h.do("GET", "/v1/integrations/salesforce", nil); code != 200 || !strings.Contains(string(body), `"configured":true`) {
		t.Fatalf("sf after hs = %d %s", code, body)
	}
	if code, body = h.do("GET", "/v1/integrations/hubspot", nil); code != 200 || !strings.Contains(string(body), `"configured":true`) {
		t.Fatalf("hs after sf = %d %s", code, body)
	}
	// short secret rejected
	if code, _ := h.do("PUT", "/v1/integrations/hubspot", map[string]any{
		"webhook_secret": "short",
	}); code != 400 {
		t.Fatal("short secret should 400")
	}
}

func TestHubspotWebhook(t *testing.T) {
	h, admin := riverTestStack(t)
	const secret = "hs-webhook-secret-9999"

	// template for the created project (exercises the template branch —
	// the latent #55 bug: projects.description/templates.description)
	code, body := h.do("POST", "/v1/templates", map[string]any{
		"name": "Onboarding std", "category": "onboarding", "description": "std flow",
		"phases": []map[string]any{{
			"name":  "Kickoff",
			"tasks": []map[string]any{{"title": "Welcome call", "due_offset_days": 1, "required": true, "customer_visible": true}},
		}},
	})
	if code != 201 {
		t.Fatalf("template = %d %s", code, body)
	}
	var tpl struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &tpl)
	if code, _ := h.do("PUT", "/v1/integrations/hubspot", map[string]any{
		"webhook_secret": secret, "default_template_id": tpl.ID,
	}); code != 204 {
		t.Fatal("settings with template failed")
	}

	deal := func(dealID, stage string) map[string]any {
		return map[string]any{
			"portalId": "25123456", "dealId": dealID, "dealName": "Glean expansion",
			"properties": map[string]any{
				"dealstage": stage, "amount": "50000", "closedate": "2026-10-01",
			},
		}
	}

	// bad signature → 401
	ev := deal("912345", "closedwon")
	raw, _ := json.Marshal(ev)
	req, _ := jsonBodyRequest(t, h, "POST", "/v1/integrations/hubspot/webhook?ws=acme", raw)
	req.Header.Set("X-OpenLane-Signature", "AAAAaGVsbG8=")
	if code, _ := doRequest(t, req); code != 401 {
		t.Fatal("bad signature should 401")
	}
	// unknown slug → 404
	req2, _ := jsonBodyRequest(t, h, "POST", "/v1/integrations/hubspot/webhook?ws=ghost", raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	req2.Header.Set("X-OpenLane-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	if code, _ := doRequest(t, req2); code != 404 {
		t.Fatal("unknown slug should 404")
	}
	// not closed-won → ignored
	if code, body := hsSignedRequest(t, h, secret, deal("912345", "appointmentscheduled")); code != 200 ||
		!strings.Contains(string(body), "ignored") {
		t.Fatalf("non-closed-won = %d %s", code, body)
	}
	// closed-won → queued
	if code, body := hsSignedRequest(t, h, secret, deal("912345", "closedwon")); code != 200 ||
		!strings.Contains(string(body), "queued") {
		t.Fatalf("closed-won = %d %s", code, body)
	}
	// duplicate delivery → duplicate
	if code, body := hsSignedRequest(t, h, secret, deal("912345", "closedwon")); code != 200 ||
		!strings.Contains(string(body), "duplicate") {
		t.Fatalf("dup = %d %s", code, body)
	}

	// run the worker, then assert the created project
	runWorkerOnce(t)
	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()
	var id, refs string
	var desc *string
	if err := ap.QueryRow(context.Background(),
		`SELECT id, external_refs::text, description FROM projects WHERE name LIKE '%Glean expansion%'`).
		Scan(&id, &refs, &desc); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if !strings.Contains(refs, "912345") {
		t.Fatalf("external_refs missing deal id: %s", refs)
	}
	// template branch exercised: description copied + name includes template name
	if desc == nil || *desc != "std flow" {
		t.Fatalf("description not copied from template: %v", desc)
	}
	var audit int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='project.created_from_hubspot'`).Scan(&audit)
	if audit != 1 {
		t.Fatalf("audit = %d", audit)
	}
}
