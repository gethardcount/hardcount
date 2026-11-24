-- +goose Up
-- A top-up invoice sells prepaid credit (billing_reason 'topup'). It is a
-- draft with no number until it is paid, and its one line keeps the credit's
-- validity in days, copied from the tenant's setting when the draft is made.
-- 00244 validates both CHECKs.
ALTER TABLE invoices DROP CONSTRAINT invoices_billing_reason_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_billing_reason_check
    CHECK (billing_reason IS NULL OR billing_reason = ANY (ARRAY['subscription_cycle', 'subscription_create',
        'subscription_cancel', 'subscription_update', 'manual', 'threshold', 'topup'])) NOT VALID;
-- A number is required, except on a top-up that was never issued: a draft,
-- or a draft voided. A top-up is issued only by its payment, and a paid
-- invoice cannot be voided.
ALTER TABLE invoices ADD CONSTRAINT invoices_invoice_number_check
    CHECK (invoice_number IS NOT NULL OR (billing_reason = 'topup' AND status IN ('draft', 'voided'))) NOT VALID;
ALTER TABLE invoices ALTER COLUMN invoice_number DROP NOT NULL;
ALTER TABLE invoice_line_items ADD COLUMN credit_valid_days INTEGER;
ALTER TABLE tenant_settings ADD COLUMN topup_credit_valid_days INTEGER;

-- +goose Down
ALTER TABLE tenant_settings DROP COLUMN topup_credit_valid_days;
ALTER TABLE invoice_line_items DROP COLUMN credit_valid_days;
ALTER TABLE invoices ALTER COLUMN invoice_number SET NOT NULL;
ALTER TABLE invoices DROP CONSTRAINT invoices_invoice_number_check;
ALTER TABLE invoices DROP CONSTRAINT invoices_billing_reason_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_billing_reason_check
    CHECK (billing_reason IS NULL OR billing_reason = ANY (ARRAY['subscription_cycle', 'subscription_create',
        'subscription_cancel', 'subscription_update', 'manual', 'threshold']));
