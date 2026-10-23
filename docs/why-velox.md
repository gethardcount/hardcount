# Why Velox exists

Velox is for teams that charge by usage, most of all AI products that
charge per token. Small AI teams kept falling into the gaps below. Each
one names the problem, what Velox does, and the source. Where another
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
   card fee, and Stripe Tax's fee if you use Stripe Tax. One meter prices
   every model and token type: each usage event carries them as labels
   ([how](ai-pricing.md#how-velox-picks-the-price-for-an-event)).

2. **Prepaid credit plus usage is a common AI contract, and basic billing
   has no commits.** Stripe's basic usage-based billing has prepaid credit
   grants but
   [no commits](https://docs.stripe.com/billing/subscriptions/usage-based/use-cases/credits-based-pricing-model).
   Orb and Metronome (now part of Stripe) offer commits as hosted services.
   **Velox instead:** a commit ("pay $9k, get $10k of usage") is one line on
   an invoice, and later usage draws it down
   ([how](ai-pricing.md#sell-prepaid-credit-a-prepaid-commit)).

3. **Some businesses must keep billing records on servers they control.**
   Stripe Billing is a hosted service, so usage and invoice data are stored
   on Stripe's servers.
   **Velox instead:** runs in your own network, and usage and invoice
   records stay in your Postgres. If you connect Stripe for card payments,
   the customer's name, email, address and the charge amount still go to
   Stripe. Velox makes no outbound calls of its own: no licence check, no
   usage telemetry, no vendor endpoint.

4. **Open-source engines hold back key parts behind a paid licence.** Some
   keep failed-payment retries, previews or wallets in a paid edition
   ([which ones](comparison.md)).
   **Velox instead:** one MIT licence with nothing held back. Failed-payment
   retries, credit notes, refunds, invoice previews and the audit log are
   all in it.

5. **They are heavy to run yourself.** Flexprice's default setup runs nine
   services, including Kafka and ClickHouse
   ([docker-compose.yml](https://github.com/flexprice/flexprice/blob/1ede2d98c81e9e565c087cc373aaf18bfd6d8d4f/docker-compose.yml)).
   OpenMeter's runs eleven ([service counts](comparison.md)).
   **Velox instead:** one server program plus Postgres and Redis, with a web
   dashboard.

6. **There is no way to fast-forward time to test a billing change.**
   ([What other projects offer](comparison.md).)
   **Velox instead:** test clocks for test customers. Pin a test customer to
   a clock and move it forward up to a year at a time, so months of billing
   run in minutes, in test mode on your normal install.

7. **You can't see which customers lose you money.** AI costs move with
   usage, so a customer can cost you more than they pay.
   **Velox instead:** margin per customer. Velox records what each usage
   event cost you when it arrives, from your provider price table or
   LiteLLM's reported cost. It sets that against the price of the same
   usage ([how](ai-pricing.md#see-your-margin)). Other engines report cost
   too: OpenMeter keeps an LLM price table with cost per feature, and
   Flexprice's cost sheets report margin (checked 2026-10-23).

8. **Every bill gets questioned, and the answer is the raw events.** One
   engineer who has built usage billing put it this way
   ([Hacker News, 2022](https://news.ycombinator.com/item?id=32670188)):

   > *"you will get a query on a bill by a customer… and you need to be able to dig into the raw data… and validate there was no billing error."*

   **Velox instead:**

   - Raw events are stored, never only added up and thrown away.
   - The price is copied onto the invoice line, so changing a price tomorrow
     cannot change what you billed last month.
   - Every usage line links to the usage events for that customer, meter and
     period.
   - The audit log cannot be edited: database triggers refuse any change or
     delete.

**Next:** run it on your computer with the [quick start](quick-start.md).
