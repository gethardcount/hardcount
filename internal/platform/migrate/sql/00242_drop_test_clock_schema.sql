-- +goose Up
-- Test clocks are removed: 00241 deleted their rows, and no code reads or
-- writes these tables, columns or the catch-up's lease since. Each index and
-- constraint is dropped by name, so a schema that differs stops the file.
DROP TABLE plan_price_version_clocks;

ALTER TABLE customers DROP CONSTRAINT customers_test_clock_id_fkey;
DROP INDEX idx_customers_test_clock;
ALTER TABLE customers DROP COLUMN test_clock_id;

ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_test_clock_id_fkey;
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_test_clock_requires_testmode;
DROP INDEX idx_subscriptions_test_clock;
ALTER TABLE subscriptions DROP COLUMN test_clock_id;
COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on an active or trialing row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';

DROP TABLE test_clocks;

DROP INDEX idx_email_outbox_test_clock;
ALTER TABLE email_outbox DROP COLUMN test_clock_id;

DROP INDEX idx_audit_log_clock;
ALTER TABLE audit_log DROP COLUMN test_clock_id;
ALTER TABLE audit_log DROP COLUMN sim_effective_at;

ALTER TABLE invoices DROP COLUMN is_simulated;
ALTER TABLE credit_notes DROP COLUMN is_simulated;

-- hardcount:blocking-ddl-ok: leader_leases holds one row per singleton role, five rows after this file
DELETE FROM leader_leases WHERE role = 'testclock_catchup';
ALTER TABLE leader_leases DROP CONSTRAINT leader_leases_role_check;
ALTER TABLE leader_leases ADD CONSTRAINT leader_leases_role_check
	CHECK (role = ANY (ARRAY['billing', 'dunning', 'webhook_outbox', 'email_outbox', 'webhook_delivery']));

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back every dropped object as
-- it was, with no rows, and the catch-up's lease row as 00226 seeded it.
ALTER TABLE leader_leases DROP CONSTRAINT leader_leases_role_check;
ALTER TABLE leader_leases ADD CONSTRAINT leader_leases_role_check
	CHECK (role = ANY (ARRAY['billing', 'dunning', 'webhook_outbox', 'email_outbox', 'webhook_delivery', 'testclock_catchup']));
INSERT INTO leader_leases (role, holder_token)
VALUES ('testclock_catchup', (extract(epoch FROM clock_timestamp()) * 1000)::bigint)
ON CONFLICT (role) DO NOTHING;

ALTER TABLE credit_notes ADD COLUMN is_simulated boolean DEFAULT false NOT NULL;
ALTER TABLE invoices ADD COLUMN is_simulated boolean DEFAULT false NOT NULL;

ALTER TABLE audit_log ADD COLUMN sim_effective_at timestamp with time zone;
ALTER TABLE audit_log ADD COLUMN test_clock_id text;
CREATE INDEX idx_audit_log_clock ON public.audit_log USING btree (tenant_id, livemode, test_clock_id, created_at DESC, id DESC) WHERE (test_clock_id IS NOT NULL);

ALTER TABLE email_outbox ADD COLUMN test_clock_id text;
CREATE INDEX idx_email_outbox_test_clock ON public.email_outbox USING btree (test_clock_id) WHERE (test_clock_id IS NOT NULL);

CREATE TABLE public.test_clocks (
    id text DEFAULT ('hc_tclk_'::text || encode(public.gen_random_bytes(12), 'hex'::text)) NOT NULL,
    tenant_id text NOT NULL,
    livemode boolean DEFAULT false NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    frozen_time timestamp with time zone NOT NULL,
    status text DEFAULT 'ready'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_failure_reason text,
    last_advance_summary jsonb,
    advance_target timestamp with time zone,
    CONSTRAINT test_clocks_pkey PRIMARY KEY (id),
    CONSTRAINT test_clocks_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE RESTRICT,
    CONSTRAINT test_clocks_livemode_check CHECK ((livemode = false)),
    CONSTRAINT test_clocks_status_check CHECK ((status = ANY (ARRAY['ready'::text, 'advancing'::text, 'internal_failure'::text])))
);
CREATE INDEX idx_test_clocks_tenant ON public.test_clocks USING btree (tenant_id, created_at DESC);
ALTER TABLE public.test_clocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY public.test_clocks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.test_clocks USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = CASE current_setting('app.livemode', true) WHEN 'on' THEN true WHEN 'off' THEN false END))));
GRANT ALL ON TABLE public.test_clocks TO hardcount_app;

COMMENT ON COLUMN subscriptions.billing_failed_for IS 'The due instant the billing_* failure record is about. The record is live ONLY while this equals LEAST(next_billing_at, CASE WHEN status = ''active'' THEN cancel_at END) on a wall-clock (test_clock_id IS NULL) active or trialing row; otherwise the five billing_* columns are stale leftovers. Read them through domain.SubscriptionBillingFailingSQL, never bare.';
ALTER TABLE subscriptions ADD COLUMN test_clock_id text;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_test_clock_requires_testmode CHECK (((test_clock_id IS NULL) OR (livemode = false)));
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_test_clock_id_fkey FOREIGN KEY (test_clock_id) REFERENCES public.test_clocks(id) ON DELETE SET NULL;
CREATE INDEX idx_subscriptions_test_clock ON public.subscriptions USING btree (test_clock_id) WHERE (test_clock_id IS NOT NULL);

ALTER TABLE customers ADD COLUMN test_clock_id text;
ALTER TABLE customers ADD CONSTRAINT customers_test_clock_id_fkey FOREIGN KEY (test_clock_id) REFERENCES public.test_clocks(id) ON DELETE SET NULL;
CREATE INDEX idx_customers_test_clock ON public.customers USING btree (tenant_id, test_clock_id) WHERE (test_clock_id IS NOT NULL);

CREATE TABLE plan_price_version_clocks (
    tenant_id     TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode      BOOLEAN NOT NULL DEFAULT false CHECK (livemode = false),
    version_id    TEXT NOT NULL REFERENCES plan_price_versions(id) ON DELETE CASCADE,
    test_clock_id TEXT NOT NULL REFERENCES test_clocks(id) ON DELETE CASCADE,
    valid_from    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (version_id, test_clock_id)
);
CREATE INDEX plan_price_version_clocks_clock ON plan_price_version_clocks (test_clock_id);
CREATE TRIGGER set_livemode BEFORE INSERT ON plan_price_version_clocks
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
ALTER TABLE plan_price_version_clocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_price_version_clocks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.plan_price_version_clocks USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = CASE current_setting('app.livemode', true) WHEN 'on' THEN true WHEN 'off' THEN false END))));
GRANT SELECT, INSERT, UPDATE, DELETE ON plan_price_version_clocks TO hardcount_app;
