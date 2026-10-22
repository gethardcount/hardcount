-- +goose Up
-- Checks the rows already in subscriptions against the two CHECKs 00193
-- added NOT VALID. Every row passes: 00193 marked every paused row
-- 'operator', and an unpaused row has neither column set.
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_pause_collection_set_by_check;
ALTER TABLE subscriptions VALIDATE CONSTRAINT subscriptions_pause_collection_set_by_pair_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00193 drops
-- the constraints.
SELECT 1;
