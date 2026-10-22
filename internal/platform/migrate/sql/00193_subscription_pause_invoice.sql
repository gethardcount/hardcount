-- +goose Up
-- A collection pause that dunning set records the invoice whose dunning set
-- it. Paying that invoice lifts the pause (ADR-036 amendment 2026-10-22).
-- NULL is an operator's pause, set through the API or the dashboard, which
-- lifts only when the operator resumes it or at its resumes_at. Every pause
-- set before this migration was set under "until the operator resumes", so
-- the NULL every row starts with is right for it.
ALTER TABLE subscriptions ADD COLUMN pause_collection_invoice_id TEXT;

-- A writer that clears the pause and forgets the column fails. 00194 checks
-- the rows already there.
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_pause_collection_invoice_id_check
    CHECK (pause_collection_invoice_id IS NULL OR pause_collection_behavior IS NOT NULL) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_pause_collection_invoice_id_check;
ALTER TABLE subscriptions DROP COLUMN pause_collection_invoice_id;
