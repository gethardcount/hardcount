// A dunning policy's schedule is one list, retry_days: the days to wait
// before each retry. The first wait counts from the failed payment, or, with
// no card on file, from the collection that found none. Each later one
// counts from the retry before it. The number of retries is its length.
// The server checks the same bounds (dunning.validateRetryDays).

export const MAX_RETRIES = 15
export const MAX_RETRY_WAIT_DAYS = 30

// retryDayNumbers: the day of each retry, counted from the failed payment
// (or the collection that found no card).
// [3, 4, 7] → [3, 7, 14].
export function retryDayNumbers(waits: number[]): number[] {
  let day = 0
  return waits.map(w => (day += w))
}

// retryPlanText: "3 retries, on day 3, 7 and 14".
export function retryPlanText(waits: number[]): string {
  if (waits.length === 0) return 'No retries'
  const days = retryDayNumbers(waits)
  const list = days.length === 1
    ? String(days[0])
    : `${days.slice(0, -1).join(', ')} and ${days[days.length - 1]}`
  return `${waits.length} ${waits.length === 1 ? 'retry' : 'retries'}, on day ${list}`
}

// parseRetryDays reads the editor's "3, 4, 7". error is set when the text
// is not 1 to MAX_RETRIES whole numbers of days from 1 to MAX_RETRY_WAIT_DAYS.
export function parseRetryDays(raw: string): { days: number[]; error?: string } {
  const parts = raw.split(',').map(s => s.trim()).filter(Boolean)
  if (parts.length === 0) return { days: [], error: 'Enter at least one wait, in days.' }
  if (parts.length > MAX_RETRIES) return { days: [], error: `At most ${MAX_RETRIES} retries.` }
  const days: number[] = []
  for (const p of parts) {
    const n = /^\d+$/.test(p) ? Number(p) : NaN
    if (!Number.isInteger(n) || n < 1 || n > MAX_RETRY_WAIT_DAYS) {
      return { days: [], error: `Each wait is a whole number of days from 1 to ${MAX_RETRY_WAIT_DAYS}.` }
    }
    days.push(n)
  }
  return { days }
}
