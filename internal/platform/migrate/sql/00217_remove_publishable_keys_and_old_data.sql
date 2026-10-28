-- +goose Up
-- Removes what v0.9.0 has no use for. v0.9.0 installs fresh only, so no
-- earlier version wrote a row here, and nothing is rewritten:
--   - API key type publishable (publishable keys are removed);
--   - invoice line types discount and tax, which 00201 kept for lines added
--     before #1324;
--   - dunning resolution manually_resolved, which 00201 kept for runs
--     v0.1.0 resolved;
--   - invoices.stripe_invoice_id and its unique index, which 00201 kept for
--     invoices the removed Stripe importer wrote;
--   - the coupons, coupon_redemptions and customer_discounts tables, which
--     00201 kept for rows written before coupons were cut (ADR-039).
-- Each narrowed CHECK holds for new rows at once; 00218 checks the rows
-- already there.
DROP TABLE customer_discounts;
DROP TABLE coupon_redemptions;
DROP TABLE coupons;

DROP INDEX idx_invoices_stripe_invoice_id;
ALTER TABLE invoices DROP COLUMN stripe_invoice_id;

ALTER TABLE api_keys DROP CONSTRAINT api_keys_key_type_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_key_type_check
    CHECK (key_type IN ('platform', 'secret')) NOT VALID;

ALTER TABLE invoice_line_items DROP CONSTRAINT invoice_line_items_line_type_check;
ALTER TABLE invoice_line_items ADD CONSTRAINT invoice_line_items_line_type_check
    CHECK (line_type IN ('base_fee', 'usage', 'add_on')) NOT VALID;

ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'amount_below_minimum')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the old schema with
-- no rows: the dropped tables and column come back empty.
ALTER TABLE invoice_dunning_runs DROP CONSTRAINT invoice_dunning_runs_resolution_check;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_resolution_check
    CHECK (resolution IS NULL OR resolution IN ('payment_recovered', 'invoice_voided', 'invoice_not_collectible',
        'retries_exhausted', 'action_failed', 'manually_resolved', 'amount_below_minimum'));

ALTER TABLE invoice_line_items DROP CONSTRAINT invoice_line_items_line_type_check;
ALTER TABLE invoice_line_items ADD CONSTRAINT invoice_line_items_line_type_check
    CHECK (line_type IN ('base_fee', 'usage', 'add_on', 'discount', 'tax'));

ALTER TABLE api_keys DROP CONSTRAINT api_keys_key_type_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_key_type_check
    CHECK (key_type IN ('platform', 'secret', 'publishable'));

ALTER TABLE invoices ADD COLUMN stripe_invoice_id TEXT;
CREATE UNIQUE INDEX idx_invoices_stripe_invoice_id ON public.invoices USING btree (stripe_invoice_id) WHERE (stripe_invoice_id IS NOT NULL);

CREATE TABLE public.coupons (
    id text NOT NULL,
    tenant_id text NOT NULL,
    code text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    type text DEFAULT 'percentage'::text NOT NULL,
    amount_off bigint DEFAULT 0 NOT NULL,
    currency text DEFAULT ''::text NOT NULL,
    max_redemptions integer,
    times_redeemed integer DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    plan_ids text[] DEFAULT '{}'::text[] NOT NULL,
    percent_off_bp integer DEFAULT 0 NOT NULL,
    livemode boolean DEFAULT true NOT NULL,
    duration text DEFAULT 'forever'::text NOT NULL,
    duration_periods integer,
    stackable boolean DEFAULT false NOT NULL,
    customer_id text,
    archived_at timestamp with time zone,
    restrictions jsonb DEFAULT '{}'::jsonb NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    CONSTRAINT coupons_pkey PRIMARY KEY (id),
    CONSTRAINT coupons_tenant_id_livemode_code_key UNIQUE (tenant_id, livemode, code),
    CONSTRAINT coupons_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE RESTRICT,
    CONSTRAINT coupons_duration_check CHECK ((duration = ANY (ARRAY['once'::text, 'repeating'::text, 'forever'::text]))),
    CONSTRAINT coupons_duration_periods_check CHECK ((((duration = 'repeating'::text) AND (duration_periods IS NOT NULL) AND (duration_periods >= 1)) OR ((duration = ANY (ARRAY['once'::text, 'forever'::text])) AND (duration_periods IS NULL))))
);
CREATE INDEX idx_coupons_tenant_archived ON public.coupons USING btree (tenant_id, archived_at) WHERE (archived_at IS NULL);
CREATE INDEX idx_coupons_tenant_customer ON public.coupons USING btree (tenant_id, customer_id) WHERE (customer_id IS NOT NULL);
CREATE TRIGGER set_livemode BEFORE INSERT ON public.coupons FOR EACH ROW EXECUTE FUNCTION public.set_livemode_from_session();
ALTER TABLE public.coupons ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY public.coupons FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.coupons USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = CASE current_setting('app.livemode', true) WHEN 'on' THEN true WHEN 'off' THEN false END))));
GRANT ALL ON TABLE public.coupons TO hardcount_app;

