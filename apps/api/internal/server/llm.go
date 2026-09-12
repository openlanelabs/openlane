package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// BYO-LLM router (P2 §15, §373): provider config per workspace, model
// picked by task class ('cheap' for nudges/summaries, 'smart' for
// migration/doc drafts), token usage metered to agent_runs.cost_cents
// by callers. Azure rides the openai driver via base_url; ollama keeps
// data in the VPC (§510-2). Keys sealed AES-256-GCM (jira/salesforce
// envelope). ErrNoLLM = zero-config degrade signal.

var ErrNoLLM = errors.New("no llm config for workspace")

type llmConfig struct {
	Provider   string
	BaseURL    string
	CheapModel string
	SmartModel string
	APIKey     string // opened, never serialized
}

type llmResult struct {
	Text      string
	Model     string
	InTokens  int
	OutTokens int
	Provider  string
}

// llmHostAllowed: explicit ops allowlist (SSRF hygiene — the calendar
// convention, applied in-process because narration is sync). Default
// covers the three providers + local ollama.
func llmHostAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	allowed := os.Getenv("OPENLANE_LLM_ALLOWED_HOSTS")
	if allowed == "" {
		allowed = "api.openai.com,api.anthropic.com,localhost,127.0.0.1,host.docker.internal"
	}
	for _, a := range strings.Split(allowed, ",") {
		if strings.TrimSpace(a) == host {
			return true
		}
	}
	return false
}

func llmHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext:     (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}}
}

// loadLLMConfig: workspace-scoped read + key unseal. q is the caller's
// tx (set_config is tx-scoped — house rule).
func loadLLMConfig(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) (*llmConfig, error) {
	cfg := &llmConfig{}
	var keyB64 string
	err := q.QueryRow(ctx,
		`SELECT provider, base_url, cheap_model, smart_model, COALESCE(api_key_sealed->>'key_enc','')
		 FROM llm_configs
		 WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&cfg.Provider, &cfg.BaseURL, &cfg.CheapModel, &cfg.SmartModel, &keyB64)
	if err != nil {
		return nil, ErrNoLLM
	}
	// key stored base64(sealed) in jsonb — the jira pat_enc convention.
	if keyB64 != "" {
		if sealed, e1 := base64.StdEncoding.DecodeString(keyB64); e1 == nil {
			if aead, e2 := sfSecretAEAD(); e2 == nil {
				if pt, e3 := openSecret(aead, sealed); e3 == nil {
					cfg.APIKey = pt
				}
			}
		}
	}
	return cfg, nil
}

// llmComplete: route by task class, call provider, return usage.
// task: "cheap" | "smart". Zero writes here — callers own agent_runs.
func llmComplete(ctx context.Context, cfg *llmConfig, task, system, prompt string) (*llmResult, error) {
	model := cfg.CheapModel
	if task == "smart" {
		model = cfg.SmartModel
	}
	if !llmHostAllowed(cfg.BaseURL) {
		return nil, fmt.Errorf("llm base_url host not allowed: %s", cfg.BaseURL)
	}
	switch cfg.Provider {
	case "openai":
		return openaiComplete(ctx, cfg, model, system, prompt)
	case "anthropic":
		return anthropicComplete(ctx, cfg, model, system, prompt)
	case "ollama":
		return ollamaComplete(ctx, cfg, model, system, prompt)
	}
	return nil, fmt.Errorf("unknown llm provider %q", cfg.Provider)
}

func llmPost(ctx context.Context, cfg *llmConfig, path string, hdr map[string]string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(cfg.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := llmHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("llm provider %s: status %d: %s", cfg.Provider, resp.StatusCode, string(b))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// OpenAI + Azure-compatible chat completions.
func openaiComplete(ctx context.Context, cfg *llmConfig, model, system, prompt string) (*llmResult, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("openai provider requires api_key")
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := llmPost(ctx, cfg, "/chat/completions", map[string]string{
		"authorization": "Bearer " + cfg.APIKey,
	}, map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("llm: empty choices")
	}
	return &llmResult{Text: out.Choices[0].Message.Content, Model: model,
		InTokens: out.Usage.PromptTokens, OutTokens: out.Usage.CompletionTokens, Provider: "openai"}, nil
}

// Anthropic messages API.
func anthropicComplete(ctx context.Context, cfg *llmConfig, model, system, prompt string) (*llmResult, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic provider requires api_key")
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := llmPost(ctx, cfg, "/v1/messages", map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}, map[string]any{
		"model":      model,
		"max_tokens": 2048,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Content) == 0 || out.Content[0].Text == "" {
		return nil, errors.New("llm: empty content")
	}
	return &llmResult{Text: out.Content[0].Text, Model: model,
		InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens, Provider: "anthropic"}, nil
}

// Ollama /api/chat — local VPC mode, cost 0.
func ollamaComplete(ctx context.Context, cfg *llmConfig, model, system, prompt string) (*llmResult, error) {
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := llmPost(ctx, cfg, "/api/chat", nil, map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
		"stream": false,
	}, &out); err != nil {
		return nil, err
	}
	if out.Message.Content == "" {
		return nil, errors.New("llm: empty ollama content")
	}
	return &llmResult{Text: out.Message.Content, Model: model, Provider: "ollama"}, nil
}

// pricePerMtok: cents per 1M tokens (in, out) — env-overridable defaults.
// ollama = 0 by construction (no meter). Rough v1; refine per model later.
func llmCostCents(r *llmResult) int {
	key := strings.ToUpper(r.Provider) + "_PER_MTOK_CENTS"
	// defaults: openai ~150/600, anthropic ~300/1500 (cents per Mtok)
	inPer, outPer := 150, 600
	switch r.Provider {
	case "anthropic":
		inPer, outPer = 300, 1500
	case "ollama":
		inPer, outPer = 0, 0
	}
	if v := os.Getenv(key); v != "" {
		if n, err := fmt.Sscanf(v, "%d,%d", &inPer, &outPer); err == nil && n == 2 {
		}
	}
	cents := (r.InTokens*inPer + r.OutTokens*outPer) / 1_000_000
	if cents == 0 && (r.InTokens > 0 || r.OutTokens > 0) && (inPer > 0 || outPer > 0) {
		cents = 1 // any real usage meters ≥1¢ (sub-cent rounding floor)
	}
	return cents
}
