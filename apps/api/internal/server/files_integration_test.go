//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// s3stub: in-memory VaultS3 stand-in. PUT stores, HEAD serves the stored
// Content-Length, GET echoes bytes — enough to exercise presigned flows.
type s3stub struct {
	mu      sync.Mutex
	objects map[string][]byte
	srv     *httptest.Server
}

func newS3Stub(t *testing.T) *s3stub {
	t.Helper()
	s := &s3stub{objects: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/openlane-files/")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			s.objects[key] = b
			w.WriteHeader(200)
		case http.MethodHead:
			if b, ok := s.objects[key]; ok {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
				w.WriteHeader(200)
			} else {
				w.WriteHeader(404)
			}
		case http.MethodGet:
			if b, ok := s.objects[key]; ok {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
				_, _ = w.Write(b)
			} else {
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func filesTestStack(t *testing.T) (*httptest.Server, *pgxpool.Pool, *httptestSrv) {
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
	pgMust(`INSERT INTO contacts (id, workspace_id, customer_id, email, display_name, is_portal_user)
		VALUES ($1, $2, $3, 'ravi@adobe.test', 'Ravi Kumar', true)`, contactID, wsA, custID)
	pgMust(`INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES ($1, $2, $3, 'Files Project', 'active')`, projA, wsA, custID)

	stub := newS3Stub(t)
	t.Setenv("OPENLANE_S3_ENDPOINT", stub.srv.URL)
	t.Setenv("OPENLANE_S3_KEY", "test-key")
	t.Setenv("OPENLANE_S3_SECRET", "test-secret")
	t.Setenv("OPENLANE_S3_BUCKET", "openlane-files")

	mux, pool, err := New(ctx, app, staffTok)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &httptestSrv{t: t, URL: srv.URL}
	return srv, pool, h
}

// doPortal: portal requests carry no staff auth; path like /p/{tok}/files
// maps to /v1/portal/{tok}/files.
func (h *httptestSrv) doPortal(method, path string) (int, []byte) {
	h.t.Helper()
	real := strings.Replace(path, "/p/", "/v1/portal/", 1)
	req, err := http.NewRequest(method, h.URL+real, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestProjectFilesCreateValidation(t *testing.T) {
	_, _, h := filesTestStack(t)

	code, body := h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
		"name": "kickoff.pdf", "content_type": "application/pdf", "size_bytes": 1024, "customer_visible": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var f fileOut
	json.Unmarshal(body, &f)
	if f.ID == "" || f.Status != "pending" || f.UploadURL == "" {
		t.Fatalf("file shape: %s", body)
	}
	if !verifyPresign(s3ConfigFromEnv(os.Getenv), "PUT", f.UploadURL) {
		t.Fatal("upload_url is not a valid presign")
	}

	// traversal name (spec edge: ../../../etc/passwd) — sanitized per spec
	// ('sanitize'), not rejected: stored name loses every path component.
	code, body = h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
		"name": "../../../etc/passwd", "content_type": "application/pdf", "size_bytes": 10,
	})
	if code != http.StatusCreated {
		t.Fatalf("traversal name = %d, want 201 (sanitized)", code)
	}
	var trav fileOut
	json.Unmarshal(body, &trav)
	if trav.Name != "passwd" || strings.Contains(trav.Name, "/") {
		t.Fatalf("stored name not sanitized: %q", trav.Name)
	}
	if strings.Contains(trav.UploadURL, "..") {
		t.Fatalf("traversal reached storage key: %q", trav.UploadURL)
	}
	// disallowed content type
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
		"name": "ok.pdf", "content_type": "application/x-msdownload", "size_bytes": 10,
	}); code != http.StatusBadRequest {
		t.Fatalf("bad type = %d, want 400", code)
	}
	// oversize
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
		"name": "ok.pdf", "content_type": "application/pdf", "size_bytes": 300 << 20,
	}); code != http.StatusBadRequest {
		t.Fatalf("oversize = %d, want 400", code)
	}
	// confirm before object exists → 409, file marked failed
	if code, _ := h.do("POST", "/v1/files/"+f.ID+"/uploaded", nil); code != http.StatusConflict {
		t.Fatalf("early uploaded = %d, want 409", code)
	}
}

