//go:build integration

package server

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const formsPortalToken = "forms-portal-token-1234567890abcdefghijkLM"

func TestFormsFlow(t *testing.T) {
	_, _, h := filesTestStack(t)
	// seed portal link (filesTestStack does not create one)
	// via staff API for authenticity:
	code, body := h.do("POST", "/v1/portal-links", map[string]any{
		"project_id": projA, "contact_id": contactID,
	})
	_ = code
	_ = body
	// staffPost path needs token header — h.do already staffs. Parse token:
	var link struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &link); err != nil || link.Token == "" {
		t.Fatalf("link create failed: %d %s", code, body)
	}
	pURL := "/p/" + link.Token

	// create form: required text + optional date
	code, body = h.do("POST", "/v1/projects/"+projA+"/forms", map[string]any{
		"title": "Onboarding intake",
		"fields": []map[string]any{
			{"key": "company_size", "label": "Company size", "type": "text", "required": true},
			{"key": "go_live_date", "label": "Preferred go-live", "type": "date", "required": false},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create form = %d %s", code, body)
	}
	var f formOut
	json.Unmarshal(body, &f)
	if f.ID == "" || len(f.Fields) != 2 {
		t.Fatalf("form shape: %s", body)
	}

	// ghost project 404, bad field type 400, dup key 400
	if code, _ = h.do("POST", "/v1/projects/99999999-9999-9999-9999-999999999999/forms",
		map[string]any{"title": "x", "fields": []map[string]any{{"key": "a", "type": "text"}}}); code != 404 {
		t.Fatal("ghost project should 404")
	}
	if code, _ = h.do("POST", "/v1/projects/"+projA+"/forms",
		map[string]any{"title": "x", "fields": []map[string]any{{"key": "a", "type": "checkbox"}}}); code != 400 {
		t.Fatal("bad type should 400")
	}
	if code, _ = h.do("POST", "/v1/projects/"+projA+"/forms",
		map[string]any{"title": "x", "fields": []map[string]any{
			{"key": "a", "type": "text"}, {"key": "a", "type": "text"},
		}}); code != 400 {
		t.Fatal("dup key should 400")
	}

	// staff list shows it
	code, body = h.do("GET", "/v1/projects/"+projA+"/forms", nil)
	if code != 200 || !strings.Contains(string(body), "Onboarding intake") {
		t.Fatalf("list = %d %s", code, body)
	}

	// portal sees the form
	code, body = h.doPortal("GET", pURL+"/forms")
	if code != 200 || !strings.Contains(string(body), "company_size") {
		t.Fatalf("portal forms = %d %s", code, body)
	}

	submit := func(ans map[string]string) (int, []byte) {
		return h.doPortalJSON("POST", pURL+"/forms/"+f.ID+"/submit", map[string]any{"answers": ans})
	}

	// missing required → 400 naming the field (THE enforcement)
	if code, body = submit(map[string]string{"go_live_date": "2026-10-01"}); code != 400 ||
		!strings.Contains(string(body), "company_size") {
		t.Fatalf("missing required = %d %s", code, body)
	}
	// bad date → 400
	if code, body = submit(map[string]string{"company_size": "500", "go_live_date": "Oct 1"}); code != 400 {
		t.Fatalf("bad date = %d %s", code, body)
	}
	// good submit → 201
	if code, body = submit(map[string]string{
		"company_size": "500", "go_live_date": "2026-10-01", "rogue_key": "stripped",
	}); code != http.StatusCreated {
		t.Fatalf("submit = %d %s", code, body)
	}
	// re-submit → 404 (no oracle: same as nonexistent form)
	if code, _ = submit(map[string]string{"company_size": "600"}); code != 404 {
		t.Fatalf("resubmit = 404 expected")
	}

	// staff CSV: header + 1 row, rogue_key column absent
	code, body = h.do("GET", "/v1/forms/"+f.ID+"/responses.csv", nil)
	if code != 200 {
		t.Fatalf("csv = %d %s", code, body)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv rows: %v", err)
	}
	if recs[0][0] != "submitted_at" || recs[0][3] != "company_size" {
		t.Fatalf("csv header: %v", recs[0])
	}
	if !strings.Contains(recs[1][3], "500") || !strings.Contains(recs[1][4], "2026-10-01") {
		t.Fatalf("csv row: %v", recs[1])
	}

	// ghost form submit → 404
	if code, _ := h.doPortalJSON("POST", pURL+"/forms/99999999-9999-9999-9999-999999999999/submit",
		map[string]any{"answers": map[string]string{}}); code != 404 {
		t.Fatal("ghost form should 404")
	}
}
