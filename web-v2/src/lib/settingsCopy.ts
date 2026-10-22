// Settings copy that has to match what the server does (sweep C7, C9, C10).

// The business email is printed in the From block of invoice and credit note
// PDFs, and the hosted invoice and payment-update pages link to it when no
// support URL is set (lib/sellerContact). No email sets it as Reply-To:
// ADR-082 leaves Reply-To out on purpose.
export const COMPANY_EMAIL_HINT =
  'Printed on invoices and credit notes. Payment pages link to it when no support URL is set.'

// One set of settings per account, shared by test and live mode (the server
// keeps one tenant_settings row). Under the test-mode banner an operator can
// reasonably think a change stays in test, so test mode says it does not.
// Live mode needs no notice: a live change affecting test is harmless.
export function sharedSettingsNotice(livemode: boolean): string | null {
  if (livemode) return null
  return 'Test and live mode share these settings, so a change here also applies to live invoices.'
}

// The prefix preview shows the number the next invoice in this mode really
// gets (next_invoice_sequence), padded to at least six digits as the server's
// padSeq does. Test and live count separately, and a prefix change keeps the
// count.
export function invoiceNumberPreview(prefix: string, nextSequence: number, livemode: boolean): string {
  const n = String(nextSequence).padStart(6, '0')
  return `Next ${livemode ? 'live' : 'test'} invoice: ${prefix}-${n}. Changing the prefix keeps the count.`
}
