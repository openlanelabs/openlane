-- +goose Up
-- Rate cards (P1 §320): per workspace/client/role, versioned by
-- effective-dated cards, currency. Rates are immutable once the card
-- exists — a rate change is a NEW card (§326: past hours stay locked to
-- the card they were logged against; only future hours resolve to the
-- new one).

CREATE TABLE rate_cards (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id UUID REFERENCES customers(id) ON DELETE CASCADE, -- NULL = workspace default card
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    currency TEXT NOT NULL DEFAULT 'USD' CHECK (char_length(currency) = 3 AND currency = upper(currency)),
    is_active BOOLEAN NOT NULL DEFAULT true,
    effective_from DATE NOT NULL DEFAULT CURRENT_DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_rate_cards_ws_customer ON rate_cards(workspace_id, customer_id, is_active);

-- One rate per role per card. role is free-form text ("architect",
-- "pm", "developer") — P0 has no roles table; the card defines the
-- vocabulary. CHECK hourly_rate > 0 (no negative capacity §328).
CREATE TABLE rate_card_rates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    rate_card_id UUID NOT NULL REFERENCES rate_cards(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (char_length(role) BETWEEN 1 AND 60),
    hourly_rate NUMERIC(12,2) NOT NULL CHECK (hourly_rate > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (rate_card_id, role)
);
CREATE INDEX idx_rate_card_rates_card ON rate_card_rates(rate_card_id);

ALTER TABLE rate_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE rate_card_rates ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON rate_cards
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON rate_card_rates
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON rate_cards TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON rate_card_rates TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON rate_card_rates;
DROP POLICY IF EXISTS tenant_isolation ON rate_cards;
DROP TABLE IF EXISTS rate_card_rates;
DROP TABLE IF EXISTS rate_cards;
