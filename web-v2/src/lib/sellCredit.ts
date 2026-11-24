// The Sell credit dialog's decisions. Selling credit makes a top-up: an
// unpaid draft with one line, which its payment issues and pays. The dialog
// only creates it. The top-up's own page then opens the confirm for the way
// the customer pays, so that confirm is the one place money or an email
// moves.

import { dollarsToWholeCents } from './priceInput'

// creditValidityText is how long the credit lasts, in the words the
// customer's payment page and emails use for the same setting
// (topup_credit_valid_days; null or unset: it never expires).
export function creditValidityText(validDays: number | null | undefined): string {
  if (validDays == null) return 'It never expires.'
  if (validDays === 1) return 'It is valid for 1 day after payment.'
  return `It is valid for ${validDays} days after payment.`
}

export type SellCreditAmounts =
  | { ok: true; payCents: number; creditCents: number }
  | { ok: false; errors: { pay?: string; credit?: string } }

// sellCreditAmounts reads the two amounts the operator typed, in currency
// units. An empty credit box means the customer gets what they pay.
export function sellCreditAmounts(payText: string, creditText: string): SellCreditAmounts {
  const errors: { pay?: string; credit?: string } = {}
  const pay = readAmount(payText)
  if (typeof pay === 'string') errors.pay = pay
  const credit = creditText.trim() === '' ? pay : readAmount(creditText)
  if (typeof credit === 'string' && creditText.trim() !== '') errors.credit = credit
  if (typeof pay === 'number' && typeof credit === 'number') return { ok: true, payCents: pay, creditCents: credit }
  return { ok: false, errors }
}

function readAmount(text: string): number | string {
  const cents = dollarsToWholeCents(text)
  if (cents === null) return 'Enter an amount like 100 or 100.50'
  if (cents === 0) return 'Enter an amount greater than 0'
  return cents
}

// discountLine says, in one line, when the credit differs from the price:
// the discount case, or the rarer one where the customer pays more than they
// get. Null when the two are equal.
export function discountLine(payCents: number, creditCents: number, money: (cents: number) => string): string | null {
  if (creditCents > payCents) return `They get ${money(creditCents - payCents)} more credit than they pay.`
  if (creditCents < payCents) return `They pay ${money(payCents - creditCents)} more than the credit they get.`
  return null
}

export type PayChoice = 'send_link' | 'charge_card' | 'received' | 'later'

export interface PayOption {
  value: PayChoice
  label: string
  // 'checking' while a fact it needs is still loading.
  state: 'available' | 'unavailable' | 'checking'
  // Why the option is unavailable, or what happens after Create top-up.
  note: string
}

// The facts the options depend on. undefined: still loading.
export interface PayFacts {
  // No customer is picked yet (the Credit page's picker).
  noCustomer?: boolean
  // '' when the customer has no email address.
  customerEmail: string | undefined
  // Email can be sent from this server (SMTP).
  smtpConfigured: boolean | undefined
  // The server has the address of the customer's payment page
  // (HOSTED_INVOICE_BASE_URL), so a link can be built.
  paymentPageSet: boolean | undefined
  // Stripe is connected in the mode the operator is in.
  stripeConnected: boolean | undefined
  livemode: boolean
  // The customer's default saved card; null when they have none.
  card: { card_last4?: string | null } | null | undefined
  // A read behind the link's facts, or the card's, failed: the option is not
  // offered on a guess.
  linkFactsFailed?: boolean
  cardFactsFailed?: boolean
}

// payOptions lists the ways the customer can pay, in order, each with what
// it needs. The link needs the customer's email, email on this server, the
// payment page's address and Stripe to take the card: the same checks the
// server's send makes. A charge needs a saved card and Stripe.
export function payOptions(f: PayFacts): PayOption[] {
  const mode = f.livemode ? 'live' : 'test'
  const stripeMissing = `Stripe isn't connected in ${mode} mode.`

  let link: PayOption = { value: 'send_link', label: 'Send them the payment link', state: 'available', note: 'Next, you check the email and send it.' }
  const linkBlock = f.customerEmail !== undefined && !f.customerEmail.trim() ? 'The customer has no email address.'
    : f.smtpConfigured === false ? "Email isn't set up on this server."
      : f.paymentPageSet === false ? 'This server has no payment page address (HOSTED_INVOICE_BASE_URL).'
        : f.stripeConnected === false ? stripeMissing
          : null
  if (linkBlock) link = { ...link, state: 'unavailable', note: linkBlock }
  else if (f.linkFactsFailed) link = { ...link, state: 'unavailable', note: "Couldn't check whether the link can be sent." }
  else if (f.customerEmail === undefined || f.smtpConfigured === undefined || f.paymentPageSet === undefined || f.stripeConnected === undefined) {
    link = { ...link, state: 'checking', note: 'Checking whether the link can be sent…' }
  }

  const last4 = f.card?.card_last4
  let card: PayOption = {
    value: 'charge_card',
    label: last4 ? `Charge the card ending ${last4}` : 'Charge their saved card',
    state: 'available',
    note: 'Next, you confirm the charge.',
  }
  if (f.card === null) card = { ...card, state: 'unavailable', note: 'The customer has no saved card.' }
  else if (f.stripeConnected === false) card = { ...card, state: 'unavailable', note: stripeMissing }
  else if (f.cardFactsFailed) card = { ...card, state: 'unavailable', note: "Couldn't check for a saved card." }
  else if (f.card === undefined || f.stripeConnected === undefined) card = { ...card, state: 'checking', note: 'Checking for a saved card…' }

  if (f.noCustomer) {
    const pick = 'Pick a customer first.'
    link = { ...link, state: 'checking', note: pick }
    card = { ...card, state: 'checking', note: pick }
  }

  return [
    link,
    card,
    { value: 'received', label: 'I already received the payment', state: 'available', note: 'Next, you record the payment, such as a bank transfer.' },
    { value: 'later', label: 'Decide later', state: 'available', note: 'The top-up waits on its page, where you can copy its link.' },
  ]
}

// The confirm the top-up's page opens on arrival, named in its address
// (?next=). Decide later opens none.
export type ArrivalConfirm = 'email' | 'collect' | 'record'

export function confirmAfterCreate(choice: PayChoice): ArrivalConfirm | null {
  switch (choice) {
    case 'send_link': return 'email'
    case 'charge_card': return 'collect'
    case 'received': return 'record'
    default: return null
  }
}

// arrivalConfirm reads ?next= on the invoice page: the confirm to open, only
// for an unpaid top-up, which is what the Sell credit dialog lands on. Any
// other value, or any other invoice, opens nothing.
export function arrivalConfirm(next: string | null, inv: { status: string; billing_reason?: string | null }): ArrivalConfirm | null {
  if (inv.status !== 'draft' || inv.billing_reason !== 'topup') return null
  return next === 'email' || next === 'collect' || next === 'record' ? next : null
}
