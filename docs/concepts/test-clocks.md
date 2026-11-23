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
| `advancing` | Hardcount is running everything that fell due in the skipped time. | Wait, and read the clock again. A change to one of its customers (the customer, its subscriptions, invoices, credit notes and credits, and usage you backfill or release), or a new customer on it, answers `409 clock_advancing`. Reads, usage your app or proxy sends, payments through Stripe, price changes and deleting the clock go on; usage, payments and price changes that arrive during the advance take effect after it. |
| `internal_failure` | A job failed, or the run took more than 10 minutes. The clock stops at the time it had got to; work done before the failure is kept, and the jobs after it did not run. `last_failure_reason` names the job that failed and why. | `POST /v1/test-clocks/{id}/retry-advance`, which carries on from there, or delete the clock. |

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

Hardcount moves the clock forward through each time something falls due
for its customers, in order: a billing period's end, a trial's end, a
pause's end, a scheduled cancel, a payment retry. So things happen in the
order they would in real time: a payment retry due before a renewal runs
before the renewal is billed. While it moves, `frozen_time` shows where it
has got to. At each of those times it runs these jobs, in this order:

1. trial ends;
2. billing period closes, which make the invoices;
3. pause ends: pauses that reach their `resumes_at`;
4. spend thresholds;
5. tax retries: tax calculations waiting for a retry;
6. relief credit notes: credit notes (documents that lower what a customer
   owes) waiting to be issued, so an invoice is charged the lower amount;
7. card charges of invoices waiting to be charged, such as new invoices
   or ones whose customer just added a card;
8. relief credit notes again, for those that were waiting for an invoice
   to settle;
9. payment retries: dunning retries, reminder emails and final actions;
10. credit expiry, last.

If a job fails, the jobs after it do not run: the clock stops at
`internal_failure` at the time it had got to, and its `last_failure_reason`
starts with the job's name from this list. Retry advance runs that time
again and carries on to the time you asked for; the counts in
`last_advance_summary` add up across Retry advance.

A draft whose tax cannot be calculated stays a draft and the move goes on,
as in real time; the tax retry tries it again at each later time the clock
reaches.

A card charge that could not be made is a failure on a clock: the clock
stops at `internal_failure`, and `last_failure_reason` names the invoice and
says what to do.

- Credits could not be applied first, or Stripe could not take the charge
  just then (a rate limit, an open circuit breaker): nothing was charged,
  and Retry advance charges the invoice.
- Stripe refused the account's API key, or none is connected: nothing was
  charged. Fix the key under **Settings → Payments**, then Retry advance.
  Until the key is fixed, each Retry advance fails the same way, and the
  jobs after card charges (relief credit notes waiting for a settle,
  payment retries, credit expiry) do not run for that clock.
- An earlier charge attempt has not finished (Stripe's answer was unclear,
  it refused the key while charging, or another charge of the invoice was
  running): the invoice is not charged again until it has, so it cannot be
  charged twice. Hardcount checks that attempt on a billing run, within the
  hour; Retry advance then charges what is left. If the key was refused,
  fix it first.

Outside a clock, a later billing run does the same work (hourly, or every 5
minutes when `APP_ENV` is `local`). Not a failure, on a clock or off it: a
card refusal, with or without a decline code (the invoice goes to dunning),
an invoice the customer may be paying on the hosted page, one whose
collection is paused, and one settled meanwhile. Collection waits for the page or the pause, as outside a clock.

Payment retries work the same way. A retry that gets no definite answer
from Stripe is not counted as an attempt, and on a clock it stops the move
at `internal_failure`; Retry advance tries it again. One move takes each
dunning run through every step due in it (its retries, then the final
actions), each at its own time. A final action that fails, or a run that
stops moving, also stops the move. So does a
retry whose invoice has an earlier payment that has not finished (a charge
whose result is not known yet, or one still being sent): it cannot be
charged twice. Hardcount checks that payment on its next billing run
(hourly by default); Retry advance once it is done, or void or write off
the invoice to stop waiting.

While a customer has an invoice's payment page open (up to about a day of
real time, not clock time), that invoice's payment retries and final action
wait, and the move does not stop for them. They run on the first move after
the page is paid or closes.

A clock has no invoice grace period: moving past a period's end makes its
invoice at once.

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
