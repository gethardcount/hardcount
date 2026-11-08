# Refunds a restore's catch-up did not apply

This page handles each refund `hardcount restore catch-up` listed instead of applying, at step 7 of [Restore Hardcount](../restore.md#handle-what-the-catch-up-listed). Back to the [runbook index](index.md).

## Symptom

The catch-up printed lines under `Refunds not applied, because applying them could refund or reverse tax a second time:`. Each names a refund (`re_…`), its amount in the currency's smallest unit, its status, the payment and the invoice, and why it was not applied.

## Why

Each is a refund of a payment the restored database holds, made after the backup, which the restored database does not hold. Until you have handled it, issue no credit note that refunds its invoice: the invoice shows that money as refundable again.

- **`made by Hardcount for credit note hc_cn_…`:** Hardcount made the refund for a credit note the restore lost, or left without the refund's ID. Applied as a webhook, it would be acknowledged with no record.
- **`made in Stripe; the invoice was reported to Stripe Tax`:** someone refunded in Stripe after the backup. The old server may have recorded it as a credit note and reversed its tax in Stripe Tax. Applied again, it would reverse the tax a second time.

## Fix

**`made by Hardcount for credit note hc_cn_…`.** Look up the note: `SET app.bypass_rls = 'on'; SELECT id, status, stripe_refund_id FROM credit_notes WHERE id = 'hc_cn_…';`

- **`issued`, without this refund's ID:** click **Retry refund** on it. Hardcount finds the refund by its metadata and records it, without refunding again.
- **`draft`:** click **Void** on it. Issuing it can refund the customer a second time.
- **No row, and the invoice's `tax_transaction_id` is empty:** record the refund with a credit note as in step 1 of [A refund made in Stripe](refund-made-in-stripe.md#fix). Put the amount in `out_of_band_amount_cents`, in place of `refund_amount_cents` and `stripe_refund_id`, and the refund's ID in the reason.
- **No row, and `tax_transaction_id` is set:** the old server reversed the refund's tax in Stripe Tax when it issued the note, and a new credit note would reverse it again. Record the refund in your own books only.

**`made in Stripe; the invoice was reported to Stripe Tax`.** Look in your Stripe Tax transactions for a reversal of the invoice's `tax_transaction_id` made after the backup time, for this refund's amount. If there is one, record the refund in your own books only. If not, record it with step 1 of [A refund made in Stripe](refund-made-in-stripe.md#fix), which reverses its tax once.

## Verify

Each listed refund is recorded once: as a credit note on its invoice, or, where this page says so, in your own books.
