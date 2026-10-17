# ADR-004: Event-Sourced Credit Ledger

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Summary

Credits live in an append-only ledger, `customer_credit_ledger`: every grant, use, adjustment and expiry is a new row, so any balance can be traced to its causes. Entry amounts are never edited, though a grant row keeps a `consumed_cents` counter of how much of it has been used. Each row records `balance_after`, and the balance read sums the rows. Writers for one customer are serialized by a per-customer transaction advisory lock, which replaced the `SELECT ... FOR UPDATE` below because that lock missed a customer's first entry; since [ADR-078](078-prepaid-commits-phase-1.md) every balance writer takes it. Status: active.

## Context
Velox supports customer credits — prepaid balances that reduce invoice amounts before payment collection. Credits can be granted (sales incentives, refunds-as-credit), applied to invoices, adjusted, expired, and reversed (when an invoice is voided). A mutable balance field (`UPDATE credits SET balance = balance - 100`) would be simple but makes it impossible to answer "why does this customer have $47.23 in credits?" without external audit logs.

Billing disputes are common. When a customer questions a charge or a finance team needs to reconcile, the system must produce a complete, tamper-evident history of every credit movement.

## Decision
Credits use an append-only ledger (`customer_credit_ledger` table). Every credit operation — grant, usage, adjustment, expiry, reversal — is an immutable entry with a `balance_after` field computed at write time. The current balance is derived from the latest entry, not stored as a mutable field.

Entry types: `grant` (positive), `usage` (negative, applied to invoice), `adjustment` (positive or negative, manual), `expiry` (negative, automated). Each entry records the `invoice_id` it relates to, enabling per-invoice credit tracking.

Concurrent writes are serialized using `SELECT ... FOR UPDATE` on the customer's existing entries, ensuring `balance_after` is always computed against a consistent state. Reversals (e.g., voiding an invoice) query all `usage` entries for that invoice, sum them, and append a new `grant` entry for the reversed amount.

## Consequences

### Positive
- Complete audit trail: every credit movement is traceable to a cause (invoice, manual adjustment, expiry)
- Reversibility without data loss: voiding an invoice appends a reversal entry rather than mutating history
- Reconciliation: `SUM(amount_cents) WHERE entry_type = 'grant'` must equal `total_granted` — discrepancies are immediately detectable
- No lost-update bugs: concurrent credit operations are serialized, preventing double-spend

### Negative
- Table grows unboundedly per customer (one row per credit event, not one row per customer)
- Balance queries require reading the latest entry rather than a simple column lookup
- Reversal logic must correctly handle partial credit application (credits applied < invoice total)

### Trade-offs
- Storage growth is acceptable: even a high-volume customer generates at most one credit entry per invoice per billing cycle. At 12 invoices/year, a customer accumulates ~50 entries/year including grants and adjustments.

## Alternatives Considered
- **Mutable balance column**: `UPDATE SET balance = balance - amount`. Simple but loses history. A bug that sets the wrong balance is unrecoverable without backups. Rejected for a financial system.
- **Mutable balance + separate audit log**: Two sources of truth that can diverge. The audit log becomes a second-class citizen that developers forget to update. Rejected.
- **Full event sourcing with projections**: Rebuilding balance from all events on every read. Correct but slow for balance lookups. We chose the hybrid: append-only entries with a precomputed `balance_after` on each entry, giving O(1) balance reads with full auditability.
