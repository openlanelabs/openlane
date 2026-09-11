package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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

// ipBlocked: loopback/private/link-local IPs and local hostnames. The
// worker's dial-time check is the authoritative guard; this pre-check
// fails fast on obviously-bad URLs.
func ipBlocked(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local")
}

// importableEvents: parse + filter shared by preview and the worker.
// Returns the events eligible for import (past, 5min–12h span).
func importableEvents(r io.Reader) ([]icsEvent, error) {
	events, _, err := parseICS(r)
	if err != nil {
		return nil, errors.New("malformed ics")
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

// calendarPreview: POST /v1/calendar/preview — dry run on pasted ICS
// content (§306 one-click convert UX: paste the calendar export,
// preview, then queue the feed URL for import). No network on this
// path — parsing only.
func (s *Server) calendarPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ICSText string `json:"ics_text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ICSText == "" {
		problem(w, http.StatusBadRequest, "ics_text required (paste the calendar export)")
		return
	}
	events, err := importableEvents(strings.NewReader(req.ICSText))
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

// calendarImport: POST /v1/calendar/import — enqueues the fetch as a
// river job (the platform's SSRF answer: user URLs fetch in the worker,
// like SF/HS/slack). Counts land in the audit log; re-imports no-op by
// calendar UID (idx_time_entries_cal_uid).
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
	// fast validation without fetching: scheme + obviously-private hosts
	u, err := url.Parse(req.ICSURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		problem(w, http.StatusBadRequest, "ics_url must be an http(s) URL")
		return
	}
	if ipBlocked(u.Hostname()) {
		problem(w, http.StatusBadRequest, "private calendar hosts are not allowed")
	}
	if s.river == nil {
		problem(w, http.StatusServiceUnavailable, "queue unavailable")
		return
	}
	ctx := r.Context()
	if _, err := s.river.Insert(ctx, CalendarImportArgs{
		WorkspaceID: workspaceFromCtx(ctx),
		ICSURL:      req.ICSURL,
		ProjectID:   req.ProjectID,
		UserID:      userFromCtx(ctx),
	}, nil); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}
