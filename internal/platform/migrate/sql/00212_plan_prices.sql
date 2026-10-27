-- +goose Up
-- Usage prices belong to the plan (ADR-130 §1). A price is a row on a plan:
-- a meter, the events it takes (dimension_match) and its rate. Its content
-- over time is a list of versions, and on a test-mode plan each version also
-- records when it takes effect on each test clock (§7). A customer's price
-- override names a price. A plan's meters are the meters it has a price for,
-- so plans.meter_ids goes, and so do the rating rules, the meter pricing
-- rules and the meter's default price.
--
-- This release has no upgrade path from Velox 0.8.x, so no price is moved:
-- if any rating rule, pricing rule, override, plan meter list or invoice line
-- that names a rating rule exists, the upgrade stops here before changing
-- anything, and the error counts them.
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
    SELECT count(*) INTO n FROM meter_pricing_rules;
    IF n > 0 THEN found := found || format(E'\n  %s meter pricing rule(s)', n); END IF;
    SELECT count(*) INTO n FROM customer_price_overrides;
    IF n > 0 THEN found := found || format(E'\n  %s customer price override(s)', n); END IF;
    SELECT count(*) INTO n FROM plans WHERE jsonb_typeof(meter_ids) = 'array' AND jsonb_array_length(meter_ids) > 0;
    IF n > 0 THEN found := found || format(E'\n  %s plan(s) with meters', n); END IF;
    SELECT count(*) INTO n FROM invoice_line_items WHERE rating_rule_version_id IS NOT NULL;
    IF n > 0 THEN found := found || format(E'\n  %s invoice line(s) priced by a rating rule', n); END IF;
    IF found <> '' THEN
        RAISE EXCEPTION 'migration 00212 moves usage prices from meters to plans, and this database has prices it does not move:%', found
            USING HINT = 'This release has no upgrade path from Velox 0.8.x: install fresh (see the v0.9.0 release notes). On a development database, re-create it: docs/ops/runbook/migration-failed-midway.md#00212-found-prices-it-does-not-move';
    END IF;
END
$$;
-- +goose StatementEnd

