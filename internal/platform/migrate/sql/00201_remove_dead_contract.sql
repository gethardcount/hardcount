-- +goose Up
-- Removes values, a column and four tables that no Velox code writes any
-- more:
--   - subscription status archived;
--   - plan status draft (a plan is created active; its default becomes
--     active);
--   - invoice tax_status failed (a failed calculation stays pending);
--   - payment recovery run state paused (the paused flag stays), resolution
--     manually_resolved, and event types retry_scheduled, retry_succeeded,
--     retry_failed, paused and resumed;
--   - invoices.stripe_invoice_id and its unique index;
--   - the coupons, coupon_redemptions, customer_discounts and billed_entries
--     tables (coupons were cut in ADR-039).
-- The run and event state and the event type get a CHECK for the first time.
-- billing_threshold_reset_cycle defaults to false, as the API does.
--
-- Invoice line types discount and tax stay allowed: lines added before #1324
-- have them, and an issued invoice's lines are never changed. The API
-- refuses them on a new line.
--
-- A draft plan becomes active: it was already sold and billed like an
-- active one, so nothing about it changes but the label. Nothing else is
-- rewritten. If any other row holds a removed value, the upgrade stops here
-- before changing anything, and the error lists them. No Velox release
-- since v0.2.0 writes those values.
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
    SELECT count(*) INTO n FROM subscriptions WHERE status = 'archived';
    IF n > 0 THEN found := found || format(E'\n  %s subscription(s) with status archived', n); END IF;
    SELECT count(*) INTO n FROM invoices WHERE tax_status = 'failed';
    IF n > 0 THEN found := found || format(E'\n  %s invoice(s) with tax_status failed', n); END IF;
    SELECT count(*) INTO n FROM invoices WHERE stripe_invoice_id IS NOT NULL;
    IF n > 0 THEN found := found || format(E'\n  %s invoice(s) with a stripe_invoice_id', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_runs WHERE state NOT IN ('active', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery run(s) with state paused or another state outside active, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_runs WHERE resolution = 'manually_resolved';
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery run(s) with resolution manually_resolved', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_events
     WHERE event_type NOT IN ('dunning_started', 'retry_attempted', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery event(s) with event_type retry_scheduled, retry_succeeded, retry_failed, paused or resumed, or another type outside dunning_started, retry_attempted, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_events WHERE state NOT IN ('active', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery event(s) with state paused or another state outside active, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM coupons;
    IF n > 0 THEN found := found || format(E'\n  %s row(s) in coupons', n); END IF;
    SELECT count(*) INTO n FROM coupon_redemptions;
    IF n > 0 THEN found := found || format(E'\n  %s row(s) in coupon_redemptions', n); END IF;
    SELECT count(*) INTO n FROM customer_discounts;
    IF n > 0 THEN found := found || format(E'\n  %s row(s) in customer_discounts', n); END IF;
    SELECT count(*) INTO n FROM billed_entries;
    IF n > 0 THEN found := found || format(E'\n  %s row(s) in billed_entries', n); END IF;
    IF found <> '' THEN
        RAISE EXCEPTION 'migration 00201 removes values that no Velox code writes, and this database has rows that hold them:%', found
            USING HINT = 'Nothing was changed. No Velox release since v0.2.0 writes these values, and Velox does not rewrite stored history to make room. Keep running the release you have, or install this one on a new database';
    END IF;
END
$$;
-- +goose StatementEnd

UPDATE plans SET status = 'active' WHERE status = 'draft';

DROP TABLE coupon_redemptions;
DROP TABLE customer_discounts;
DROP TABLE coupons;
DROP TABLE billed_entries;

-- Dropping the column drops idx_invoices_stripe_invoice_id with it.
ALTER TABLE invoices DROP COLUMN stripe_invoice_id;

-- Each narrowed or new CHECK holds for new rows at once; 00202 checks the
-- rows already there.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('draft', 'trialing', 'active', 'canceled')) NOT VALID;
ALTER TABLE subscriptions ALTER COLUMN billing_threshold_reset_cycle SET DEFAULT false;

ALTER TABLE plans DROP CONSTRAINT plans_status_check;
ALTER TABLE plans ADD CONSTRAINT plans_status_check
    CHECK (status IN ('active', 'archived')) NOT VALID;
ALTER TABLE plans ALTER COLUMN status SET DEFAULT 'active';

ALTER TABLE invoices DROP CONSTRAINT invoices_tax_status_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_tax_status_check
    CHECK (tax_status IN ('ok', 'pending')) NOT VALID;

ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'amount_below_minimum')) NOT VALID;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_state_check
    CHECK (state IN ('active', 'escalated', 'resolved')) NOT VALID;

ALTER TABLE invoice_dunning_events ADD CONSTRAINT invoice_dunning_events_event_type_check
    CHECK (event_type IN ('dunning_started', 'retry_attempted', 'escalated', 'resolved')) NOT VALID;
ALTER TABLE invoice_dunning_events ADD CONSTRAINT invoice_dunning_events_state_check
    CHECK (state IN ('active', 'escalated', 'resolved')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the old schema; rows
-- the tables held before the Up were none (the Up refuses to run otherwise).
-- A plan the Up made active stays active.
ALTER TABLE invoice_dunning_events DROP CONSTRAINT invoice_dunning_events_state_check;
ALTER TABLE invoice_dunning_events DROP CONSTRAINT invoice_dunning_events_event_type_check;
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_state_check;
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'manually_resolved', 'amount_below_minimum'));
ALTER TABLE invoices DROP CONSTRAINT invoices_tax_status_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_tax_status_check
    CHECK (tax_status IN ('ok', 'pending', 'failed'));
