-- +goose Up
-- effective_at is the instant an email's or a charge attempt's action decided,
-- in both modes; the activity timelines sort on it. 00233 fills it and 00234
-- makes it NOT NULL. sim_effective_at no longer pairs with test_clock_id.
ALTER TABLE email_outbox ADD COLUMN effective_at TIMESTAMPTZ;
ALTER TABLE invoice_charge_attempts ADD COLUMN effective_at TIMESTAMPTZ;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_sim_anchor_pair_check;

-- +goose Down
-- Rollback runs only with APP_ENV=local. The previous version reads a test
-- clock's instant from sim_effective_at, which rows written since leave empty,
-- and its pair check would refuse their next update (the dispatcher's), so
-- those rows get it back first.
SET LOCAL app.bypass_rls = 'on';
UPDATE email_outbox SET sim_effective_at = effective_at
    WHERE test_clock_id IS NOT NULL AND sim_effective_at IS NULL;
UPDATE invoice_charge_attempts a SET sim_effective_at = a.effective_at
    FROM invoices i
    WHERE i.id = a.invoice_id AND i.is_simulated AND a.sim_effective_at IS NULL;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_sim_anchor_pair_check CHECK ((sim_effective_at IS NULL) = (test_clock_id IS NULL)) NOT VALID;
ALTER TABLE invoice_charge_attempts DROP COLUMN effective_at;
ALTER TABLE email_outbox DROP COLUMN effective_at;
