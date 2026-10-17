// Stripe-canonical tax_id_type codes. Source of truth:
// https://stripe.com/docs/api/customer_tax_ids/object#tax_id_object-type
//
// Velox stores `tax_id_type` in this exact form (the backend's
// NormalizeTaxIDType folds legacy Velox shorthand — gstin/vat/abn — to the
// canonical Stripe code on write). The dashboard always writes canonical;
// legacy values read back from older rows are pinned at the top of the
// dropdown with a "Legacy" annotation so existing data isn't lost.

export interface TaxIdTypeOption {
  value: string
  label: string
  country: string
}

export const STRIPE_TAX_ID_TYPES: ReadonlyArray<TaxIdTypeOption> = [
  { value: 'ad_nrt', label: 'Andorra NRT', country: 'AD' },
  { value: 'ae_trn', label: 'UAE TRN', country: 'AE' },
  { value: 'ar_cuit', label: 'Argentina CUIT', country: 'AR' },
  { value: 'au_abn', label: 'Australia ABN', country: 'AU' },
  { value: 'au_arn', label: 'Australia ARN', country: 'AU' },
  { value: 'br_cnpj', label: 'Brazil CNPJ', country: 'BR' },
  { value: 'br_cpf', label: 'Brazil CPF', country: 'BR' },
  { value: 'ca_bn', label: 'Canada BN', country: 'CA' },
  { value: 'ca_gst_hst', label: 'Canada GST/HST', country: 'CA' },
  { value: 'ca_pst_bc', label: 'Canada PST British Columbia', country: 'CA' },
  { value: 'ca_pst_mb', label: 'Canada PST Manitoba', country: 'CA' },
  { value: 'ca_pst_sk', label: 'Canada PST Saskatchewan', country: 'CA' },
  { value: 'ca_qst', label: 'Canada QST Quebec', country: 'CA' },
  { value: 'ch_vat', label: 'Switzerland VAT', country: 'CH' },
  { value: 'cl_tin', label: 'Chile TIN', country: 'CL' },
  { value: 'co_nit', label: 'Colombia NIT', country: 'CO' },
  { value: 'eg_tin', label: 'Egypt TIN', country: 'EG' },
  { value: 'es_cif', label: 'Spain CIF', country: 'ES' },
  { value: 'eu_oss_vat', label: 'EU OSS VAT', country: 'EU' },
  { value: 'eu_vat', label: 'EU VAT', country: 'EU' },
  { value: 'gb_vat', label: 'United Kingdom VAT', country: 'GB' },
  { value: 'hk_br', label: 'Hong Kong BR', country: 'HK' },
  { value: 'hu_tin', label: 'Hungary TIN', country: 'HU' },
  { value: 'id_npwp', label: 'Indonesia NPWP', country: 'ID' },
  { value: 'il_vat', label: 'Israel VAT', country: 'IL' },
  { value: 'in_gst', label: 'India GSTIN', country: 'IN' },
  { value: 'is_vat', label: 'Iceland VAT', country: 'IS' },
  { value: 'jp_cn', label: 'Japan CN', country: 'JP' },
  { value: 'jp_rn', label: 'Japan RN', country: 'JP' },
  { value: 'jp_trn', label: 'Japan TRN', country: 'JP' },
  { value: 'kr_brn', label: 'South Korea BRN', country: 'KR' },
  { value: 'li_uid', label: 'Liechtenstein UID', country: 'LI' },
  { value: 'mx_rfc', label: 'Mexico RFC', country: 'MX' },
  { value: 'my_frp', label: 'Malaysia FRP', country: 'MY' },
  { value: 'my_itn', label: 'Malaysia ITN', country: 'MY' },
  { value: 'my_sst', label: 'Malaysia SST', country: 'MY' },
  { value: 'no_vat', label: 'Norway VAT', country: 'NO' },
  { value: 'nz_gst', label: 'New Zealand GST', country: 'NZ' },
  { value: 'ph_tin', label: 'Philippines TIN', country: 'PH' },
  { value: 'ru_inn', label: 'Russia INN', country: 'RU' },
  { value: 'ru_kpp', label: 'Russia KPP', country: 'RU' },
  { value: 'sa_vat', label: 'Saudi Arabia VAT', country: 'SA' },
  { value: 'sg_gst', label: 'Singapore GST', country: 'SG' },
  { value: 'sg_uen', label: 'Singapore UEN', country: 'SG' },
  { value: 'si_tin', label: 'Slovenia TIN', country: 'SI' },
  { value: 'th_vat', label: 'Thailand VAT', country: 'TH' },
  { value: 'tr_tin', label: 'Turkey TIN', country: 'TR' },
  { value: 'tw_vat', label: 'Taiwan VAT', country: 'TW' },
  { value: 'ua_vat', label: 'Ukraine VAT', country: 'UA' },
  { value: 'us_ein', label: 'United States EIN', country: 'US' },
  { value: 'za_vat', label: 'South Africa VAT', country: 'ZA' },
] as const

// Hint text printed under the tax-ID input for the codes Velox knows how to
// format-validate. Other codes pass through silently — Stripe / downstream
// tax engines apply any format rules.
export const TAX_ID_HINTS: Record<string, string> = {
  in_gst: 'e.g. 27AAEPM1234C1Z5 (15-char India GSTIN)',
  eu_vat: 'e.g. DE123456789 — 2-letter country prefix + alphanumerics',
  gb_vat: 'e.g. GB123456789',
  au_abn: 'e.g. 51824753556 — 11 digits',
  us_ein: 'e.g. 12-3456789',
}

const CANONICAL_VALUES = new Set<string>(STRIPE_TAX_ID_TYPES.map(t => t.value))

// Builds the option list for the tax_id_type dropdown. If `currentValue` is a
// non-canonical legacy code (e.g. "gstin" left over from a row written before
// the Stripe-canonical migration), pin a "Legacy" option at the top so the
// existing value renders. Once the user re-saves, the backend normalizes it
// to the canonical Stripe code via NormalizeTaxIDType.
export function taxIdTypeOptions(currentValue: string): TaxIdTypeOption[] {
  const sorted = [...STRIPE_TAX_ID_TYPES].sort((a, b) => a.label.localeCompare(b.label))
  if (currentValue && !CANONICAL_VALUES.has(currentValue)) {
    return [
      { value: currentValue, label: `${currentValue} (Legacy)`, country: '' },
      ...sorted,
    ]
  }
  return sorted
}