ALTER TABLE plans ALTER COLUMN status SET DEFAULT 'draft';
ALTER TABLE plans DROP CONSTRAINT plans_status_check;
ALTER TABLE plans ADD CONSTRAINT plans_status_check
    CHECK (status IN ('draft', 'active', 'archived'));
ALTER TABLE subscriptions ALTER COLUMN billing_threshold_reset_cycle SET DEFAULT true;
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('draft', 'trialing', 'active', 'canceled', 'archived'));

ALTER TABLE invoices ADD COLUMN stripe_invoice_id TEXT;
CREATE UNIQUE INDEX idx_invoices_stripe_invoice_id ON invoices (stripe_invoice_id) WHERE stripe_invoice_id IS NOT NULL;

CREATE TABLE billed_entries (
    id TEXT DEFAULT ('vlx_ble_' || encode(gen_random_bytes(12), 'hex')) NOT NULL,
    tenant_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    meter_id TEXT NOT NULL,
    amount_cents BIGINT NOT NULL,
    idempotency_key TEXT,
    source TEXT DEFAULT 'api' NOT NULL,
    "timestamp" TIMESTAMPTZ DEFAULT now() NOT NULL,
    livemode BOOLEAN DEFAULT true NOT NULL,
    CONSTRAINT billed_entries_pkey PRIMARY KEY (id),
    CONSTRAINT billed_entries_tenant_id_livemode_idempotency_key_key UNIQUE (tenant_id, livemode, idempotency_key),
    CONSTRAINT billed_entries_source_check CHECK (source IN ('api', 'replay_adjustment')),
    CONSTRAINT billed_entries_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE RESTRICT,
    CONSTRAINT billed_entries_meter_id_fkey FOREIGN KEY (meter_id) REFERENCES meters(id) ON DELETE RESTRICT,
    CONSTRAINT billed_entries_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
);
CREATE TRIGGER set_livemode BEFORE INSERT ON billed_entries FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE billed_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE billed_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billed_entries USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off')))));
GRANT ALL ON TABLE billed_entries TO velox_app;

