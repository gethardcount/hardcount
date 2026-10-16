## What & why

Stacked on #1095. Resolves **M-CUST-16**.

Stark: a $500 grant was fully applied to VLX-000006. The customer page read Credit Balance $0.00; the "Credit grants" card was gated on grants with something left (`listCreditGrants` without `include_exhausted`), so the grant vanished; Outstanding $4,160 sat beside the $4,660 invoice with no word about the credit.

- `components/customer/CustomerCreditsSection.tsx` (replaces the old "Credit grants" card; built on `Section`, reused as the Credits tab in the redesign PR): balance + "Granted · Used · Expired" totals from `GET /v1/credits/balance/{id}` (`total_granted/total_used/total_expired` were already returned), commit/promotional split, all grants via `include_exhausted=true` with **Used up** / **Expired** badges (finished grants behind "Show N used-up or expired grants"), 5 latest ledger entries ("Applied" instead of the ledger's "usage"), **Full ledger** link, **Grant credits**.
- `pages/Credits.tsx`: `CreditDialog` exported, plus an optional `currency` prop for the amount label and confirm (cross-area file, 3-line additive change).
- Strip credit stat: totals line. Recent invoices: "$500.00 credit applied · $4,160.00 due". Outstanding: "Amount due after credit applied" when a recent invoice had credit.
- `lib/customerCredits.ts`: pure helpers (`creditTotalsLine`, `grantState`, `splitGrants`, `invoiceCreditNote`, `ledgerEntryLabel`).

**Flow "grant credits": before 6 steps → after 4 steps** (before: customer → Credit Balance link → /credits → Grant → form → confirm; after: Grant credits → form → Grant → confirm). The confirm stays: a grant moves money onto the next invoice.

| | Before | After |
|---|---|---|
| Stark credit stat | $0.00 | $0.00 · Granted $500.00 · Used $500.00 |
| Used-up $500 grant | not shown | row with "Used up" behind a toggle |
| Outstanding vs invoice | $4,160 vs $4,660, unexplained | invoice row "$500.00 credit applied · $4,160.00 due" |
| Grant credits | only on /credits | on the customer page, in the customer's currency |

## Evidence (`/tmp/claude-0/ux/build-customers/credits/`)

- `before-stark__light.png`: no grants card, Credit Balance $0.00.
- `after-stark-top__light.png`: strip "Granted $500.00 · Used $500.00", Outstanding "$4,160.00 · 1 invoice / Amount due after credit applied".
- `after-stark-credits__light.png` / `__dark.png` / `__mobile.png` (toggle opened): grant row with "Used up", latest entries −$500.00 "Applied" and +$500.00 "Grant"; at 390px the grants table scrolls inside its box.
- `after-grant-dialog__light.png`: Wayne (EUR) → dialog labelled "Amount (€)". Nothing submitted.

## Tests

`web-v2/tests/customerCredits.test.ts` (6). Mutation: gate the totals on balance instead of granted → `not ok 1 - a fully used grant still shows in the totals`.

## Checklist

- [x] No Go change
- [x] CHANGELOG (Fixed); MANUAL_TEST CU10: 1 `[x]` (test named), 2 new `[ ]`; `make lint-manual-test` OK
- [x] tsc clean; eslint 0 errors (3 pre-existing warnings: Credits.tsx `customers` memo deps and two `incompatible-library`); `npm test` 266 pass; build OK

## Money path only

Display plus one existing writer reused: `POST /v1/credits/grant` through the unchanged `CreditDialog` (idempotency key per open, confirm step kept). After success `invalidateMoneySurfaces` refreshes balance, grants (`['credit-grants', id, 'all']` is under the invalidated prefix), the recent-ledger query (keyed under `['credits']`), overview and invoices. No server change.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
