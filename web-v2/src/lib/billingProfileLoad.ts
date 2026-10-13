// A failed billing-profile fetch must not read as "this customer has no
// profile". The customer page offers "Set Up Billing Profile" when there is
// none, and saving that blank form replaces the whole profile: the tax status
// resets and the tax ID is cleared, which changes the tax on later invoices.
// These helpers keep "the server said there is none" apart from "we could
// not find out".

// profileOrAbsent resolves to null only when the server answers 404, which is
// how it says the customer has no billing profile. Every other failure is
// rethrown so the query reports an error.
export async function profileOrAbsent<T>(
  fetchProfile: () => Promise<T>,
  isNotFound: (err: unknown) => boolean,
): Promise<T | null> {
  try {
    return await fetchProfile()
  } catch (err) {
    if (isNotFound(err)) return null
    throw err
  }
}

export type BillingProfileCard = 'profile' | 'absent' | 'loading' | 'error'

// billingProfileCard picks what the Billing Profile card shows. Only
// 'profile' and 'absent' carry the Edit / Set Up button. An error wins over
// an earlier answer: after a save the page refetches, and if that refetch
// fails the earlier answer is out of date.
export function billingProfileCard(q: { data: unknown; isError: boolean }): BillingProfileCard {
  if (q.isError) return 'error'
  if (q.data === undefined) return 'loading'
  return q.data === null ? 'absent' : 'profile'
}
