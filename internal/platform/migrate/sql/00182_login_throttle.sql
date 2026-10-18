-- +goose Up
-- Counters for the sign-in throttle (ADR-094, amended 2026-10-18). Each row
-- counts recent sign-in attempts for one subject: an address and an email
-- together, an email alone, or a browser that has signed in to that email.
-- The subject is an HMAC, so the table holds no email, address or cookie
-- value. There is no tenant before sign-in, so the table has no tenant_id
-- and no row-level security, like users and password_reset_tokens.
CREATE TABLE login_throttle (
    subject         TEXT PRIMARY KEY CHECK (subject ~ '^[0-9a-f]{64}$'),
    attempts        INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    blocked_until   TIMESTAMPTZ,
    signed_in_at    TIMESTAMPTZ
);
CREATE INDEX idx_login_throttle_last_attempt ON login_throttle (last_attempt_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON login_throttle TO velox_app;

-- +goose Down
DROP TABLE login_throttle;
