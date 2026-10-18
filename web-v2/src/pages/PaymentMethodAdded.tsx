import { useSearchParams } from 'react-router-dom'
import { usePageTitle } from '@/hooks/usePageTitle'
import { CheckCircle2, AlertCircle, ShieldCheck } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { VeloxLogo } from '@/components/VeloxLogo'

// PaymentMethodAdded is the public landing page Stripe Checkout
// redirects to after a customer completes (or cancels) a setup-mode
// session. Hit via the URL the operator sent in the "Add payment
// method" email — the customer is not signed in to anything, so this
// page is unauthenticated and self-contained.
//
// Two states, gated on ?status= query (?status=success default; the
// cancel URL passes ?status=cancel). Stripe's Checkout-mode
// success_url + cancel_url both point here so the SPA renders the
// right copy without us needing two routes.
//
// Customers have no Velox sign-in (the self-serve portal was removed,
// ADR-051), so this page is the end of the flow: confirm, encourage
// closing the tab.
//
// We deliberately don't try to poll for the new payment_method row.
// The setup_intent.succeeded webhook is async; by the time the
// customer reads this page Stripe may not have called us yet. Better
// UX to give the confirmation immediately than to spin a loader on
// data we can't guarantee.
export default function PaymentMethodAddedPage() {
  usePageTitle('Payment method added')
  const [searchParams] = useSearchParams()
  const canceled = searchParams.get('status') === 'cancel'

  return (
    <div className="min-h-screen bg-background flex flex-col">
      <header className="border-b border-border bg-card">
        <div className="max-w-2xl mx-auto px-4 py-4">
          <VeloxLogo />
        </div>
      </header>
      <main className="flex-1 flex items-center justify-center px-4 py-12">
        <Card className="w-full max-w-md">
          <CardContent className="p-8 text-center space-y-4">
            {canceled ? (
              <>
                <AlertCircle className="mx-auto h-12 w-12 text-muted-foreground" />
                <h1 className="text-xl font-semibold text-foreground">Setup canceled</h1>
                <p className="text-sm text-muted-foreground">
                  Your payment method was not added. Payment-update links can
                  only be used once, so this link is no longer active — please
                  contact support or your billing contact to request a new one.
                </p>
              </>
            ) : (
              <>
                <CheckCircle2 className="mx-auto h-12 w-12 text-emerald-500" />
                <h1 className="text-xl font-semibold text-foreground">Payment method added</h1>
                <p className="text-sm text-muted-foreground">
                  Your card is on file. You can close this tab — we'll use it for upcoming invoices automatically.
                </p>
                <div className="flex items-start gap-2 rounded-md bg-muted/50 px-3 py-2 text-left mt-6">
                  <ShieldCheck size={16} className="text-muted-foreground shrink-0 mt-0.5" />
                  <p className="text-xs text-muted-foreground">
                    Your card details went directly to our payment processor (Stripe). We never see, store, or transmit your card number.
                  </p>
                </div>
              </>
            )}
          </CardContent>
        </Card>
      </main>
    </div>
  )
}
