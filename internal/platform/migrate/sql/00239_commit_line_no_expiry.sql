-- +goose Up
-- Bought credit never expires: a prepaid commit's line no longer carries an
-- expiry. The CHECK that tied the expiry to a commit line is replaced by one
-- without it. A draft line that held an expiry finalizes with none; a
-- finalized commit keeps the expiry already written on its credit grant.
ALTER TABLE invoice_line_items DROP CONSTRAINT invoice_line_items_commit_check;
ALTER TABLE invoice_line_items DROP COLUMN commit_expires_at;
ALTER TABLE invoice_line_items ADD CONSTRAINT invoice_line_items_commit_check
    CHECK (commit_granted_cents IS NULL OR (line_type = 'add_on' AND commit_granted_cents > 0)) NOT VALID;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the column, empty, and
-- the old CHECK.
ALTER TABLE invoice_line_items DROP CONSTRAINT invoice_line_items_commit_check;
ALTER TABLE invoice_line_items ADD COLUMN commit_expires_at TIMESTAMPTZ;
ALTER TABLE invoice_line_items ADD CONSTRAINT invoice_line_items_commit_check
    CHECK ((commit_granted_cents IS NULL AND commit_expires_at IS NULL)
        OR (line_type = 'add_on' AND commit_granted_cents > 0));
