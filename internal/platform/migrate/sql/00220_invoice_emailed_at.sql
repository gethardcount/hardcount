-- +goose Up
-- When Stripe is not connected for an invoice's mode, the auto-charge
-- collector emails the customer the invoice itself instead of a card-setup
-- link (ADR-060 amendment 2026-10-30). This is that email's send-once marker:
-- stamped after the email is queued, read so the next visit sends nothing
-- more. NULL means the collector has not emailed the invoice. It is separate
-- from no_pm_notified_at, which keeps meaning "a setup link was emailed".
ALTER TABLE invoices ADD COLUMN invoice_emailed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE invoices DROP COLUMN invoice_emailed_at;
