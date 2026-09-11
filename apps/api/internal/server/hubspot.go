package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// hubspot.go — HubSpot one-way (issue #66, spec §163/§335): closed-won
// Deal webhook → customer upsert + project from template. Mirrors the
// Salesforce shape (#55): HMAC raw-body verify, SECURITY DEFINER
// secret resolver by slug, dedupe + job enqueue in ONE tx.

type hsWebhookEvent struct {
	PortalID string `json:"portalId"`
	DealID   string `json:"dealId"`
	DealName string `json:"dealName"`
	// properties carry the closed-won signal
	Properties struct {
		DealStage string `json:"dealstage"`
		Amount    string `json:"amount"`
		CloseDate string `json:"closedate"`
		Company   string `json:"company"` // associated company name, if the flow sends it
	} `json:"properties"`
}

// readBody1MB reads the raw body (HMAC is over raw bytes). nil on
// oversize (the response is already written).
func readBody1MB(w http.ResponseWriter, r *http.Request) []byte {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	raw := make([]byte, 0, 4<<10)
	buf := make([]byte, 4<<10)
	for {
		n, err := body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			if err != io.EOF && len(raw) >= 1<<20 {
				problem(w, http.StatusRequestEntityTooLarge, "body too large")
				return nil
			}
			break
		}
	}
	return raw
}

func (s *Server) putHSSettings(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("hsWebhook: seal %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_integrations (workspace_id, provider, instance_url, webhook_secret_enc, default_template_id)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'hubspot', NULLIF($1,''), $2, NULLIF($3,'')::uuid)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET
		  instance_url = NULLIF($1,''), webhook_secret_enc = $2, default_template_id = NULLIF($3,'')::uuid, updated_at = now()`,
		req.InstanceURL, sealed, req.DefaultTemplateID); err != nil {
		log.Printf("hsWebhook: upsert %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getHSSettings(w http.ResponseWriter, r *http.Request) {
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
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND provider = 'hubspot'`).
		Scan(&instanceURL, &templateID, &updated, &enc)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
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

func (s *Server) hsWebhook(w http.ResponseWriter, r *http.Request) {
	raw := readBody1MB(w, r)
	if raw == nil {
		return
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-OpenLane-Signature"))
	if err != nil || len(sig) == 0 {
		problem(w, http.StatusUnauthorized, "missing or malformed X-OpenLane-Signature")
		return
	}
	var ev hsWebhookEvent
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
	var wsID string
	var enc []byte
	if err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, webhook_secret_enc
		FROM hs_webhook_secret($1)`, slug).Scan(&wsID, &enc); err != nil {
		problem(w, http.StatusNotFound, "not found") // unknown slug or unconfigured — no oracle
		return
	}
	aead, err := sfSecretAEAD()
	if err != nil {
		problem(w, http.StatusInternalServerError, "integration encryption unavailable")
		return
	}
	secret, err := openSecret(aead, enc)
	if err != nil {
		log.Printf("hsWebhook: open %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), sig) {
		problem(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// closed-won only: HubSpot default closed-won stage id for the
	// dealstage property. Accept the internal name too.
	if ev.Properties.DealStage != "closedwon" && ev.Properties.DealStage != "Closed Won" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", wsID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var externalID string
	err = tx.QueryRow(ctx, `
		INSERT INTO integration_sync_log (workspace_id, provider, external_id)
		VALUES ($1::uuid, 'hubspot', $2)
		ON CONFLICT (provider, external_id) DO NOTHING
		RETURNING external_id`, wsID, "hs:"+ev.PortalID+":"+ev.DealID).Scan(&externalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}
		log.Printf("hsWebhook: dedupe %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// company falls back to the deal name — HubSpot deals carry the
	// associated company via a separate event; P0 keeps one event.
	company := ev.Properties.Company
	if company == "" {
		company = ev.DealName
	}
	if _, err := s.river.InsertTx(ctx, tx, HSProjectCreateArgs{
		WorkspaceID: wsID,
		DealID:      ev.DealID,
		DealName:    ev.DealName,
		Company:     company,
		Amount:      ev.Properties.Amount,
		CloseDate:   ev.Properties.CloseDate,
	}, nil); err != nil {
		log.Printf("hsWebhook: enqueue %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}
