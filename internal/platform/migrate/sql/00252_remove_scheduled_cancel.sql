-- +goose Up
-- Removes the scheduled cancel: a subscription ends by a cancel now. The file
-- is one transaction. In the order it runs:
-- 1. webhook_endpoints forces row-level security, so the bypass is turned
--    on: the updates must reach every account's rows.
-- 2. The billing_failed_for comment names next_billing_at, the one due
--    instant left.
-- 3. subscription.cancel_scheduled and subscription.cancel_cleared are no
--    longer sent. An endpoint that listed only those events is deleted;
--    every other list loses the names.
-- 4. cancel_at and cancel_at_period_end are dropped, and the index on
--    cancel_at with them. A subscription that had a cancel scheduled loses
--    the schedule and keeps billing: a cancel cannot be made in SQL, because
--    its final invoice and its relief need the billing engine.
--    docs/upgrades.md says to cancel such subscriptions before the upgrade.
SET LOCAL app.bypass_rls = 'on';

COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals next_billing_at on an active row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';

-- The list stays as it was: emptied, the API and the dashboard would read it
-- as "all events".
UPDATE webhook_endpoints
SET deleted_at = now(), active = false, updated_at = now()
WHERE deleted_at IS NULL
  AND (events @> '["subscription.cancel_scheduled"]' OR events @> '["subscription.cancel_cleared"]')
  AND events <@ '["subscription.cancel_scheduled", "subscription.cancel_cleared"]';

UPDATE webhook_endpoints
SET events = events - 'subscription.cancel_scheduled' - 'subscription.cancel_cleared', updated_at = now()
WHERE (events @> '["subscription.cancel_scheduled"]' OR events @> '["subscription.cancel_cleared"]')
  AND NOT events <@ '["subscription.cancel_scheduled", "subscription.cancel_cleared"]';

ALTER TABLE subscriptions DROP COLUMN cancel_at;
ALTER TABLE subscriptions DROP COLUMN cancel_at_period_end;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the columns, empty,
-- their index and the old comment. Endpoints stay as the Up left them: the Up
-- kept no record of what it changed.
ALTER TABLE subscriptions ADD COLUMN cancel_at_period_end boolean DEFAULT false NOT NULL;
ALTER TABLE subscriptions ADD COLUMN cancel_at timestamp with time zone;
CREATE INDEX idx_subscriptions_cancel_at ON subscriptions USING btree (cancel_at) WHERE (cancel_at IS NOT NULL);
COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on an active row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';