-- A dimension_match is an object of at most 16 labels. Each key and each
-- value is a string of 1 to 100 characters (ADR-130 §1).
-- +goose StatementBegin
CREATE FUNCTION plan_price_match_ok(m jsonb) RETURNS boolean
    LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN jsonb_typeof(m) IS DISTINCT FROM 'object' THEN false
        ELSE (SELECT count(*) <= 16
                 AND bool_and(jsonb_typeof(e.value) = 'string'
                     AND length(e.key) BETWEEN 1 AND 100
                     AND length(e.value #>> '{}') BETWEEN 1 AND 100) IS NOT FALSE
                FROM jsonb_each(m) AS e)
    END
$$;
-- +goose StatementEnd

CREATE TABLE plan_prices (
    id              TEXT PRIMARY KEY DEFAULT ('vlx_prc_' || encode(gen_random_bytes(12), 'hex')),
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode        BOOLEAN NOT NULL DEFAULT true,
    plan_id         TEXT NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
    meter_id        TEXT NOT NULL REFERENCES meters(id) ON DELETE RESTRICT,
    dimension_match JSONB NOT NULL DEFAULT '{}' CHECK (plan_price_match_ok(dimension_match)),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- jsonb equality ignores key order, so one match is one price.
    CONSTRAINT plan_prices_identity UNIQUE (tenant_id, plan_id, meter_id, dimension_match)
);
CREATE INDEX plan_prices_meter ON plan_prices (tenant_id, meter_id);

-- A version is a price's content from valid_from on. A removal is a version
-- with removed set and no content. valid_from is '-infinity' for the one
-- version of a price on a plan no subscription has used.
CREATE TABLE plan_price_versions (
    id                   TEXT PRIMARY KEY DEFAULT ('vlx_ppv_' || encode(gen_random_bytes(12), 'hex')),
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode             BOOLEAN NOT NULL DEFAULT true,
    price_id             TEXT NOT NULL REFERENCES plan_prices(id) ON DELETE RESTRICT,
    valid_from           TIMESTAMPTZ NOT NULL,
    removed              BOOLEAN NOT NULL DEFAULT false,
    mode                 TEXT CHECK (mode IN ('per_unit', 'graduated', 'package')),
    unit_amount_cents    NUMERIC CHECK (unit_amount_cents >= 0),
    tiers                JSONB,
    package_size         BIGINT CHECK (package_size > 0),
    package_amount_cents NUMERIC CHECK (package_amount_cents >= 0),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plan_price_versions_shape CHECK (
        (removed AND mode IS NULL AND unit_amount_cents IS NULL AND tiers IS NULL
            AND package_size IS NULL AND package_amount_cents IS NULL)
        OR (NOT removed AND mode = 'per_unit' AND unit_amount_cents IS NOT NULL AND tiers IS NULL
            AND package_size IS NULL AND package_amount_cents IS NULL)
        OR (NOT removed AND mode = 'graduated' AND unit_amount_cents IS NULL AND jsonb_typeof(tiers) = 'array'
            AND package_size IS NULL AND package_amount_cents IS NULL)
        OR (NOT removed AND mode = 'package' AND unit_amount_cents IS NULL AND tiers IS NULL
            AND package_size IS NOT NULL AND package_amount_cents IS NOT NULL)),
    CONSTRAINT plan_price_versions_at UNIQUE (price_id, valid_from)
);

-- When a version takes effect on one test clock: the clock's time when the
-- version was written, plus 1 microsecond (ADR-130 §7). Test mode only.
CREATE TABLE plan_price_version_clocks (
    tenant_id     TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode      BOOLEAN NOT NULL DEFAULT false CHECK (livemode = false),
    version_id    TEXT NOT NULL REFERENCES plan_price_versions(id) ON DELETE CASCADE,
    test_clock_id TEXT NOT NULL REFERENCES test_clocks(id) ON DELETE CASCADE,
    valid_from    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (version_id, test_clock_id)
);
CREATE INDEX plan_price_version_clocks_clock ON plan_price_version_clocks (test_clock_id);

-- A customer's negotiated price for one plan price, in force over
-- [created_at, deactivated_at) in the customer's own time (ADR-130 §6). The
-- table is empty (checked above), so it is made again in the new shape.
DROP TABLE customer_price_overrides;
CREATE TABLE customer_price_overrides (
    id                   TEXT PRIMARY KEY DEFAULT ('vlx_cpo_' || encode(gen_random_bytes(12), 'hex')),
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode             BOOLEAN NOT NULL DEFAULT true,
    customer_id          TEXT NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    price_id             TEXT NOT NULL REFERENCES plan_prices(id) ON DELETE RESTRICT,
    mode                 TEXT NOT NULL CHECK (mode IN ('per_unit', 'graduated', 'package')),
    unit_amount_cents    NUMERIC NOT NULL DEFAULT 0,
    tiers                JSONB NOT NULL DEFAULT '[]',
    package_size         BIGINT NOT NULL DEFAULT 0,
    package_amount_cents NUMERIC NOT NULL DEFAULT 0,
    reason               TEXT,
    active               BOOLEAN NOT NULL DEFAULT true,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    deactivated_at       TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX customer_price_overrides_active ON customer_price_overrides (tenant_id, customer_id, price_id) WHERE active;
CREATE INDEX customer_price_overrides_price ON customer_price_overrides (tenant_id, price_id);

CREATE TRIGGER set_livemode BEFORE INSERT ON plan_prices
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
CREATE TRIGGER set_livemode BEFORE INSERT ON plan_price_versions
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
CREATE TRIGGER set_livemode BEFORE INSERT ON plan_price_version_clocks
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
CREATE TRIGGER set_livemode BEFORE INSERT ON customer_price_overrides
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();

ALTER TABLE plan_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_prices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON plan_prices USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
ALTER TABLE plan_price_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_price_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON plan_price_versions USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
ALTER TABLE plan_price_version_clocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_price_version_clocks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON plan_price_version_clocks USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
ALTER TABLE customer_price_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_price_overrides FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON customer_price_overrides USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));

GRANT SELECT, INSERT, UPDATE, DELETE ON plan_prices TO velox_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON plan_price_versions TO velox_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON plan_price_version_clocks TO velox_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON customer_price_overrides TO velox_app;

-- An invoice line names the price that billed it, and the events that price
-- takes. No foreign key: a line is a record of what was billed, and price
-- rows are never deleted.
ALTER TABLE invoice_line_items ADD COLUMN price_id TEXT;
ALTER TABLE invoice_line_items ADD COLUMN dimension_match JSONB;
ALTER TABLE invoice_line_items DROP COLUMN rating_rule_version_id;

ALTER TABLE meters DROP COLUMN rating_rule_version_id;
ALTER TABLE plans DROP COLUMN meter_ids;
DROP TABLE meter_pricing_rules;
DROP TABLE rating_rule_versions;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts the old schema back, with
-- no rows in the old pricing tables. Plan prices, their versions and clock
-- stamps, and price overrides are deleted: the old schema has no place for
-- them. Plans come back with no meters, and invoice lines lose their price.
SET LOCAL app.bypass_rls = 'on';

CREATE TABLE rating_rule_versions (
    id text DEFAULT ('vlx_rrv_' || encode(gen_random_bytes(12), 'hex')) NOT NULL PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    rule_key text NOT NULL,
    name text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    lifecycle_state text DEFAULT 'draft' NOT NULL,
    mode text NOT NULL,
    currency text DEFAULT 'USD' NOT NULL,
    unit_amount_cents numeric DEFAULT 0 NOT NULL,
    tiers jsonb DEFAULT '[]' NOT NULL,
    package_size bigint DEFAULT 0 NOT NULL,
    package_amount_cents numeric DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    livemode boolean DEFAULT true NOT NULL,
    CONSTRAINT rating_rule_versions_lifecycle_state_check CHECK (lifecycle_state IN ('draft', 'active', 'archived')),
    CONSTRAINT rating_rule_versions_mode_check CHECK (mode IN ('per_unit', 'graduated', 'package')),
    CONSTRAINT rating_rule_versions_tenant_id_livemode_rule_key_version_key UNIQUE (tenant_id, livemode, rule_key, version)
);
CREATE TRIGGER set_livemode BEFORE INSERT ON rating_rule_versions
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE rating_rule_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE rating_rule_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON rating_rule_versions USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
GRANT ALL ON TABLE rating_rule_versions TO velox_app;

CREATE TABLE meter_pricing_rules (
    id text DEFAULT ('vlx_mpr_' || encode(gen_random_bytes(12), 'hex')) NOT NULL PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    meter_id text NOT NULL REFERENCES meters(id) ON DELETE CASCADE,
    rating_rule_version_id text NOT NULL REFERENCES rating_rule_versions(id) ON DELETE RESTRICT,
    dimension_match jsonb DEFAULT '{}' NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT meter_pricing_rules_tenant_id_meter_id_rating_rule_version__key UNIQUE (tenant_id, meter_id, rating_rule_version_id)
);
CREATE INDEX idx_meter_pricing_rules_lookup ON meter_pricing_rules (tenant_id, meter_id, priority DESC);
ALTER TABLE meter_pricing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE meter_pricing_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON meter_pricing_rules USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR (tenant_id = current_setting('app.tenant_id', true)));
GRANT ALL ON TABLE meter_pricing_rules TO velox_app;

