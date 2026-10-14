// What the Stripe card says when the key check after Connect failed. The
// server records Stripe's own sentence (last_verified_error) and whether
// Stripe refused the key or was never reached (last_verified_error_kind);
// before, the card printed Stripe's whole error as JSON under "verification
// failed" whatever the cause.
//
// Dependency-free so node --test can load it.

export interface StripeVerifyNotice {
  title: string
  body: string
  // The toast after Connect points at the notice instead of repeating it.
  toastTitle: string
}

export function stripeVerifyNotice(kind: string | undefined): StripeVerifyNotice {
  if (kind === 'unavailable') {
    return {
      // Not "Couldn't reach Stripe": this kind also covers Stripe answering
      // 429 or 5xx, where it was reached but did not check the key.
      title: "Couldn't check keys with Stripe",
      body: "Stripe didn't answer the check, so these keys weren't tested. Paste them again in a minute to retry.",
      toastTitle: 'Keys saved, not checked yet',
    }
  }
  // 'rejected', and rows that failed before the kind was recorded: those were
  // almost all refused keys, and the advice is the same.
  return {
    title: "Stripe didn't accept these keys",
    body: 'Copy the current secret and publishable keys from your Stripe dashboard and paste them below.',
    toastTitle: "Keys saved, but Stripe didn't accept them",
  }
}

// The toast's sentence: the notice sits above the key form.
export const STRIPE_VERIFY_TOAST_BODY = 'See the message above the form.'
