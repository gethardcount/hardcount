import { Card, CardContent } from '@/components/ui/card'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { Shimmer } from '@/components/ui/TableSkeleton'

interface DetailSkeletonProps {
  to: string
  parentLabel: string
}

// DetailSkeleton is the initial-load state for entity detail pages
// (invoice, customer, subscription, plan, meter). It renders
// the REAL breadcrumb in its final position plus a stable header + body
// skeleton, so nothing shifts when data arrives — previously these pages
// early-returned a centered spinner with a hand-written breadcrumb
// fallback, and the whole page popped/shifted on resolve. List pages get
// the same treatment from TableSkeleton; this is the detail-page sibling.
export function DetailSkeleton({ to, parentLabel }: DetailSkeletonProps) {
  return (
    <div aria-busy aria-label="Loading">
      <DetailBreadcrumb
        to={to}
        parentLabel={parentLabel}
        currentLabel={<Shimmer className="h-3.5 w-28 inline-block align-middle" />}
      />

      {/* Header: title block + action buttons, mirroring the detail-page header shape. */}
      <div className="flex items-start justify-between gap-4 mb-6">
        <div className="space-y-2">
          <Shimmer className="h-7 w-56" />
          <Shimmer className="h-4 w-36" />
        </div>
        <div className="flex gap-2">
          <Shimmer className="h-9 w-24 rounded-md" />
          <Shimmer className="h-9 w-28 rounded-md" />
        </div>
      </div>

      {/* Stat strip */}
      <div className="grid gap-4 md:grid-cols-3 mb-6">
        {Array.from({ length: 3 }).map((_, i) => (
          <Card key={i}>
            <CardContent className="px-6 py-4 space-y-2">
              <Shimmer className="h-3 w-20" />
              <Shimmer className="h-6 w-28" />
            </CardContent>
          </Card>
        ))}
      </div>

      {/* Body card with ghost rows */}
      <Card>
        <CardContent className="px-6 py-4 space-y-3">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="flex items-center gap-4">
              <Shimmer className="h-4 w-32" />
              <Shimmer className="h-4 flex-1 max-w-sm" />
              <Shimmer className="h-4 w-20" />
            </div>
          ))}
        </CardContent>
      </Card>
    </div>
  )
}
