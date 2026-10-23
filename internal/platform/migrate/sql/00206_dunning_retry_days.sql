-- +goose Up
-- A dunning policy's schedule becomes one list, retry_days: the whole days
-- to wait before each retry, the first counted from the failed payment and
-- each later one from the retry before it. The number of retries is the
-- list's length. It replaces three columns that had to agree:
-- grace_period_days (the wait before retry 1), retry_schedule (Go
-- durations, the waits before retries 2 to N) and max_retry_attempts (N).
--
-- Each policy is converted exactly: retry_days is grace_period_days, then
-- the first max_retry_attempts - 1 entries of retry_schedule in days. The
-- runs retry at the same instants as before. Entries past the last retry
-- were never used and are dropped. A schedule the caller left out was
-- stored as JSON null (one retry needed none) and read as an empty list;
-- it converts as one.
--
-- A policy that cannot be converted exactly stops the upgrade before
-- anything changes, and the error names it and why: a wait used by a retry
-- that is not a whole number of days from 1 to 30 (Go durations allowed
-- "36h" or "1h30m"), or values Velox's own policy writer refused (a count
-- outside 1 to 15, a first wait outside 1 to 30 days, a schedule shorter
-- than the count). Rounding such a wait would retry on a day nobody chose.
-- Under the clean break from 0.8.x only local databases hold such rows.
--
-- retry_days gets a CHECK (1 to 15 entries, each a whole number from 1 to
-- 30), NOT VALID here so adding it reads no rows; 00207 checks the rows
-- this file wrote. The table holds a few rows per account.
--
-- This table forces row-level security, and the user that runs migrations
-- may be subject to it, so the file turns the bypass on for its own
-- transaction.
SET LOCAL app.bypass_rls = 'on';

-- The Go writer stored an omitted schedule as json.Marshal(nil), JSON null,
-- and the Go reader read it back as an empty list. Write it as one, so the
-- checks and the conversion below read it the same way. If the check
-- refuses, the transaction rolls this back too.
UPDATE dunning_policies SET retry_schedule = '[]'::jsonb WHERE jsonb_typeof(retry_schedule) = 'null';

