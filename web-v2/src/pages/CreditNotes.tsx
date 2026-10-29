import { useState, useMemo, useEffect } from 'react'
import { genericStatus, invoiceStatus } from '@/lib/statusLabels'
import { StatusBadge } from '@/components/StatusBadge'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { api, downloadCreditNotePDF, formatCents, formatDate, formatDateTime, getActiveCurrency } from '@/lib/api'
import type { CreditNote, Invoice, Customer } from '@/lib/api'
import { wallClockNow } from '@/lib/effectiveNow'
import { showApiError } from '@/lib/formErrors'
import { downloadCSV } from '@/lib/csv'
import { CREDIT_NOTE_CSV_HEADERS, creditNoteCsvRow } from '@/lib/creditNoteCsv'
import { creditNoteStatFigures, creditNotesListView } from '@/lib/creditNoteStats'
import { canIssueCreditNote } from '@/lib/creditNoteCeilings'
import { Layout } from '@/components/Layout'
import { ScrollPane } from '@/components/ui/scroll-pane'
import { SimulatedBadge } from '@/components/TestClockBadge'
import { CustomerCombobox } from '@/components/CustomerCombobox'
import { IssueCreditDialog } from '@/components/IssueCreditDialog'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import { useSortable, type SortDir } from '@/hooks/useSortable'
import { useUrlState } from '@/hooks/useUrlState'
import { cn } from '@/lib/utils'
import { creditNoteReasonLabel } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { LoadError } from '@/components/LoadError'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from '@/components/ui/pagination'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Plus, Search, Download, Loader2, ArrowUpDown, ArrowUp, ArrowDown, FileMinus, Mail } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

const PAGE_SIZE = 25

function SortableHead({
  label, sortKey: key, activeSortKey, sortDir, onSort, className,
}: {
  label: string; sortKey: string; activeSortKey: string; sortDir: 'asc' | 'desc'; onSort: (key: string) => void; className?: string
}) {
  const active = key === activeSortKey
  return (
    <TableHead className={className}>
      <button
        onClick={() => onSort(key)}
        className="flex items-center gap-1.5 text-xs font-medium hover:text-foreground transition-colors"
      >
        {label}
        {active ? (
          sortDir === 'asc' ? <ArrowUp size={14} /> : <ArrowDown size={14} />
        ) : (
          <ArrowUpDown size={14} className="opacity-40" />
        )}
      </button>
    </TableHead>
  )
}

// refundUnconfirmed: an issued credit note with a refund amount whose refund
// result was never recorded (a failed write after Stripe answered, or a crash
// in that window). Same rule as the backend's needs-attention filter
// (domain.CreditNoteRefundNeedsAttentionSQL): real credit notes only, after a
// one-hour grace so a refund still being issued is not flagged. The backend
// times the hour from updated_at, which for this state is the issue time, so
// issued_at is the same instant. Retry refund recovers it: the backend looks
// for the Stripe refund before creating one.
function refundUnconfirmed(note: CreditNote): boolean {
  if (note.is_simulated || note.status !== 'issued' || note.refund_status !== 'none' || note.refund_amount_cents <= 0) return false
  const issued = Date.parse(note.issued_at || note.created_at)
  return Number.isFinite(issued) && wallClockNow() - issued > 60 * 60 * 1000
}

