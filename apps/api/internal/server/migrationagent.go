package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Migration Agent v1 (P2 §15.2 — "our viral wedge"): CSV in → LLM
// suggests mapping + transforms in plain English → deterministic code
// validates + previews → manager approves → code runs the full
// transform, quarantining bad rows instead of failing. The LLM only
// ever *chooses* the mapping; execution is code — hallucinations
// can't corrupt data.

var migrationDestFields = map[string][]string{
	"salesforce_accounts": {"Name", "Phone", "Website", "Industry", "Description", "NumberOfEmployees", "BillingCity"},
	"hubspot_contacts":    {"firstname", "lastname", "email", "phone", "company", "jobtitle", "city"},
	"generic":             {"name", "email", "phone", "notes"},
}

// transforms the agent can pick (deterministic implementations below)
var migrationTransforms = map[string]bool{
	"phone_e164": true, "date_iso": true, "email_lower": true,
	"dedupe": true, "sanitize_formula": true,
}

type migrationSuggestReq struct {
	Name    string `json:"name"`
	Dest    string `json:"dest"`
	CSVText string `json:"csv_text"`
}

// what the LLM is asked to return
type llmMapping struct {
	Columns      map[string]string `json:"columns"` // csv header -> dest field
	Transforms   []string          `json:"transforms"`
	PlainEnglish string            `json:"plain_english"`
}

type migrationRowOut struct {
	Row    map[string]string `json:"row"`
	Errors []string          `json:"errors,omitempty"`
}

type migrationPreview struct {
	Headers []string          `json:"headers"`
	Rows    []migrationRowOut `json:"rows"`
}

type migrationQuarantined struct {
	Line    int      `json:"line"`
	Row     []string `json:"row"`
	Reasons []string `json:"reasons"`
}