-- The length of a Go duration string (time.ParseDuration's grammar) in
-- seconds, NULL when it is not one. Temporary: dropped at the end of the Up.
-- +goose StatementBegin
CREATE FUNCTION pg_temp.velox_go_duration_seconds(d text) RETURNS numeric
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    s text := d;
    sign numeric := 1;
    total numeric := 0;
    part text[];
BEGIN
    IF s IS NULL THEN
        RETURN NULL;
    END IF;
    IF left(s, 1) IN ('+', '-') THEN
        IF left(s, 1) = '-' THEN
            sign := -1;
        END IF;
        s := substr(s, 2);
    END IF;
    IF s = '0' THEN
        RETURN 0;
    END IF;
    IF s !~ '^(([0-9]+\.?[0-9]*|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+$' THEN
        RETURN NULL;
    END IF;
    FOR part IN SELECT regexp_matches(s, '([0-9]+\.?[0-9]*|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)', 'g') LOOP
        total := total + part[1]::numeric * CASE part[2]
            WHEN 'h' THEN 3600
            WHEN 'm' THEN 60
            WHEN 's' THEN 1
            WHEN 'ms' THEN 0.001
            WHEN 'ns' THEN 0.000000001
            ELSE 0.000001
        END;
    END LOOP;
    RETURN sign * total;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    found text;
BEGIN
    WITH used AS (
        SELECT p.id, e.ord, e.entry, pg_temp.velox_go_duration_seconds(e.entry) AS secs
        FROM dunning_policies p,
            jsonb_array_elements_text(CASE WHEN jsonb_typeof(p.retry_schedule) = 'array' THEN p.retry_schedule ELSE '[]'::jsonb END)
                WITH ORDINALITY AS e(entry, ord)
        WHERE e.ord <= p.max_retry_attempts - 1
    ), bad AS (
        SELECT id, format('max_retry_attempts %s is outside 1 to 15', max_retry_attempts) AS why
        FROM dunning_policies WHERE max_retry_attempts NOT BETWEEN 1 AND 15
        UNION ALL
        SELECT id, format('grace_period_days %s is outside 1 to 30', grace_period_days)
        FROM dunning_policies WHERE grace_period_days NOT BETWEEN 1 AND 30
        UNION ALL
        SELECT id, 'retry_schedule is not a list'
        FROM dunning_policies WHERE jsonb_typeof(retry_schedule) <> 'array'
        UNION ALL
        SELECT id, format('retry_schedule has %s entries; max_retry_attempts %s needs %s',
            jsonb_array_length(retry_schedule), max_retry_attempts, max_retry_attempts - 1)
        FROM dunning_policies
        WHERE jsonb_typeof(retry_schedule) = 'array' AND jsonb_array_length(retry_schedule) < max_retry_attempts - 1
        UNION ALL
        SELECT id, format('retry_schedule entry %s (%s) is not a whole number of days from 1 to 30', ord, entry)
        FROM used
        WHERE secs IS NULL OR secs % 86400 <> 0 OR secs / 86400 NOT BETWEEN 1 AND 30
    )
    SELECT string_agg(format(E'\n  dunning policy %s: %s', id, why), '' ORDER BY id, why) INTO found FROM bad;
    IF found IS NOT NULL THEN
        RAISE EXCEPTION 'migration 00206 writes each dunning schedule as whole days, and these policies cannot be converted exactly:%', found
            USING HINT = 'Fix each policy named above so it converts exactly (every wait its retries use a whole number of days from 1 to 30, for example retry_schedule "72h", not "36h"), or delete it, then run the upgrade again: docs/ops/runbook/migration-failed-midway.md#00206-a-dunning-schedule-is-not-whole-days';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE dunning_policies ADD COLUMN retry_days jsonb NOT NULL DEFAULT '[]'::jsonb;

UPDATE dunning_policies p
SET retry_days = jsonb_build_array(p.grace_period_days) || COALESCE((
    SELECT jsonb_agg((pg_temp.velox_go_duration_seconds(e.entry) / 86400)::int ORDER BY e.ord)
    FROM jsonb_array_elements_text(p.retry_schedule) WITH ORDINALITY AS e(entry, ord)
    WHERE e.ord <= p.max_retry_attempts - 1
), '[]'::jsonb);

-- Every writer names the schedule; there is no default one.
ALTER TABLE dunning_policies ALTER COLUMN retry_days DROP DEFAULT;

-- The pattern admits only a list of integers (not 3.0, which Go cannot
-- read into a whole day); the path then bounds each one.
ALTER TABLE dunning_policies ADD CONSTRAINT dunning_policies_retry_days_check CHECK (
    jsonb_typeof(retry_days) = 'array'
    AND jsonb_array_length(retry_days) BETWEEN 1 AND 15
    AND retry_days::text ~ '^\[[0-9]+(, [0-9]+)*\]$'
    AND NOT jsonb_path_exists(retry_days, '$[*] ? (@ < 1 || @ > 30)')
) NOT VALID;

ALTER TABLE dunning_policies DROP COLUMN grace_period_days;
ALTER TABLE dunning_policies DROP COLUMN retry_schedule;
ALTER TABLE dunning_policies DROP COLUMN max_retry_attempts;

DROP FUNCTION pg_temp.velox_go_duration_seconds(text);

-- +goose Down
-- Rollback runs only with APP_ENV=local. It writes each schedule back in
-- the three columns, exactly: grace_period_days is the first wait,
-- max_retry_attempts the count, retry_schedule the other waits in hours.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE dunning_policies ADD COLUMN retry_schedule jsonb DEFAULT '[]'::jsonb NOT NULL;
ALTER TABLE dunning_policies ADD COLUMN max_retry_attempts integer DEFAULT 3 NOT NULL;
ALTER TABLE dunning_policies ADD COLUMN grace_period_days integer DEFAULT 3 NOT NULL;

UPDATE dunning_policies p
SET grace_period_days = (p.retry_days ->> 0)::int,
    max_retry_attempts = jsonb_array_length(p.retry_days),
    retry_schedule = COALESCE((
        SELECT jsonb_agg(format('%sh', e.days::int * 24) ORDER BY e.ord)
        FROM jsonb_array_elements_text(p.retry_days) WITH ORDINALITY AS e(days, ord)
        WHERE e.ord > 1
    ), '[]'::jsonb);

ALTER TABLE dunning_policies DROP CONSTRAINT dunning_policies_retry_days_check;
ALTER TABLE dunning_policies DROP COLUMN retry_days;
