## What & why

The Usage page reported numbers that contradict billing (resolves **M-USG-1**, **M-USG-2**; the Usage-page part of **M-CUST-8**; UC1, UC2, E2E-21):

- Every meter was a plain `SUM(quantity)`. The Storage meter bills on its **Maximum**, yet it showed 550 GB (140 + 210 + 120 + 80) across customers and months.
- "Total Units" added tokens, calls and GB into one number (842,540), and the bars put different units on one scale.

**Server (additive API change).** `GET /v1/usage-events/aggregate` rows gain:
- `value`: the meter's usage under its own aggregation. `sum` → sum; `count` → number of events; `max` → each customer's peak in the window, added across customers; `last` → each customer's latest reading (ties broken by `id DESC`, the order billing uses), added across customers.
- `aggregation`, `events`, `customers`, `peak` (highest single reading) and `daily` (one value per calendar day with events, same mode rules).
- New `tz` query param buckets `daily` by IANA zone (default UTC). An unknown zone is a 422, not a silent fall-back to UTC.
- `total` (plain sum) and `total_units` are unchanged for compatibility; the spec now says `total_units` mixes units.

The handler reads each meter's mode through a new narrow `usage.MeterLister` (wired to `pricing.Service` in `router.go`), so `usage` does not import `pricing`. The store does the per-customer roll-up in one query and returns all candidates; the service picks one per mode.

**Dashboard.** Total Units is gone. The strip reads Events / Meters with usage / Customers with usage. "Usage by meter" is one row per meter: name (links to the meter) with its aggregation badge, events · customers, the value in the meter's unit, a caption saying what the number is ("Peak reading per customer, added across 2 customers"), and a daily sparkline over the selected window (tenant timezone, zero-filled). The section description says that Maximum and Latest value take one reading per customer **in this window**, while billing takes one per billing period, so a multi-month window is never presented as a bill.

| | Before | After |
|---|---|---|
| Storage (Maximum), All time | 550 GB | 350 GB — "Peak reading per customer, added across 2 customers" (Northwind 140 + Globex peak 210) |
| Cross-meter total | Total Units 842,540 | removed |
| Breakdown | one bar scale for tokens, calls, GB | one row per meter in its own unit, with a daily line |

## Evidence (screenshots looked at)

`/tmp/claude-0/ux/build-catalog/usage-totals/{before,after}/` — `usage` (All time) and `usage-storage` (meter = Storage), each light, dark and 390px.
- After, light: Storage row reads **350 GB**, "Maximum" badge, "4 events · 2 customers", caption names the peak-per-customer rule; API calls reads 243,000 calls with a spiky daily line; Tokens (events on a single day) shows no line rather than a dot.
- After, dark: badges and lines readable, no raw palette colours.
- After, 390px: the KPI strip is 2+1; each meter row stacks (name, value, line); nothing overflows (`main` scroll width = client width).
- Before (main, :5173): "Total Units 842,540" and Storage 550 GB.

## Tests

- `internal/usage/aggregate_integration_test.go` `TestAggregateFollowsMeterMode` (real Postgres): max / last / count / unknown meters across two customers and two Berlin days, including an event at 23:30 UTC that belongs to the next Berlin day. Existing `TestAggregateRespectsListFilters` still passes (`total`, ordering, RLS).
- `internal/usage/handler_4xx_test.go` `TestUsageHandler_Aggregate_UnknownTZ_Returns422`.
- `web-v2/tests/usageSummary.test.ts`: captions per mode, zero-filled days, window cap, sparkline geometry.

**Mutation checks (each went red, then restored):**
- `pick()` returning the sum for `max` → `aggregate_integration_test.go:275: storage max: value 550, want 290`.
- Ignoring `tz` (always UTC) → `aggregate_integration_test.go:309: daily … 2026-03-01: got 2, want 1`.
- `fillDays` not zero-filling quiet days → `not ok 4 - fillDays zero-fills quiet days across the window` (3 tests failed).

## Checks

- gofmt clean · `go vet ./...` clean · `golangci-lint run ./internal/usage/... ./internal/api/...` 0 issues
- `make lint-clock lint-tz lint-funding-set lint-manual-test` all ok
- `go test ./... -short -count=1` all ok
- Integration: `./internal/usage/...` ok. `./internal/api/` has one failure, `TestE2E_ReferencedIDsAreTenantAndModeScoped`: its webhook-endpoint seed is refused because this sandbox cannot resolve `example.com` ("no address was found for example.com"). It is unrelated to this change.
- `make gen` run, generated files committed.
- Web: `npx tsc -b` clean; `npx eslint` 0 errors (2 existing warnings in UsageEvents.tsx); `npm test` 249/249; `npm run build` ok.

## Checklist

- [x] Tests cover the new behavior (unit + real-Postgres integration)
- [x] `go test ./... -short -count=1` passes
- [x] golangci-lint passes on touched packages
- [x] `CHANGELOG.md` entry added (Fixed, with an `API:` note)
- [x] `MANUAL_TEST.md` FLOW U3 rewritten (Total Units boxes replaced; the meter-mode box is `[x]` with its tests)
- [x] `make gen` re-run
- [x] Frontend: build, test, lint pass

Read-only reporting change: no money state is written, so there is no money-path site-set. Billing (`AggregateForBillingPeriodByAgg`) is untouched; the page now uses the same modes and the same `last` tie-break.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
