import {
  LayoutDashboard, Users, FileText, CreditCard, Tag, Wallet, Settings,
  Receipt, AlertTriangle, ScrollText, Globe, Key, BarChart3, Sparkles,
  Clock, Coins, ListChecks, Webhook,
  type LucideIcon,
} from 'lucide-react'

// The dashboard's navigation, in one place. The sidebar and the Ctrl+K
// palette both read it, so a page can't be reachable from one and missing
// from the other (the palette had drifted: no Recipes, Provider costs,
// Dunning policies or Test clocks).
//
// Grouped by the job an operator is doing: run billing day to day, recover
// and adjust money, set up what you sell, and developer tooling. Labels are
// sentence case; every item has its own icon.
export interface NavItem {
  to: string
  label: string
  icon: LucideIcon
  // Palette subtitle: what the page is for.
  hint: string
  // Shown only in test mode (test clocks).
  testOnly?: boolean
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
    title: 'Billing',
    items: [
      { to: '/customers', label: 'Customers', icon: Users, hint: 'Accounts you bill' },
      { to: '/subscriptions', label: 'Subscriptions', icon: CreditCard, hint: 'Plans customers are on' },
      { to: '/invoices', label: 'Invoices', icon: FileText, hint: 'Drafts, open and paid invoices' },
      { to: '/usage', label: 'Usage', icon: BarChart3, hint: 'Metered usage events' },
    ],
  },
  {
    title: 'Revenue',
    items: [
      { to: '/dunning', label: 'Dunning', icon: AlertTriangle, hint: 'Failed-payment recovery' },
      { to: '/credits', label: 'Credits', icon: Wallet, hint: 'Customer credit balances' },
      { to: '/credit-notes', label: 'Credit notes', icon: Receipt, hint: 'Refunds and adjustments' },
    ],
  },
  {
    title: 'Catalog',
    items: [
      { to: '/pricing', label: 'Pricing', icon: Tag, hint: 'Plans, meters and prices', owns: ['/plans/', '/meters/'] },
      { to: '/recipes', label: 'Recipes', icon: Sparkles, hint: 'Ready-made pricing setups' },
      { to: '/provider-costs', label: 'Provider costs', icon: Coins, hint: 'What providers charge you, for margin' },
      { to: '/dunning-policies', label: 'Dunning policies', icon: ListChecks, hint: 'Retry schedules for failed payments' },
    ],
  },
  {
    title: 'Developers',
    items: [
      { to: '/api-keys', label: 'API keys', icon: Key, hint: 'Keys for the API' },
      { to: '/webhooks', label: 'Webhooks', icon: Webhook, hint: 'Endpoints and event deliveries' },
      { to: '/audit-log', label: 'Audit log', icon: ScrollText, hint: 'Who changed what' },
      { to: '/test-clocks', label: 'Test clocks', icon: Clock, hint: 'Simulate time in test mode', testOnly: true },
    ],
  },
]

// Settings sits in the sidebar footer, not in a section.
export const SETTINGS_NAV: NavItem = { to: '/settings', label: 'Settings', icon: Settings, hint: 'Business, invoicing, tax, payments, team' }

// visibleNav returns the sections for the current mode, dropping test-only
// items in live mode and sections left empty.
export function visibleNav(livemode: boolean): NavSection[] {
  return NAV_SECTIONS
    .map(s => ({ ...s, items: s.items.filter(i => !(i.testOnly && livemode)) }))
    .filter(s => s.items.length > 0)
}

// allNavItems is the flat list for search (palette), including Settings.
export function allNavItems(livemode: boolean): NavItem[] {
  return [...visibleNav(livemode).flatMap(s => s.items), SETTINGS_NAV]
}

// isNavActive keeps a section highlighted on its detail pages
// (/customers/:id highlights Customers; /plans/:id highlights Pricing).
export function isNavActive(item: NavItem, pathname: string): boolean {
  if (pathname === item.to) return true
  if (item.to !== '/' && pathname.startsWith(item.to + '/')) return true
  return (item.owns ?? []).some(p => pathname.startsWith(p))
}

// Globe is kept exported for pages that link to webhooks with the old icon.
export { Globe }
