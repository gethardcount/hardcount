-- +goose NO TRANSACTION
-- +goose Up
-- The period slot: one live invoice per subscription and billing period,
-- as idx_invoices_billing_idempotency kept it, except that a manual invoice
-- no longer takes the slot. A manual invoice created with a subscription
-- and its exact cycle window used to take it, and the cycle close then
-- skipped that period (#1331). Proration invoices stay out, as before.
-- This index's rows are a subset of the old index's, so building it cannot
-- meet a duplicate. A manual invoice that already holds a period keeps its
-- row as it is; it simply stops holding the period. 00216 drops the old index.
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_billing_period_dedup;
CREATE UNIQUE INDEX CONCURRENTLY idx_invoices_billing_period_dedup ON public.invoices USING btree (tenant_id, subscription_id, billing_period_start, billing_period_end) WHERE ((status <> 'voided'::text) AND (source_plan_changed_at IS NULL) AND (billing_reason IS DISTINCT FROM 'manual'::text));
RESET lock_timeout;
-- +goose Down
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_billing_period_dedup;
RESET lock_timeout;
