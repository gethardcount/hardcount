-- +goose Up
-- Card disputes (chargebacks) that Stripe reports against an invoice's
-- payment (#979). One row per Stripe dispute, holding the newest snapshot of
-- it that a charge.dispute.* webhook carried: last_event_at is that event's
-- creation time, so events arriving out of order never overwrite a newer one.
-- Hardcount only records and reports a dispute. Nothing here changes an invoice's
-- payment status or moves money.
CREATE TABLE invoice_disputes (
    id                       TEXT PRIMARY KEY DEFAULT ('hc_dsp_' || encode(gen_random_bytes(12), 'hex')),
    tenant_id                TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode                 BOOLEAN NOT NULL DEFAULT true,
    invoice_id               TEXT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    stripe_dispute_id        TEXT NOT NULL CHECK (stripe_dispute_id <> ''),
    stripe_charge_id         TEXT NOT NULL DEFAULT '',
    stripe_payment_intent_id TEXT NOT NULL CHECK (stripe_payment_intent_id <> ''),
    -- domain.DisputeStatus: Stripe's eight Dispute.status values.
    status                   TEXT NOT NULL CHECK (status IN (
        'warning_needs_response', 'warning_under_review', 'warning_closed',
        'needs_response', 'under_review', 'won', 'lost', 'prevented')),
    reason                   TEXT NOT NULL DEFAULT '',
    amount_cents             BIGINT NOT NULL CHECK (amount_cents >= 0),
    currency                 TEXT NOT NULL CHECK (currency <> '' AND currency = upper(currency)),
    evidence_due_by          TIMESTAMPTZ,
    opened_at                TIMESTAMPTZ NOT NULL,
    closed_at                TIMESTAMPTZ,
    last_event_at            TIMESTAMPTZ NOT NULL,
    last_stripe_event_id     TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A dispute has a close time exactly when its status is a closed one.
    CONSTRAINT invoice_disputes_closed_at_matches_status CHECK (
        (closed_at IS NOT NULL) = (status IN ('warning_closed', 'won', 'lost', 'prevented')))
);
CREATE UNIQUE INDEX invoice_disputes_stripe_dispute ON invoice_disputes (tenant_id, stripe_dispute_id);
CREATE INDEX invoice_disputes_invoice ON invoice_disputes (tenant_id, invoice_id);
CREATE TRIGGER set_livemode BEFORE INSERT ON invoice_disputes
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE invoice_disputes ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_disputes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_disputes USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
GRANT SELECT, INSERT, UPDATE, DELETE ON invoice_disputes TO hardcount_app;

-- +goose Down
DROP TABLE invoice_disputes;
