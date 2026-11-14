-- +goose Up
-- The database states an API key's shape, which only the Go code held:
--   - key_prefix is hc_secret_ or hc_ingest_, then live_ or test_, then 12
--     lowercase hex characters; its type part is the row's key_type and its
--     mode part the row's livemode;
--   - key_hash is 64 lowercase hex characters (a SHA-256);
--   - key_salt is 32 lowercase hex characters, or empty for a key the setup
--     command wrote unsalted;
--   - the name is not blank;
--   - no two keys share a prefix, the column a request's key is looked up by.
-- key_type and livemode lose their defaults: every insert names the type,
-- and the set_livemode trigger sets the mode.
--
-- The CHECKs are added NOT VALID, which checks new rows at once without
-- scanning the table. 00231 validates the rows already there, in a file of
-- its own: a VALIDATE here would hold this file's lock on api_keys for the
-- whole scan.
--
-- Two keys that share a prefix stop the upgrade here, before anything
-- changes: Hardcount draws 48 random bits for each prefix, so such rows were
-- written by hand, and the migration does not choose which credential to
-- delete. api_keys forces row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction: the count must see every account's keys.
SET LOCAL app.bypass_rls = 'on';

-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM (
        SELECT key_prefix FROM api_keys GROUP BY key_prefix HAVING count(*) > 1
    ) shared;
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 00230 makes an API key''s prefix unique, and % prefix(es) in this database belong to more than one key', n
            USING HINT = 'Hardcount never issues two keys with one prefix, so these rows were written by hand. Keep one key for each prefix, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00230-found-api-keys-that-share-a-prefix';
    END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX idx_api_keys_prefix;
-- hardcount:blocking-ddl-ok: api_keys holds a few rows per account, so building its unique index takes no time
CREATE UNIQUE INDEX api_keys_key_prefix_key ON api_keys (key_prefix);

-- A valid prefix is hc_, the type (secret or ingest, equal to key_type), _,
-- the mode (live exactly when livemode, else test), _, then 12 lowercase hex
-- characters: hc_secret_live_0123456789ab.
ALTER TABLE api_keys ADD CONSTRAINT api_keys_prefix_shape_check CHECK (
    key_prefix ~ '^hc_(secret|ingest)_(live|test)_[0-9a-f]{12}$'
    AND split_part(key_prefix, '_', 2) = key_type
    AND (split_part(key_prefix, '_', 3) = 'live') = livemode
) NOT VALID;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_hash_shape_check
    CHECK (key_hash ~ '^[0-9a-f]{64}$') NOT VALID;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_salt_shape_check
    CHECK (key_salt ~ '^([0-9a-f]{32})?$') NOT VALID;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_name_check
    CHECK (btrim(name) <> '') NOT VALID;

ALTER TABLE api_keys ALTER COLUMN key_type DROP DEFAULT;
ALTER TABLE api_keys ALTER COLUMN livemode DROP DEFAULT;

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the defaults and the
-- lookup index 00180 made, and drops the shape.
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE api_keys ALTER COLUMN livemode SET DEFAULT true;
ALTER TABLE api_keys ALTER COLUMN key_type SET DEFAULT 'secret';
ALTER TABLE api_keys DROP CONSTRAINT api_keys_name_check;
ALTER TABLE api_keys DROP CONSTRAINT api_keys_salt_shape_check;
ALTER TABLE api_keys DROP CONSTRAINT api_keys_hash_shape_check;
ALTER TABLE api_keys DROP CONSTRAINT api_keys_prefix_shape_check;
DROP INDEX api_keys_key_prefix_key;
CREATE INDEX idx_api_keys_prefix ON api_keys (key_prefix) WHERE revoked_at IS NULL;
