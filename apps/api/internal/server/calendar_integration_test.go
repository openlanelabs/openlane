//go:build integration

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// calTestStack: fresh stack + local ICS feed server (public-ish: tests
// run with the loopback allowance only for stub? — no: fetchICS blocks
// loopback. Tests use OPENLANE_CAL_ALLOW_ANY? No — the guard is
// hardcoded. So the stack monkey-patches via env? Simplest: tests run
func TestCalendarPreviewAndImport(t *testing.T) {
	t.Setenv("OPENLANE_CAL_ALLOW_ANY", "1")
	_, _, h, _ := timeTestStack(t)
	_, _, access := devLogin(h)

	// preview: pasted ICS text — dry run, sync, no network
	var prev struct {
		Events []struct {
			UID     string `json:"uid"`
			Minutes int    `json:"minutes"`
		} `json:"events"`
		Count int `json:"count"`
	}
	now := time.Now().UTC()
	fmtICS := func(d time.Duration) string {
		return now.Add(d).UTC().Format("20060102T150405Z")
	}
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:meet-1",
		"SUMMARY:Onboarding call w/ Adobe",
		"DTSTART:" + fmtICS(-3*time.Hour),
		"DTEND:" + fmtICS(-2*time.Hour),
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:meet-2",
		"SUMMARY:Future planning (skipped)",
		"DTSTART:" + fmtICS(24*time.Hour),
		"DTEND:" + fmtICS(25*time.Hour),
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:meet-3",
		"SUMMARY:Too short (skipped)",
		"DTSTART:" + fmtICS(-6*time.Hour),
		"DTEND:" + fmtICS(-6*time.Hour+3*time.Minute),
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:meet-4",
		"SUMMARY:All day grind (capped 12h)",
		"DTSTART:" + fmtICS(-30*time.Hour),
		"DTEND:" + fmtICS(-6*time.Hour),
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	if code, body := h.doJWT("POST", "/v1/calendar/preview", map[string]any{
		"ics_text": ics,
	}, access); code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, body)
	} else if err := json.Unmarshal(body, &prev); err != nil {
		t.Fatal(err)
	}
	if prev.Count != 2 {
		t.Fatalf("preview count = %d, want 2 (future + <5min skipped)", prev.Count)
	}
	minutes := map[string]int{}
	for _, ev := range prev.Events {
		minutes[ev.UID] = ev.Minutes
	}
	if minutes["meet-1"] != 60 || minutes["meet-4"] != 720 {
		t.Fatalf("minutes = %v", minutes)
	}

	// import: 202 queued as a river job (fetch happens in the worker)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if err := EnsureRiver(context.Background(), ap); err != nil {
		t.Fatalf("EnsureRiver: %v", err)
	}
	if _, err := ap.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

	if code, body := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": "https://calendar.example/feed.ics", "project_id": projA,
	}, access); code != http.StatusAccepted || !strings.Contains(string(body), "queued") {
		t.Fatalf("import = %d %s", code, body)
	}

	var kind, argsJSON string
	if err := ap.QueryRow(context.Background(),
		`SELECT kind, args::text FROM river_job WHERE kind = 'calendar_import' LIMIT 1`).Scan(&kind, &argsJSON); err != nil {
		t.Fatalf("no calendar_import job: %v", err)
	}
	if !strings.Contains(argsJSON, "feed.ics") || !strings.Contains(argsJSON, projA) {
		t.Fatalf("job args = %s", argsJSON)
	}

	// bad scheme → 400 (fast validation, no enqueue)
	if code, _ := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": "ftp://x", "project_id": projA,
	}, access); code != http.StatusBadRequest {
		t.Fatalf("ftp scheme = %d", code)
	}
	// private host → 400
	if code, _ := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": "http://127.0.0.1:9/feed.ics", "project_id": projA,
	}, access); code != http.StatusBadRequest {
		t.Fatalf("private host = %d", code)
	}
}
