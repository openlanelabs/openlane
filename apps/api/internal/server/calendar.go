package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Calendar import v1 (P1 §306): ICS feed → draft time entries.
// Google/Outlook both export ICS; this is the zero-secret path that
// works self-hosted. OAuth + per-event project mapping come later.

type icsEvent struct {
	UID     string
	Summary string
	Start   time.Time
	End     time.Time
}

// parseICS: line-oriented VEVENT scan — DTSTART/DTEND/SUMMARY/UID only,
// simple unfold (continuation lines start with a space). Feeds from
// Google/Outlook are pre-expanded, so RRULE handling is out of scope.
func parseICS(r io.Reader) ([]icsEvent, time.Time, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 2<<20))
	if err != nil {
		return nil, time.Time{}, err
	}
	// unfold per RFC 5545 §3.1 (CRLF + space continuation; be liberal)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines)-1; i++ {
		for i+1 < len(lines) && strings.HasPrefix(lines[i+1], " ") {
			lines[i] += strings.TrimPrefix(lines[i+1], " ")
			lines = append(lines[:i+1], lines[i+2:]...)
		}
	}
	var events []icsEvent
	var cur *icsEvent
	var stamp time.Time
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "BEGIN:VEVENT"):
			cur = &icsEvent{}
		case strings.HasPrefix(ln, "END:VEVENT"):
			if cur != nil && !cur.Start.IsZero() && !cur.End.IsZero() && cur.UID != "" {
				events = append(events, *cur)
			}
			cur = nil
		case cur == nil:
			continue
		default:
			i := strings.Index(ln, ":")
			if i <= 0 {
				continue
			}
			base, val := ln[:i], ln[i+1:]
			if j := strings.Index(base, ";"); j > 0 {
				base = base[:j]
			}
			switch strings.ToUpper(base) {
			case "UID":
				cur.UID = strings.TrimSpace(val)
			case "SUMMARY":
				cur.Summary = strings.TrimSpace(unescapeICS(val))
			case "DTSTART":
				if t, ok := parseICSDate(val); ok {
					cur.Start = t
				}
			case "DTEND":
				if t, ok := parseICSDate(val); ok {
					cur.End = t
				}
			}
		}
		// LAST-MODIFIED etc. ignored
	}
	_ = stamp
	return events, stamp, nil
}

// parseICSDate: UTC Z form and naive form (treated UTC per issue scope).
func parseICSDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "Z") {
		if t, err := time.Parse("20060102T150405Z", v); err == nil {
			return t, true
		}
		return time.Time{}, false
	}
	if t, err := time.Parse("20060102T150405", v); err == nil {
		return t.UTC(), true
	}
	// all-day DATE values: treat as midnight UTC, 0 minutes → skipped by the min filter
	if t, err := time.Parse("20060102", v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func unescapeICS(v string) string {
	r := strings.NewReplacer(`\n`, "\n", `\,`, ",", `\;`, ";", `\\`, "\\")
	return r.Replace(v)
}

// fetchICS: SSRF-guarded GET — http(s) only, no private IPs, 10s cap.
func fetchICS(ctx context.Context, raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("ics_url must be an http(s) URL")
	}
	if os.Getenv("OPENLANE_CAL_ALLOW_ANY") != "1" && ipBlocked(u.Hostname()) {
		// tests/dev point at httptest servers (same convention as slack)
		return nil, errors.New("private calendar hosts are not allowed")
	}
	// The dial-time IP check is the real guard: hostname validation can
	// be DNS-rebinding-bypassed; the Control callback sees the actual
	// connected address (ssrf prevention, CodeQL-clean).
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if os.Getenv("OPENLANE_CAL_ALLOW_ANY") == "1" {
				return nil // tests/dev: httptest servers
			}
			if ipBlocked(host) {
				return errors.New("private calendar hosts are not allowed")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		return nil, fmt.Errorf("calendar feed returned %d", res.StatusCode)
	}
	return res.Body, nil
}

