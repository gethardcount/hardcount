import {
  LayoutDashboard, Users, FileText, CreditCard, Tag, Wallet, Settings,
  Receipt, AlertTriangle, ScrollText, Key, BarChart3, Webhook,
  type LucideIcon,
} from 'lucide-react'

// The dashboard's navigation, in one place. The sidebar and the Ctrl+K
// palette both read it, so a page can't be reachable from one and missing
// from the other.
//
// It follows the operator's job: the customers and the money they bring in,
// day to day; then what is set once (prices, payment retries); then
// developer tooling. Labels are sentence case; every item has its own icon.
export interface NavItem {
  to: string
  label: string
  icon: LucideIcon
  // Palette subtitle: what the operator finds on the page.
  hint: string
  // Extra path prefixes that belong to this section (detail routes).
  owns?: string[]
}

export interface NavSection {
  // null = the top block with no heading.
  title: string | null
  items: NavItem[]
}

export const NAV_SECTIONS: NavSection[] = [
  {
    title: null,
    items: [
      { to: '/', label: 'Home', icon: LayoutDashboard, hint: 'Revenue and what needs attention' },
    ],
  },
  {
    title: 'Customers and money',
    items: [
      { to: '/customers', label: 'Customers', icon: Users, hint: 'Who you bill, and what each has used' },
      { to: '/credits', label: 'Credit', icon: Wallet, hint: 'What each customer has left' },
      { to: '/usage', label: 'Usage', icon: BarChart3, hint: 'Each usage event your gateway sends' },
      { to: '/invoices', label: 'Invoices', icon: FileText, hint: 'What you billed, and what is paid' },
      { to: '/credit-notes', label: 'Credit notes', icon: Receipt, hint: 'Refunds and corrections to invoices' },
      { to: '/subscriptions', label: 'Subscriptions', icon: CreditCard, hint: 'Which plan each customer is on' },
    ],
  },
  {
    title: 'Setup',
    items: [
      { to: '/pricing', label: 'Pricing', icon: Tag, hint: 'What you charge, and what providers charge you', owns: ['/plans/', '/meters/'] },
      { to: '/dunning', label: 'Payment retries', icon: AlertTriangle, hint: 'Failed payments, and when they are retried' },
    ],
  },
  {
    title: 'Developers',
    items: [
      { to: '/api-keys', label: 'API keys', icon: Key, hint: 'The keys your gateway and server use' },
      { to: '/webhooks', label: 'Webhooks', icon: Webhook, hint: 'Where Hardcount sends events, and what it sent' },
      { to: '/audit-log', label: 'Audit log', icon: ScrollText, hint: 'Who changed what' },
    ],
  },
]

// Settings sits in the sidebar footer, not in a section.
export const SETTINGS_NAV: NavItem = { to: '/settings', label: 'Settings', icon: Settings, hint: 'Business, invoicing, tax, payments, team' }

// allNavItems is the flat list for search (palette), including Settings.
export function allNavItems(): NavItem[] {
  return [...NAV_SECTIONS.flatMap(s => s.items), SETTINGS_NAV]
}

// isNavActive keeps a section highlighted on its detail pages
// (/customers/:id highlights Customers; /plans/:id highlights Pricing).
export function isNavActive(item: NavItem, pathname: string): boolean {
  if (pathname === item.to) return true
  if (item.to !== '/' && pathname.startsWith(item.to + '/')) return true
  return (item.owns ?? []).some(p => pathname.startsWith(p))
}
