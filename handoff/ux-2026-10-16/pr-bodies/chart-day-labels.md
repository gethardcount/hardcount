## What & why

**Chart bars keep their own day west of UTC.**

The server buckets chart data by UTC calendar day or month and sends each bucket's key as that day's name:

- Revenue chart (`internal/analytics/revenue_chart.go`): `to_char(date_trunc(...), 'YYYY-MM-DD' | 'YYYY-MM')` → `"2026-10-15"`, or `"2026-10"` on the 12m tab.
- Customer usage chart (`internal/usage/postgres.go` `date_trunc('day', timestamp AT TIME ZONE 'UTC')`, `composeDailyBuckets`): `bucket_start` = the UTC midnight that starts the day, `"2026-10-01T00:00:00Z"`.

The dashboard treated these keys as instants and formatted them in the tenant timezone. West of UTC every label moved one day back.

Scenario (tenant TZ `America/New_York`):

| Surface | Bucket key | Before | After |
|---|---|---|---|
| Dashboard revenue, 30d | `2026-10-15` | Oct 14 | Oct 15 |
| Dashboard revenue, 12m | `2026-10` | Sep 30 | Oct 2026 |
| Customer Activity "Daily usage" axis | `2026-10-01T00:00:00Z` | Sep 30 | Oct 1 |
| Same, tooltip | `2026-10-01T00:00:00Z` | Wed, Sep 30 | Thu, Oct 1 |

Fix: new `web-v2/src/lib/chartLabels.ts` (`utcBucketLabel`, `revenueBucketLabel`) labels a bucket by its own UTC calendar date. Month keys read `MMM yyyy`. `Dashboard.tsx` (`formatShortDate` removed) and `CostDashboard.tsx` (`UsageChart` axis + tooltip) use it.

Not changed: server bucketing (which days are grouped is a product decision) and the "N days" badge.

### Test and mutation check

`web-v2/tests/chartLabels.test.ts` (4 tests). Mutation: replaced `'UTC'` in `utcBucketLabel` with `'America/New_York'` (the old tenant-TZ conversion) → 3 of 4 fail:

```
not ok 1 - revenue day bucket "2026-10-15" reads Oct 15, not Oct 14   (actual: 'Oct 14')
not ok 2 - revenue month bucket "2026-10" reads Oct 2026, not Sep 30   (actual: 'Sep 2026')
not ok 3 - usage bucket at UTC midnight Oct 1 reads Oct 1, not Sep 30  (actual: 'Sep 30')
```

Restored → 4/4 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint` on the 4 changed/added TS files: pass
- `npm test`: 237/237 pass
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. **Dashboard**, tenant TZ `America/New_York`, 30d tab, with a paid invoice whose `paid_at` is Oct 15 UTC: the x-axis tick and hover tooltip for that point read "Oct 15".
2. **Dashboard**, 12m tab: ticks read "Nov 2025 … Oct 2026"; check the ticks do not crowd or overlap at desktop and 390px width (the label is now longer than "Sep 30").
3. **Customer page → Activity card → "Daily usage" chart**, tenant TZ `America/New_York`, usage on Oct 1 UTC: the bar's axis tick reads "Oct 1"; tooltip header "Thu, Oct 1".
4. Same, tenant TZ `Asia/Kolkata`: labels unchanged from before (east of UTC the conversion already gave the same day).

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go touched; not run)
- [ ] `make lint` passes (no Go touched; not run)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed (FLOW TZ1, pending box)
- [x] Frontend touched? `npx tsc -b && npm test && npx eslint <changed files>` passes (full `npm run build` / `npm run lint` not run)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
