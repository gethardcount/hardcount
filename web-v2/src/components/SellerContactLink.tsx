import type { SellerContact } from '@/lib/sellerContact'

// SellerContactLink names the company on a public page, linked to its support
// page or email when the account has set one, plain text otherwise.
export function SellerContactLink({ contact }: { contact: SellerContact }) {
  if (!contact.href) return <>{contact.name}</>
  const external = !contact.href.startsWith('mailto:')
  return (
    <a
      href={contact.href}
      className="underline underline-offset-2 text-foreground hover:text-primary"
      {...(external ? { target: '_blank', rel: 'noopener noreferrer' } : {})}
    >
      {contact.name}
    </a>
  )
}
