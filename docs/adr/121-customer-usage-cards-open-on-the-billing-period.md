# ADR-121: The customer page's Activity and Margin cards open on the billing period

**Date:** 2026-10-16
**Status:** Accepted

## Summary

On the customer page, the Activity card opens on **Cycle** and the Margin card on
**Current period**. Both are the current billing period of the customer's
subscription whose cycle started last, the window `GET /v1/customers/{id}/usage`
uses by default. Margin also offers **Last invoiced period**: the period of the
customer's most recent subscription invoice, so its usage figure covers what that
invoice billed. The rolling 7, 30 and 90 days stay as options. When a window holds
no usage, both cards name the date of the customer's latest usage and offer a range
that includes it. The API is unchanged.

## Context

Both cards opened on a rolling "last 30 days". After `scripts/demo.sh` (usage sent
on day 1, test clock advanced 32 days), Margin read $0 and "No usage in the window
yet" while the invoice on the same page read $3.88. The rolling window started two
days after the usage.

This is not specific to the demo. A billing period runs 28 to 31 days and starts on
the anchor day, so a rolling 30 days never equals a period. A rolling total sits
next to invoice and cycle figures on the same page and disagrees with them.

What comparable tools do, read from their source on 2026-10-16:

- **Orb**: customer costs default to "the subscription's current billing period";
  with no active subscription, "the most recently active subscription"
  (`orb-python` `src/orb/resources/customers/costs.py:100-104`).
- **Lago**: current usage is per subscription and always the current billing
  period; the API takes no from/to (`lago-api`
  `app/services/invoices/customer_usage_service.rb:200-219`). Past usage lists
  invoiced periods (`app/queries/past_usage_query.rb:35-46`).
- **Flexprice**: per-subscription usage defaults to the subscription's current
  period (`internal/ee/service/subscription.go:2800-2813`). Its customer margin view
  opens on the calendar month to date
  (`flexprice-front` `src/pages/customer/tabs/CustomerAnalyticsTab.tsx:41-45`).
- **Stripe, Metronome, OpenMeter**: usage APIs take a caller-chosen window and have
  no per-customer default. Metronome offers `current_period=true`
  (`metronome-python` `types/v1/usage_list_with_groups_params.py:33-38`).

None of them opens a per-customer view on a rolling 30 days.

## Decision

- Activity's default preset is `cycle` (`DEFAULT_ACTIVITY_PRESET`).
- Margin's presets are Current period (default), Last invoiced period, Last 7, 30
  and 90 days, and Custom (`MARGIN_PRESETS`, `DEFAULT_MARGIN_PRESET`).
  - **Current period** sends the `period` from `GET /v1/customers/{id}/usage`
    (no params) as the margin `from`/`to`. Activity's Cycle tab uses the same
    query, so the page makes one request for both.
  - **Last invoiced period** reads the customer's invoices and takes the latest
    period end among invoices that are not draft or voided and have a
    subscription (`lastInvoicedPeriod`).
  - Both print the window as its inclusive dates under the card description.
- An empty window reads the customer's newest usage event. If it falls outside the
  window, Margin shows "No usage in this window. Latest usage: <date>." with a
  button that switches to a custom range from that day to today; Activity links to
  Usage events on that range.
- Code: `web-v2/src/lib/usageWindow.ts`, `web-v2/src/hooks/useCustomerUsageWindows.ts`,
  `MarginCard.tsx`, `CostDashboard.tsx`.

## Why this design

The card's figures should cover the same window as the invoice and cycle figures
next to them. The billing period is the one window those share. The rolling
presets stay for questions about recent activity.

The empty-state pointer handles the case a default cannot: right after a cycle
closes, the new period is empty by definition. Naming the latest usage keeps
"no usage here" from reading as "no usage at all".

## Alternatives considered

- **A. Keep 30 days and add only the empty-state pointer.** Removes the empty card
  after the demo but keeps a default that never equals a billing period, so the
  figure still disagrees with the invoice beside it.
- **B. Calendar month to date (Flexprice).** One customer-wide window that is simple
  across several subscriptions, but it matches invoices only for calendar billing,
  and after the demo's advance it is also empty.
- **C. Last 90 days.** Hides the demo case; still never equals a period.
- **D. Resolve the periods on the server (a `period` parameter on the margin API).**
  Cleaner for API callers. Not needed for the dashboard, and "last invoiced" would
  make the usage domain read invoices, a new cross-domain edge. Deferred.

## Consequences

### Positive
- Activity and Margin open on the same window as the Upcoming card and the invoice.
- After the demo, one click (Last invoiced period, or the latest-usage button)
  shows the usage the invoice billed.

### Risks / open items
- **Several subscriptions on different anchors.** There is no single customer
  period. Current period is the cycle of the subscription whose cycle started last;
  other subscriptions' meters are rated over that window. The card prints the
  window, so the figure is labelled. A per-subscription breakdown (Lago's model) is
  not built.
- **Margin revenue is still rated over the window**, not read from invoice lines.
  It still differs from the invoice when a plan changed mid-period (old meters are
  not rated), after a cancel (canceled subscriptions are not rated), and for trial
  usage (rated, never invoiced). Reading revenue from invoices is a separate
  decision.
- A customer with no active subscription opens Margin on "no current billing
  period" with a button for the last 30 days; Activity shows its existing empty
  state.
- The latest-usage read is one extra request, made only when a window is empty.

## References

- ADR-079 (provider cost tables and per-customer margin), ADR-086 (relative time
  follows the test clock), ADR-120 (invoice grace period).
- Sources above were read from shallow clones of `getlago/lago-api`,
  `orbcorp/orb-python`, `Metronome-Industries/metronome-python`,
  `flexprice/flexprice`, `flexprice/flexprice-front`, `openmeterio/openmeter` and
  `stripe/openapi` on 2026-10-16.
