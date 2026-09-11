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
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

// HSProjectCreateArgs mirrors the api package struct (JSON coupling —
// kind name is the contract, #55).
type HSProjectCreateArgs struct {
	WorkspaceID string `json:"workspace_id"`
	DealID      string `json:"deal_id"`
	DealName    string `json:"deal_name"`
	Company     string `json:"company"`
	Amount      string `json:"amount,omitempty"`
	CloseDate   string `json:"close_date,omitempty"`
}

func (HSProjectCreateArgs) Kind() string { return "hubspot_deal_create" }

// TimeReminderArgs: nudge members with zero time logged this week
// (P1 §307). Window tag makes the enqueue unique per fire window —
// river's unique jobs dedupe the periodic ticks that land inside it.
type TimeReminderArgs struct {
	Window string `json:"window" river:"unique"` // e.g. 2026-W37-fri
}

func (TimeReminderArgs) Kind() string { return "time_reminder" }

// JiraStatusPushArgs mirrors the api package struct (JSON coupling).
type JiraStatusPushArgs struct {
	WorkspaceID string `json:"workspace_id"`
	TaskID      string `json:"task_id"`
	IssueKey    string `json:"issue_key"`
	Status      string `json:"status"`
}

func (JiraStatusPushArgs) Kind() string { return "jira_status_push" }

// jiraSecretAEAD — duplicated from api/server (apps couple via job JSON,
// not imports; OPENLANE_INTEGRATION_KEY is the shared secret).
func jiraSecretAEAD() (cipher.AEAD, error) {
	raw := os.Getenv("OPENLANE_INTEGRATION_KEY")
	if raw == "" {
		return nil, errors.New("OPENLANE_INTEGRATION_KEY not set")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("OPENLANE_INTEGRATION_KEY must be 32 bytes, base64")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func openJiraSecret(aead cipher.AEAD, sealed []byte) (string, error) {
	ns := aead.NonceSize()
	if len(sealed) < ns {
		return "", errors.New("sealed secret too short")
	}
	pt, err := aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// JiraStatusPushWorker: outbound half of §336 — task status → Jira
// transition. Loop guard: skip when last_synced_at >= task updated_at
// (the change came FROM jira).
type JiraStatusPushWorker struct {
	river.WorkerDefaults[JiraStatusPushArgs]
	pool *pgxpool.Pool
}

var jiraTransitionNames = map[string][]string{
	"in_progress": {"In Progress", "Start Progress"},
	"done":        {"Done", "Close Issue"},
	"todo":        {"To Do", "Reopen"},
}

func (w *JiraStatusPushWorker) Work(ctx context.Context, job *river.Job[JiraStatusPushArgs]) error {
	// one tx: set_config is tx-scoped (house rule) — every query below
	// rides the same connection or the RLS scope evaporates.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", job.Args.WorkspaceID); err != nil {
		return err
	}

	var instanceURL, patB64 string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(instance_url,''), COALESCE(external_refs->>'pat_enc','')
		FROM workspace_integrations WHERE provider = 'jira'`).Scan(&instanceURL, &patB64); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // unconfigured between enqueue and run
		}
		return err
	}
	if instanceURL == "" || patB64 == "" {
		return nil // PAT never set: inbound-only mode
	}
	patSealed, err := base64.StdEncoding.DecodeString(patB64)
	if err != nil {
		return err
	}
	// loop guard: change originated in jira
	var lastSync, taskUpdated time.Time
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(tl.last_synced_at, to_timestamp(0)), t.updated_at
		FROM tasks t JOIN task_links tl ON tl.task_id = t.id AND tl.provider = 'jira'
		WHERE t.id = $1`, job.Args.TaskID).Scan(&lastSync, &taskUpdated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // link removed since enqueue
		}
		return err
	}
	if !lastSync.Before(taskUpdated) {
		return nil
	}

	aead, err := jiraSecretAEAD()
	if err != nil {
		return err
	}
	pat, err := openJiraSecret(aead, patSealed)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(instanceURL, "/")
	names, ok := jiraTransitionNames[job.Args.Status]
	if !ok {
		return nil
	}

	// list transitions, find by name
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/rest/api/3/issue/"+job.Args.IssueKey+"/transitions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("jira transitions %d", res.StatusCode)
	}
	var tr struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return err
	}
	transitionID := ""
	for _, t := range tr.Transitions {
		for _, want := range names {
			if strings.EqualFold(t.Name, want) {
				transitionID = t.ID
			}
		}
	}
	if transitionID == "" {
		return nil // no matching transition available
	}

	post, err := http.NewRequestWithContext(ctx, "POST", base+"/rest/api/3/issue/"+job.Args.IssueKey+"/transitions",
		bytes.NewReader([]byte(`{"transition":{"id":`+jsonString(transitionID)+`}}`)))
	if err != nil {
		return err
	}
	post.Header.Set("Authorization", "Bearer "+pat)
	post.Header.Set("Content-Type", "application/json")
	pres, err := http.DefaultClient.Do(post)
	if err != nil {
		return err
	}
	_ = pres.Body.Close()
	if pres.StatusCode != http.StatusNoContent && pres.StatusCode != http.StatusOK {
		return fmt.Errorf("jira transition %d", pres.StatusCode)
	}

	// audit + mark synced — same tx, workspace-scoped
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'system', NULL,
		        'task.jira_sync_out', 'api', $2)`,
		job.Args.TaskID, []byte(`{"status":`+jsonString(job.Args.Status)+`}`)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE task_links SET last_synced_at = now() WHERE task_id = $1 AND provider = 'jira'`, job.Args.TaskID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reminderWindow: the firing window or "" outside them. Fri 16:00–16:59