ALTER TABLE plans ADD COLUMN meter_ids jsonb DEFAULT '[]' NOT NULL;
ALTER TABLE meters ADD COLUMN rating_rule_version_id text REFERENCES rating_rule_versions(id) ON DELETE RESTRICT;

ALTER TABLE invoice_line_items ADD COLUMN rating_rule_version_id text;
ALTER TABLE invoice_line_items DROP COLUMN dimension_match;
ALTER TABLE invoice_line_items DROP COLUMN price_id;

DROP TABLE customer_price_overrides;
CREATE TABLE customer_price_overrides (
    id text DEFAULT ('vlx_cpo_' || encode(gen_random_bytes(12), 'hex')) NOT NULL PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    customer_id text NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    rating_rule_version_id text NOT NULL REFERENCES rating_rule_versions(id) ON DELETE RESTRICT,
    mode text NOT NULL,
    unit_amount_cents numeric DEFAULT 0 NOT NULL,
    tiers jsonb DEFAULT '[]' NOT NULL,
    package_size bigint DEFAULT 0 NOT NULL,
    package_amount_cents numeric DEFAULT 0 NOT NULL,
    reason text,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    livemode boolean DEFAULT true NOT NULL,
    rule_key text NOT NULL,
    deactivated_at timestamp with time zone,
    CONSTRAINT customer_price_overrides_mode_check CHECK (mode IN ('per_unit', 'graduated', 'package'))
);
CREATE UNIQUE INDEX idx_price_overrides_active_rule_key ON customer_price_overrides (tenant_id, customer_id, rule_key) WHERE active;
CREATE TRIGGER set_livemode BEFORE INSERT ON customer_price_overrides
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE customer_price_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_price_overrides FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON customer_price_overrides USING (
    (current_setting('app.bypass_rls', true) = 'on')
    OR ((tenant_id = current_setting('app.tenant_id', true))
        AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))));
GRANT ALL ON TABLE customer_price_overrides TO velox_app;

DROP TABLE plan_price_version_clocks;
DROP TABLE plan_price_versions;
DROP TABLE plan_prices;
DROP FUNCTION plan_price_match_ok(jsonb);
