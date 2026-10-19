# Clawback drafts not issuing

A clawback is a credit note Velox issues when a paid, in-advance period is
cut short, for example by a downgrade. Velox first writes it as a draft,
then issues it. This page is for drafts that pile up. Back to the
[runbook index](index.md).

## Symptom

`velox_creditnote_pending_issue_drafts` shows sustained growth over days.

## Why

- Drafts that wait behind a source payment still in flight sit here
  legitimately, and do not appear in error logs.
- The reconciler skips them on purpose until the source payment settles.
- So alert on growth or age, not presence.

## Check

Is `velox_parked_invoices{disposition="open"}` above zero? It counts the
parked invoices Velox has not ended on schedule, and their drafts wait on
them.

## Fix

If it is above zero, follow [Parked invoices](parked-invoices.md); each
draft is released when its park ends. Otherwise the drafts are waiting on
source payments still in flight, including parks inside their 24 hours,
and need no action.

## Verify

These drafts drain on the next reconciler pass.

Related: [ADR-059](../../adr/059-guard-invoice-mutations-while-payment-in-flight.md)
and the [glossary](../../glossary.md).
