# Peer evidence: time windows in comparable billing tools (read from source, 2026-10-16)

Clones (shallow, read-only, nothing executed): /tmp/claude-0/peers/{lago-api, lago-front, openmeter, openapi (Stripe spec), orb-python, metronome-python}.
Paths below are relative to each clone.

## Per-customer usage / cost views: default window and scope

| Vendor | Default window | Scope | Evidence |
|---|---|---|---|
| Orb | Current billing period of the active subscription (or the most recently active one); explicit timeframe sums every subscription active in it; cumulative from period start | customer or subscription | orb-python `src/orb/resources/customers/costs.py:100-111,121-127`; `resources/subscriptions.py:993-997` |
| Lago | Current billing period only, no from/to; "full_usage" = lifetime | one subscription (required) | lago-api `app/services/invoices/customer_usage_service.rb:39-43,200-219`; `app/controllers/api/v1/customers/usage_controller.rb:7-19` |
| Lago (dashboard) | Subscription page: current billing period table, lifetime graph, realtime 24h. Customer "Insights" tab: gross revenue only, last 12 calendar months | subscription; customer only for revenue | lago-front `SubscriptionCurrentUsageTable.tsx:186-200,314-316`; `CustomerUsage.tsx:34-36,69-77`; `graphs/Gross.tsx:88` |
| Lago (new analytics, tenant-wide) | Usage: rolling 30 days, daily; MRR/revenue: rolling 12 months, monthly | tenant | lago-front `analytics/usage/useUsageAnalyticsOverview.ts:82-94`; `useMrrAnalyticsOverview.ts:95-111` |
| Metronome | None for batch usage (required); grouped usage has `current_period=true` | customer + metric | metronome-python `resources/v1/usage.py:55-60`; `types/v1/usage_list_with_groups_params.py:33-38` |
| Stripe | None: meter summaries and analytics require start/end | customer + meter | openapi `spec3.yaml:78513-78566`; `spec3.beta.sdk.yaml:117687,117769` |
| OpenMeter | Meter query: from = beginning (meter EventFrom), to = now. Entitlement history: last reset → now | any / customer+feature | openmeter `api/spec/packages/legacy/src/meters.tsp:628-638`; `openmeter/streaming/clickhouse/meter_query.go:63-89`; `aip/src/customers/entitlements/operations.tsp:102-126` |

Margin (revenue vs cost): none of the five has it. OpenMeter has a cost-only feature query (`aip/src/features/cost.tsp:14-22`); Orb "costs" are billed amounts.

## End boundary convention

| Vendor | Convention | Evidence |
|---|---|---|
| Orb | `[start, end)` | `costs.py:209-211`; `subscriptions.py:1021-1028` |
| Stripe | `[start, end)` for meter summaries/analytics (one schema line contradicts); invoice LINE period inclusive at both ends | `spec3.yaml:78564-78566, 4843-4852, 20450-20462` |
| Metronome | invoice filters/lines: `ending_before` exclusive | `resources/v1/customers/invoices.py:218-244`; `types/v1/customers/invoice.py:612-613` |
| OpenMeter | code `[from, to)`; legacy spec says `to` "Inclusive" (spec/code mismatch) | `meter_query.go:458-461` vs `meters.tsp:438-455` |
| Lago | inclusive: period ends 23:59:59.999999 customer TZ, events `<=`; date-only filter end = end of that day | `app/services/subscriptions/dates_service.rb:84,124,245-249`; `app/models/event.rb:18-19`; lago-front `DateRangeFilterFields.tsx:53-71` |

## Timezone of day buckets / dates

| Vendor | Policy | Evidence |
|---|---|---|
| Orb | day buckets at the CUSTOMER's local midnight; customer tz defaults to account tz, immutable | `subscriptions.py:1091-1094`; `customers/customers.py:391-393` |
| Lago | customer `applicable_timezone` (customer → billing entity → UTC) for periods, issuing_date, due date, period display; org tz for dashboard display/filters; some filters UTC | `app/models/customer.rb:233-237`; `create_generating_service.rb:61-63,95-97` |
| Stripe | daily summaries UTC-aligned only; analytics `timezone` param default UTC | `spec3.yaml:78587-78593`; beta `117778` |
| OpenMeter | caller-chosen `windowTimeZone`, default UTC | `meter_query.go:121-124`; `meters.tsp:465-473` |
| Metronome | unknown for usage | — |

## Zero-fill of empty buckets

Lago client zero-fills monthly and daily charts (`lago-front src/components/graphs/utils.ts:50-55`; `analytics/usage/utils.ts:60-91`); Orb fetch_usage includes every window (`subscriptions.py:1008-1011`); OpenMeter v3 entitlement history includes empty windows (`history.tsp:34-37`), meter query does not.

## Due dates and past due

