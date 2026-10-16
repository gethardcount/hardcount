# docs: make time-window and clock statements match the code

## What & why

A truth PR: each statement below disagreed with the code. Every claim was re-derived from the code before editing. No behaviour changes. **There is no failing test, because nothing executable changed.** Only comments and Markdown changed.

| Where | Was | Code says | Now |
|---|---|---|---|
| `MANUAL_TEST.md` FLOW TC4 (box "Operator dashboard surfaces follow the clock") | Activity 7/30/90 and Margin "Last 30 days" show "(non-zero totals)" after a full-cycle advance | `rollingWindow(days, now)` = `[now − N×24h, now + 1 ms]`. With usage on day 1 and the clock +32 d (`scripts/demo.sh`), the 7d and 30d windows exclude it and read $0 | Observable: post an untimestamped event at a mid-period clock time, advance; a preset reaching back to it is non-zero, one that does not is $0; period-start usage is $0 in 7d/30d after +32 d. `[~]` → `[ ]` |
| `MANUAL_TEST.md` FLOW TZ1 (inclusive period box) + annotation | Period shows on "the PDF / hosted invoice / portal"; "hosted page computes the same client-side" | `hostedinvoice.viewInvoice` has no period field; `HostedInvoice.tsx` renders only Due/Received/Voided. The hosted and portal **PDFs** do show it (`BuildPDFContext` stamps `BillingPeriodDisplay`) | "every invoice PDF: downloaded, emailed, and the PDF linked from the hosted page and the portal". A dated correction note was added to the annotation; the original evidence was kept. `[~]` → `[ ]` |
| `internal/invoice/pdf.go` ~455, `internal/domain/invoice.go` ~309 comments | "PDF, hosted page, and dashboard all show" the period string | as above | The hosted page itself has no Period row; its linked PDF does |
| `MANUAL_TEST.md` ~2325 Margin picker evidence | `to=2026-08-05T08:03:03Z` (`to` = now) | `rollingWindow` ends at now + 1 ms, ISO with ms | Evidence kept, with a note that current code sends `to=…T08:03:03.001Z` |
| `docs/ops/runbook.md` §15 Fix | "compare the customer's usage for the period (`GET /v1/customers/{id}/usage`)" | No `from`/`to` → `resolvePeriod` uses the primary active sub's **current** cycle (the next one after close). Canceled sub: `filterActiveSubs` → none → `422 customer_has_no_subscription` (bare) or `meters: []` (with a window) | Command with `?from=$BILLING_PERIOD_START&to=$BILLING_PERIOD_END`; for a canceled sub, `GET /v1/usage-events?customer_id&meter_id&from&to` (`from` inclusive, `to` exclusive, `timestamp >= from AND timestamp < to` in `usage/postgres.go`) |
| `docs/ops/runbook.md` §16 Check | customer usage call without a window, framed as "this period" | same | Note: a WARN at period close concerns the closed period; add that invoice's `from`/`to`; canceled → usage-events |
| `docs/ai-pricing.md` ~78 | unmatched row via `GET /v1/customers/{id}/usage` | default is the current cycle | "The API call covers the current cycle unless you pass `from` and `to`" |
| `deploy/compose/README.md` ~126 | `VELOX_BASE=http://localhost ./scripts/demo.sh …` | `demo.sh` prints `${VELOX_DASHBOARD_URL:-http://localhost:5173}`; compose serves the dashboard on port 80 (`quickstart.sh` sets `VELOX_DASHBOARD_URL`) | Command sets `VELOX_DASHBOARD_URL=http://localhost` and says why |
| `internal/integrations/litellm/payload.go` ~99 | "falls back to time.Now if zero" | `mapper.go` ~184-201 leaves the timestamp nil; `usage.Service` stamps the customer's effective now (test clock frozen_time or wall clock) | Comment says that |
| `internal/usage/summary.go` ~26 | "aggregates usage … in the current billing period" | handler defaults to the UTC calendar month of the customer's effective now | Comment says that, and that it is not the billing period |
| `internal/usage/customer_usage.go` ~103 | Buckets: "Sums match Meters[]" | buckets are raw `SUM(quantity)` per meter including unmatched usage (`AggregateDailyBuckets`); `Meters[]` uses the meter's aggregation and excludes the unmatched row | Comment says they need not add up |

Not edited: ADR bodies (writing-style: do not rewrite records).

**CHANGELOG:** one entry under Fixed, because the runbook's §15/§16 procedure changed meaning (an operator following it compared the wrong cycle) and the compose README command changed. The code-comment and MANUAL_TEST edits need no entry.

### Checks

- `gofmt -l` on the 5 Go files: empty
- `go vet ./internal/domain/... ./internal/integrations/litellm/... ./internal/invoice/... ./internal/usage/...`: OK
- `golangci-lint run …`: **could not run** — the installed binary is built with go1.25 and refuses the repo's Go 1.26.9 target (`can't load config: the Go language version (go1.25) used to build golangci-lint is lower than the targeted Go version (1.26.9)`). Comment-only Go changes.
- `make lint-clock lint-tz lint-funding-set lint-manual-test`: all OK
- `go test ./... -short -count=1`: exit 0 (47 packages ok, no FAIL)
- Integration (`flock … go test -p 1 -count=1 -short=false`): `./internal/usage/...`, `./internal/integrations/litellm/...`, `./internal/invoice/...`, `./internal/domain/...`: all ok
- `vale` and `lychee` are not installed here; not run. The new runbook anchor `#15-invoice-timing-and-late-usage` was checked against the heading `### 15. Invoice timing and late usage` by hand.

## Checklist

- [ ] Tests cover the new behavior — docs only; no executable change, no test
- [x] `go test ./... -short -count=1` passes (without `-race`)
- [ ] `make lint` passes — golangci-lint could not run (version mismatch, above)
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (two boxes reopened, one evidence note)
- [ ] `make gen` re-run — n/a
- [ ] Frontend touched? — n/a

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
