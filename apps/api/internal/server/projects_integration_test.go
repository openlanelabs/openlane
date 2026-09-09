//go:build integration

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func projTestStack(t *testing.T) *httptestSrv {
	t.Helper()
	srv, _, adminPool, ctx := importTestStack(t) // reuse: resets DB, seeds ws+customer+project
	_ = adminPool
	_ = ctx
	return &httptestSrv{t: t, URL: srv.URL}
}

type httptestSrv struct {
	t   *testing.T
	URL string
}

func (h *httptestSrv) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+staffTok)
	req.Header.Set("X-Workspace-Id", wsA)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestProjectsCRUDFlow(t *testing.T) {
	h := projTestStack(t)

	// create: name too short
	if code, _ := h.do("POST", "/v1/projects", map[string]any{"customer_id": custID, "name": "ab"}); code != 400 {
		t.Fatalf("short name = %d, want 400", code)
	}
	// create ok (draft)
	code, body := h.do("POST", "/v1/projects", map[string]any{"customer_id": custID, "name": "New Delivery"})
	if code != 201 {
		t.Fatalf("create = %d: %s", code, body)
	}
	var p struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(body, &p)
	if p.Status != "draft" {
		t.Fatalf("new project status = %q", p.Status)
	}

	// draft→active without start_date = 400
	if code, _ := h.do("PATCH", "/v1/projects/"+p.ID, map[string]any{"status": "active"}); code != 400 {
		t.Fatalf("draft→active without start = %d, want 400", code)
	}
	// draft→active with start but 0 tasks = 400
	if code, _ := h.do("PATCH", "/v1/projects/"+p.ID, map[string]any{"status": "active", "start_date": "2026-09-10"}); code != 400 {
		t.Fatalf("draft→active with 0 tasks = %d, want 400", code)
	}
	// add a task via importer (generic), then activate
	_, b2 := h.doMultipartImport(t, "generic", "title,status\nSet up tenant,todo\n", map[string]string{"project_id": p.ID})
	_ = b2
	if code, _ := h.do("PATCH", "/v1/projects/"+p.ID, map[string]any{"status": "active", "start_date": "2026-09-10"}); code != 200 {
		t.Fatalf("draft→active with task = %d, want 200", code)
	}
	// active→completed with open required task = 400
	if code, _ := h.do("PATCH", "/v1/projects/"+p.ID, map[string]any{"status": "completed"}); code != 400 {
		t.Fatalf("active→completed with open required = %d, want 400", code)
	}
	// detail shows progress
	code, body = h.do("GET", "/v1/projects/"+p.ID, nil)
	if code != 200 {
		t.Fatalf("get = %d", code)
	}
	var d struct {
		ProgressPct int `json:"progress_pct"`
	}
	json.Unmarshal(body, &d)
	if d.ProgressPct != 0 {
		t.Fatalf("progress = %d", d.ProgressPct)
	}
	// list filter
	code, body = h.do("GET", "/v1/projects?status=draft", nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	var list []map[string]any
	json.Unmarshal(body, &list)
	if len(list) != 0 {
		t.Fatalf("draft filter = %d rows, want 0 (ours is active)", len(list))
	}
	// soft delete → 404 on get
	if code, _ := h.do("DELETE", "/v1/projects/"+p.ID, nil); code != 204 {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _ := h.do("GET", "/v1/projects/"+p.ID, nil); code != 404 {
		t.Fatalf("get after archive = %d, want 404", code)
	}
	// foreign workspace 404
	if code, _ := h.do("GET", "/v1/projects/99999999-9999-9999-9999-999999999999", nil); code != 404 {
		t.Fatalf("foreign get = %d, want 404", code)
	}
}

func TestTemplatesAndFromTemplate(t *testing.T) {
	h := projTestStack(t)

	tpl := map[string]any{
		"name":     "Enterprise Onboarding",
		"category": "onboarding",
		"phases": []map[string]any{
			{"name": "Kickoff", "tasks": []map[string]any{
				{"title": "Sign SOW", "due_offset_days": 2, "required": true, "customer_visible": true},
			}},
			{"name": "Go-live", "tasks": []map[string]any{
				{"title": "DNS cutover", "due_offset_days": 9, "required": true, "customer_visible": true},
				{"title": "Internal retro", "due_offset_days": 10, "required": false, "customer_visible": false},
			}},
		},
	}
	code, body := h.do("POST", "/v1/templates", tpl)
	if code != 201 {
		t.Fatalf("create template = %d: %s", code, body)
	}
	var tOut struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	json.Unmarshal(body, &tOut)
	if tOut.Version != "1.0.0" {
		t.Fatalf("version = %q", tOut.Version)
	}
	// bad category
	if code, _ := h.do("POST", "/v1/templates", map[string]any{"name": "x", "category": "wat", "phases": []map[string]any{}}); code != 400 {
		t.Fatalf("bad category = %d", code)
	}

	// from-template
	code, body = h.do("POST", "/v1/projects/from-template", map[string]any{
		"template_id": tOut.ID, "customer_id": custID, "name": "Templated Delivery", "start_date": "2026-09-10",
	})
	if code != 201 {
		t.Fatalf("from-template = %d: %s", code, body)
	}
	var p struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(body, &p)
	if p.Status != "active" {
		t.Fatalf("from-template status = %q (want active)", p.Status)
	}
	// tasks created with offsets: verify via importer-less path — use the API? No list-tasks route at P0.
	// Verify via dry-run import? No — verify via admin pool through a second from-template call is overkill;
	// the portal suite covers tasks RLS. Here verify template usage_count bump + new-version:
	code, body = h.do("GET", "/v1/templates/"+tOut.ID, nil)
	var d struct {
		UsageCount int  `json:"usage_count"`
		IsActive   bool `json:"is_active"`
	}
	json.Unmarshal(body, &d)
	if code != 200 || d.UsageCount != 1 || !d.IsActive {
		t.Fatalf("usage_count/active: %d %+v", code, d)
	}
	// new version: old inactive, new active, versions bump
	code, body = h.do("POST", "/v1/templates/"+tOut.ID+"/new-version", nil)
	if code != 201 {
		t.Fatalf("new-version = %d", code)
	}
	var nv struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		IsActive bool   `json:"is_active"`
	}
	json.Unmarshal(body, &nv)
	if nv.Version != "1.0.1" || !nv.IsActive {
		t.Fatalf("new version = %+v", nv)
	}
	code, body = h.do("GET", "/v1/templates/"+tOut.ID, nil)
	json.Unmarshal(body, &d)
	if d.IsActive {
		t.Fatalf("old template still active after new-version")
	}
	// foreign workspace template 404
	if code, _ := h.do("GET", "/v1/templates/99999999-9999-9999-9999-999999999999", nil); code != 404 {
		t.Fatalf("foreign template = %d, want 404", code)
	}
	_ = fmt.Sprintf
}

// doMultipartImport: reuse importMultipart but against this server.
func (h *httptestSrv) doMultipartImport(t *testing.T, source, csv string, fields map[string]string) (int, []byte) {
	t.Helper()
	req, err := importMultipart(t, source, csv, fields)
	if err != nil {
		t.Fatal(err)
	}
	req.URL, _ = req.URL.Parse(h.URL + "/v1/imports")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}
