-- +goose Up
-- Whether an invoice's prices include tax, recorded when its lines are
-- priced. Until now an invoice did not say, and the code that reads its
-- amounts later took the tenant's setting at the time of reading, which
-- changes when the tenant switches it.
--
-- The one INSERT into invoices writes it, from the setting the invoice was
-- priced under, and nothing updates it after. Adding the column with a
-- constant default rewrites no row. 00228 sets it on the invoices that
-- existed before, in its own file, so this lock on invoices is short.
ALTER TABLE invoices ADD COLUMN tax_inclusive boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE invoices DROP COLUMN tax_inclusive;
