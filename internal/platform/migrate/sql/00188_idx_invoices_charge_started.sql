-- An index build: goose runs it without a transaction, and the file keeps
-- exactly this shape (docs/dev/migrations.md). It serves the reconciler's
-- hourly check of charge records (ADR-127): only invoices with a record are in
-- it, a handful at most, so the check never scans the whole table.
-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_charge_started;
CREATE INDEX CONCURRENTLY idx_invoices_charge_started ON invoices (livemode, charge_started_at) WHERE charge_started_at IS NOT NULL;
RESET lock_timeout;
-- +goose Down
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_charge_started;
RESET lock_timeout;
