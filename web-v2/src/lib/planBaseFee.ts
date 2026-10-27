import type { BillTiming } from './api'

// The Edit plan dialog's base fee and billing timing (ADR-130 §5, §14). The
// server refuses a base of 0 with in_advance, and fixes the timing once a
// subscription has used the plan (pricing.Service.UpdatePlan). So a used
// plan shows its stored timing, and a used in_advance plan keeps a base fee
// above 0, even once every subscription on it is canceled. The dialog said
// "0 for no base fee" there and showed the timing as "At end of period".
export function editPlanBaseFee(input: {
  baseCents: number
  chosenTiming: BillTiming
  storedTiming: BillTiming
  used: boolean
  interval: string
}): { timing: BillTiming; timingDisabled: boolean; baseHint: string; timingHint: string; baseError: string | null } {
  const noBase = input.baseCents === 0
  // An unused plan with no base fee bills at the end of each period.
  const timing = input.used ? input.storedTiming : noBase ? 'in_arrears' : input.chosenTiming
  const fixedInAdvance = input.used && input.storedTiming === 'in_advance'
  const interval = input.interval === 'yearly' ? 'Yearly' : 'Monthly'
  const timingHint = (timing === 'in_advance'
    ? 'Customer charged the base fee on day 1 of each period; usage settles at period end.'
    : noBase
      ? 'A plan with no base fee bills its usage at the end of each period.'
      : 'Customer charged the base fee plus any usage at the end of each period.') +
    (input.used ? ' Fixed once a subscription has used the plan.' : '')
  return {
    timing,
    timingDisabled: input.used || noBase,
    baseHint: fixedInAdvance ? `${interval} recurring charge, above 0.` : `${interval} recurring charge. 0 for no base fee.`,
    timingHint,
    baseError: fixedInAdvance && noBase
      ? 'This plan bills its base fee at the start of each period, so the fee must be above 0. To charge no base fee, create a new plan.'
      : null,
  }
}
