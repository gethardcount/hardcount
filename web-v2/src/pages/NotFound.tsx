import { Link } from 'react-router-dom'
import { Compass } from 'lucide-react'

import { Layout } from '@/components/Layout'
import { Button } from '@/components/ui/button'
import { usePageTitle } from '@/hooks/usePageTitle'

// Any address no route matches: a mistyped or old link. Before this the
// screen was blank, with no sidebar and no way back.
export default function NotFoundPage() {
  usePageTitle('Page not found')
  return (
    <Layout>
      <div className="flex flex-col items-center justify-center px-6 py-20 text-center">
        <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-muted">
          <Compass size={22} className="text-muted-foreground" aria-hidden />
        </div>
        <h1 className="text-lg font-semibold text-foreground">Page not found</h1>
        <p className="mt-1 max-w-sm text-sm text-muted-foreground">
          This link doesn't match any page. Check the address, or go to Home.
        </p>
        <Button size="sm" className="mt-4" render={<Link to="/" />}>
          Go to Home
        </Button>
      </div>
    </Layout>
  )
}
