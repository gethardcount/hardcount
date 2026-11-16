-- +goose Up
-- Checks the rows already there against the CHECKs 00233 added NOT VALID,
-- then makes effective_at NOT NULL. Every row passes: 00233 filled each one,
-- and every row written since carries it.
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE email_outbox VALIDATE CONSTRAINT email_outbox_effective_at_not_null;
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_effective_at_not_null;
-- hardcount:blocking-ddl-ok: the validated CHECKs above prove effective_at IS NOT NULL, so Postgres skips the scan
ALTER TABLE email_outbox ALTER COLUMN effective_at SET NOT NULL;
ALTER TABLE invoice_charge_attempts ALTER COLUMN effective_at SET NOT NULL;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_effective_at_not_null;
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_effective_at_not_null;

-- +goose Down
ALTER TABLE email_outbox ALTER COLUMN effective_at DROP NOT NULL;
ALTER TABLE invoice_charge_attempts ALTER COLUMN effective_at DROP NOT NULL;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_effective_at_not_null CHECK (effective_at IS NOT NULL) NOT VALID;
