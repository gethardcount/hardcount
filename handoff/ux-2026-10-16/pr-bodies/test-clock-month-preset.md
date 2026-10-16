# fix(test-clocks): +1mo preset steps a calendar month in the tenant timezone

## What & why

The test clock Advance dialog shows and composes times in the tenant timezone, but its `+1mo` preset stepped the month with the browser's local `Date` fields (`getDate` / `setMonth`). When the browser zone differs from the tenant zone, the preset filled the wrong day.

Scenario (tenant timezone UTC, browser in America/Los_Angeles):

| Clock frozen at | Before | After (engine's answer) |
|---|---|---|
| 2026-01-31 00:00Z | 2026-03-01 00:00Z | 2026-02-28 00:00Z |
| 2026-10-01 00:00Z | 2026-10-31 00:00Z (misses the Nov 1 cycle close) | 2026-11-01 00:00Z |
| 2026-02-15 12:00Z | 2026-03-15 11:00Z (browser DST shift) | 2026-03-15 12:00Z |

Fix: new `addCalendarMonthInTZ(instant, tz)` in `web-v2/src/lib/dates.ts`. It reads the civil date and wall time in the tenant zone, steps to the next month, clamps the day to that month's last day, and re-grounds with `fromZonedTime`. This mirrors `domain.advanceAnchored` (ADR-055 clamp, computed in the billing TZ per ADR-058). `TestClockDetail.tsx` calls it with the dialog's `tz`.

Note: `domain.addIntervalIn` (the no-anchor branch) uses Go `AddDate`, which overflows (Jan 31 → Mar 3) instead of clamping. Subscriptions with an anchor day use the clamped `advanceAnchored`; the preset already documented the clamp as intended, so this PR keeps the clamp and only fixes the zone.

### Test

`web-v2/tests/dates.test.ts`: three `addCalendarMonthInTZ …` tests (month-end clamp + leap year + December rollover; 1st-of-month cycle close in UTC and Asia/Kolkata; wall time kept across DST for America/New_York and for a UTC tenant). Every case names its zone. Passes under `TZ=UTC`, `TZ=America/Los_Angeles`, `TZ=Asia/Kolkata`.

### Mutation check

Replaced the helper body with the old browser-local logic:

- `TZ=America/Los_Angeles`: 3 fail, e.g. `expected: '2026-11-01T00:00:00.000Z' actual: '2026-10-31T00:00:00.000Z'`
- `TZ=Asia/Kolkata`: 1 fail, `expected: '2026-03-15T13:00:00.000Z' actual: '2026-03-15T14:00:00.000Z'`
- `TZ=UTC`: 2 fail, `expected: '2026-02-27T18:30:00.000Z' actual: '2026-02-28T18:30:00.000Z'`

Restored → all pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/pages/TestClockDetail.tsx tests/dates.test.ts`: 0 errors, 2 warnings (existing unused `eslint-disable` directives at TestClockDetail.tsx:474/480, lines not touched)
- `npm test`: 236 pass, 0 fail (also under `TZ=America/Los_Angeles`)
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. Settings → timezone **UTC**. Run the browser in America/Los_Angeles (OS zone or `TZ=America/Los_Angeles` when launching Chrome).
2. Test Clocks → create a clock at **Jan 31 2026 00:00** → Advance → click **+1mo**: date field shows **Feb 28, 2026**, time **00:00**.
3. Create a clock at **Oct 1 2026 00:00** → Advance → **+1mo**: shows **Nov 1, 2026 00:00**.
4. Control: set the browser zone to UTC and repeat; the results are the same.

## Checklist

- [x] Tests cover the new behavior (unit)
- [ ] `go test ./... -race -short -count=1` passes — no Go changed; not run
- [ ] `make lint` passes — no Go changed; not run
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW TC2, new `[ ]` box)
- [ ] `make gen` re-run — n/a
- [x] Frontend touched? `npx tsc -b`, `npm test`, `eslint` on changed files pass (`npm run build` not run separately)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
