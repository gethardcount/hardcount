// What the Payment retries page says about whether failed payments are
// retried. An account has one retry policy per mode. Every new account
// starts with one (ADR-123); an older account may have none, and nothing
// adds one for it: a policy appearing on a running account would start
// retrying old failed invoices. So the page says, in words, when nothing is
// retried.

export type RetriesNotice = { title: string; body: string }

/**
 * A notice when a payment that fails now starts no retries in this mode:
 * there is no policy (null), or it is turned off. Null when retries are on.
 * Turning the policy off stops new retries only: StartDunning reads
 * `enabled`, a retry already under way does not.
 */
export function retriesOffNotice(policy: { enabled: boolean } | null): RetriesNotice | null {
  if (!policy) {
    return {
      title: 'Retries are not set up',
      body: 'Failed payments are not retried until you save a retry policy.',
    }
  }
  if (!policy.enabled) {
    return {
      title: 'Retries are turned off',
      body: 'A payment that fails now is not retried. Retries already under way finish their schedule. Turn retries on to start them for new failures.',
    }
  }
  return null
}
