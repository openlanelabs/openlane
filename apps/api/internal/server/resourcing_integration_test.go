//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPeopleCRUDAndUtilization(t *testing.T) {
	srv, _, h, _ := timeTestStack(t)
	defer srv.Close()
	_, _, access := devLogin(h)

	// create
	code, body := h.doJWT("POST", "/v1/people", map[string]any{
		"name": "Anu Rao", "role": "architect", "skills": []string{"kubernetes", "go"},
		"cost_rate": "75", "bill_rate": "180", "capacity_hrs": "40",
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var p struct {
		ID          string   `json:"id"`
		Skills      []string `json:"skills"`
		CapacityHrs string   `json:"capacity_hrs"`
		Timezone    string   `json:"timezone"`
		Active      bool     `json:"active"`
	}
	json.Unmarshal(body, &p)
	if p.Timezone != "UTC" || !p.Active || p.CapacityHrs != "40.0" {
		t.Fatalf("defaults: %s", body)
	}
	if len(p.Skills) != 2 || p.Skills[0] != "go" { // text[] ordering not guaranteed; check membership
		found := map[string]bool{}
		for _, s := range p.Skills {
			found[s] = true
		}
		if !found["kubernetes"] || !found["go"] {
			t.Fatalf("skills: %v", p.Skills)
		}
	}

	// validation: bad timezone
	if code, body = h.doJWT("POST", "/v1/people", map[string]any{"name": "X", "role": "dev", "timezone": "Mars/Olympus"}, access); code != http.StatusBadRequest {
		t.Fatalf("tz validation = %d", code)
	}

	// list with skills filter
	if code, body = h.doJWT("GET", "/v1/people?skills=go,react", nil, access); code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var list []struct{ Name string }
	json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].Name != "Anu Rao" {
		t.Fatalf("filtered list = %s", body)
	}
	// skills filter excludes non-matching
	if code, body = h.doJWT("GET", "/v1/people?skills=rust", nil, access); code != http.StatusOK {
		t.Fatal(code)
	}
	json.Unmarshal(body, &list)
	if len(list) != 0 {
		t.Fatalf("rust filter = %s", body)
	}

	// patch: deactivate
	if code, body = h.doJWT("PATCH", "/v1/people/"+p.ID, map[string]any{"active": false}, access); code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	// active filter
	if code, body = h.doJWT("GET", "/v1/people?active=true", nil, access); code != http.StatusOK {
		t.Fatal(code)
	}
	json.Unmarshal(body, &list)
	if len(list) != 0 {
		t.Fatalf("active filter = %s", body)
	}
	if code, body = h.doJWT("PATCH", "/v1/people/"+p.ID, map[string]any{"active": true}, access); code != http.StatusOK {
		t.Fatal(code)
	}

	// foreign person → 404
	if code, _ = h.doJWT("PATCH", "/v1/people/99999999-9999-9999-9999-999999999999", map[string]any{"active": false}, access); code != http.StatusNotFound {
		t.Fatalf("foreign person = %d", code)
	}
}

func TestAllocationsAndUtilizationMath(t *testing.T) {
	srv, _, h, taskID := timeTestStack(t)
	defer srv.Close()
	_, _, access := devLogin(h)

	// person linked to the seeded member (user aaaa) so utilization finds their time
	code, body := h.doJWT("POST", "/v1/people", map[string]any{
		"name": "Asha M", "role": "admin", "user_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"capacity_hrs": "40", "bill_rate": "200",
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("person = %d %s", code, body)
	}
	var p struct{ ID string }
	json.Unmarshal(body, &p)

	// duplicate user link → 400 (unique)
	if code, body = h.doJWT("POST", "/v1/people", map[string]any{
		"name": "Dup", "role": "dev", "user_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}, access); code != http.StatusBadRequest {
		t.Fatalf("dup user link = %d", code)
	}

	// allocation on projA
	if code, body = h.doJWT("POST", "/v1/people/"+p.ID+"/allocations", map[string]any{
		"project_id": projA, "role": "admin", "hours_week": "20",
		"starts_on": "2026-01-01", "ends_on": "2026-03-31", "kind": "soft",
	}, access); code != http.StatusCreated {
		t.Fatalf("allocation = %d %s", code, body)
	}
	var a struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	json.Unmarshal(body, &a)
	if a.Kind != "soft" {
		t.Fatalf("kind: %s", body)
	}

	// inverted dates → 400
	if code, _ = h.doJWT("POST", "/v1/people/"+p.ID+"/allocations", map[string]any{
		"project_id": projA, "role": "admin", "hours_week": "10",
		"starts_on": "2026-03-31", "ends_on": "2026-01-01",
	}, access); code != http.StatusBadRequest {
		t.Fatalf("inverted = %d", code)
	}

	// list allocations, filter by project
	if code, body = h.doJWT("GET", "/v1/people/"+p.ID+"/allocations?project_id="+projA, nil, access); code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var allocs []struct{ ID string }
	json.Unmarshal(body, &allocs)
	if len(allocs) != 1 {
		t.Fatalf("allocs = %s", body)
	}

	// log 2h of time today, then utilization for a window containing it:
	// 5 workdays assumed; utilization = 120min / (workdays × 8h × 60)
	st, en := fixedSpan(120, 3)
	if code, body = h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st, "ended_at": en,
	}, access); code != http.StatusCreated {
		t.Fatalf("log = %d %s", code, body)
	}
	start := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	end := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	if code, body = h.doJWT("GET", "/v1/people/"+p.ID+"/utilization?start="+start+"&end="+end, nil, access); code != http.StatusOK {
		t.Fatalf("util = %d %s", code, body)
	}
	var u struct {
		LoggedMinutes   int      `json:"logged_minutes"`
		LoggableMinutes float64  `json:"loggable_minutes"`
		Utilization     *float64 `json:"utilization"`
		Weeks           float64  `json:"weeks"`
	}
	json.Unmarshal(body, &u)
	if u.LoggedMinutes != 120 {
		t.Fatalf("logged = %d", u.LoggedMinutes)
	}
	if u.Weeks < 1 || u.Weeks > 1.2 { // 7 days incl. ≥5 workdays
		t.Fatalf("weeks = %f", u.Weeks)
	}
	if u.Utilization == nil || *u.Utilization <= 0 || *u.Utilization > 1 {
		t.Fatalf("utilization = %s", body)
	}

	// delete allocation (soft)
	if code, _ = h.doJWT("DELETE", "/v1/allocations/"+a.ID, nil, access); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, body = h.doJWT("GET", "/v1/people/"+p.ID+"/allocations", nil, access); code != http.StatusOK {
		t.Fatal(code)
	}
	json.Unmarshal(body, &allocs)
	if len(allocs) != 0 {
		t.Fatalf("after delete = %s", body)
	}
	// re-delete → 404
	if code, _ = h.doJWT("DELETE", "/v1/allocations/"+a.ID, nil, access); code != http.StatusNotFound {
		t.Fatalf("re-delete = %d", code)
	}
}
