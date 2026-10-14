-- 0178: why the last Stripe key check failed, as a kind the dashboard can
-- branch on (operator messages 6/6).
--
-- last_verified_error held the whole Stripe error as JSON, and a network
-- failure landed in the same column with no way to tell the two apart. The
-- dashboard said "verification failed" either way. The check now stores
-- Stripe's own sentence in last_verified_error and the kind of failure here:
--   rejected     Stripe answered and refused the key (a 4xx).
--   unavailable  Stripe could not be reached, or answered with a 429/5xx, so
--                the key was not checked.
--
-- NULL when the last check passed, and on rows that failed before this
-- migration. No backfill: the old JSON could be parsed, but the next Connect
-- rewrites the row anyway, and the dashboard treats NULL as "rejected".
ALTER TABLE stripe_provider_credentials
    ADD COLUMN last_verified_error_kind TEXT
        CHECK (last_verified_error_kind IN ('rejected', 'unavailable'));

COMMENT ON COLUMN stripe_provider_credentials.last_verified_error_kind IS 'Kind of the last failed Stripe key check: rejected (Stripe refused the key) or unavailable (Stripe not reached, or a 429/5xx). NULL when the check passed or the row predates migration 0178.';
