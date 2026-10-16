# fix(dashboard): revenue chart zero-fills days with no paid invoices

## What & why

`GET /v1/analytics/revenue-chart` (`internal/analytics/revenue_chart.go`) groups paid invoices with `GROUP BY date_trunc(...)` and returns only the buckets that had revenue. `Dashboard.tsx` plotted them on a category axis with a monotone area. With payments on **Sep 20** and **Oct 15** only, the 30-day chart showed two neighbouring points joined by a slope, as if revenue ran between them. The customer usage chart already zero-fills by design (`internal/usage/customer_usage.go`).

Fix: `fillRevenueBuckets(data, period, nowMs)` in `web-v2/src/lib/revenueChart.ts`.

- Window mirrors `analytics.parsePeriod`: `[now − N days, now)` in UTC, N = 7 / 30 / 90 / 365, day buckets for 7d/30d/90d and month buckets for 12m. Unknown period → 30d, like the server.
- Keys match the SQL's `to_char` format (`YYYY-MM-DD` / `YYYY-MM`). The response carries only `period` (the normalized key), not the window bounds, so the client recomputes the window. `nowMs` is the query's `dataUpdatedAt` (fetch time), the closest match to the server's `time.Now()`.
- Server points outside the computed window (browser and server clocks differ) are kept, so no revenue is dropped. Zero-fill adds no revenue (test asserts the sum).
- Dashboard: the chart uses the filled series. The empty state still keys off the raw data, so a period with no payments still reads "No paid USD invoices in this period."

Assumption to flag: `date_trunc` on `paid_at` (timestamptz) runs in the DB session zone. Nothing in `internal/platform/postgres` sets it, so it is the server default (UTC in the shipped compose/Postgres images). If a deployment sets a non-UTC Postgres `TimeZone`, server buckets would be that zone's days; zero-fill still keeps every server point, but grid days could be off by one at the edges.

Not changed:

- **MRR card sparkline** (`sparklineRevenue`, last 12 raw revenue points) still plots only days with revenue. Whether it should be zero-filled, or be an MRR series at all, is a product question.
- X-axis labels: `formatShortDate` parses `YYYY-MM-DD` as UTC midnight and formats in the tenant zone, so a western tenant sees the previous day's label. That is the separate `fix/chart-day-labels` branch; the two branches touch nearby lines in `Dashboard.tsx` and may need a trivial rebase.

### Test

`web-v2/tests/revenueChart.test.ts` (5 tests): 30d has 31 buckets Sep 16 … Oct 16, with Sep 20 and Oct 15 25 buckets apart and zeros between; 7d/90d sizes; 12m has 13 month buckets Oct 2025 … Oct 2026; an out-of-window server bucket is kept; unknown period → 30d.

### Mutation check

Made the helper return the server data unchanged (the pre-fix behaviour):

```
not ok 1 - 30d: every day in the window is a bucket, missing days are zero
  expected: 31
  actual: 2
```

(3 of 5 fail.) Restored → 5/5 pass.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/pages/Dashboard.tsx src/lib/revenueChart.ts tests/revenueChart.test.ts`: clean
- `npm test`: 238 pass, 0 fail (also under `TZ=America/Los_Angeles`)
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. A tenant with two paid invoices in the workspace currency, `paid_at` on two days about 25 days apart within the last 30 days (not on a test clock: the chart excludes simulated invoices).
2. Dashboard, period **30 days**: the Revenue area sits on $0 for the days between the two payments, with two separate peaks. Hover a gap day: tooltip shows "$0.00".
3. Switch to **7 days**, **90 days**: one slot per day, zeros where nothing was paid. **12 months**: one slot per month.
4. Control: a tenant with no paid invoices still shows "No paid USD invoices in this period." (no flat zero line).
5. Check the MRR card sparkline looks as before.

## Checklist

- [x] Tests cover the new behavior (unit)
- [ ] `go test ./... -race -short -count=1` passes — no Go changed; not run
- [ ] `make lint` passes — no Go changed; not run
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW U1, new `[ ]` box)
- [ ] `make gen` re-run — n/a
- [x] Frontend touched? `npx tsc -b`, `npm test`, `eslint` on changed files pass (`npm run build` not run separately)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
