## What & why

**Dashboard charts bucket by UTC day on any Postgres.**

`internal/analytics/handler.go` promises UTC periods. The bucket queries were `to_char(date_trunc($1, <timestamptz>), $2)`, and both functions read the **session TimeZone**. Nothing sets that zone, so the buckets followed the server's `TimeZone` setting. Sites:

- `revenue_chart.go` (`paid_at`);
- `usage.go` (`timestamp`);
- `mrr_movement.go` (`activated_at`, `canceled_at`, `changed_at`).

Scenario: Postgres session TimeZone `America/New_York`. A payment, a usage event and a subscription activation all happen at `2026-10-16T02:00:00Z`.

| Endpoint (`period=30d`) | Before | After |
|---|---|---|
| `/v1/analytics/revenue-chart` | bucket `2026-10-15` | `2026-10-16` |
| `/v1/analytics/usage` | `2026-10-15` | `2026-10-16` |
| `/v1/analytics/mrr-movement` (new) | `2026-10-15` | `2026-10-16` |

Fix: `date_trunc($1, <col> AT TIME ZONE 'UTC')`, at all five sites. `AT TIME ZONE 'UTC'` on a `timestamptz` gives a UTC wall-clock `timestamp` (no zone). `date_trunc` and `to_char` on that type never consult the session zone, so the rendered `YYYY-MM-DD` / `YYYY-MM` string is UTC too. The schema check confirms all five columns are `timestamp with time zone`, so the conversion runs in the intended direction. This matches `usage/postgres.go`'s explicit `AT TIME ZONE 'UTC'`. The WHERE filters are unchanged, so index use is unchanged.

OpenAPI: the three bucket `date` descriptions said "in the database server's time zone (UTC by default)". They now say "in UTC". `make gen` re-run.

### Tests

`TestAnalyticsBuckets_AreUTCWhateverTheSessionTimeZone` (real Postgres) seeds data with the normal test DB. It then runs the handlers on a second app-role pool limited to one connection, with `SET TimeZone = 'America/New_York'`. A control asserts that `SHOW TimeZone` inside the handler's own `TxTenant` tx reads `America/New_York`. The instant is 02:00Z on the latest UTC day already past, so it falls in the trailing window.

Mutation check (the three source files reverted):

```
revenue-chart: got [{Date:2026-10-15 RevenueCents:12300 InvoiceCount:1}], want one bucket 2026-10-16 ...
usage: got [{Date:2026-10-15 Events:1 Quantity:7}], want one bucket 2026-10-16 ...
mrr-movement: got [{Date:2026-10-15 New:1000 ...}], want one bucket 2026-10-16 ...
```

Passes with the fix restored. Not individually locked: the churned (`canceled_at`) and expansion/contraction (`changed_at`) bucket expressions. They received the identical edit.

### Checks

- `gofmt -l` clean; `go vet` clean; `golangci-lint` v2.12.2 (CI's pin) on `./internal/analytics/...`: 0 issues.
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: pass.
- `go test -p 1 -count=1 -short=false ./internal/analytics/...`: pass.
- `make gen`: expected comment drift only, committed.
- web-v2 (generated files only): `npx tsc -b` ok; `npm test` 233 pass, 0 fail.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [x] `go test ./... -short -count=1` passes (run without `-race`)
- [x] `make lint` passes (golangci-lint v2.12.2 on the touched package)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated: FLOW U1 box, auto-locked
- [x] `make gen` re-run — required if `api/openapi.yaml` changed
- [x] Frontend touched? Generated files only: `tsc -b` and `npm test` pass

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