CREATE TABLE public.coupon_redemptions (
    id text NOT NULL,
    tenant_id text NOT NULL,
    coupon_id text NOT NULL,
    customer_id text NOT NULL,
    subscription_id text,
    invoice_id text,
    discount_cents bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    livemode boolean DEFAULT true NOT NULL,
    periods_applied integer DEFAULT 0 NOT NULL,
    idempotency_key text,
    voided_at timestamp with time zone,
    CONSTRAINT coupon_redemptions_pkey PRIMARY KEY (id),
    CONSTRAINT coupon_redemptions_coupon_id_fkey FOREIGN KEY (coupon_id) REFERENCES public.coupons(id) ON DELETE RESTRICT,
    CONSTRAINT coupon_redemptions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE RESTRICT,
    CONSTRAINT coupon_redemptions_periods_applied_check CHECK ((periods_applied >= 0))
);
CREATE INDEX idx_coupon_redemptions_coupon ON public.coupon_redemptions USING btree (tenant_id, coupon_id, created_at);
CREATE INDEX idx_coupon_redemptions_customer ON public.coupon_redemptions USING btree (tenant_id, customer_id);
CREATE UNIQUE INDEX idx_coupon_redemptions_idempotency ON public.coupon_redemptions USING btree (tenant_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);
CREATE UNIQUE INDEX idx_coupon_redemptions_invoice_unique ON public.coupon_redemptions USING btree (tenant_id, coupon_id, invoice_id) WHERE ((invoice_id IS NOT NULL) AND (subscription_id IS NULL));
CREATE INDEX idx_coupon_redemptions_subscription_active ON public.coupon_redemptions USING btree (tenant_id, subscription_id) WHERE ((subscription_id IS NOT NULL) AND (voided_at IS NULL));
CREATE UNIQUE INDEX idx_coupon_redemptions_subscription_unique ON public.coupon_redemptions USING btree (tenant_id, coupon_id, subscription_id) WHERE (subscription_id IS NOT NULL);
CREATE TRIGGER set_livemode BEFORE INSERT ON public.coupon_redemptions FOR EACH ROW EXECUTE FUNCTION public.set_livemode_from_session();
ALTER TABLE public.coupon_redemptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY public.coupon_redemptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.coupon_redemptions USING (((current_setting('app.bypass_rls', true) = 'on') OR ((tenant_id = current_setting('app.tenant_id', true)) AND (livemode = CASE current_setting('app.livemode', true) WHEN 'on' THEN true WHEN 'off' THEN false END))));
GRANT ALL ON TABLE public.coupon_redemptions TO hardcount_app;

CREATE TABLE public.customer_discounts (
    id text DEFAULT ('hc_cud_'::text || encode(public.gen_random_bytes(12), 'hex'::text)) NOT NULL,
    tenant_id text NOT NULL,
    customer_id text NOT NULL,
    coupon_id text NOT NULL,
    periods_applied integer DEFAULT 0 NOT NULL,
    idempotency_key text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_discounts_pkey PRIMARY KEY (id),
    CONSTRAINT customer_discounts_coupon_id_fkey FOREIGN KEY (coupon_id) REFERENCES public.coupons(id) ON DELETE RESTRICT,
    CONSTRAINT customer_discounts_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE,
    CONSTRAINT customer_discounts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE RESTRICT,
    CONSTRAINT customer_discounts_periods_applied_check CHECK ((periods_applied >= 0))
);
CREATE UNIQUE INDEX idx_customer_discounts_active ON public.customer_discounts USING btree (tenant_id, customer_id) WHERE (revoked_at IS NULL);
CREATE INDEX idx_customer_discounts_coupon ON public.customer_discounts USING btree (tenant_id, coupon_id);
CREATE UNIQUE INDEX idx_customer_discounts_idempotency ON public.customer_discounts USING btree (tenant_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);
ALTER TABLE public.customer_discounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY public.customer_discounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.customer_discounts USING (((current_setting('app.bypass_rls'::text, true) = 'on'::text) OR (tenant_id = current_setting('app.tenant_id'::text, true))));
