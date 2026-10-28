-- +goose Up
-- Removes values and a table that no Hardcount code writes:
--   - subscription status archived;
--   - plan status draft (a plan is created active; its default becomes
--     active);
--   - invoice tax_status failed (a failed calculation stays pending);
--   - payment recovery run state paused (the paused flag stays), and event
--     types retry_scheduled, retry_succeeded, retry_failed, paused and
--     resumed;
--   - the billed_entries table.
-- The run and event state and the event type get a CHECK for the first time.
-- The run state loses its default 'scheduled', which the CHECK refuses;
-- every writer names the state. billing_threshold_reset_cycle defaults to
-- false, as the API does.
--
-- It keeps what an earlier version wrote and no true value can replace:
--   - invoice line types discount and tax (lines added before #1324; an
--     issued invoice's lines are never changed);
--   - resolution manually_resolved (written by v0.1.0; migration 0170 left
--     it on runs whose invoice was neither voided nor written off);
--   - invoices.stripe_invoice_id (written by the Stripe importer, removed
--     2026-04-29);
--   - the coupons, coupon_redemptions and customer_discounts tables (written
--     before coupons were cut in ADR-039).
--
-- A draft plan becomes active: it was already sold and billed like an
-- active one, so nothing about it changes but the label. Nothing else is
-- rewritten. No Hardcount version wrote the other removed values, so a row that
-- holds one was written outside Hardcount. If there is such a row, the upgrade
-- stops here before changing anything, and the error lists them.
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
    SELECT count(*) INTO n FROM invoice_dunning_runs WHERE state NOT IN ('active', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery run(s) with state paused or another state outside active, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_events
     WHERE event_type NOT IN ('dunning_started', 'retry_attempted', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery event(s) with event_type retry_scheduled, retry_succeeded, retry_failed, paused or resumed, or another type outside dunning_started, retry_attempted, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM invoice_dunning_events WHERE state NOT IN ('active', 'escalated', 'resolved');
    IF n > 0 THEN found := found || format(E'\n  %s payment recovery event(s) with state paused or another state outside active, escalated, resolved', n); END IF;
    SELECT count(*) INTO n FROM billed_entries;
    IF n > 0 THEN found := found || format(E'\n  %s row(s) in billed_entries', n); END IF;
    IF found <> '' THEN
        RAISE EXCEPTION 'migration 00201 removes values that no Hardcount code writes, and this database has rows that hold them:%', found
            USING HINT = 'No Hardcount version wrote these values, so these rows were written outside Hardcount. Change or delete those rows, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00201-found-rows-no-hardcount-version-wrote';
    END IF;
END
$$;
-- +goose StatementEnd

UPDATE plans SET status = 'active' WHERE status = 'draft';

DROP TABLE billed_entries;

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

ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_state_check
    CHECK (state IN ('active', 'escalated', 'resolved')) NOT VALID;
ALTER TABLE invoice_dunning_runs ALTER COLUMN state DROP DEFAULT;

ALTER TABLE invoice_dunning_events ADD CONSTRAINT invoice_dunning_events_event_type_check
    CHECK (event_type IN ('dunning_started', 'retry_attempted', 'escalated', 'resolved')) NOT VALID;
ALTER TABLE invoice_dunning_events ADD CONSTRAINT invoice_dunning_events_state_check
    CHECK (state IN ('active', 'escalated', 'resolved')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the old schema;
-- billed_entries held no rows before the Up (the Up refuses to run
-- otherwise). A plan the Up made active stays active.
ALTER TABLE invoice_dunning_events DROP CONSTRAINT invoice_dunning_events_state_check;
ALTER TABLE invoice_dunning_events DROP CONSTRAINT invoice_dunning_events_event_type_check;
ALTER TABLE invoice_dunning_runs ALTER COLUMN state SET DEFAULT 'scheduled';
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_state_check;
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

CREATE TABLE billed_entries (
    id TEXT DEFAULT ('hc_ble_' || encode(gen_random_bytes(12), 'hex')) NOT NULL,
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
GRANT ALL ON TABLE billed_entries TO hardcount_app;
