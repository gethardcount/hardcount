ALTER TABLE stripe_provider_credentials
    DROP COLUMN IF EXISTS last_verified_error_kind;
