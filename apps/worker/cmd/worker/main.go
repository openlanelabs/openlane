// OpenLane worker — River job processors (spec §15, ADR-0003). Runs as
// the openlane_app role: RLS applies, so tenant-scoped work must set
// app.workspace_id per job (the SF processor does; slack delivery needs
// no DB access — its payload is self-contained).
//
// Arg structs are duplicated from the API deliberately: they couple via
// the job's JSON (like the OpenAPI contract), not via internal imports
// (Go internal/ visibility forbids cross-app reuse). If a shape changes,
// both move — the kind name is the contract.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type SlackNotifyArgs struct {
	WorkspaceID string `json:"workspace_id"`
	URL         string `json:"url"`
	Text        string `json:"text"`
}

func (SlackNotifyArgs) Kind() string { return "slack_notify" }

type SFProjectCreateArgs struct {
	WorkspaceID   string `json:"workspace_id"`
	OpportunityID string `json:"opportunity_id"`
	AccountName   string `json:"account_name"`
	Amount        string `json:"amount,omitempty"`
	CloseDate     string `json:"close_date,omitempty"`
}

func (SFProjectCreateArgs) Kind() string { return "sf_project_create" }

type SlackNotifyWorker struct {
	river.WorkerDefaults[SlackNotifyArgs]
	client *http.Client
}

func NewSlackNotifyWorker() *SlackNotifyWorker {
	return &SlackNotifyWorker{client: &http.Client{Timeout: 2 * time.Second}}
}

func (w *SlackNotifyWorker) Work(ctx context.Context, job *river.Job[SlackNotifyArgs]) error {
	body := []byte(`{"text":` + jsonString(job.Args.Text) + `}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.Args.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := w.client.Do(req)
	if err != nil {
		return err // River retries with backoff
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned %d", res.StatusCode)
	}
	return nil
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// SFProjectCreateWorker: creates the project (from the workspace's
// default template if set) + queues the project.created slack notify in
// the same tx. Runs as openlane_app under RLS: workspace scope is the
// job's own WorkspaceID — a forged job row can only act inside its own
// workspace anyway (tenant_isolation WITH CHECK).
type SFProjectCreateWorker struct {
	river.WorkerDefaults[SFProjectCreateArgs]
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

func (w *SFProjectCreateWorker) Work(ctx context.Context, job *river.Job[SFProjectCreateArgs]) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true)", job.Args.WorkspaceID); err != nil {
		return err
	}

	var templateID *string
	var accountName string
	if err := tx.QueryRow(ctx, `
		SELECT default_template_id, $2 FROM workspace_integrations
		WHERE workspace_id = $1`, job.Args.WorkspaceID, job.Args.AccountName).
		Scan(&templateID, &accountName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // integration removed between enqueue and run — drop
		}
		return err
	}

	// the SF Account becomes the customer — projects.customer_id is NOT
	// NULL and every closed-won belongs to an account. Lookup-then-insert
	// by (workspace, name); no unique constraint exists on names.
	// ponytail: two closed-wons from one SF account can create duplicate
	// customer rows — fine for P0; store SF account IDs when OAuth lands
	// (P1) and match on those instead.
	var customerID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM customers
		WHERE workspace_id = $1::uuid AND name = $2`,
		job.Args.WorkspaceID, job.Args.AccountName).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO customers (workspace_id, name)
			VALUES ($1::uuid, $2) RETURNING id`,
			job.Args.WorkspaceID, job.Args.AccountName).Scan(&customerID)
	}
	if err != nil {
		return err
	}

	name := job.Args.AccountName + " — onboarding"
	var id string
	if templateID != nil && *templateID != "" {
		// from template: copy name/description skeleton (P0: template body
		// copy = templates.name/description; task-tree copy is P1)
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, description, status)
			SELECT workspace_id, $2::uuid, $3 || ' — ' || name, description, 'active'
			FROM templates WHERE id = $1::uuid
			RETURNING id`, *templateID, customerID, name).Scan(&id); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, status)
			VALUES ($1::uuid, $2::uuid, $3, 'active') RETURNING id`,
			job.Args.WorkspaceID, customerID, name).Scan(&id); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'system', NULL,
		        'project.created_from_salesforce', 'system')`, id); err != nil {
		return err
	}

	// queue the slack notify in the same tx (transactional outbox — if
	// this job retries, the notify insert retries with it)
	var url string
	if err := tx.QueryRow(ctx, `
		SELECT slack_webhook_url FROM workspace_settings
		WHERE workspace_id = $1 AND notify_project_created`,
		job.Args.WorkspaceID).Scan(&url); err == nil && url != "" {
		if _, err := w.river.InsertTx(ctx, tx, SlackNotifyArgs{
			WorkspaceID: job.Args.WorkspaceID,
			URL:         url,
			Text:        name + " created from Salesforce (closed-won " + job.Args.OpportunityID + ")",
		}, nil); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// enqueue-only client first: the SF worker uses its InsertTx to queue
	// slack_notify from inside its own tx. One client, registered workers,
	// then Start — InsertTx is valid before the client runs.
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		log.Fatalf("river client: %v", err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, NewSlackNotifyWorker())
	river.AddWorker(workers, &SFProjectCreateWorker{pool: pool, river: rc})

	rc, err = river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			"default": {MaxWorkers: 10},
		},
		Workers: workers,
	})
	if err != nil {
		log.Fatalf("river client: %v", err)
	}
	if err := rc.Start(ctx); err != nil {
		log.Fatalf("river start: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("worker stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rc.Stop(shutdownCtx); err != nil {
		log.Fatalf("river stop: %v", err)
	}
	log.Println("worker stopped")
}
