-- +goose Up
-- A canceled or archived subscription carries no cancel schedule, no
-- collection pause and no pending plan change: those say what a live
-- subscription does next. Before this release every cancel left whichever of
-- them the subscription had. The API and the dashboard then showed a
-- scheduled cancel or a paused collection (with Undo and Resume) on a
-- canceled subscription, and once a pause's resumes_at passed, the
-- pause-resume sweep sent subscription.collection_resumed for it. Every
-- cancel now clears them in its own transaction; this clears the rows
-- earlier cancels left.
--
-- Clearing the pending columns writes no subscription_item_changes row: the
-- trigger records plan, quantity and removal changes only.
--
-- subscriptions and subscription_items force row-level security, and the
-- user that runs migrations may be subject to it, so the file turns the
-- bypass on for its own transaction.
SET LOCAL app.bypass_rls = 'on';
UPDATE subscriptions
SET cancel_at = NULL, cancel_at_period_end = false,
    pause_collection_behavior = NULL, pause_collection_resumes_at = NULL
WHERE status IN ('canceled', 'archived')
  AND (cancel_at IS NOT NULL OR COALESCE(cancel_at_period_end, false)
       OR pause_collection_behavior IS NOT NULL OR pause_collection_resumes_at IS NOT NULL);
UPDATE subscription_items i
SET pending_plan_id = NULL, pending_plan_effective_at = NULL
FROM subscriptions s
WHERE s.id = i.subscription_id AND s.status IN ('canceled', 'archived')
  AND i.pending_plan_id IS NOT NULL;

-- +goose Down
-- Nothing to undo: the cleared values described what a canceled
-- subscription would do next, and it does nothing next.
SELECT 1;
