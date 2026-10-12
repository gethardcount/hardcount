ALTER TABLE subscriptions
    DROP COLUMN IF EXISTS billing_last_error,
    DROP COLUMN IF EXISTS billing_next_attempt_at,
    DROP COLUMN IF EXISTS billing_failed_at,
    DROP COLUMN IF EXISTS billing_failure_count,
    DROP COLUMN IF EXISTS billing_failed_for;
