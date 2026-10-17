# Glossary

The words you meet in the Velox docs and dashboard, each in one plain
sentence first, then the detail. The last section, [For
contributors](#for-contributors), holds words from the code and the
operations docs. For the big picture first, read [How Velox
works](how-velox-works.md). Back to the [docs index](README.md).

## Accounts and access

- **tenant**: One business account in Velox, with its own customers, data,
  dashboard users and API keys. One Velox install can hold many tenants, and each sees only its own data.
  Dashboard users can belong to several tenants. [More](self-host.md)
- **operator**: Has two meanings. Usually the business that runs Velox to
  bill its customers, or its staff who sign in to the dashboard. In
  self-host and ops docs, the engineer who deploys and runs the install.
  [More](self-host.md)
- **customer**: A person or company that a tenant bills. Each customer
  belongs to one tenant and has an `external_id`: your own ID for it, such
  as the user ID in your database. Usage events use it to say whose usage it
  is. [More](api-surface.md)
- **test mode and live mode**: Two separate sets of data in one tenant, as
  in Stripe. Test mode is for trying things, and nothing in it charges real
  money. Live mode is real billing. An API key's mode (the `_test_` or
  `_live_` in its prefix) or the dashboard's mode switch picks which set you
  see. Tenant settings are shared, but each mode has its own Stripe
  connection. [More](api-keys.md)
- **API key types (secret, publishable, platform)**: The three kinds of
  key you send as `Authorization: Bearer <key>`. A secret key
  (`vlx_secret_`) has full access to one tenant and stays on your server. A
  publishable key (`vlx_pub_`) is safe in a browser, but reads no tenant
  data. A platform key (`vlx_platform_`) manages tenants and is not issued
  in the product. [More](api-keys.md)

## Pricing and usage

- **meter**: A counter for one kind of usage you charge for, such as tokens
  or API requests. Each meter has a unique key, a unit, a way to combine
  readings (its aggregation), and optionally a default rating rule. Usage
  events name the meter by its key in `event_name`.
  [More](how-velox-works.md)
- **usage event**: One "this customer used this much" record that your app
  sends to `POST /v1/usage-events`. It names the customer and the meter and
  gives a decimal quantity. It carries a required idempotency key, so a
  resent event is not counted twice, and it can carry dimensions.
  [More](api-surface.md)
