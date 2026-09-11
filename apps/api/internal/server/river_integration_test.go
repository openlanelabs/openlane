//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tiny local helpers (self-contained test plumbing)
func jsonBodyRequest(t *testing.T, h *httptestSrv, method, path string, raw []byte) (*http.Request, error) {
	t.Helper()
	req, err := http.NewRequest(method, h.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req, err
}

func doRequest(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func httptestServerStatus(t *testing.T, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func httptestServerFunc(t *testing.T, f http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

// riverTestStack: fresh DB (files stack shape) + integration key env.
func riverTestStack(t *testing.T) (*httptestSrv, string) {
	t.Helper()
	_, _, h := filesTestStack(t)
	// filesTestStack runs goose reset+up as owner; river's own schema +
	// grants must exist before river jobs insert
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if err := EnsureRiver(context.Background(), ap); err != nil {
		t.Fatalf("EnsureRiver: %v", err)
	}
	// river_job lives outside goose (River's own migrator), so it survives
	// the goose reset in filesTestStack — clear it per test run.
	if _, err := ap.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatalf("river_job cleanup: %v", err)
	}
	// 32-byte base64 key
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc := base64.StdEncoding.EncodeToString(key)
	_ = key
	t.Setenv("OPENLANE_INTEGRATION_KEY", enc)
	return h, admin
}

func TestSFWebhookDedupeAndSignature(t *testing.T) {
	h, admin := riverTestStack(t)

	// configure SF integration (staff static token = ws ctx)
	secret := "supersecret-webhook-key"
	code, body := h.do("PUT", "/v1/integrations/salesforce", map[string]any{
		"instance_url":   "https://acme.my.salesforce.com",
		"webhook_secret": secret,
	})
	if code != 204 {
		t.Fatalf("put sf settings = %d %s", code, body)
	}

	// GET never echoes the secret
	code, body = h.do("GET", "/v1/integrations/salesforce", nil)
	if code != 200 || strings.Contains(string(body), secret) {
		t.Fatalf("get sf settings leaked or failed: %d %s", code, body)
	}

	// closed-won event, HMAC-signed
	ev := map[string]any{
		"organizationId": "00Dxx0000000001",
		"opportunityId":  "006xx0000000001",
		"accountName":    "Acme Corp",
		"amount":         "50000",
		"closeDate":      "2026-09-10",
		"stageName":      "Closed Won",
		"event":          "opportunity.closed_won",
	}
	raw, _ := json.Marshal(ev)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	post := func(rawBody []byte, sigHeader string) (int, []byte) {
		req, _ := jsonBodyRequest(h.t, h, "POST", "/v1/integrations/salesforce/webhook?ws=acme", rawBody)
		req.Header.Set("X-OpenLane-Signature", sigHeader)
		return doRequest(h.t, req)
	}
	post0 := func(sigHeader string) (int, []byte) { return post(raw, sigHeader) }

	// bad signature → 401
	if code, body := post0("garbage"); code != 401 {
		t.Fatalf("bad sig = %d %s", code, body)
	}

	// valid → queued
	code, body = post0(sig)
	if code != 200 || !strings.Contains(string(body), "queued") {
		t.Fatalf("closed-won = %d %s", code, body)
	}

	// duplicate → "duplicate", no second job
	code, body = post0(sig)
	if code != 200 || !strings.Contains(string(body), "duplicate") {
		t.Fatalf("duplicate delivery = %d %s", code, body)
	}

	// non-closed-won → ignored
	ev["stageName"] = "Prospecting"
	ev["opportunityId"] = "006xx0000000002"
	raw2, _ := json.Marshal(ev)
	mac2 := hmac.New(sha256.New, []byte(secret))
	mac2.Write(raw2)
	if code, body := post(raw2, base64.StdEncoding.EncodeToString(mac2.Sum(nil))); code != 200 || !strings.Contains(string(body), "ignored") {
		t.Fatalf("prospecting = %d %s", code, body)
	}

	// unknown ws slug → 404
	req, _ := jsonBodyRequest(h.t, h, "POST", "/v1/integrations/salesforce/webhook?ws=nope", raw)
	req.Header.Set("X-OpenLane-Signature", sig)
	if code, _ := doRequest(h.t, req); code != 404 {
		t.Fatalf("unknown slug = %d, want 404", code)
	}

	// exactly one sf_project_create job committed
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var n int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'sf_project_create'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sf jobs = %d %v", n, err)
	}
	var dedupe int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM integration_sync_log WHERE provider = 'salesforce'`).Scan(&dedupe); err != nil || dedupe != 1 {
		t.Fatalf("sync log = %d %v", dedupe, err)
	}
}

func TestSlackNotifyOutbox(t *testing.T) {
	// project.created now enqueues a slack_notify river job (transactional
	// outbox) instead of firing a goroutine.
	h, admin := riverTestStack(t)

	// point slack at a sink that always fails: the JOB must still exist
	// (durable), which is the whole point of the outbox
	stub := httptestServerStatus(t, 500)
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "1")
	code, body := h.do("PUT", "/v1/settings/slack", map[string]any{"webhook_url": stub})
	if code != 204 {
		t.Fatalf("slack settings = %d %s", code, body)
	}
	code, body = h.do("POST", "/v1/projects", map[string]any{"name": "Outbox Proof", "customer_id": custID})
	if code != 201 {
		t.Fatalf("project create = %d %s", code, body)
	}

	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var n int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'slack_notify' AND state = 'available'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("slack_notify jobs = %d %v", n, err)
	}
}

// TestWorkerEndToEnd: run the real worker binary against the test DB;
// it processes slack_notify (against a stub) and sf_project_create
// (creates the project from the webhook test flow).
func TestWorkerEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: runs the worker binary")
	}
	h, admin := riverTestStack(t)

	// slack sink stub recording deliveries
	var deliveries int32
	stub := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deliveries, 1)
		w.WriteHeader(200)
	})
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "1")
	h.do("PUT", "/v1/settings/slack", map[string]any{"webhook_url": stub})

	// seed + fire the sf webhook (closed-won); template set so the
	// worker's template branch runs (latent #55 bug: it referenced
	// projects.description before 00015 added the column)
	secret := "worker-e2e-secret"
	code, tplBody := h.do("POST", "/v1/templates", map[string]any{
		"name": "SF std", "category": "onboarding", "description": "sf flow",
		"phases": []map[string]any{{
			"name":  "Kickoff",
			"tasks": []map[string]any{{"title": "Kickoff call", "due_offset_days": 1, "required": true, "customer_visible": true}},
		}},
	})
	if code != 201 {
		t.Fatalf("template = %d %s", code, tplBody)
	}
	var tpl struct {
		ID string `json:"id"`
	}
	json.Unmarshal(tplBody, &tpl)
	h.do("PUT", "/v1/integrations/salesforce", map[string]any{
		"instance_url": "https://x.my.salesforce.com", "webhook_secret": secret,
		"default_template_id": tpl.ID,
	})
	ev := map[string]any{
		"organizationId": "00Dxx0000000002", "opportunityId": "006xx0000000099",
		"accountName": "Worker E2E Corp", "stageName": "Closed Won", "event": "opportunity.closed_won",
	}
	raw, _ := json.Marshal(ev)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	req, _ := jsonBodyRequest(t, h, "POST", "/v1/integrations/salesforce/webhook?ws=acme", raw)
	req.Header.Set("X-OpenLane-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	if code, body := doRequest(t, req); code != 200 {
		t.Fatalf("webhook = %d %s", code, body)
	}

	// run the real worker binary (app-role DSN); it processes until we
	// kill it — poll for effects instead of waiting on exit.
	app := appDSN(t)
	workerDir, _ := filepath.Abs("../../../worker/cmd/worker")
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = workerDir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+app)
	// new process group: `go run` spawns a child binary — killing the
	// parent alone orphans the worker (found the hard way). Kill the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()
	waitFor := func(q string) bool {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := ap.QueryRow(context.Background(), q).Scan(&n); err == nil && n == 1 {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}
	if !waitFor(`SELECT count(*) FROM projects WHERE name LIKE '%Worker E2E Corp%'`) {
		t.Fatalf("sf project not created; worker out: %s", buf.String())
	}
	// template branch: description copied + CRM origin recorded (§6.3)
	var desc string
	var refs string
	if err := ap.QueryRow(context.Background(),
		`SELECT COALESCE(description,''), external_refs::text FROM projects WHERE name LIKE '%Worker E2E Corp%'`).
		Scan(&desc, &refs); err != nil {
		t.Fatalf("project fetch: %v", err)
	}
	if desc != "sf flow" || !strings.Contains(refs, "006xx0000000099") {
		t.Fatalf("template branch broken: desc=%q refs=%s", desc, refs)
	}
	if !waitFor(`SELECT count(*) FROM audit_logs WHERE action='project.created_from_salesforce'`) {
		t.Fatalf("audit missing; worker out: %s", buf.String())
	}
	if !waitFor(`SELECT count(*) FROM river_job WHERE kind='slack_notify' AND state='completed'`) {
		t.Fatalf("slack job never completed; worker out: %s", buf.String())
	}
	if atomic.LoadInt32(&deliveries) != 1 {
		t.Fatalf("slack deliveries = %d; worker out: %s", deliveries, buf.String())
	}
}

// runWorkerOnce: boots the real worker binary, waits until no jobs are in
// available/retryable-with-due state, kills the process group. Shared by
// tests that need queued work actually processed.
func runWorkerOnce(t *testing.T) {
	t.Helper()
	app := appDSN(t)
	workerDir, _ := filepath.Abs("../../../worker/cmd/worker")
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = workerDir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+app)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // go run spawns a child; kill the group
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := ap.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE state IN ('available','retryable','running')`).Scan(&n); err == nil && n == 0 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("worker did not drain jobs; out: %s", buf.String())
}
