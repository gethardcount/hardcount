## What & why

**Pause collection no longer offers today as the auto-resume date.**

Subscription page → Pause collection → "Auto-resume on". The DatePicker floor was `minDate={nowDate}` (the subscription's effective now), so today was selectable. The mutation sends `resumes_at = startOfDayInTZ(date)`. Start of today is already past, and the server refuses it: `internal/subscription/service.go` `if !ts.After(s.clock.Now(ctx)) → errs.Invalid("resumes_at", "must be in the future")` → 422.

Scenario (tenant TZ UTC, wall clock 2026-10-16 09:00Z):

| Pick | Before | After |
|---|---|---|
| Oct 16 (today) | selectable → `resumes_at=2026-10-16T00:00:00Z` → 422 "must be in the future" | disabled; typing it shows "Date must be on or after Oct 17, 2026" |
| Oct 17 | accepted | accepted (first enabled day) |

Fix: `civilDateFrom(nowMs, days, tz?)` in `web-v2/src/lib/dates.ts` returns the tenant-TZ civil date `days` after the anchor. The dialog floor is `civilDateFrom(nowDate.getTime(), 1)`, still from the existing anchor (`effectiveNow(frozen_time)` for a clock-pinned sub). The DatePicker reads its floor with browser-local getters, so it gets `new Date('<ymd>T00:00:00')` (local midnight of that civil date, the same way the picker parses its own value).

Side effect: the floor is now the tenant-TZ date, not the browser-local date of `nowDate`, so the picker also agrees with `startOfDayInTZ` when the browser and tenant zones differ.

Not changed: the extend-trial picker (`minDate` = trial end) and the server check.

### Test and mutation check

`web-v2/tests/dates.test.ts` › `civilDateFrom(now, 1) is tomorrow in the tenant zone, and its start is in the future` (New York evening, UTC morning, year rollover; asserts `startOfDayInTZ(result) > now`). Mutation: dropped `+ days` (floor = today, the old behaviour) →

```
not ok 4 - civilDateFrom(now, 1) is tomorrow in the tenant zone, and its start is in the future
  expected: '2026-10-17'
  actual: '2026-10-16'
```

Restored → 4/4 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/pages/SubscriptionDetail.tsx tests/dates.test.ts`: pass
- `npm test`: 234/234 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Subscription page → Pause collection dialog**, wall-clock sub, tenant TZ UTC: open the "Auto-resume on" calendar → today is greyed out, tomorrow is the first enabled day, and the calendar opens on the current month.
2. Same dialog: type today's date and blur → red "Date must be on or after <tomorrow>." under the field.
3. Clock-pinned sub frozen mid-day (e.g. 2027-08-01 12:00): the calendar opens on August 2027 with Aug 1 disabled and Aug 2 enabled; pick Aug 2 → Pause → toast "Collection paused" (no 422).
4. Tenant TZ UTC, browser in `America/New_York`, at 9 PM EDT on Oct 16 (= 01:00Z Oct 17): the first enabled day is **Oct 18**. Before, the floor was the browser's Oct 16, and both Oct 16 and Oct 17 (start 00:00Z Oct 17, already past) were refused with 422.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (FLOW TC9, pending box)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

## Money path only

Subscription pause state. Client-side picker floor only; no writer, effect or server gate changes. The one writer (`PUT /v1/subscriptions/{id}/pause-collection` → `service.PauseCollection`) and its `resumes_at > clock.Now` gate are unchanged; this change only stops the dialog offering a value that gate always refuses.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
