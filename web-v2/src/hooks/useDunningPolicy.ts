import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/lib/api'
import { getDunningPolicy, getGetDunningPolicyQueryKey } from '@/lib/gen/queries.gen'

/**
 * The account's retry policy for the current mode, or null when the mode has
 * none: the API answers 404 then, which is an answer, not a failure. Any
 * other error is the query's error.
 */
export function useDunningPolicy() {
  return useQuery({
    queryKey: getGetDunningPolicyQueryKey(),
    queryFn: () => getDunningPolicy().catch((err: unknown) => {
      if (err instanceof ApiError && err.status === 404) return null
      throw err
    }),
  })
}
