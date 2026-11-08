import { StrictMode, lazy, Suspense, Component } from 'react'
import type { ReactNode, ErrorInfo } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, createRoutesFromElements, RouterProvider, Route, Navigate, Outlet, useLocation, useRouteError } from 'react-router-dom'
import { returnPath } from '@/lib/returnPath'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { AuthProvider, useAuth } from '@/contexts/AuthContext'
import { ModeAwareQueryProvider } from '@/contexts/QueryProvider'
import { TenantTimezoneBootstrap } from '@/lib/TenantTimezoneBootstrap'
import '@fontsource-variable/geist/index.css'
import '@fontsource-variable/geist-mono/index.css'
import './index.css'

// Error Boundary
class ErrorBoundary extends Component<
  { children: ReactNode },
  { hasError: boolean; error?: Error }
> {
  state = { hasError: false, error: undefined as Error | undefined }

  static getDerivedStateFromError(error: Error) {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ErrorBoundary:', error, info)
  }

  render() {
    if (this.state.hasError) {
      return <ErrorFallback message={this.state.error?.message} />
    }
    return this.props.children
  }
}

function ErrorFallback({ message }: { message?: string }) {
  // The raw error ("Cannot read properties of undefined") means nothing
  // to an operator, so it sits in Details for support. A failed page
  // download after an upgrade lands here too, and a reload fixes it.
  const raw = message
  return (
    <div className="min-h-screen flex items-center justify-center p-8 bg-background">
      <div className="text-center max-w-md">
        <h1 className="text-lg font-semibold mb-2 text-foreground">This page hit an error</h1>
        <p className="text-sm text-muted-foreground mb-4">
          Reload to try again. If it keeps happening, send the details below to support.
        </p>
        <div className="flex items-center justify-center gap-2">
          <button
            onClick={() => window.location.reload()}
            className="px-4 py-2 bg-primary text-primary-foreground rounded-lg text-sm"
          >
            Reload page
          </button>
          <button
            onClick={() => { window.location.href = '/' }}
            className="px-4 py-2 border border-border text-foreground rounded-lg text-sm"
          >
            Go to Dashboard
          </button>
        </div>
        {raw && (
          <details className="mt-4 text-left text-xs text-muted-foreground">
            <summary className="cursor-pointer text-center hover:text-foreground">Details</summary>
            <pre className="mt-2 whitespace-pre-wrap break-words rounded bg-muted p-2 font-mono text-[11px]">{raw}</pre>
          </details>
        )}
      </div>
    </div>
  )
}

// The router catches a page's render error before ErrorBoundary can, so
// its error element shows the same screen.
function RouteError() {
  const error = useRouteError()
  return <ErrorFallback message={error instanceof Error ? error.message : undefined} />
}

function ProtectedRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  const location = useLocation()
  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
    )
  }
  if (!user) {
    // Remember where they were going so login can return them there (a
    // shared invoice link should open the invoice, not the home page).
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  }
  return <>{children}</>
}

// Mirror of ProtectedRoute for pages that only make sense when logged out
// (login, forgot-password). A signed-in user landing on /login is sent to the
// dashboard instead of being shown a sign-in form for an account they're
// already in. Token-driven flows (reset-password, accept-invite) deliberately
// skip this — those tokens are the proof of intent and may apply to a
// different account than the active session.
function PublicOnlyRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  const location = useLocation()
  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
    )
  }
  if (user) {
    return <Navigate to={returnPath(location.state)} replace />
  }
  return <>{children}</>
}

