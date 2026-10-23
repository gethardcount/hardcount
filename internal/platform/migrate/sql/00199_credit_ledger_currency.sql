-- +goose Up
-- Every credit ledger row stores its own currency (ADR-129). A credit pays
-- only an invoice in that currency; amounts in two currencies are never
-- added together or converted.
--
-- customer_credit_ledger forces row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction.
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE customer_credit_ledger ADD COLUMN currency TEXT;

-- Dev/demo installs only; there is no production data. Each customer's rows
-- get the one currency a balance was read in before this release: the
-- billing profile's, then the plan of the live subscription, then the newest
-- invoice's, then the workspace default. So every customer's balance in that
-- currency equals its balance before. Ledger history that was really in
-- another currency is relabeled; the doctor's ledger_currency_matches_invoice
-- check reports those rows.
WITH ids AS (
    SELECT DISTINCT tenant_id, customer_id FROM customer_credit_ledger
),
live_plan AS (
    SELECT DISTINCT ON (s.tenant_id, s.customer_id) s.tenant_id, s.customer_id, p.currency
      FROM subscriptions s
      JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
      JOIN plans p ON p.id = si.plan_id
     WHERE s.status IN ('active', 'trialing')
     ORDER BY s.tenant_id, s.customer_id, s.current_billing_period_start DESC NULLS LAST, s.created_at DESC
),
newest_invoice AS (
    SELECT DISTINCT ON (i.tenant_id, i.customer_id) i.tenant_id, i.customer_id, i.currency
      FROM invoices i
     ORDER BY i.tenant_id, i.customer_id, i.created_at DESC
),
resolved AS (
    SELECT ids.tenant_id, ids.customer_id,
           UPPER(COALESCE(NULLIF(bp.currency, ''), lp.currency, ni.currency,
                          NULLIF(ts.default_currency, ''), 'USD')) AS currency
      FROM ids
      LEFT JOIN customer_billing_profiles bp ON bp.tenant_id = ids.tenant_id AND bp.customer_id = ids.customer_id
      LEFT JOIN live_plan lp ON lp.tenant_id = ids.tenant_id AND lp.customer_id = ids.customer_id
      LEFT JOIN newest_invoice ni ON ni.tenant_id = ids.tenant_id AND ni.customer_id = ids.customer_id
      LEFT JOIN tenant_settings ts ON ts.tenant_id = ids.tenant_id
)
UPDATE customer_credit_ledger l
   SET currency = r.currency
  FROM resolved r
 WHERE l.tenant_id = r.tenant_id AND l.customer_id = r.customer_id;

-- +goose Down
-- Rollback runs only with APP_ENV=local.
ALTER TABLE customer_credit_ledger DROP COLUMN currency;
