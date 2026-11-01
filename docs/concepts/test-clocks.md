# Test clocks

A test clock is a pretend clock for test customers. You create a clock, add
a new customer on it, then move the clock forward. Renewals, trial ends,
invoices and payment retries then happen in seconds, not months. This page
covers what a clock moves, how to move it safely, and what deleting one
does.

A test clock exists only in test mode. A live key gets `403` on every
test-clock route.

## What a clock holds

A clock holds the customers you create on it, and everything they own:
subscriptions, usage, invoices, credit and payment retries. Their dates
come from the clock, not from the real time.

- **Put a customer on a clock when you create it,** with `test_clock_id`
  on `POST /v1/customers`. An existing customer cannot be moved onto a
  clock, or off one.
- **Usage without a `timestamp`** gets the clock's time.
- **The normal billing runs skip these customers.** Only moving the clock
  bills them.
- **Home in the dashboard leaves them out**, because their dates are
  pretend ones. The invoices show "Simulated".

## Statuses

| Status | What it means | What you can do |
|---|---|---|
| `ready` | Idle. | Move it, or delete it. |
| `advancing` | Hardcount is running everything that fell due in the skipped time. | Wait, and read the clock again. |
| `internal_failure` | A step failed, or the run took more than 10 minutes. Work done before the failure is kept. `last_failure_reason` says why. | `POST /v1/test-clocks/{id}/retry-advance`, or delete the clock. |

## Moving a clock

1. **Create it** with a start time: `POST /v1/test-clocks` with
   `{"name": "renewal test", "frozen_time": "2026-01-01T00:00:00Z"}`.
2. **Create a customer on it**, then a subscription, as for any customer.
3. **Move it forward:** `POST /v1/test-clocks/{id}/advance` with a later
   `frozen_time`.
   - The time must be later than the clock's current time, and at most one
     year later. A longer move is refused with `422`: move in steps.
   - A clock that is not `ready` is refused with `409 invalid_state`.
   - The answer comes at once, with `status: "advancing"`.
4. **Read the clock** with `GET /v1/test-clocks/{id}` every second or two,
   until `status` is `ready` or `internal_failure`. Stop on either one: a
   loop that waits only for `ready` never ends after a failure.

When it is `ready`, `last_advance_summary` counts what the move did, such
as `invoices_generated` and `charges_retried`.

## What runs while the clock moves

Hardcount runs everything that fell due between the old time and the new
one. It runs it by kind, not in time order:

1. trial ends;
2. billing period closes, which make the invoices;
3. pauses that reach their `resumes_at`;
4. spend thresholds;
5. tax calculations waiting for a retry;
6. card charges of invoices waiting to be charged, such as new invoices
   or ones whose customer just added a card;
7. credit notes that were waiting for an invoice to settle;
8. dunning steps: payment retries, reminder emails and final actions;
9. credit expiry, last.

A clock has no invoice grace period: moving past a period's end makes its
invoice at once.

### Testing a dunning policy that pauses or cancels

Because the kinds run in this order, one long move can close the next
billing period before dunning's final action runs. The pause or cancel is
then dated at that next billing date, not at the last retry. On a plan
billed in advance, the next period's invoice is made too.

So move the clock in two steps:

1. To the time of the invoice's last retry, or later but before the next
   billing date. The pause or cancel then runs, dated at that retry.
2. Then to the next billing date.

## Deleting a clock

`DELETE /v1/test-clocks/{id}` deletes the clock and everything its
customers own: the customers, their subscriptions, usage events, invoices,
credit notes, credit and saved cards. It cannot be undone.

- It works in any status, even `advancing`.
- It keeps what is not the customers': meters, plans and prices, dunning
  policies, API keys and the audit log.
- It frees the deleted usage events' idempotency keys, so the same keys can
  be used again.

## Limits

- One move covers at most one year.
- A move that runs longer than 10 minutes stops at `internal_failure`.
- One server runs one move at a time, for all accounts. A move waits for
  the one before it.

**Next:** a dated walkthrough: [Test a billing change with a test
clock](../guides/test-a-billing-change.md).
