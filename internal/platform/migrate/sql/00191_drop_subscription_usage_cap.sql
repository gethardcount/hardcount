-- +goose Up
-- The block usage cap is cut (#973): it scaled every meter's billed quantity
-- by one cap/total factor across meters, which no peer does, and turned the
-- threshold tier ladder off, so a capped close could bill more than the
-- usage costs. No code reads or writes these columns any more. Dropping a
-- column only changes the catalog (no table rewrite), so it takes the lock
-- for an instant. A subscription that had a cap bills all its usage from its
-- next close; the CHANGELOG has the query that lists them.
ALTER TABLE subscriptions DROP COLUMN usage_cap_units, DROP COLUMN overage_action;

-- +goose Down
-- Rollback runs only with APP_ENV=local. The columns come back as the
-- baseline had them, empty: every subscription is uncapped ('charge').
ALTER TABLE subscriptions ADD COLUMN usage_cap_units bigint,
    ADD COLUMN overage_action text DEFAULT 'charge' NOT NULL;