- **dimensions (on a usage event)**: Labels on a usage event, such as
  `model` or `token_type`, as key-value pairs. Pricing rules match on them,
  so one meter can be priced at many rates. Up to 16 keys, and each value is
  a single value (text, number, true/false or null), never a list or an
  object. [More](design/multi-dim-meters.md#subset-match-semantics)
- **pricing rule**: A rule on a meter that picks the usage events to price.
  It takes the events whose dimensions contain every key-value pair in its
  `dimension_match`. It combines them into one quantity (sum, count, max or
  last value) and prices that quantity with a rating rule. If rules overlap,
  the highest-priority rule claims each event, so nothing counts twice.
  [More](design/multi-dim-meters.md#priority--claim-semantics)
- **rating rule**: A price formula that turns a usage quantity into money:
  a flat rate per unit, graduated tiers, or fixed-size packages. It is
  identified by its `rule_key` and has versions. Billing uses the version
  that was active when the period opened, so a new version takes effect
  next period. [More](adr/070-price-change-semantics.md)
- **plan**: A price package you sell: a currency, a billing interval
  (monthly or yearly), a fixed base fee, and the meters it charges usage
  for. A subscription has one or more items, each a plan with a quantity.
  A plan's status is draft, active or archived. [More](features.md)
- **recipe**: A ready-made pricing setup that one API call applies. The
  built-in ones are `anthropic_style`, `openai_style` and
  `replicate_style`. A recipe creates meters, rating rules, pricing rules
  and plans, and sets the tenant's dunning policy. It also adds a webhook
  endpoint that stays inactive until you give it a real URL. Applying the
  same recipe again changes nothing. [More](quick-start.md#what-each-demo-step-does)
- **idempotency key**: A unique string that makes a retry safe: the same
  key means the same operation, so it takes effect once. The main kinds
  are the `Idempotency-Key` HTTP header, the key on each usage event, and
  the key Velox sends to Stripe on each charge. The header keeps one
  response for 24 hours. The usage-event key is required and kept forever,
  so a key used twice stores only the first event. [More](api-surface.md)

## Subscriptions and invoices

- **subscription**: A customer's agreement to pay for one or more plans,
  period after period. It holds items (each a plan and a quantity). Its
  status is draft, trialing, active, canceled or archived.
  [More](features.md#billing-building-blocks)
- **billing period**: The dates one subscription invoice covers. The start
  day is included and the end is not. Periods start and end at midnight in
  the tenant's billing timezone. A monthly period starts either on the 1st
  of the month (calendar billing, after a first partial period) or on the
  day the subscription started (anniversary billing). Yearly periods are
  always anniversary. [More](adr/012-day-grade-calendar-billing.md)
- **billing timezone**: The one timezone for your whole account (a tenant
  setting, UTC by default). Velox puts period starts and ends at midnight in
  it, and counts days for proration in it. Each invoice also keeps the zone
  it was issued under, so its dates never shift later.
  [More](adr/077-org-level-billing-timezone.md)
- **day-grade billing**: Billing counts whole days, not hours. Normal period
  starts and ends snap to midnight in the billing timezone, and the start
  day counts in full. Day counts are rounded, so daylight saving time never
  adds or loses a day. A spend threshold that restarts the cycle can start a
  period during the day. [More](adr/012-day-grade-calendar-billing.md)
- **bill timing (in advance / in arrears)**: A plan setting for when its
  fixed base fee is invoiced. In advance: at the start of the period it
  covers. In arrears: at the end. The default is in arrears. Usage is always
  billed in arrears. [More](adr/031-per-plan-base-bill-timing.md)
- **proration**: Charging only for the whole days a price was active in a
  billing period. For a plan billed in advance, a change in the middle of a
  period charges the difference now, or credits it on a downgrade, and a
  short first period is prorated when billed. A plan billed in arrears is
  prorated by days when the period closes.
  [More](adr/050-unpaid-source-proration-policy.md)
- **spend threshold**: A limit on one subscription that issues an invoice
  early. It is either a money amount (`amount_gte`) checked against the
  period's running subtotal, or a usage count on one item (`usage_gte`).
  Crossing it issues an invoice with `billing_reason` "threshold", and can
  restart the cycle if you choose. Usage is never blocked.
  [More](design/billing-thresholds.md)
- **invoice states (draft, finalized, paid, void, uncollectible)**: Where
  an invoice is in its life. Draft: you can still change it. Finalized:
  issued and owed. Paid. Void: canceled, as if the debt never existed (the
  API calls it `voided`). Uncollectible: written off as bad debt, but kept
  for the records. [More](adr/110-written-off-invoices-close-self-service-payment.md)
- **hosted invoice page**: A public web page for one issued invoice (not a
  draft). It opens from a secret link, so the customer needs no login. It
  shows the invoice and its PDF. While money is still owed, it has a Pay
  button that opens Stripe Checkout.
  [More](adr/021-hosted-invoice-pay-flow-saves-pm.md)

## Credits

- **credit grant / credit block**: Credit added to a customer's balance.
  Examples: a prepaid commit, free promotional credit, a goodwill credit,
  or credit from a proration or a credit note. The balance is a ledger that
  is only ever added to. Each grant is a block that invoices draw down when
  billed: promotional credit first, then the soonest to expire, then the
  oldest. [More](adr/071-credit-expiry-retires-the-block.md)
- **prepaid commit**: Usage credit the customer pays for up front, sold as
  a line on a one-off invoice. When that invoice is finalized, Velox adds a
  credit block to the customer's balance. The credit can be more than the
  price (pay $9k, get $10k). [More](ai-pricing.md)
- **drawdown**: Using a customer's credit balance to pay part of an invoice
  when it is billed, so less is charged. Free promotional credit is used
  before paid credit. Within each, the credit that expires soonest goes
  first. A commit's own purchase invoice can never be paid with credit.
  [More](design/prepaid-commits.md#d7-drawdown-order-null-safe)
- **credit note (CN)**: A document that lowers what a customer owes on an
  invoice. On an unpaid invoice it lowers the amount due. On a paid one,
  its total is split between a refund to the card, credit to the
  customer's balance, and money returned outside Velox (out of band). Its
  status is draft, issued or voided.
  [More](adr/038-credit-note-three-channel-allocation.md)
- **clawback**: Has two meanings. In subscriptions: a paid period billed in
  advance is cut short (a cancel, a downgrade, a removed item or a plan
  swap). Velox then issues a credit note that adds the unused amount, tax
  included, to the customer's credit balance, and reverses that tax. In the
  credit ledger: a negative entry that removes credit that was granted.
  [More](adr/048-credit-clawback-tax-reversal.md)

## Payments and collection

- **PaymentIntent**: Stripe's name for one attempt to charge a card. Velox
  charges cards only through PaymentIntents, not through Stripe Billing, so
  Velox keeps the invoice. An invoice stores its PaymentIntent ID. A payment
  made outside Stripe stores an `out_of_band:` marker instead.
  [More](adr/001-paymentintent-only-stripe.md)
- **payment method (PM)**: A customer's saved card. Stripe holds the card,
  and Velox keeps its Stripe ID and the details it shows. A customer can
  have several. Velox marks one as the default and names that exact card on
  every automatic charge, so Stripe never picks one.
  [More](adr/053-explicit-payment-method-at-charge.md)
- **unknown payment status**: A charge got an unclear answer (a timeout, a
  server error, a dropped connection), so Velox cannot tell if money moved.
  Nothing retries it. A Stripe webhook or Velox's regular Stripe check
  settles it. If no PaymentIntent ID was recorded, the invoice is parked.
  [More](adr/107-unknown-is-terminal-until-a-human.md)
- **parked invoice**: An invoice whose charge result is unknown and that has
  no PaymentIntent ID recorded, so Velox cannot tell if the customer paid.
  Nothing charges it. It leaves this state when a Stripe webhook or Velox's
  Stripe search finds the charge, or when an operator writes it off.
  [More](adr/108-parked-invoices-search-and-adopt.md)
- **parked subscription**: A subscription whose billing failed eight times
  in a row for the same period. Velox stops trying to bill it, so it stays
  due and no invoice is issued for that period. It leaves this state when an
  operator calls `POST /v1/subscriptions/{id}/retry-billing`, or when what
  is due changes. It has nothing to do with a parked invoice.
  [More](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **dunning**: Velox's automatic process for collecting an unpaid invoice.
  A failed charge or a missing payment method starts it. It retries the
  charge on a schedule and sends reminders. When the retries run out, the
  dunning policy can pause or cancel the subscription, write off the
  invoice, both, or neither. [More](how-velox-works.md)
- **dunning run**: One dunning process for one unpaid invoice (at most one
  per invoice). It records the policy, the number of tries, when the next
  action is due, whether it is paused, and its state: active, resolved or
  escalated (every retry failed and the final action was taken). Once
  closed, it also records how it was resolved.
  [More](adr/064-dunning-run-creation-derived-from-invoice-state.md)
- **write-off**: Marking an invoice uncollectible. Velox stops collecting
  it, but keeps it in the records. Nothing charges it again and its tax is
  not reversed, but you can still record a payment made outside Stripe. An
  operator, or dunning's final action, writes an invoice off.
  [More](adr/110-written-off-invoices-close-self-service-payment.md)

## Costs and margin

- **cost table**: Your list of what an AI provider charges you per token,
  by provider, model and token type, kept separately for live and test
  mode. When a usage event arrives, Velox uses it to record that event's
  cost, unless the sender already sent the real cost.
  [More](design/cost-tables.md)
- **COGS and margin**: COGS (cost of goods sold) is what you pay providers
  for a customer's usage. Velox records it on each usage event when the
  event arrives, from the sent cost or the cost table. Margin is what you
  billed for that usage minus that cost, as a share of what you billed, for
  one customer over a date range. Only you see it, never the customer.
  [More](adr/079-provider-cost-tables.md)
- **cost dashboard**: A view of one customer's usage, charges and expected
  bill for the current period. It never shows your provider cost. It comes
  in two forms: a card on the customer page in the dashboard, and a public,
  read-only JSON address that a partner can embed. That address opens with
  a secret token that is shown once.
  [More](adr/032-public-cost-dashboard-projection.md)

## Testing with simulated time

- **test clock**: A pretend clock for test customers. Create a clock, add a
  new customer on it, then move the clock forward to see renewals, trial
  ends, invoices and payment retries happen in minutes, not months. It
  exists only in test mode. [More](quick-start.md)
- **catch-up (catchup)**: The work Velox does after a test clock moves forward. It
  runs every trial end, billing period, threshold, retry, dunning step and
  credit expiry that fell in the skipped time. The clock then returns to
  `ready`, or to `internal_failure` if any step failed.
  [More](adr/015-test-clock-async-catchup.md)
- **clock-pinned**: Describes a customer on a test clock, and everything it
  owns (subscriptions, invoices, credits). Its time comes from the clock,
  not the real clock, and the normal billing run skips it.
  [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)
- **simulated time**: The pretend current time of a test clock. Velox uses
  it for every business date on a clock-pinned record (periods, due dates,
  paid dates). The dashboard marks such invoices and credit notes
  "Simulated", and Home leaves them out of its totals.
  [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)

## Project terms

- **design partner (DP)**: An early customer who works with the team to
  shape the product. Many features in Velox wait until a design partner
  asks for them, for example roles and permissions (RBAC) or Helm charts.
  [More](roadmap.md#not-planned-for-now)

## For contributors

Words from the code, the database and the operations docs. You do not need
them to use Velox.

### Database access

- **RLS (Row-Level Security)**: A PostgreSQL feature that keeps each
  tenant's data invisible to the others. Tenants share the same tables, and
  each tenant-owned row has a `tenant_id`. A policy shows only the rows that
  match the transaction's tenant, and on mode-aware tables its live or test
  mode, unless the transaction set the bypass flag.
  [More](adr/003-postgresql-rls-multi-tenancy.md)
- **TxTenant and TxBypass**: The two ways to open a database transaction
  with `db.BeginTx`. TxTenant sets the tenant (and test or live mode), so
  RLS shows only that tenant's rows. TxBypass turns RLS off for work across
  tenants, such as the billing scheduler.
  [More](adr/003-postgresql-rls-multi-tenancy.md#decision)
- **Table and column names behind the words above**:
  - dimensions: the `properties` column of a usage event.
  - cost table: `provider_cost_rates`.
  - saved cards: `payment_methods`.
  - dunning run: a row in `invoice_dunning_runs`.
  - an invoice's billing timezone: `invoices.billing_timezone`.
  - a test clock's time: `frozen_time`.
  - clock-pinned invoices and credit notes: `is_simulated` is set.

### Running Velox

- **outbox**: A database table of messages waiting to be sent
  (`webhook_outbox`, `email_outbox`). Most webhook events, and the four
  money emails (payment receipt, payment failed, dunning warning, dunning
  escalation), are queued inside the transaction that caused them. The rest
  are queued right after it commits. A background dispatcher then delivers
  each row, with retries. [More](adr/040-outbox-always-on.md)
- **leader lease**: A row in the `leader_leases` table that lets exactly
  one copy of Velox run one tick of a job that must run once across all
  copies (billing, dunning, both outbox dispatchers, webhook delivery). It
  is renewed during the tick and released after it. It expires 10 seconds
  after the last renewal.
  [More](adr/114-leader-leases-tick-scoped-fencing.md)
- **fencing token**: A number for each job in `leader_leases`
  (`holder_token`) that goes up every time a copy takes the lease. The five
  queries that claim due work check it with `leader_fence`, so a former
  leader that stalled claims nothing.
  [More](adr/114-leader-leases-tick-scoped-fencing.md#fencing-boundary-the-five-claim-funnels)
- **velox-doctor**: A read-only command that scans a Velox database for
  states no correct code could produce, such as invoice totals that do not
  add up. It exits 1 when it finds a problem, or 2 if a check cannot run. It
  runs in CI and needs a database role that can see every row.
  [More](records/product-stability-plan.md)

### Working on the code

- **site-set (and effect-firer, gated reader)**: Every place in the code
  that touches one state, such as an invoice status. That means each
  writer and each effect-firer (code that sends an email, a webhook, a
  Stripe call or a ledger write when the state changes). It also means each
  gated reader (code that branches on the state), each caller and callee,
  and each crash point.
  [More](dev/money-path-robustness-playbook.md#2-the-meta-practice-complete-site-set-enumeration)
- **FLOW (in MANUAL_TEST)**: A named set of test steps run by hand, with a
  stable ID such as FLOW S1 or FLOW TC4. Some code comments and ADRs still
  cite these IDs. MANUAL_TEST.md was retired in October 2026;
  [How we test](testing.md#old-flow-ids) says how to read it in git
  history.