func (s *Server) migrationSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req migrationSuggestReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5242880)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Name == "" {
		problem(w, http.StatusBadRequest, "name is required")
		return
	}
	destFields, ok := migrationDestFields[req.Dest]
	if !ok {
		problem(w, http.StatusBadRequest, "dest must be one of salesforce_accounts, hubspot_contacts, generic")
		return
	}
	headers, records, err := parseCSV(req.CSVText, 2000)
	if err != nil {
		problem(w, http.StatusBadRequest, "csv: "+err.Error())
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil || !enabled {
		problem(w, http.StatusServiceUnavailable, "agents disabled for this workspace (kill switch)")
		return
	}
	cfg, err := loadLLMConfig(ctx, tx)
	if err != nil {
		problem(w, http.StatusBadRequest, "no llm configured for this workspace — set one under Agents → BYO-LLM")
		return
	}

	// Ask the LLM for the mapping. We give it the dest schema + csv
	// headers + 5 sample rows; it picks columns + transforms and
	// explains in plain English.
	sys := "You are a data migration specialist. Given a destination schema's fields and a CSV's headers + sample rows, choose the best mapping (csv column -> destination field, use each destination field at most once, leave unmapped columns out) and pick transforms from exactly: phone_e164, date_iso, email_lower, dedupe, sanitize_formula. Respond as JSON only: {\"columns\": {\"<csv header>\": \"<dest field>\"}, \"transforms\": [\"...\"], \"plain_english\": \"2-3 sentences explaining the mapping and transforms\"}."
	var sb strings.Builder
	sb.WriteString("DESTINATION SCHEMA (" + req.Dest + ") fields: " + strings.Join(destFields, ", ") + "\n\n")
	sb.WriteString("CSV HEADERS: " + strings.Join(headers, ", ") + "\n\nSAMPLE ROWS:\n")
	for i, rec := range records {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&sb, "%v\n", rec)
	}
	res, err := llmComplete(ctx, cfg, "smart", sys, sb.String())
	if err != nil {
		problem(w, http.StatusBadGateway, "llm provider failed: "+err.Error())
		return
	}

	// Validate the LLM's mapping against reality — hallucinated dest
	// fields or unknown transforms are dropped and replaced by our own
	// best-guess (case-insensitive name match).
	var llm llmMapping
	var plainEnglish string
	if err := json.Unmarshal([]byte(stripCodeFence(res.Text)), &llm); err == nil && llm.Columns != nil {
		plainEnglish = llm.PlainEnglish
	}
	validFields := map[string]bool{}
	for _, f := range destFields {
		validFields[strings.ToLower(f)] = true
	}
	usedFields := map[string]bool{}
	columns := map[string]string{}
	byLower := map[string]string{}
	for _, h := range headers {
		byLower[strings.ToLower(strings.TrimSpace(h))] = h
	}
	for _, f := range destFields {
		fl := strings.ToLower(f)
		if h, ok := byLower[fl]; ok {
			columns[h] = f
			usedFields[fl] = true
			continue
		}
		// best-guess 2: header mentions the dest field (contact_phone → Phone)
		for h := range byLower {
			hl := strings.ToLower(h)
			if !usedFields[fl] && (strings.Contains(hl, fl) || (fl == "name" && strings.Contains(hl, "company"))) {
				columns[h] = f
				usedFields[fl] = true
				break
			}
		}
	}
	for csvCol, destField := range llm.Columns {
		dl := strings.ToLower(strings.TrimSpace(destField))
		if !validFields[dl] || usedFields[dl] {
			continue // hallucinated or duplicate — best-guess stands
		}
		if _, isHeader := byLower[strings.ToLower(strings.TrimSpace(csvCol))]; !isHeader {
			continue
		}
		columns[strings.TrimSpace(csvCol)] = destField
		usedFields[dl] = true
	}
	transforms := []string{"sanitize_formula"}
	for _, t := range llm.Transforms {
		t = strings.TrimSpace(t)
		if migrationTransforms[t] && !hasStr(transforms, t) {
			transforms = append(transforms, t)
		}
	}

	// Preview: transform the first 10 rows deterministically + errors
	preview := buildPreview(headers, records, columns, transforms, 10)

	var runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO migration_runs (workspace_id, name, dest, status, csv_text, mapping, plain_english, preview, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, 'draft', $3, $4, $5, $6,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id`,
		req.Name, req.Dest, req.CSVText,
		mustJSON(map[string]any{"columns": columns, "transforms": transforms}),
		plainEnglish, mustJSON(preview)).Scan(&runID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'migration', 'succeeded', $1,
		        $2, 'migration:' || $3::text, $4, now())`,
		res.Model, req.Name, runID, llmCostCents(res)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": runID, "name": req.Name, "dest": req.Dest, "status": "draft",
		"columns": columns, "transforms": transforms,
		"plain_english": plainEnglish, "preview": preview,
		"model": res.Model, "cost_cents": llmCostCents(res),
	})
}

