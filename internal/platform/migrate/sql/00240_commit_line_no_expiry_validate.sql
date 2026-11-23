-- +goose Up
-- Every row passes the commit CHECK 00239 added NOT VALID: the old CHECK
-- already required the same of a commit line.
ALTER TABLE invoice_line_items VALIDATE CONSTRAINT invoice_line_items_commit_check;

-- +goose Down
-- Nothing to undo: validating changes no row.
SELECT 1;
