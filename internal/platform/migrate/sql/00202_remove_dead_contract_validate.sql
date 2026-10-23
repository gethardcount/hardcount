-- +goose Up
-- Checks the rows already there against the CHECKs 00201 narrowed or added
-- NOT VALID. Every row passes: 00201 makes draft plans active, and stops
-- before changing anything when another row holds a value it removes.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_status_check;
ALTER TABLE plans VALIDATE CONSTRAINT plans_status_check;
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_tax_status_check;
ALTER TABLE invoice_dunning_runs VALIDATE CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs VALIDATE CONSTRAINT invoice_dunning_runs_state_check;
ALTER TABLE invoice_dunning_events VALIDATE CONSTRAINT invoice_dunning_events_event_type_check;
ALTER TABLE invoice_dunning_events VALIDATE CONSTRAINT invoice_dunning_events_state_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00201 puts
-- the old constraints back.
SELECT 1;
