package server

// Salesforce one-way integration (issue #54, spec §335/§350/§585):
// staff configure a workspace's webhook secret (encrypted at rest,
// AES-256-GCM); Salesforce's Outbound Message / webhook hits the
// unauthenticated receiver with HMAC-SHA256 over the raw body. A
// closed-won Opportunity enqueues sf_project_create in the SAME tx as
// the dedupe-log insert — duplicate webhook deliveries dedupe to one
// project, and a crashed API can't lose the create (outbox).
//
// One-way only in v1: OpenLane never writes to Salesforce.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
)

// sfSecretAEAD: derives the AES-GCM for webhook secrets from
// OPENLANE_INTEGRATION_KEY (32 bytes, base64). No KMS in P0 — the key
// comes from the operator's secret store (Doppler/vault/env, spec §420).
// ponytail: single env key; per-workspace data keys + KMS when a real
// key-management story lands (P1+).
func sfSecretAEAD() (cipher.AEAD, error) {
	raw := os.Getenv("OPENLANE_INTEGRATION_KEY")
	if raw == "" {
		return nil, errors.New("OPENLANE_INTEGRATION_KEY not set (32-byte base64)")
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

func sealSecret(aead cipher.AEAD, plain string) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func openSecret(aead cipher.AEAD, sealed []byte) (string, error) {
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("sealed secret too short")
	}
	ns := aead.NonceSize()
	plain, err := aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// putSFSettings: staff configures instance URL + webhook secret.
func (s *Server) putSFSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstanceURL       string `json:"instance_url"`
		WebhookSecret     string `json:"webhook_secret"`
		DefaultTemplateID string `json:"default_template_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.WebhookSecret == "" || len(req.WebhookSecret) < 16 {
		problem(w, http.StatusBadRequest, "webhook_secret required (min 16 chars)")
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	sealed, err := sealSecret(aead, req.WebhookSecret)
	if err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_integrations (workspace_id, instance_url, webhook_secret_enc, default_template_id)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, NULLIF($1,''), $2, NULLIF($3,'')::uuid)
		ON CONFLICT (workspace_id) DO UPDATE SET
		  instance_url = NULLIF($1,''), webhook_secret_enc = $2, default_template_id = NULLIF($3,'')::uuid, updated_at = now()`,
		req.InstanceURL, sealed, req.DefaultTemplateID); err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getSFSettings: never echoes the secret (only "configured: true").
func (s *Server) getSFSettings(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var (
		instanceURL *string
		templateID  *string
		updated     *string
		enc         []byte
	)
	err := tx.QueryRow(r.Context(), `
		SELECT instance_url, default_template_id::text, updated_at::text, webhook_secret_enc
		FROM workspace_integrations
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&instanceURL, &templateID, &updated, &enc)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := map[string]any{"configured": true}
	if instanceURL != nil {
		out["instance_url"] = *instanceURL
	}
	if templateID != nil {
		out["default_template_id"] = *templateID
	}
	if updated != nil {
		out["updated_at"] = *updated
	}
	writeJSON(w, http.StatusOK, out)
}

// sfWebhookEvent: the subset of Salesforce's Outbound Message we act on.
type sfWebhookEvent struct {
	OrganizationID string `json:"organizationId"`
	OpportunityID  string `json:"opportunityId"`
	AccountName    string `json:"accountName"`
	Amount         string `json:"amount"`
	CloseDate      string `json:"closeDate"`
	Stage          string `json:"stageName"`
	Event          string `json:"event"` // OpportunityStateChanged etc.
}

// sfWebhook: unauthenticated, HMAC-verified. Which workspace? The
// OrganizationID -> workspace mapping lives in instance data (P0: the
// request carries X-Workspace-Slug; orgId mapping is P1 with OAuth).
func (s *Server) sfWebhook(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	raw := make([]byte, 0, 4<<10)
	buf := make([]byte, 4<<10)
	for {
		n, err := body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-OpenLane-Signature"))
	if err != nil || len(sig) == 0 {
		problem(w, http.StatusUnauthorized, "missing or malformed X-OpenLane-Signature")
		return
	}
	var ev sfWebhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		problem(w, http.StatusBadRequest, "malformed event")
		return
	}
	slug := r.URL.Query().Get("ws")
	if slug == "" {
		problem(w, http.StatusBadRequest, "ws query parameter required")
		return
	}

	ctx := r.Context()
	// resolve workspace by slug + decrypt secret in one hop
	var (
		wsID string
		enc  []byte
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, webhook_secret_enc
		FROM sf_webhook_secret($1)`, slug).Scan(&wsID, &enc); err != nil {
		// 404 both for unknown slug and unconfigured workspace — no oracle
		problem(w, http.StatusNotFound, "not found")
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	secret, err := openSecret(aead, enc)
	if err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), sig) {
		problem(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// closed-won only: stageName is the single source of truth (SF
	// Outbound Messages carry it on every Opportunity update).
	if ev.Stage != "Closed Won" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	// dedupe + enqueue in ONE tx: a duplicate delivery hits the unique
	// (provider, external_id) and no-ops; the create job can't be lost.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The resolved workspace becomes the RLS scope — machine auth acting
	// on behalf of that tenant, AFTER its identity was verified by HMAC.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", wsID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var externalID string
	err = tx.QueryRow(ctx, `
		INSERT INTO integration_sync_log (workspace_id, provider, external_id)
		VALUES ($1::uuid, 'salesforce', $2)
		ON CONFLICT (provider, external_id) DO NOTHING
		RETURNING external_id`, wsID, "sfdc:"+ev.OrganizationID+":"+ev.OpportunityID).Scan(&externalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// duplicate delivery — already processed
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := s.river.InsertTx(ctx, tx, SFProjectCreateArgs{
		WorkspaceID:   wsID,
		OpportunityID: ev.OpportunityID,
		AccountName:   ev.AccountName,
		Amount:        ev.Amount,
		CloseDate:     ev.CloseDate,
	}, nil); err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("sfWebhook DEBUG: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}
