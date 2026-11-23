-- +goose Up
-- advance_target is the time a test clock's advance moves it to: set while
-- the clock is advancing or its advance failed, so Retry advance carries on
-- to it, and NULL when the clock is ready. The price stamps read it while it
-- is set, so a price saved during an advance takes effect after its target.
-- Today frozen_time is already the target while a clock advances, so a clock
-- that is advancing or failed during the upgrade gets its frozen_time.
--
-- No CHECK ties it to status: during a rolling deploy a replica that runs
-- the code before this column marks a clock advancing without it, and a NULL
-- target reads as frozen_time, today's value.
--
-- test_clocks forces row-level security, and the user that runs migrations
-- may be subject to it, so the file turns the bypass on for its own
-- transaction.
SET LOCAL app.bypass_rls = 'on';

ALTER TABLE test_clocks ADD COLUMN advance_target TIMESTAMPTZ;

UPDATE test_clocks SET advance_target = frozen_time WHERE status <> 'ready';

-- +goose Down
ALTER TABLE test_clocks DROP COLUMN advance_target;
