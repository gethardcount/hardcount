// listPathFor is where a test/live switch takes you from a record's own
// page: that record's list in the other mode. A record lives in one mode, so
// staying on its page after the switch could only say "Couldn't load" with a
// Retry that can never work (B-18); Stripe sends you to the list too. Null
// for any page that is not one record's page: those stay where they are.
const DETAIL_TO_LIST: [RegExp, string][] = [
  [/^\/customers\/[^/]+\/?$/, '/customers'],
  [/^\/subscriptions\/[^/]+\/?$/, '/subscriptions'],
  [/^\/invoices\/[^/]+\/?$/, '/invoices'],
  [/^\/plans\/[^/]+\/?$/, '/pricing'],
  [/^\/meters\/[^/]+\/?$/, '/pricing?tab=meters'],
  [/^\/test-clocks\/[^/]+\/?$/, '/test-clocks'],
  [/^\/webhooks\/endpoints\/[^/]+\/?$/, '/webhooks'],
]

export function listPathFor(pathname: string): string | null {
  for (const [re, list] of DETAIL_TO_LIST) if (re.test(pathname)) return list
  return null
}
