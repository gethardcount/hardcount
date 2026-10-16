// What the invoice document shows about its seller and its issue state, in
// one place, so the detail page agrees with the PDF and the hosted page.
//
// Two rules:
// - Never print a made-up seller. The page used to show "VELOX" and "Your
//   Company" when the company name was unset, while the PDF printed "Velox".
//   Now an unset name shows nothing on the document and the page says the
//   seller details are missing, with a link to Settings.
// - A draft is not issued. Its issue (and, for a composed invoice, due) date
//   is set when it is finalized, so the page says "Draft — not issued"
//   instead of a date the customer will never see.

export interface SupplierFields {
  name?: string
  email?: string
  phone?: string
  address_line1?: string
  address_line2?: string
  city?: string
  state?: string
  postal_code?: string
  country?: string
}

export interface CompanySettingsFields {
  company_name?: string
  company_email?: string
  company_phone?: string
  company_address_line1?: string
  company_address_line2?: string
  company_city?: string
  company_state?: string
  company_postal_code?: string
  company_country?: string
}

export interface SellerBlock {
  name: string
  email?: string
  phone?: string
  addressLines: string[]
}

export function formatAddressLines(a: {
  line1?: string; line2?: string; city?: string; state?: string; postal?: string; country?: string
}): string[] {
  const lines: string[] = []
  if (a.line1) lines.push(a.line1)
  if (a.line2) lines.push(a.line2)
  const cityStatePostal = ([a.city, a.state].filter(Boolean).join(', ') + (a.postal ? ` ${a.postal}` : '')).trim()
  if (cityStatePostal) lines.push(cityStatePostal)
  if (a.country) lines.push(a.country)
  return lines
}

// sellerBlock is the "From" content. An issued invoice shows the company
// details it was issued with (ADR-109), so a later settings edit does not
// change it. A draft, or an invoice issued before the copy existed, shows the
// current settings. `name` is "" when unset; callers never substitute a
// placeholder.
export function sellerBlock(
  invoice: { supplier_snapshot?: SupplierFields | null },
  settings?: CompanySettingsFields | null,
): SellerBlock {
  const snap = invoice.supplier_snapshot
  if (snap) {
    return {
      name: (snap.name ?? '').trim(), email: snap.email, phone: snap.phone,
      addressLines: formatAddressLines({
        line1: snap.address_line1, line2: snap.address_line2, city: snap.city,
        state: snap.state, postal: snap.postal_code, country: snap.country,
      }),
    }
  }
  if (!settings) return { name: '', addressLines: [] }
  return {
    name: (settings.company_name ?? '').trim(), email: settings.company_email, phone: settings.company_phone,
    addressLines: formatAddressLines({
      line1: settings.company_address_line1, line2: settings.company_address_line2, city: settings.company_city,
      state: settings.company_state, postal: settings.company_postal_code, country: settings.company_country,
    }),
  }
}

// sellerMissing: the document would go out with no seller name. Only
// decided once settings have loaded (or the invoice carries its own copy),
// so the warning never flashes while the page loads.
export function sellerMissing(
  invoice: { supplier_snapshot?: SupplierFields | null },
  settings: CompanySettingsFields | null | undefined,
  settingsLoaded: boolean,
): boolean {
  if (!invoice.supplier_snapshot && !settingsLoaded) return false
  return sellerBlock(invoice, settings).name === ''
}

// SELLER_SETTINGS_PATH is where the company name and address are set.
export const SELLER_SETTINGS_PATH = '/settings?tab=business'

export interface IssueDates {
  issued: string
  due: string
  // Shown under the due value when it is not a date yet.
  dueHint?: string
}

// issueDates is what the document shows for Issued and Due. `fmt` formats a
// timestamp as a date (the page passes its own, timezone-aware formatter).
// A composed (manual) draft gets its dates at finalize: due is Net N from
// then. A cycle draft (a paused subscription's invoice, for example) keeps
// the due date it was built with, so that date is shown. "Composed" matches
// the server's isOperatorComposed: billing_reason 'manual', or none at all
// (drafts written before the reason was stamped).
export function issueDates(
  inv: { status: string; issued_at?: string | null; created_at?: string; due_at?: string | null; billing_reason?: string; net_payment_term_days?: number | null },
  fmt: (iso: string) => string,
): IssueDates {
  if (inv.status === 'draft') {
    const net = inv.net_payment_term_days
    const composed = !inv.billing_reason || inv.billing_reason === 'manual'
    if (composed || !inv.due_at) {
      return {
        issued: 'Draft — not issued',
        due: '—',
        dueHint: net == null ? 'Set when finalized' : net === 0 ? 'Due on receipt once finalized' : `Net ${net} from finalizing`,
      }
    }
    return { issued: 'Draft — not issued', due: fmt(inv.due_at) }
  }
  const issuedAt = inv.issued_at ?? inv.created_at
  return {
    issued: issuedAt ? fmt(issuedAt) : '—',
    due: inv.due_at ? fmt(inv.due_at) : '—',
  }
}
