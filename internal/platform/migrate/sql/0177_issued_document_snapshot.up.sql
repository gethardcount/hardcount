-- 0177: what an issued document printed about its buyer and its seller
-- (issue #964, ADR-109 amendment 2026-10-14).
--
-- An issued invoice or credit note used to re-read the customer, billing
-- profile and company settings every time it was rendered, so a later edit
-- changed a document that had already been sent. These columns hold a copy of
-- the bill-to and supplier blocks, written in the same transaction that
-- issues the document. The shape is domain.DocumentBillTo and
-- domain.DocumentSupplier (internal/domain/document_snapshot.go).
--
-- NULL means the document is a draft, or was issued before this migration.
-- Those rows keep rendering from live data. There is no backfill: nothing
-- records what an old document showed, so any value written here would be
-- invented.
--
-- Additive and metadata-only: nullable columns, no default, no index.
ALTER TABLE invoices
    ADD COLUMN bill_to_snapshot  JSONB,
    ADD COLUMN supplier_snapshot JSONB;

ALTER TABLE credit_notes
    ADD COLUMN bill_to_snapshot  JSONB,
    ADD COLUMN supplier_snapshot JSONB;

COMMENT ON COLUMN invoices.bill_to_snapshot IS 'Buyer details the invoice was issued with (domain.DocumentBillTo). Written when the invoice leaves draft; NULL on drafts and on invoices issued before migration 0177, which render live.';
COMMENT ON COLUMN invoices.supplier_snapshot IS 'Seller details the invoice was issued with (domain.DocumentSupplier). Written when the invoice leaves draft; NULL on drafts and on invoices issued before migration 0177, which render live.';
COMMENT ON COLUMN credit_notes.bill_to_snapshot IS 'Buyer details the credit note was issued with (domain.DocumentBillTo). Written at issue; NULL on drafts and on credit notes issued before migration 0177, which render live.';
COMMENT ON COLUMN credit_notes.supplier_snapshot IS 'Seller details the credit note was issued with (domain.DocumentSupplier). Written at issue; NULL on drafts and on credit notes issued before migration 0177, which render live.';