// migrationApprove: draft → approved → run the FULL transform inline
// (v1). Deterministic code, no LLM: good rows to result, bad rows to
// quarantine with reasons (§15.2 partial-fail edge).
func (s *Server) migrationApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var dest, csvText string
	var mappingRaw, previewRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT dest, csv_text, mapping, preview FROM migration_runs
		WHERE id = $1::uuid AND status = 'draft'`,
		r.PathValue("id")).Scan(&dest, &csvText, &mappingRaw, &previewRaw); err != nil {
		problem(w, http.StatusNotFound, "migration not found or not in draft")
		return
	}
	var mapping struct {
		Columns    map[string]string `json:"columns"`
		Transforms []string          `json:"transforms"`
	}
	if err := json.Unmarshal(mappingRaw, &mapping); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = previewRaw

	headers, records, err := parseCSV(csvText, 2000)
	if err != nil {
		problem(w, http.StatusInternalServerError, "stored csv unreadable: "+err.Error())
		return
	}

	// build row map (csv header -> value), then map to dest fields
	result := []map[string]string{}
	quarantine := []migrationQuarantined{}
	seenKeys := map[string]bool{}
	dedupe := hasStr(mapping.Transforms, "dedupe")
	for i, rec := range records {
		src := map[string]string{}
		for j, h := range headers {
			if j < len(rec) {
				src[h] = rec[j]
			}
		}
		out := map[string]string{}
		var errs []string
		for csvCol, destField := range mapping.Columns {
			v := src[csvCol]
			phoneSafe := false
			switch {
			case hasStr(mapping.Transforms, "phone_e164") && isPhoneField(destField):
				var e string
				v, e = phoneE164(v)
				if e != "" {
					errs = append(errs, destField+": "+e)
				}
				phoneSafe = e == ""
			case hasStr(mapping.Transforms, "date_iso") && isDateField(destField):
				var e string
				v, e = dateISO(v)
				if e != "" {
					errs = append(errs, destField+": "+e)
				}
			case hasStr(mapping.Transforms, "email_lower") && isEmailField(destField):
				v = strings.ToLower(strings.TrimSpace(v))
			}
			// E.164 outputs are [+0-9] only — safe by construction;
			// everything else gets the §15.2 formula-injection guard.
			if hasStr(mapping.Transforms, "sanitize_formula") && !phoneSafe {
				v = sanitizeFormula(v)
			}
			out[destField] = v
		}
		key := dedupeKey(out)
		if dedupe && seenKeys[key] {
			errs = append(errs, "duplicate row (same mapped values as an earlier row)")
		}
		if len(errs) > 0 {
			quarantine = append(quarantine, migrationQuarantined{Line: i + 2, Row: rec, Reasons: errs})
			continue // good rows continue (§15.2)
		}
		seenKeys[key] = true
		result = append(result, out)
	}

	stats := map[string]any{"total": len(records), "ok": len(result), "quarantined": len(quarantine)}
	if _, err := tx.Exec(ctx, `
		UPDATE migration_runs SET status = 'done', result = $2, quarantine = $3, stats = $4, updated_at = now()
		WHERE id = $1::uuid AND status = 'draft'`,
		r.PathValue("id"), mustJSON(result), mustJSON(quarantine), mustJSON(stats)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'migration_run', $1::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'migration.approved', 'agent', $2)`,
		r.PathValue("id"), mustJSON(stats)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "status": "done", "stats": stats})
}

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT id, name, dest, status, stats, created_at FROM migration_runs
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, dest, status string
		var stats []byte
		var created time.Time
		if err := rows.Scan(&id, &name, &dest, &status, &stats, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "dest": dest,
			"status": status, "stats": json.RawMessage(stats), "created_at": created})
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var name, dest, status string
	var mappingRaw, plainEnglish, previewRaw, resultRaw, quarantineRaw, statsRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT name, dest, status, mapping, plain_english, preview, result, quarantine, stats
		FROM migration_runs WHERE id = $1::uuid AND
		workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`,
		r.PathValue("id")).
		Scan(&name, &dest, &status, &mappingRaw, &plainEnglish, &previewRaw, &resultRaw, &quarantineRaw, &statsRaw); err != nil {
		problem(w, http.StatusNotFound, "migration not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": r.PathValue("id"), "name": name, "dest": dest, "status": status,
		"mapping": json.RawMessage(mappingRaw), "plain_english": string(plainEnglish),
		"preview":    json.RawMessage(previewRaw),
		"result":     json.RawMessage(resultRaw),
		"quarantine": json.RawMessage(quarantineRaw),
		"stats":      json.RawMessage(statsRaw),
	})
	_ = tx.Commit(ctx)
}

func (s *Server) deleteMigration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM migration_runs WHERE id = $1::uuid AND
		workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`,
		r.PathValue("id"))
	if err != nil || tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "migration not found")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- deterministic helpers ----

func parseCSV(text string, maxRows int) ([]string, [][]string, error) {
	if !utf8.ValidString(text) {
		return nil, nil, fmt.Errorf("not valid utf-8 (encoding auto-detect is a later phase)")
	}
	rd := csv.NewReader(strings.NewReader(text))
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	header, err := rd.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("empty csv")
	}
	var records [][]string
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if len(records) >= maxRows {
			return nil, nil, fmt.Errorf("more than %d data rows (streaming is a later phase)", maxRows)
		}
		records = append(records, rec)
	}
	if len(header) == 0 || strings.TrimSpace(header[0]) == "" {
		return nil, nil, fmt.Errorf("missing header row")
	}
	return header, records, nil
}

// stripCodeFence: tolerate ```json fences from chatty models.
var codeFenceRe = regexp.MustCompile("(?s)^```[a-z]*\\s*|\\s*```$")

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	return codeFenceRe.ReplaceAllString(s, "")
}

