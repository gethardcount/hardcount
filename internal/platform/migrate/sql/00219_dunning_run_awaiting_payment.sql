-- +goose Up
-- A dunning retry whose payment comes back still processing keeps its run
-- active and records here the PaymentIntent the run waits on (#1289). The
-- payment's failure report clears it with a compare-and-set, so the
-- webhook and the reconciler record that failure, and send its warning
-- email, at most once. NULL means the run waits on no payment.
ALTER TABLE invoice_dunning_runs ADD COLUMN awaiting_payment_intent_id TEXT;

-- +goose Down
ALTER TABLE invoice_dunning_runs DROP COLUMN awaiting_payment_intent_id;
