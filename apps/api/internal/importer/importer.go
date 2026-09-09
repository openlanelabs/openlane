// Package importer parses CSV exports from competitor tools and maps them to
// OpenLane task rows (spec §15.2 — the migration wedge). It is deliberately
// dependency-free: stdlib csv + time only.
package importer

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Row is the normalized shape every adapter must produce.
type Row struct {
	SourceRow int // 1-based line in the CSV (header = line 1)
	Title     string
	DueAt     *time.Time
	Notes     string
	Status    string // todo|in_progress|blocked|review|done|waived ("" → todo)
}

// RowError is a quarantined row with a human reason.
type RowError struct {
	SourceRow int    `json:"source_row"`
	Reason    string `json:"reason"`
}

// Verdict is the dry-run or post-run report.
type Verdict struct {
	OK      int        `json:"ok_count"`
	Errors  []RowError `json:"errors"`
	Source  string     `json:"source"`
	Warning []string   `json:"warnings,omitempty"`
}

// dateFormats accepted across sources. Asana exports US MM/DD/YYYY; Rocketlane
// and most APIs use ISO. Ambiguity (01/02/2026) resolves as US per Asana spec.
var dateFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04Z",
	"2006-01-02",
	"01/02/2006",
	"1/2/2006",
	"02-Jan-2006",
	"Jan 2, 2006",
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, f := range dateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// sanitize neutralizes spreadsheet formula injection (§15.2): a cell starting
// with =, +, -, @ can execute when the CSV is re-opened in Excel. We strip the
// leading sigil and mark the row.
func sanitize(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return s, false
	}
	switch t[0] {
	case '=', '+', '@':
		return t[1:], true
	case '-':
		// '-' alone is a legit list bullet; only risky when followed by a digit/formula char
		if len(t) > 1 && (t[1] == '=' || t[1] == '(' || (t[1] >= '0' && t[1] <= '9')) {
			return t[1:], true
		}
	}
	return s, false
}

// Parse reads a whole CSV (any encoding: UTF-8 with/without BOM, or latin-1)
// and maps rows via the source adapter.
func Parse(r io.Reader, source string) (*Verdict, []Row) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return &Verdict{Source: source, Errors: []RowError{{0, "read error: " + err.Error()}}}, nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if !utf8.Valid(raw) {
		raw = latin1ToUTF8(raw)
	}
	recs, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		return &Verdict{Source: source, Errors: []RowError{{0, "csv error: " + err.Error()}}}, nil
	}
	if len(recs) < 1 {
		return &Verdict{Source: source, Errors: []RowError{{0, "empty file"}}}, nil
	}

	var adapt func(cols map[string]string, line int) (Row, string) // returns row or reason
	switch strings.ToLower(source) {
	case "asana":
		adapt = adaptAsana
	case "rocketlane":
		adapt = adaptRocketlane
	case "generic":
		adapt = adaptGeneric
	default:
		return &Verdict{Source: source, Errors: []RowError{{0, fmt.Sprintf("unknown source %q (want asana|rocketlane|generic)", source)}}}, nil
	}

	v := &Verdict{Source: strings.ToLower(source)}
	rows := make([]Row, 0, len(recs)-1)
	for i, rec := range recs[1:] { // header consumed
		cols := map[string]string{}
		for j, h := range recs[0] {
			if j < len(rec) {
				cols[strings.ToLower(strings.TrimSpace(h))] = rec[j]
			}
		}
		row, reason := adapt(cols, i+2) // +2: 1 header + 1-based
		if reason != "" {
			v.Errors = append(v.Errors, RowError{i + 2, reason})
			continue
		}
		rows = append(rows, row)
		v.OK++
	}
	return v, rows
}

func baseRow(cols map[string]string) Row {
	raw := cols["title"]
	if raw == "" {
		raw = cols["name"] // asana calls it Name; generic/rocketlane docs say title/taskName
	}
	title, _ := sanitize(raw)
	notes, _ := sanitize(cols["notes"])
	return Row{Title: strings.TrimSpace(title), Notes: strings.TrimSpace(notes), Status: "todo"}
}

func adaptAsana(cols map[string]string, line int) (Row, string) {
	row := baseRow(cols)
	if row.Title == "" {
		if cols["name"] != "" { // title was pure formula junk
			return row, "title empty after sanitization"
		}
		return row, "missing name"
	}
	row.Notes = cols["notes"]
	if d, ok := parseDate(cols["due date"]); ok {
		row.DueAt = &d
	}
	if c, ok := parseDate(cols["completion date"]); ok {
		row.Status = "done"
		row.DueAt = nilOrKeep(row.DueAt, c)
		_ = c
	}
	return row, ""
}

func adaptRocketlane(cols map[string]string, line int) (Row, string) {
	row := baseRow(cols)
	if row.Title == "" {
		// rocketlane headers use taskName
		t, _ := sanitize(cols["taskname"])
		row.Title = strings.TrimSpace(t)
	}
	if row.Title == "" {
		return row, "missing task name"
	}
	if d, ok := parseDate(cols["duedate"]); ok {
		row.DueAt = &d
	}
	switch strings.ToLower(strings.TrimSpace(cols["status"])) {
	case "completed", "complete", "done":
		row.Status = "done"
	case "in progress", "in-progress":
		row.Status = "in_progress"
	case "blocked":
		row.Status = "blocked"
	case "not started", "open", "todo":
		row.Status = "todo"
	}
	return row, ""
}

func adaptGeneric(cols map[string]string, line int) (Row, string) {
	row := baseRow(cols)
	if row.Title == "" {
		return row, "missing title"
	}
	if d, ok := parseDate(cols["due_at"]); ok {
		row.DueAt = &d
	}
	switch strings.ToLower(strings.TrimSpace(cols["status"])) {
	case "todo", "":
		row.Status = "todo"
	case "in_progress", "in progress":
		row.Status = "in_progress"
	case "blocked":
		row.Status = "blocked"
	case "review":
		row.Status = "review"
	case "done":
		row.Status = "done"
	case "waived":
		row.Status = "waived"
	default:
		return row, "invalid status"
	}
	return row, ""
}

func nilOrKeep(orig *time.Time, _ time.Time) *time.Time { return orig }

// latin1ToUTF8 re-decodes bytes as ISO-8859-1 (common for Excel exports in
// non-UTF8 locales) so Mojibake doesn't reach the DB.
func latin1ToUTF8(b []byte) []byte {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		if c < 0x80 {
			out = append(out, c)
		} else {
			out = append(out, 0xC0|c>>6, 0x80|c&0x3F)
		}
	}
	return out
}
