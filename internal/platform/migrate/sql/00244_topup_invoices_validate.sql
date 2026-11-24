-- +goose Up
-- Every row passes both CHECKs 00243 added NOT VALID: no row has 'topup' or
-- a missing number before it.
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_billing_reason_check;
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_invoice_number_check;

-- +goose Down
-- Nothing to undo: validating changes no row.
SELECT 1;
