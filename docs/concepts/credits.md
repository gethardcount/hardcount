# Credits

Credit is money a customer can spend on future invoices, such as prepaid
credit they bought or free credit you gave them. This page covers how
credit is added, the order in which it pays invoices, when it expires, and
how your app reads what is left.

Think of a gift card with several top-ups. Each top-up is its own block,
and each invoice spends from the blocks in a fixed order.

## The credit ledger

Each customer's credit is a ledger: a list of entries, one for each change.

| Entry | What it is | Who writes it |
|---|---|---|
| `grant` | Credit added: a block that invoices can spend. | You, or Hardcount for a prepaid commit, a plan change or a credit note |
| `usage` | Credit an invoice spent. | Hardcount |
| `expiry` | The unspent part of a block that passed its expiry date. | Hardcount |
| `adjustment` | A correction, up or down. | You |

The balance is kept per currency, and credit pays only invoices in its own
currency. A customer with USD and EUR credit has two balances, which are
never added together or converted.

Read them with:

- `GET /v1/credits/balance/{customer_id}`: the settled balance per
  currency.
- `GET /v1/credits/ledger/{customer_id}`: every entry.
- `GET /v1/credits/grants/{customer_id}`: each block, with what is used,
  what is left and when it expires.

## Adding credit

There are three ways:

- **A grant:** `POST /v1/credits/grant` with `customer_id`, `currency`,
  `amount_cents` (up to 100,000,000, so $1,000,000), a `description`, and
  optionally `expires_at` and `"grant_kind": "promotional"`. Use it for
  goodwill credit or free promotional credit.
- **A prepaid commit:** the customer pays up front on a one-off invoice
  and gets credit when it is issued. See [Prepaid commits](#prepaid-commits).
- **An adjustment:** `POST /v1/credits/adjust` with a positive or negative
  `amount_cents`. An adjustment down that is larger than the balance is
  refused with `422`.

Hardcount also adds credit itself: for the unused part of a period paid in
advance, after a downgrade or a cancel, and for a credit note that gives
credit to the balance.

## The order credit is spent

When an invoice spends credit, it takes from the blocks in this order:

1. **Promotional credit first**, so free credit is used before any other
   credit.
2. **Then the block that expires soonest.** A block with no expiry comes
   last.
3. **Then the oldest block.**

Only blocks in the invoice's currency count, and a block past its expiry
is skipped.

## When credit pays an invoice

Credit pays an invoice right after it is issued, before any card charge:

- It lowers `amount_due_cents`, and `credits_applied_cents` shows by how
  much. The invoice's total does not change.
- It pays the whole amount due, tax included.
- If credit covers it all, the invoice is marked `paid` and no card is
  charged.
- Each payment retry uses any credit the customer got since the last try.
  So credit you add later can pay an older unpaid invoice.
- An invoice that sells a prepaid commit is never paid with credit.

## Expiry

A block with an `expires_at` cannot be spent after that time. Hardcount's
billing run writes an `expiry` entry for its unspent part. A block past its
date is never spent, even before that entry is written.

## Credit given back

When a void, or a credit note on an unpaid invoice, gives back credit the
invoice spent, the credit goes back to the blocks it came from, with their
expiry and kind. A share whose block has already expired expires at once.
A credit note on a paid invoice always raises the balance by its full
credit amount.

## Prepaid commits

A prepaid commit is credit the customer pays for up front, such as "pay
$9,000, get $10,000 of credit". You sell it as a line on a one-off invoice:

- `unit_amount_cents` is what the customer pays;
- `commit_granted_cents` is the credit they get, and can be more;
- `commit_expires_at`, if you set it, is when the credit expires.

When the invoice is finalized, the customer gets a block of kind `commit`.
One invoice can hold one commit line. The purchase is not taxed; the usage
the credit pays for is. Voiding the purchase invoice takes back what is left
of the credit.

More: [Sell prepaid credit](../ai-pricing.md#sell-prepaid-credit-a-prepaid-commit),
with every call, and [tax on prepaid credit](../ai-pricing.md#tax-on-prepaid-credit).

## Worked example: $10,000 of credit, one month of usage

A customer buys a commit: they pay $9,000 and get $10,000 of credit, with
no expiry. You also grant them $500 of promotional credit that expires on
December 31. Their USD blocks:

| Block | Kind | Amount | Expires |
|---|---|---|---|
| 1 | `commit` | $10,000.00 | never |
| 2 | `promotional` | $500.00 | December 31 |

In July they use $1,200.00 of tokens. With 10% tax, the July invoice is
$1,200.00 + $120.00 = $1,320.00. It is issued on August 1 and spends
credit in order:

1. Block 2 first, because it is promotional: $500.00. Nothing is left in
   it.
2. Then block 1: $1,320.00 − $500.00 = $820.00. $9,180.00 is left.

The invoice shows `credits_applied_cents` 132,000 and `amount_due_cents`
0, so it is paid and no card is charged. The customer's balance is now
$10,500.00 − $1,320.00 = $9,180.00.

If you void that July invoice later, its $1,320.00 goes back: $500.00 to
block 2, still expiring on December 31, and $820.00 to block 1.

## Your app's view: the live balance

The settled balance changes only when an invoice, grant, adjustment or
expiry is written. Usage not invoiced yet does not lower it. To know what a
customer can still spend right now, read the live balance:
`GET /v1/credits/balance/{customer_id}/live`. It subtracts the period's
usage not yet invoiced.

To stop serving a customer whose credit has run out, see
[Stop usage when credit runs out](../stop-usage-when-credit-runs-out.md).

## Webhooks

| Event | When |
|---|---|
| `credit.balance_low` | A currency's balance falls below the low-balance threshold in your settings. With no threshold set, it is never sent. |
| `credit.balance_depleted` | A currency's balance reaches 0. |
| `credit.balance_recovered` | A currency's balance goes back above 0. |
| `credit.commit_retired` | A credit note took back commit credit. |

These follow the settled balance, so they can come weeks after the live
balance reached 0. Use them for top-up reminders, not to stop usage.

More: payloads in the [webhook event catalog](../webhooks.md#credit-datacredit_balance).

**Next:** the payments that settle what credit does not cover:
[Payments](payments.md).
