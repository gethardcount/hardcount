## What & why

**A prepaid-credit expiry of tomorrow is accepted in the evening.**

Customer page → New invoice → a line with "This line sells prepaid credits" and an expiry date. `validate()` in `CustomerDetail.tsx` checked:

```ts
const today = new Date(now).toISOString().slice(0, 10)   // UTC date
if (l.commitExpiresAt <= today) → "Credit expiry must be a future date"
else commitExpires = endOfDayInTZ(l.commitExpiresAt)      // tenant TZ
```

The check used the UTC date; the expiry is resolved in the tenant timezone.

Scenario (org TZ `America/New_York`, now 2026-10-16 21:00 EDT = 2026-10-17T01:00Z):

| Picked expiry | Before | After |
|---|---|---|
| 2026-10-17 (tomorrow locally) | refused, "Credit expiry must be a future date" | accepted, sent as 2026-10-18T03:59:59.999Z |
| 2026-10-16 (today) | refused | refused |

East of UTC the old check was looser: in Kolkata at 01:30 on Oct 17 (UTC still Oct 16) it accepted Oct 17, which is today there. It now refuses that, as its message says. The server (`internal/invoice/service.go` `!CommitExpiresAt.After(clock.Now)`) would have accepted it, since end of day is still ahead; nothing bad was written either way.

Fix: `isAfterCivilToday(ymd, nowMs, tz?)` in `web-v2/src/lib/dates.ts` compares against `formatYMDInTZ` of the customer's effective now (the test-clock anchor `useEffectiveNow(customer?.test_clock_id)` is unchanged).

### Test and mutation check

`web-v2/tests/dates.test.ts` › `isAfterCivilToday uses the tenant date, not the UTC date`. Mutation: restored the `toISOString().slice(0, 10)` comparison inside the helper →

```
not ok 4 - isAfterCivilToday uses the tenant date, not the UTC date
  expected: true
  actual: false
```

Restored → 4/4 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/pages/CustomerDetail.tsx tests/dates.test.ts`: pass (0 errors; 1 pre-existing `react-hooks/incompatible-library` warning at CustomerDetail.tsx:1407, not in changed lines)
- `npm test`: 234/234 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Customer page → New invoice dialog**, org TZ `America/New_York`, browser time after 8 PM EDT: add a line, tick "This line sells prepaid credits", set granted cents and expiry = tomorrow → Save draft creates the draft; no red "Credit expiry must be a future date" under the line.
2. Same dialog, expiry = today → the red error still shows under the line.
3. Same dialog on a clock-pinned customer: today/tomorrow follow the clock's frozen date.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (credits flow, next to the ADR-086 expiry box; pending)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

## Money path only

Credits. Client-side validation only; the value written (`commit_expires_at = endOfDayInTZ(date)`) is unchanged for every date that passed before. Site-set for the "is this expiry in the future" decision: this composer check (changed); the Grant Credits dialog (uses the DatePicker floor, not this code; not changed); the server's `> now` check on grant/commit creation (authoritative, not changed).

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
