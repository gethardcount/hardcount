-- +goose Up
-- Refunds made in Stripe outside Hardcount (the Stripe dashboard, or Stripe's
-- API called directly) on a payment Hardcount collected (ADR-134). One row per
-- Stripe refund, holding the newest snapshot of it that a refund webhook
-- carried: last_event_at is that event's creation time, so events arriving
-- out of order never overwrite a newer one. Hardcount's own refunds are never
-- recorded here: they carry hardcount_cn_id metadata, or a credit note already
-- holds their id.
--
-- outcome is what Hardcount did with the refund:
--   awaiting:      first seen pending or requiring action; decided when a
--                  later event shows it succeeded, failed or was canceled;
--   credit_note:   recorded as an issued credit note whose card refund is
--                  this refund (credit_note_id names it);
--   other_payment: the refund is of a payment that did not pay the invoice
--                  (a second charge, or money on a voided invoice), so no
--                  credit note;
--   needs_review:  a refund of the invoice's own payment that Hardcount did
--                  not record (review_reason says why); the operator records
--                  it with a credit note or dismisses it;
--   not_refunded:  first seen already failed or canceled, so no money moved;
--   dismissed:     a needs_review refund the operator handled outside
--                  Hardcount.
CREATE TABLE invoice_stripe_refunds (
    id                       TEXT PRIMARY KEY DEFAULT ('hc_srf_' || encode(gen_random_bytes(12), 'hex')),
    tenant_id                TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode                 BOOLEAN NOT NULL DEFAULT true,
    invoice_id               TEXT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    stripe_refund_id         TEXT NOT NULL CHECK (stripe_refund_id <> ''),
    stripe_payment_intent_id TEXT NOT NULL CHECK (stripe_payment_intent_id <> ''),
    stripe_charge_id         TEXT NOT NULL DEFAULT '',
    amount_cents             BIGINT NOT NULL CHECK (amount_cents > 0),
    currency                 TEXT NOT NULL CHECK (currency <> '' AND currency = upper(currency)),
    -- domain.StripeRefundStatus: Stripe's five Refund.status values.
    status                   TEXT NOT NULL CHECK (status IN (
        'pending', 'requires_action', 'succeeded', 'failed', 'canceled')),
    reason                   TEXT NOT NULL DEFAULT '',
    failure_reason           TEXT NOT NULL DEFAULT '',
    outcome                  TEXT NOT NULL CHECK (outcome IN (
        'awaiting', 'credit_note', 'other_payment', 'needs_review', 'not_refunded', 'dismissed')),
    review_reason            TEXT NOT NULL DEFAULT '' CHECK (review_reason IN (
        '', 'invoice_not_paid', 'currency_mismatch', 'prepaid_commit', 'seen_late',
        'credit_note_refund_failed', 'exceeds_card_refundable', 'exceeds_creditable')),
    credit_note_id           TEXT REFERENCES credit_notes(id) ON DELETE RESTRICT,
    dismissed_note           TEXT NOT NULL DEFAULT '',
    dismissed_at             TIMESTAMPTZ,
    refund_created_at        TIMESTAMPTZ NOT NULL,
    -- The creation time of the first event Hardcount saw for the refund: the
    -- 24-hour rule compares it with refund_created_at, whenever the refund
    -- is decided.
    first_event_at           TIMESTAMPTZ NOT NULL,
    last_event_at            TIMESTAMPTZ NOT NULL,
    last_stripe_event_id     TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A credit note is named exactly when the refund was recorded as one.
    CONSTRAINT invoice_stripe_refunds_credit_note_matches_outcome CHECK (
        (credit_note_id IS NOT NULL) = (outcome = 'credit_note')),
    -- A review reason is given for a refund flagged for review, and kept when
    -- the operator dismisses it or records it later; the other outcomes
    -- carry none.
    CONSTRAINT invoice_stripe_refunds_review_reason_matches_outcome CHECK (
        (review_reason <> '') = (outcome IN ('needs_review', 'dismissed'))
        OR (outcome = 'credit_note')),
    -- A dismissal time is set exactly when the operator dismissed it.
    CONSTRAINT invoice_stripe_refunds_dismissed_at_matches_outcome CHECK (
        (dismissed_at IS NOT NULL) = (outcome = 'dismissed'))
);
CREATE UNIQUE INDEX invoice_stripe_refunds_stripe_refund ON invoice_stripe_refunds (tenant_id, stripe_refund_id);
CREATE INDEX invoice_stripe_refunds_invoice ON invoice_stripe_refunds (tenant_id, invoice_id);
CREATE TRIGGER set_livemode BEFORE INSERT ON invoice_stripe_refunds
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE invoice_stripe_refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_stripe_refunds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_stripe_refunds USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = CASE current_setting('app.livemode', true) WHEN 'on' THEN true WHEN 'off' THEN false END)));
GRANT SELECT, INSERT, UPDATE, DELETE ON invoice_stripe_refunds TO hardcount_app;

-- A credit note whose card refund is a refund made in Stripe: recorded from a
-- refund webhook, or linked by the operator at create. Hardcount made no
-- refund call for it.
ALTER TABLE credit_notes ADD COLUMN recorded_from_stripe BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE credit_notes DROP COLUMN recorded_from_stripe;
DROP TABLE invoice_stripe_refunds;
