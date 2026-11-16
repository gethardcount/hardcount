-- +goose Up
-- Every row passes the status CHECK 00235 added NOT VALID, because 00235
-- canceled every draft.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_status_check;

-- +goose Down
-- Nothing to undo: validating changes no row.
SELECT 1;
