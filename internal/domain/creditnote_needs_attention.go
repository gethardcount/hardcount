package domain

// CreditNoteRefundNeedsAttentionSQL is the refunds-needing-attention predicate, shared by the
// list filter and the dashboard count so the two never disagree:
//   - failed: terminal, the customer was not refunded;
//   - pending for over 72h (about 3 business days): genuinely stuck, while
//     fresh pending is normal async settlement;
//   - none with a refund amount for over an hour: the refund leg ran but its
//     outcome was never recorded (a failed write after Stripe answered, or a
//     crash in that window). RetryRefund recovers it by searching Stripe for
//     the refund before creating one.
//
// Test-clock credit notes are not an operator obligation.
const CreditNoteRefundNeedsAttentionSQL = `status = 'issued' AND (refund_status = 'failed' OR (refund_status = 'pending' AND updated_at < now() - interval '72 hours') OR (refund_status = 'none' AND refund_amount_cents > 0 AND updated_at < now() - interval '1 hour')) AND is_simulated = false`
