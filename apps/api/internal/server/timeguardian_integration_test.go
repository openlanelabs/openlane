//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTimeGuardianDigest: the §15.5 read-only scan — missing weekday
// time, >12h days, the agents_enabled kill switch, run as the APP role
// (the EXECUTE grant is under test).
func TestTimeGuardianDigest(t *testing.T) {
	_, _, h := filesTestStack(t)
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminPool.Close() })

	// users + memberships (asha/ravi members)
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha'),
		  ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'ravi@acme.test', 'ravi')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES
		  ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'member'),
		  ('11111111-1111-1111-1111-111111111111', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'member')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}

	// ravi = active billable person (should flag missing day); asha logs 13h
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO people (workspace_id, user_id, name, role, capacity_hrs, active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
		        'Ravi', 'Consultant', 40, true)`); err != nil {
		t.Fatal(err)
	}

	// pick a past weekday (walk back until Mon-Fri)
	day := time.Now().UTC().AddDate(0, 0, -1)
	for {
		if d := day.Weekday(); d != time.Saturday && d != time.Sunday {
			break
		}
		day = day.AddDate(0, 0, -1)
	}
	// asha: 13h across two entries (720m CHECK forces the split)
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, note)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		        'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
		        $1::date + interval '9 hours', $1::date + interval '17 hours', 480, 'morning'),
		       ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		        'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
		        $1::date + interval '18 hours', $1::date + interval '23 hours', 300, 'evening')`, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	// slack webhook (kill switch default on)
	if code, body := h.do("PUT", "/v1/settings/slack", map[string]any{
		"webhook_url": "https://hooks.slack.com/services/T000/B000/XXX",
	}); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("put slack = %d %s", code, body)
	}

	ap, err := pgxpool.New(ctx, appDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	runDigest := func() map[string][]map[string]any {
		rows, err := ap.Query(ctx, `SELECT workspace_id::text, flags FROM time_guardian_digest($1)`, day)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		defer rows.Close()
		out := map[string][]map[string]any{}
		for rows.Next() {
			var ws string
			var raw []byte
			if err := rows.Scan(&ws, &raw); err != nil {
				t.Fatal(err)
			}
			var flags []map[string]any
			if err := json.Unmarshal(raw, &flags); err != nil {
				t.Fatal(err)
			}
			out[ws] = flags
		}
		return out
	}

	got := runDigest()
	flagsA := got["11111111-1111-1111-1111-111111111111"]
	if len(flagsA) != 2 {
		t.Fatalf("flags = %v, want missing_day (ravi) + over_max (asha)", flagsA)
	}
	kinds := map[string]bool{}
	for _, f := range flagsA {
		kinds[f["kind"].(string)] = true
	}
	if !kinds["missing_day"] || !kinds["over_max"] {
		t.Fatalf("kinds = %v", kinds)
	}

	// kill switch off → no rows for ws A
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO workspace_settings (workspace_id) VALUES ('11111111-1111-1111-1111-111111111111')
		ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	got = runDigest()
	if _, ok := got["11111111-1111-1111-1111-111111111111"]; ok {
		t.Fatal("kill switch should drop ws A from the digest")
	}
}
