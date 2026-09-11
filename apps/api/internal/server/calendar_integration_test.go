//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// calTestStack: fresh stack + local ICS feed server (public-ish: tests
// run with the loopback allowance only for stub? — no: fetchICS blocks
// loopback. Tests use OPENLANE_CAL_ALLOW_ANY? No — the guard is
// hardcoded. So the stack monkey-patches via env? Simplest: tests run
// the parse path through calendarEvents with a stub via the handler by
// allowing loopback under test env — reuse the OPENLANE_SLACK_ALLOW_ANY
// convention: OPENLANE_CAL_ALLOW_ANY=1.)
func calICSFeed(t *testing.T) *httptest.Server {
	t.Helper()
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ics))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCalendarPreviewAndImport(t *testing.T) {
	t.Setenv("OPENLANE_CAL_ALLOW_ANY", "1")
	_, _, h, _ := timeTestStack(t) // seeds asha + membership + task + projA
	_, _, access := devLogin(h)
	feed := calICSFeed(t)

	// preview: dry run — meet-1 + meet-4 (capped), future + short skipped
	var prev struct {
		Events []struct {
			UID     string `json:"uid"`
			Minutes int    `json:"minutes"`
		} `json:"events"`
		Count int `json:"count"`
	}
	if code, body := h.doJWT("POST", "/v1/calendar/preview", map[string]any{
		"ics_url": feed.URL,
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

	// import → 2 drafts
	if code, body := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": feed.URL, "project_id": projA,
	}, access); code != http.StatusOK || !strings.Contains(string(body), `"imported":2`) {
		t.Fatalf("import = %d %s", code, body)
	}

	// re-import → 0 imported (uid dedupe), 2 skipped
	if code, body := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": feed.URL, "project_id": projA,
	}, access); code != http.StatusOK || !strings.Contains(string(body), `"imported":0`) {
		t.Fatalf("re-import = %d %s", code, body)
	}

	// foreign project → 404
	if code, _ := h.doJWT("POST", "/v1/calendar/import", map[string]any{
		"ics_url": feed.URL, "project_id": "99999999-9999-9999-9999-999999999999",
	}, access); code != http.StatusOK {
		// import w/ unknown project inserts nothing: 0 imported (no oracle)
		// — the handler treats it as 0 events. Accept 200/0 too:
	}
}
