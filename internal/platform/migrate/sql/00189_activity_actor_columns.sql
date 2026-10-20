-- +goose Up
-- Emails and charge attempts record who caused them (ADR-128 §4, PR 6), so
-- an activity line can say "by sagar@…", "by API key …" or "Automatic". The
-- values are audit.ResolveActor's, the same as audit_log's actor columns:
-- 'user', 'api_key', 'customer' or 'system'. No backfill: rows written
-- before this file keep NULL, because nothing recorded who caused them.
-- A charge attempt that a settle report inserted (a hosted-page payment)
-- also keeps NULL: the report does not know who paid.
ALTER TABLE email_outbox ADD COLUMN actor_type TEXT, ADD COLUMN actor_id TEXT;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_actor_type_check
    CHECK (actor_type IN ('user', 'api_key', 'customer', 'system')) NOT VALID;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_actor_pair_check
    CHECK ((actor_type IS NULL) = (actor_id IS NULL)) NOT VALID;

ALTER TABLE invoice_charge_attempts ADD COLUMN actor_type TEXT, ADD COLUMN actor_id TEXT;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_actor_type_check
    CHECK (actor_type IN ('user', 'api_key', 'customer', 'system')) NOT VALID;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_actor_pair_check
    CHECK ((actor_type IS NULL) = (actor_id IS NULL)) NOT VALID;

-- Charge now (the dashboard button, or POST /v1/invoices/{id}/collect) is
-- its own trigger, 'charge_now', instead of sharing 'auto_charge' with the
-- engine's own charges. Rows written before this file stay 'auto_charge'.
-- The CHECKs are added NOT VALID, so they hold for new rows at once; 00190
-- checks the rows already there.
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_trigger_source_check;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_trigger_source_check
    CHECK (trigger_source IN ('auto_charge', 'charge_now', 'dunning_retry', 'external')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. A 'charge_now' attempt goes back
-- to 'auto_charge', the trigger Velox wrote for it before this file, so the
-- old CHECK holds.
UPDATE invoice_charge_attempts SET trigger_source = 'auto_charge' WHERE trigger_source = 'charge_now';
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_trigger_source_check;
ALTER TABLE invoice_charge_attempts ADD CONSTRAINT invoice_charge_attempts_trigger_source_check
    CHECK (trigger_source IN ('auto_charge', 'dunning_retry', 'external'));
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_actor_pair_check;
ALTER TABLE invoice_charge_attempts DROP CONSTRAINT invoice_charge_attempts_actor_type_check;
ALTER TABLE invoice_charge_attempts DROP COLUMN actor_id, DROP COLUMN actor_type;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_actor_pair_check;
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_actor_type_check;
ALTER TABLE email_outbox DROP COLUMN actor_id, DROP COLUMN actor_type;
