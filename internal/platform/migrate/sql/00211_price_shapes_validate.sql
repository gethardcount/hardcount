-- +goose Up
-- Checks the rows already there against the CHECKs 00210 added NOT VALID.
-- There are none: 00210 stops before changing anything when these tables
-- hold rows.
ALTER TABLE rating_rule_versions VALIDATE CONSTRAINT rating_rule_versions_mode_check;
ALTER TABLE customer_price_overrides VALIDATE CONSTRAINT customer_price_overrides_mode_check;
ALTER TABLE provider_cost_rates VALIDATE CONSTRAINT provider_cost_rates_unit_cost_cents_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00210 drops
-- the constraints.
SELECT 1;
