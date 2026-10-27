import type { BillTiming } from './api'

// The Edit plan dialog's base fee and billing timing (ADR-130 §5, §14). The
// server refuses a base of 0 with in_advance, and fixes the timing once a
// subscription has used the plan (pricing.Service.UpdatePlan).
export function editPlanBaseFee(input: {
  baseCents: number
  chosenTiming: BillTiming
  storedTiming: BillTiming
  used: boolean
  interval: string
}): { timing: BillTiming; timingDisabled: boolean; baseHint: string; timingHint: string; baseError: string | null } {
  const noBase = input.baseCents === 0
  const timing = noBase ? 'in_arrears' : input.chosenTiming
  const interval = input.interval === 'yearly' ? 'Yearly' : 'Monthly'
  const timingHint = (noBase
    ? 'A plan with no base fee bills its usage at the end of each period.'
    : timing === 'in_advance'
      ? 'Customer charged the base fee on day 1 of each period; usage settles at period end.'
      : 'Customer charged the base fee plus any usage at the end of each period.') +
    (input.used ? ' Fixed once a subscription has used the plan.' : '')
  return {
    timing,
    timingDisabled: input.used || noBase,
    baseHint: `${interval} recurring charge. 0 for no base fee.`,
    timingHint,
    baseError: null,
  }
}