// ipBlocked: loopback/private/link-local IPs and local hostnames. The
// dial-time Control check in fetchICS is the authoritative guard; this
// pre-check fails fast on obviously-bad URLs.
func ipBlocked(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local")
}

// calendarEvents: shared parse + filter for preview and import.
// Returns the events eligible for import (past, 5min–12h span).
func (s *Server) calendarEvents(ctx context.Context, icsURL string) ([]icsEvent, error) {
	body, err := fetchICS(ctx, icsURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	events, _, err := parseICS(body)
	if err != nil {
		return nil, errors.New("malformed ics feed")
	}
	now := time.Now().UTC()
	weekAgo := now.AddDate(0, 0, -7)
	out := make([]icsEvent, 0, len(events))
	for _, ev := range events {
		st, en := ev.Start.UTC(), ev.End.UTC()
		// future events are not logged (§308 no future policy)
		if !en.After(weekAgo) || en.After(now) {
			continue
		}
		// noise filter: <5min skipped; >12h capped (§308 max day)
		if en.Sub(st) < 5*time.Minute {
			continue
		}
		if en.Sub(st) > 12*time.Hour {
			ev.End = st.Add(12 * time.Hour)
		}
		out = append(out, ev)
	}
	return out, nil
}

type calEventOut struct {
	UID     string `json:"uid"`
	Summary string `json:"summary"`
	Start   string `json:"started_at"`
	End     string `json:"ended_at"`
	Minutes int    `json:"minutes"`
}

// calendarPreview: POST /v1/calendar/preview — dry run (§306 one-click
// convert UX: preview, then confirm).
func (s *Server) calendarPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ICSURL string `json:"ics_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ICSURL == "" {
		problem(w, http.StatusBadRequest, "ics_url required")
		return
	}
	events, err := s.calendarEvents(r.Context(), req.ICSURL)
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]calEventOut, 0, len(events))
	for _, ev := range events {
		out = append(out, calEventOut{
			UID: ev.UID, Summary: ev.Summary,
			Start:   ev.Start.UTC().Format(time.RFC3339),
			End:     ev.End.UTC().Format(time.RFC3339),
			Minutes: int(ev.End.Sub(ev.Start).Minutes()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "count": len(out)})
}

// calendarImport: POST /v1/calendar/import — creates draft entries,
// idempotent by calendar UID (re-import = no duplicates, §311).
func (s *Server) calendarImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ICSURL    string `json:"ics_url"`
		ProjectID string `json:"project_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ICSURL == "" || req.ProjectID == "" {
		problem(w, http.StatusBadRequest, "ics_url and project_id required")
		return
	}
	events, err := s.calendarEvents(r.Context(), req.ICSURL)
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
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

	imported := 0
	for _, ev := range events {
		minutes := int(ev.End.Sub(ev.Start).Minutes())
		if minutes < 1 {
			continue
		}
		var id string
		// idx_time_entries_cal_uid makes re-imports no-op (ON CONFLICT
		// DO NOTHING → no RETURNING row → already imported, §311)
		err := tx.QueryRow(ctx, `
			INSERT INTO time_entries (workspace_id, project_id, task_id, user_id, started_at, ended_at, minutes, note, refs)
			SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, p.id, NULL,
			       NULLIF(current_setting('app.user_id', true), '')::uuid, $2, $3, $4,
			       COALESCE(NULLIF($5, ''), 'Imported from calendar'),
			       jsonb_build_object('calendar_uid', $6::text)
			FROM projects p
			WHERE p.id = $1::uuid AND p.deleted_at IS NULL
			ON CONFLICT DO NOTHING
			RETURNING id`,
			req.ProjectID, ev.Start, ev.End, minutes, ev.Summary, ev.UID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // duplicate calendar UID — already imported
		}
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		imported++
		_ = id
	}
	skipped := len(events) - imported
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'integration', NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'calendar.imported', 'api', $1)`,
		mustJSON(map[string]int{"imported": imported, "skipped": skipped})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported, "skipped": skipped})
}
