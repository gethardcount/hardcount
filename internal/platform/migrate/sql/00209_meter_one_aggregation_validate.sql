-- +goose Up
-- Checks the meters already there against the aggregation CHECK 00208
-- added NOT VALID. Every row passes: 00208 stops before changing anything
-- when a meter has another aggregation.
ALTER TABLE meters VALIDATE CONSTRAINT meters_aggregation_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00208 drops
-- the constraint.
SELECT 1;
