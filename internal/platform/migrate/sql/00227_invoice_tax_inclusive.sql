-- +goose Up
-- Whether an invoice's prices include tax, recorded when its lines are
-- priced. Until now an invoice did not say, and the code that reads its
-- amounts later took the tenant's setting at the time of reading, which
-- changes when the tenant switches it.
--
-- The one INSERT into invoices writes it, from the same settings read that
-- priced the lines, and nothing updates it after.
--
-- An existing invoice takes its tenant's current setting: that is the one
-- thing known about it, and it is how the code reads it today, so no
-- existing invoice is read differently. Adding the column with a constant
-- default rewrites no row; only the invoices of tenants whose prices include
-- tax are updated.
ALTER TABLE invoices ADD COLUMN tax_inclusive boolean NOT NULL DEFAULT false;

UPDATE invoices i
SET tax_inclusive = true
FROM tenant_settings ts
WHERE ts.tenant_id = i.tenant_id
  AND ts.tax_inclusive;

-- +goose Down
ALTER TABLE invoices DROP COLUMN tax_inclusive;
