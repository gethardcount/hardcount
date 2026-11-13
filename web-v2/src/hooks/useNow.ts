import { useCallback, useEffect, useMemo, useState } from 'react'
import { useQuery, type QueryClient } from '@tanstack/react-query'
import { api, type TestClock } from '@/lib/api'
import { useAuth } from '@/contexts/AuthContext'
import { clockTime, wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'

// What time it is for a record. Every page asks it here before it draws
// anything measured from a record's "now" (see lib/effectiveNow). The one
// exception is the clock page, which measures its advance picker from its own
// read of that clock.
//
// There are three answers:
//   - real time: the record has loaded and is on no test clock;
//   - the clock's time: the record is on a clock, and the clock is in the
//     loaded clock list;
//   - not known (undefined): the record has not loaded, or its clock is still
//     loading, failed to load or is not in the list. Live mode reads no clock
//     list and keeps its own cache, so a live record with a clock id is not
//     known either.
// While the answer is not known, a page draws nothing worked out from "now"
// and shows only stored facts: dates, amounts and the stored status. When the
// clock list fails to load, the page shows that load error once, with Retry
// (TestClockBanner on a record's page, TestClocksLoadError on a list).
// The clock list (useTestClocks) is the one source of a clock's time, so
// every page shows the same time for a clock, and refreshTestClocks after a
// clock change updates them all.

/** A record that can be on a test clock: a customer, subscription, invoice or dunning run. */
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
 * refetch them. Call it after a clock is created, advanced or retried, and
 * when the clock page sees an advance finish. Deleting a clock removes that
 * clock's reads first, so nothing reads it again, then refreshes the list
 * alone (handleDelete in TestClockDetail).
 */
export function refreshTestClocks(qc: QueryClient) {
  return qc.invalidateQueries({ queryKey: ['test-clocks'] })
}

/**
 * useTestClock finds one clock in the clock list. `missing` is true only when
 * a list read made after this component mounted does not hold the clock. A
 * list cached from before can predate a clock made since (through the API or
 * in another tab), so when that list lacks the clock it is read again once,
 * and until then the clock is not known rather than missing.
 */
export function useTestClock(id: string | null | undefined): { clock: TestClock | undefined; missing: boolean } {
  const { data, dataUpdatedAt, isFetching, isError, refetch } = useTestClocks()
  const [readBeforeMount] = useState(dataUpdatedAt)
  const readSinceMount = dataUpdatedAt > readBeforeMount
  const clock = id ? data?.data.find(c => c.id === id) : undefined
  const notInList = !!id && !!data && !clock && !isFetching

  const readAgain = notInList && !readSinceMount && !isError
  useEffect(() => {
    if (readAgain) void refetch({ cancelRefetch: false })
  }, [readAgain, refetch])

  return { clock, missing: notInList && readSinceMount }
}

// A record's clock time: the clock's frozen_time, null for a record on no
// clock, or undefined when it is not known.
type ClockAnswer = string | null | undefined

function clockAnswer(record: ClockPinned | undefined, frozenTimeOf: (clockId: string) => string | undefined): ClockAnswer {
  if (!record) return undefined
  if (!record.test_clock_id) return null
  return frozenTimeOf(record.test_clock_id)
}

function toNow(answer: ClockAnswer): EffectiveNow | undefined {
  if (answer === undefined) return undefined
  return answer === null ? wallClockNow() : clockTime(answer)
}

/**
 * useNowFor is "now" for one record: pass the loaded record, or undefined
 * while it loads. It is undefined while the answer is not known, and the
 * caller then draws nothing worked out from "now". The value changes only
 * when the answer does, so it is the one for a dependency array or a query
 * key: a record on no clock keeps one real-time value for the life of the
 * component. A value that is only drawn takes useNowResolver instead, so it
 * moves with real time while the page stays open.
 */
export function useNowFor(record: ClockPinned | undefined): EffectiveNow | undefined {
  const { clock } = useTestClock(record?.test_clock_id)
  const answer = clockAnswer(record, () => clock?.frozen_time)
  return useMemo(() => toNow(answer), [answer])
}

/**
 * useNowResolver returns a function that answers "now" for a record each time
 * it is called, so a record on no clock gets the real time of that render. A
 * list whose rows can be on different clocks calls it for each row, and a
 * detail page calls it for the values it only draws. The function returns
 * undefined while the answer is not known.
 */
export function useNowResolver(): (record: ClockPinned | undefined) => EffectiveNow | undefined {
  const { data } = useTestClocks()
  const clocks = useMemo(() => data && new Map(data.data.map(c => [c.id, c.frozen_time])), [data])
  return useCallback((record: ClockPinned | undefined) => toNow(clockAnswer(record, id => clocks?.get(id))), [clocks])
}
