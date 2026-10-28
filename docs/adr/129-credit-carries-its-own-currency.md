# ADR-129: A credit carries its own currency and pays only invoices in it

**Date:** 2026-10-23
**Status:** Accepted
**Amends:** [ADR-100](100-currency-coherence-guard-ring.md) (closes its
"Credit-ledger currency column" deferral and the manual-invoice pin gap;
supersedes its 2026-10-22 amendment), [ADR-078](078-prepaid-commits-phase-1.md)
§D8 (balance events are per currency).

## Summary

Every credit ledger row stores its currency. A credit pays only invoices in
that currency and is never converted. A customer with credit in two
currencies has two balances, and no figure adds them together. A manual
invoice uses the customer's currency: an explicit currency must agree with the
billing profile and every live subscription's plan, and an empty one is taken
from them. Design and review: velox-ops `design-credit-currency-2026-10-23.md`.

## Context

Before this, the ledger had no currency column. The drain summed a customer's
whole ledger and paid any invoice with it, one cent for one cent. ADR-100's
guards keep a customer's live subscriptions in one currency, but two routes
still moved credit across currencies:

1. **Manual invoices were not pinned.** A EUR invoice for a USD customer
   drew the customer's USD credit. A EUR prepaid-commit invoice paid in cash
   minted a commit grant that then drained against the customer's USD cycle
   invoices.
2. **Currency changes.** A canceled USD subscription's unpaid invoices beside
   a new EUR subscription passed ADR-100's guard (it reads non-terminal
   subscriptions only). The USD credit then paid EUR invoices, and EUR credit
   paid the old USD ones.

The 2026-10-22 amendment to ADR-100 made the dashboard show a balance in a
currency derived at read time. That fixed the symbol, not the money.

## Decision

1. **Each ledger row stores its currency** (`customer_credit_ledger.currency`,
   NOT NULL, `CHECK (currency ~ '^[A-Z]{3}$')`, migrations 00199/00200). The
   store's append is the chokepoint: it uppercases the currency and refuses a
   row without one with a plain error, since that is a caller bug.
2. **Every writer names the currency from a stored fact.** An operator grant
   or adjustment takes a required, allowlist-validated `currency`. A
   credit-note grant takes the invoice's currency, whether the note pays out
   to the balance (paid invoice) or returns credits applied to it (unpaid
   invoice). A commit grant takes its funding invoice's. An expiry entry and a
   commit retire take the grant's. A void reversal takes the currency of the
   usage rows it reverses and refuses rows in two currencies.
3. **The drain is per currency.** `ApplyToInvoiceAtomic` reads
   `UPPER(i.currency)` from the locked invoice row (invoices never change
   currency after insert) and sums, drains and writes only that currency's
   rows. A deduction draws only on its currency's blocks.
4. **Balances, events and reads are per currency.** `balance_after` is the
   balance in the row's currency. The balance-crossing events
   (`credit.balance_low`, `_depleted`, `_recovered`) and `credit.commit_retired`
   carry `currency`, and a crossing is computed on that currency's balance.
   The balance, live balance, grants rollup, cost dashboard and analytics
   overview return one entry per currency.
5. **A same-source entry in another currency is a mismatch**, not a replay
   (`credit_source_amount_mismatch`).
6. **Manual invoice currency** (ADR-100 G1/G2 grafts, `invoice.Service.Create`):
   an explicit currency that differs from the billing-profile currency or any
   non-terminal subscription's plan currency is refused with 409
   `customer_currency_conflict`. An empty one takes the profile's, then the
   plan currency, then the workspace default, the order the engine stamps a
   cycle invoice. A customer with neither fact may be invoiced in any allowed
   currency.
7. **Three doctor checks**: `balance_exceeds_block_capacity` is per currency;
   `ledger_currency_matches_invoice` and `drained_block_currency_matches` are
   new.

## Rejected alternative: one pinned credit currency per customer

