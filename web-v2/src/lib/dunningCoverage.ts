// What the dunning pages say about whether failed payments are retried.
//
// Every new account starts with a "Default retries" policy per mode
// (ADR-123). Accounts made before that have none, and nothing adds one for
// them: a policy appearing on a running account would start retrying old
// failed invoices. So those accounts need to be told, in words, that
// nothing is retried until they create a policy.

type PolicyLike = {
  name: string
  enabled: boolean
  is_default: boolean
  seeded?: boolean
}

export type RetriesNotice = { title: string; body: string }

/**
 * A notice when failed payments in this mode are not retried, or null when
 * the default policy is on. With no default there are no policies at all
 * (the first one created becomes the default, and the default cannot be
 * deleted), so "nothing is retried" is exactly true.
 */
export function retriesOffNotice(policies: PolicyLike[]): RetriesNotice | null {
  const def = policies.find(p => p.is_default)
  if (!def) {
    return {
      title: 'Failed payments are not retried',
      body: 'Create a policy to start automatic retries.',
    }
  }
  if (!def.enabled) {
    return {
      title: 'Default policy is turned off',
      body: `Customers on the default policy, ${def.name || 'unnamed'}, are not retried. Turn it on, or make another policy the default.`,
    }
  }
  return null
}

/** Shown on the policy an account was created with, until it is edited or chosen. */
export const SEEDED_POLICY_NOTE =
  'Created with your account. When retries run out nothing changes: the subscription keeps billing, so each new unpaid invoice is retried on its own. Choose "Pause collection" to stop that. The first enabled policy you create becomes the default instead, unless you edit this one or make it the default first.'

/**
 * What applying a recipe's retry policy does to the default, so the
 * preview says it before anything is created.
 */
export function recipeRetryNote(policies: PolicyLike[]): string {
  const def = policies.find(p => p.is_default)
  if (!def) return 'becomes your default'
  if (def.seeded) return `becomes your default, replacing ${def.name}`
  return `added next to your default, ${def.name}`
}

/**
 * What saving a new policy does to the default, shown in the create dialog
 * before anything is saved. Only the seeded default hands over (ADR-123),
 * and only to an enabled policy: a policy created turned off leaves it in
 * place. Null when the new policy is just added next to the default.
 */
export function newPolicyDefaultNote(policies: PolicyLike[], enabled: boolean): string | null {
  const def = policies.find(p => p.is_default)
  if (!def?.seeded) return null
  if (!enabled) return `${def.name} stays your default while this policy is turned off.`
  return `Saving makes this your default: every customer without their own policy uses it instead of ${def.name}.`
}
