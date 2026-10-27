import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'

import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { showApiError } from '@/lib/formErrors'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import {
  getGetV1ProviderCostsQueryKey,
  useDeleteV1ProviderCostsId,
  usePutV1ProviderCosts,
} from '@/lib/gen/queries.gen'
import { api } from '@/lib/api'
import { usePageTitle } from '@/hooks/usePageTitle'
import { money, shiftDecimal } from '@/lib/priceDisplay'
import { dollarsToRateCents } from '@/lib/priceInput'

// Provider costs (ADR-079): the operator's COGS table — what THEY pay LLM
// providers per token. Feeds the per-event cost stamp at ingest and the
// customer margin card. Current-rate semantics: editing a rate only affects
// NEW events; history keeps its stamp.
//
// The API takes unit_cost_cents, US cents per token. Providers publish token
// prices per 1M tokens, so the page takes and shows dollars per 1M tokens and
// converts by moving the decimal point (no float math): $3.00 per 1M tokens
// is 300 cents per 1M, 0.0003 cents per token.
export default function ProviderCostsPage() {
  usePageTitle('Provider costs')
  const queryClient = useQueryClient()

  // The whole rate table, every page.
  const { data, isLoading, error: loadError, refetch } = useQuery({
    queryKey: getGetV1ProviderCostsQueryKey(),
    queryFn: () => api.listProviderCosts(),
  })
  const rates = data?.data ?? []

  const [provider, setProvider] = useState('')
  const [model, setModel] = useState('')
  const [tokenType, setTokenType] = useState('input')
  const [costPerMillion, setCostPerMillion] = useState('')

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: getGetV1ProviderCostsQueryKey() })

  const upsert = usePutV1ProviderCosts({
    mutation: {
      onSuccess: () => {
        invalidate()
        setCostPerMillion('')
        toast.success('Rate saved — new usage events pick it up automatically')
      },
      onError: (err) => showApiError(err, 'Failed to save rate'),
    },
  })
  const remove = useDeleteV1ProviderCostsId({
    mutation: {
      onSuccess: () => {
        invalidate()
        toast.success('Rate deleted — already-recorded event costs are unchanged')
      },
      onError: (err) => showApiError(err, 'Failed to delete rate'),
    },
  })

  // Cents per 1M tokens, or null when the box does not hold a plain number.
  const centsPerMillion = dollarsToRateCents(costPerMillion)
  const canSave =
    provider.trim() !== '' && model.trim() !== '' && tokenType.trim() !== '' &&
    centsPerMillion !== null && !centsPerMillion.startsWith('-')

  const save = () => {
    if (!canSave || centsPerMillion === null) return
    upsert.mutate({
      data: {
        provider: provider.trim().toLowerCase(),
        model: model.trim(),
        token_type: tokenType.trim().toLowerCase(),
        unit_cost_cents: shiftDecimal(centsPerMillion, -6),
      },
    })
  }

  return (
    <Layout>
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Provider costs</h1>
          <p className="text-sm text-muted-foreground mt-1 max-w-2xl">
            Enter what each AI provider charges you per 1M tokens, and Velox shows your
            margin on each customer. Add rates before you send usage: Velox works
            out each event's cost when it arrives and never goes back to older events.
          </p>
        </div>
      </div>

      <Card className="mt-6">
        <CardHeader>
          <CardTitle>Add or update a rate</CardTitle>
          <CardDescription>
            Saving the same provider, model and token type again replaces that
            rate. For the model, use the exact name your gateway reports (e.g.
            claude-sonnet-4-5-20250929) or the family (claude-sonnet-4.5); an
            exact name wins over a family.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="pc-provider">Provider</Label>
              <Input id="pc-provider" placeholder="anthropic" className="w-[150px]"
                value={provider} onChange={e => setProvider(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pc-model">Model</Label>
              <Input id="pc-model" placeholder="claude-sonnet-4.5" className="w-[230px]"
                value={model} onChange={e => setModel(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pc-token-type">Token type</Label>
              <Input id="pc-token-type" placeholder="input" className="w-[130px]"
                value={tokenType} onChange={e => setTokenType(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pc-cost">$ per 1M tokens</Label>
              <Input id="pc-cost" placeholder="3.00" inputMode="decimal" className="w-[150px]"
                value={costPerMillion} onChange={e => setCostPerMillion(e.target.value)} />
            </div>
            <Button onClick={save} disabled={!canSave || upsert.isPending}>
              {upsert.isPending
                ? <><Loader2 size={14} className="animate-spin mr-2" />Saving…</>
                : <><Plus size={14} className="mr-1.5" />Save rate</>}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground mt-2">
            Token type is the <code>token_type</code> value your usage events carry:
            input, output, cache_read, cache_write_5m, or cache_write_1h.
          </p>
        </CardContent>
      </Card>

      <Card className="mt-6">
        <CardHeader>
          <CardTitle>Current rates</CardTitle>
        </CardHeader>
        <CardContent>
          {loadError ? (
            // Not "No rates yet": a failed load must not look like an empty table.
            <LoadError what="rates" error={loadError} onRetry={() => refetch()} />
          ) : isLoading ? (
            <div className="flex justify-center p-8">
              <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : rates.length === 0 ? (
            <div className="text-center py-10">
              <p className="text-sm font-medium text-foreground">No rates yet</p>
              <p className="text-sm text-muted-foreground mt-1">
                Add what you pay per 1M tokens above — margin reporting starts with the
                first rate.
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Provider</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Token type</TableHead>
                  <TableHead className="text-right">$ / 1M tokens</TableHead>
                  <TableHead className="w-[50px]"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rates.map(r => (
                  <TableRow key={r.id}>
                    <TableCell className="text-sm">{r.provider}</TableCell>
                    <TableCell className="text-sm font-mono">{r.model}</TableCell>
                    <TableCell className="text-sm">{r.token_type}</TableCell>
                    <TableCell className="text-sm font-mono text-right">{money(r.unit_cost_cents, 6, r.currency)}</TableCell>
                    <TableCell>
                      <Button variant="ghost" size="icon" aria-label={`Delete ${r.model} ${r.token_type} rate`}
                        onClick={() => remove.mutate({ id: r.id })} disabled={remove.isPending}>
                        <Trash2 size={14} />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </Layout>
  )
}
