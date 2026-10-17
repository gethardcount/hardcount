# What's in the box

This page lists what Velox does, grouped by job, with what each feature does
for you and where to read more. Everything below ships in the one MIT
edition: there is no paid tier. For what Velox does not cover, see
[What Velox is not](../README.md#what-velox-is-not).

**Who it is for:** anyone checking whether a feature they need exists.

## Pricing for AI and usage

A meter is a counter for one kind of usage, such as tokens. Dimensions are
labels on a usage event, such as `model` or `token_type`.

| Feature | What it does for you | Learn more |
|---|---|---|
| Meters with dimensions | One meter can have many prices. Each usage event carries labels, such as `model`, that pick its price. So one Tokens meter prices every model and token type. | [How Velox works](how-velox-works.md) |
| Pricing recipes | One call creates a meter, prices, a plan and a dunning policy for a common pricing shape: `anthropic_style`, `openai_style` or `replicate_style`. | [AI pricing](ai-pricing.md) |
| LiteLLM | Point LiteLLM's spend callback at Velox, and each AI call becomes token usage, labelled with `model` and `provider`. A resent call is counted once. No SDK (client library) is needed. | [LiteLLM guide](integrations/litellm.md) |
| Prepaid commits | Sell "pay $9k, get $10k of usage" as a line on a one-off invoice. The credit is added when that invoice is finalized, in the same step and before it is paid. Each later invoice uses it up when it is issued, free (promotional) credit first. | [AI pricing](ai-pricing.md#sell-prepaid-credit-a-prepaid-commit) |
| Margin per customer | Enter what each AI provider charges you per token, once. Velox then shows each customer's usage revenue next to what it cost you. | [AI pricing](ai-pricing.md#see-your-margin) |
| Exact decimals | Quantities can be fractions, such as part of a GPU-hour, with up to 12 decimal places. Prices are kept exactly as written, such as $3.00 per 1M tokens. Each invoice line is rounded to whole cents. | [Glossary](glossary.md) |
| Ways to count | Each price picks how usage adds up: `sum`, `count`, `max`, the last value in the period (`last_during_period`), or the last value ever (`last_ever`). | [Glossary](glossary.md) |
| Usage your customers can see | A usage and cost breakdown per customer in the dashboard, and a JSON link your app can show the customer ("$4.31 of GPT-4 this billing period"). | [Roadmap](roadmap.md) |

Good to know:

- **LiteLLM** needs a `tokens` meter (the AI recipes create one) and
  customers whose external ID matches LiteLLM's `user`.
- **Margin** uses the costs saved with `/v1/provider-costs` and is read with
  `GET /v1/customers/{id}/margin`. It leaves out base fees, credits and
  taxes. Usage with no saved cost gets no cost, and the report counts it.
  Revenue it cannot match to a model is shown apart, in
  `unattributed_revenue_cents`.
- **The customer's JSON link** opens with a per-customer token. It shows
  usage charges so far this period and the credit balance. A ready-made
  widget is on the roadmap, not shipped.

## Runs on your own servers

| Feature | What it does for you | Learn more |
|---|---|---|
| One program to run | Velox is a single Go program. Background jobs run inside it, so there is no queue, worker fleet or separate scheduler to run. The Compose file adds Postgres, Redis (for rate limits only), the dashboard and nginx in front: five containers, one of them Velox. | [Self-host](self-host.md) |
| Nothing is locked | MIT licence, no licence key, no premium tier, no feature that unlocks when you pay. That includes dunning (retrying failed payments). An MIT release stays MIT, and the maintainers do not plan to move any feature behind a key. | [Comparison](comparison.md) |
| Nothing phones home | No licence check, no telemetry, no analytics SDK. Tracing (OpenTelemetry) goes only to a collector you set up. | [Self-host](self-host.md) |
| Your data stays with you | Billing data lives in your own Postgres. Velox sends data only to services you set up, such as Stripe for payments and tax, and your email server. You still own the compliance work; self-hosting does not do it for you. | [Why Velox](why-velox.md) |
| Audit log that cannot be edited | A log of who changed what in your account. The database refuses to change or delete any entry in it. | [Architecture](architecture.md) |
| Data kept apart | The database itself keeps test data apart from live data, and one account's data apart from another's (Row-Level Security). | [Self-host](self-host.md#the-two-database-users) |

Good to know: check "nothing is locked" against whatever else you are
evaluating. For example, Lago's automatic dunning is a
[premium add-on](https://getlago.com/docs/guide/dunning/automatic-dunning).
Velox has no SSO or role-based access to lock (see
[What Velox is not](../README.md#what-velox-is-not)).

## Subscriptions and invoices

| Feature | What it does for you | Learn more |
|---|---|---|
| Subscriptions | Trials, pausing collection (invoices stay drafts), cancelling at period end, and plan changes with proration: the customer pays only for the part of the period each price was active. | [The Velox API](api-surface.md#customers-and-subscriptions) |
| Discounts | A different price for one customer (a price override), or credit. There are no coupons: they were cut before launch, and credit and price overrides do the job. | [The Velox API](api-surface.md#credits-and-refunds) |
| Invoices | PDF invoices, a hosted invoice page the customer opens from a secure link, and emails with your branding. | [How Velox works](how-velox-works.md) |
| Invoice preview | See the next invoice before it is made. The preview shows the subtotal only, without tax or credits. | [The Velox API](api-surface.md#invoices-and-billing) |
| Dunning | When a card payment fails, Velox retries it on a schedule and emails the customer. If every retry fails, it does what your policy says. | [How Velox works](how-velox-works.md) |
| Billing thresholds | Set a spend or usage level, and Velox makes an invoice early once the period's total reaches it. It bills earlier; it does not stop usage. | [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) |

Good to know about billing thresholds: one fires at most once per billing
period. With `reset_billing_cycle`, it starts a new period, so it can fire
again. Velox checks thresholds on each billing run: every hour, or every 5
minutes when `APP_ENV=local`.

## Credits and refunds

| Feature | What it does for you | Learn more |
|---|---|---|
| Credit balance | Give customers credit and see every change to it. Each change is a new record; nothing is overwritten. | [The Velox API](api-surface.md#credits-and-refunds) |
| Live balance | The credit left after usage not yet invoiced, so your app can stop serving a customer whose credit has run out. | [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) |
| Credit notes and refunds | A credit note lowers what a customer owes on an invoice, as a balance credit or a refund to the card. | [The Velox API](api-surface.md#credits-and-refunds) |

## Reliability

| Feature | What it does for you | Learn more |
|---|---|---|
| Safe retries | Send an `Idempotency-Key` header on a POST, PUT or PATCH, and a retry within 24 hours gets the first answer instead of running again. A server error is saved too (except on usage routes, where each event's own key stops double counting), so a retry cannot repeat a charge that may have gone through. | [The Velox API](api-surface.md#sending-usage-safely) |
| Webhooks that are not lost | Every webhook waits in a database table and is retried for about 26.5 hours. Payment, subscription and credit events are saved together with the change, so a crash cannot drop them. | [Webhooks](webhooks.md#three-rules-for-your-receiver) |
| Signed webhooks | Every webhook is signed. When you rotate the secret, both the old and the new secret sign each message for 72 hours. | [Webhooks](webhooks.md#verifying-signatures) |
| Live webhook log | Watch webhooks as they go out, with every delivery attempt. | [Webhooks](webhooks.md#retries) |
| Test clocks | A test clock is a pretend clock for test customers: move it forward and months of billing happen in seconds. | [First invoice](first-invoice.md) |
| Emails are not lost | Customer emails are queued after the change is saved, and sent at least once from the queue. | [Email setup](ops/email-setup.md) |

Good to know about webhooks: these are saved together with the change that
causes them: `invoice.paid`, `payment.succeeded`, `payment.failed`, the
credit-balance events, `dunning.started`, and subscription `created`,
`activated`, `trial_ended` and `canceled`. The rest are saved just after
the change. If that save fails, Velox logs an error and the event is not
sent.

Related: [CHANGELOG](../CHANGELOG.md) (everything shipped, by date) and the
decision records behind coupons ([ADR-039](adr/039-cut-coupons-pre-launch.md)),
prepaid commits ([ADR-078](adr/078-prepaid-commits-phase-1.md)) and margin
([ADR-079](adr/079-provider-cost-tables.md)).
