-- +goose Up
-- A subscription starts, or starts its trial, when it is created: the status
-- draft and POST /v1/subscriptions/{id}/activate are removed. The status
-- CHECK loses draft, and the column loses its default 'draft': every insert
-- names the status.
--
-- A draft bills nothing, and only the release being upgraded from can start
-- or cancel one. So while a subscription is a draft the upgrade stops here
-- and changes nothing, and the error says how many there are and names the
-- first five.
--
-- The DROP comes first: its lock keeps a new draft out until the new CHECK
-- is in place. The CHECK is added NOT VALID, which checks new rows at once
-- without scanning the table; 00236 validates the rows already there.
--
-- A webhook endpoint may list subscription.activated, the event the
-- activation sent. The endpoint update refuses a list with a name outside
-- the event catalog, and the dashboard sends the stored list back on every
-- edit, so the name is taken out of every stored list. An endpoint that
-- listed nothing else is turned off: an empty list matches no event.
--
-- subscriptions and webhook_endpoints force row-level security, and the user
-- that runs migrations may be subject to it, so the file turns the bypass on
-- for its own transaction: the count must see every account's rows.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;

-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
    named text;
BEGIN
    SELECT count(*) INTO n FROM subscriptions WHERE status = 'draft';
    IF n > 0 THEN
        SELECT string_agg(format('%s (tenant %s)', id, tenant_id), ', ' ORDER BY id) INTO named
        FROM (SELECT id, tenant_id FROM subscriptions WHERE status = 'draft' ORDER BY id LIMIT 5) first_few;
        IF n > 5 THEN
            named := named || format(', and %s more', n - 5);
        END IF;
        RAISE EXCEPTION 'migration 00235 removes draft subscriptions, and this database has % of them: %', n, named
            USING HINT = 'Start the release you are upgrading from, activate or cancel each draft subscription, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00235-found-draft-subscriptions';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('trialing', 'active', 'canceled')) NOT VALID;
ALTER TABLE subscriptions ALTER COLUMN status DROP DEFAULT;

UPDATE webhook_endpoints
SET events = events - 'subscription.activated',
    active = active AND jsonb_array_length(events - 'subscription.activated') > 0
WHERE events @> '["subscription.activated"]';

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the default and the
-- CHECK 00201 left. A webhook endpoint's list stays as the Up left it: the
-- Up kept no record of which lists held subscription.activated.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('draft', 'trialing', 'active', 'canceled'));
ALTER TABLE subscriptions ALTER COLUMN status SET DEFAULT 'draft';
