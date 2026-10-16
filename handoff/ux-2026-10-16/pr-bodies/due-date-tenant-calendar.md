## What & why

**"Net N" due dates now count N calendar days in the billing timezone.** Every due date was `now.AddDate(0, 0, N)` on a UTC instant, which adds N×24 hours. When the span crosses a DST change, the due instant lands one hour off civil midnight in the tenant zone and prints a day early.

Scenario (America/New_York tenant, Net 30):

| | `due_at` | In New York | PDF / dashboard |
|---|---|---|---|
| Cycle closes 2026-11-01 00:00 EDT (`2026-11-01T04:00Z`), before | `2026-12-01T04:00:00Z` | Nov 30 23:00 EST | "Due November 30, 2026" |
| After | `2026-12-01T05:00:00Z` | Dec 1 00:00 EST | "Due December 1, 2026" |

Change:

- New `domain.AddDaysIn(t, n, loc)`: `t.In(loc).AddDate(0, 0, n).UTC()`, nil loc = UTC.
- All seven `due_at` writers use it with the location already resolved at that site (list below).
- The `addIntervalIn` comment in `internal/domain/subscription.go` claimed "Day-grade adds (AddDate(0,0,N) for trial/due dates) are TZ-invariant". That is wrong and is corrected.
- Trial-end code is not touched here (separate branch `fix/trial-end-tenant-calendar`, which carries a copy of the same helper; whichever merges second rebases and drops the duplicate).

Outside a DST change the result is unchanged (same wall-clock time in the zone = same instant). Net 0 ("Due on receipt") still gives `due_at == issued_at`. UTC tenants are unchanged.

Not done: no backfill of existing `due_at` values. Rows already issued keep their stored due date.

### Tests

- `internal/domain/add_days_in_test.go` `TestAddDaysIn_CrossesDSTOnCivilCalendar`: fall-back (Nov 1 + 30 → `2026-12-01T05:00Z`), spring-forward (Mar 1 + 14 → `2026-03-15T04:00Z`), no-DST, zero days, nil loc.
- `internal/billing/due_date_tenant_calendar_test.go` `TestRunCycle_NetTermsDueDate_UsesTenantCalendarAcrossDST`: engine cycle close for the scenario above.
- `internal/invoice/due_date_tenant_calendar_test.go` `TestManualInvoice_NetTermsDueDate_UsesTenantCalendarAcrossDST`: manual invoice Create and Finalize at Nov 1 00:00 EDT.

Mutation check (reverted the engine cycle-close site and both invoice-service sites to `AddDate(0, 0, N)`, ran, restored):

```
due_date_tenant_calendar_test.go:74: due_at = 2026-12-01T04:00:00Z (Nov 30 23:00 EST in NY), want 2026-12-01T05:00:00Z (Dec 1 00:00 EST)
due_date_tenant_calendar_test.go:78: printed due date = "November 30, 2026", want "December 1, 2026"
due_date_tenant_calendar_test.go:55: finalized due_at = 2026-12-01 04:00:00 +0000 UTC, want 2026-12-01T05:00:00Z (Dec 1 00:00 EST)
```

### Checks run

- `gofmt -l internal/ cmd/`: empty.
- `go vet ./internal/domain/... ./internal/billing/... ./internal/invoice/... ./internal/subscription/...`: ok.
- `golangci-lint run` (v2.12.2, the CI pin, built with go1.26) on the same packages: `0 issues.`
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: all ok.
- `go test -p 1 -count=1 -short=false` on domain, billing, invoice, subscription (real Postgres): all ok.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (ran without `-race`; passes)
- [x] `make lint` passes (golangci-lint v2.12.2 on touched packages: 0 issues)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — FLOW TZ1, new `[x]` box after the P24 "Due on receipt" box

## Money path only

State: `invoices.due_at` (and so the dunning/overdue/attention readers that branch on it). Site-set from
`grep -rn "AddDate(0, 0\|DueAt\|due_at\|NetPaymentTermDays\|FinalizeWithDates" internal --include=*.go`:

**Writers (compute due_at) — all now `domain.AddDaysIn(…, loc)`:**

- [x] `internal/billing/engine.go` `billOnePeriod` (cycle close), loc = `billingLoc` (`e.tenantLocation`)
- [x] `internal/billing/engine.go` `BillOnCreate` (day-1 in-advance invoice), loc = `billingLoc`
- [x] `internal/billing/engine.go` final invoice on immediate cancel, loc = `billingLoc`
- [x] `internal/billing/threshold_scan.go` `fireThreshold`, loc = `billingLoc`
- [x] `internal/subscription/handler.go` item-proration invoice, loc = `billingLoc` (`h.tenantLoc`)
- [x] `internal/invoice/service.go` `Create` (manual draft), loc = `s.invoiceLocation(ctx, tenantID)` (tenant TZ, UTC on read failure, same as the period display)
- [x] `internal/invoice/service.go` `Finalize` re-stamp for operator-composed invoices, loc = `s.invoiceDisplayLoc(ctx, inv)` (the invoice's stamped `billing_timezone`, else tenant TZ)

**Store writes (persist the computed value, no date math):** `internal/invoice/postgres.go` Create insert (~331), Tx insert (~2112), `FinalizeWithDates` (~773). No SQL or migration computes `due_at`.

**No other day-grade add writes `due_at`:** remaining `AddDate(0, 0, …)` hits are analytics/usage report windows and trial end (`internal/subscription/service.go:796`, separate branch).

**Gated readers (unchanged, read the stored instant):** overdue filter `postgres.go` (~3952, `due_at < now()`), `domain/invoice_attention.go` `DueBy`, PDF `pdf.go` ~452 (renders in `billing_timezone`), hosted invoice `hostedinvoice/handler.go`, CSV export `api/exports.go`. On DST-crossing invoices they see a due instant one hour different (later in autumn, earlier in spring), which is the fix.

**Effect-firers / crash points:** none added. The change is a pure value computation inside existing writes; no new tx, effect or Stripe call.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
