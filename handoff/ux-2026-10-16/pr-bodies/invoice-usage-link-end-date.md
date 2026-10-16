## What & why

**A cancel invoice's "view usage" link includes the cancel day.**

The invoice page links each usage line to `/usage?customer=…&meter=…&range=custom&from=…&to=…`. The contract (FLOW I1, `dates.ts` comment) is that the link shows what the invoice billed. `to` came from `inclusiveEndCivilYMD`, which always steps back one civil day from the half-open period end.

That is right for a cycle ending at civil midnight. A cancel invoice ends at the cancel instant (`internal/billing/engine.go` `BillingPeriodEnd: canceledAt`; its usage lines use the same instant), so the step-back drops billed usage.

Scenario (invoice `billing_timezone` UTC):

| Period | Before | After |
|---|---|---|
| Cycle `2026-10-01T00:00Z` → `2026-11-01T00:00Z` | from=2026-10-01&to=2026-10-31 | unchanged |
| Cancel `2026-10-01T00:00Z` → `2026-10-16T15:00Z` | to=2026-10-15 (Oct 16 00:00–15:00 billed events missing) | to=2026-10-16 |
| Same-day cancel `2026-10-16T00:00Z` → `2026-10-16T14:23Z` | from=2026-10-16&to=2026-10-15 → "No events match" | from=2026-10-16&to=2026-10-16 |

Fix: `usageFilterEndYMD(startISO, endISO, tz)` in `web-v2/src/lib/dates.ts` replaces `inclusiveEndCivilYMD` (its only caller was this link). It steps back a day only when the end is exactly 00:00:00.000 in the invoice's billing timezone; otherwise it keeps the end's own civil date; it never returns a date before the start's civil date.

Not changed: the displayed period text (`formatCivilPeriod` / Go `InclusiveDisplayEnd`). For the cancel case above it still reads "Oct 1, 2026 – Oct 15, 2026"; whether a partly covered last day should be shown is a product question.

Known limits, not changed here:
- The filter is day-grained, so a mid-day end now also covers the rest of that day (Oct 16 15:00–24:00), which the invoice did not bill. This is the smaller error than dropping billed usage, and the link is already a superset for multi-rule meters.
- The link's dates are computed in `invoice.billing_timezone`, but the Usage page converts them with the live tenant timezone (`Usage.tsx` `startOfDayInTZ`/`endOfDayInTZ` with no zone). These differ only after an org re-zones. Pre-existing.

### Test and mutation check

Three new tests in `web-v2/tests/dates.test.ts` (`usageFilterEndYMD` …). Mutation: made the helper return the old always-step-back value → 2 fail:

```
not ok 5 - usageFilterEndYMD keeps the end day of a mid-day cancel invoice   (expected '2026-10-16', actual '2026-10-15')
not ok 6 - usageFilterEndYMD never ends before the start (same-day cancel)  (expected '2026-10-16', actual '2026-10-15')
```

Restored → 6/6 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/pages/InvoiceDetail.tsx tests/dates.test.ts`: pass (0 errors; 1 pre-existing `react-hooks/exhaustive-deps` warning at InvoiceDetail.tsx:244, not in changed lines)
- `npm test`: 236/236 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Invoice page** of a final invoice from an immediate cancel at Oct 16 15:00 (org TZ UTC) with usage on Oct 16: hover the usage line's "View … events" link → URL ends `from=2026-10-01&to=2026-10-16`; follow it → the Usage page's custom range shows Oct 1 – Oct 16 and lists the Oct 16 events.
2. Same, for a same-day cancel: the Usage page shows Oct 16 – Oct 16 with the events, not "No events match".
3. A normal cycle invoice (Oct 1 → Nov 1): link still ends `to=2026-10-31`.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (FLOW I1, pending box)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