| Vendor | Due date | Past due | Evidence |
|---|---|---|---|
| Lago | DATE = issuing_date (customer-TZ date) + N days | hourly job: `payment_due_date < Time.current` (date vs instant) | `create_generating_service.rb:95-97`; `mark_invoices_as_payment_overdue_job.rb:13`; `mark_overdue_service.rb:23` |
| Orb | net_terms days from invoice or issue date; invoice_date is midnight in customer tz | no overdue status | `resources/invoices.py:97-100,124-128`; `types/shared/invoice.py:654` |
| OpenMeter | ISO-8601 duration (default P30D) added in UTC | set externally (Stripe webhook / API) | `billing/service/invoicecalc/dueat.go:11-37`; `app/stripe/httpdriver/webhook.go:313-330` |
| Stripe | `days_until_due` from creation; tz/rounding not in spec | subscription past_due when unpaid by due date (send_invoice) | `spec3.yaml:101642-101645, 55229-55270` |
| Metronome | `net_payment_terms_days`, undocumented | unknown | `types/v1/customers/invoice.py:584` |

## Mid-period end, trials, plan changes

- Lago: termination ends the period at the instant; display = that instant's customer-TZ date ("Nov 1 – Nov 15" for a Nov 15 14:00 cancel); prorated days use ceil, so the partial day counts as full (`dates_service.rb:84-89,125`; `app/services/utils/datetime.rb:64-77`).
- Lago: trial waives only the base fee; usage during a trial IS billed (`calculate_fees_service.rb:268-282,342-351`). Trial end = started_at + N days as a UTC instant (`subscription.rb:185-189`).
- Orb: trial usage appears at list price in subtotal and is discounted in total (`costs.py:194-203`). Immediate cancel invoices usage up to cancellation (`subscriptions.py:724-734`).
- Lago: upgrade terminates the old subscription now and starts the new one now; each subscription counts events in its own window (`activate_service.rb:60-70`); downgrade waits for the period end (`plan_downgrade_service.rb:31-33`).
- OpenMeter: end-of-subscription billing `bill_actual_period` by default (`billing/defaults.go:19`); no first-class trial.

## Invoice list date filter

Lago filters invoices by `issuing_date` (a DATE, inclusive both ends) and shows that date (`app/queries/invoices_query.rb:169-170`; lago-front `FiltersItemIssuingDate.tsx`).

## Flexprice (added later; clones /tmp/claude-0/peers/flexprice, flexprice-front)

Paths: be/ = flexprice, fe/ = flexprice-front/src. Verified by hand: the four lines marked (v).

- **Has a margin view (the only peer that does).** Customer → Analytics tab shows Revenue, COGS, Margin, Margin %, ROI. Cost = metered usage × a tenant-wide costsheet price; revenue = metered usage revenue. be/internal/ee/service/revenue_analytics.go:101-112 (v); costsheet_usage_tracking.go:66-73.
- **Default margin window: 1st of the current calendar month (browser-local midnight) → now.** Free date pickers, no presets, no billing-period option. fe/pages/customer/tabs/CustomerAnalyticsTab.tsx:41-45 (v).
- **Scope: per customer across all subscriptions.** Currency taken from the first subscription and amounts summed across items without a currency split. be/internal/ee/service/meter_usage.go:1335-1337, 1986-1992 (v).
- Other defaults: APIs (/events/analytics, /costs/analytics) last 7 days (be/internal/api/v1/events.go:491-509; dto/costsheet_analytics.go:47-56); tenant Cost Analytics now−30d (fe/pages/usage/cost-analytics/CostAnalytics.tsx:22-23); Revenue page this calendar quarter UTC (fe/pages/insights-tools/revenue/Revenue.tsx:98); customer portal tenant-configured preset (today/7d/30d/current month/last month).
- Per-subscription usage (/subscriptions/usage) and entitlement usage default to each subscription's current billing period (be/internal/ee/service/subscription.go:2800-2813; billing.go:3139-3150).
- Boundaries: billing/analytics SQL `[start, end)` (meter_usage_query_builder.go:181-185); event list inclusive `<=` (repository event.go:587-591). Period end exclusive; display inconsistent: invoice tables show end−1s ("1 Jun – 31 Aug", fe/utils/common/format_date.ts:206-240), portal and PDF show the exclusive end.
- Timezone: on the customer (default UTC), copied to subscriptions; drives period boundaries, trial end, due-date arithmetic and analytics day buckets (meter_usage.go:1107-1118; meter_usage_query_builder.go:699-700). No tenant timezone. PDF dates effectively UTC.
- Due date: issue instant + N days via AddDate in the subscription timezone (time of day kept), recomputed at finalize (billing.go:2403-2414; invoice.go:1310-1331). Overdue is an instant compare: `DueDate.Before(now)` (invoice.go:2950), sweep `due_date <= asOf − grace`.
- Mid-period cancel sets period end = cancel instant; proration second-based.
- Trial: end = start + N local calendar days (subscription_trial.go:53-55); trial usage billing not determinable from code.
- Analytics time series not zero-filled.
- Date filters: "before X" is `<` on local/UTC midnight of X, so a picked end day is excluded.

| Flexprice | 1st of month → now (browser-local) for margin; APIs 7d | per customer, currencies summed | exclusive in SQL; display mixed | customer TZ | instant + N days in sub TZ | instant | yes (revenue, COGS, margin, ROI) |
