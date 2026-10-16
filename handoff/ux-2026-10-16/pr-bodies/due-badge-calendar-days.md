## What & why

**The invoice due badge agrees with the due date.**

`DueBadge` used `daysUntil` (`effectiveNow.ts`, `Math.ceil((due − now) / 24h)`). `due_at` is issue time + N days, so it carries the issue hour. The invoice page prints the date beside the badge as `formatDate(invoice.due_at)` (tenant TZ), and InvoiceAttention says the deadline is "this calendar date".

Scenario (tenant TZ UTC, `due_at = 2026-10-16T20:16Z`, page shows "Due Oct 16, 2026"):

| Now | Before | After |
|---|---|---|
| Oct 16 09:00 | Due in 1d | Due today |
| Oct 16 21:00 | Due today (ceil gave −0) | Due today |
| Oct 17 19:00 | Due today | Past due 1d |
| Sep 16 01:00 | Due in 31d | Due in 30d |

Fix: `calendarDaysUntil(iso, now: EffectiveNow, tz?)` in `web-v2/src/lib/dates.ts` returns the difference between the civil dates of `now` and `iso` in tenant TZ (the zone `formatDate` prints the Due date in). `DueBadge` uses it with the same `EffectiveNow` anchor (test clocks unchanged). `daysUntil` stays for `ExpiryBadge` (API keys); its comment now says so.

Zone choice: the adjacent date on the invoice page is `formatDate(invoice.due_at)` with no zone → live tenant TZ, so the badge uses tenant TZ too. The invoice's `billing_timezone` is not used, because the page does not print the date in it. The two differ only after an org re-zones.

Not changed (product questions):
- Whether "past due" means the instant or the date. The Past due tab (`internal/invoice/postgres.go`, `due_at < now()`) lists the invoice from 20:16 on Oct 16 while the badge reads "Due today" until midnight. Before, the two disagreed the other way (badge "Due today" for most of Oct 17).
- Interplay with the open branch `fix/due-date-tenant-calendar` (Net N due dates counted in the billing TZ): if `due_at` lands at civil midnight, the server calls it past due from 00:00 on the printed due date, while the badge reads "Due today" that whole day. Same product question.

### Test and mutation check

New `web-v2/tests/dueBadgeDays.test.ts` (5 tests). Mutation: replaced the body with the old `Math.ceil((t − now) / 86_400_000)` → 4 of 5 fail:

```
not ok 1 - on the due date the count is 0 all day            (expected 0, actual 1)
not ok 2 - the day after the due date is 1 day past due      (expected -1, actual -0)
not ok 3 - days before the due date count whole calendar days (expected 30, actual 31)
```

Restored → 5/5 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/lib/effectiveNow.ts src/components/DueBadge.tsx tests/dueBadgeDays.test.ts`: pass
- `npm test`: 238/238 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Invoice page**, finalized invoice, clock-pinned customer, `due_at` Oct 16 20:16 UTC, tenant TZ UTC: clock at Oct 16 09:00 → header shows "Due Oct 16, 2026" with an amber **Due today** badge.
2. Same invoice, clock at Oct 17 19:00 → red **Past due 1d**.
3. **Invoices list**, same invoice row: the badge text matches the invoice page at the same clock time.
4. Tenant TZ `America/New_York`, wall-clock invoice due the next local day: badge reads **Due in 1d** all day, regardless of the hour.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (FLOW U11, next to the Past due tab box; pending)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
