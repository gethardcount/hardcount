## What & why

**Depends on #1089** (`feat/usage-cards-billing-period-default`, ADR-121): merged into this branch first; review that PR first. Base: `feat/app-shell`.

Resolves **M-CUST-15** and **M-X-11** (customer page part). The customer page took a currency per card: the credit balance used billing profile → invoices → workspace default; Activity's usage total, Margin's usage figure and the New invoice dialog used the workspace default. Wayne (EUR plan, no profile, no invoice) read **$0.00** for credit balance, usage and margin, and the composer opened in USD, while Upcoming showed €45.00.

- `web-v2/src/lib/customerCurrency.ts`: `resolveCustomerCurrency` — live subscription's plan (period started last, the same "primary" pick as `GET /v1/customers/{id}/usage`) → billing-profile currency → newest invoice → workspace default. Exported, with `customerCurrencyNote` for provenance text. Pure, tested.
- `web-v2/src/hooks/useCustomerCurrency.ts`: feeds it from shared query keys (`customer-subs-upcoming`, `plans`, `customer-billing-profile`, `customer-overview`) so the page makes no extra requests. Also `useCustomerSubscriptions` (server-filtered `?customer_id=`); the page previously filtered the unfiltered first page client-side, which misses subscriptions past the newest 50.
- CustomerDetail passes the currency to CostDashboard (empty-window total), MarginCard (usage figure when the report names no revenue currency) and the New invoice dialog default; credit balance and grant table use it.
- Provider cost stays in its own currency (USD), unchanged.

| | Before | After |
|---|---|---|
| Wayne credit balance | $0.00 | €0.00 |
| Wayne Activity "Billed usage" (empty cycle) | $0.00 | €0.00 |
| Wayne Margin "Usage billed" | $0.00 | €0.00 |
| New invoice currency for Wayne | USD | EUR |

## Evidence

Screenshots in `/tmp/claude-0/ux/build-customers/currency/` (before = main on :5173, after = this branch on :5181):
- `before-wayne__light.png`: strip reads "Credit Balance $0.00".
- `after-wayne__light.png` / `__dark.png` / `__mobile.png`: Credit Balance €0.00, Activity €0.00, Margin "Usage billed €0.00" with provider cost $0.0000; dark and 390px render the same values.
- `after-newinvoice__light.png`: dialog opens with "€ EUR — Euro", column "UNIT (€)".

## Tests

- `web-v2/tests/customerCurrency.test.ts` (6 cases). Mutation checks:
  - disable the subscription step → 3 fail (`not ok 1 - a customer on a EUR plan with no profile and no invoice is shown in EUR`, also 2 and 4);
  - count `canceled` as live → `not ok 3 - a canceled subscription does not set the currency`.

## Checklist

- [x] Tests cover the new behavior
- [x] No Go files changed (Go checks not affected)
- [x] `CHANGELOG.md` entry (Fixed)
- [x] `MANUAL_TEST.md`: new FLOW CU10 "Customer page money figures" (`[x]` with the automated test); `make lint-manual-test` OK
- [x] Frontend: `npx tsc -b` clean; `npx eslint` on changed files 0 errors (1 pre-existing warning, `react-hooks/incompatible-library` at the billing-profile form); `npm test` 256 pass; `npm run build` OK

## Money path only

Display only: no writer, effect-firer or state change. Gated readers of the currency choice touched: credit balance + grants table, Activity empty total, Margin usage figure, New invoice default currency (the operator still picks; the server validates).

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
