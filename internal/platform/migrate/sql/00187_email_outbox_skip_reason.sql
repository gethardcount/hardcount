-- +goose Up
-- A queued email that Hardcount did not send records why: the final state its
-- invoice reached while it waited (paid, voided, uncollectible, or gone when
-- a test clock's teardown deleted the invoice). Until now that state was
-- only inside last_error's text, so the activity lines could say no more
-- than "The invoice was already closed" (ADR-128). Rows skipped before this
-- migration keep NULL and keep that line.
-- The CHECK is added NOT VALID, so it holds for new rows at once; 00188
-- checks the rows already there.
ALTER TABLE email_outbox ADD COLUMN skip_reason TEXT;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_skip_reason_check
    CHECK (skip_reason IN ('paid', 'voided', 'uncollectible', 'gone')) NOT VALID;

-- +goose Down
ALTER TABLE email_outbox DROP CONSTRAINT email_outbox_skip_reason_check;
ALTER TABLE email_outbox DROP COLUMN skip_reason;
