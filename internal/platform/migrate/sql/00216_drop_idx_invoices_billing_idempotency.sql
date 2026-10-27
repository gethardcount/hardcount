-- +goose NO TRANSACTION
-- +goose Up
-- idx_invoices_billing_period_dedup (00215) now holds the period slot.
-- The Down builds the old index back. It fails while a manual invoice and
-- another live invoice share a subscription period; void one of them first.
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_billing_idempotency;
RESET lock_timeout;
-- +goose Down
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_billing_idempotency;
CREATE UNIQUE INDEX CONCURRENTLY idx_invoices_billing_idempotency ON public.invoices USING btree (tenant_id, subscription_id, billing_period_start, billing_period_end) WHERE ((status <> 'voided'::text) AND (source_plan_changed_at IS NULL));
RESET lock_timeout;
