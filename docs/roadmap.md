# Roadmap

What Hardcount is building now, what comes next, what waits for a reason, and
what it will not build. Plans can change; when they do, this page changes
first. Every shipped change, with its date, is in the
[CHANGELOG](../CHANGELOG.md).

## Now

The item planned right after v0.9.0 has shipped ([Shipped recently](#shipped-recently)). What comes next is below.

## Next

**v0.10.0**, planned after v0.9.0:

- **Spend alerts.** An alert level on each subscription, checked
  against live usage, with an optional repeat. It is not the
  [spend threshold](glossary.md#subscriptions-and-invoices) that bills early. When usage crosses it, Hardcount
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
  retrying an unclear card charge under a new key while Stripe may still
  finish the first. A new key is used only once Stripe has shown no payment
  for that charge for 24 hours
  ([ADR-127](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
- Coupons, a customer portal, quotes, contracts and minimum spend.
- Telemetry of any kind.
- A separate docs site. The docs stay in this repo.

## Shipped recently

After v0.9.0 (in [Unreleased](../CHANGELOG.md#unreleased)):

- **A usage-only API key for LiteLLM and other gateways.** It can send usage events and nothing else ([key types](api-keys.md#key-types)). The platform key type and `/v1/tenants`, which nothing could use, are removed ([ADR-133](adr/133-api-key-types-secret-and-ingest.md)).

October 2026, v0.9.0 ([everything it changes](../CHANGELOG.md)):

- **Velox is now Hardcount.** Binaries, settings, key and id prefixes, images, metrics, headers and the cookie carry the new name. v0.9.0 installs fresh: it refuses a database an earlier build made.
- **Usage prices are set per plan.** Two plans can charge different rates for one meter. A price is `per_unit`, `graduated` or `package`, and a change applies from each subscriber's next billing period. The base fee stays locked while a subscription uses the plan; to change it, make a new plan ([ADR-130](adr/130-usage-prices-belong-to-the-plan.md)).
- **A meter has one aggregation,** fixed once it has a usage event. Its `key` is now `event_name`.
- **A usage event says when it will not be billed.** The 201 and each batch result carry a `billing_status`: no price takes it, the plan does not price its meter, no subscription covers its time, or its period was already invoiced.
- **Prepaid credit carries its currency** and pays only invoices in that currency ([ADR-129](adr/129-credit-carries-its-own-currency.md)).
- **3-D Secure is not a decline.** A card payment the bank wants the customer to confirm is no longer treated as a declined card.
- **Migrations run with goose.** A failed migration leaves nothing to repair, and `hardcount migrate force` is gone.
- **One shape for the API.** Lists answer `{data, total}`, every error uses one envelope, request bodies with an unknown field are refused, and publishable API keys are removed.
- **The first-invoice guide runs in CI** on every pull request, and each output is checked against the page.

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
- An automated check that runs 32 money consistency rules in CI.
- Running several copies of Hardcount at once, with background jobs handed over in seconds when one stops.

**Next:** try it on your computer with the [quick start](quick-start.md).
