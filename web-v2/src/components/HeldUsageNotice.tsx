import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { toast } from 'sonner'

import { Callout } from '@/components/Callout'
import { Button } from '@/components/ui/button'
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
import {
  getListHeldUsageQueryKey,
  useDiscardHeldUsage,
  useListHeldUsage,
  useReleaseHeldUsage,
} from '@/lib/gen/queries.gen'
import { formatDateTime } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { canRelease, groupHeldByCustomer, heldLine, heldNextStep, heldUnlisted, releaseSummary, type HeldCustomer } from '@/lib/heldUsage'

// HeldUsageNotice shows usage the LiteLLM integration kept because its
// customer or meter did not exist yet (#1044). Before, those rows were lost
// and the only trace was a server log line. Renders nothing when nothing is
// held.
export function HeldUsageNotice({ className, onReleased }: { className?: string; onReleased?: () => void }) {
  const queryClient = useQueryClient()
  const { data } = useListHeldUsage()
  const groups = data?.data ?? []
  const [discardTarget, setDiscardTarget] = useState<HeldCustomer | null>(null)

  const refresh = () => queryClient.invalidateQueries({ queryKey: getListHeldUsageQueryKey() })

  const release = useReleaseHeldUsage({
    mutation: {
      onSuccess: (res) => {
        const s = releaseSummary(res)
        toast.success(s.title, { description: s.description || undefined })
        refresh()
        onReleased?.()
      },
      onError: (err) => showApiError(err, 'Release failed'),
    },
  })
  const discard = useDiscardHeldUsage({
    mutation: {
      onSuccess: (res) => {
        toast.success(`Discarded ${res.discarded.toLocaleString('en-US')} held usage ${res.discarded === 1 ? 'row' : 'rows'}`)
        setDiscardTarget(null)
        refresh()
      },
      onError: (err) => showApiError(err, 'Discard failed'),
    },
  })

  if (groups.length === 0) return null
  const customers = groupHeldByCustomer(groups)
  // The list is capped at the 200 customers held most recently; the totals
  // count every held row.
  const total = data?.total_rows ?? customers.reduce((n, c) => n + c.rows, 0)
  const unlisted = heldUnlisted(customers, data?.total_customers ?? customers.length, total)

  return (
    <>
      <Callout
        tone="amber"
        role="status"
        className={className}
        title="Usage held, not billed"
        details={
          <p>
            LiteLLM sent these rows for a customer or meter that did not exist. Velox kept them with their
            idempotency keys, so releasing them records each row once. A row counts toward the billing period its
            time falls in only if that period&apos;s invoice has not been built yet.
          </p>
        }
      >
        <p>
          {total.toLocaleString('en-US')} usage {total === 1 ? 'row is' : 'rows are'} held because the customer or
          meter does not exist yet. They are not billed until you release them.
        </p>
        {unlisted && <p className="mt-1 text-xs text-muted-foreground">{unlisted}</p>}
        <ul className="mt-2 space-y-2" aria-label="Held usage">
          {customers.map(c => (
            <li key={c.externalId} className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
              <div className="min-w-0">
                <p className="font-medium text-foreground break-words">{heldLine(c)}</p>
                <p className="text-xs text-muted-foreground">
                  {heldNextStep(c)} First row {formatDateTime(c.firstEventAt)}, last {formatDateTime(c.lastEventAt)}.
                </p>
              </div>
              <div className="flex shrink-0 gap-2">
                {!c.customerId && (
                  <Button variant="outline" size="sm" render={<Link to="/customers" />}>
                    Create customer
                  </Button>
                )}
                {canRelease(c, groups) && (
                  <Button
                    size="sm"
                    disabled={release.isPending}
                    onClick={() => release.mutate({ data: { external_customer_id: c.externalId } })}
                    aria-label={`Release held usage for ${c.externalId}`}
                  >
                    Release
                  </Button>
                )}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setDiscardTarget(c)}
                  aria-label={`Discard held usage for ${c.externalId}`}
                >
                  Discard
                </Button>
              </div>
            </li>
          ))}
        </ul>
      </Callout>

      <AlertDialog open={!!discardTarget} onOpenChange={(open) => { if (!open) setDiscardTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard held usage</AlertDialogTitle>
            <AlertDialogDescription>
              {discardTarget
                ? `Discard ${discardTarget.rows.toLocaleString('en-US')} held usage rows for '${discardTarget.externalId}'? They will never be billed. Velox keeps them as a record.`
                : ''}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setDiscardTarget(null)}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => discardTarget && discard.mutate({ data: { external_customer_id: discardTarget.externalId } })}
              disabled={discard.isPending}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              Discard
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
