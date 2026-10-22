// The Credit Notes export. Each row names its note's currency: a EUR note and
// a USD note side by side are otherwise two bare numbers.

export interface CreditNoteCsvInput {
  credit_note_number: string
  status: string
  currency: string
  refund_amount_cents: number
  refund_status?: string | null
  credit_amount_cents: number
  out_of_band_amount_cents?: number | null
  reason: string
  total_cents: number
  issued_at?: string | null
  created_at: string
}

export const CREDIT_NOTE_CSV_HEADERS = ['Number', 'Customer', 'Invoice', 'Status', 'Refund', 'Refund status', 'Credit', 'Out of band', 'Reason', 'Amount', 'Currency', 'Date']

const amount = (cents: number) => (cents / 100).toFixed(2)

export function creditNoteCsvRow(n: CreditNoteCsvInput, customerName: string, invoiceNumber: string): string[] {
  return [
    n.credit_note_number,
    customerName,
    invoiceNumber,
    n.status,
    amount(n.refund_amount_cents),
    // Without this column a failed refund's amount reads as returned money
    // in the spreadsheet — the same lie the stat card told on screen.
    n.refund_amount_cents > 0 ? (n.refund_status || '') : '',
    amount(n.credit_amount_cents),
    amount(n.out_of_band_amount_cents ?? 0),
    n.reason,
    amount(n.total_cents),
    n.currency,
    n.issued_at || n.created_at,
  ]
}
