-- +goose Up
-- A credit-note refund whose Stripe result is unclear (a timeout, a network
-- error, a Stripe 5xx) is 'unknown', not 'failed' (#965): the customer may
-- have been refunded. refund_unknown_key keeps that request's idempotency
-- key, so Retry refund resends the same request, and Stripe answers with the
-- first request's result instead of making a second refund. Velox sets it
-- only while refund_status is 'unknown'.
-- The CHECK is added NOT VALID, so it holds for new rows at once; 00184
-- checks the rows already there.
ALTER TABLE credit_notes ADD COLUMN refund_unknown_key TEXT;
ALTER TABLE credit_notes DROP CONSTRAINT credit_notes_refund_status_check;
ALTER TABLE credit_notes ADD CONSTRAINT credit_notes_refund_status_check
    CHECK (refund_status IN ('none', 'pending', 'succeeded', 'failed', 'unknown')) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. A refund still 'unknown' goes back
-- to 'failed', the label Velox wrote before #965, so the old CHECK holds.
UPDATE credit_notes SET refund_status = 'failed' WHERE refund_status = 'unknown';
ALTER TABLE credit_notes DROP CONSTRAINT credit_notes_refund_status_check;
ALTER TABLE credit_notes ADD CONSTRAINT credit_notes_refund_status_check
    CHECK (refund_status IN ('none', 'pending', 'succeeded', 'failed'));
ALTER TABLE credit_notes DROP COLUMN refund_unknown_key;
