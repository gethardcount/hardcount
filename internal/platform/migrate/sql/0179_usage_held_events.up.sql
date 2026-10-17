-- 0179: held usage (#1044, ADR-122).
--
-- The LiteLLM callback answers 200 for a row whose customer or meter does
-- not exist yet, because a non-2xx makes LiteLLM resend the whole batch.
-- Before this table the row was then dropped: LiteLLM treated the batch as
-- delivered and nothing resent it. usage_events cannot store it, because its
-- customer_id and meter_id must name real rows.
--
-- A held row keeps everything needed to ingest it later, exactly once:
--   idempotency_key  the key the row would have had in usage_events. Release
--                    inserts it under the SAME key, so a copy LiteLLM resent
--                    after the customer was created is deduplicated, not
--                    billed twice. NOT NULL: a row without a key cannot be
--                    released safely, and the callback refuses those.
--   event_at         the payload's time, or the time Velox received the row
--                    when the payload had none.
-- status moves held -> released or held -> discarded, never back. Release
-- claims rows with UPDATE ... WHERE status = 'held', so two concurrent
-- releases get disjoint rows.
CREATE TABLE usage_held_events (
    id                   TEXT PRIMARY KEY,
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode             BOOLEAN NOT NULL DEFAULT true,
    source               TEXT NOT NULL CHECK (source IN ('litellm')),
    external_customer_id TEXT NOT NULL,
    meter_key            TEXT NOT NULL,
    quantity             NUMERIC(38, 12) NOT NULL,
    properties           JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key      TEXT NOT NULL,
    event_at             TIMESTAMPTZ NOT NULL,
    observed_cost_micros BIGINT,
    reason               TEXT NOT NULL CHECK (reason IN ('customer_not_found', 'meter_not_found')),
    received_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    status               TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'released', 'discarded')),
    -- The usage_events row the release wrote. NULL on a released row means
    -- usage_events already held that key (a resend got there first).
    released_event_id    TEXT,
    released_customer_id TEXT,
    resolved_at          TIMESTAMPTZ,
    UNIQUE (tenant_id, livemode, idempotency_key)
);

CREATE INDEX usage_held_events_open
    ON usage_held_events (tenant_id, livemode, status, external_customer_id);

ALTER TABLE usage_held_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_held_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON usage_held_events FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR (tenant_id = current_setting('app.tenant_id', true)
        AND livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))
);

GRANT ALL ON TABLE usage_held_events TO velox_app;

-- livemode comes from the tx session like every other mode-partitioned
-- table, so a held test-mode row can only be released into test mode.
CREATE TRIGGER set_livemode
    BEFORE INSERT ON usage_held_events
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
