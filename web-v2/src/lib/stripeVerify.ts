// What the Stripe card says when the key check after Connect failed. The
// server records the reason text (last_verified_error) and whether Stripe
// refused the key or the key could not be checked (last_verified_error_kind;
// tenantstripe describeVerifyError). Before, the card printed Stripe's whole
// error as JSON under "verification failed" whatever the cause.
//
// Dependency-free so node --test can load it.

import type { CalloutTone } from './calloutTone'

export interface StripeVerifyNotice {
  tone: CalloutTone
  title: string
  body: string
  // Label of the collapsed Details holding last_verified_error. Only a
  // 'rejected' row is known to hold Stripe's own sentence; an 'unavailable'
  // row may hold a network error from our side (DNS, dial, timeout), and a
  // row from before migration 0178 may hold the old JSON. So "Stripe's
  // response" is said only when it is true.
  detailsLabel: string
  // The toast after Connect points at the notice instead of repeating it.
  toastLevel: 'error' | 'warning'
  toastTitle: string
}

export function stripeVerifyNotice(kind: string | undefined): StripeVerifyNotice {
  if (kind === 'unavailable') {
    // Covers three cases: Stripe not reached (network), Stripe answered 429,
    // or Stripe answered 5xx. The sentence must hold for all three, so it
    // doesn't claim Stripe "didn't answer". The keys are saved but untested:
    // uncertain, not refused, so amber and a warning toast.
    return {
      tone: 'amber',
      title: "Couldn't check keys with Stripe",
      body: "Stripe couldn't check these keys just now, so they weren't tested. Paste them again in a minute to retry.",
      detailsLabel: 'Details',
      toastLevel: 'warning',
      toastTitle: 'Keys saved, not checked yet',
    }
  }
  // 'rejected', and rows that failed before the kind was recorded: those were
  // almost all refused keys, and the advice is the same.
  return {
    tone: 'red',
    title: "Stripe didn't accept these keys",
    body: 'Copy the current secret and publishable keys from your Stripe dashboard and paste them below.',
    detailsLabel: kind === 'rejected' ? "Stripe's response" : 'Details',
    toastLevel: 'error',
    toastTitle: "Keys saved, but Stripe didn't accept them",
  }
}

// The toast's sentence: the notice sits above the key form.
export const STRIPE_VERIFY_TOAST_BODY = 'See the message above the form.'
