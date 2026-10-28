# Roadmap

What Hardcount is building now, what comes next, what waits for a reason, and
what it will not build. Plans can change; when they do, this page changes
first. Every shipped change, with its date, is in the
[CHANGELOG](../CHANGELOG.md).

## Now

**v0.9.0**, not released yet:

- **Done:** migrations run with goose, and `hardcount migrate force` is gone.
  There is no upgrade from v0.8.x: install fresh.
- **Done:** a card payment the customer's bank wants them to confirm (3-D
  Secure) is no longer treated as a declined card.
- **Done:** each prepaid credit carries its own currency and pays only
  invoices in that currency. Grant and adjust take a `currency`
  ([ADR-129](adr/129-credit-carries-its-own-currency.md)).
- **Done:** a meter has one aggregation, which every price of it bills
  under. It is fixed once the meter has a usage event; held usage does
  not count. A meter's `key` is now
  `event_name`, the name usage events send it by
  ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md) §2).
- **Done:** a price is `per_unit`, `graduated` or `package`, and a package
  price bills a part package as a whole one. Every `*_cents` field is in
  the currency's smallest unit, provider costs included
  ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md) §3).
- **Done:** eight API operations that another operation already did are
  removed. The [CHANGELOG](../CHANGELOG.md) names the replacement for each.
- **Done:** a dunning policy's schedule is one list of days, `retry_days`:
  the days to wait before each retry. It replaces three fields that had to
  agree.
- **Planned:** public names change once, before the release: API key
  prefixes, header names, environment variables and the session cookie.
  Publishable keys are removed. Code that calls the API or checks webhook
  signatures must change.
- **Done:** the commands in the [first-invoice guide](first-invoice.md)
  run in CI on every pull request, on that pull request's build, and each
  output is checked against the page, so a change that breaks them fails.
- **Done: usage prices are set per plan.** A price belongs to a plan, so
  two plans can charge different rates for one meter. A price change applies
  at once on a plan no one has subscribed to, and from each subscriber's next
  billing period otherwise, on a test clock too. A subscriber who first
  joins the plan in a period after a change pays it from then. A customer's negotiated
  rate names one plan price. Rating rules, meter pricing rules and a meter's
  default price are gone
  ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md)).
- **Done:** a recipe file holds only meters and plans with their
  prices, and an install creates nothing else: no dunning policy and no
  webhook endpoint.
- **Done:** the answer to a usage event says when Hardcount, as it stored
  the event, found a reason it is not billed: no price of the plan takes
  it, the plan does not price its meter, no subscription covers its time,
  or its period was already invoiced. A batch answer gives each event's status
  ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md) §12).
- **Not in v0.9.0:** a base fee change that waits for each subscriber's
  next period. The base fee stays locked while a subscription that is not
  canceled uses the plan; to change it, create a new plan and move subscribers to it
  ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md) section 5).

Everything else v0.9.0 changes: [Unreleased](../CHANGELOG.md#unreleased).

Also now, in v0.9.0 or right after it:

- A secret API key that can only send usage, for LiteLLM and other
  gateways.

## Next

**v0.10.0**, planned after v0.9.0:

- **Spend alerts.** A spend threshold on each subscription, checked
  against live usage, with an optional repeat. When usage crosses it, Hardcount
  sends a webhook and an email. Cutting a customer off stays in your app
  ([how](stop-usage-when-credit-runs-out.md)).
- **Generated SDKs for Go and TypeScript,** built from
  [`api/openapi.yaml`](../api/openapi.yaml), once the per-plan pricing API
  has settled.
- **Billed or not, on each usage event.** The Usage page shows whether an
  invoice billed each event and, if not, why, with a filter. Hardcount records
  it when it invoices the period, so it says what the invoice did. Today
  only the answer to the event says what Hardcount found when it arrived.

## Later

Each item waits until someone using Hardcount needs it:

- Several subscriptions for one customer on one meter, such as one per
  project.
- Payment methods beyond cards, such as SEPA or ACH debits, on the hosted
  invoice page.
- A second payment processor.
- Splitting the usage table by month, a retention window for old events,
  and a read replica: when an install passes about 50 million events, or
  expects more than 5 million a day ([scale](scale.md#if-you-need-more)).
- Quarterly billing, volume tiers, a start date on subscriptions, and
  currencies with no minor unit, such as JPY.
- Monthly usage on a yearly plan, on one subscription. Today a yearly plan
  bills its usage at the end of the year; for monthly usage, add a monthly
  subscription ([how](ai-pricing.md#usage-on-a-yearly-plan)).
- Billing late usage on the next invoice.
- Automatic credit top-ups.
- Importing LLM prices from a public price list.
- Two-factor sign-in.
- Data deletion, compliance documents (SOC 2, audit-log retention), and
  roles that limit what a team member can do. Every team member has full
  access today.
- A base fee edit on a used plan that applies from each subscriber's next
  period.
- Bulk actions, moving many subscriptions to a new plan at once, and a tool
  to move from Stripe Billing.
- A Helm chart and a Terraform module. Docker Compose is the supported
  install.

## Not building

- Kafka, ClickHouse, Temporal or a fleet of workers. Hardcount stays one server
  program on Postgres, plus Redis in production. Past one table's limits,
  the plan is to split the Postgres table by month.
- Stripe Billing or Stripe Invoicing as a second record of each invoice.
- A licence key or an enterprise edition. Hardcount stays MIT, with one
  edition.
- Entitlements (feature gating) and hard usage caps inside Hardcount. Your app
  enforces limits. This is looked at again if two or more teams using
  Hardcount ask for feature gating.
- A plugin system.
- Plan phases, plan versions, an XML catalog, several API versions side by
  side, or GraphQL beside REST. Once prices are set per plan, keeping
  customers on an old price means a separate plan.
- A double-entry accounting ledger or revenue recognition.
- Dedup that is optional or expires, money in floating-point numbers, and
  retrying an unclear card charge under a new key.
- Coupons, a customer portal, quotes, contracts and minimum spend.
- Telemetry of any kind.
- A separate docs site. The docs stay in this repo.

## Shipped recently

October 2026 (v0.5.0 to v0.8.2):

- **Try it with one script.** `deploy/compose/quickstart.sh` starts Hardcount from the published images and prints a login, and `./quickstart.sh demo` runs AI token usage through to an invoice.
- **Late usage is billed.** An invoice is built an hour after its period ends (`INVOICE_GRACE_PERIOD`), so usage that arrives a little late still counts.
- **New accounts retry failed payments.** Each new account starts with a "Default retries" policy.
- **Live credit balance.** `GET /v1/credits/balance/{customer_id}/live` and the public cost page show how much prepaid credit a customer has left, including usage not yet invoiced.
- **LiteLLM usage for a customer that does not exist yet is kept.** Hardcount holds it and shows it on the Usage page. Create the customer, then release it once.
- **Issued invoices do not change.** An invoice or credit note keeps the customer and company details it was issued with.
- **Release images for ARM and Intel/AMD**, signed with cosign.

July to September 2026:

- Prepaid commits, drawn down by later invoices.
- Provider cost tables and per-customer margin.
- Team invites.
- Safer handling of card charges whose outcome is unclear, and of unpaid invoices.
- An automated check that runs 30 money consistency rules in CI.
- Running several copies of Hardcount at once, with background jobs handed over in seconds when one stops.

**Next:** try it on your computer with the [quick start](quick-start.md).
