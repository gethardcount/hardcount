-- +goose Up
-- Stripe's ten dispute statuses include the appeal of a lost dispute:
-- appeal_needs_response and appeal_under_review (domain.DisputeStatus). Both
-- are open, so the closed_at CHECK keeps its four closed statuses.
-- hardcount:blocking-ddl-ok: invoice_disputes holds one row per card dispute, a few per account
ALTER TABLE invoice_disputes DROP CONSTRAINT invoice_disputes_status_check;
ALTER TABLE invoice_disputes ADD CONSTRAINT invoice_disputes_status_check CHECK (status IN (
    'warning_needs_response', 'warning_under_review', 'warning_closed',
    'needs_response', 'under_review', 'appeal_needs_response', 'appeal_under_review',
    'won', 'lost', 'prevented'));

-- +goose Down
ALTER TABLE invoice_disputes DROP CONSTRAINT invoice_disputes_status_check;
ALTER TABLE invoice_disputes ADD CONSTRAINT invoice_disputes_status_check CHECK (status IN (
    'warning_needs_response', 'warning_under_review', 'warning_closed',
    'needs_response', 'under_review', 'won', 'lost', 'prevented'));
