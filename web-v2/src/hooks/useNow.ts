import { useCallback, useMemo } from 'react'
import { useQuery, type QueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { useAuth } from '@/contexts/AuthContext'
import { clockTime, wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'

// What time it is for a record. A page asks it here, and only here, before it
// draws anything measured from "now" (see lib/effectiveNow).
//
// There are three answers:
//   - real time: the record has loaded and is on no test clock;
//   - the clock's time: the record is on a clock, and the clock is in the
//     loaded clock list;
//   - not known: the record has not loaded, or its clock is still loading,
//     failed to load or is not in the list.
// The clock list (useTestClocks) is the one source of a clock's time, so
// every page shows the same time for a clock, and refreshTestClocks after a
// clock change updates them all.

/** A record that can be on a test clock: a customer, subscription or invoice. */
export interface ClockPinned {
  test_clock_id?: string | null
}

/**
 * useTestClocks reads every test clock in the account. Test clocks exist only
 * in test mode: in live mode it sends no request, and no live record is on a
 * clock.
 */
export function useTestClocks() {
  const { user } = useAuth()
  return useQuery({
    queryKey: ['test-clocks'],
    queryFn: () => api.listTestClocks(),
    enabled: !(user?.livemode ?? false),
  })
}

/**
 * refreshTestClocks marks every cached test-clock read stale (the list, and
 * each clock page's clock, subscriptions and customers), so open pages
 * refetch them. Call it after any change to a clock.
 */
export function refreshTestClocks(qc: QueryClient) {
  return qc.invalidateQueries({ queryKey: ['test-clocks'] })
}

// A record's clock time: the clock's frozen_time, null for a record on no
// clock, or undefined when it is not known.
type ClockAnswer = string | null | undefined

function clockAnswer(record: ClockPinned | undefined, clocks: Map<string, string> | undefined): ClockAnswer {
  if (!record) return undefined
  if (!record.test_clock_id) return null
  return clocks?.get(record.test_clock_id)
}

function toNow(answer: ClockAnswer): EffectiveNow {
  if (answer) return clockTime(answer)
  // Not known (undefined) is answered with real time here, and nowhere else.
  // The next pull request, "the hook returns undefined for not known", makes
  // this return undefined, and every page then hides what it works out from
  // "now" until the answer is known.
  return wallClockNow()
}

// useClockTimes maps each clock id to its frozen_time, or is undefined while
// the list has not loaded (still loading, failed, or live mode).
function useClockTimes(): Map<string, string> | undefined {
  const { data } = useTestClocks()
  return useMemo(() => data && new Map(data.data.map(c => [c.id, c.frozen_time])), [data])
}

/**
 * useNowFor is "now" for one record: pass the loaded record, or undefined
 * while it loads. The value changes only when the answer does, so it is safe
 * in a dependency array or a query key: a record on no clock keeps one
 * real-time value for the life of the component.
 */
export function useNowFor(record: ClockPinned | undefined): EffectiveNow {
  const answer = clockAnswer(record, useClockTimes())
  return useMemo(() => toNow(answer), [answer])
}

/**
 * useNowResolver is useNowFor for a list whose rows can be on different
 * clocks: it returns a function that answers for one row's record.
 */
export function useNowResolver(): (record: ClockPinned | undefined) => EffectiveNow {
  const clocks = useClockTimes()
  return useCallback((record: ClockPinned | undefined) => toNow(clockAnswer(record, clocks)), [clocks])
}
