-- +goose Up
-- Removes the subscription status draft: a subscription starts, or starts its
-- trial, when it is created. The file is one transaction. In the order it runs:
-- 1. Both tables force row-level security, so the bypass is turned on: the
--    updates must reach every account's rows.
-- 2. The DROP's lock keeps a new draft out until the new CHECK is in place. It
--    is held while the table is scanned for drafts (a full scan: no index
--    covers the status). That is acceptable, because the upgrade runs with the
--    previous version stopped.
-- 3. Every draft is canceled, with the columns a cancel writes. A draft has no
--    billing period and no invoice, so no other row changes.
-- 4. The new CHECK is added NOT VALID, and 00236 validates it. The default
--    'draft', which the CHECK would refuse, is dropped.
-- 5. subscription.activated is no longer sent. An endpoint that listed only
--    that event is deleted; every other list loses the name.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;

UPDATE subscriptions
SET status = 'canceled', canceled_at = now(), updated_at = now(),
    cancel_at = NULL, cancel_at_period_end = false
WHERE status = 'draft';

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('trialing', 'active', 'canceled')) NOT VALID;
ALTER TABLE subscriptions ALTER COLUMN status DROP DEFAULT;

-- The list stays as it was: emptied, the API and the dashboard would read it
-- as "all events".
UPDATE webhook_endpoints
SET deleted_at = now(), active = false, updated_at = now()
WHERE deleted_at IS NULL
  AND events @> '["subscription.activated"]'
  AND events <@ '["subscription.activated"]';

UPDATE webhook_endpoints
SET events = events - 'subscription.activated', updated_at = now()
WHERE events @> '["subscription.activated"]'
  AND NOT events <@ '["subscription.activated"]';

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the default and the old
-- CHECK. Canceled subscriptions stay canceled, and endpoints stay as the Up
-- left them: the Up kept no record of what it changed.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('draft', 'trialing', 'active', 'canceled'));
ALTER TABLE subscriptions ALTER COLUMN status SET DEFAULT 'draft';