// and Mon 09:00–09:59 UTC (spec §307; server TZ, per-user TZ later).
func reminderWindow(now time.Time) string {
	_, week := now.ISOWeek()
	if now.Weekday() == time.Friday && now.Hour() == 16 {
		return fmt.Sprintf("%d-W%02d-fri", now.Year(), week)
	}
	if now.Weekday() == time.Monday && now.Hour() == 9 {
		return fmt.Sprintf("%d-W%02d-mon", now.Year(), week)
	}
	return ""
}

// TimeReminderWorker: one query per Slack-configured workspace — members
// with zero entries this ISO week get one workspace-channel digest line.
type TimeReminderWorker struct {
	river.WorkerDefaults[TimeReminderArgs]
	pool *pgxpool.Pool
}

func (w *TimeReminderWorker) Work(ctx context.Context, job *river.Job[TimeReminderArgs]) error {
	// jitter (§455): spread the herd across the window
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(rand.Intn(60)) * time.Second):
	}

	rows, err := w.pool.Query(ctx, `SELECT workspace_id::text, slack_url, names, n FROM time_reminder_digest()`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var wsID, url, names string
		var n int
		if err := rows.Scan(&wsID, &url, &names, &n); err != nil {
			return err
		}
		// own short tx: enqueue the delivery (durable per ADR-0003)
		tx, err := w.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", wsID); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		// queue in river directly via SQL insert (worker app has no
		// enqueue client here; the api's InsertTx equivalent is the
		// river_job row itself) — ponytail: direct insert, river reads it.
		if _, err := tx.Exec(ctx, `
			INSERT INTO river_job (queue, kind, state, args, created_at, max_attempts, metadata)
			VALUES ('default', 'slack_notify', 'available',
			        jsonb_build_object('workspace_id', $1, 'url', $2, 'text', $3),
			        now(), 25, '{}'::jsonb)`,
			wsID, url, fmt.Sprintf("⏰ %d member(s) haven't logged time this week: %s", n, names)); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return rows.Err()
}

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
	defer func() { _ = res.Body.Close() }()
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
		WHERE workspace_id = $1 AND provider = 'salesforce' `, job.Args.WorkspaceID, job.Args.AccountName).
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
	refs := `{"salesforce_opportunity_id":"` + job.Args.OpportunityID + `"}`
	var id string
	if templateID != nil && *templateID != "" {
		// from template: copy name/description skeleton (P0: template body
		// copy = templates.name/description; task-tree copy is P1)
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, description, external_refs, status)
			SELECT workspace_id, $2::uuid, $3 || ' — ' || name, description, $4::jsonb, 'active'
			FROM templates WHERE id = $1::uuid
			RETURNING id`, *templateID, customerID, name, refs).Scan(&id); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, external_refs, status)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 'active') RETURNING id`,
			job.Args.WorkspaceID, customerID, name, refs).Scan(&id); err != nil {
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

type HSProjectCreateWorker struct {
	river.WorkerDefaults[HSProjectCreateArgs]
	pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

func (w *HSProjectCreateWorker) Work(ctx context.Context, job *river.Job[HSProjectCreateArgs]) error {
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
	if err := tx.QueryRow(ctx, `
		SELECT default_template_id FROM workspace_integrations
		WHERE workspace_id = $1 AND provider = 'hubspot'`, job.Args.WorkspaceID).Scan(&templateID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // integration removed between enqueue and run
		}
		return err
	}

	// Deal's company becomes the customer — lookup-then-insert, dupes
	// accepted at P0 like SF (ponytail: match HubSpot company IDs when
	// OAuth lands P1).
	var customerID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM customers
		WHERE workspace_id = $1::uuid AND name = $2`,
		job.Args.WorkspaceID, job.Args.Company).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO customers (workspace_id, name)
			VALUES ($1::uuid, $2) RETURNING id`,
			job.Args.WorkspaceID, job.Args.Company).Scan(&customerID)
	}
	if err != nil {
		return err
	}

	name := job.Args.DealName + " — onboarding"
	refs := `{"hubspot_deal_id":"` + job.Args.DealID + `"}`
	var id string
	if templateID != nil && *templateID != "" {
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, description, external_refs, status)
			SELECT workspace_id, $2::uuid, $3 || ' — ' || name, description, $4::jsonb, 'active'
			FROM templates WHERE id = $1::uuid
			RETURNING id`, *templateID, customerID, name, refs).Scan(&id); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, external_refs, status)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 'active') RETURNING id`,
			job.Args.WorkspaceID, customerID, name, refs).Scan(&id); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'system', NULL,
		        'project.created_from_hubspot', 'system')`, id); err != nil {
		return err
	}

	var url string
	if err := tx.QueryRow(ctx, `
		SELECT slack_webhook_url FROM workspace_settings
		WHERE workspace_id = $1 AND notify_project_created`,
		job.Args.WorkspaceID).Scan(&url); err == nil && url != "" {
		if _, err := w.river.InsertTx(ctx, tx, SlackNotifyArgs{
			WorkspaceID: job.Args.WorkspaceID,
			URL:         url,
			Text:        name + " created from HubSpot (closed-won " + job.Args.DealID + ")",
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
	river.AddWorker(workers, &HSProjectCreateWorker{pool: pool, river: rc})
	river.AddWorker(workers, &JiraStatusPushWorker{pool: pool})
	river.AddWorker(workers, &TimeReminderWorker{pool: pool})

	rc, err = river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			"default": {MaxWorkers: 10},
		},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			// every 15m; Work self-checks the Fri-16 / Mon-09 windows and
			// river's unique jobs (window tag) dedupe ticks inside one.
			river.NewPeriodicJob(
				river.PeriodicInterval(15*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) {
					w := reminderWindow(time.Now().UTC())
					if w == "" {
						return nil, nil
					}
					return TimeReminderArgs{Window: w}, &river.InsertOpts{
						UniqueOpts: river.UniqueOpts{ByArgs: true},
					}
				},
				nil),
		},
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
