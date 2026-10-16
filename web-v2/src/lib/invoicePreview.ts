// What an "Upcoming invoice" card shows, for the subscription page and the
// customer page's Upcoming card (M-SUB-2, M-SUB-8).
//
// Three states used to look alike: a preview with no lines yet (a usage plan
// early in its period) drew a headerless empty table on one page and
// "Projection unavailable" on the other, and a failed request was swallowed
// into the same words. Now:
//   - lines  → the lines and the total;
//   - empty  → $0.00, with the server's notes (lib/invoicePreviewNotes) in
//              place of the table;
//   - none   → the server says there is nothing to preview (no period, no
//              items, no active subscription), in its own words;
//   - error  → "Couldn't load" with Retry.
// Plain data in, plain data out (no api.ts import), so node --test runs it.

export interface PreviewLike {
  billing_period_start: string
  billing_period_end: string
  lines: { amount_cents: number; currency: string }[]
  totals: { currency: string; amount_cents: number }[]
  warnings?: string[] | null
  generated_at: string
}

export type PreviewView =
  | { kind: 'loading' }
  | { kind: 'lines'; total: number; currency: string; multiCurrency: boolean }
  | { kind: 'empty'; total: 0; currency: string }
  | { kind: 'none'; message: string }
  | { kind: 'error' }

// A 4xx the server sends when there is honestly nothing to preview. Anything
// else (5xx, network) is a failure the operator can retry.
function isNothingToPreview(err: unknown): err is { status: number; message: string } {
  if (!err || typeof err !== 'object') return false
  const status = (err as { status?: unknown }).status
  return typeof status === 'number' && status >= 400 && status < 500 && status !== 408 && status !== 429
}

export function previewView(
  preview: PreviewLike | null | undefined,
  error: unknown,
  fallbackCurrency: string,
): PreviewView {
  if (error) {
    if (isNothingToPreview(error)) {
      const msg = (error as { message?: string }).message
      return { kind: 'none', message: msg && msg.trim() ? msg : 'There is nothing to preview for this subscription.' }
    }
    return { kind: 'error' }
  }
  if (!preview) return { kind: 'loading' }
  const totals = preview.totals ?? []
  if ((preview.lines ?? []).length === 0 || totals.length === 0) {
    return { kind: 'empty', total: 0, currency: totals[0]?.currency || fallbackCurrency }
  }
  return { kind: 'lines', total: totals[0].amount_cents, currency: totals[0].currency, multiCurrency: totals.length > 1 }
}
