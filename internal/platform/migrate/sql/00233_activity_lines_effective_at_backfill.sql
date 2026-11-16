-- +goose Up
-- Fills effective_at on the rows already there: sim_effective_at, else
-- created_at (occurred_at for an attempt). The UPDATEs take row locks only;
-- the CHECKs come after them, so no update of an unfilled row is refused.
-- Operators: this rewrites every row of email_outbox once, in time proportional
-- to the emails ever queued, and the table grows until vacuum.
SET LOCAL app.bypass_rls = 'on';
UPDATE email_outbox SET effective_at = COALESCE(sim_effective_at, created_at) WHERE effective_at IS NULL;
UPDATE invoice_charge_attempts SET effective_at = COALESCE(sim_effective_at, occurred_at) WHERE effective_at IS NULL;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;

-- +goose Down
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_effective_at_not_null;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_effective_at_not_null;
