-- +goose Up
-- Checks the rows already in email_outbox and invoice_charge_attempts
-- against the CHECKs that 00189 added NOT VALID. Every row passes: the
-- actor columns are NULL on every row written before 00189, and 00189 only
-- adds a trigger value.
ALTER TABLE email_outbox VALIDATE CONSTRAINT email_outbox_actor_type_check;
ALTER TABLE email_outbox VALIDATE CONSTRAINT email_outbox_actor_pair_check;
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_actor_type_check;
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_actor_pair_check;
ALTER TABLE invoice_charge_attempts VALIDATE CONSTRAINT invoice_charge_attempts_trigger_source_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00189 drops
-- or replaces the constraints.
SELECT 1;
