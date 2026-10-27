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
  if (subscription.status === 'canceled') {
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

// What finalizing a draft does, for the confirmation before it (#1109). It
// follows the server: invoice.Service.Finalize issues the invoice (a manual
// one gets its tax worked out first), and the finalize route then collects it
// at once through billing.CollectInvoice: the customer's credit balance first,
// then the card on file, or with no card an email asking for one.
//   - hasCard: whether the customer has a default card; undefined while the
//     page has not loaded it.
//   - hasEmail: whether the customer has an email address.
//   - canEmailSetupLinks: whoami.can_email_setup_links. The no-card email
//     needs SMTP and PAYMENT_UPDATE_URL; without them nothing is sent, so
//     only true promises one.
//   - taxAtFinalize: a manual invoice, whose tax is worked out at finalize.
//   - stripeConnected: whether Stripe is connected in this mode (GET
//     /settings/stripe has a row for it, the same test as the server's
//     StripeClients.HasFor); undefined while not known. Without it no card
//     can be charged, a setup link is refused (409 stripe_not_connected) and
//     the update-payment page says adding a card isn't available, so the
//     confirmation promises none of them.
//   - collectionPaused: the invoice's subscription has collection paused
//     now (pause_collection in force, as domain.PauseCollection.InForceAt).
//     The finalize still queues the invoice, but the auto-charge claim skips
//     a paused subscription's invoices (notPausedForCollection), so nothing
//     is collected until collection resumes.
export function finalizeConfirmLines(o: {
  collectionPaused: boolean
  stripeConnected: boolean | undefined
  hasCard: boolean | undefined
  hasEmail: boolean
  canEmailSetupLinks: boolean | undefined
  taxAtFinalize: boolean
}): string[] {
  const lines = ["The invoice is issued. You can't edit it afterwards, and the only way to cancel it is to void it."]
  if (o.taxAtFinalize) lines.push('Tax is worked out now, so the total may change.')
  const collect = "Velox then collects payment right away. It uses the customer's credit balance first."
  const canEmail = o.canEmailSetupLinks === true
  if (o.collectionPaused) {
    lines.push('Collection is paused on this subscription, so nothing is charged now. Velox collects payment once collection resumes.')
  } else if (o.stripeConnected === false) {
    lines.push(`${collect} Stripe isn't connected in this mode, so no card can be charged or added online. Connect Stripe under Settings to collect by card.`)
  } else if (o.stripeConnected === undefined) {
    lines.push(`${collect} If anything is left to pay, it charges their card on file if they have one and if Stripe is connected.`)
  } else if (o.hasCard === true) {
    lines.push(`${collect} If anything is left to pay, it charges their card on file.`)
  } else if (o.hasCard === false) {
    if (!o.hasEmail) {
      lines.push(`${collect} The customer has no card on file and no email address, so nothing is charged.`)
    } else if (canEmail) {
      lines.push(`${collect} The customer has no card on file, so if anything is left to pay, Velox emails them a link to add one.`)
    } else {
      lines.push(`${collect} The customer has no card on file and this server can't email setup links, so if anything is left to pay, copy a setup link from the customer page and send it to them.`)
    }
  } else if (canEmail) {
    lines.push(`${collect} If anything is left to pay, it charges their card on file, or emails them a link to add one.`)
  } else {
    lines.push(`${collect} If anything is left to pay, it charges their card on file if they have one.`)
  }
  return lines
}

// collectionPausedNow says whether a subscription's collection pause holds
// collection at nowMs (the page's effective now, test clock aware). It is the
// server's collection rule (notPausedForCollection, InForceAt): a pause with
// no resumes_at holds, a timed one holds strictly before resumes_at. Unlike
// collectionPausedFlag it does not look at the subscription's status: the
// claim skips a paused row's invoices whatever its status.
export function collectionPausedNow(pause: { resumes_at?: string | null } | null | undefined, nowMs: number): boolean {
  if (!pause) return false
  return !pause.resumes_at || new Date(pause.resumes_at).getTime() > nowMs
}
