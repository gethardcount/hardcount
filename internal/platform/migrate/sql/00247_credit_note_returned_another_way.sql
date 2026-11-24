-- +goose Up
-- When the operator marked a credit note's failed card refund as returned
-- another way (paid back outside Stripe). NULL on every other note. A second
-- mark, and Retry refund, answer that it is already recorded.
ALTER TABLE credit_notes ADD COLUMN returned_another_way_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE credit_notes DROP COLUMN returned_another_way_at;
