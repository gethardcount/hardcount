# Glossary

Words used in the Hardcount docs and dashboard. For the big picture, read
[How Hardcount works](how-hardcount-works.md). Words from the code and ops docs:
[contributor glossary](dev/glossary.md).

Each entry ends with a link to the page that explains it. Where a design
decision record (ADR) explains why it works that way, a second link
follows.

## Accounts and access

- **tenant**: One business account in Hardcount, with its own customers, data,
  dashboard users and API keys. One Hardcount install can hold many tenants, and each sees only its own data.
  A dashboard user signs in to one tenant: the first one they joined. There
  is no way yet to switch to another. [More](self-host.md)
- **operator**: Has two meanings. Usually the business that runs Hardcount to
  bill its customers, or its staff who sign in to the dashboard. In
  self-host and ops docs, the engineer who deploys and runs the install.
  [More](self-host.md)
- **customer**: A person or company that a tenant bills. Each customer
  belongs to one tenant and has an `external_id`: your own ID for it, such
  as the user ID in your database. Usage events use it to say whose usage it
  is. [More](concepts/data-model.md)
- **test mode and live mode**: Two separate sets of data in one tenant, as
  in Stripe. Test mode is for trying things, and nothing in it charges real
  money. Live mode is real billing. An API key's mode (the `_test_` or
  `_live_` in its prefix) or the dashboard's mode switch picks which set you
  see. Tenant settings are shared, but each mode has its own Stripe
  connection. [More](api-keys.md)
- **API key types (secret, usage only)**: The two kinds of key you send as
  `Authorization: Bearer <key>`. A secret key (`hc_secret_`) has full
  access to one tenant and stays on your server. A usage-only key
  (`hc_ingest_`, `ingest` in the API) can only send usage events, for
  LiteLLM or another gateway. [More](api-keys.md)

## Pricing and usage

