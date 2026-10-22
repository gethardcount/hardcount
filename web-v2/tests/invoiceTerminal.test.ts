// Pins the terminal-invoice rule that has now been missed three times, always
// by forgetting `uncollectible` (see src/lib/invoiceTerminal.ts for the three).
//
// The failure mode these guard is not a crash — it is a customer-facing page
// asking for money on an invoice nobody is collecting, which renders perfectly.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  TERMINAL_INVOICE_STATUSES,
  isTerminalInvoiceStatus,
  invoiceAmountCents,
  invoiceAmountLabel,
  invoiceAmountLine,
  invoiceAwaitingPayment,
  invoiceCanCollect,
  invoiceDueCents,
  invoiceHeadline,
  invoicePaymentInFlight,
  invoicePaymentSucceeded,
  invoiceRowAmount,
} from '../src/lib/invoiceTerminal.ts'

test('uncollectible is terminal — the miss this module exists for', () => {
  assert.equal(isTerminalInvoiceStatus('uncollectible'), true)
  assert.equal(invoiceAmountLabel('uncollectible'), 'Invoice amount')
})

test('all three collection-ending states are terminal', () => {
  for (const s of ['paid', 'voided', 'uncollectible']) {
    assert.equal(isTerminalInvoiceStatus(s), true, `${s} must be terminal`)
  }
  // void / write-off keep amount_due — it is the invoice's amount.
  for (const s of ['voided', 'uncollectible']) {
    assert.equal(invoiceAmountLabel(s), 'Invoice amount', `${s} must relabel the figure`)
  }
})

test('paid zeroes amount_due, so its figure is what is still due, not the invoice amount', () => {
  // "Invoice amount $0.00" on a paid invoice (FLOW I10 walk, 2026-10-11).
  assert.equal(invoiceAmountLabel('paid'), 'Amount due')
})

test('collectible states are NOT terminal — the negative control', () => {
  // Without these, a predicate that returned true for everything would pass
  // every assertion above.
  for (const s of ['finalized', 'draft']) {
    assert.equal(isTerminalInvoiceStatus(s), false, `${s} is still collectible`)
    assert.equal(invoiceAmountLabel(s), 'Amount due', `${s} must still say Amount due`)
  }
})

test('absent / unknown status is treated as collectible, not terminal', () => {
  // invoice_status is optional on the token payload. Defaulting to "terminal"
  // would silently stop asking for payment on invoices that ARE owed — the
  // expensive direction of this mistake.
  for (const s of [undefined, null, '', 'some_future_status']) {
    assert.equal(isTerminalInvoiceStatus(s), false)
    assert.equal(invoiceAmountLabel(s), 'Amount due')
  }
})

test('the exported list and the predicate cannot drift apart', () => {
  for (const s of TERMINAL_INVOICE_STATUSES) {
    assert.equal(isTerminalInvoiceStatus(s), true, `${s} is listed but not detected`)
  }
  assert.equal(TERMINAL_INVOICE_STATUSES.length, 3)
})

// ── The dashboard's reads of an invoice's money and payment fields ──────────
//
// The operator screens used to read payment_status and amount_due_cents raw: a
// voided invoice read "pending" and "$29.00 due" on the subscription page,
// Home, the command palette, the Invoices and Dunning lists and the customer's
// "Last invoice", while its own page said $0.00 (product sweep 2026-10-21, A7 /
// E1 / E2 / E3). These helpers are the one place that reads them; the lint rule
// in eslint.config.js keeps every screen outside src/lib on them.

const base = { currency: 'USD', amount_due_cents: 2900, amount_paid_cents: 0 }

test('invoiceDueCents: a void owes nothing; everything else owes amount_due', () => {
  assert.equal(invoiceDueCents({ ...base, status: 'voided' }), 0)
  // A write-off stays on the books (ADR-110): its own page still says due.
  assert.equal(invoiceDueCents({ ...base, status: 'uncollectible' }), 2900)
  // Negative controls: open and draft invoices owe their amount.
  assert.equal(invoiceDueCents({ ...base, status: 'finalized' }), 2900)
  assert.equal(invoiceDueCents({ ...base, status: 'draft' }), 2900)
  // Paid: the settle zeroed amount_due.
  assert.equal(invoiceDueCents({ ...base, status: 'paid', amount_due_cents: 0, amount_paid_cents: 2900 }), 0)
})

test('invoiceAmountCents: a list row shows the invoice amount, not what is owed', () => {
  // A void keeps amount_due; the Invoices and Dunning lists show it under
  // "Amount" (sign-off of #1328: every void read $0.00 there).
  assert.equal(invoiceAmountCents({ ...base, status: 'voided' }), 2900)
  assert.equal(invoiceAmountCents({ ...base, status: 'uncollectible' }), 2900)
  // Paid: what paid it, card plus customer credit; amount_due was zeroed.
  assert.equal(invoiceAmountCents({ ...base, status: 'paid', amount_due_cents: 0, amount_paid_cents: 1900, credits_applied_cents: 1000 }), 2900)
  // Open and draft: amount_due.
  assert.equal(invoiceAmountCents({ ...base, status: 'finalized', amount_due_cents: 1900, credits_applied_cents: 1000 }), 1900)
  assert.equal(invoiceAmountCents({ ...base, status: 'draft' }), 2900)
  // Negative control: what is owed on the same void is still nothing.
  assert.equal(invoiceDueCents({ ...base, status: 'voided' }), 0)
})

