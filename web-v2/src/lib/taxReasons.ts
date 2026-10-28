// taxReasonLabel maps Stripe's canonical per-line `taxability_reason` values
// (see https://stripe.com/docs/api/tax/calculations/object#tax_calculation_object-line_items-tax_breakdown-taxability_reason)
// to human-readable casing for the dashboard badge.
//
// We deliberately render only non-trivial reasons. `standard_rated` (the
// default sales-tax path) and `''` (non-Stripe providers / pre-#4 invoices)
// would just add noise to the line item table — the operator already sees
// the Tax column for that case. Reasons not in the map fall back to the raw
// string so a future Stripe addition still surfaces, just without polished
// casing. Keep this file small and copy the wording verbatim from Stripe's
// docs where reasonable so the operator can grep their docs and find a hit.
const NON_TRIVIAL_LABELS: Record<string, string> = {
  reverse_charge: 'Reverse charge',
  not_collecting: 'Not collecting in this jurisdiction',
  product_exempt: 'Product exempt',
  customer_exempt: 'Customer exempt',
  excluded_territory: 'Excluded territory',
  jurisdiction_unsupported: 'Jurisdiction unsupported',
  not_subject_to_tax: 'Not subject to tax',
  reduced_rated: 'Reduced rate',
  zero_rated: 'Zero rated',
  // Hardcount's own reason, not Stripe's: a prepaid commit purchase is not
  // taxed; the usage it pays for is (ADR-078 amendment 2026-10-19).
  prepaid_credit_purchase: 'Prepaid credit, not taxed',
}

// taxReasonLabel returns the human-readable label for a Stripe taxability
// reason, or null when the reason is trivial (empty, standard_rated) and
// should not render a badge.
export function taxReasonLabel(reason: string | undefined | null): string | null {
  if (!reason || reason === 'standard_rated') return null
  return NON_TRIVIAL_LABELS[reason] ?? reason
}
