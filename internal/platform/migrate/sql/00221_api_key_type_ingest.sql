-- +goose Up
-- API keys are secret or ingest (ADR-133). An ingest key sends usage
-- events and nothing else. The platform key type is removed with
-- /v1/tenants: only a platform key could issue one, so no route ever issued
-- the first. A platform row was written by hand. If there is one, the
-- upgrade stops here and changes nothing, rather than deleting a
-- credential someone made.
--
-- The DROP comes first so its lock keeps new keys out until the new CHECK
-- is in place. api_keys forces row-level security, and the user that runs
-- migrations may be subject to it, so the file turns the bypass on for its
-- own transaction: the count must see every account's keys.
-- hardcount:blocking-ddl-ok: api_keys holds a few rows per account, so checking every row takes no time
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE api_keys DROP CONSTRAINT api_keys_key_type_check;

-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM api_keys WHERE key_type = 'platform';
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 00221 removes the platform API key type, and this database holds % platform key(s)', n
            USING HINT = 'No Hardcount route issues a platform key, so these rows were written by hand. Delete them, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00221-found-platform-api-keys';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE api_keys ADD CONSTRAINT api_keys_key_type_check
    CHECK (key_type IN ('secret', 'ingest'));

-- +goose Down
-- Rollback runs only with APP_ENV=local. It puts back the CHECK 00218
-- validated. It fails while an ingest key exists, which that CHECK refuses:
-- delete the ingest keys first.
SET LOCAL app.bypass_rls = 'on';
ALTER TABLE api_keys DROP CONSTRAINT api_keys_key_type_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_key_type_check
    CHECK (key_type IN ('platform', 'secret'));
