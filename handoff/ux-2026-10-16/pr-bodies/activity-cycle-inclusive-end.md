## What & why

**Activity's Cycle dates end on the period's last day.**

`GET /v1/customers/{id}/usage` with no `from`/`to` answers the primary subscription's current period as a half-open window, `period.source = "current_billing_cycle"` (`internal/usage/customer_usage.go` `resolvePeriod`). The customer Activity card printed `formatDate(from) → formatDate(to)`, so `to` showed the exclusive boundary.

Scenario (org TZ UTC, calendar-monthly subscription, period `2026-10-01T00:00Z` → `2026-11-01T00:00Z`):

| Line | Before | After |
|---|---|---|
| Upcoming card (`current_billing_period_display`) | Oct 1, 2026 – Oct 31, 2026 | unchanged |
| Activity header, Cycle | Oct 1, 2026 → Nov 1, 2026 | Oct 1, 2026 – Oct 31, 2026 |
| Activity header, 30d | Sep 16, 2026 → Oct 16, 2026 | unchanged |

Fix: new `web-v2/src/lib/activityPeriod.ts` `activityCyclePeriodLabel(period, tz)` returns `formatCivilPeriod(from, to, tz)` for a cycle window (the client mirror of Go `FormatInclusivePeriod`, which authors `current_billing_period_display`), and `null` otherwise. `CostDashboard.tsx` passes `undefined` (org/tenant TZ, the subscription's anchor per ADR-077) and falls back to the old `from → to` text on `null`.

**Rolling windows left as they are.** 7d/30d/90d come from `rollingWindow`: `to` = now + 1 ms, not a day boundary. `formatCivilPeriod` would step back one day and print yesterday as the end, hiding today. The plain dates are already right there. The separators now differ (– for Cycle, → for rolling); I did not unify them, since that is a copy choice.

### Test and mutation check

`web-v2/tests/activityPeriod.test.ts` (3 tests: UTC cycle, New York cycle, explicit window → null). Mutation: made the helper always return `null` (the pre-fix render path) → 2 of 3 fail:

```
not ok 1 - Cycle preset reads Oct 1 – Oct 31, not → Nov 1
  expected: 'Oct 1, 2026 – Oct 31, 2026'
  actual: ~
```

Restored → 3/3 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint` on changed/added files: pass
- `npm test`: 236/236 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Customer page → Activity card**, customer with a calendar-monthly subscription (October period), **Cycle** tab: the small line under "Activity" reads "Oct 1, 2026 – Oct 31, 2026", identical to the Upcoming card's period above.
2. Same card, **30d** tab: line still reads "Sep 16, 2026 → Oct 16, 2026" (ends today).
3. Same, org TZ `America/New_York` with a period that starts at Oct 1 00:00 EDT: Cycle still reads "Oct 1, 2026 – Oct 31, 2026".
4. At 390px width: the longer Cycle line does not push the preset tabs off the card.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (FLOW U14, pending box)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
