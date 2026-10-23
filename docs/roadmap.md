# Roadmap

What Velox is building now, what comes next, what waits for a reason, and
what it will not build. Plans can change; when they do, this page changes
first. Every shipped change, with its date, is in the
[CHANGELOG](../CHANGELOG.md).

## Now

**v0.9.0**, not released yet:

- **Done:** migrations run with goose, and `velox migrate force` is gone.
  There is no upgrade from v0.8.x: install fresh.
- **Done:** a card payment the customer's bank wants them to confirm (3-D
  Secure) is no longer treated as a declined card.
- **In review:** each prepaid credit carries its own currency and pays only
  invoices in that currency.
- **In review:** while live subscriptions use a meter, a pricing edit that
  would change the price of a billing period already open is refused.
- **Planned:** the commands in the [first-invoice guide](first-invoice.md)
  run in CI, so a change that breaks them fails.

Everything else v0.9.0 changes: [Unreleased](../CHANGELOG.md#unreleased).

Also now, in v0.9.0 or right after it:

- The answer to a usage event says when Velox stores it but will not bill
  it: the customer's plan does not price that meter, or no subscription
  covers the event's time. It already says so for usage that arrives after its period was
  invoiced. A batch answer will give each event's status.
- A secret API key that can only send usage, for LiteLLM and other
  gateways.
- The design for usage prices set per plan (v0.10.0, below), for review.

## Next

Planned after v0.9.0:

- **v0.10.0: usage prices set per plan.** Today a usage price is set on the
  meter, so two plans that charge different token rates need two meters.
  In v0.10.0 each plan sets its own price for each meter. A price edit
  takes effect at each subscriber's next billing period. The pricing API
  changes: for example, mode `flat` is renamed `per_unit`, and the meter's
  default price goes away. A migration moves v0.9.0 prices over.
- **Spend alerts.** A spend threshold on each subscription, checked
  against live usage, with an optional repeat. When usage crosses it, Velox
  sends a webhook and an email. Cutting a customer off stays in your app
  ([how](stop-usage-when-credit-runs-out.md)).
- **Generated SDKs for Go and TypeScript,** built from
  [`api/openapi.yaml`](../api/openapi.yaml) once v0.10.0 settles the API.

## Later

Each item waits until someone using Velox needs it:

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
- Billing late usage on the next invoice.
- Automatic credit top-ups.
- Importing LLM prices from a public price list.
- Two-factor sign-in.
- Data deletion, compliance documents (SOC 2, audit-log retention), and
  roles that limit what a team member can do. Every team member has full
  access today.
- Bulk actions, moving many subscriptions to a new plan at once, and a tool
  to move from Stripe Billing.
- A Helm chart and a Terraform module. Docker Compose is the supported
  install.

## Not building

- Kafka, ClickHouse, Temporal or a fleet of workers. Velox stays one server
  program on Postgres, plus Redis in production. Past one table's limits,
  the plan is to split the Postgres table by month.
- Stripe Billing or Stripe Invoicing as a second record of each invoice.
- A licence key or an enterprise edition. Velox stays MIT, with one
  edition.
- Entitlements (feature gating) and hard usage caps inside Velox. Your app
  enforces limits. This is looked at again if two or more teams using
  Velox ask for feature gating.
- A plugin system.
- Plan phases, plan versions, an XML catalog, several API versions side by
  side, or GraphQL beside REST. From v0.10.0, keeping customers on an old
  price means a separate plan.
- A double-entry accounting ledger or revenue recognition.
- Dedup that is optional or expires, money in floating-point numbers, and
  retrying an unclear card charge under a new key.
- Coupons, a customer portal, quotes, contracts and minimum spend.
- Telemetry of any kind.
- A separate docs site. The docs stay in this repo.

## Shipped recently

October 2026 (v0.5.0 to v0.8.2):

- **Try it with one script.** `deploy/compose/quickstart.sh` starts Velox from the published images and prints a login, and `./quickstart.sh demo` runs AI token usage through to an invoice.
- **Late usage is billed.** An invoice is built an hour after its period ends (`INVOICE_GRACE_PERIOD`), so usage that arrives a little late still counts.
- **New accounts retry failed payments.** Each new account starts with a "Default retries" policy.
- **Live credit balance.** `GET /v1/credits/balance/{customer_id}/live` and the public cost page show how much prepaid credit a customer has left, including usage not yet invoiced.
- **LiteLLM usage for a customer that does not exist yet is kept.** Velox holds it and shows it on the Usage page. Create the customer, then release it once.
- **Issued invoices do not change.** An invoice or credit note keeps the customer and company details it was issued with.
- **Release images for ARM and Intel/AMD**, signed with cosign.

July to September 2026:

- Prepaid commits, drawn down by later invoices.
- Provider cost tables and per-customer margin.
- Team invites.
- Safer handling of card charges whose outcome is unclear, and of unpaid invoices.
- An automated check that runs 30 money consistency rules in CI.
- Running several copies of Velox at once, with background jobs handed over in seconds when one stops.

**Next:** try it on your computer with the [quick start](quick-start.md).
