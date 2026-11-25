-- +goose Up
-- Removes subscription trials: a subscription is active from its creation.
-- The file is one transaction. In the order it runs:
-- 1. Both tables force row-level security, so the bypass is turned on: the
--    updates must reach every account's rows.
-- 2. The DROP's lock keeps a new trialing row out until the new CHECK is in
--    place. It is held while the table is scanned for trialing rows (a full
--    scan: no index covers the status). That is acceptable, because the
--    upgrade runs with the previous version stopped.
-- 3. Every trialing subscription is canceled, with the columns a cancel
--    writes, and its scheduled cancel is cleared. Nothing was invoiced for a
--    trial and nothing is owed, so no event or audit row is written.
-- 4. The new CHECK is added NOT VALID, and 00250 validates it.
-- 5. The trial columns are dropped, and activated_at with them: a
--    subscription is active from the instant it starts (started_at), and
--    nothing reads it.
-- 6. The billing_failed_for comment names the one status that bills.
-- 7. subscription.trial_ended and subscription.trial_extended are no longer
--    sent. An endpoint that listed only those events is deleted; every other
--    list loses the names.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;

UPDATE subscriptions
SET status = 'canceled', canceled_at = now(), updated_at = now(),
    cancel_at = NULL, cancel_at_period_end = false
WHERE status = 'trialing';

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active', 'canceled')) NOT VALID;

ALTER TABLE subscriptions DROP COLUMN trial_start_at;
ALTER TABLE subscriptions DROP COLUMN trial_end_at;
ALTER TABLE subscriptions DROP COLUMN activated_at;

COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on an active row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';

-- The list stays as it was: emptied, the API and the dashboard would read it
-- as "all events".
UPDATE webhook_endpoints
SET deleted_at = now(), active = false, updated_at = now()
WHERE deleted_at IS NULL
  AND (events @> '["subscription.trial_ended"]' OR events @> '["subscription.trial_extended"]')
  AND events <@ '["subscription.trial_ended", "subscription.trial_extended"]';

UPDATE webhook_endpoints
SET events = events - 'subscription.trial_ended' - 'subscription.trial_extended', updated_at = now()
WHERE (events @> '["subscription.trial_ended"]' OR events @> '["subscription.trial_extended"]')
  AND NOT events <@ '["subscription.trial_ended", "subscription.trial_extended"]';

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the columns, empty, and
-- the old CHECK. Canceled subscriptions stay canceled, and endpoints stay as
-- the Up left them: the Up kept no record of what it changed.
ALTER TABLE subscriptions ADD COLUMN trial_start_at timestamp with time zone;
ALTER TABLE subscriptions ADD COLUMN trial_end_at timestamp with time zone;
ALTER TABLE subscriptions ADD COLUMN activated_at timestamp with time zone;
COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on an active or trialing row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('trialing', 'active', 'canceled'));
