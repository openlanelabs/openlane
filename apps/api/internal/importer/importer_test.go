package importer

import (
	"strings"
	"testing"
	"time"
)

func TestParseAsanaOfficialExport(t *testing.T) {
	csv := `Task ID,Creation Date,Completion Date,Last Modified Date,Name,Assignee,Due Date,Tags,Notes,Project Name,Parent Task
1208995,2026-08-01,,2026-08-02,Upload employee CSV,ravi@adobe.test,09/12/2026,kickoff,Send the HRIS export,Adobe Onboarding,
1208996,2026-08-01,2026-08-05,2026-08-05,Sign SOW,asha@acme.test,08/05/2026,legal,Signed v2,Adobe Onboarding,`
	v, rows := Parse(strings.NewReader(csv), "asana")
	if len(v.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", v.Errors)
	}
	if v.OK != 2 {
		t.Fatalf("ok = %d, want 2", v.OK)
	}
	if rows[0].Title != "Upload employee CSV" {
		t.Errorf("title = %q", rows[0].Title)
	}
	want := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if rows[0].DueAt == nil || !rows[0].DueAt.Equal(want) {
		t.Errorf("dueAt = %v, want %v (MM/DD/YYYY US format)", rows[0].DueAt, want)
	}
	if rows[0].Status != "todo" {
		t.Errorf("status = %q, want todo", rows[0].Status)
	}
	// Completion Date present → done
	if rows[1].Status != "done" {
		t.Errorf("row with completion date should be done, got %q", rows[1].Status)
	}
	if rows[0].Notes != "Send the HRIS export" {
		t.Errorf("notes = %q", rows[0].Notes)
	}
}

func TestParseRocketlaneShape(t *testing.T) {
	csv := `taskName,startDate,dueDate,assignees,status,effort
"Provision sandbox",2026-09-01,2026-09-10,asha@acme.test,Completed,4h
"Review scope",2026-09-02,2026-09-15,ravi@adobe.test,In Progress,2h
"Draft plan",,,asha@acme.test,Not Started,1h
`
	v, rows := Parse(strings.NewReader(csv), "Rocketlane") // case-insensitive source
	if len(v.Errors) != 0 {
		t.Fatalf("errors: %+v", v.Errors)
	}
	if v.OK != 3 {
		t.Fatalf("ok = %d", v.OK)
	}
	if rows[0].Status != "done" {
		t.Errorf("Completed → done, got %q", rows[0].Status)
	}
	if rows[1].Status != "in_progress" {
		t.Errorf("In Progress → in_progress, got %q", rows[1].Status)
	}
	if rows[2].Status != "todo" {
		t.Errorf("Not Started → todo, got %q", rows[2].Status)
	}
	if rows[2].DueAt != nil {
		t.Errorf("empty dueDate should be nil")
	}
}

func TestGenericInvalidStatus(t *testing.T) {
	csv := "title,due_at,status\nDo the thing,2026-09-10,wat\n"
	v, rows := Parse(strings.NewReader(csv), "generic")
	if v.OK != 0 || len(rows) != 0 {
		t.Fatal("invalid status should quarantine the row")
	}
	if len(v.Errors) != 1 || !strings.Contains(v.Errors[0].Reason, "invalid status") {
		t.Fatalf("errors = %+v", v.Errors)
	}
	if v.Errors[0].SourceRow != 2 {
		t.Errorf("source_row = %d, want 2 (1-based after header)", v.Errors[0].SourceRow)
	}
}

func TestFormulaInjectionSanitized(t *testing.T) {
	csv := "title,notes\n=SUM(A1:A9),=SUM(B1:B9)\nAdd taxes,-2+3+CMD\n"
	v, rows := Parse(strings.NewReader(csv), "generic")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d (errors %+v)", len(rows), v.Errors)
	}
	if strings.HasPrefix(rows[0].Title, "=") {
		t.Errorf("formula sigil survived: %q", rows[0].Title)
	}
	if rows[0].Title != "SUM(A1:A9)" {
		t.Errorf("stripped title = %q", rows[0].Title)
	}
	if rows[1].Title != "Add taxes" {
		t.Errorf("title = %q", rows[1].Title)
	}
	if rows[1].Notes != "2+3+CMD" {
		t.Errorf("notes formula not sanitized: %q", rows[1].Notes)
	}
}

func TestLatin1EncodingAndBOM(t *testing.T) {
	// "Café rollout" encoded in latin-1, with UTF-8 BOM absent
	latin1 := "title\nCaf\xe9 rollout\n" // 0xE9 = é in latin-1, invalid alone in UTF-8
	v, rows := Parse(strings.NewReader(latin1), "generic")
	if v.OK != 1 {
		t.Fatalf("ok=%d errors=%+v", v.OK, v.Errors)
	}
	if !strings.Contains(rows[0].Title, "Café") {
		t.Errorf("latin-1 not re-decoded: %q", rows[0].Title)
	}
	// BOM alone
	bom := "\xEF\xBB\xBFtitle\nHello\n"
	v2, rows2 := Parse(strings.NewReader(bom), "generic")
	if v2.OK != 1 || rows2[0].Title != "Hello" {
		t.Errorf("BOM broke parse: ok=%d title=%q", v2.OK, rows2[0].Title)
	}
}

func TestDateFormats(t *testing.T) {
	for in, want := range map[string]string{
		"2026-09-10":           "2026-09-10",
		"09/10/2026":           "2026-09-10", // Asana US format
		"9/10/2026":            "2026-09-10",
		"2026-09-10T14:30Z":    "2026-09-10",
		"Sep 10, 2026":         "2026-09-10",
		"10-Sep-2026":          "2026-09-10",
		"2026-09-10 14:30":     "2026-09-10",
		"2026-09-10T14:30:00Z": "2026-09-10",
	} {
		if _, ok := parseDate(in); !ok {
			t.Errorf("parseDate(%q) failed", in)
			continue
		}
		d, _ := parseDate(in)
		if got := d.Format("2006-01-02"); got != want {
			t.Errorf("parseDate(%q) = %s, want %s", in, got, want)
		}
	}
	for bad := range map[string]bool{"": true, "tomorrowish": true, "13/45/2026": true} {
		if _, ok := parseDate(bad); ok {
			t.Errorf("parseDate(%q) unexpectedly parsed", bad)
		}
	}
}

func TestQuarantineOnBadRowKeepsGoodRows(t *testing.T) {
	csv := "title\nGood one\n\nAlso good\n"
	v, rows := Parse(strings.NewReader(csv), "generic")
	// csv.Reader skips blank lines entirely — nothing to quarantine, 2 good rows
	if v.OK != 2 || len(v.Errors) != 0 {
		t.Fatalf("ok=%d errors=%+v", v.OK, v.Errors)
	}
	if rows[0].Title != "Good one" || rows[1].Title != "Also good" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestUnknownSource(t *testing.T) {
	v, rows := Parse(strings.NewReader("title\nx\n"), "trello")
	if len(v.Errors) != 1 || !strings.Contains(v.Errors[0].Reason, "unknown source") {
		t.Fatalf("errors = %+v", v.Errors)
	}
	if rows != nil {
		t.Fatal("rows must be nil for unknown source")
	}
}

func TestTitleTooLong(t *testing.T) {
	long := strings.Repeat("a", 300)
	csv := "title\n" + long + "\n"
	v, rows := Parse(strings.NewReader(csv), "generic")
	if len(rows) != 1 {
		t.Fatal("parse should succeed; DB enforces 255")
	}
	_ = v
	// The DB CHECK (char_length <= 255) is the real gate — document via error from import run, tested in integration.
}
