-- +goose Up
-- Checks the rows already in customer_credit_ledger against the currency
-- CHECK that 00199 added NOT VALID. Every row passes: 00199 gave each one
-- an uppercase code, and the CHECK has held for every row written since.
ALTER TABLE customer_credit_ledger VALIDATE CONSTRAINT customer_credit_ledger_currency_check;
-- hardcount:blocking-ddl-ok: the validated CHECK above proves currency IS NOT NULL, so Postgres skips the scan
ALTER TABLE customer_credit_ledger ALTER COLUMN currency SET NOT NULL;

-- +goose Down
-- Nothing to undo here: the Down of 00199 drops the column and its CHECK.
SELECT 1;
