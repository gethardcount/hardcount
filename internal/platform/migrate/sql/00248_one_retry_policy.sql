-- +goose Up
-- An account has one dunning policy per mode, and it governs every
-- payment retry in that mode. Each (tenant_id, livemode) keeps one policy:
-- its default, or the oldest when it has none. The others are deleted. A
-- customer assigned to another policy follows the kept one from now on,
-- and so does a run already under way. Runs and customers stop naming a
-- policy, and the columns that told several policies apart go.
--
-- The run's policy_id goes first: its foreign key is ON DELETE RESTRICT,
-- so it would refuse the delete. The customer's is ON DELETE SET NULL.
--
-- These tables force row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE invoice_dunning_runs DROP COLUMN policy_id;

DELETE FROM dunning_policies p
USING (
    SELECT id, row_number() OVER (
        PARTITION BY tenant_id, livemode
        ORDER BY is_default DESC, created_at ASC, id ASC
    ) AS rank
    FROM dunning_policies
) ranked
WHERE p.id = ranked.id AND ranked.rank > 1;

-- Drops idx_customers_dunning_policy_id and the foreign key with it.
ALTER TABLE customers DROP COLUMN dunning_policy_id;

DROP INDEX idx_dunning_policies_one_default_per_tenant;
ALTER TABLE dunning_policies DROP COLUMN name, DROP COLUMN is_default, DROP COLUMN seeded;

-- hardcount:blocking-ddl-ok: dunning_policies holds at most two rows per account
CREATE UNIQUE INDEX idx_dunning_policies_one_per_mode ON dunning_policies (tenant_id, livemode);

-- +goose Down
-- Rollback runs only with APP_ENV=local. The columns come back as the
-- baseline made them. Each mode's one policy is its default, named
-- "Retries", and no customer is assigned to a policy. A run's policy_id
-- cannot be empty, so it is the policy of the run's mode.
SET LOCAL app.bypass_rls = 'on';

DROP INDEX idx_dunning_policies_one_per_mode;
ALTER TABLE dunning_policies
    ADD COLUMN name TEXT NOT NULL DEFAULT 'Retries',
    ADD COLUMN is_default BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN seeded BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE dunning_policies ALTER COLUMN name DROP DEFAULT;
UPDATE dunning_policies SET is_default = true;
CREATE UNIQUE INDEX idx_dunning_policies_one_default_per_tenant ON dunning_policies (tenant_id, livemode) WHERE is_default;
COMMENT ON COLUMN dunning_policies.seeded IS 'True for the policy created with the account that nobody has edited or chosen as default yet. While true, the next policy created takes over the default (ADR-123).';

ALTER TABLE customers ADD COLUMN dunning_policy_id TEXT;
ALTER TABLE customers ADD CONSTRAINT customers_dunning_policy_id_fkey FOREIGN KEY (dunning_policy_id) REFERENCES dunning_policies(id) ON DELETE SET NULL;
CREATE INDEX idx_customers_dunning_policy_id ON customers (tenant_id, dunning_policy_id) WHERE dunning_policy_id IS NOT NULL;

ALTER TABLE invoice_dunning_runs ADD COLUMN policy_id TEXT;
UPDATE invoice_dunning_runs r SET policy_id = p.id
FROM dunning_policies p
WHERE p.tenant_id = r.tenant_id AND p.livemode = r.livemode;
ALTER TABLE invoice_dunning_runs ALTER COLUMN policy_id SET NOT NULL;
ALTER TABLE invoice_dunning_runs ADD CONSTRAINT invoice_dunning_runs_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES dunning_policies(id) ON DELETE RESTRICT;
