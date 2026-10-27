-- +goose Up
-- A charge attempt Stripe refused because the amount is below its minimum
-- charge has its own outcome, so the timeline does not show it as a failed
-- payment. Re-added NOT VALID, so this only takes a short lock; 00198
-- checks the rows already there.
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_outcome_check;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_outcome_check
    CHECK (outcome IN ('pending', 'unknown', 'failed', 'succeeded', 'below_minimum')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local.
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_outcome_check;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_outcome_check
    CHECK (outcome IN ('pending', 'unknown', 'failed', 'succeeded')) NOT VALID;
