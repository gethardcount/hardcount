// Customer-facing copy for the public "update payment method" page. The
// customer arrives from a failed-payment email; they never see a server
// message or an error code, only what happened and what they can do.

export interface LinkErrorCopy {
  title: string
  body: string
  // askForNewLink is true when a new link is the way forward, so the page
  // adds "ask <company> for a new link".
  askForNewLink: boolean
}

// linkErrorCopy is the page shown when the link cannot be used. `status` is
// the HTTP status of the link check, or 'missing' when the URL carries no
// link code at all (a truncated copy-paste).
export function linkErrorCopy(status: number | 'missing'): LinkErrorCopy {
  if (status === 'missing') {
    return {
      title: 'This link is incomplete',
      body: 'Open it again from your email.',
      askForNewLink: false,
    }
  }
  // 401: expired, already used, or never valid. 404: the invoice or the
  // customer it points to is gone. Either way the link is dead.
  if (status === 401 || status === 404) {
    return {
      title: 'This link has expired or was already used',
      body: 'Each link to update a payment method works once, for a limited time.',
      askForNewLink: true,
    }
  }
  return {
    title: 'We couldn’t check this link',
    body: 'Please try again in a few minutes.',
    askForNewLink: false,
  }
}

// checkoutErrorCopy is the inline message when "Add payment method" fails.
// `who` is the company name (or its plain fallback).
export function checkoutErrorCopy(code: string | undefined, who: string): string {
  if (code === 'invalid_token') {
    return `This link has expired or was already used. Ask ${who} for a new one.`
  }
  return `We couldn’t open the card form right now. Please try again in a few minutes. If it keeps failing, contact ${who}.`
}
