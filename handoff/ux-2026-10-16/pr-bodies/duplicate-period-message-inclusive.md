## What & why

**The duplicate-period refusal now names the period the way invoices show it.** `mapInvoiceUniqueViolation` (`internal/invoice/postgres.go` ~154) builds the `invoice_billing_period_taken` message. It printed the stored half-open period, which ends on the next period's start. Every other surface shows that period with an inclusive last day, using `domain.FormatInclusivePeriod`: the dashboard, PDF, hosted page and API `billing_period_display`.

| | Message |
|---|---|
| Before | `invoice already exists for subscription "sub_x" period 2026-11-01..2026-12-01` |
| After | `invoice already exists for subscription "sub_x" period Nov 1, 2026 – Nov 30, 2026` |

Change:

- The period is formatted with `domain.FormatInclusivePeriod` in `LoadLocationOrUTC(inv.BillingTimezone)`.
- `FormatInclusivePeriod` returns `""` for a zero-length period. In that case the message names the single day.
- The error code `invoice_billing_period_taken` and field `billing_period` are unchanged. No caller or test matches on the old text (grep).

**Which timezone.** The mapper only has the row being inserted, not the conflicting row. Both rows have the same subscription and period. The org zone is stamped on each at write (ADR-077), so the inserted row's zone stands in for the conflicting row's. The old code used the same zone, and the fallback for an empty zone stays UTC.

Limit: if the org changed its timezone between the two writes, the message uses the new zone. Reading the conflicting row would need a new transaction after the failed insert. I judged that out of scope for an error message.

### Tests

`internal/invoice/postgres_unique_violation_test.go` `TestMapInvoiceUniqueViolation_BillingPeriodMessageIsInclusive` covers three cases:

- a New York period with a stamped zone;
- a legacy row with no zone (UTC);
- a zero-length period.

Mutation check (restored `postgres.go` from origin/main, ran, restored):

```
got  "invoice already exists for subscription \"sub_x\" period 2026-11-01..2026-12-01"
want "invoice already exists for subscription \"sub_x\" period Nov 1, 2026 – Nov 30, 2026"
got  "invoice already exists for subscription \"sub_x\" period 2026-11-01..2026-11-01"
```

### Checks run

- `gofmt -l internal/ cmd/`: empty.
- `go vet ./internal/invoice/...`: ok.
- `golangci-lint run ./internal/invoice/...` (v2.12.2, the CI pin): `0 issues.`
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: all ok.
- `go test -p 1 -count=1 -short=false ./internal/invoice/...` (real Postgres): ok.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (ran without `-race`; passes)
- [x] `make lint` passes (golangci-lint v2.12.2 on touched packages: 0 issues)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — new `[ ]` box after "Run billing twice in same period" (the 409 over HTTP is not walked; the test locks the message text)

## Money path only

Message text only. No state, writer or effect changes. The site-set for this message, from
`grep -rn "mapInvoiceUniqueViolation\|invoice_billing_period_taken" internal --include=*.go`:

- [x] Builder: `invoice/postgres.go` `mapInvoiceUniqueViolation` (the only site that builds this message).
- [x] Callers: `PostgresStore.Create` (~349) and the Tx create (~2132). Both pass the row being inserted.
- [x] Readers that branch on the error: the engine's `errors.Is(err, errs.ErrAlreadyExists)` idempotent-skip paths (`billing/engine.go` ~3524 and others). They match on the kind, not the text, so they are unaffected.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
