-- +goose Up
-- Stripe's ten dispute statuses include the two of an appeal, which Stripe
-- sends only when appeals are turned on: under_review moves to
-- appeal_needs_response when the bank rejects the response and the dispute
-- can be appealed, then to appeal_under_review once the appeal is sent, and
-- the dispute closes won or lost (domain.DisputeStatus). Both are open, so the
-- closed_at CHECK keeps its four closed statuses.
-- hardcount:blocking-ddl-ok: invoice_disputes holds one row per card dispute, a few per account
ALTER TABLE invoice_disputes DROP CONSTRAINT invoice_disputes_status_check;
ALTER TABLE invoice_disputes ADD CONSTRAINT invoice_disputes_status_check CHECK (status IN (
    'warning_needs_response', 'warning_under_review', 'warning_closed',
    'needs_response', 'under_review', 'appeal_needs_response', 'appeal_under_review',
    'won', 'lost', 'prevented'));

-- +goose Down
-- Fails, and changes nothing, while a row holds an appeal status.
ALTER TABLE invoice_disputes DROP CONSTRAINT invoice_disputes_status_check;
ALTER TABLE invoice_disputes ADD CONSTRAINT invoice_disputes_status_check CHECK (status IN (
    'warning_needs_response', 'warning_under_review', 'warning_closed',
    'needs_response', 'under_review', 'won', 'lost', 'prevented'));