- **meter**: A counter for one kind of usage you charge for, such as tokens
  or API requests. Each meter has a unique `event_name`, a unit, one way to
  combine readings (its aggregation: `sum`, `count`, `max` or `last`). A
  meter only counts: plans price it. Every price of the meter bills under its
  aggregation, which is fixed once the meter has a usage event (held usage
  does not count). Usage events name the meter by its `event_name`.
  [More](concepts/meters-and-prices.md#meters)
- **usage event**: One "this customer used this much" record that your app
  sends to `POST /v1/usage-events`. It names the customer and the meter and
  gives a decimal quantity. It carries a required idempotency key, so a
  resent event is not counted twice, and it can carry dimensions.
  [More](concepts/usage-events.md)
- **dimensions (on a usage event)**: Labels on a usage event, such as
  `model` or `token_type`, as key-value pairs. Prices match on them, so one
  meter can be priced at many rates. Up to 16 keys, and each value is
  a single value (text, number, true/false or null), never a list or an
  object. [More](concepts/meters-and-prices.md#how-hardcount-picks-the-price-for-an-event)
- **price**: One usage rate of a plan. It names a meter and the labels an
  event must carry for the price to take it (`dimension_match`; none takes
  every event). Its rate is a price per unit (`per_unit`), graduated tiers
  (`graduated`), or fixed-size packages (`package`; a part package bills as
  a whole one). It
  takes the plan's currency. When an event fits more than one price of the
  plan, the price with the most labels takes it, so nothing counts twice. A
  billing period is priced as the plan's prices were when it started. A
  subscriber who joins the plan inside a period (a new subscription, an
  immediate plan change, an added item) is priced as the plan was when they
  first joined it in that period; after a trial, when the trial ended.
  [More](concepts/meters-and-prices.md#prices), and
  [the decision](adr/130-usage-prices-belong-to-the-plan.md)
- **used plan**: A plan that a subscription has used, canceled ones
  included. A price change on a used plan applies from each subscriber's next
  billing period, and from the join for a subscriber who first joins the plan
  in a period after it; on a plan no one has used, at once.
  [More](concepts/meters-and-prices.md#prices-belong-to-the-plan)
- **no_price**: Usage of a meter that no price of the customer's plan
  takes. Hardcount records it and bills nothing for it. When ingest finds
  this, it answers `billing_status: "no_price"`.
  [More](ops/runbook/usage-with-no-price.md)
- **billing_status (on an ingest answer)**: What Hardcount found when it
  stored an event: `after_close` (its time is already invoiced),
  `not_on_plan` (the plan of a period not invoiced yet has no price for its
  meter), `outside_subscription` (no subscription bills its time) or
  `no_price`. A status that is present was true then. No status means Hardcount
  found no reason then; it is not a promise that the event will be billed.
  The invoice is the final answer. Only the answer that wrote the event
  carries it.
  [More](concepts/usage-events.md#which-period-an-event-counts-in)
- **invoice grace period**: How long Hardcount waits after a billing period
  ends before it makes the period's invoice, so usage that arrives a little
  late is still billed. It is `INVOICE_GRACE_PERIOD` on the server: 1 hour
  by default, from 0 to 72 hours. A test clock has none.
  [More](concepts/usage-events.md#late-usage), and
  [the decision](adr/120-invoice-grace-period-for-late-usage.md)
- **late usage**: A usage event that arrives after the invoice of the
  period its `timestamp` falls in was made. Hardcount stores it, answers
  `billing_status: "after_close"`, and bills it on no invoice. Bill it with
  a one-off invoice. [More](concepts/usage-events.md#late-usage)
- **backfill**: Sending usage that happened in the past, with
  `POST /v1/usage-events/backfill`, for example from an old billing system.
  The `timestamp` is required and may not be in the future. A backfilled
  event counts toward its period only while that period's invoice is not
  made yet, like any late usage. Each backfill is recorded in the audit log.
  [More](concepts/usage-events.md#late-usage)
- **held usage**: Usage from a LiteLLM proxy whose customer, or `tokens`
  meter, does not exist yet. Hardcount keeps it without billing it and shows
  it on the **Usage** page. Once the customer exists, you release it, and
  it becomes usage events under its original keys and times; or you discard
  it. [More](integrations/litellm.md#usage-for-a-customer-hardcount-does-not-know-yet),
  and [the decision](adr/122-hold-and-release-usage-for-unknown-customers.md)
- **plan**: A price package you sell: a currency, a billing interval
  (monthly or yearly), a fixed base fee, and its usage prices. Its meters
  are the meters it has a price for. A subscription has one or more items, each a plan with a quantity.
  A plan's status is active or archived. [More](concepts/meters-and-prices.md)
- **recipe**: A ready-made pricing setup that one API call applies. The
  built-in ones are `anthropic_style`, `openai_style` and
  `replicate_style`. A recipe creates a plan with its prices, and each
  meter they bill that does not exist yet, and nothing else. Applying the same recipe again changes nothing. [More](guides/bill-for-ai-tokens.md)
- **idempotency key**: A unique string that makes a retry safe: the same
  key means the same operation, so it takes effect once. The main kinds
  are the `Idempotency-Key` HTTP header, the key on each usage event, and
  the key Hardcount sends to Stripe on each charge. The header keeps one
  response for 24 hours; the usage routes ignore it. The usage-event key is
  required and kept forever, so a key used twice stores only the first
  event. [More](concepts/usage-events.md#counted-once)

## Subscriptions and invoices

- **subscription**: A customer's agreement to pay for one or more plans,
  period after period. It holds items (each a plan and a quantity). Its
  status is draft, trialing, active or canceled.
  [More](concepts/subscriptions.md)
- **billing period**: The dates one subscription invoice covers. The start
  day is included and the end is not. Periods start and end at midnight in
  the tenant's billing timezone. A monthly period starts either on the 1st
  of the month (calendar billing, the default, after a first partial
  period) or on the day the subscription started (anniversary billing).
  With a trial, the first period starts on the day the trial ends. A start
  day that a month lacks, such as the 31st, falls on the month's last day.
  Yearly periods are always anniversary.
  [More](concepts/subscriptions.md#billing-periods), and
  [the decision](adr/012-day-grade-calendar-billing.md)
- **billing timezone**: The one timezone for your whole account (a tenant
  setting, UTC by default). Hardcount puts period starts and ends at midnight in
  it, and counts days for proration in it. Each invoice also keeps the zone
  it was issued under, so its dates never shift later.
  [More](concepts/subscriptions.md#billing-periods), and
  [the decision](adr/077-org-level-billing-timezone.md)
- **day-grade billing**: Billing counts whole days, not hours. Normal period
  starts and ends snap to midnight in the billing timezone, and the start
  day counts in full. Day counts are rounded, so daylight saving time never
  adds or loses a day. [More](adr/012-day-grade-calendar-billing.md)
- **bill timing (in advance / in arrears)**: A plan setting for when its
  fixed base fee is invoiced. In advance: at the start of the period it
  covers. In arrears: at the end. There is no default: a plan with a base
  fee must say which, and a plan with no base fee bills in arrears. Usage is
  always billed in arrears.
  [More](concepts/meters-and-prices.md#the-base-fee-and-when-it-is-billed), and
  [the decision](adr/031-per-plan-base-bill-timing.md)
- **proration**: Charging only for the whole days a price was active in a
  billing period. For a plan billed in advance, a change in the middle of a
  period charges the difference now, or credits it on a downgrade, and a
  short first period is prorated when billed. An upgrade is refused while
  the period's invoice is unpaid. A plan billed in arrears is prorated by
  days when the period closes.
  [More](concepts/subscriptions.md#plan-changes), and
  [the decision](adr/050-unpaid-source-proration-policy.md)
- **spend threshold**: A limit on one subscription that issues an invoice
  early. It is either a money amount (`amount_gte`) checked against the
  period's usage so far, or a usage count on one item (`usage_gte`).
  Crossing it issues an invoice with `billing_reason` "threshold" for that
  usage, and no base fee. The period keeps its dates, and the base fee is
  billed as it would be with no threshold.
  Usage is never blocked.
  [More](ai-pricing.md#usage-on-a-yearly-plan)
- **invoice states (draft, finalized, paid, void, uncollectible)**: Where
  an invoice is in its life. Draft: you can still change it. Finalized:
  issued and owed. Paid. Void: canceled, as if the debt never existed (the
  API calls it `voided`). Uncollectible: written off as bad debt, but kept
  for the records. [More](concepts/invoices.md#statuses)
- **hosted invoice page**: A public web page for one issued invoice (not a
  draft). It opens from a secret link, so the customer needs no login. It
  shows the invoice and its PDF. While money is due, the invoice is not
  written off and no payment is in flight, it has a Pay button that opens
  Stripe Checkout.
  [More](concepts/invoices.md#pdf-and-the-hosted-invoice-page), and
  [the decision](adr/021-hosted-invoice-pay-flow-saves-pm.md)
- **outbound webhook**: A message Hardcount sends to a URL in your app when
  something happens, such as `invoice.paid`. It is signed, so your app can
  check that Hardcount sent it, and sent again on a schedule when your app
  does not answer `2xx`.
  You add the URL as a webhook endpoint. Not to be confused with the
  webhooks Stripe sends to Hardcount. [More](webhooks.md)

## Credits

- **credit grant / credit block**: Credit added to a customer's balance.
  Examples: a prepaid commit, free promotional credit, a goodwill credit,
  or credit from a proration or a credit note. The balance is a ledger of
  grant, usage, expiry and adjustment entries. Each grant is a block that
  invoices draw down when billed: promotional credit first, then the
  soonest to expire, then the oldest. A draw-down updates the grant entry's
  used amount in place. A void, or a credit note on an unpaid invoice,
  puts drawn credit back on the grants it came from, with their expiry and
  kind. A share of an expired grant expires at once. A credit note on a paid
  invoice always adds its full credit to the balance.
  [More](concepts/credits.md), and
  [the decision](adr/132-returned-credit-goes-back-to-its-grants.md)
- **prepaid commit**: Usage credit the customer pays for up front, sold as
  a line on a one-off invoice. When that invoice is finalized, Hardcount adds a
  credit block to the customer's balance. The credit can be more than the
  price (pay $9k, get $10k). The purchase is not taxed; the usage the
  credit pays for is. [More](concepts/credits.md#prepaid-commits)
- **drawdown**: Using a customer's credit balance to pay part of an invoice
  right after it is issued, so less is charged. Free promotional credit is
  used before other credit. Within each, the credit that expires soonest
  goes first. A commit's own purchase invoice can never be paid with credit.
  [More](concepts/credits.md#the-order-credit-is-spent)
- **credit note (CN)**: A document that lowers what a customer owes on an
  invoice. On an unpaid invoice it lowers the amount due; if nothing is left
  due, the invoice is marked paid, and a part beyond what was due gives
  back credit the invoice used. On a paid one,
  its total is split between a refund to the card, credit to the
  customer's balance, and money returned outside Hardcount (out of band). Its
  status is draft, issued or voided; only a draft can be voided.
  [More](concepts/invoices.md#credit-notes), and
  [the decision](adr/038-credit-note-three-channel-allocation.md)
- **recorded from Stripe**: A credit note whose card refund is a refund made
  in Stripe outside Hardcount, for example in the Stripe dashboard
  (`recorded_from_stripe`). Hardcount writes it from the refund's webhook when
  the refund plainly fits the invoice, or the operator links the refund at
  create; either way Hardcount calls nothing at Stripe for it.
  [More](ops/runbook/refund-made-in-stripe.md), and
  [the decision](adr/134-refunds-made-in-stripe-are-recorded.md)
- **clawback**: Has two meanings. In subscriptions: a paid period billed in
  advance is cut short (a cancel, a downgrade, a removed item or a plan
  swap). Hardcount then issues a credit note for the unused amount, tax
  included, and reverses that tax. The part already paid goes to the
  customer's credit balance; an unpaid part lowers what the open invoice
  asks for. In the
  credit ledger: a negative entry that removes credit that was granted.
  [More](concepts/subscriptions.md#plan-changes), and
  [the decision](adr/048-credit-clawback-tax-reversal.md)

## Payments and collection

- **PaymentIntent**: Stripe's name for one attempt to charge a card. Hardcount
  charges cards only through PaymentIntents, not through Stripe Billing, so
  Hardcount keeps the invoice. An invoice stores its PaymentIntent ID. A payment
  recorded as made outside Hardcount has none, and sets `paid_out_of_band`.
  [More](concepts/payments.md), and
  [the decision](adr/001-paymentintent-only-stripe.md)
- **payment method (PM)**: A customer's saved card. Stripe holds the card,
  and Hardcount keeps its Stripe ID and the details it shows. A customer can
  have several. Hardcount marks one as the default and names that exact card on
  every automatic charge, so Stripe never picks one.
  [More](guides/connect-stripe.md#3-save-a-customers-card), and
  [the decision](adr/053-explicit-payment-method-at-charge.md)
- **unknown payment status**: A charge got an unclear answer (a timeout, a
  server error, a dropped connection), so Hardcount cannot tell if money moved.
  Nothing charges the invoice while the status is unknown. A Stripe webhook
  or Hardcount's regular Stripe check settles it. If no PaymentIntent ID was
  recorded, the invoice is parked (below), which can end in a new
  charge.
  [More](concepts/payments.md#when-the-answer-is-unclear), and
  [the decision](adr/107-unknown-is-terminal-until-a-human.md)
- **charge hold**: A 5-minute claim Hardcount puts on an invoice while it
  charges it, so only one charge runs at a time. Automatic charges, payment
  retries and **Collect payment** all share it. A charge with a clear answer
  lifts it at once. After a lost answer (a crash, a shutdown) it stays until
  the reconciler has asked Stripe.
  [More](concepts/payments.md#when-the-answer-is-unclear)
- **reconciler**: The check that asks Stripe what happened to payments
  Hardcount did not hear back about: `unknown` and `processing` payments,
  charge holds left after a lost answer, and parked invoices. It runs with
  every scheduled billing run, hourly (every 5 minutes with `APP_ENV=local`).
  [More](concepts/payments.md#when-the-answer-is-unclear)
- **parked invoice**: An invoice whose charge result is unknown and that has
  no PaymentIntent ID recorded, so Hardcount cannot yet tell if the customer
  paid. Nothing charges it. On every scheduled billing run (every hour, or
  every 5 minutes with `APP_ENV=local`, but not `POST /v1/billing/run`),
  Hardcount lists the customer's payments in Stripe. A payment that appears is
  recorded, and if none has appeared
  24 hours after the charge, the invoice is charged again as usual. An
  operator can write it off meanwhile.
  [More](concepts/payments.md#when-the-answer-is-unclear), and
  [the decision](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)
- **parked subscription**: A subscription whose billing failed eight times
  in a row for the same due time. Hardcount stops trying to bill it, so it
  stays due and no invoice is issued for that period. It leaves this state
  when an operator calls `POST /v1/subscriptions/{id}/retry-billing`, or
  when what is due changes. It has nothing to do with a parked invoice.
  [More](ops/runbook/billing-failing.md), and
  [the decision](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **dunning**: Hardcount's automatic process for collecting an unpaid invoice.
  A failed charge or a missing payment method starts it. It retries the
  charge on a schedule and sends reminders. When the retries run out, the
  dunning policy can pause or cancel the subscription, write off the
  invoice, both, or neither. A new account starts with one policy in each
  mode, "Default retries", which retries three times and then does neither.
  [More](concepts/payments.md#retries-dunning)
- **dunning run**: One dunning process for one unpaid invoice (at most one
  per invoice). It records the policy, the number of tries, when the next
  action is due, whether it is paused, and its state: active, resolved or
  escalated (every retry failed and the final action was taken). Once
  closed, it also records how it was resolved.
  [More](concepts/payments.md#retries-dunning), and
  [the decision](adr/064-dunning-run-creation-derived-from-invoice-state.md)
- **write-off**: Marking an invoice uncollectible. Hardcount stops collecting
  it, but keeps it in the records. Nothing charges it again and its tax is
  not reversed, but you can still record a payment made outside Stripe.
  Hardcount asks Stripe to close a Checkout page still open for it; if Stripe
  refuses, the page stays payable until it expires, at most an hour after
  it opened. If it billed its
  subscription's current period, it still counts as unpaid when that
  subscription changes. An operator, or dunning's final action, writes an
  invoice off.
  [More](concepts/invoices.md#how-an-invoice-changes), and
  [the decision](adr/110-written-off-invoices-close-self-service-payment.md)

## Costs and margin

- **cost table**: Your list of what an AI provider charges you per token,
  by provider, model and token type, kept separately for live and test
  mode. When a usage event arrives, Hardcount uses it to record that event's
  cost, unless the sender already sent the real cost.
  [More](concepts/margin.md#costs-what-each-event-cost-you)
- **COGS and margin**: COGS (cost of goods sold) is what you pay providers
  for a customer's usage. Hardcount records it on each usage event when the
  event arrives, from the sent cost or the cost table. Margin is what that
  usage earns at your current prices minus that cost, as a share of what it
  earns, for one customer over a date range. Only you see it, never the
  customer.
  [More](concepts/margin.md), and
  [the decision](adr/079-provider-cost-tables.md)
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
  exists only in test mode. [More](concepts/test-clocks.md)
- **catch-up (catchup)**: The work Hardcount does after a test clock moves
  forward. It runs everything that fell due in the skipped time: trial
  ends, billing periods, pauses that end, thresholds, tax retries, charges,
  dunning steps and credit expiries. It runs them by kind, not in time
  order, in that order. So one long move can bill the next period before
  dunning's final action runs, and a pause or cancel is then dated at that
  next billing date. To test a policy that pauses or cancels, move the
  clock to the invoice's last retry first, then to the next billing date
  ([#1276](https://github.com/gethardcount/hardcount/issues/1276)). The
  clock then returns to `ready`, or to `internal_failure` if a step failed
  or the move took more than 10 minutes.
  [More](concepts/test-clocks.md#what-runs-while-the-clock-moves), and
  [the decision](adr/015-test-clock-async-catchup.md)
- **clock-pinned**: Describes a customer on a test clock, and everything it
  owns (subscriptions, invoices, credits). Its time comes from the clock,
  not the real clock, and the normal billing run skips it.
  [More](concepts/test-clocks.md#what-a-clock-holds), and
  [the decision](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)
- **simulated time**: The pretend current time of a test clock. Hardcount uses
  it for every business date on a clock-pinned record (periods, due dates,
  paid dates). The dashboard marks such invoices and credit notes
  "Simulated", and Home leaves them out of its totals.
  [More](concepts/test-clocks.md), and
  [the decision](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)

**Next:** see these words in action in the [quick start](quick-start.md).
