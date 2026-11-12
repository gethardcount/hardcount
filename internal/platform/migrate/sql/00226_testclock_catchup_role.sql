-- +goose Up
-- The test-clock catch-up becomes a leader-leased role (ADR-140, ADR-114).
-- The catch-up runs everything that fell due in the time a test clock
-- skipped. It ran on a queue in each process, so a deploy could strand an
-- advance or run it twice. Now one leader at a time runs it, on a tick, and
-- every write it makes to a clock row proves the role's fence.
--
-- The role list is the CHECK below, pinned to leader.Roles by
-- TestLease_RolesSeededMatchGoConstants. The new row is seeded as 0174
-- seeded the others: a holder_token from clock_timestamp() in milliseconds,
-- so a token never goes back below one an earlier install handed out.
--
-- hardcount:blocking-ddl-ok: leader_leases holds one row per singleton role, six rows on every install
ALTER TABLE leader_leases DROP CONSTRAINT leader_leases_role_check;
ALTER TABLE leader_leases ADD CONSTRAINT leader_leases_role_check
	CHECK (role = ANY (ARRAY['billing', 'dunning', 'webhook_outbox', 'email_outbox', 'webhook_delivery', 'testclock_catchup']));
INSERT INTO leader_leases (role, holder_token)
VALUES ('testclock_catchup', (extract(epoch FROM clock_timestamp()) * 1000)::bigint)
ON CONFLICT (role) DO NOTHING;

-- +goose Down
DELETE FROM leader_leases WHERE role = 'testclock_catchup';
ALTER TABLE leader_leases DROP CONSTRAINT leader_leases_role_check;
-- hardcount:blocking-ddl-ok: leader_leases holds one row per singleton role, five rows after this rollback
ALTER TABLE leader_leases ADD CONSTRAINT leader_leases_role_check
	CHECK (role = ANY (ARRAY['billing', 'dunning', 'webhook_outbox', 'email_outbox', 'webhook_delivery']));
