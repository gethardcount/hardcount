import { useCallback, useMemo } from 'react'
import { wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'

// What time it is for a record. Every page asks it here before it draws
// anything measured from a record's "now" (see lib/effectiveNow): real time
// once the record has loaded, and not known (undefined) before. While the
// answer is not known, a page draws nothing worked out from "now" and shows
// only stored facts: dates, amounts and the stored status.

/** A record a page measures from "now": a customer, subscription, invoice or dunning run. */
export type MeasuredRecord = object

/**
 * useNowFor is "now" for one record: pass the loaded record, or undefined
 * while it loads. It is undefined until the record loads, and the caller then
 * draws nothing worked out from "now". The value changes only when the answer
 * does, so it is the one for a dependency array or a query key: it keeps one
 * real-time value for the life of the component. A value that is only drawn
 * takes useNowResolver instead, so it moves with real time while the page
 * stays open.
 */
export function useNowFor(record: MeasuredRecord | undefined): EffectiveNow | undefined {
  const loaded = !!record
  return useMemo(() => (loaded ? wallClockNow() : undefined), [loaded])
}

/**
 * useNowResolver returns a function that answers "now" for a record each time
 * it is called: the real time of that render. A list calls it for each row,
 * and a detail page calls it for the values it only draws. The function
 * returns undefined for a record that has not loaded.
 */
export function useNowResolver(): (record: MeasuredRecord | undefined) => EffectiveNow | undefined {
  return useCallback((record: MeasuredRecord | undefined) => (record ? wallClockNow() : undefined), [])
}
