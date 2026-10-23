-- +goose Up
-- Checks the rows already there against the retry_days CHECK that 00206
-- added NOT VALID. Every row passes: 00206 wrote them after refusing any
-- schedule it could not convert.
ALTER TABLE dunning_policies VALIDATE CONSTRAINT dunning_policies_retry_days_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00206 drops
-- the constraint.
SELECT 1;
