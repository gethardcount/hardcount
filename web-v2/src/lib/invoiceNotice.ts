// What the invoice page shows above the invoice, and the lines inside the
// written-off notice that depend on real state. Plain data in, plain data
// out, so the unit tests can check every branch without rendering.

// The page shows at most one notice above the invoice (the most urgent one).
// Two can apply:
//   - the server's attention notice (invoice.attention), and
//   - the written-off notice, drawn by the page for status uncollectible.
//
// On a written-off invoice the server raises attention only in three cases,
// and each one outranks the plain "Written off" explanation:
//   - a payment anomaly (money that doesn't reconcile, critical),
//   - prepaid credit still spendable from the unpaid invoice (money at risk),
//   - a payment still being confirmed, whose sentence already says the
//     invoice is written off and warns against charging it again (offering
//     Record payment beside it would invite recording money twice).
// So attention wins whenever it is present.
export type InvoiceNotice = 'attention' | 'written_off' | null

export function pickInvoiceNotice(invoice: { status: string; attention?: unknown | null }): InvoiceNotice {
  if (invoice.attention) return 'attention'
  if (invoice.status === 'uncollectible') return 'written_off'
  return null
}

// The written-off notice's subscription line. Writing an invoice off does not
// stop its subscription (ADR-112), and dunning's default final step pauses
// collection, so the page must say which of the three it actually is instead
// of the old fixed "The subscription remains active".
export type WrittenOffSubscriptionState = 'billing' | 'paused' | 'ended'

export interface WrittenOffSubscriptionLine {
  state: WrittenOffSubscriptionState
  text: string
  // Whether the line offers [Open subscription]: only while it still bills,
  // because that is the case where the operator may want to stop it.
  openSubscription: boolean
}

export function writtenOffSubscriptionLine(
  subscription: { status: string; pause_collection?: unknown | null } | null | undefined,
): WrittenOffSubscriptionLine | null {
  // A one-off invoice has no subscription, so there is nothing to say.
  if (!subscription) return null
  if (subscription.status === 'canceled' || subscription.status === 'archived') {
    return { state: 'ended', text: 'The subscription is canceled.', openSubscription: false }
  }
  if (subscription.pause_collection) {
    return { state: 'paused', text: 'Collection on the subscription is paused.', openSubscription: false }
  }
  return { state: 'billing', text: 'The subscription is still billing.', openSubscription: true }
}

// The written-off notice's "No card on file" line offers to email the
// customer a link to add a card only when that email can really go out: the
// customer has an address AND this server can send setup links (SMTP and
// PAYMENT_UPDATE_URL, reported by whoami). A written-off invoice carries no
// server attention notice to decide this, so the page decides it here.
// Otherwise the line says why and offers the customer page, whose "Add
// payment method" dialog copies the link to share directly. Unknown server
// support (whoami still loading or failed) is treated as "can't": offering a
// button that may fail is the lie this exists to remove.
export interface WrittenOffCardLine {
  action: 'email_link' | 'open_customer'
  // Why the email button is missing, or null when it is offered.
  note: string | null
}

export function writtenOffCardLine(
  customerEmail: string | null | undefined,
  canEmailSetupLinks: boolean | undefined,
): WrittenOffCardLine {
  if (!customerEmail) {
    return { action: 'open_customer', note: 'The customer has no email address, so share a link from the customer page.' }
  }
  if (canEmailSetupLinks !== true) {
    return { action: 'open_customer', note: "This server can't email setup links, so copy one from the customer page." }
  }
  return { action: 'email_link', note: null }
}

// The message is one sentence per line, up to three lines split on "\n": the
// cause, then facts that must show up front (the setup-link email, prepaid
// credit at risk).
export function messageLines(message: string | undefined | null): string[] {
  return (message ?? '').split('\n').map(l => l.trim()).filter(Boolean)
}

// Where "Open in Stripe" goes for a payment: the payment in the account's
// Stripe dashboard, under /test/ for test-mode data.
export function stripePaymentUrl(paymentIntentId: string, livemode: boolean): string {
  return `https://dashboard.stripe.com/${livemode ? '' : 'test/'}payments/${encodeURIComponent(paymentIntentId)}`
}