test('invoiceRowAmount says what the figure is, and a closed invoice is never "due"', () => {
  assert.equal(invoiceRowAmount({ ...base, status: 'finalized' }), '$29.00 due')
  // Paid shows what was paid, not the zeroed amount_due ("succeeded $0.00").
  assert.equal(invoiceRowAmount({ ...base, status: 'paid', amount_due_cents: 0, amount_paid_cents: 2900 }), '$29.00 paid')
  // Void, write-off and draft: the invoice amount, bare; the badge beside it says which.
  for (const status of ['voided', 'uncollectible', 'draft']) {
    assert.equal(invoiceRowAmount({ ...base, status }), '$29.00', status)
  }
  // In the invoice's own currency.
  assert.equal(invoiceRowAmount({ ...base, currency: 'EUR', status: 'finalized' }), '€29.00 due')
})

test('invoiceRowAmount: customer credit that settled a paid invoice counts as paid', () => {
  // Credit lowers amount_due and leaves amount_paid alone, so an invoice a
  // prepaid commit paid in full settles with amount_paid 0 and read "$0.00
  // paid" (review of #1328). The invoice page lists the credit as paying it.
  const paid = { ...base, status: 'paid', amount_due_cents: 0 }
  assert.equal(invoiceRowAmount({ ...paid, amount_paid_cents: 0, credits_applied_cents: 2900 }), '$29.00 paid')
  // Part credit, part card.
  assert.equal(invoiceRowAmount({ ...paid, amount_paid_cents: 1900, credits_applied_cents: 1000 }), '$29.00 paid')
  // Negative control: a credit note that left nothing due paid nothing.
  assert.equal(invoiceRowAmount({ ...paid, amount_paid_cents: 0, credits_applied_cents: 0 }), '$0.00 paid')
  // Applied credit on an open invoice is not money due or paid on the row.
  assert.equal(invoiceRowAmount({ ...base, status: 'finalized', amount_due_cents: 1900, credits_applied_cents: 1000 }), '$19.00 due')
})

test('invoiceAmountLine and invoiceHeadline keep the public pages’ figures', () => {
  // The amount line: amount_due under the status-aware label.
  assert.deepEqual(invoiceAmountLine({ ...base, status: 'voided' }), { label: 'Invoice amount', cents: 2900 })
  assert.deepEqual(invoiceAmountLine({ ...base, status: 'finalized' }), { label: 'Amount due', cents: 2900 })
  // The headline of a paid invoice is what was paid.
  assert.deepEqual(
    invoiceHeadline({ ...base, status: 'paid', amount_due_cents: 0, amount_paid_cents: 2900 }),
    { label: 'Amount paid', cents: 2900 },
  )
  assert.deepEqual(invoiceHeadline({ ...base, status: 'uncollectible' }), { label: 'Invoice amount', cents: 2900 })
})

test('payment predicates: collect, awaiting payment, in flight', () => {
  const open = { ...base, status: 'finalized', payment_status: 'failed' }
  assert.equal(invoiceCanCollect(open), true)
  assert.equal(invoiceCanCollect({ ...open, amount_due_cents: 0 }), false)
  assert.equal(invoiceCanCollect({ ...open, payment_status: 'succeeded' }), false)
  assert.equal(invoiceCanCollect({ ...open, payment_status: 'processing' }), false)
  // A parked charge (unknown) is not collectible: it may already have been taken.
  assert.equal(invoiceCanCollect({ ...open, payment_status: 'unknown' }), false)
  for (const status of ['voided', 'uncollectible', 'paid', 'draft']) {
    assert.equal(invoiceCanCollect({ ...open, status }), false, status)
  }

  // Awaiting payment: open, nothing succeeded or processing. A parked charge
  // still awaits (mark uncollectible is its way out, ADR-107).
  assert.equal(invoiceAwaitingPayment(open), true)
  assert.equal(invoiceAwaitingPayment({ ...open, payment_status: 'unknown' }), true)
  assert.equal(invoiceAwaitingPayment({ ...open, payment_status: 'processing' }), false)
  assert.equal(invoiceAwaitingPayment({ ...open, payment_status: 'succeeded' }), false)
  assert.equal(invoiceAwaitingPayment({ ...open, status: 'voided' }), false)

  assert.equal(invoicePaymentInFlight({ ...open, payment_status: 'processing' }), 'processing')
  assert.equal(invoicePaymentInFlight({ ...open, payment_status: 'unknown' }), 'unknown')
  assert.equal(invoicePaymentInFlight(open), null)
  assert.equal(invoicePaymentSucceeded({ ...open, payment_status: 'succeeded' }), true)
  assert.equal(invoicePaymentSucceeded(open), false)
})
