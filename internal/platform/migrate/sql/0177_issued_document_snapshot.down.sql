ALTER TABLE credit_notes
    DROP COLUMN IF EXISTS supplier_snapshot,
    DROP COLUMN IF EXISTS bill_to_snapshot;

ALTER TABLE invoices
    DROP COLUMN IF EXISTS supplier_snapshot,
    DROP COLUMN IF EXISTS bill_to_snapshot;
