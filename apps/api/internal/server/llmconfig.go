package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// LLM config CRUD (P2 §15, §373). PUT admin-only: seals the api key
// (jira pat_enc envelope). GET returns config WITHOUT the key —
// presence + model names + provider, so the UI can say "narration on".

type llmConfigRequest struct {
	Provider   string `json:"provider"`
	BaseURL    string `json:"base_url"`
	CheapModel string `json:"cheap_model"`
	SmartModel string `json:"smart_model"`
	APIKey     string `json:"api_key"` // write-only; empty = keep existing
}

func validLLMBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return strings.Contains(h, ".") || h == "localhost" || h == "127.0.0.1" || h == "host.docker.internal"
}

func (s *Server) putLLMConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req llmConfigRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
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
	if !isAdminRole(ctx, tx) {
		problem(w, http.StatusForbidden, "admin role required")
		return
	}
	switch req.Provider {
	case "openai", "anthropic", "ollama":
	default:
		problem(w, http.StatusBadRequest, "provider must be openai, anthropic, or ollama")
		return
	}
	if !validLLMBaseURL(req.BaseURL) {
		problem(w, http.StatusBadRequest, "base_url must be an absolute http(s) URL")
		return
	}
	if req.CheapModel == "" || req.SmartModel == "" {
		problem(w, http.StatusBadRequest, "cheap_model and smart_model are required")
		return
	}
	if (req.Provider == "openai" || req.Provider == "anthropic") && req.APIKey == "" {
		problem(w, http.StatusBadRequest, "api_key required for openai/anthropic")
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	keyEnc := ""
	if req.APIKey != "" {
		sealed, err := sealSecret(aead, req.APIKey)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		keyEnc = base64.StdEncoding.EncodeToString(sealed)
	}
	// empty api_key + existing row = keep old key (key kept via COALESCE)
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_configs (workspace_id, provider, base_url, cheap_model, smart_model, api_key_sealed, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3, $4,
		        CASE WHEN $5::text <> '' THEN jsonb_build_object('key_enc', $5::text) ELSE '{}'::jsonb END,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		ON CONFLICT (workspace_id) DO UPDATE SET
		    provider = EXCLUDED.provider, base_url = EXCLUDED.base_url,
		    cheap_model = EXCLUDED.cheap_model, smart_model = EXCLUDED.smart_model,
		    api_key_sealed = CASE WHEN EXCLUDED.api_key_sealed <> '{}'::jsonb THEN EXCLUDED.api_key_sealed
		                          ELSE llm_configs.api_key_sealed END,
		    updated_at = now()`,
		req.Provider, req.BaseURL, req.CheapModel, req.SmartModel, keyEnc); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'integration',
		        NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'llm.config_saved', 'api')`,
	); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": req.Provider, "base_url": req.BaseURL,
		"cheap_model": req.CheapModel, "smart_model": req.SmartModel,
	})
}

func (s *Server) getLLMConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	cfg, err := loadLLMConfig(ctx, tx)
	if err != nil {
		if err == ErrNoLLM {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":  true,
		"provider":    cfg.Provider,
		"base_url":    cfg.BaseURL,
		"cheap_model": cfg.CheapModel,
		"smart_model": cfg.SmartModel,
	})
}
