## What & why

Resolves **M-HOME-1**, **M-HOME-2**, **M-HOME-3** and the verifier's **NEW-1** / **NEW-2**. Every Home trend now compares two numbers the server computed with one definition.

- **Failed payments** compared against a hard-coded `0` (`Dashboard.tsx:237`). The overview now returns `failed_payments_prev` (same predicate, previous window), and the card hint says "K in the previous 30 days".
- **Active customers** counted customer records (incl. a customer with only a canceled sub and one with no sub), and its baseline was `active - new`, which read **-1** on today's data (shown as "↑ 800%"). The card is now **Customers**: customers with at least one active or trialing subscription (`live_customers`), compared with the same count at the window start (`live_customers_prev`, rebuilt from `started_at`/`activated_at`/`canceled_at`, like `mrr_prev`). Hovering the card shows the definition.
- **Trialing** was always 0: the query matched `status='active' AND trial_end_at > now()`, but trials have `status='trialing'`. Fixed; the openapi description that documented the bug is corrected.
- **MRR sparkline** was drawn from revenue buckets. Dropped (no MRR history exists yet).

API (additive): `live_customers`, `live_customers_prev`, `failed_payments_prev`. `active_customers` keeps its meaning (the onboarding checklist reads it) and is now documented. `trialing_subscriptions` changes value (bug fix, CHANGELOG "API:" note).

Base: `feat/app-shell`. Related: #1084 / #1071 also touch `Dashboard.tsx` (chart only; no overlap with these lines). #1070 touches other analytics files, not `overview.go`.

## Before / after

| | Before (main, :5173) | After (this branch, :5185 + this branch's API on :8185) |
|---|---|---|
| Customers card | "ACTIVE CUSTOMERS 7 ↑800% · +8 new · 4 subs" | "CUSTOMERS 5 — · 4 active · 1 trialing subscriptions" |
| Failed payments | "0 — 0.0% · 30 days" (vs hard-coded 0) | "0 — 0.0% · 0 in the previous 30 days" |
| Trialing | `trialing_subscriptions: 0` with initech-trial trialing | `1` |
| MRR sparkline | drawn from revenue | none |

Live API check on the dev tenant: non-simulated subs are 4 active + 1 trialing across 5 customers → `live_customers: 5`, `active_subscriptions: 4`, `trialing_subscriptions: 1`.

## Evidence

Screens in `/tmp/claude-0/ux/build-admin/home-numbers/` (`before-*` from :5173, `after-*` from :5185; light, dark, 390px; plus 90-day tab). I looked at each: after-light shows "CUSTOMERS 5" with "4 active · 1 trialing subscriptions" and Failed payments "0 in the previous 30 days"; before-light shows "ACTIVE CUSTOMERS 7 ↑ 800%". Dark and mobile render the same values; the mobile Recent invoices row wraps as before (pre-existing, fixed in the Home redesign PR).

## Tests

- `internal/analytics/overview_home_integration_test.go` `TestOverview_HomeCountsAndBaselines` (real Postgres): seven customers (active, trialing, canceled in window, no sub, two subs, draft, canceled before window) plus a simulated trialing customer; failed invoices in both windows.
- `web-v2/tests/homeKpis.test.ts`: baselines map to server fields and are never negative.
- Mutation checks (each reverted after):
  - old trialing predicate → `trialing_subscriptions: got 0, want 1`
  - `COUNT(customer_id)` without DISTINCT → `live_customers: got 4, want 3`
  - prev without the cancel check → `live_customers_prev: got 4, want 3`
  - prev without the started bound → `live_customers_prev: got 4, want 3`
  - `failed_payments_prev` on the current window → `failed_payments_prev: got 1, want 2`
  - web: `previous: 0` → `not ok - failed payments compare against the previous window from the server`; no clamp → `not ok - a baseline is never negative`

## Checklist

- [x] Tests cover the new behavior (unit + real-Postgres integration)
- [x] `go test ./... -short -count=1` passes; `go test -race -short ./internal/analytics/...` passes; `flock … go test -p 1 -short=false ./internal/analytics/` passes
- [x] gofmt clean, `go vet ./...` clean, `golangci-lint run ./internal/analytics/...` 0 issues, `make lint-clock lint-tz lint-funding-set lint-manual-test` OK
- [x] `CHANGELOG.md` entry (Fixed, with API note)
- [x] `MANUAL_TEST.md` FLOW U1 KPI box rewritten (`[ ]`) + automated box (`[x]`)
- [x] `make gen` re-run, generated files committed, no drift
- [x] Web: `tsc -b`, eslint (changed files) 0 errors, `npm test` 246 pass, `npm run build` OK

## Money path only

Read-only analytics: no writer, effect-firer or state transition changes. The new queries only read `subscriptions` and `invoices` inside the existing RLS tenant tx, with the ADR-086 simulated-data fragments (`notSimSubBare`, `notSimInvoice`); `TestOverview_ExcludesSimulatedData` still deep-equals the whole response.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
