//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// metricsTestStack: files stack + a user + an old completed project with
// actual_go_live (TTV data) + a second fresh project.
func metricsTestStack(t *testing.T) (*pgxpool.Pool, *httptestSrv) {
	t.Helper()
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)
	// user for JWT + membership
	if _, err := ap.Exec(t.Context(),
		`INSERT INTO users (id, email, display_name) VALUES ($1, 'asha@acme.test', 'Asha') ON CONFLICT (id) DO NOTHING`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(t.Context(),
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, 'admin') ON CONFLICT DO NOTHING`, wsA, userID); err != nil {
		t.Fatal(err)
	}
	// TTV anchor: old project created exactly 10 days ago (day-truncated),
	// completed with go-live exactly 6 days ago → exactly 4.00-day TTV
	if _, err := ap.Exec(t.Context(), `
		INSERT INTO projects (id, workspace_id, customer_id, name, status, actual_go_live, created_at)
		VALUES ('99999999-1111-4111-8111-999999999999', $1, $2, 'Done Project', 'completed',
		        date_trunc('day', now()) - interval '6 days',
		        date_trunc('day', now()) - interval '10 days')`,
		wsA, custID); err != nil {
		t.Fatalf("seed ttv project: %v", err)
	}
	return ap, h
}

func TestMetricsStaff(t *testing.T) {
	_, h := metricsTestStack(t)
	_, _, access := devLogin(h)

	// TTV: 4 days (seeded); no other completed-with-golive projects exist
	code, body := h.doJWT("GET", "/v1/metrics", nil, access)
	if code != http.StatusOK {
		t.Fatalf("metrics = %d %s", code, body)
	}
	var m struct {
		TTV struct {
			Count int     `json:"count"`
			Avg   float64 `json:"avg_days"`
		} `json:"ttv"`
		PortalOpen struct {
			ActiveLinks int `json:"active_links"`
			Opened      int `json:"opened"`
		} `json:"portal_open_rate"`
		APILatency struct {
			P95MS    float64 `json:"p95_ms"`
			Requests uint64  `json:"requests"`
		} `json:"api_latency"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v %s", err, body)
	}
	if m.TTV.Count != 1 || m.TTV.Avg != 4 {
		t.Fatalf("ttv = %+v (want 1 project, 4d avg)", m.TTV)
	}
	// portal: no links yet → zero rate, no divide-by-zero
	if m.PortalOpen.ActiveLinks != 0 || m.PortalOpen.Opened != 0 {
		t.Fatalf("portal open = %+v", m.PortalOpen)
	}
	// latency recorded (this request itself counts)
	if m.APILatency.Requests == 0 {
		t.Fatal("requests == 0 — middleware not recording")
	}

	// Prometheus text shape
	code, body = h.doJWT("GET", "/metrics", nil, access)
	if code != http.StatusOK {
		t.Fatalf("prometheus = %d %s", code, body)
	}
	txt := string(body)
	for _, want := range []string{
		"openlane_ttv_days{stat=\"avg\"} 4.0",
		"openlane_api_requests_total",
		"openlane_api_latency_ms{p=\"95\"}",
	} {
		if !strings.Contains(txt, want) {
			t.Fatalf("prometheus text missing %q:\n%s", want, txt)
		}
	}
}

func TestMetricsPortalOpenRate(t *testing.T) {
	_, h := metricsTestStack(t)
	_, _, access := devLogin(h)

	// Create a link, never open it → rate 0
	code, body := h.doJWT("POST", "/v1/portal-links", map[string]any{
		"project_id": projA, "contact_id": contactID, "ttl_hours": 168,
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("link = %d %s", code, body)
	}
	var link struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &link)

	code, body = h.doJWT("GET", "/v1/metrics", nil, access)
	var m struct {
		PortalOpen struct {
			ActiveLinks int     `json:"active_links"`
			Opened      int     `json:"opened"`
			Rate        float64 `json:"rate"`
		} `json:"portal_open_rate"`
	}
	json.Unmarshal(body, &m)
	if m.PortalOpen.ActiveLinks != 1 || m.PortalOpen.Opened != 0 || m.PortalOpen.Rate != 0 {
		t.Fatalf("before open: %+v", m.PortalOpen)
	}

	// Open the portal once (session) → last_used_at set → rate 1.0
	if c, b := h.doPortal("GET", "/p/"+link.Token+"/session"); c != 200 {
		t.Fatalf("portal session = %d %s", c, b)
	}
	code, body = h.doJWT("GET", "/v1/metrics", nil, access)
	json.Unmarshal(body, &m)
	if m.PortalOpen.ActiveLinks != 1 || m.PortalOpen.Opened != 1 || m.PortalOpen.Rate != 1 {
		t.Fatalf("after open: %+v", m.PortalOpen)
	}
}

func TestMetricsAuthRequired(t *testing.T) {
	_, h := metricsTestStack(t)
	// portal-style request (no bearer) → 401; metrics must never be public
	req, err := http.NewRequest("GET", h.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth /metrics = %d (want 401)", res.StatusCode)
	}
	// static token works (staff shape)
	code, body := h.do("GET", "/v1/metrics", nil)
	if code != http.StatusOK {
		t.Fatalf("static metrics = %d %s", code, body)
	}
}

// TTV anchor set automatically on active→completed (§588 TTV tracked).
func TestProjectCompletionSetsGoLive(t *testing.T) {
	_, h := metricsTestStack(t)
	_, _, access := devLogin(h)

	// draft → active needs start_date + a task; then complete it (task not
	// required) — actual_go_live must be stamped automatically.
	code, body := h.doJWT("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Metrics task", "owner_type": "internal", "customer_visible": false, "required": false,
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("task = %d %s", code, body)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &task)

	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{
		"status": "active", "start_date": "2026-01-01",
	}, access); code != http.StatusOK {
		t.Fatalf("activate = %d %s", code, body)
	}
	// complete the task first (completion guard needs required done; this one isn't required, but complete it anyway for realism)
	if code, body = h.doJWT("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "in_progress"}, access); code != http.StatusOK {
		t.Fatalf("task in_progress = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "done"}, access); code != http.StatusOK {
		t.Fatalf("task done = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{
		"status": "completed",
	}, access); code != http.StatusOK {
		t.Fatalf("complete = %d %s", code, body)
	}

	// metrics must now count 2 completed-with-golive (seeded 4d + this one)
	code, body = h.doJWT("GET", "/v1/metrics", nil, access)
	var m struct {
		TTV struct {
			Count int `json:"count"`
		} `json:"ttv"`
	}
	json.Unmarshal(body, &m)
	if m.TTV.Count != 2 {
		t.Fatalf("ttv count = %d (want 2 — auto go-live stamp failed?)", m.TTV.Count)
	}
	_ = time.Now
}
