//go:build integration

package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTimeReminderDigest: migration 00027 + time_reminder_digest().
// Seeds: ws A with slack + reminder toggle; asha (member, no entries)
// and ravi (member, one entry this week). Digest must name asha only.
func TestTimeReminderDigest(t *testing.T) {
	_, _, h := filesTestStack(t)
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminPool.Close() })

	// seed: asha (member, no entries this week), ravi (member, one entry)
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha'),
		  ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'ravi@acme.test', 'Ravi')
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
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, note)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		        'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', now() - interval '2 hours', now() - interval '1 hour', 60, 'already logged')`); err != nil {
		t.Fatal(err)
	}

	// slack webhook + default reminder toggle (migration default true)
	if code, body := h.do("PUT", "/v1/settings/slack", map[string]any{
		"webhook_url": "https://hooks.slack.com/services/T000/B000/XXX",
	}); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("put slack = %d %s", code, body)
	}

	// run the digest as the APP role (the grant is the thing under test)
	ap, err := pgxpool.New(ctx, appDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	rows, err := ap.Query(ctx, `SELECT workspace_id::text, slack_url, names, n FROM time_reminder_digest()`)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var ws, url, names string
		var n int
		if err := rows.Scan(&ws, &url, &names, &n); err != nil {
			t.Fatal(err)
		}
		if ws == "11111111-1111-1111-1111-111111111111" {
			found = true
			if names != "Asha" || n != 1 {
				t.Fatalf("digest names = %q n = %d, want asha only", names, n)
			}
			if url == "" {
				t.Fatal("digest lost the slack url")
			}
		}
	}
	if !found {
		t.Fatal("digest missing ws A row (asha has no entries this week)")
	}

	// toggle off → no rows
	if _, err := adminPool.Exec(ctx, `
		UPDATE workspace_settings SET notify_reminders = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM time_reminder_digest()`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("toggle off: n=%d err=%v", n, err)
	}
}
