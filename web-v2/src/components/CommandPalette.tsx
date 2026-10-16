import { useCallback, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import {
  FileText, User, Hash, Zap, type LucideIcon,
} from 'lucide-react'
import { useAuth } from '@/contexts/AuthContext'
import { allNavItems, isTestMode } from '@/lib/navigation'
import { invoiceStatus } from '@/lib/statusLabels'
import { wallClockNow } from '@/lib/effectiveNow'
import { api, type Customer, type Invoice, type Plan, type Subscription, formatCents } from '@/lib/api'
import {
  Command,
  CommandDialog,
  CommandInput,
  CommandList,
  CommandEmpty,
  CommandGroup,
  CommandItem,
  CommandSeparator,
} from '@/components/ui/command'

interface CommandPaletteProps {
  open: boolean
  onClose: () => void
}

interface NavItem {
  id: string
  title: string
  subtitle: string
  icon: LucideIcon
  href: string
}

// Pages come from the shared navigation config, so the palette lists exactly
// what the sidebar does (it had drifted and missed four pages).
function navItems(testMode: boolean): NavItem[] {
  return allNavItems(testMode).map(i => ({
    id: `nav-${i.to}`,
    title: i.label,
    subtitle: i.hint,
    icon: i.icon,
    href: i.to,
  }))
}

// The palette doesn't load test clocks, so it can only judge "Past due" for
// real-time invoices. A simulated invoice gets no "now" (NaN) and reads Open
// rather than Past due by the wall clock, which its test clock may not have
// reached.
function paletteNow(inv: Invoice): number {
  return inv.is_simulated ? NaN : wallClockNow()
}

export function CommandPalette({ open, onClose }: CommandPaletteProps) {
  const navigate = useNavigate()
  const { user } = useAuth()

  // Controlled input → debounced server-side search. With no query the
  // palette shows the 50 most recent rows per entity (jump-to-recent);
  // once the operator types, the term goes to the backend's search=
  // param so matches come from the backend, not whichever 50 rows
  // happened to be cached. Customer search matches name / email /
  // external ID (the backend decrypt-matches the encrypted PII columns,
  // bounded by its 5000-row scan cap); invoices match by number;
  // subscriptions by name / code.
  const [query, setQuery] = useState('')
  const term = useDebouncedValue(query.trim(), 250)
  // Reset the query in the close handlers (not an effect) so the next
  // ⌘K opens fresh — selection + dialog-dismiss both route through
  // handleClose below.
  const handleClose = useCallback(() => {
    setQuery('')
    onClose()
  }, [onClose])

  // Four parallel queries gated on `enabled: open` so the lists are
  // fetched lazily on first ⌘K open. Keys include the search term;
  // placeholderData keeps the previous results rendered while the
  // next search is in flight (no flicker-to-empty between keystrokes).
  const customersQuery = useQuery({
    queryKey: ['command-palette', 'customers', term],
    queryFn: () => api.listCustomers(term ? `search=${encodeURIComponent(term)}&limit=10` : 'limit=50'),
    enabled: open,
    placeholderData: (prev) => prev,
  })
  const invoicesQuery = useQuery({
    queryKey: ['command-palette', 'invoices', term],
    queryFn: () => api.listInvoices(term ? `search=${encodeURIComponent(term)}&limit=10` : 'limit=50'),
    enabled: open,
    placeholderData: (prev) => prev,
  })
  const plansQuery = useQuery({
    queryKey: ['command-palette', 'plans'],
    queryFn: () => api.listPlans(),
    enabled: open,
  })
  const subscriptionsQuery = useQuery({
    queryKey: ['command-palette', 'subscriptions', term],
    queryFn: () => api.listSubscriptions(term ? `search=${encodeURIComponent(term)}&limit=10` : 'limit=50'),
    enabled: open,
    placeholderData: (prev) => prev,
  })
  const customers: Customer[] = customersQuery.data?.data ?? []
  const invoices: Invoice[] = invoicesQuery.data?.data ?? []
  const plans: Plan[] = plansQuery.data?.data ?? []
  const subscriptions: Subscription[] = subscriptionsQuery.data?.data ?? []

  const go = useCallback((href: string) => {
    navigate(href)
    handleClose()
  }, [navigate, handleClose])

  return (
    <CommandDialog open={open} onOpenChange={(isOpen) => { if (!isOpen) handleClose() }} className="sm:max-w-[560px] top-[15%] translate-y-0">
      <Command className="rounded-lg border-none shadow-none">
      <CommandInput
        placeholder="Search customers, invoices, subscriptions..."
        className="h-12 text-base"
        value={query}
        onValueChange={setQuery}
      />
      <CommandList className="max-h-[400px]">
        <CommandEmpty>No results found</CommandEmpty>

        <CommandGroup heading="Navigation">
          {navItems(isTestMode(user)).map(item => {
            const Icon = item.icon
            return (
              <CommandItem
                key={item.id}
                value={item.title}
                onSelect={() => go(item.href)}
                className="py-2.5 px-3 data-selected:bg-accent data-selected:text-accent-foreground"
              >
                <Icon className="mr-2 h-4 w-4" />
                <div className="flex flex-col">
                  <span>{item.title}</span>
                  <span className="text-xs opacity-60">{item.subtitle}</span>
                </div>
              </CommandItem>
            )
          })}
        </CommandGroup>

        {customers.length > 0 && (
          <>
            <CommandSeparator />
            <CommandGroup heading="Customers">
              {customers.slice(0, 10).map(c => (
                <CommandItem
                  key={`cust-${c.id}`}
                  value={`customer ${c.display_name} ${c.external_id} ${c.email || ''} ${c.id}`}
                  onSelect={() => go(`/customers/${c.id}`)}
                  className="py-2.5 px-3 data-selected:bg-accent data-selected:text-accent-foreground"
                >
                  <User className="mr-2 h-4 w-4" />
                  <div className="flex flex-col">
                    <span>{c.display_name}</span>
                    {/* Email beside external_id — support/ops often only
                        know the customer's email; show the field the
                        match likely hit. */}
                    <span className="text-xs opacity-60">
                      {c.external_id}
                      {c.email ? ` · ${c.email}` : ''}
                    </span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        )}

        {invoices.length > 0 && (
          <>
            <CommandSeparator />
            <CommandGroup heading="Invoices">
              {invoices.slice(0, 10).map(inv => (
                <CommandItem
                  key={`inv-${inv.id}`}
                  value={`invoice ${inv.invoice_number} ${inv.status}`}
                  onSelect={() => go(`/invoices/${inv.id}`)}
                  className="py-2.5 px-3 data-selected:bg-accent data-selected:text-accent-foreground"
                >
                  <FileText className="mr-2 h-4 w-4" />
                  <div className="flex flex-col">
                    <span>{inv.invoice_number}</span>
                    <span className="text-xs opacity-60">{invoiceStatus(inv, paletteNow(inv)).label} · {formatCents(inv.amount_due_cents, inv.currency)} due</span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        )}

        {plans.length > 0 && (
          <>
            <CommandSeparator />
            <CommandGroup heading="Plans">
              {plans.slice(0, 10).map(p => (
                <CommandItem
                  key={`plan-${p.id}`}
                  value={`plan ${p.name} ${p.code}`}
                  onSelect={() => go(`/plans/${p.id}`)}
                  className="py-2.5 px-3 data-selected:bg-accent data-selected:text-accent-foreground"
                >
                  <Zap className="mr-2 h-4 w-4" />
                  <div className="flex flex-col">
                    <span>{p.name}</span>
                    <span className="text-xs opacity-60">{p.code}</span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        )}

        {subscriptions.length > 0 && (
          <>
            <CommandSeparator />
            <CommandGroup heading="Subscriptions">
              {subscriptions.slice(0, 10).map(s => (
                <CommandItem
                  key={`sub-${s.id}`}
                  value={`subscription ${s.display_name} ${s.code} ${s.status}`}
                  onSelect={() => go(`/subscriptions/${s.id}`)}
                  className="py-2.5 px-3 data-selected:bg-accent data-selected:text-accent-foreground"
                >
                  <Hash className="mr-2 h-4 w-4" />
                  <div className="flex flex-col">
                    <span>{s.display_name}</span>
                    <span className="text-xs opacity-60">{s.code} · {s.status}</span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        )}
      </CommandList>
      </Command>
    </CommandDialog>
  )
}
