-- 0180: mark the dunning policy a new account starts with (ADR-123, #1152).
--
-- Every new account now gets one policy per mode, "Default retries", as its
-- default. seeded = true says nobody has chosen it yet. While it is true, the
-- first policy anyone creates (by hand or from a recipe) takes over the
-- default, exactly as the first policy did before accounts had one. Editing
-- the seeded policy or making it the default sets seeded = false, and from
-- then on it keeps the default like any other policy.
--
-- No backfill, and no policies are created for existing accounts: a default
-- policy appearing on a running account would start retrying every failed or
-- unpaid invoice that has no run, of any age, with charges and emails.
ALTER TABLE dunning_policies
    ADD COLUMN seeded BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN dunning_policies.seeded IS 'True for the policy created with the account that nobody has edited or chosen as default yet. While true, the next policy created takes over the default (ADR-123).';
