## What & why

**A date-only `to` filter now includes the last second of the named day.**

`internal/api/timefilter` turned `to=YYYY-MM-DD` into `23:59:59.000 UTC`. Postgres `timestamptz` keeps microseconds, so every consumer that compares `<= to` dropped rows stamped `23:59:59.000001`–`.999999` on that day.

Scenario: a usage event backfilled at `2026-09-30T23:59:59.4Z`, then
`GET /v1/exports/usage-events.csv?from=2026-09-01&to=2026-09-30`.

| | `to` parsed as | rows in the file |
|---|---|---|
| Before | `2026-09-30T23:59:59Z` | none (the event is missing) |
| After | `2026-09-30T23:59:59.999999Z` | the 23:59:59.4 event; the `2026-10-01T00:00:00Z` event stays out |

Fix: `to` = `23:59:59.999999 UTC` (`999_999_000` ns). Not `.999999999`: Postgres cannot store nanoseconds, so the value would be truncated or rounded into the next day depending on encoding.

Docs made true (no behaviour change):

- The package doc said "every consumer's SQL is inclusive on both ends". It now lists both groups:
  - inclusive `<= to`: invoice list + `invoices.csv`, `customers.csv`, `subscriptions.csv`, `usage-events.csv`, audit log + `audit-log.csv` (`created_at` and `sim_effective_at`);
  - exclusive `< to`: usage summary (`usage.buildUsageWhere`) and customer usage (`usage.AggregateByPricingRules`).
- The doc said errors "surface a 400"; `errs.Invalid` → `respond.FromError` answers **422**. Fixed.
- `api/openapi.yaml`: `to` descriptions for customer usage (now also says "exclusive", which matches its `<` SQL), usage summary, invoice list and audit log name `23:59:59.999999`. `make gen` re-run.

Not changed: `<` vs `<=` across consumers (product decision). Noted, not fixed: the dashboard's own `endOfDayInTZ` (`web-v2/src/lib/dates.ts`) sends `…23:59:59.999` (milliseconds), so dashboard-originated filters still miss `.999001`–`.999999`. Out of scope for this PR.

### Tests

- `TestParseRange_DateOnlyToIsLastMicrosecondOfDay` (unit): asserts `to.Nanosecond() == 999999000`.
- `TestStreamForExport_DateOnlyToIncludesLastSecondOfDay` (real Postgres): parses the request with `timefilter.ParseRange`, streams `usage-events.csv` rows, expects exactly the 23:59:59.4 event.

Mutation check (fix reverted to `23, 59, 59, 0`):

```
timefilter_test.go:131: to: got 2026-10-31T23:59:59Z want 2026-10-31T23:59:59.999999Z
export_end_of_day_integration_test.go:61: exported timestamps for to=2026-09-30: got [], want exactly [2026-09-30T23:59:59.4Z] (to parsed as 2026-09-30T23:59:59Z)
```

Both pass with the fix restored.

### Checks

- `gofmt -l` clean; `go vet` clean; `golangci-lint` v2.12.2 (CI's pin) on `./internal/api/timefilter/... ./internal/usage/...`: 0 issues.
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: pass.
- Integration: `./internal/usage/...`, `./internal/api/timefilter/...`, `./internal/audit/...` pass. `./internal/api/` has one failure, `TestE2E_ReferencedIDsAreTenantAndModeScoped` (webhook URL `example.com` cannot be resolved in this sandbox, no DNS); it fails the same way on `main`.
- `make gen`: only expected comment drift (Go + TS generated files), committed.
- web-v2 (generated files only): `npx tsc -b` ok, `npm test` 0 failures.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [x] `go test ./... -short -count=1` passes (run without `-race`)
- [x] `make lint` passes (golangci-lint v2.12.2 on the touched packages)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated: new FLOW EX box, auto-locked; EX1's `to` anchor text corrected and downgraded to `[ ]`
- [x] `make gen` re-run — required if `api/openapi.yaml` changed
- [x] Frontend touched? Generated files only: `tsc -b` and `npm test` pass

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
