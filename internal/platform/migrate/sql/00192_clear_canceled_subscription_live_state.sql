-- +goose Up
-- A canceled or archived subscription carries no cancel schedule and no
-- pending plan change: both say what a live subscription does next. Before
-- this release every cancel left them, so the API and the dashboard showed a
-- scheduled cancel (with Undo) or a coming plan change on a canceled
-- subscription. Every cancel now clears them in its own transaction; this
-- clears the rows earlier cancels left.
--
-- The collection pause is kept: it still holds the subscription's unpaid
-- invoices (the auto-charge sweep and the tax retry read it) until an
-- operator resumes collection.
--
-- Clearing the pending columns writes no subscription_item_changes row: the
-- trigger records plan, quantity and removal changes only.
--
-- subscriptions and subscription_items force row-level security, and the
-- user that runs migrations may be subject to it, so the file turns the
-- bypass on for its own transaction.
SET LOCAL app.bypass_rls = 'on';
UPDATE subscriptions
SET cancel_at = NULL, cancel_at_period_end = false
WHERE status IN ('canceled', 'archived')
  AND (cancel_at IS NOT NULL OR COALESCE(cancel_at_period_end, false));
UPDATE subscription_items i
SET pending_plan_id = NULL, pending_plan_effective_at = NULL
FROM subscriptions s
WHERE s.id = i.subscription_id AND s.status IN ('canceled', 'archived')
  AND i.pending_plan_id IS NOT NULL;

-- +goose Down
-- Nothing to undo: the cleared values described what a canceled
-- subscription would do next, and it does nothing next.
SELECT 1;
