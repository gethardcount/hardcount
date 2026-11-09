import { formatInTimeZone } from 'date-fns-tz'
import type { EffectiveNow } from './effectiveNow'

// A date picker works in days of the account's zone, as yyyy-mm-dd. A day
// picked there is sent as an instant in that zone (startOfNextDayInTZ for a
// credit's "Valid through"), so the picker's today and its first allowed day
// must be days of that zone too. The browser's zone and clock never enter:
// an operator in Los Angeles granting credit in a Tokyo account at 20:00 on
// Nov 8 sees Nov 9, which is already today in Tokyo.
export interface PickerDays {
  // The day marked as today, and the one the Today button picks: the day of
  // the customer's now, a test clock's now when the customer is on one.
  today: string
  // False when today is before the first day the picker allows.
  todayAllowed: boolean
}

export function pickerDays(now: EffectiveNow, timeZone: string, min?: string): PickerDays {
  const today = formatInTimeZone(new Date(now), timeZone, 'yyyy-MM-dd')
  // yyyy-mm-dd days compare correctly as text.
  return { today, todayAllowed: !min || today >= min }
}

// dayAsDate turns a yyyy-mm-dd day into the Date the calendar draws it as:
// that day's local midnight. The calendar counts days, not instants, so the
// browser's zone never moves one.
export function dayAsDate(day: string): Date {
  const [y, m, d] = day.split('-').map(Number)
  return new Date(y, m - 1, d)
}
