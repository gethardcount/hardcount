-- +goose Up
-- Checks the subscriptions already there against the status CHECK 00235
-- added NOT VALID. Every row passes: 00235 stops before changing anything
-- while a subscription is a draft.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_status_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00235 puts
-- the old constraint back.
SELECT 1;
