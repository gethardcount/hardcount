-- +goose Up
-- An email and a charge attempt carry effective_at: the instant their action
-- decided, in both modes. The invoice's activity card sorts on it, as it sorts
-- every other line, so an email or an attempt no longer sorts by the
-- database's clock beside lines stamped by the server's. 00233 makes it NOT
-- NULL. sim_effective_at is no longer written; a later file drops it.
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE email_outbox ADD COLUMN effective_at TIMESTAMPTZ;
ALTER TABLE invoice_charge_attempts ADD COLUMN effective_at TIMESTAMPTZ;

-- Old rows take a test clock's instant, else the time the row was written
-- (an email that was sent sorted on its send time before). One UPDATE per
-- table. email_outbox grows with
-- every email sent and can be the largest of the two; there is no production
-- install yet, so a single pass is enough (an install with millions of rows
-- would batch it by id).
UPDATE email_outbox SET effective_at = COALESCE(sim_effective_at, created_at);
UPDATE invoice_charge_attempts SET effective_at = COALESCE(sim_effective_at, occurred_at);

ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;

-- sim_effective_at is no longer written, so it no longer pairs with
-- test_clock_id, which still says the email's clock.
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_sim_anchor_pair_check;

-- +goose Down
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_sim_anchor_pair_check CHECK ((sim_effective_at IS NULL) = (test_clock_id IS NULL)) NOT VALID;
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_effective_at_not_null;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_effective_at_not_null;
ALTER TABLE invoice_charge_attempts DROP COLUMN effective_at;
ALTER TABLE email_outbox DROP COLUMN effective_at;
