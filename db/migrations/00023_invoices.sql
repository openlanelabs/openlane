-- +goose Up
-- Invoice drafts (P1 §319-§328): time → line items → draft invoice.
-- v1: JSONB line items (§ponytail — no normalized invoice_lines table until
-- QuickBooks export needs per-line mutation), statuses draft|sent|paid|void,
-- totals computed at generation and stored (audit-friendly snapshot).

CREATE TABLE invoices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id   uuid NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    status        text NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','sent','paid','void')),
    period_start  date NOT NULL,
    period_end    date NOT NULL,
    line_items    jsonb NOT NULL DEFAULT '[]'::jsonb,
    subtotal      numeric(12,2) NOT NULL DEFAULT 0,
    notes         text,
    created_by    uuid REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    CHECK (period_end >= period_start)
);
CREATE INDEX idx_invoices_workspace ON invoices (workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_invoices_customer ON invoices (customer_id);

COMMENT ON TABLE invoices IS 'Draft invoices generated from approved time; locked once sent.';

-- time_entries gains 'invoiced' status; entries move there on generation so
-- the same window can never be billed twice.
ALTER TABLE time_entries DROP CONSTRAINT time_entries_status_check;
ALTER TABLE time_entries ADD CONSTRAINT time_entries_status_check
    CHECK (status IN ('draft','submitted','approved','rejected','invoiced'));

-- tenant RLS mirrors projects (staff-only surface; no portal SELECT ever)
ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;

CREATE POLICY invoices_tenant ON invoices
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- +goose Down
-- invoiced rows would violate the narrowed CHECK below
UPDATE time_entries SET status = 'approved' WHERE status = 'invoiced';
DROP POLICY invoices_tenant ON invoices;
ALTER TABLE invoices NO FORCE ROW LEVEL SECURITY;
ALTER TABLE invoices DISABLE ROW LEVEL SECURITY;
DROP TABLE invoices;
ALTER TABLE time_entries DROP CONSTRAINT time_entries_status_check;
ALTER TABLE time_entries ADD CONSTRAINT time_entries_status_check
    CHECK (status IN ('draft','submitted','approved','rejected'));
