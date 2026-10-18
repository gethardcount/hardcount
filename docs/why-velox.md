# Why Velox exists

Velox exists for teams that charge by usage, most of all AI products that
charge per token. It is built around four needs. Three of them Stripe covers
only through a hosted product, or not at all. The fourth is what every
billing system is judged on.

| Need | What Stripe's basic usage billing does | What Velox does for you |
|---|---|---|
| 1. Price each AI model and token type | Needs a separate meter for each model and token type | One meter prices them all |
| 2. Sell prepaid credit (a commit) | Has prepaid credit grants, but no commits | A commit is one line on an invoice |
| 3. Keep billing records on your own servers | Stores usage and invoices on Stripe's servers | Usage and invoices stay in your own Postgres |
| 4. Answer "why is my bill this much?" | Not compared: every billing system needs this | Keeps every raw event and the exact price used |

If none of these match you, see [What Velox is not](../README.md#is-velox-right-for-you).

## The gaps that made us build it

Small AI teams kept falling into the same five gaps. Each one below names
what we found, with its source, and what Velox does instead. Where another
project is ahead, [How Velox compares](comparison.md) says so.

1. **Stripe Billing takes a cut, and can't price by model on one meter.**
   Its pay-as-you-go plan charges 0.7% of billing volume
   ([Stripe pricing](https://stripe.com/billing/pricing), checked
   2026-10-17). Its basic usage billing has no dimensional pricing, so a
   different rate per model and token type needs a meter for each. For
   that, Stripe points you to Metronome, its main usage-billing product
   ([Stripe's comparison](https://docs.stripe.com/billing/subscriptions/usage-based/compare-metronome)).
   **Velox instead:** does the billing itself and asks Stripe only to charge
   the card, so there is no Stripe Billing fee. Stripe still charges its
   card fee, and Stripe Tax's fee if you use Stripe Tax. One meter carries
   model and token type.

2. **Open-source engines hold back key parts behind a paid licence.** In
   Lago, automatic failed-payment retries
   ([docs](https://getlago.com/docs/guide/dunning/automatic-dunning)) and
   invoice previews ([docs](https://getlago.com/docs/guide/invoicing/previews))
   need a premium licence. In Flexprice, the billing, invoice and
   subscription services sit under an
   [enterprise licence](https://github.com/flexprice/flexprice/blob/1ede2d98c81e9e565c087cc373aaf18bfd6d8d4f/internal/ee/LICENSE).
   Kill Bill's usage aggregation and wallets are in its paid add-on, Aviate
   ([sources](comparison.md)).
   **Velox instead:** one MIT licence with nothing held back. Failed-payment
   retries, credit notes, refunds, invoice previews and the audit log are
   all in it.

3. **They are heavy to run yourself.** Flexprice's default setup runs nine
   services, including Kafka, ClickHouse and Temporal
   ([docker-compose.yml](https://github.com/flexprice/flexprice/blob/1ede2d98c81e9e565c087cc373aaf18bfd6d8d4f/docker-compose.yml)).
   OpenMeter's runs eleven and calls itself "not a production topology"
   ([sources](comparison.md)). Across these projects, more than 30 people
   have reported a fresh install failing before the first invoice, often at
   sign-up, for example [lago#514](https://github.com/getlago/lago/issues/514),
   [lago#368](https://github.com/getlago/lago/issues/368) and
   [flexprice#819](https://github.com/flexprice/flexprice/issues/819).
   Kill Bill is the exception: it runs fewer services than Velox.
   **Velox instead:** one server program plus Postgres and Redis, with a web
   dashboard. You can try it on your laptop in about five minutes.

4. **There is no way to fast-forward time to test a billing change.** We
   found no test clock in Lago's or Flexprice's docs or source. Kill Bill
   has one clock for the whole server ([sources](comparison.md)).
   **Velox instead:** test clocks for test customers. Pin a test customer to
   a clock and move it forward up to a year at a time, so months of billing
   run in minutes, in test mode on your normal install.

5. **You can't see which customers lose you money.** AI costs move with
   usage, so a customer can cost you more than they pay.
   **Velox instead:** margin per customer. Velox records what each request
   cost you, from your provider price table or LiteLLM's reported cost,
   next to what you billed.

Velox is young: pre-1.0, one maintainer, Stripe and cards only, no SDKs
yet.

## 1. AI products price each model and token type differently

AI products charge a different price for each model, and for each kind of
token. A meter is a counter for one kind of usage, and dimensions are labels
on each usage event.

- Real model pricing today is `model × token_type × tier`. The token type
  alone has five kinds that never overlap: `input`, `output`, `cache_read`,
  `cache_write_5m` and `cache_write_1h`.
- Stripe's basic usage-based billing does not price by dimension, so each
  model and token type needs its own meter. Stripe's
  [own comparison](https://docs.stripe.com/billing/subscriptions/usage-based/compare-metronome)
  points to Metronome, its hosted product, for that.
- Velox puts the labels on the event and lets one meter carry them all.
- If you already run a [LiteLLM](integrations/litellm.md) proxy, it sends
  usage straight to Velox, with no SDK (client library).

## 2. AI infrastructure sells prepaid credit plus usage

A prepaid commit is credit the customer pays for up front, often at a
discount. "Pay $9k up front, get $10k of usage to draw down" is a common
AI infrastructure contract.

- Stripe's basic usage-based billing has prepaid credit grants but
  [no commits](https://docs.stripe.com/billing/subscriptions/usage-based/use-cases/credits-based-pricing-model).
  Orb and Metronome (now part of Stripe) offer commits as hosted services.
  Open-source engines offer prepaid wallets: see
  [How Velox compares](comparison.md).
- In Velox a commit is one line on an invoice. When the invoice is
  finalized, the credit is added in the same step. Usage uses it up, free
  (promotional) credit first.
- To stop serving a customer when credit runs out, see
  [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

## 3. Some businesses must keep billing records on servers they control

Examples: data-residency rules, healthcare-adjacent SaaS, government
buying rules.

- Stripe Billing is a hosted service, so usage and invoice data are stored
  on Stripe's servers.
- Velox runs in your own cloud network, and usage and invoice records stay
  in your Postgres. If you connect Stripe for card payments, the customer's
  name, email, address and the charge amount still go to Stripe.
- The database itself keeps test data apart from live data, and one
  account's data apart from another's (Postgres Row-Level Security).
- The program makes no outbound calls of its own: no licence check, no
  usage telemetry, no vendor endpoint. You can confirm this by searching
  the source.

## 4. Every bill gets questioned, and the answer is the raw events

One engineer who has built usage billing put it this way
([Hacker News, 2022](https://news.ycombinator.com/item?id=32670188)):

> *"you will get a query on a bill by a customer… and you need to be able to dig into the raw data… and validate there was no billing error."*

Velox is built for that moment:

- Raw events are stored, never only added up and thrown away.
- The price is copied onto the invoice line, so changing a price tomorrow
  cannot change what you billed last month.
- Every usage line links to the usage events for that customer, meter and
  period.
- The audit log cannot be edited: database triggers refuse any change or
  delete.

## How it compares

A fact-by-fact comparison with Lago, OpenMeter, Flexprice and Kill Bill,
with sources: [How Velox compares](comparison.md).
