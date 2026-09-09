//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const asanaSample = `Task ID,Creation Date,Completion Date,Last Modified Date,Name,Assignee,Due Date,Tags,Notes,Project Name,Parent Task
1208995,2026-08-01,,2026-08-02,Import task one,ravi@adobe.test,09/12/2026,kickoff,First row,Imported Project,
1208996,2026-08-01,2026-08-05,2026-08-05,Import task done,asha@acme.test,08/05/2026,legal,Second row,Imported Project,
,,,,,,13/45/2026,,,,
`

func importTestStack(t *testing.T) (*httptest.Server, *pgxpool.Pool, *pgxpool.Pool, context.Context) {
	t.Helper()
	admin, app := adminDSN(t), appDSN(t)
	ctx := context.Background()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("go", "run", "github.com/pressly/goose/v3/cmd/goose@v3.22.0", "-dir", "../../../../db/migrations", "postgres", admin, "reset")
	run("go", "run", "github.com/pressly/goose/v3/cmd/goose@v3.22.0", "-dir", "../../../../db/migrations", "postgres", admin, "up")
	adminPool, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminPool.Close() })
	pgMust := func(q string, args ...any) {
		if _, err := adminPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	pgMust(`ALTER ROLE openlane_app WITH PASSWORD 'openlane_app'`)
	pgMust(`INSERT INTO workspaces (id, name, slug) VALUES ($1,'Acme SI','acme')`, wsA)
	pgMust(`INSERT INTO customers (id, workspace_id, name) VALUES ($1, $2, 'Adobe')`, custID, wsA)
	pgMust(`INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES ($1, $2, $3, 'Imported Project', 'active')`, projA, wsA, custID)

	mux, pool, err := New(ctx, app, staffTok)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, pool, adminPool, ctx
}

func importMultipart(t *testing.T, source, csv string, fields map[string]string) (*http.Request, error) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, err := mw.CreateFormFile("file", "export.csv")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write([]byte(csv)); err != nil {
		return nil, err
	}
	_ = mw.WriteField("source", source)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "http://test/v1/imports", &b)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+staffTok)
	req.Header.Set("X-Workspace-Id", wsA)
	return req, nil
}

func TestImportDryRunThenCommit(t *testing.T) {
	srv, _, adminPool, ctx := importTestStack(t)
	client := &http.Client{}

	post := func(query string) *http.Response {
		req, err := importMultipart(t, "asana", asanaSample, map[string]string{"project_id": projA})
		if err != nil {
			t.Fatal(err)
		}
		req.URL, _ = req.URL.Parse(srv.URL + "/v1/imports" + query)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// ---- dry run: nothing written, full verdict ----
	res := post("?dry_run=true")
	if res.StatusCode != 200 {
		var buf bytes.Buffer
		buf.ReadFrom(res.Body)
		t.Fatalf("dry run = %d: %s", res.StatusCode, buf.String())
	}
	var dr struct {
		DryRun  bool `json:"dry_run"`
		OKCount int  `json:"ok_count"`
		Errors  []struct {
			SourceRow int    `json:"source_row"`
			Reason    string `json:"reason"`
		} `json:"errors"`
	}
	json.NewDecoder(res.Body).Decode(&dr)
	res.Body.Close()
	if !dr.DryRun || dr.OKCount != 2 || len(dr.Errors) != 1 {
		t.Fatalf("dry run = %+v (want ok=2, 1 quarantine: title empties to formula-junk + bad date)", dr)
	}
	if dr.Errors[0].SourceRow != 4 {
		t.Errorf("source_row = %d, want 4", dr.Errors[0].SourceRow)
	}
	var n int
	adminPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title LIKE 'Import task%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("dry run wrote %d rows!", n)
	}

	// ---- commit: 2 written, 1 quarantined, audit row ----
	res2 := post("")
	if res2.StatusCode != 200 {
		t.Fatalf("import = %d", res2.StatusCode)
	}
	var ir struct {
		ProjectID string            `json:"project_id"`
		OKCount   int               `json:"ok_count"`
		Errors    []json.RawMessage `json:"errors"`
	}
	json.NewDecoder(res2.Body).Decode(&ir)
	res2.Body.Close()
	if ir.OKCount != 2 || len(ir.Errors) != 1 || ir.ProjectID != projA {
		t.Fatalf("import = %+v", ir)
	}
	adminPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title LIKE 'Import task%'`).Scan(&n)
	if n != 2 {
		t.Fatalf("committed rows = %d, want 2", n)
	}
	adminPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title = 'Import task done' AND status = 'done'`).Scan(&n)
	if n != 1 {
		t.Fatalf("completed Asana task not imported done: %d", n)
	}
	adminPool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'import.completed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

func TestImportCreateProject(t *testing.T) {
	srv, _, adminPool, ctx := importTestStack(t)
	client := &http.Client{}
	req, err := importMultipart(t, "generic", "title,due_at,status\nFrom scratch,2026-10-01,todo\n", map[string]string{
		"create_project": `{"customer_id":"` + custID + `","name":"Fresh Migrated Project"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	req.URL, _ = req.URL.Parse(srv.URL + "/v1/imports")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ir struct {
		ProjectID string `json:"project_id"`
		OKCount   int    `json:"ok_count"`
	}
	json.NewDecoder(res.Body).Decode(&ir)
	res.Body.Close()
	if res.StatusCode != 200 || ir.OKCount != 1 || ir.ProjectID == "" {
		t.Fatalf("create-project import: %d %+v", res.StatusCode, ir)
	}
	var n int
	adminPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title = 'From scratch'`).Scan(&n)
	if n != 1 {
		t.Fatalf("task in created project: %d", n)
	}
	_ = os.Environ
	_ = strings.TrimSpace
}

func TestImportForeignProject404(t *testing.T) {
	srv, _, _, _ := importTestStack(t)
	client := &http.Client{}
	req, err := importMultipart(t, "generic", "title\nEvil\n", map[string]string{"project_id": "99999999-9999-9999-9999-999999999999"})
	if err != nil {
		t.Fatal(err)
	}
	req.URL, _ = req.URL.Parse(srv.URL + "/v1/imports?dry_run=true")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("foreign project = %d, want 404 (RLS, no oracle)", res.StatusCode)
	}
}