// Lazy load pages. Layout too: the public pages (sign-in, the hosted invoice)
// don't use it, and it would add about 55 kB gzipped to the code every page
// loads first.
const Layout = lazy(() => import('@/components/Layout').then(m => ({ default: m.Layout })))
const LoginPage = lazy(() => import('@/pages/Login'))
const ForgotPasswordPage = lazy(() => import('@/pages/ForgotPassword'))
const ResetPasswordPage = lazy(() => import('@/pages/ResetPassword'))
const AcceptInvitePage = lazy(() => import('@/pages/AcceptInvite'))
const DashboardPage = lazy(() => import('@/pages/Dashboard'))
const CustomersPage = lazy(() => import('@/pages/Customers'))
const PricingPage = lazy(() => import('@/pages/Pricing'))
const DunningPage = lazy(() => import('@/pages/Dunning'))
const DunningPoliciesPage = lazy(() => import('@/pages/DunningPolicies'))
const WebhooksPage = lazy(() => import('@/pages/Webhooks'))
const WebhookEventsPage = lazy(() => import('@/pages/WebhookEvents'))
const WebhookEndpointDetailPage = lazy(() => import('@/pages/WebhookEndpointDetail'))
const ApiKeysPage = lazy(() => import('@/pages/ApiKeys'))
const TestClocksPage = lazy(() => import('@/pages/TestClocks'))
const TestClockDetailPage = lazy(() => import('@/pages/TestClockDetail'))
const SettingsPage = lazy(() => import('@/pages/Settings'))
const InvoicesPage = lazy(() => import('@/pages/Invoices'))
const SubscriptionsPage = lazy(() => import('@/pages/Subscriptions'))
const UsageEventsPage = lazy(() => import('@/pages/UsageEvents'))
const CreditsPage = lazy(() => import('@/pages/Credits'))
const ProviderCostsPage = lazy(() => import('@/pages/ProviderCosts'))
const CreditNotesPage = lazy(() => import('@/pages/CreditNotes'))
const AuditLogPage = lazy(() => import('@/pages/AuditLog'))
const UpdatePaymentPage = lazy(() => import('@/pages/UpdatePayment'))
const PaymentMethodAddedPage = lazy(() => import('@/pages/PaymentMethodAdded'))
const HostedInvoicePage = lazy(() => import('@/pages/HostedInvoice'))
const CustomerDetailPage = lazy(() => import('@/pages/CustomerDetail'))
const InvoiceDetailPage = lazy(() => import('@/pages/InvoiceDetail'))
const SubscriptionDetailPage = lazy(() => import('@/pages/SubscriptionDetail'))
const PlanDetailPage = lazy(() => import('@/pages/PlanDetail'))
const MeterDetailPage = lazy(() => import('@/pages/MeterDetail'))
const RecipesPage = lazy(() => import('@/pages/Recipes'))
const NotFoundPage = lazy(() => import('@/pages/NotFound'))

// Every page renders inside RouteShell: one Suspense for the lazy public
// pages and for Layout. A signed-in page loads inside Layout's own Suspense,
// so the sidebar stays on screen while the page's code downloads.
function RouteShell() {
  return (
    <Suspense
      fallback={
        <div className="min-h-screen flex items-center justify-center">
          <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
        </div>
      }
    >
      <Outlet />
    </Suspense>
  )
}

