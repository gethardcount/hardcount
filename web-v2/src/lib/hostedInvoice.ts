// Customer-facing copy for the public (hosted) invoice page. The customer is
// not an operator: they never see a server message or an error code, only
// what happened and what they can do.

export interface HostedTotalsInput {
  status: string
  subtotal_cents: number
  discount_cents: number
  tax_amount_cents: number
  total_amount_cents: number
  credits_applied_cents: number
  amount_paid_cents: number
}

export type HostedTotalRow = 'subtotal' | 'discount' | 'tax' | 'total' | 'credits' | 'paid'

// hostedTotalRows lists the rows above the final amount line. A row only
// appears when it says something new: an invoice with no discount, tax,
// credit or payment showed "Subtotal", "Total" and "Amount due" as three
// identical figures.
export function hostedTotalRows(inv: HostedTotalsInput): HostedTotalRow[] {
  const adjusted = inv.discount_cents > 0 || inv.tax_amount_cents > 0
  const settled = inv.credits_applied_cents > 0 || inv.amount_paid_cents > 0
  const rows: HostedTotalRow[] = []
  if (adjusted) {
    rows.push('subtotal')
    if (inv.discount_cents > 0) rows.push('discount')
    if (inv.tax_amount_cents > 0) rows.push('tax')
  }
  if (adjusted || settled) rows.push('total')
  if (inv.credits_applied_cents > 0) rows.push('credits')
  if (inv.amount_paid_cents > 0) rows.push('paid')
  return rows
}

// payErrorCopy maps a failed "Pay" request to what the customer reads.
// `seller` is the company name, or "" when the tenant has not set one.
// A 409 with any other code means the invoice changed or a payment is
// already in flight: paying again is what the server is protecting against.
export function payErrorCopy(code: string | undefined, seller: string, status = 0): string {
  const who = seller || 'the sender of this invoice'
  if (status === 409 && code !== 'not_payable' && code !== 'online_payment_unavailable' && code !== 'amount_below_minimum') {
    return 'A payment on this invoice is already being processed, so you can\u2019t pay twice. Refresh the page in a minute.'
  }
  switch (code) {
    case 'online_payment_unavailable':
    case 'stripe_unavailable':
      return `Online payment isn't available for this invoice. Contact ${who} to pay another way.`
    case 'not_payable':
      return 'This invoice can no longer be paid online. Refresh the page to see its current status.'
    // Stripe refused the amount as below its minimum charge: no card can pay
    // it, so "try again" would be false.
    case 'amount_below_minimum':
      return belowMinimumCopy(who)
    case 'invalid_state':
      return 'A payment on this invoice is already being processed, so you can’t pay twice. Refresh the page in a minute.'
    default:
      return 'We couldn’t start the payment. Nothing was charged. Please try again in a few minutes.'
  }
}

// payUnavailableCopy is the line shown in place of the Pay button when the
// invoice is owed but can't be paid online: the amount is below Stripe's
// minimum charge, or (any other reason) the seller has no payment provider
// connected.
export function payUnavailableCopy(seller: string, reason?: string): string {
  const who = seller || 'the sender of this invoice'
  if (reason === 'amount_below_minimum') return belowMinimumCopy(who)
  return `Online payment isn't set up for this invoice. Contact ${who} to arrange payment.`
}

function belowMinimumCopy(who: string): string {
  return `This amount is below the minimum for a card payment, so it can\u2019t be paid online. Contact ${who} to pay another way.`
}

// loadErrorCopy is the page shown when the link does not resolve. A 404 is
// the common case (a mistyped or rotated link); anything else is a fault on
// our side the customer can retry.
export function loadErrorCopy(status: number): { title: string; body: string } {
  if (status === 404) {
    return {
      title: 'This link isn’t valid',
      body: 'It may have been mistyped, or replaced with a newer link. Ask the sender for the current link to this invoice.',
    }
  }
  return {
    title: 'We couldn’t load this invoice',
    body: 'Please try again in a few minutes.',
  }
}
