-- +goose Up
-- Nothing reads or writes the charge hold's token: the stale-hold sweep
-- clears every stale hold the same way (ADR-137, amendment 2026-11-24).
-- 00225 added the column with no index or constraint.
ALTER TABLE invoices DROP COLUMN charge_hold_token;

-- +goose Down
-- Rollback runs only with APP_ENV=local. The column comes back empty, as
-- 00225 added it.
ALTER TABLE invoices ADD COLUMN charge_hold_token TIMESTAMPTZ;