export default function CreditNotesPage() {
  usePageTitle('Credit notes')
  const [showCreate, setShowCreate] = useState(false)
  // The invoice picked in the chooser; its own dialog then issues the note.
  const [creditInvoice, setCreditInvoice] = useState<Invoice | null>(null)
  const [confirmIssue, setConfirmIssue] = useState<string | null>(null)
  const [confirmVoid, setConfirmVoid] = useState<string | null>(null)
  const [sendTarget, setSendTarget] = useState<CreditNote | null>(null)
  const [urlState, setUrlState] = useUrlState({
    search: '',
    status: '',
    refund_status: '',
    page: '1',
    sort: 'created_at',
    dir: 'desc',
  })
  const { search, status: filterStatus, refund_status: refundStatusFilter, sort: sortKey } = urlState
  const sortDir = urlState.dir as SortDir
  const page = Math.max(1, parseInt(urlState.page) || 1)
  const queryClient = useQueryClient()

  // Server-side sort: pass sort/dir; backend tie-breaks on id.
  const notesQueryParams = useMemo(() => {
    const params = new URLSearchParams()
    // Explicit page-size ceiling (server clamps at 100; the implicit
    // default was 25, silently truncating the list under the client-side
    // pagination). Search here spans joined customer/invoice fields, so
    // rows stay client-paginated; the notice below surfaces truncation.
    params.set('limit', '100')
    if (filterStatus) params.set('status', filterStatus)
    if (refundStatusFilter) params.set('refund_status', refundStatusFilter)
    if (sortKey) params.set('sort', sortKey)
    if (sortDir) params.set('dir', sortDir)
    return params.toString()
  }, [filterStatus, refundStatusFilter, sortKey, sortDir])
  const { data: notesData, isLoading: notesLoading, error: notesErrorObj, refetch: refetchNotes } = useQuery({
    queryKey: ['credit-notes', filterStatus, refundStatusFilter, sortKey, sortDir],
    queryFn: () => api.listCreditNotes(notesQueryParams),
  })

  // Fetch only the customers + invoices referenced by visible credit
  // notes (avoids the "Unknown" / truncated-ID pagination bug — see
  // Invoices.tsx for full rationale). Both lookups re-fetch when the
  // visible credit-note page changes.
  const refIds = useMemo(() => {
    const cSet = new Set<string>()
    const iSet = new Set<string>()
    ;(notesData?.data ?? []).forEach(n => {
      if (n.customer_id) cSet.add(n.customer_id)
      if (n.invoice_id) iSet.add(n.invoice_id)
    })
    return { customers: Array.from(cSet), invoices: Array.from(iSet) }
  }, [notesData])
  const { data: invoicesData } = useQuery({
    queryKey: ['invoices-by-ids-for-cn', refIds.invoices],
    queryFn: () => api.listInvoices(refIds.invoices.length > 0 ? `ids=${refIds.invoices.join(',')}&limit=${refIds.invoices.length}` : ''),
    enabled: refIds.invoices.length > 0,
  })

  const { data: customersData } = useQuery({
    queryKey: ['customers-by-ids-for-cn', refIds.customers],
    queryFn: () => api.listCustomers(refIds.customers.length > 0 ? `ids=${refIds.customers.join(',')}&limit=${refIds.customers.length}` : ''),
    enabled: refIds.customers.length > 0,
  })

  const notes = useMemo(() => notesData?.data ?? [], [notesData])
  const loading = notesLoading

  const invoiceMap = useMemo(() => {
    const iMap: Record<string, Invoice> = {}
    ;(invoicesData?.data ?? []).forEach(inv => { iMap[inv.id] = inv })
    return iMap
  }, [invoicesData])

  const customerMap = useMemo(() => {
    const cMap: Record<string, Customer> = {}
    ;(customersData?.data ?? []).forEach(c => { cMap[c.id] = c })
    return cMap
  }, [customersData])

  const issueMutation = useMutation({
    mutationFn: (id: string) => api.issueCreditNote(id),
    onSuccess: () => {
      invalidateMoneySurfaces(queryClient)
      toast.success('Credit note issued')
    },
    onError: (err) => showApiError(err, 'Failed to issue'),
  })

  const voidMutation = useMutation({
    mutationFn: (id: string) => api.voidCreditNote(id),
    onSuccess: () => {
      invalidateMoneySurfaces(queryClient)
      toast.success('Credit note voided')
    },
    onError: (err) => showApiError(err, 'Failed to void'),
  })

  const retryRefundMutation = useMutation({
    mutationFn: (id: string) => api.retryCreditNoteRefund(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['credit-notes'] })
      toast.success('Refund retried successfully')
    },
    onError: (err) => showApiError(err, 'Failed to retry refund'),
  })

  // Header, tabs and stat cards come from the server, which counts every note
  // and sums money per currency. The list below is a page of at most 100 rows
  // filtered to the open tab, so it cannot be their source (E12).
  const { data: statsData } = useQuery({
    queryKey: ['credit-notes', 'stats'],
    queryFn: () => api.getCreditNoteStats(),
  })
  const stats = statsData ? creditNoteStatFigures(statsData, getActiveCurrency()) : null
  const counts = stats?.counts

  // Filter + search
  const filtered = useMemo(() => notes.filter(n => {
    if (filterStatus && n.status !== filterStatus) return false
    if (search) {
      const q = search.toLowerCase()
      const custName = customerMap[n.customer_id]?.display_name?.toLowerCase() || ''
      const invNum = invoiceMap[n.invoice_id]?.invoice_number?.toLowerCase() || ''
      if (!n.credit_note_number.toLowerCase().includes(q) && !custName.includes(q) && !invNum.includes(q) && !n.reason.toLowerCase().includes(q)) return false
    }
    return true
  }), [notes, filterStatus, search, customerMap, invoiceMap])

  // Server-side sort end-to-end. Client-side re-sort would only sort
  // the current page, breaking pagination semantics.
  const { onSort } = useSortable(
    filtered,
    sortKey,
    sortDir,
    (key, dir) => setUrlState({ sort: key, dir }),
  )
  const sorted = filtered

  // The list is the 100 newest notes that match the server-side filters;
  // total counts them all.
  const listTotal = notesData?.total ?? 0
  const truncated = listTotal > notes.length
  // Tab counts, the open tab's total and the empty list's words for the open
  // filters. The server counts by status only, so under the refund filter the
  // tabs show no counts.
  const { tabCounts, empty } = creditNotesListView(counts, filterStatus, refundStatusFilter)
  const totalPages = Math.ceil(sorted.length / PAGE_SIZE)
  const currentPage = Math.min(page, totalPages || 1)
  const paginated = sorted.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE)

  const handleExport = () => {
    const rows = filtered.map(n => creditNoteCsvRow(
      n,
      customerMap[n.customer_id]?.display_name || '',
      invoiceMap[n.invoice_id]?.invoice_number || '',
    ))
    downloadCSV('credit-notes.csv', CREDIT_NOTE_CSV_HEADERS, rows)
  }

  return (
    <Layout>
      {/* Header */}
      <div className="flex flex-wrap justify-between items-center gap-3">
        <div className="min-w-0 flex-1 basis-72">
          <h1 className="text-2xl font-semibold text-foreground">Credit notes</h1>
          <p className="text-sm text-muted-foreground mt-1">A credit note takes money off an invoice. If the invoice is unpaid, the customer owes less; if it's paid, the money goes back as a refund or as credit on their balance{counts && counts.all > 0 ? ` · ${counts.all} total` : ''}</p>
        </div>
        <div className="flex items-center gap-2">
          {notes.length > 0 && (
            <Button variant="outline" size="sm" onClick={handleExport}>
              <Download size={16} className="mr-2" /> Export
            </Button>
          )}
          <Button size="sm" onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" /> Issue credit note
          </Button>
        </div>
      </div>

      {/* Summary cards */}
      {stats && stats.counts.all > 0 && (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mt-6">
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Total credited</p>
              <p className="text-xl font-semibold text-foreground mt-1 tabular-nums">{stats.total}</p>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Refunded to Card</p>
              <p className="text-xl font-semibold text-primary mt-1 tabular-nums">{stats.refunded}</p>
              {(stats.refundPending || stats.refundFailed) && (
                <p className="text-xs mt-0.5">
                  {stats.refundPending && <span className="text-warning-foreground">{stats.refundPending} not confirmed yet</span>}
                  {stats.refundPending && stats.refundFailed && <span className="text-muted-foreground"> · </span>}
                  {stats.refundFailed && <span className="text-danger-foreground">{stats.refundFailed} failed — not returned</span>}
                </p>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Applied to Balance</p>
              <p className="text-xl font-semibold text-success-foreground mt-1 tabular-nums">{stats.credited}</p>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Issued</p>
              <p className="text-xl font-semibold text-foreground mt-1">{stats.counts.issued}</p>
              {stats.counts.draft > 0 && <p className="text-xs text-warning-foreground mt-0.5">{stats.counts.draft} draft</p>}
            </CardContent>
          </Card>
        </div>
      )}

      {/* Filter tabs + search */}
      {(notes.length > 0 || filterStatus) && (
        <div className="flex items-center gap-3 mt-6">
          <div className="flex gap-1 bg-muted rounded-lg p-1">
            {[
              { value: '', label: 'All', count: tabCounts?.all ?? 0 },
              { value: 'draft', label: 'Draft', count: tabCounts?.draft ?? 0 },
              { value: 'issued', label: 'Issued', count: tabCounts?.issued ?? 0 },
              { value: 'voided', label: 'Voided', count: tabCounts?.voided ?? 0 },
            ].map(f => (
              <button
                key={f.value}
                onClick={() => setUrlState({ status: f.value, page: '1' })}
                className={cn(
                  'px-3 py-1.5 rounded-md text-xs font-medium transition-colors',
                  filterStatus === f.value
                    ? 'bg-background text-foreground shadow-sm'
                    : 'text-muted-foreground hover:text-foreground'
                )}
              >
                {f.label}
                {f.count > 0 && <span className="ml-1 text-muted-foreground">{f.count}</span>}
              </button>
            ))}
          </div>
          <div className="relative flex-1">
            <Search size={16} className="absolute left-3 top-2.5 text-muted-foreground" />
            <Input
              value={search}
              onChange={e => setUrlState({ search: e.target.value, page: '1' })}
              placeholder="Search by number, customer, invoice, or reason..."
              className="pl-9"
            />
          </div>
        </div>
      )}

      {refundStatusFilter === 'needs_attention' && (
        <div className="mt-3 flex items-center gap-2 text-xs">
          <span className="inline-flex items-center gap-1.5 rounded-full bg-destructive/10 px-2.5 py-1 font-medium text-destructive">
            Showing refunds needing attention (failed, unconfirmed, or pending &gt; 72h)
            <button
              onClick={() => setUrlState({ refund_status: '', page: '1' })}
              className="hover:text-foreground"
              aria-label="Clear refund filter"
            >
              ✕
            </button>
          </span>
        </div>
      )}

      {/* Table */}
      <Card className="mt-4">
        <CardContent className="p-0">
          {notesErrorObj ? (
            <LoadError what="credit notes" error={notesErrorObj} onRetry={() => refetchNotes()} />
          ) : loading ? (
            <TableSkeleton columns={7} />
          ) : notes.length === 0 ? (
            empty ? (
              <EmptyState
                title={empty.title}
                description={empty.description}
                action={{
                  label: 'Clear filter',
                  variant: 'outline',
                  onClick: () => setUrlState({ [empty.clear]: '', page: '1' }),
                }}
              />
            ) : (
              <EmptyState
                icon={FileMinus}
                title="No credit notes yet"
                description="Open an invoice and issue a credit note to lower what it charges."
                action={{
                  label: 'Issue credit note',
                  icon: Plus,
                  onClick: () => setShowCreate(true),
                }}
              />
            )
          ) : filtered.length === 0 ? (
            <p className="px-6 py-8 text-sm text-muted-foreground text-center">
              No credit notes match your search
            </p>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <SortableHead label="Number" sortKey="credit_note_number" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <TableHead className="text-xs font-medium">Customer</TableHead>
                    <TableHead className="text-xs font-medium">Invoice</TableHead>
                    <TableHead className="text-xs font-medium">Reason</TableHead>
                    <SortableHead label="Status" sortKey="status" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <SortableHead label="Amount" sortKey="total_cents" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} className="text-right" />
                    <SortableHead label="Date" sortKey="created_at" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {paginated.map((note: CreditNote) => {
                    const isRefund = note.refund_amount_cents > 0
                    const channels: string[] = []
                    if (note.refund_amount_cents > 0) channels.push('refund')
                    if (note.credit_amount_cents > 0) channels.push('credit')
                    if ((note.out_of_band_amount_cents ?? 0) > 0) channels.push('out of band')
                    // All three channels zero = an adjustment that reduced the
                    // invoice's amount_due (the unpaid-downgrade path), not a refund/credit.
                    const channelLabel = channels.length === 0 ? 'applied to invoice' : channels.join(' + ')
                    return (
                      <TableRow key={note.id}>
                        <TableCell>
                          <span className="inline-flex items-center gap-2">
                            <p className="text-sm font-medium text-foreground">{note.credit_note_number}</p>
                            {note.is_simulated && <SimulatedBadge title="Dates on this credit note follow its test clock's calendar, not today's date" />}
                          </span>
                          <p className="text-xs text-muted-foreground mt-0.5">{channelLabel}</p>
                        </TableCell>
                        <TableCell className="text-sm">
                          <Link to={`/customers/${note.customer_id}`} className="text-primary hover:underline">
                            {customerMap[note.customer_id]?.display_name || 'Unknown'}
                          </Link>
                        </TableCell>
                        <TableCell className="text-sm">
                          <Link to={`/invoices/${note.invoice_id}`} className="text-primary hover:underline font-mono">
                            {invoiceMap[note.invoice_id]?.invoice_number || note.invoice_id.slice(0, 12) + '...'}
                          </Link>
                        </TableCell>
                        {/* Tooltip must show what the CELL shows. `note.reason` is the raw
                            enum code for system-issued notes — the cell renders
                            "Quantity decrease" while the hover said
                            "subscription_quantity_decrease". For operator free text
                            the two are identical, so nothing is lost either way. */}
                        <TableCell className="text-sm text-muted-foreground max-w-[220px] truncate" title={creditNoteReasonLabel(note.reason)}>
                          {creditNoteReasonLabel(note.reason)}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5">
                            <StatusBadge status={genericStatus(note.status)} />
                            {isRefund && refundUnconfirmed(note) && (
                              <Badge variant="danger" title="The refund's result was never recorded. Retry checks Stripe before creating a new refund.">
                                not confirmed
                              </Badge>
                            )}
                            {isRefund && note.status === 'issued' && note.refund_status && note.refund_status !== 'none' && (
                              <Badge variant={note.refund_status === 'succeeded' ? 'success' : note.refund_status === 'failed' ? 'danger' : 'warning'}>
                                {note.refund_status === 'succeeded' ? 'refunded' : note.refund_status === 'failed' ? 'refund failed' : 'refund pending'}
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className="text-right tabular-nums font-mono text-sm">
                          {formatCents(note.total_cents, note.currency)}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center justify-between">
                            <span className="text-sm text-muted-foreground whitespace-nowrap">
                              {note.issued_at ? formatDate(note.issued_at) : formatDateTime(note.created_at)}
                            </span>
                            {note.status === 'draft' && (
                              <div className="flex items-center gap-1 ml-3">
                                <Button variant="outline" size="sm" onClick={() => setConfirmIssue(note.id)}>
                                  Issue
                                </Button>
                                <Button variant="outline" size="sm" className="text-destructive hover:text-destructive" onClick={() => setConfirmVoid(note.id)}>
                                  Void
                                </Button>
                              </div>
                            )}
                            {note.status === 'issued' && (
                              <div className="flex items-center gap-1 ml-3">
                                {(note.refund_status === 'failed' || note.refund_status === 'pending' || refundUnconfirmed(note)) && (
                                  <Button
                                    variant="outline"
                                    size="sm"
                                    onClick={() => retryRefundMutation.mutate(note.id)}
                                    disabled={retryRefundMutation.isPending}
                                    title={note.refund_status === 'failed' ? 'Retry Stripe refund' : 'Process Stripe refund'}
                                  >
                                    Retry refund
                                  </Button>
                                )}
                                <Button
                                  variant="outline"
                                  size="sm"
                                  onClick={async () => {
                                    try {
                                      await downloadCreditNotePDF(note.id, note.credit_note_number)
                                    } catch (err) {
                                      showApiError(err, 'Failed to download PDF')
                                    }
                                  }}
                                >
                                  <Download size={14} className="mr-1.5" /> PDF
                                </Button>
                                <Button variant="outline" size="sm" onClick={() => setSendTarget(note)}>
                                  <Mail size={14} className="mr-1.5" /> Send
                                </Button>
                              </div>
                            )}
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>

              {truncated && (
                <div className="border-t border-border px-4 py-2 text-xs text-muted-foreground">
                  {`Showing the ${notes.length} most recent of ${listTotal} credit notes. Use the status filter to narrow older ones.`}
                </div>
              )}

              {/* Pagination */}
              {totalPages > 1 && (
                <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                  <p className="text-xs text-muted-foreground">
                    Showing {(currentPage - 1) * PAGE_SIZE + 1}
                    {'\u2013'}
                    {Math.min(currentPage * PAGE_SIZE, sorted.length)} of {sorted.length}
                  </p>
                  <Pagination>
                    <PaginationContent>
                      <PaginationItem>
                        <PaginationPrevious
                          onClick={() => setUrlState({ page: String(Math.max(1, currentPage - 1)) })}
                          className={cn(currentPage <= 1 && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                      {Array.from({ length: Math.min(totalPages, 5) }, (_, i) => {
                        let pageNum: number
                        if (totalPages <= 5) {
                          pageNum = i + 1
                        } else if (currentPage <= 3) {
                          pageNum = i + 1
                        } else if (currentPage >= totalPages - 2) {
                          pageNum = totalPages - 4 + i
                        } else {
                          pageNum = currentPage - 2 + i
                        }
                        return (
                          <PaginationItem key={pageNum}>
                            <PaginationLink
                              onClick={() => setUrlState({ page: String(pageNum) })}
                              isActive={currentPage === pageNum}
                            >
                              {pageNum}
                            </PaginationLink>
                          </PaginationItem>
                        )
                      })}
                      <PaginationItem>
                        <PaginationNext
                          onClick={() => setUrlState({ page: String(Math.min(totalPages, currentPage + 1)) })}
                          className={cn(currentPage >= totalPages && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                    </PaginationContent>
                  </Pagination>
                </div>
              )}
            </>
          )}
        </CardContent>
      </Card>

      {/* Issue a credit note: choose the invoice, then its own dialog opens */}
      {showCreate && (
        <ChooseInvoiceDialog
          onClose={() => setShowCreate(false)}
          onChoose={(inv) => { setShowCreate(false); setCreditInvoice(inv) }}
        />
      )}
      {creditInvoice && (
        <IssueCreditForInvoice
          invoice={creditInvoice}
          onClose={() => setCreditInvoice(null)}
          onCreated={() => {
            setCreditInvoice(null)
            invalidateMoneySurfaces(queryClient, { invoiceId: creditInvoice.id, customerId: creditInvoice.customer_id })
            toast.success('Credit note issued')
          }}
        />
      )}

      {/* Issue Confirm */}
      <AlertDialog open={confirmIssue !== null} onOpenChange={(open) => { if (!open) setConfirmIssue(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Issue credit note</AlertDialogTitle>
            <AlertDialogDescription>
              Issue this credit note? This will apply the credit or initiate the refund. Once issued, it cannot be reversed.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => { if (confirmIssue) issueMutation.mutate(confirmIssue); setConfirmIssue(null) }}>
              Issue credit note
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Void Confirm */}
      <TypedConfirmDialog
        open={confirmVoid !== null}
        onOpenChange={(open) => { if (!open) setConfirmVoid(null) }}
        title="Void credit note"
        description="Voiding this credit note reverses its effect permanently. This action cannot be undone."
        confirmWord="VOID"
        confirmLabel="Void credit note"
        onConfirm={() => { if (confirmVoid) voidMutation.mutate(confirmVoid); setConfirmVoid(null) }}
        loading={voidMutation.isPending}
      />

      {/* Send credit-note email */}
      {sendTarget && (
        <SendCreditNoteDialog
          note={sendTarget}
          onClose={() => setSendTarget(null)}
          onSent={() => { setSendTarget(null); toast.success('Credit note email sent') }}
        />
      )}
    </Layout>
  )
}

// --- Send Credit Note Email Dialog (ADR-082) ---

function SendCreditNoteDialog({ note, onClose, onSent }: {
  note: CreditNote; onClose: () => void; onSent: () => void
}) {
  // Fetch the full customer for the recipient + CC prefill — the list
  // rows the page already holds don't carry additional_emails.
  const { data: cust } = useQuery({
    queryKey: ['customer', note.customer_id],
    queryFn: () => api.getCustomer(note.customer_id),
  })
  const [email, setEmail] = useState('')
  const [cc, setCc] = useState('')
  const [touched, setTouched] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState('')

  // Prefill once the customer loads (unless the operator already typed).
  if (cust && !touched && email === '' && (cust.email || (cust.additional_emails || []).length > 0)) {
    setEmail(cust.email || '')
    setCc((cust.additional_emails || []).join(', '))
  }

  const handleSend = async () => {
    if (!email.trim() || sending) return
    setSending(true)
    setError('')
    try {
      const ccList = cc.split(',').map(s => s.trim()).filter(Boolean)
      await api.sendCreditNoteEmail(note.id, email.trim(), ccList)
      onSent()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to send')
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Email Credit Note {note.credit_note_number}</DialogTitle>
          <DialogDescription>Sends the credit note PDF to your customer.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-2">
            <label htmlFor="cn-email" className="text-sm font-medium">Recipient email</label>
            <Input id="cn-email" type="email" value={email}
              onChange={e => { setEmail(e.target.value); setTouched(true) }}
              placeholder="customer@example.com" />
          </div>
          <div className="space-y-2">
            <label htmlFor="cn-cc" className="text-sm font-medium">CC</label>
            <Input id="cn-cc" value={cc}
              onChange={e => { setCc(e.target.value); setTouched(true) }}
              placeholder="finance@customer.com, ap@customer.com" />
            <p className="text-xs text-muted-foreground">Separate with commas. Clear the field to email only the recipient.</p>
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button onClick={handleSend} disabled={sending || !email.trim()}>
              {sending ? <><Loader2 size={14} className="animate-spin mr-2" />Sending...</> : 'Send email'}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// --- Issue a credit note from this page ---

// ChooseInvoiceDialog picks the invoice to credit: the customer first, then
// one of their invoices that can take a credit note. The note itself is made
// in the invoice page's dialog (IssueCreditDialog), so both places apply the
// same ceilings and channels. This page used to have its own form, which
// listed only the newest 50 finalized and 50 paid invoices of the whole
// account, labelled amounts in the workspace currency, capped the amount at
// the invoice's due or paid figure while ignoring earlier notes, and offered a
// refund the server always refused (E8 / D-13).
// Mounted only while open, so each opening starts with no customer picked.
function ChooseInvoiceDialog({ onClose, onChoose }: {
  onClose: () => void
  onChoose: (invoice: Invoice) => void
}) {
  const [customerId, setCustomerId] = useState('')
  const [numberSearch, setNumberSearch] = useState('')
  const search = useDebouncedValue(numberSearch.trim(), 300)

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['credit-notes', 'choose-invoice', customerId, search],
    queryFn: () => {
      const params = new URLSearchParams({ customer_id: customerId, limit: '100' })
      if (search) params.set('search', search)
      return api.listInvoices(params.toString())
    },
    enabled: !!customerId,
  })
  const rows = data?.data ?? []
  const creditable = rows.filter(canIssueCreditNote)
  const truncated = rows.length >= 100

  return (
    <Dialog open onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Choose an invoice to credit</DialogTitle>
          <DialogDescription>
            A credit note takes money off one invoice. Pick the customer, then the invoice.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-2">
            <Label>Customer</Label>
            <CustomerCombobox value={customerId} onChange={setCustomerId} />
          </div>

          {customerId && (
            <div className="space-y-2">
              <Label htmlFor="cn-invoice-search">Invoice</Label>
              <Input
                id="cn-invoice-search"
                value={numberSearch}
                onChange={(e) => setNumberSearch(e.target.value)}
                placeholder="Filter by invoice number"
              />
              {error ? (
                <LoadError what="invoices" error={error} onRetry={() => refetch()} />
              ) : isLoading ? (
                <div className="py-6 flex justify-center">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : creditable.length === 0 ? (
                <p className="text-sm text-muted-foreground py-3">
                  {search
                    ? 'No invoice of this customer with that number can take a credit note.'
                    : 'This customer has no invoice that can take a credit note. Only finalized or paid invoices can, and not while a payment is in progress.'}
                </p>
              ) : (
                <ScrollPane as="ul" className="max-h-72 rounded-md border divide-y">
                  {creditable.map(inv => (
                    <li key={inv.id}>
                      <button
                        type="button"
                        onClick={() => onChoose(inv)}
                        className="w-full flex items-center justify-between gap-3 px-3 py-2 text-left text-sm hover:bg-muted focus-visible:bg-muted focus-visible:outline-none"
                      >
                        <span className="flex items-center gap-2 min-w-0">
                          <span className="font-mono truncate">{inv.invoice_number}</span>
                          <StatusBadge status={invoiceStatus(inv, wallClockNow())} />
                        </span>
                        <span className="tabular-nums text-muted-foreground whitespace-nowrap">
                          {formatCents(inv.total_amount_cents, inv.currency)}
                        </span>
                      </button>
                    </li>
                  ))}
                </ScrollPane>
              )}
              {truncated && (
                <p className="text-xs text-muted-foreground">
                  Showing this customer&apos;s newest 100 invoices. Type a number to find an older one.
                </p>
              )}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// IssueCreditForInvoice opens the invoice page's dialog for the chosen
// invoice, once its existing credit notes are loaded: the dialog's ceilings
// count every non-voided note, as the server does.
function IssueCreditForInvoice({ invoice, onClose, onCreated }: {
  invoice: Invoice
  onClose: () => void
  onCreated: () => void
}) {
  const { data: existing, error } = useQuery({
    queryKey: ['credit-notes', 'for-invoice', invoice.id],
    queryFn: async () => (await api.listInvoiceCreditNotes(invoice.id)).data,
  })
  useEffect(() => {
    if (error) {
      showApiError(error, "Couldn't load this invoice's credit notes")
      onClose()
    }
  }, [error, onClose])
  if (!existing) return null
  return (
    <IssueCreditDialog
      invoice={invoice}
      existingCreditNotes={existing}
      onClose={onClose}
      onCreated={onCreated}
    />
  )
}
