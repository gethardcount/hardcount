-- +goose Up
-- A meter has one aggregation (ADR-130 §2). Until now each pricing rule
-- also had its own (meter_pricing_rules.aggregation_mode), and a rule's
-- events were rolled up under the rule's mode. From here every rule bills
-- under its meter's aggregation (meters.aggregation), so the column goes.
-- A rule whose mode is its meter's (last_during_period is a last meter's)
-- bills exactly as before. A rule whose mode differs, last_ever included,
-- would bill differently, so if there is one the upgrade stops here before
-- changing anything, and the error counts them. So does a meter whose
-- aggregation the new CHECK below would refuse. This release has no
-- upgrade path from Velox 0.8.x, so no such row is converted.
--
-- The meter's key becomes event_name, the name usage events send it by,
-- and so does the meter_key of held usage rows. A meter's aggregation gets
-- a CHECK for the first time; 00209 checks the rows already there.
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
    SELECT count(*) INTO n
      FROM meter_pricing_rules r JOIN meters m ON m.id = r.meter_id
     WHERE r.aggregation_mode IS DISTINCT FROM CASE m.aggregation WHEN 'last' THEN 'last_during_period' ELSE m.aggregation END;
    IF n > 0 THEN found := found || format(E'\n  %s pricing rule(s) whose aggregation_mode is not their meter''s aggregation', n); END IF;
    SELECT count(*) INTO n FROM meters WHERE aggregation NOT IN ('sum', 'count', 'max', 'last');
    IF n > 0 THEN found := found || format(E'\n  %s meter(s) whose aggregation is not sum, count, max or last', n); END IF;
    IF found <> '' THEN
        RAISE EXCEPTION 'migration 00208 gives each meter one aggregation for all its pricing rules, and this database has rows it cannot keep as they bill:%', found
            USING HINT = 'This release has no upgrade path from Velox 0.8.x: install fresh (see the v0.9.0 release notes). On a development database, change or delete those rows, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00208-found-rows-it-cannot-keep-as-they-bill';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE meter_pricing_rules DROP COLUMN aggregation_mode;

ALTER TABLE meters RENAME COLUMN key TO event_name;
ALTER TABLE meters RENAME CONSTRAINT meters_tenant_id_livemode_key_key TO meters_tenant_id_livemode_event_name_key;
ALTER TABLE usage_held_events RENAME COLUMN meter_key TO event_name;

ALTER TABLE meters ADD CONSTRAINT meters_aggregation_check
    CHECK (aggregation IN ('sum', 'count', 'max', 'last')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts the old schema back. Each
-- rule's aggregation_mode is its meter's aggregation, the mode the rule
-- billed under (last is last_during_period).
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE meters DROP CONSTRAINT meters_aggregation_check;
ALTER TABLE usage_held_events RENAME COLUMN event_name TO meter_key;
ALTER TABLE meters RENAME CONSTRAINT meters_tenant_id_livemode_event_name_key TO meters_tenant_id_livemode_key_key;
ALTER TABLE meters RENAME COLUMN event_name TO key;
ALTER TABLE meter_pricing_rules ADD COLUMN aggregation_mode text DEFAULT 'sum' NOT NULL;
UPDATE meter_pricing_rules r
   SET aggregation_mode = CASE m.aggregation WHEN 'last' THEN 'last_during_period' ELSE m.aggregation END
  FROM meters m
 WHERE m.id = r.meter_id;
ALTER TABLE meter_pricing_rules ADD CONSTRAINT meter_pricing_rules_aggregation_mode_check
    CHECK (aggregation_mode IN ('sum', 'count', 'last_during_period', 'last_ever', 'max'));
