-- +goose Up
-- A spend threshold always keeps the billing period (2026-10-23): the
-- option to start a new period at each fire (reset_billing_cycle) is
-- removed, and no code reads or writes this column any more. A
-- subscription that had it true keeps its thresholds and, from its next
-- fire on, keeps its period, as one with false does. Nothing else changes:
-- the column was configuration, read only when a threshold fired, so no
-- issued invoice changes. Dropping a column only changes the catalog (no
-- table rewrite), so it takes the lock for an instant. The CHANGELOG has
-- the query that lists the subscriptions it applies to.
ALTER TABLE subscriptions DROP COLUMN billing_threshold_reset_cycle;

-- +goose Down
-- Rollback runs only with APP_ENV=local. The column comes back as 00201
-- left it, false on every row: the values the Up dropped are not restored.
ALTER TABLE subscriptions ADD COLUMN billing_threshold_reset_cycle boolean DEFAULT false NOT NULL;
