package server

// River queue glue (ADR-0003): job arg shapes + transactional enqueue.
// The outbox pattern lives here — jobs are inserted with the API's river
// client in the SAME tx as the data they serve, so a crash between write
// and notify can't lose one half of the pair.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// SlackNotifyArgs: one job per outbound Slack webhook POST.
// payload is pre-rendered so the worker needs no DB access to send it.
type SlackNotifyArgs struct {
	WorkspaceID string `json:"workspace_id"`
	URL         string `json:"url"`
	Text        string `json:"text"`
}

func (SlackNotifyArgs) Kind() string { return "slack_notify" }

// SFProjectCreateArgs: closed-won Opportunity -> project (+ template).
type SFProjectCreateArgs struct {
	WorkspaceID   string `json:"workspace_id"`
	OpportunityID string `json:"opportunity_id"`
	AccountName   string `json:"account_name"`
	Amount        string `json:"amount,omitempty"`
	CloseDate     string `json:"close_date,omitempty"`
}

func (SFProjectCreateArgs) Kind() string { return "sf_project_create" }

// newRiverClient: enqueue-only client (no Start()) backed by the pool.
// Workers live in apps/worker; the API only produces jobs.
func newRiverClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{})
}

// enqueueSlack queues a Slack notification in the caller's tx.
func enqueueSlack(ctx context.Context, rc *river.Client[pgx.Tx], tx pgx.Tx, workspaceID, url, text string) error {
	if url == "" { // no slack configured — nothing to notify, not an error
		return nil
	}
	_, err := rc.InsertTx(ctx, tx, SlackNotifyArgs{
		WorkspaceID: workspaceID, URL: url, Text: text,
	}, nil)
	return err
}

// EnsureRiver: applies River's own schema + grants for the app role.
// Runs as the OWNER connection (migrations are owner-only per the compose
// trust boundary). Idempotent: River's migrator no-ops when current.
// ponytail: grants live here instead of a goose migration because River's
// migrator must run first and is versioned independently — one startup
// call, not a two-phase migration dance.
func EnsureRiver(ctx context.Context, adminPool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(adminPool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	_, err = adminPool.Exec(ctx, `
		GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE river_job, river_queue, river_leader, river_migration, river_notification TO openlane_app;
		GRANT USAGE, SELECT ON SEQUENCE river_job_id_seq, river_notification_id_seq TO openlane_app;`)
	if err != nil {
		return fmt.Errorf("river grants: %w", err)
	}
	return nil
}
