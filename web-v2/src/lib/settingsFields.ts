// Which Settings tab holds each saved field, in the order the fields appear.
// Save Changes validates every tab, but only the open tab is rendered, so an
// invalid field on another tab used to make Save do nothing with no message.
// firstInvalidSettingsField finds the field to show: its tab, its label and
// the reason, so the page can switch tabs and say what to fix.

export type SettingsFormTab = 'general' | 'invoicing' | 'tax'

export const SETTINGS_TAB_LABELS: Record<SettingsFormTab, string> = {
  general: 'General',
  invoicing: 'Invoicing',
  tax: 'Tax',
}

export const SETTINGS_FIELDS: ReadonlyArray<{ field: string; tab: SettingsFormTab; label: string }> = [
  { field: 'company_name', tab: 'general', label: 'Business name' },
  { field: 'company_email', tab: 'general', label: 'Email' },
  { field: 'company_phone', tab: 'general', label: 'Phone' },
  { field: 'support_url', tab: 'general', label: 'Support URL' },
  { field: 'logo_url', tab: 'general', label: 'Logo URL' },
  { field: 'brand_color', tab: 'general', label: 'Brand color' },
  { field: 'company_country', tab: 'general', label: 'Country' },
  { field: 'company_address_line1', tab: 'general', label: 'Address line 1' },
  { field: 'company_address_line2', tab: 'general', label: 'Address line 2' },
  { field: 'company_city', tab: 'general', label: 'City' },
  { field: 'company_state', tab: 'general', label: 'State' },
  { field: 'company_postal_code', tab: 'general', label: 'Postal code' },
  { field: 'timezone', tab: 'general', label: 'Timezone' },
  { field: 'default_currency', tab: 'invoicing', label: 'Default currency' },
  { field: 'invoice_prefix', tab: 'invoicing', label: 'Invoice prefix' },
  { field: 'net_payment_terms', tab: 'invoicing', label: 'Payment terms' },
  { field: 'credit_balance_low_threshold', tab: 'invoicing', label: 'Credit low-balance alert' },
  { field: 'invoice_footer', tab: 'invoicing', label: 'Invoice footer' },
  { field: 'tax_id', tab: 'tax', label: 'Tax ID' },
  { field: 'tax_provider', tab: 'tax', label: 'Tax provider' },
  { field: 'tax_name', tab: 'tax', label: 'Tax name' },
  { field: 'tax_rate', tab: 'tax', label: 'Tax rate' },
  { field: 'tax_inclusive', tab: 'tax', label: 'Tax-inclusive pricing' },
  { field: 'default_product_tax_code', tab: 'tax', label: 'Default product tax code' },
]

export interface InvalidSettingsField {
  field: string
  tab: SettingsFormTab
  label: string
  message: string
}

// firstInvalidSettingsField returns the first field with an error, in page
// order, or null. A field with an error that is not in the list (a future
// field nobody mapped) is still returned, on the General tab, so Save is
// never silent.
export function firstInvalidSettingsField(
  errors: Partial<Record<string, { message?: unknown } | undefined>>,
): InvalidSettingsField | null {
  for (const f of SETTINGS_FIELDS) {
    const e = errors[f.field]
    if (e) return { ...f, message: messageOf(e) }
  }
  const other = Object.keys(errors).find(k => errors[k])
  if (!other) return null
  return { field: other, tab: 'general', label: other, message: messageOf(errors[other]!) }
}

function messageOf(e: { message?: unknown }): string {
  return typeof e.message === 'string' && e.message.trim() ? e.message : 'Check this value.'
}

// invalidSettingsToast is the message for a Save that cannot go through.
export function invalidSettingsToast(f: InvalidSettingsField): { title: string; description: string } {
  const msg = /[.!?]$/.test(f.message) ? f.message : f.message + '.'
  return {
    title: "Couldn't save settings",
    description: `${f.label} on the ${SETTINGS_TAB_LABELS[f.tab]} tab: ${msg}`,
  }
}
