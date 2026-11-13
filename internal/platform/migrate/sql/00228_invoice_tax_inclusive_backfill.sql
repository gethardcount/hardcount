-- +goose Up
-- invoices.tax_inclusive (00227) for the invoices that existed before it: an
-- invoice takes its tenant's current setting. That is the one thing known
-- about it, and it is how the code reads it today, so no existing invoice is
-- read differently. Only the invoices of tenants whose prices include tax
-- change; the default covers the rest.
--
-- invoices and tenant_settings force row-level security, and the user that
-- runs migrations may be subject to it, so the file turns the bypass on for
-- its own transaction.
SET LOCAL app.bypass_rls = 'on';
UPDATE invoices i
SET tax_inclusive = true
FROM tenant_settings ts
WHERE ts.tenant_id = i.tenant_id
  AND ts.tax_inclusive;

-- +goose Down
-- Nothing to undo: 00227's Down drops the column.
SELECT 1;
