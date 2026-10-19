# ADR-128: Activity line standard

**Date:** 2026-10-19
**Status:** Accepted
**Amends:** [ADR-104](104-one-activity-lane-entity-calendar.md) for the Sent
emails card, and [ADR-102](102-invoice-charge-attempts.md) for hosted-page
payment attempts.
**Builds on:** [ADR-030](030-simulated-time-everywhere-on-clock-pinned-entities.md)
and ADR-104.

## Summary

An activity line is one row on an activity card: the invoice and subscription
Activity cards, the Dunning page's recovery timeline and the customer's Sent
emails card. Every line is to follow one standard. An event has one wording on
every page, the line names who did it from a fixed set, and a test-clock line
is the real-clock line plus one "Recorded" part. The backend builds every
word; the dashboard only formats money and times and lays the row out. The
owner approved the standard's eight decisions on 2026-10-18. The code follows
in a series of PRs (see [Consequences](#consequences)); until each one ships,
its page shows what it shows today.

## Context

The owner asked for this on 2026-10-18, in his words:

> "I mean Activity line see how you can improve or make it standard,
> consistent, intuitive.. see you use ur intelligence at this stage how you
> want ti go for this e2e.."

A test clock is a pretend clock for test customers. A customer pinned to one
lives on that clock's calendar (ADR-030).

A read of every activity line in the code that day found five kinds of
problem:

- **One event, many wordings.** A write-off and a successful payment each had
  several wordings across pages. The Dunning page said "Dunning started" where
  the invoice page said "Payment recovery started".
- **Lines that state false things.** Two examples:
  - Every payment-recovery escalation read "Escalated for manual review".
    The renderer (`describeDunningEvent` in `internal/invoice/handler.go`)
    switches on policy tokens that the writer stopped storing at ADR-112.
  - A trial that simply ran out read "Trial ended early", because the
    renderer ignores `triggered_by`.

  Five more were found, among them a Sent-emails card that shows real send
  times under a banner that says the page follows the test clock.
- **Test-clock rows built by different rules.** Some rows had a Recorded line
  and some did not. A failed payment on the hosted payment page sorted by real
  time among simulated rows.
- **Who acted was shown on one page only.** The subscription card named the
  actor; the invoice card and the Dunning page never did.
- **Mixed styles within a page.** Several detail separators; money as `5000¢`,
  `12.00 CAD` and `CA$12.00`; raw codes such as `subscription_downgrade` and
  Go error strings on screen; no links to the invoices and credit notes a row
  names.

These are Velox's own defects. The standard is anchored on them, not on
industry shape. Where it makes a design choice that no billing product was
verified to make (day headers), it says so.

## Decision

### The eight rules

1. **One event, one wording.** An event has the same title on every page it
   appears on, and in the audit log's headline verb.
2. **The title is object plus past participle, with no values.** "Invoice
   finalized", "Payment failed". No dashes, no brackets, no actor. Two
   outcomes of one event share one title; the difference goes in the
   problem or meta line.
3. **Money only when the event moves money, asks for it, or cancels money
   owed.** It follows the title, in the document's own currency, through the
   dashboard's one money formatter.
4. **Who did it, on every line, from a fixed set:** `by <name>` (or email),
   `by API key <key name>`, `Customer, on the payment page`,
   `Customer, in the portal`, or `Automatic`. The actor is the last detail
   part, and it comes from a fact written with the row.
5. **Plain words.** No enum codes, table names, Go errors or provider
   wrappers. Decline reasons are written about the cardholder.
6. **One clock rule.** A line is dated on the entity's calendar. An automatic
   event is dated at the instant the contract names (renewal, trial end,
   retry slot), never at the instant a clock was moved. On a test clock the
   meta line ends with `Recorded <real date and time>`.
7. **Four tones, one meaning each.** Neutral (things that happened), success
   (money arrived or was returned), warning (needs a look, or money will not
   be collected), failure (an attempt failed). A failure or warning row says
   what went wrong in a problem line, unless the title already says it all.
8. **Link what the row's own data names:** an invoice, a credit note or a
   subscription whose id the row carries, while it still exists. No link
   goes to a payment-recovery run, because the Dunning page cannot open on
   one run.

Writers apply them from
[Activity lines](../dev/writing-style.md#activity-lines) in the writing-style
guide, which gives the examples.

### The owner's eight decisions

On 2026-10-18 the owner, Sagar, was asked eight questions, each with a
proposed answer. He approved all eight as proposed, in his words: "go as per
your call - you verify..". He also delegated the visual review. A PR that
changes a screen still merges only after its CI screenshots have been compared
with main's; the working session does that comparison in his place.

| # | Question | Decided |
|---|---|---|
| 1 | The word for anything Velox did on its own | "Automatic". "Invoice paid · by Velox" reads as if Velox paid |
| 2 | Day headers on activity cards | Yes. A design choice: one test-clock advance drops many rows on one day |
| 3 | Green only for money | Yes. Finalized, activated and resumed rows turn grey |
| 4 | Where the timezone is named | Once, in the card header ("Times in GMT+5:30"), not on every row |
| 5 | A Recorded part on every test-clock row | Yes, at the end of the meta line, engine rows included. Rows keep the same height on both clocks |
| 6 | The Sent-emails card follows the test clock | Yes. This amends ADR-104 (below) |
| 7 | Hosted-page payment attempts dated on the test clock | Yes. This reverses ADR-102's wall-clock rule for them (below) |
| 8 | One credit-note title | Yes. "Credit note issued · <total>" on every credit note. The refund's state shows in the dot and a problem line, not in the title |

### Time

Which time a line shows depends on why it happened:

| Basis | Kind of line | Real-clock entity | Test-clock entity |
|---|---|---|---|
| Act | A person, an API key or the customer acted | The real moment | The clock's time at that moment, plus Recorded |
| Due | Velox acted at an instant the contract names | When the scheduler acted, within minutes of the named instant | The named instant exactly, plus Recorded |
| Check | Velox found something by checking (a spend threshold crossed) | When the check ran | When the check ran: once per advance, so a long jump dates it at the jump's end |
| Cause | The line follows from another event (a receipt after a payment) | The cause's instant | The cause's instant, plus Recorded |
| Legacy | Rows written before the real-time stamps existed | Their only time | Their only time, no Recorded part |

- The Recorded part shows when the entity is on a test clock and the row has
  a real-time stamp. The backend fills it only for pinned entities, so the
  dashboard has no clock logic for rows.
- Times are absolute, never relative ("2d ago"), and grouped under day
  headers on the entity's calendar.
- Activity cards read oldest first. Logs read newest first: the audit log and
  the Sent-emails card.
- Every activity surface orders rows with one function, `timeline.Order` in
  `internal/platform/timeline`, built on `SortStable`. It is ADR-104's
  four-level rule: the entity-calendar instant, then the real order when the
  whole same-instant group is stamped, then the cause-before-effect ladder,
  then insertion order.

### Where the words are built

The backend builds every word. A new package, `internal/platform/activity`,
holds the `Line` type and the shared parts: email names and outcome words,
the actor mapping from `audit.ResolveActor` kinds, the tone enum, credit-note
reason labels and the decline-code map. Each domain keeps its own describe
functions and returns `activity.Line`.

The dashboard owns only formatting (money, times in the account's timezone,
day headers), turning a link part into a route, the four-entry tone-to-colour
map, and layout. The page-level label maps (`EVENT_LABELS` on the Dunning
page, the email labels and badges on the customer page) are deleted.

The timeline APIs gain an additive `line` field. Times travel only inside it.
The old time fields (both APIs' `timestamp`, the invoice API's row-level
`recorded_at`, the subscription API's `sim_effective_at`) are deprecated
together one release after the pages move.

### Tests that hold the standard

- **Golden files per domain** list every fact variant and the `Line` it
  renders. Each enumerates its writer tokens from code: every dunning
  escalation outcome, every refund status, every outbox status and delivery
  state, every email trigger, every subscription action a writer emits, every
  mapped decline code. Subscription actions become constants so the test can
  range over them.
- **A no-fallback test.** No known token may render a fallback line. This is
  the test that would have caught both false lines quoted in Context.
- **A Go parity test on real Postgres** runs each scenario for a pinned and a
  real-clock customer. The test-clock arm advances to two days past each
  boundary, so a row dated at the advance instead of its named instant shows
  up.
- **Playwright text parity** reads each activity card on both clocks with
  every `<time>` element removed, and compares the two.
- **Screenshots** of each card on both clocks are uploaded by CI for review.
  They are not a pass/fail gate.

### Amendment to ADR-104: the Sent-emails card

ADR-104's surface table put the customer's Sent-emails card on the real
calendar, next to webhook deliveries. The card moves to the entity's
calendar: each email is dated at its `sim_effective_at`, with the real send
time as its Recorded part. Webhook deliveries stay on real time.

### Reversal: hosted-page payment attempts

ADR-102 made a charge attempt created by the settle path (the hosted payment
page, `trigger_source='external'`) "deliberately wall-clock", because "an
interactive act is a wall fact". That rule is reversed. When the settle path
writes the attempt, it stamps `sim_effective_at` from the invoice's clock,
the same as any other act on a pinned invoice. The change touches the settle
path, so it follows the
[money-path playbook](../dev/money-path-robustness-playbook.md).

## Why this design

- **The backend owns the words because the facts live there.** Folds,
  attempt pairing, dunning outcome tokens, `triggered_by`, `billing_reason`
  and `decline_code` never reach the browser. Both false lines quoted in
  Context are a Go writer token and a Go renderer drifting apart; a golden
  test in the same package catches that class. Most titles were already
  built in Go.
- **API callers get the same sentences.** The timelines are public API
  (`api/openapi.yaml`), so a sentence built once serves the dashboard and
  integrators alike.
- **Acts are dated on the entity's calendar too.** A void done today
  (October 2026) would otherwise sort before the February 2027 invoice it
  voided. The Recorded part keeps the real moment on screen.
- **A customer paying is an act on the invoice,** like an operator voiding
  it. Operator credit notes moved off real time for the same reason, in the
  2026-07-29 fix that ADR-104 records. Left on real time, a hosted-page
  attempt sorts among simulated rows by its real time.
- **The Sent-emails card sits under a banner that says the page follows the
  test clock.** Showing unlabeled real times there contradicts the banner.
  The Recorded part still answers "did it go out, and when, really".

## Alternatives considered

- **A. Real time for operator actions, simulated time for engine events.**
  It breaks the order (above). ADR-104 already tried a split by provenance,
  as two lanes, and deleted it after four bug fixes in eight days.
- **B. Build the words in the dashboard.** The bigger move, since most titles
  are already in Go. The browser lacks the facts the words depend on, and API
  callers would get different sentences from the dashboard.
- **C. Relative times with the exact time on hover.** It hides the exact time
  behind a hover, and Velox puts no information behind hover only.
- **D. One credit-note title per refund status** (today's nine titles). The
  title's amount changes meaning between them. One title with the total, and
  meta parts for where each dollar went, says it once.
- **E. Hosted-page attempts stay on real time** (decision 7 answered no).
  They would stay the one known exception to the clock rule, sorting by real
  time among simulated rows.

## Consequences

### Positive

- The same event reads the same on every page and in the API.
- The false lines found in the inventory are fixed by golden-tested backend
  words, with a test that fails if a writer token reaches a fallback.
- A test-clock row differs from its real-clock row by one Recorded part, and
  CI checks it.

### Risks / open items

- **The work ships in steps.** After this record:
  1. An e2e spec that screenshots each activity card on both clocks, and a CI
     upload step.
  2. Backend: `internal/platform/activity` and the invoice lines.
  3. Backend: the subscription lines.
  4. Backend: lines for Dunning events and the email outbox list.
  5. Facts: `actor_type` and `actor_id` on `email_outbox` and
     `invoice_charge_attempts` (one migration, no backfill).
  6. Facts: dates. Trial-end catch-up dated at the trial end, the finalize
     flag for invoices born finalized, and the hosted-page attempt stamp. It
     follows the money-path playbook.
  7. The dashboard's shared activity list, on the invoice page.
  8. The subscription, Dunning and customer pages, then the audit log words.
  9. One release later, deprecate the old time fields.

  Until a step ships, its page shows today's words and times.
- **Legacy rows are not backfilled.** Rows written before migrations
  0162 to 0164 keep their only time and show no Recorded part. They are
  pre-launch fixture data (the ADR-104 precedent).
- **Check-basis rows on a test clock are coarse.** A test clock checks once
  per advance, so a long jump dates a spend-threshold crossing at the end of
  the jump. Advancing in smaller steps gives finer dates.
- **Card declines and card refunds are not covered by the browser tests,**
  which run without Stripe keys. Golden tests and the release checklist's
  Stripe test-mode walk cover them.
- **A credit note's refund can now be `unknown`** (#1267, merged after the
  standard was written). The standard names problem lines for failed,
  pending and never-ran refunds only. The invoice-lines PR gives `unknown`
  its own; the golden file over every refund status fails until it does.
- **Card disputes add invoice lines the standard has not weighed** (#1287,
  merged after the standard was written). The invoice page now shows
  "Payment disputed" and four closing titles such as "Dispute lost: the bank
  took the payment back". The meta shows the Stripe dispute id and Stripe's
  reason code. The invoice-lines PR words them under the rules above.
  Their dates are an open question. #1287 dates both rows at Stripe's real
  time on every invoice, on purpose: its code comment calls them real-world
  Stripe instants. On a test-clock invoice that breaks rule 6, the way
  hosted-page attempts do today. Decide before the dates PR: date disputes
  on the invoice's clock, like hosted-page attempts (decision 7), or name
  them as the one known exception.
- **Not in this standard,** each with the trigger that brings it back:
  - A customer-level activity timeline. Trigger: an operator asks "what
    happened to this customer" and the audit log filter is not enough.
  - Renewals and payments on the subscription card. The subscription page's
    invoice list covers them today.
  - A line when the final action fails after retries run out. Trigger: an
    operator misses a stuck final action. Then one event on the first
    failure only, not one per 24-hour attempt.
  - A line when a subscription's billing run fails. Trigger: an operator
    needs the history of the failures, not just the current banner.

## References

- [ADR-030](030-simulated-time-everywhere-on-clock-pinned-entities.md):
  simulated time on clock-pinned entities.
- [ADR-090](090-audit-in-tx-emission.md): in-transaction audit rows and their
  frozen action strings, the style the subscription action constants follow.
- [ADR-102](102-invoice-charge-attempts.md): charge attempts as rows.
- [ADR-104](104-one-activity-lane-entity-calendar.md): one activity lane on
  the entity's calendar, and its ordering rule.
- [ADR-112](112-dunning-exhaustion-settles-two-questions.md): the dunning
  escalation outcome tokens.
- Writing rules: [Activity lines](../dev/writing-style.md#activity-lines).