// A data router, so a page can ask before the operator leaves it with
// unsaved changes (useBlocker; components/UnsavedChangesGuard).
const router = createBrowserRouter(createRoutesFromElements(
  <Route element={<RouteShell />} errorElement={<RouteError />}>
    <Route path="/login" element={<PublicOnlyRoute><LoginPage /></PublicOnlyRoute>} />
    <Route path="/forgot-password" element={<PublicOnlyRoute><ForgotPasswordPage /></PublicOnlyRoute>} />
    <Route path="/reset-password" element={<PublicOnlyRoute><ResetPasswordPage /></PublicOnlyRoute>} />
    {/* Token-driven: no PublicOnlyRoute — see comment above. */}
    <Route path="/accept-invite" element={<AcceptInvitePage />} />
    {/* Every signed-in page. ProtectedRoute sends a signed-out visitor to
        sign in first, and Layout draws the sidebar around the page. Both
        stay mounted while the operator moves between these pages. */}
    <Route element={<ProtectedRoute><Layout /></ProtectedRoute>}>
      <Route path="/" element={<DashboardPage />} />
      <Route path="/recipes" element={<RecipesPage />} />
      <Route path="/customers" element={<CustomersPage />} />
      <Route path="/pricing" element={<PricingPage />} />
      <Route path="/dunning" element={<DunningPage />} />
      <Route path="/dunning-policies" element={<DunningPoliciesPage />} />
      <Route path="/webhooks" element={<WebhooksPage />} />
      <Route path="/webhooks/events" element={<WebhookEventsPage />} />
      <Route path="/webhooks/endpoints/:id" element={<WebhookEndpointDetailPage />} />
      <Route path="/api-keys" element={<ApiKeysPage />} />
      <Route path="/test-clocks" element={<TestClocksPage />} />
      <Route path="/test-clocks/:id" element={<TestClockDetailPage />} />
      <Route path="/invoices" element={<InvoicesPage />} />
      <Route path="/subscriptions" element={<SubscriptionsPage />} />
      <Route path="/usage" element={<UsageEventsPage />} />
      <Route path="/credits" element={<CreditsPage />} />
      <Route path="/provider-costs" element={<ProviderCostsPage />} />
      <Route path="/credit-notes" element={<CreditNotesPage />} />
      <Route path="/audit-log" element={<AuditLogPage />} />
      <Route path="/settings" element={<SettingsPage />} />
      <Route path="/customers/:id" element={<CustomerDetailPage />} />
      <Route path="/invoices/:id" element={<InvoiceDetailPage />} />
      <Route path="/subscriptions/:id" element={<SubscriptionDetailPage />} />
      <Route path="/plans/:id" element={<PlanDetailPage />} />
      <Route path="/meters/:id" element={<MeterDetailPage />} />
      {/* Any other address: a mistyped or old link. Signed-out
          visitors sign in first, then see this page. */}
      <Route path="*" element={<NotFoundPage />} />
    </Route>
    <Route path="/update-payment" element={<UpdatePaymentPage />} />
    {/* Stripe Checkout setup-session return URL.
        Public, unauthenticated — customers arrive here via
        the email link → Stripe Checkout → here, with no
        sign-in. Renders success/cancel copy based on
        ?status=success|cancel query. */}
    <Route path="/payment-method-added" element={<PaymentMethodAddedPage />} />
    <Route path="/invoice/:token" element={<HostedInvoicePage />} />
  </Route>,
))

const App = () => (
  <ErrorBoundary>
    <AuthProvider>
      <ModeAwareQueryProvider>
      <TenantTimezoneBootstrap />
      <TooltipProvider>
        <RouterProvider router={router} />
      </TooltipProvider>
      </ModeAwareQueryProvider>
      {/* Outside ModeAwareQueryProvider on purpose. That provider is
          keyed on livemode to force a remount when the mode flips; a
          Toaster inside it is unmounted in the same commit that the
          mode-toggle handler fires its "Switched to live mode" toast,
          so the toast never renders. A remounted Toaster starts empty
          because its list is local component state seeded to [] and
          filled only by the subscription it registers on mount — it
          never reads sonner's module-level store back, so toasts
          published to the outgoing instance are simply lost.
          The bottom offset clears what is pinned to the bottom edge: the
          Settings Discard/Save bar (about 57px tall) and the onboarding
          pill (bottom 20px, about 34px tall), so a toast never covers the
          button the operator needs next. */}
      <Toaster
        position="bottom-right"
        richColors
        closeButton
        offset={{ bottom: 72, right: 24 }}
        mobileOffset={{ bottom: 72, right: 16, left: 16 }}
      />
    </AuthProvider>
  </ErrorBoundary>
)

// A focused number box steps its value on the mouse wheel, so scrolling a
// dialog past a typed quantity changed it, and the form saved the new one.
// Blurring the box first lets the page scroll and leaves the value as typed.
// Amount boxes are text boxes; this covers the whole-number boxes.
document.addEventListener('wheel', (e) => {
  const t = e.target
  if (t instanceof HTMLInputElement && t.type === 'number' && t === document.activeElement) t.blur()
}, { passive: true })

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
