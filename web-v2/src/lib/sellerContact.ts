// Customer-facing "who do I contact" for the public pages (hosted invoice,
// update payment method). The reader is the operator's end customer: they
// know the company they pay, not Velox and not a "billing provider".

export interface SellerBranding {
  company_name?: string
  company_email?: string
  support_url?: string
}

export interface SellerContact {
  // name is the company name, or a plain description when none is set.
  name: string
  // href links to the support page, else mails the company; undefined when
  // the account has set neither.
  href?: string
}

export const UNNAMED_SELLER = 'the company that sent you this link'

export function sellerContact(branding: SellerBranding | undefined | null): SellerContact {
  const name = branding?.company_name?.trim() || UNNAMED_SELLER
  const url = branding?.support_url?.trim()
  if (url) return { name, href: url }
  const email = branding?.company_email?.trim()
  if (email) return { name, href: `mailto:${email}` }
  return { name }
}