var phoneDigitsRe = regexp.MustCompile(`[^0-9+]`)

// phoneE164: keep digits + one leading +, require ≥7 digits. US-style
// 10-digit numbers get no country guess (v1: pass through digits-only).
func phoneE164(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	plus := strings.HasPrefix(v, "+")
	digits := phoneDigitsRe.ReplaceAllString(v, "")
	if plus && !strings.HasPrefix(digits, "+") {
		digits = "+" + digits
	}
	n := strings.Count(digits, "+")
	if n > 1 || (n == 1 && !strings.HasPrefix(digits, "+")) {
		digits = strings.TrimLeft(digits, "+")
		digits = "+" + digits
	}
	if utf8.RuneCountInString(strings.TrimPrefix(digits, "+")) < 7 {
		return "", "invalid phone (fewer than 7 digits)"
	}
	if len(digits) > 17 {
		return "", "invalid phone (too long)"
	}
	return digits, ""
}

var dateFormats = []string{"2006-01-02", "01/02/2006", "1/2/2006", "2006/01/02", "02-Jan-2006", "Jan 2, 2006", "2 Jan 2006", "January 2, 2006"}

// dateISO: normalize common formats to YYYY-MM-DD.
func dateISO(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	for _, f := range dateFormats {
		if t, err := time.Parse(f, v); err == nil {
			return t.Format("2006-01-02"), ""
		}
	}
	return "", "unparseable date (expected YYYY-MM-DD, MM/DD/YYYY, or similar)"
}

// sanitizeFormula (§15.2 formula-injection edge): cells that could
// execute as a spreadsheet formula get a leading apostrophe.
func sanitizeFormula(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '=', '+', '-', '@':
		return "'" + v
	}
	return v
}

func isPhoneField(f string) bool {
	return strings.Contains(strings.ToLower(f), "phone")
}
func isDateField(f string) bool {
	l := strings.ToLower(f)
	return strings.Contains(l, "date") || l == "created" || l == "updated"
}
func isEmailField(f string) bool {
	return strings.Contains(strings.ToLower(f), "email")
}

func hasStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// dedupeKey: stable serialization of mapped values for duplicate
// detection. Keys are sorted so column order never matters.
func dedupeKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte(0)
		sb.WriteString(m[k])
		sb.WriteByte(0)
	}
	return sb.String()
}

// buildPreview: transform the first n rows + per-row validation errors
// — the §15.2 'preview + validation errors' contract.
func buildPreview(headers []string, records [][]string, columns map[string]string, transforms []string, n int) migrationPreview {
	preview := migrationPreview{Headers: []string{}}
	// dest-field order, sorted for stable output
	destCols := map[string]string{}
	for csvCol, destField := range columns {
		destCols[destField] = csvCol
	}
	sortedDest := make([]string, 0, len(destCols))
	for d := range destCols {
		sortedDest = append(sortedDest, d)
	}
	sort.Strings(sortedDest)
	preview.Headers = append(preview.Headers, sortedDest...)
	for i, rec := range records {
		if i >= n {
			break
		}
		src := map[string]string{}
		for j, h := range headers {
			if j < len(rec) {
				src[h] = rec[j]
			}
		}
		out := map[string]string{}
		var errs []string
		for csvCol, destField := range columns {
			v := src[csvCol]
			phoneSafe := false
			switch {
			case hasStr(transforms, "phone_e164") && isPhoneField(destField):
				var e string
				v, e = phoneE164(v)
				if e != "" {
					errs = append(errs, destField+": "+e)
				}
				phoneSafe = e == ""
			case hasStr(transforms, "date_iso") && isDateField(destField):
				var e string
				v, e = dateISO(v)
				if e != "" {
					errs = append(errs, destField+": "+e)
				}
			case hasStr(transforms, "email_lower") && isEmailField(destField):
				v = strings.ToLower(strings.TrimSpace(v))
			}
			if hasStr(transforms, "sanitize_formula") && !phoneSafe {
				v = sanitizeFormula(v)
			}
			out[destField] = v
		}
		preview.Rows = append(preview.Rows, migrationRowOut{Row: out, Errors: errs})
	}
	return preview
}
