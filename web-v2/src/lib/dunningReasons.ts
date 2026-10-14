// Plain words for the reason tokens on dunning runs and their timeline
// events. The server stores stable machine tokens (domain/dunning.go); the
// Dunning page used to print them as-is ("no_payment_method",
// "subscription_paused+invoice_marked_uncollectible").
//
// run.reason is why recovery STARTED, not a payment failure: a card-less run
// never charged anything, so a red "Last failure reason" box was wrong.

const LABELS: Record<string, string> = {
  // Start causes (DunningStartCause)
  payment_failed: 'Card payment failed',
  no_payment_method: 'No payment method on file',
  // Retry and resolve outcomes
  succeeded: 'Payment succeeded',
  payment_recovered: 'Payment recovered',
  invoice_paid: 'Invoice paid',
  invoice_voided: 'Invoice voided',
  invoice_uncollectible: 'Invoice written off',
  invoice_not_collectible: 'Invoice written off',
  manually_resolved: 'Resolved by hand',
  retries_exhausted: 'All retries used',
  action_failed: 'Final step failed, will try again',
  // Final actions at exhaustion (DunningEscalationOutcome.Reason, joined by "+")
  subscription_paused: 'Subscription paused',
  subscription_canceled: 'Subscription canceled',
  invoice_marked_uncollectible: 'Invoice written off',
}

const TOKEN = /^[a-z][a-z_]*$/

// dunningReasonLabel returns the plain label for a reason. Joined outcomes
// ("a+b") become "A · B". "invoice <resolution>" (ResolveByInvoice) maps its
// resolution. Free text, such as a card decline message from Stripe, is
// returned unchanged: it is already words.
export function dunningReasonLabel(reason: string): string {
  const r = reason.trim()
  if (!r) return ''
  if (r.startsWith('invoice ') && TOKEN.test(r.slice('invoice '.length))) {
    return dunningReasonLabel(r.slice('invoice '.length))
  }
  const parts = r.split('+')
  if (!parts.every(p => TOKEN.test(p))) return r
  return parts.map(p => LABELS[p] ?? humanize(p)).join(' · ')
}

function humanize(token: string): string {
  const words = token.replace(/_/g, ' ')
  return words.charAt(0).toUpperCase() + words.slice(1)
}