CREATE TABLE coupons (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    code TEXT NOT NULL,
    name TEXT DEFAULT '' NOT NULL,
    type TEXT DEFAULT 'percentage' NOT NULL,
    amount_off BIGINT DEFAULT 0 NOT NULL,
    currency TEXT DEFAULT '' NOT NULL,
    max_redemptions INTEGER,
    times_redeemed INTEGER DEFAULT 0 NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    plan_ids TEXT[] DEFAULT '{}' NOT NULL,
    percent_off_bp INTEGER DEFAULT 0 NOT NULL,
    livemode BOOLEAN DEFAULT true NOT NULL,
    duration TEXT DEFAULT 'forever' NOT NULL,
    duration_periods INTEGER,
    stackable BOOLEAN DEFAULT false NOT NULL,
    customer_id TEXT,
    archived_at TIMESTAMPTZ,
    restrictions JSONB DEFAULT '{}' NOT NULL,
    metadata JSONB DEFAULT '{}' NOT NULL,
    version INTEGER DEFAULT 1 NOT NULL,
    CONSTRAINT coupons_pkey PRIMARY KEY (id),
    CONSTRAINT coupons_tenant_id_livemode_code_key UNIQUE (tenant_id, livemode, code),
    CONSTRAINT coupons_duration_check CHECK (duration IN ('once', 'repeating', 'forever')),
    CONSTRAINT coupons_duration_periods_check CHECK (((duration = 'repeating') AND (duration_periods IS NOT NULL) AND (duration_periods >= 1))
        OR ((duration IN ('once', 'forever')) AND (duration_periods IS NULL))),
    CONSTRAINT coupons_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
);
CREATE INDEX idx_coupons_tenant_archived ON coupons (tenant_id, archived_at) WHERE archived_at IS NULL;
CREATE INDEX idx_coupons_tenant_customer ON coupons (tenant_id, customer_id) WHERE customer_id IS NOT NULL;
CREATE TRIGGER set_livemode BEFORE INSERT ON coupons FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE coupons ENABLE ROW LEVEL SECURITY;
ALTER TABLE coupons FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON coupons USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off')))));
GRANT ALL ON TABLE coupons TO velox_app;

CREATE TABLE customer_discounts (
    id TEXT DEFAULT ('vlx_cud_' || encode(gen_random_bytes(12), 'hex')) NOT NULL,
    tenant_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    coupon_id TEXT NOT NULL,
    periods_applied INTEGER DEFAULT 0 NOT NULL,
    idempotency_key TEXT,
    metadata JSONB DEFAULT '{}' NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT customer_discounts_pkey PRIMARY KEY (id),
    CONSTRAINT customer_discounts_periods_applied_check CHECK (periods_applied >= 0),
    CONSTRAINT customer_discounts_coupon_id_fkey FOREIGN KEY (coupon_id) REFERENCES coupons(id) ON DELETE RESTRICT,
    CONSTRAINT customer_discounts_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE,
    CONSTRAINT customer_discounts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX idx_customer_discounts_active ON customer_discounts (tenant_id, customer_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_customer_discounts_coupon ON customer_discounts (tenant_id, coupon_id);
CREATE UNIQUE INDEX idx_customer_discounts_idempotency ON customer_discounts (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
ALTER TABLE customer_discounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_discounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON customer_discounts USING (((current_setting('app.bypass_rls', true) = 'on') OR (tenant_id = current_setting('app.tenant_id', true))));

CREATE TABLE coupon_redemptions (
    id TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    coupon_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    subscription_id TEXT,
    invoice_id TEXT,
    discount_cents BIGINT DEFAULT 0 NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    livemode BOOLEAN DEFAULT true NOT NULL,
    periods_applied INTEGER DEFAULT 0 NOT NULL,
    idempotency_key TEXT,
    voided_at TIMESTAMPTZ,
    CONSTRAINT coupon_redemptions_pkey PRIMARY KEY (id),
    CONSTRAINT coupon_redemptions_periods_applied_check CHECK (periods_applied >= 0),
    CONSTRAINT coupon_redemptions_coupon_id_fkey FOREIGN KEY (coupon_id) REFERENCES coupons(id) ON DELETE RESTRICT,
    CONSTRAINT coupon_redemptions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
);
CREATE INDEX idx_coupon_redemptions_coupon ON coupon_redemptions (tenant_id, coupon_id, created_at);
CREATE INDEX idx_coupon_redemptions_customer ON coupon_redemptions (tenant_id, customer_id);
CREATE UNIQUE INDEX idx_coupon_redemptions_idempotency ON coupon_redemptions (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX idx_coupon_redemptions_invoice_unique ON coupon_redemptions (tenant_id, coupon_id, invoice_id) WHERE (invoice_id IS NOT NULL) AND (subscription_id IS NULL);
CREATE INDEX idx_coupon_redemptions_subscription_active ON coupon_redemptions (tenant_id, subscription_id) WHERE (subscription_id IS NOT NULL) AND (voided_at IS NULL);
CREATE UNIQUE INDEX idx_coupon_redemptions_subscription_unique ON coupon_redemptions (tenant_id, coupon_id, subscription_id) WHERE subscription_id IS NOT NULL;
CREATE TRIGGER set_livemode BEFORE INSERT ON coupon_redemptions FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE coupon_redemptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE coupon_redemptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON coupon_redemptions USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off')))));
GRANT ALL ON TABLE coupon_redemptions TO velox_app;
