// The payment terms New invoice offers: days from issue to due date. The
// server computes due_at = issued_at + N days, so 0 means due on receipt.
const PRESET_DAYS = [0, 7, 14, 30, 60]

// paymentTermOptions is the presets, plus the account's own terms (Settings,
// Payment terms) when no preset has them, in order of days. New invoice
// starts at the account's terms, so they must be a choice it can show.
export function paymentTermOptions(accountDays: number): { value: string; label: string }[] {
  const days = PRESET_DAYS.includes(accountDays) ? PRESET_DAYS : [...PRESET_DAYS, accountDays].sort((a, b) => a - b)
  return days.map(d => ({ value: String(d), label: d === 0 ? 'Due on receipt' : `Net ${d}` }))
}
