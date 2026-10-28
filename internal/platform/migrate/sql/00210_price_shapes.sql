-- +goose Up
-- Price shapes take ADR-130's names (§3). The flat mode is per_unit, and
-- its rate column flat_amount_cents is unit_amount_cents. graduated_tiers
-- is tiers, whose open last tier is "up_to": null (it was 0). A package
-- bills ceil(quantity / package_size) packages, so a part package bills
-- as a whole one: package_amount_cents becomes a decimal like every other
-- price amount, and overage_unit_amount_cents goes. Customer price
-- overrides take the same shape. A provider cost rate is in cents, like
-- every *_cents field: cost_per_token (dollars) is unit_cost_cents.
--
-- Every row of these tables, and every invoice line labelled flat, would
-- need converting. This release has no upgrade path from Hardcount 0.8.x, so
-- nothing is converted: if any such row exists, the upgrade stops here
-- before changing anything, and the error counts them. 00211 checks the
-- new mode constraints against the rows already there, which are none.
--
-- These tables force row-level security, and the user that runs migrations
-- may be subject to it, so the file turns the bypass on for its own
-- transaction.
SET LOCAL app.bypass_rls = 'on';

-- +goose StatementBegin
DO $$
DECLARE
    found text := '';
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM rating_rule_versions;
    IF n > 0 THEN found := found || format(E'\n  %s rating rule version(s)', n); END IF;
    SELECT count(*) INTO n FROM customer_price_overrides;
    IF n > 0 THEN found := found || format(E'\n  %s customer price override(s)', n); END IF;
    SELECT count(*) INTO n FROM provider_cost_rates;
    IF n > 0 THEN found := found || format(E'\n  %s provider cost rate(s)', n); END IF;
    SELECT count(*) INTO n FROM invoice_line_items WHERE pricing_mode = 'flat';
    IF n > 0 THEN found := found || format(E'\n  %s invoice line(s) priced flat', n); END IF;
    IF found <> '' THEN
        RAISE EXCEPTION 'migration 00210 renames the price shapes and moves provider costs to cents, and this database has rows it does not convert:%', found
            USING HINT = 'This release has no upgrade path from Hardcount 0.8.x: install fresh (see the v0.9.0 release notes). On a development database, re-create it: docs/ops/runbook/migration-failed-midway.md#00210-found-prices-it-does-not-convert';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE rating_rule_versions DROP CONSTRAINT rating_rule_versions_mode_check;
ALTER TABLE rating_rule_versions RENAME COLUMN flat_amount_cents TO unit_amount_cents;
ALTER TABLE rating_rule_versions RENAME COLUMN graduated_tiers TO tiers;
ALTER TABLE rating_rule_versions ALTER COLUMN package_amount_cents TYPE numeric;
ALTER TABLE rating_rule_versions DROP COLUMN overage_unit_amount_cents;
ALTER TABLE rating_rule_versions ADD CONSTRAINT rating_rule_versions_mode_check
    CHECK (mode IN ('per_unit', 'graduated', 'package')) NOT VALID;

ALTER TABLE customer_price_overrides DROP CONSTRAINT customer_price_overrides_mode_check;
ALTER TABLE customer_price_overrides RENAME COLUMN flat_amount_cents TO unit_amount_cents;
ALTER TABLE customer_price_overrides RENAME COLUMN graduated_tiers TO tiers;
ALTER TABLE customer_price_overrides ALTER COLUMN package_amount_cents TYPE numeric;
ALTER TABLE customer_price_overrides DROP COLUMN overage_unit_amount_cents;
ALTER TABLE customer_price_overrides ADD CONSTRAINT customer_price_overrides_mode_check
    CHECK (mode IN ('per_unit', 'graduated', 'package')) NOT VALID;

ALTER TABLE provider_cost_rates DROP CONSTRAINT provider_cost_rates_cost_per_token_check;
ALTER TABLE provider_cost_rates RENAME COLUMN cost_per_token TO unit_cost_cents;
ALTER TABLE provider_cost_rates ALTER COLUMN unit_cost_cents TYPE numeric(22,12);
ALTER TABLE provider_cost_rates ADD CONSTRAINT provider_cost_rates_unit_cost_cents_check
    CHECK (unit_cost_cents >= 0) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts the old schema back and
-- turns rows written since into the old words: per_unit is flat, an open
-- last tier is up_to 0, a provider cost is dollars again. A package's
-- decimal amount is rounded to whole cents, and its overage is 0.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE provider_cost_rates DROP CONSTRAINT provider_cost_rates_unit_cost_cents_check;
ALTER TABLE provider_cost_rates ALTER COLUMN unit_cost_cents TYPE numeric(20,12) USING round(unit_cost_cents / 100, 12);
ALTER TABLE provider_cost_rates RENAME COLUMN unit_cost_cents TO cost_per_token;
ALTER TABLE provider_cost_rates ADD CONSTRAINT provider_cost_rates_cost_per_token_check
    CHECK (cost_per_token >= 0);

ALTER TABLE customer_price_overrides DROP CONSTRAINT customer_price_overrides_mode_check;
ALTER TABLE customer_price_overrides ADD COLUMN overage_unit_amount_cents numeric DEFAULT 0 NOT NULL;
ALTER TABLE customer_price_overrides ALTER COLUMN package_amount_cents TYPE bigint USING round(package_amount_cents);
ALTER TABLE customer_price_overrides RENAME COLUMN tiers TO graduated_tiers;
ALTER TABLE customer_price_overrides RENAME COLUMN unit_amount_cents TO flat_amount_cents;
UPDATE customer_price_overrides SET mode = 'flat' WHERE mode = 'per_unit';
UPDATE customer_price_overrides
   SET graduated_tiers = (
       SELECT jsonb_agg(CASE WHEN t->'up_to' = 'null'::jsonb THEN jsonb_set(t, '{up_to}', '0') ELSE t END ORDER BY i)
         FROM jsonb_array_elements(graduated_tiers) WITH ORDINALITY AS e(t, i))
 WHERE jsonb_array_length(graduated_tiers) > 0;
ALTER TABLE customer_price_overrides ADD CONSTRAINT customer_price_overrides_mode_check
    CHECK (mode IN ('flat', 'graduated', 'package'));

ALTER TABLE rating_rule_versions DROP CONSTRAINT rating_rule_versions_mode_check;
ALTER TABLE rating_rule_versions ADD COLUMN overage_unit_amount_cents numeric DEFAULT 0 NOT NULL;
ALTER TABLE rating_rule_versions ALTER COLUMN package_amount_cents TYPE bigint USING round(package_amount_cents);
ALTER TABLE rating_rule_versions RENAME COLUMN tiers TO graduated_tiers;
ALTER TABLE rating_rule_versions RENAME COLUMN unit_amount_cents TO flat_amount_cents;
UPDATE rating_rule_versions SET mode = 'flat' WHERE mode = 'per_unit';
UPDATE rating_rule_versions
   SET graduated_tiers = (
       SELECT jsonb_agg(CASE WHEN t->'up_to' = 'null'::jsonb THEN jsonb_set(t, '{up_to}', '0') ELSE t END ORDER BY i)
         FROM jsonb_array_elements(graduated_tiers) WITH ORDINALITY AS e(t, i))
 WHERE jsonb_array_length(graduated_tiers) > 0;
ALTER TABLE rating_rule_versions ADD CONSTRAINT rating_rule_versions_mode_check
    CHECK (mode IN ('flat', 'graduated', 'package'));

UPDATE invoice_line_items SET pricing_mode = 'flat' WHERE pricing_mode = 'per_unit';
