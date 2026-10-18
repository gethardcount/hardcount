import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { toast } from 'sonner'

import { api, formatCents, formatDate } from '@/lib/api'
import { creditTotalsLine, grantState, grantUsedCents, ledgerEntryLabel, ledgerEntryTitle, splitGrants } from '@/lib/customerCredits'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { useEffectiveNow } from '@/hooks/useClockFrozenMap'
import { Section } from '@/components/page/Section'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { CreditDialog } from '@/pages/Credits'

// CustomerCreditsSection is the customer page's credit picture: the balance
// with where the credit went (granted, used, expired, canceled, deducted);
// every grant, including the finished ones behind a toggle; the latest
// ledger entries; and Grant credits.
// A grant that was fully applied to an invoice used to leave no trace here.
export function CustomerCreditsSection({ customerId, customerName, currency, testClockId, readOnly }: {
  customerId: string
  customerName: string
  currency: string
  testClockId?: string
  readOnly?: boolean
}) {
  const qc = useQueryClient()
  const now = useEffectiveNow(testClockId)
  const [showFinished, setShowFinished] = useState(false)
  const [grantOpen, setGrantOpen] = useState(false)

  // Same key as the page's balance stat, so one request serves both. No zero
  // fallback: a balance that failed to load shows a dash with Retry.
  const balance = useQuery({
    queryKey: ['customer-balance', customerId],
    queryFn: () => api.getBalance(customerId),
  })
  const grants = useQuery({
    queryKey: ['credit-grants', customerId, 'all'],
    queryFn: () => api.listCreditGrants(customerId, true),
  })
  // Under ['credits'] so invalidateMoneySurfaces refreshes it.
  const ledger = useQuery({
    queryKey: ['credits', 'customer-recent-ledger', customerId],
    queryFn: () => api.listLedger(customerId, { limit: 5, sort: 'created_at', dir: 'desc' }),
  })

  const split = splitGrants(grants.data?.grants ?? [], now)
  const shown = showFinished ? [...split.active, ...split.finished] : split.active
  const totals = balance.data ? creditTotalsLine(balance.data, currency) : null

  return (
    <Section
      title="Credits"
      description="Prepaid balance applied to this customer's invoices before payment is collected."
      actions={
        <>
          <Button variant="ghost" size="sm" render={<Link to={`/credits?customer=${customerId}`} />}>
            Full ledger
          </Button>
          {!readOnly && (
            <Button variant="outline" size="sm" onClick={() => setGrantOpen(true)}>Grant credits</Button>
          )}
        </>
      }
    >
      <div className="space-y-5">
        <div>
          <p className="text-xs text-muted-foreground">Balance</p>
          {balance.isError ? (
            <div className="mt-1 flex items-center gap-2">
              <p className="text-xl font-semibold" title="Couldn't load the credit balance">—</p>
              <Button variant="outline" size="xs" onClick={() => balance.refetch()}>Retry</Button>
            </div>
          ) : (
            <p className="mt-1 text-xl font-semibold tabular-nums">
              {balance.data ? formatCents(balance.data.balance_cents, currency) : '—'}
            </p>
          )}
          {totals && <p className="mt-0.5 text-xs text-muted-foreground tabular-nums">{totals}</p>}
          {grants.data && (grants.data.commit_remaining_cents > 0 || grants.data.promotional_remaining_cents > 0) && (
            // Kind split: money-backed commit must not read as one number
            // with free promotional credit.
            <p className="mt-0.5 text-xs text-muted-foreground tabular-nums">
              {grants.data.commit_remaining_cents > 0 && <>Commit {formatCents(grants.data.commit_remaining_cents, currency)}</>}
              {grants.data.commit_remaining_cents > 0 && grants.data.promotional_remaining_cents > 0 && ' · '}
              {grants.data.promotional_remaining_cents > 0 && <>Promotional {formatCents(grants.data.promotional_remaining_cents, currency)}</>}
            </p>
          )}
        </div>

        {(split.active.length > 0 || split.finished.length > 0) && (
          <div>
            <p className="mb-2 text-xs font-medium text-muted-foreground">Grants</p>
            {shown.some(g => g.grant_kind === 'commit' || g.grant_kind === 'promotional') && (
              <p className="mb-2 text-xs text-muted-foreground">
                Commit credit was bought on an invoice. Promotional credit is free and is used up first.
              </p>
            )}
            {shown.length > 0 && (
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Grant</TableHead>
                      <TableHead className="text-right">Granted</TableHead>
                      <TableHead className="text-right">Used</TableHead>
                      <TableHead className="text-right">Left</TableHead>
                      <TableHead>Expires</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {shown.map(g => {
                      const state = grantState(g, now)
                      return (
                        <TableRow key={g.id}>
                          <TableCell className="max-w-[16rem]">
                            <div className="flex items-center gap-2 min-w-0">
                              <span className="truncate text-sm" title={g.description}>{g.description || 'Credit grant'}</span>
                              {g.grant_kind === 'commit' && <Badge variant="info">Commit</Badge>}
                              {g.grant_kind === 'promotional' && <Badge variant="secondary">Promotional</Badge>}
                              {state === 'used_up' && <Badge variant="secondary">Used up</Badge>}
                              {state === 'expired' && <Badge variant="warning">Expired</Badge>}
                              {state === 'canceled' && <Badge variant="secondary">Canceled</Badge>}
                              {state === 'deducted' && <Badge variant="secondary">Deducted</Badge>}
                            </div>
                          </TableCell>
                          <TableCell className="text-right text-sm tabular-nums">{formatCents(g.amount_cents, currency)}</TableCell>
                          <TableCell className="text-right text-sm tabular-nums">
                            {formatCents(grantUsedCents(g), currency)}
                            {(g.deducted_cents ?? 0) > 0 && (
                              <div className="text-xs text-muted-foreground whitespace-nowrap">Deducted {formatCents(g.deducted_cents, currency)}</div>
                            )}
                          </TableCell>
                          <TableCell className="text-right text-sm font-medium tabular-nums">{formatCents(g.remaining_cents, currency)}</TableCell>
                          <TableCell className="text-sm text-muted-foreground whitespace-nowrap">{g.expires_at ? formatDate(g.expires_at) : 'Never'}</TableCell>
                        </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </div>
            )}
            {split.finished.length > 0 && (
              <button
                type="button"
                aria-expanded={showFinished}
                onClick={() => setShowFinished(v => !v)}
                className="mt-2 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
              >
                {showFinished ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                {showFinished ? 'Hide' : 'Show'} {split.finished.length} finished {split.finished.length === 1 ? 'grant' : 'grants'}
              </button>
            )}
          </div>
        )}

        {(ledger.data?.data.length ?? 0) > 0 && (
          <div>
            <p className="mb-2 text-xs font-medium text-muted-foreground">Latest entries</p>
            <ul className="divide-y divide-border rounded-md border border-border">
              {ledger.data!.data.map(e => (
                <li key={e.id} className="flex items-center justify-between gap-3 px-3 py-2 text-sm">
                  <div className="min-w-0">
                    <p className="truncate">
                      {ledgerEntryTitle(e)}
                      {e.invoice_id && (
                        <Link to={`/invoices/${e.invoice_id}`} className="ml-2 text-xs text-primary underline-offset-2 hover:underline">Invoice</Link>
                      )}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      {ledgerEntryTitle(e) === ledgerEntryLabel(e.entry_type) ? '' : `${ledgerEntryLabel(e.entry_type)} · `}{formatDate(e.created_at)}
                    </p>
                  </div>
                  <span className={e.amount_cents < 0 ? 'shrink-0 tabular-nums' : 'shrink-0 tabular-nums text-success-foreground'}>
                    {e.amount_cents > 0 ? '+' : ''}{formatCents(e.amount_cents, currency)}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        )}

        {grants.data && split.active.length === 0 && split.finished.length === 0 && (
          <p className="text-sm text-muted-foreground">No credits granted yet.</p>
        )}
      </div>

      {grantOpen && (
        <CreditDialog
          mode="grant"
          customerId={customerId}
          customerName={customerName}
          currency={currency}
          open={grantOpen}
          onOpenChange={setGrantOpen}
          onDone={() => {
            setGrantOpen(false)
            toast.success('Credits granted')
            invalidateMoneySurfaces(qc, { customerId })
          }}
        />
      )}
    </Section>
  )
}
