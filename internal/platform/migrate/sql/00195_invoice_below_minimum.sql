-- +goose Up
-- When Stripe last refused to charge the invoice because its amount is below
-- Stripe's minimum charge (amount_too_small). NULL on every existing row:
-- before this, such a refusal was recorded as a failed payment.
ALTER TABLE invoices ADD COLUMN below_minimum_at TIMESTAMPTZ;

-- A payment recovery that ended on that refusal records it as its
-- resolution. 00196 checks the rows already there.
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'manually_resolved', 'amount_below_minimum')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local.
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'manually_resolved')) NOT VALID;
ALTER TABLE invoices DROP COLUMN below_minimum_at;
