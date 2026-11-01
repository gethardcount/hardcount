// Where Stripe sends a mode's events, and how to get them to a server Stripe
// cannot reach. Stripe delivers webhooks over the internet, so a dashboard on
// this computer (localhost) shows an address Stripe can never call. There the
// Stripe CLI relays the events instead: `stripe listen` holds a connection
// open to Stripe and forwards each event to the local URL, and prints the
// signing secret to paste in step B (docs/guides/connect-stripe.md).
//
// Dependency-free so the unit tests can load it.

export function stripeWebhookUrl(apiOrigin: string, endpointId: string): string {
  return `${apiOrigin}/v1/webhooks/stripe/${endpointId}`
}

// Whether Stripe cannot reach this address: it names this computer.
export function isLocalAddress(url: string): boolean {
  let host: string
  try {
    host = new URL(url).hostname
  } catch {
    return false
  }
  return host === 'localhost' || host.endsWith('.localhost') || host === '127.0.0.1' || host === '[::1]'
}

export function stripeListenCommand(webhookUrl: string): string {
  return `stripe listen --forward-to ${webhookUrl}`
}
