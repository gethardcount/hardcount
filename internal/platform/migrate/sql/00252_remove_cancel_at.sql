-- +goose Up
-- Removes the cancel on a chosen date (subscriptions.cancel_at): a scheduled
-- cancel is at the end of the current period (cancel_at_period_end). The
-- file is one transaction. In the order it runs:
-- 1. The table forces row-level security, so the bypass is turned on: the
--    update must reach every account's rows.
-- 2. An active subscription whose cancel_at is its current period's end is
--    flagged cancel_at_period_end. The previous release canceled it at that
--    period's close, as the flag does, so nothing else about it changes.
-- 3. Every other cancel_at goes with the column, and its subscription keeps
--    billing. A cancel cannot be done here: its final invoice and relief need
--    the billing engine. docs/upgrades.md says to cancel those subscriptions
--    before upgrading, or on their date after it. No event or audit row is
--    written.
-- 4. The billing_failed_for comment names the due instant, next_billing_at.
-- 5. The column is dropped, and idx_subscriptions_cancel_at with it.
SET LOCAL app.bypass_rls = 'on';

UPDATE subscriptions
SET cancel_at_period_end = true
WHERE status = 'active' AND cancel_at = current_billing_period_end;

COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals next_billing_at on an active row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';

ALTER TABLE subscriptions DROP COLUMN cancel_at;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the column, empty, with
-- its index, and the comment. Subscriptions the Up flagged stay flagged: the
-- Up kept no record of what it changed.
ALTER TABLE subscriptions ADD COLUMN cancel_at timestamp with time zone;
CREATE INDEX idx_subscriptions_cancel_at ON subscriptions (cancel_at) WHERE cancel_at IS NOT NULL;
COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on an active row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';
