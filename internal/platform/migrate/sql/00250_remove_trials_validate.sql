-- +goose Up
-- Every row passes the status CHECK 00249 added NOT VALID, because 00249
-- canceled every trialing subscription.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_status_check;

-- +goose Down
-- Nothing to undo: validating changes no row.
SELECT 1;
