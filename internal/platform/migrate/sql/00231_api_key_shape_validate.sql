-- +goose Up
-- Checks the API keys already there against the shape 00230 states. Every
-- key Hardcount wrote has it. A key of another shape was written by hand: it
-- stops the upgrade here, before any CHECK is validated, and the migration
-- does not delete a credential. 00230 stays applied, so new keys are already
-- checked. The file turns the bypass on for its own transaction, so the
-- count sees every account's keys.
SET LOCAL app.bypass_rls = 'on';

-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM api_keys
    WHERE NOT (key_prefix ~ '^hc_(secret|ingest)_(live|test)_[0-9a-f]{12}$'
               AND split_part(key_prefix, '_', 2) = key_type
               AND (split_part(key_prefix, '_', 3) = 'live') = livemode)
       OR key_hash !~ '^[0-9a-f]{64}$'
       OR key_salt !~ '^([0-9a-f]{32})?$'
       OR btrim(name) = '';
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 00231 checks the shape of every API key, and % key(s) in this database do not have it', n
            USING HINT = 'Hardcount writes every key in this shape, so these rows were written by hand. Delete them, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00231-found-api-keys-of-another-shape';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_prefix_shape_check;
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_hash_shape_check;
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_salt_shape_check;
ALTER TABLE api_keys VALIDATE CONSTRAINT api_keys_name_check;

-- +goose Down
-- Nothing to undo: validating changes no row, and the Down of 00230 drops
-- the constraints.
SELECT 1;
