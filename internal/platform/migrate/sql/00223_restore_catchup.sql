-- +goose Up
-- The restore marker (ADR-135). One row means the database was restored from
-- a backup and has not caught up with Stripe yet: `hardcount serve` refuses
-- to start until `hardcount restore catch-up` has applied every Stripe event
-- since anchor, and deletes the row.
--
-- scripts/restore.sh and `hardcount restore begin` create this table too,
-- with the same columns, because a backup made before this migration
-- restores without it. So the Up must succeed on a database where the table
-- and its row already exist: IF NOT EXISTS, and nothing that touches the row.
--
--   id:     always true, so the table holds at most one row.
--   set_at: when the database was marked, on the database clock.
--   set_by: what marked it: restore.sh or hardcount restore begin.
--   anchor: the newest instant the restored data shows Hardcount running
--           (a leader-lease tick or a received Stripe webhook). The
--           catch-up reads events from 72 hours before it. NULL when the
--           backup holds neither.
--
-- No tenant_id and no row-level security: the row is about the whole
-- database, like leader_leases. Only the owner role writes it. The limited
-- app role may read it and nothing more, so no request can clear it.
CREATE TABLE IF NOT EXISTS restore_catchup (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    set_at timestamptz NOT NULL DEFAULT now(),
    set_by text NOT NULL,
    anchor timestamptz
);
REVOKE ALL ON TABLE public.restore_catchup FROM hardcount_app;
GRANT SELECT ON TABLE public.restore_catchup TO hardcount_app;

-- +goose Down
DROP TABLE restore_catchup;
