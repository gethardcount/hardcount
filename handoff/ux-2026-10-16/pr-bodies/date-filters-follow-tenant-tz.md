# fix(dashboard): usage and invoice date filters recompute when the tenant timezone loads

## What & why

`web-v2/src/lib/dates.ts` falls back to `'UTC'` until `TenantTimezoneBootstrap` has loaded `/v1/settings` and seeded the module-level tenant zone. `UsageEvents.tsx` and `Invoices.tsx` memoised their query strings on filter state only. A cold load that rendered before settings arrived built UTC day bounds and never rebuilt them. (The comment "Recomputed each render" in UsageEvents was not true; it was a `useMemo`.)

Scenario: tenant timezone `America/New_York`, hard reload of `/usage` (default Last 30 days) at Oct 16 2026 22:00 EDT (= Oct 17 02:00Z).

| | `from=` sent |
|---|---|
| Before | `2026-09-17T00:00:00.000Z` (UTC civil day, kept for the life of the page) |
| After | first request may still use the UTC bound; once settings load the window recomputes and requests `2026-09-16T04:00:00.000Z` |

Same class on `/invoices?dateFrom=…&dateTo=…` opened in a new tab.

Fix (minimal):

- `web-v2/src/hooks/useTenantTimezone.ts`: returns `settings.timezone` from the shared `['settings']` query (same key as the bootstrap, so no extra request), falling back to the module value, then `'UTC'`. Because it is React state, memos can depend on it. Reading the module var is not enough: the bootstrap sets it in a `useEffect`, after the render in which the page sees the new query data.
- `web-v2/src/lib/usageRange.ts`: new pure `windowQueryBounds(range, custom, tz, now?)` returns the civil window and its UTC start/end instants in `tz`.
- `UsageEvents.tsx`: memo is `windowQueryBounds(range, {from,to}, tz)` with deps `[range, filterFrom, filterTo, tz]`; `filterQs` uses the instants.
- `Invoices.tsx`: passes `tz` to `startOfDayInTZ` / `endOfDayInTZ` and adds `tz` to the memo deps.
- `civilDateAgo` gains an optional `now` argument (default unchanged) so the window can be tested at a fixed instant.

Not changed (same class, not in this PR's scope): `AuditLog.tsx` (~284, 357) and `MarginCard.tsx` (~46) call `startOfDayInTZ`/`endOfDayInTZ` without an explicit zone. AuditLog builds its query string inline on each render, so it recomputes only if something else re-renders it after settings load.

### Test

`web-v2/tests/usageRange.test.ts`:

- `windowQueryBounds: last 30 days for a New York tenant starts at New York midnight` (now = 2026-10-17T02:00Z → `from` 2026-09-16 / `2026-09-16T04:00:00.000Z`; the UTC fallback gives `2026-09-17T00:00:00.000Z`)
- `windowQueryBounds: custom dates become start and end of day in the given zone` (Asia/Kolkata)

What the test does **not** prove: the bug itself is a missing memo dependency. The node test runner cannot render React, so no automated test fails if `tz` is dropped from the deps array. The test locks the pure bounds per zone; the recompute-on-load wiring is covered only by the new `[ ]` MANUAL_TEST box (FLOW TZ1).

### Mutation check

Made `windowQueryBounds` ignore `timezone` (use the module fallback, as the page did before settings loaded):

```
not ok 14 - windowQueryBounds: last 30 days for a New York tenant starts at New York midnight
  expected: '2026-09-16'
  actual: '2026-09-17'
```

Restored → 15/15 pass (also under `TZ=America/Los_Angeles`).

### Checks

- `npx tsc -b`: pass
- `npx eslint` on the 6 changed files: 0 errors, 2 warnings (existing `customers`/`meters` useMemo warnings in UsageEvents.tsx, lines not touched)
- `npm test`: 235 pass, 0 fail (also under `TZ=America/Los_Angeles`)
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. Settings → timezone `America/New_York`. Open a new tab on `/usage` with DevTools → Network open; hard reload (throttle to "Slow 4G" to widen the window before settings load).
2. Filter Network by `usage-events`: the **last** list request and the `aggregate` request carry `from=<30 days ago>T04:00:00.000Z` (EDT) — not `T00:00:00.000Z`.
3. Open `/invoices?dateFrom=2026-09-16&dateTo=2026-09-30` in a new tab: the last `invoices` request has `from=2026-09-16T04:00:00.000Z&to=2026-10-01T03:59:59.999Z`.
4. Control: a UTC tenant shows `T00:00:00.000Z` bounds.

## Checklist

- [x] Tests cover the new behavior (unit, pure bounds; wiring is manual, see above)
- [ ] `go test ./... -race -short -count=1` passes — no Go changed; not run
- [ ] `make lint` passes — no Go changed; not run
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW TZ1, new `[ ]` box)
- [ ] `make gen` re-run — n/a
- [x] Frontend touched? `npx tsc -b`, `npm test`, `eslint` on changed files pass (`npm run build` not run separately)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
