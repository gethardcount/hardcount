# ADR-127: Each charge is recorded on its invoice before Velox calls Stripe, and an interrupted one is checked with Stripe's list API

**Date:** 2026-10-19
**Status:** Accepted
**Supersedes:** [ADR-106](106-charge-intent-ledger.md) (the parked charge-intent ledger)
**Amends:** [ADR-105](105-charge-idempotency-key-seed.md) (the counter also moves on a release), [ADR-107](107-unknown-is-terminal-until-a-human.md) (an unclear answer no longer parks), [ADR-112](112-dunning-exhaustion-settles-two-questions.md) (dunning's final action waits for the record)

## Summary

Just before Velox calls Stripe, the charge writes a record on the invoice in a commit of its own: when, which attempt, the exact idempotency key, and the Stripe customer. The record is cleared once the charge's result is saved, or when nothing was sent. While it exists no path charges the invoice again, and settling actions refuse or wait. A crash, a deploy or a deadline that ends during the call leaves the record. From 10 minutes on, the reconciler lists the customer's PaymentIntents at Stripe and matches the key. It records a payment Stripe took or a decline, or releases the invoice to be charged normally when Stripe never got the request. If Stripe cannot be read for 24 hours, the invoice parks (ADR-107). The result is one payment per attempt after any interruption, with no webhook and no person needed, except after 24 hours without Stripe.

## Context

Issue [#976](https://github.com/getvelox/velox/issues/976). Velox charges a card in three steps: take a 5-minute hold on the invoice, call Stripe, write the result. Nothing durable said "a charge was sent" until the result was written. Two things left the invoice looking unpaid after Stripe may have taken the money:

- the process stopped between the call and the write (a crash, a deploy, a SIGKILL);
- far more often, the caller's own deadline ended during the call. A dunning retry allows 15 seconds, but the Stripe HTTP client allows 30. Auto-charge allows 30 seconds, measured from before the call. **Collect payment** ends when the browser disconnects. The result write then ran on the expired context and failed.

When the hold ended, the invoice was chargeable again. The next charge reused the old PaymentIntent only if it sent the same idempotency key with the same parameters, and Stripe still remembered the key. Four things broke that:

| Way to a second payment | Why |
|---|---|
| The default card changed | The card is part of the key |
| Another charge kind | Auto-charge and Collect send no suffix; a dunning retry sends `_dunning_retry` |
| More than 24 hours | "You can remove keys from the system automatically after they're at least 24 hours old. We generate a new request if a key is reused after the original is pruned" ([Stripe](https://docs.stripe.com/api/idempotent_requests)) |
| The hosted page | Its claim refused only while the hold was live, and Stripe Checkout creates its own PaymentIntent with no key |

ADR-106 designed a ledger that replayed the stored request. It was parked for ADR-107, which parks an unclear charge for a person, and ADR-108, which searches Stripe for a parked one. Neither covers this case: a crash parks nothing, and a timeout's failed write parks nothing either. ADR-106's trigger, production cutover, fired on 2026-10-18.

Facts the design relies on, from Stripe's documentation (checked 2026-10-18):

- **Results are saved once a request starts executing.** Stripe saves a key's result "only after the execution of an endpoint begins", "including 500 errors" ([Stripe](https://docs.stripe.com/api/idempotent_requests)).
- **List is consistent; Search is not.** "Don't use search for read-after-write flows… use the various list APIs… These APIs aren't subject to the availability delays." Search "isn't available to businesses in India" ([Stripe](https://docs.stripe.com/search)).
- **List PaymentIntents** filters by `customer` and `created[gte]`, pages by cursor, and returns whole objects with `metadata` and `status` ([Stripe](https://docs.stripe.com/api/payment_intents/list)).

And from stripe-go v82.5.1: `shouldRetry` resends any non-Stripe error (a reset connection, the 30-second HTTP timeout) under the same key, up to `DefaultMaxNetworkRetries = 2`, while the context lives. So a 409 "key in use" or a 429 can answer a resend of a request Stripe is still running.

## Decision

**Record first.** `InvoiceUpdater.BeginChargeAttempt` writes four nullable columns on `invoices` (migration 00186): `charge_started_at` (the database's `now()`), `charge_started_seq` (the `charge_attempt_seq` the key was built from), `charge_started_key` (the exact key Stripe receives) and `charge_started_stripe_customer`. It is a CAS (an `UPDATE` that applies only while the row is still in the expected state): no record yet, the same seq, finalized, `pending` or `failed`, and a live charge hold. If it fails or is refused, Stripe is not called and the hold is released. A CHECK keeps the four columns all set or all `NULL` (00186 adds it `NOT VALID`, 00187 validates it). A partial index serves the check (00188).

**Send the key as metadata.** Every charge sends `velox_attempt_key = <the exact key>`. `ChargeIdempotencyKey` builds the whole key, card included, in one place; the value is unchanged.

**A time limit on sending.** From the record write on, the charge has at most 2 minutes (`chargeSendBudget`). The callers' shorter deadlines still apply. There is no `WithoutCancel`: a cancelled call keeps the record.

**One rule for clearing.** The record is cleared only once the attempt's result is saved on the invoice, or when nothing was sent (the circuit breaker was open, or the credentials could not be read: `ErrNotSent`). Every unclear answer keeps the record and writes nothing else: a timeout, a cancelled request, a network error, a 5xx, a 409 "key in use", a 429. The one exception is the 409 "same key, different parameters" (`idempotency_error`), which parks the invoice as before. Since every key is sent with one set of parameters, that answer comes only from an attempt in flight across the upgrade that added `velox_attempt_key`, whose PaymentIntent the check could not match.

**Blocked while recorded.**

- The three charge claims (`chargeNotHeldSQL`) and the hosted-page claim (`ChargeHeldSQL`) refuse the invoice.
- `Invoice.PaymentInFlight()` is true while the record exists, so void, write-off, offline payment, credit notes, Collect, both invoice emails and the hosted Pay button refuse or wait. The refusal says: "A charge on this invoice is being confirmed with Stripe. Try again later. An interrupted charge is confirmed within about an hour."
- An automatic credit note is drafted and deferred (ADR-059). A cancel or plan swap relieves an unused prebill through that deferred note instead of voiding it.
- Dunning holds its final action (ADR-112). Its due-run pickers and the deferred credit-note scans skip the invoice (`PaymentInFlightSQL`).

**Ask Stripe.** `Reconciler.sweepInterruptedCharges` runs first in every reconciler run, on the leader, behind the breaker. It lists records older than 10 minutes whose last failed check is older than 10 minutes. For each it retrieves the Stripe customer, which must exist and not be deleted, lists that customer's PaymentIntents from an hour before the record, and keeps the one whose `velox_attempt_key` equals the record's key.

| Stripe's answer | The check does | Record |
|---|---|---|
| Failed read: an error on the customer or any page, a missing or deleted customer, the breaker open, Stripe disconnected | stamp `provider_synced_at`; ask again after 10 minutes | kept |
| Failed reads 24 hours after the record | park the invoice (ADR-107), with the existing CRITICAL; if it moved on, only clear | cleared |
| `succeeded` | `SettleSucceeded`, so a payment on an already-paid invoice raises `payment.duplicate_charge` | cleared |
| `requires_payment_method` or `canceled` | `SettleFailed` with the record's seq as the CAS and the decline code and card (#966); dunning starts | cleared |
| still running, invoice at the record's seq | stamp it `processing`; the processing sweep and the webhook take over | cleared |
| still running, another outcome saved since | `requires_*`: cancel the PaymentIntent. `processing`: wait, it always ends | cleared / kept |
| no match | release: clear the record, and move the counter if it still equals the record's seq | cleared |

**Why "not found" is final.** Four facts hold together. List is consistent. The customer was just shown to exist in this Stripe account, and the key belongs to this attempt alone. The record is at least 10 minutes old, and no request for it can start or be resent more than 2 minutes after it was written. Stripe saves a result for a key only once execution has begun. The counter moves on release because an outcome was saved ("no payment created"), and a fresh key avoids a 500 Stripe may have saved under the old one.

## Why this design

- **Record, then read, instead of replay.** ADR-106 replayed the stored request, which works only while Stripe keeps the key, so it gave up after 12 hours. A consistent read of what Stripe holds has no such limit, and it never sends a second request.
- **One recovery path.** A crash and a timeout leave the same state, so they get the same recovery. Parking an unclear answer needed a person each time; on accounts without Stripe Search the only way out was writing off money that may never have been charged.
- **The read model, not a new table.** Four columns on the invoice row reach every reader that already asks `PaymentInFlight()`, as `invoice_payment_gate.go` planned for ADR-106. There is no new table, so no new RLS (row-level security), and the record commits with nothing else.

## Alternatives considered

- **A. ADR-106's ledger and replay.** A `charge_intents` table that stores the request and replays it. Rejected: about 1,900 lines, a 12-hour replay limit after which a person decides, and a second request on every recovery. The read needs none of that.
- **B. Stripe Search by `velox_invoice_id`.** Rejected as the main read: Search is eventually consistent, so an empty answer proves nothing, and it is not offered in every country. ADR-108 keeps it for parked invoices.
- **C. Detach the charge from the caller's context (`WithoutCancel`).** Rejected: it would override dunning's deliberate 15-second limit, and a cancelled call is already safe because it keeps the record.
- **D. A webhook-only recovery.** Rejected: it depends on the one delivery that may be lost, and a test clock has no forwarder.

## Consequences

### Positive

- One payment per attempt after a crash or a timeout, after a card change, a kind change, any delay, and on the hosted page.
- No webhook needed, and no person needed except after 24 hours without Stripe.
- Four places that said this was handled, and were wrong, are corrected:
  - the payment guide's "Crash after Stripe charged" row;
  - its "Timeout → parked" row;
  - the reconciler driver's "the stable key recovers it";
  - ADR-107's claim that a crash parks.

### Risks and residuals

1. **A charger frozen for more than 10 minutes between the record write and the send**, such as a VM pause that also froze its deadline. Stripe has no request expiry, so this cannot be closed completely. The live-hold check and the 2-minute budget narrow it to a pause after the record write. If the frozen request later succeeds, its settle or webhook raises `payment.duplicate_charge` on the already-paid invoice.
2. **Attempts in flight across the upgrade** have no record and behave as before. Their same-key resend carries the new metadata, gets a 409 `idempotency_error`, and parks (ADR-107/108). Upgrade with the documented 120-second stop grace period: shutdown waits up to 30 seconds for a charge in flight.
3. **Mixed versions in a rolling deploy:** a replica still on the old binary ignores records. A crash on a new replica followed, more than 5 minutes later, by a claim on an old one reopens the card and kind ways to a second payment until the rollout finishes.
4. **Delay:** an interrupted or unclear charge resolves in about 10 to 70 minutes with the hourly tick, or after 24 hours if Stripe cannot be read. Meanwhile the invoice is not charged and settling actions are refused. That is a delay, never a wrong outcome.
5. **A final dunning retry that never reached Stripe still counts.** After a release, the run exhausts on its next tick without that retry having happened, as after a crash before.
6. **A test-clock Advance interrupted mid-charge** holds that invoice until the wall-clock check runs (about 15 minutes locally). The catch-up's no-progress guard reports it.
7. **The dashboard shows no "being confirmed" state.** `payment_status` stays Pending or Failed, and the refusal copy explains. An API field would be a separate change, if integrators ask.

### To verify in Stripe test mode before a release

The release checklist carries these:

- Create a PaymentIntent and list it by customer within a second.
- List for a deleted customer, and for another account's customer.
- Send two concurrent creates on one key, and record the 409's type and code.
- SIGKILL the process during a charge, then confirm the next reconciler run resolves it with one PaymentIntent.

## References

- Design and review record: the `charge-intent-976-design-2026-10-18.md` note in the maintainers' ops repository.
- Code: `internal/payment/stripe.go` (`chargeInvoice`), `internal/payment/reconciler_interrupted.go`, `internal/invoice/postgres_charge_record.go`, `internal/domain/invoice.go` (`PaymentInFlight`, `ChargeNotHeldSQL`).
- Migrations 00186, 00187, 00188.
- Tests: `internal/payment/interrupted_charge_integration_test.go`, `internal/invoice/charge_record_integration_test.go`, `internal/dunning/exhaust_charge_record_integration_test.go`.
- Operators: [Interrupted charges](../ops/runbook/interrupted-charges.md).
