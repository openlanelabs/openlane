package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// Doc Agent v1 (P2 §15.1): draft SOW/BRD-style docs from supplied
// sources (transcript paste, notes, emails) + a template, with the
// citation contract enforced twice — prompt-side (every factual
// sentence ends with [source label]) and code-side (uncited paragraphs
// are flagged in the response, §15.1's 'uncited badge' — the draft
// saves but the human sees what wasn't grounded). PII is masked in
// sources before they reach the provider. Smart-model class.

type docDraftRequest struct {
	ProjectID  string        `json:"project_id"`
	Title      string        `json:"title"`
	TemplateMD string        `json:"template_md"`
	Sources    []docDraftSrc `json:"sources"`
}

type docDraftSrc struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

type uncitedSpan struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

var (
	piiEmailRe = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	piiPhoneRe = regexp.MustCompile(`\+?[0-9][0-9 ()\-.]{7,}[0-9]`)
	piiCardRe  = regexp.MustCompile(`\b(?:[0-9]\s?){13,19}\b`)
	// a sentence is cited when it ends with (or contains) [source …]
	citationRe = regexp.MustCompile(`\[source[^\]]*\]`)
)

// maskPII (§15.1): redact emails/phones/cards from source text before
// it leaves for the provider.
func maskPII(s string) string {
	s = piiCardRe.ReplaceAllString(s, "[card-redacted]")
	s = piiEmailRe.ReplaceAllString(s, "[email-redacted]")
	s = piiPhoneRe.ReplaceAllString(s, "[phone-redacted]")
	return s
}

// uncitedParagraphs: paragraphs (split on blank lines) whose body text
// carries no [source …] citation. Bullets count as sentences here —
// anything factual must cite or it gets flagged.
func uncitedParagraphs(md string) []uncitedSpan {
	out := []uncitedSpan{}
	for i, para := range strings.Split(md, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "---") {
			continue // headings/dividers exempt
		}
		if !citationRe.MatchString(p) {
			if len(p) > 200 {
				p = p[:200] + "…"
			}
			out = append(out, uncitedSpan{Index: i, Text: p})
		}
	}
	return out
}

const docAgentSystem = "You draft professional services documents (SOWs, BRDs, solution docs) " +
	"from supplied sources. RULES: every factual sentence or bullet MUST end with a citation " +
	"like [source <label>] naming which source supports it. Only use facts present in the " +
	"sources — if the sources lack something the template asks for, write 'TBD (not in sources)' " +
	"rather than inventing it. Output GitHub-flavored markdown only, following the template's structure."

func (s *Server) draftDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req docDraftRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 262144)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.ProjectID == "" || req.Title == "" || req.TemplateMD == "" {
		problem(w, http.StatusBadRequest, "project_id, title, and template_md are required")
		return
	}
	if len(req.Sources) == 0 {
		problem(w, http.StatusBadRequest, "at least one source is required")
		return
	}
	// cap sources v1: 10 sources, 8k chars each (chunk/map-reduce is the §15.1 edge, later)
	for _, src := range req.Sources {
		if src.Label == "" || len(src.Text) > 8192 {
			problem(w, http.StatusBadRequest, "each source needs a label; text capped at 8k chars v1")
			return
		}
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
	// project exists + belongs to this workspace (scoped RLS does both)
	var projName string
	if err := tx.QueryRow(ctx, `SELECT name FROM projects
		WHERE id = $1::uuid AND deleted_at IS NULL`, req.ProjectID).
		Scan(&projName); err != nil {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	// kill switch
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil || !enabled {
		problem(w, http.StatusServiceUnavailable, "agents disabled for this workspace (kill switch)")
		return
	}
	// LLM required — explicit for a generation task
	cfg, err := loadLLMConfig(ctx, tx)
	if err != nil {
		problem(w, http.StatusBadRequest, "no llm configured for this workspace — set one under Agents → BYO-LLM")
		return
	}

	// build the prompt: sources (PII-masked) + template
	var sb strings.Builder
	sb.WriteString("Project: " + projName + "\n\nSOURCES:\n")
	for _, src := range req.Sources {
		sb.WriteString("--- source " + src.Label + " ---\n" + maskPII(src.Text) + "\n\n")
	}
	sb.WriteString("TEMPLATE TO FOLLOW:\n" + req.TemplateMD)

	res, err := llmComplete(ctx, cfg, "smart", docAgentSystem, sb.String())
	if err != nil {
		problem(w, http.StatusBadGateway, "llm provider failed: "+err.Error())
		return
	}
	draft := strings.TrimSpace(res.Text)
	uncited := uncitedParagraphs(draft)

	// save draft doc + version 1 (customer_visible=false — a human publishes)
	var docID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO docs (workspace_id, project_id, title, customer_visible, latest_version, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1::uuid, $2, false, 1,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id`, req.ProjectID, req.Title).Scan(&docID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO doc_versions (workspace_id, doc_id, version, content_md, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, 1, $2,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)`,
		docID, draft); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// agent_runs: the draft IS the auditable artifact (model, cost, doc ref)
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'doc', 'succeeded', $1,
		        $2, 'doc:' || $3::text, $4, now())`,
		res.Model, req.Title, docID, llmCostCents(res)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id":     docID,
		"version":    1,
		"uncited":    uncited,
		"model":      res.Model,
		"cost_cents": llmCostCents(res),
	})
}
