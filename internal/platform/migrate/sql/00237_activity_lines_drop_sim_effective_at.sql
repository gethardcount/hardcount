-- +goose Up
-- effective_at (00232) dates an email and a charge attempt in both modes, and
-- nothing reads or writes sim_effective_at on these two tables since.
ALTER TABLE email_outbox DROP COLUMN sim_effective_at;
ALTER TABLE invoice_charge_attempts DROP COLUMN sim_effective_at;

-- +goose Down
-- The columns come back empty; 00232's Down fills them from effective_at.
ALTER TABLE invoice_charge_attempts ADD COLUMN sim_effective_at TIMESTAMPTZ;
ALTER TABLE email_outbox ADD COLUMN sim_effective_at TIMESTAMPTZ;