A `credit_currency` on the customer, set by the first grant, with ledger rows
left without a currency. Rejected:

- It adds a race: a USD first grant and a EUR subscription create in two
  transactions both commit, and the pin can never be undone, so the USD
  credit is stranded.
- It blocks money. A pin missing while the balance is above zero fails every
  collector; a credit note on a paid invoice in another currency fails at
  Issue; a draft commit invoice in another currency fails at finalize.
- Ledger history would be right only while the pin never changes.
- Stripe, Lago and Orb all put the currency on the credit (Orb also pins the
  customer, and still stores a currency on each ledger entry).

## Consequences

- **Accepted: stranded credit.** A grant in a currency the customer is not
  billed in, or credit left after the customer moved to another currency,
  stays as its own balance. No invoice draws it. The operator deducts it or
  refunds it outside Hardcount. Nothing converts it.
- **Accepted: one low threshold for every currency.** The tenant's
  `credit_balance_low_threshold_cents` applies in each currency's own units
  (1000 is $10 or €10). Every allowed currency has two decimals. A
  per-currency threshold waits until a tenant asks.
- **Accepted: the expiry hold ignores currency.** `domain.CreditDrawOwedSQL`
  can hold a USD grant past its expiry while a EUR draw is owed. The drain
  filters by both currency and expiry, so no wrong money moves; only the
  write-off and its webhook arrive later.
- **Accepted: a credit note on an old invoice** in the customer's previous
  currency credits that currency. It is the currency of the money refunded;
  the operator can refund to the card instead.
- **Deferred (ADR-100's TOCTOU):** the manual-invoice check reads the profile
  and subscriptions in their own transactions, so a concurrent currency change
  can pass it. Credit money stays safe because the drain is per currency.
  Named fix: a customer-scoped `pg_advisory_xact_lock`; trigger: the first
  multi-operator tenant.
- **Breaking, pre-launch:** `GET /v1/credits/balance/{id}` returns
  `{data: [...]}`; the live balance returns `balances[]`; the grants rollup is
  `remaining_by_currency[]`; analytics returns `credit_balance_by_currency`
  instead of `credit_balance_total`; the cost dashboard's `credits` is an
  array; grant and adjust require `currency`.
- **Dev data:** 00199 gave each customer's existing rows the one currency a
  balance was read in before (profile, live plan, newest invoice, workspace
  default). History really in another currency is relabeled, and
  `ledger_currency_matches_invoice` reports it.

## References

- Stripe: [customer balance transactions](https://docs.stripe.com/api/customer_balance_transactions/create)
  ("currency (enum, required) … Specifies the invoice_credit_balance that this
  transaction will apply to") and the [customer object](https://docs.stripe.com/api/customers/object)
  ("Stripe only applies a balance in a specific currency to an invoice after
  that invoice (which is in the same currency) finalizes").
- Lago: [create a wallet](https://getlago.com/docs/api-reference/wallets/create)
  and [prepaid credits](https://getlago.com/docs/guide/wallet-and-prepaid-credits/overview)
  ("Prepaid credits only apply to invoices sharing the same currency as the
  wallet").
- Orb: [update customer](https://docs.withorb.com/api-reference/customer/update-customer)
  and [create ledger entry](https://docs.withorb.com/api-reference/credit/create-ledger-entry).
- Code: `internal/credit/postgres.go` (`appendEntryInTx`,
  `drainPositiveBlocks`, `ApplyToInvoiceAtomic`, `AdjustAtomicAudited`),
  `internal/invoice/service.go` (`resolveCurrency`),
  `internal/doctor/checks.go`.
- Tests: `internal/credit/currency_integration_test.go`,
  `internal/api/credit_currency_integration_test.go`,
  `TestCollect_CreditInAnotherCurrencyIsNotApplied` (billing),
  `TestDunningRetry_AppliesOnlyTheInvoiceCurrency` (api),
  `TestCreditLedgerCurrency_BackfillValidateAndRollback` (migrate).
