-- 0176: a saved failure record for subscription billing (issue #960, ADR-119).
--
-- A subscription whose billing fails used to be re-attempted on every billing
-- run and left only a log line. These five columns hold the latest failure of
-- the work the subscription has due: how many attempts failed, when, why, and
-- the earliest next attempt.
--
-- The record is keyed to the due instant it failed on (billing_failed_for).
-- Nothing clears it: a statement that makes the failing work stop being due
-- (a clean period close, a plan swap, clearing a failing mid-period cancel)
-- moves the row's due instant in that same statement, and the record stops
-- counting. So there is no trigger, no CHECK and no index here on purpose.
--
-- Additive and metadata-only: nullable columns and one constant default.
ALTER TABLE subscriptions
    ADD COLUMN billing_failed_for      TIMESTAMPTZ,
    ADD COLUMN billing_failure_count   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN billing_failed_at       TIMESTAMPTZ,
    ADD COLUMN billing_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN billing_last_error      TEXT;

COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on a wall-clock (test_clock_id IS NULL) active or trialing row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';
COMMENT ON COLUMN subscriptions.billing_next_attempt_at IS 'Earliest next billing attempt for a live failure record (application clock). NULL on a live record means parked: nothing retries until POST /v1/subscriptions/{id}/retry-billing.';