func TestProjectFilesUploadListDeleteURL(t *testing.T) {
	_, _, h := filesTestStack(t)

	code, body := h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
		"name":         "spec.docx",
		"content_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"size_bytes":   14, "customer_visible": false,
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var f fileOut
	json.Unmarshal(body, &f)
	if f.UploadURL == "" {
		t.Fatal("no upload url")
	}
	put, _ := http.NewRequest(http.MethodPut, f.UploadURL, strings.NewReader("SPEC CONTENTS!"))
	pres, err := http.DefaultClient.Do(put)
	if err != nil || pres.StatusCode != 200 {
		t.Fatalf("client PUT failed: %v %d", err, pres.StatusCode)
	}
	if code, body := h.do("POST", "/v1/files/"+f.ID+"/uploaded", nil); code != http.StatusOK {
		t.Fatalf("uploaded = %d %s", code, body)
	}
	// double-confirm → 404
	if code, _ := h.do("POST", "/v1/files/"+f.ID+"/uploaded", nil); code != http.StatusNotFound {
		t.Fatalf("double uploaded = %d, want 404", code)
	}
	// list
	code, body = h.do("GET", "/v1/projects/"+projA+"/files", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "spec.docx") {
		t.Fatalf("list = %d %s", code, body)
	}
	// download URL round-trips the exact bytes
	code, body = h.do("GET", "/v1/files/"+f.ID+"/url", nil)
	if code != http.StatusOK {
		t.Fatalf("url = %d %s", code, body)
	}
	var dl struct {
		URL string `json:"url"`
	}
	json.Unmarshal(body, &dl)
	if dl.URL == "" || !verifyPresign(s3ConfigFromEnv(os.Getenv), "GET", dl.URL) {
		t.Fatalf("bad download url: %s", body)
	}
	gres, err := http.Get(dl.URL)
	if err != nil || gres.StatusCode != 200 {
		t.Fatalf("GET object failed: %v %d", err, gres.StatusCode)
	}
	gb, _ := io.ReadAll(gres.Body)
	if string(gb) != "SPEC CONTENTS!" {
		t.Fatalf("bytes mismatch: %q", gb)
	}
	// audit (read via ADMIN pool: the app role without ctx would be RLS-hidden —
	// the row exists precisely because the suite can't see it)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var audit int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='file.uploaded'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("audit file.uploaded = %d %v", audit, err)
	}
	// delete → gone
	if code, _ := h.do("DELETE", "/v1/files/"+f.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	code, body = h.do("GET", "/v1/projects/"+projA+"/files", nil)
	if code != http.StatusOK || strings.Contains(string(body), "spec.docx") {
		t.Fatalf("post-delete list = %d %s", code, body)
	}
	if code, _ := h.do("GET", "/v1/files/"+f.ID+"/url", nil); code != http.StatusNotFound {
		t.Fatalf("post-delete url = %d", code)
	}
	var delAudit int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='file.deleted'`).Scan(&delAudit); err != nil || delAudit != 1 {
		t.Fatalf("audit file.deleted = %d %v", delAudit, err)
	}
}

// Portal visibility: customer_visible files only; internal file 404s by id.
func TestPortalFilesVisibility(t *testing.T) {
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, encode(sha256('files-portal-token-1234567890-abcdefghijXYZ'::bytea),'hex'), now() + interval '7 days')`,
		wsA, projA, contactID); err != nil {
		t.Fatalf("link: %v", err)
	}

	mk := func(name string, visible bool) fileOut {
		code, body := h.do("POST", "/v1/projects/"+projA+"/files", map[string]any{
			"name": name, "content_type": "text/plain", "size_bytes": 3, "customer_visible": visible,
		})
		if code != http.StatusCreated {
			t.Fatalf("mk %s = %d %s", name, code, body)
		}
		var f fileOut
		json.Unmarshal(body, &f)
		put, _ := http.NewRequest(http.MethodPut, f.UploadURL, strings.NewReader("abc"))
		if pres, err := http.DefaultClient.Do(put); err != nil || pres.StatusCode != 200 {
			t.Fatalf("put %s: %v %d", name, err, pres.StatusCode)
		}
		if code, _ := h.do("POST", "/v1/files/"+f.ID+"/uploaded", nil); code != 200 {
			t.Fatalf("uploaded %s = %d", name, code)
		}
		return f
	}
	vis := mk("share.txt", true)
	internal := mk("secret.txt", false)

	code, body := h.doPortal("GET", "/p/files-portal-token-1234567890-abcdefghijXYZ/files")
	if code != 200 || !strings.Contains(string(body), "share.txt") || strings.Contains(string(body), "secret.txt") {
		t.Fatalf("portal list = %d %s", code, body)
	}
	code, body = h.doPortal("GET", "/p/files-portal-token-1234567890-abcdefghijXYZ/files/"+vis.ID+"/url")
	if code != 200 {
		t.Fatalf("portal visible url = %d %s", code, body)
	}
	if code, _ := h.doPortal("GET", "/p/files-portal-token-1234567890-abcdefghijXYZ/files/"+internal.ID+"/url"); code != 404 {
		t.Fatalf("portal internal url = %d, want 404", code)
	}
}
