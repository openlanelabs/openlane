package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Rate cards (P1 §320): per workspace/client/role, versioned by
// effective-dated cards, currency. Staff-only — money is never
// portal-visible (§207). Rates are immutable after create: a rate change
// is a NEW card (§326 — past hours stay locked to the card they were
// logged against).

type rateOut struct {
	ID            string     `json:"id"`
	CustomerID    *string    `json:"customer_id"`
	Name          string     `json:"name"`
	Currency      string     `json:"currency"`
	IsActive      bool       `json:"is_active"`
	EffectiveFrom string     `json:"effective_from"`
	Rates         []rateLine `json:"rates,omitempty"`
	CreatedAt     string     `json:"created_at"`
}

type rateLine struct {
	Role       string  `json:"role"`
	HourlyRate float64 `json:"hourly_rate"`
}

func (s *Server) createRateCard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CustomerID    string     `json:"customer_id"`
		Name          string     `json:"name"`
		Currency      string     `json:"currency"`
		EffectiveFrom string     `json:"effective_from"`
		Rates         []rateLine `json:"rates"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if len(req.Name) < 1 || len(req.Name) > 120 {
		problem(w, http.StatusBadRequest, "name must be 1-120 chars")
		return
	}
	if len(req.Rates) == 0 {
		problem(w, http.StatusBadRequest, "at least one rate is required")
		return
	}
	if len(req.Currency) != 0 && len(req.Currency) != 3 {
		problem(w, http.StatusBadRequest, "currency must be a 3-letter ISO code")
		return
	}
	for _, rate := range req.Rates {
		if len(rate.Role) < 1 || len(rate.Role) > 60 {
			problem(w, http.StatusBadRequest, "role must be 1-60 chars")
			return
		}
		if rate.HourlyRate <= 0 {
			problem(w, http.StatusBadRequest, "hourly_rate must be > 0 (§328)")
			return
		}
	}

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

	var card rateOut
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO rate_cards (workspace_id, customer_id, name, currency, effective_from, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid,
		        NULLIF($1, '')::uuid, $2, COALESCE(NULLIF($3, ''), 'USD'), COALESCE(NULLIF($4, ''), CURRENT_DATE::text)::date, NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id, customer_id::text, name, currency, is_active, effective_from::text, created_at`,
		req.CustomerID, req.Name, req.Currency, req.EffectiveFrom).Scan(
		&card.ID, &card.CustomerID, &card.Name, &card.Currency, &card.IsActive, &card.EffectiveFrom, &created)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			problem(w, http.StatusBadRequest, "customer not found in this workspace")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	card.CreatedAt = created.UTC().Format(time.RFC3339)

	card.Rates = make([]rateLine, 0, len(req.Rates))
	for _, rate := range req.Rates {
		if _, err := tx.Exec(ctx, `
			INSERT INTO rate_card_rates (workspace_id, rate_card_id, role, hourly_rate)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3)`,
			card.ID, rate.Role, rate.HourlyRate); err != nil {
			problem(w, http.StatusBadRequest, "duplicate role in rates")
			return
		}
		card.Rates = append(card.Rates, rate)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'rate_card', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'rate_card.created', 'api', $2)`,
		card.ID, `{"name":"`+card.Name+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	card.Rates = card.Rates[:len(card.Rates)]
	writeJSON(w, http.StatusCreated, card)
}

func (s *Server) listRateCards(w http.ResponseWriter, r *http.Request) {
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
	rows, err := tx.Query(ctx, `
		SELECT rc.id, rc.customer_id::text, rc.name, rc.currency, rc.is_active, rc.effective_from::text, rc.created_at,
		       COALESCE(json_agg(json_build_object('role', rcr.role, 'hourly_rate', rcr.hourly_rate::float8) ORDER BY rcr.role)
		         FILTER (WHERE rcr.id IS NOT NULL), '[]')
		FROM rate_cards rc
		LEFT JOIN rate_card_rates rcr ON rcr.rate_card_id = rc.id
		WHERE rc.deleted_at IS NULL
		GROUP BY rc.id
		ORDER BY rc.customer_id NULLS FIRST, rc.effective_from DESC, rc.created_at DESC`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	cards := []rateOut{}
	for rows.Next() {
		var c rateOut
		var created time.Time
		var ratesJSON []byte
		if err := rows.Scan(&c.ID, &c.CustomerID, &c.Name, &c.Currency, &c.IsActive, &c.EffectiveFrom, &created, &ratesJSON); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		c.CreatedAt = created.UTC().Format(time.RFC3339)
		_ = json.Unmarshal(ratesJSON, &c.Rates)
		cards = append(cards, c)
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, cards)
}

func (s *Server) getRateCard(w http.ResponseWriter, r *http.Request) {
	s.rateCardByID(w, r, r.PathValue("id"), true)
}

func (s *Server) rateCardByID(w http.ResponseWriter, r *http.Request, id string, withRates bool) {
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
	var c rateOut
	var created time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, customer_id::text, name, currency, is_active, effective_from::text, created_at
		FROM rate_cards WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&c.ID, &c.CustomerID, &c.Name, &c.Currency, &c.IsActive, &c.EffectiveFrom, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "rate card not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	c.CreatedAt = created.UTC().Format(time.RFC3339)
	if withRates {
		rows, err := tx.Query(ctx, `SELECT role, hourly_rate::float8 FROM rate_card_rates WHERE rate_card_id = $1 ORDER BY role`, id)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer rows.Close()
		c.Rates = []rateLine{}
		for rows.Next() {
			var l rateLine
			if err := rows.Scan(&l.Role, &l.HourlyRate); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			c.Rates = append(c.Rates, l)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) patchRateCard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IsActive *bool `json:"is_active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.IsActive == nil {
		problem(w, http.StatusBadRequest, "is_active is the only patchable field (rates are immutable — create a new card §326)")
		return
	}
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
	var c rateOut
	var created time.Time
	err = tx.QueryRow(ctx, `
		UPDATE rate_cards SET is_active = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING id, customer_id::text, name, currency, is_active, effective_from::text, created_at`,
		r.PathValue("id"), *req.IsActive).Scan(
		&c.ID, &c.CustomerID, &c.Name, &c.Currency, &c.IsActive, &c.EffectiveFrom, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "rate card not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	c.CreatedAt = created.UTC().Format(time.RFC3339)
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'rate_card', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'rate_card.updated', 'api', $2)`,
		c.ID, `{"is_active":`+strconv.FormatBool(*req.IsActive)+`}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteRateCard(w http.ResponseWriter, r *http.Request) {
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
	tag, err := tx.Exec(ctx, `
		UPDATE rate_cards SET deleted_at = now(), is_active = false, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "rate card not found")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'rate_card', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'rate_card.deleted', 'api')`,
		r.PathValue("id")); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
