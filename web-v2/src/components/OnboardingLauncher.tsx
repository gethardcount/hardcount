import { useEffect, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { toast } from 'sonner'
import { Check, X, ChevronUp, ArrowRight } from 'lucide-react'
import { cn } from '@/lib/utils'
import type { SetupStep } from '@/lib/setupSteps'
import {
  useOnboardingSteps, openSetupChecklist, onOpenSetupChecklist,
} from '@/hooks/useOnboardingSteps'
import { useAuth } from '@/contexts/AuthContext'
import { Card } from '@/components/ui/card'
import { ScrollPane } from '@/components/ui/scroll-pane'

// OnboardingLauncher — the "Setup n/N" row in the sidebar footer. It opens
// the checklist panel (SetupChecklistPanel), which Layout renders once,
// outside the sidebar: the sidebar is drawn twice (desktop and phone menu),
// and on a phone the closed menu is moved off screen, which took a panel
// inside it along. Self-hides when the user completes the checklist or
// dismisses it; state lives in the shared useOnboardingSteps hook.
export function OnboardingLauncher({ onOpen }: { onOpen?: () => void }) {
  const { complete, total, show } = useOnboardingSteps()
  if (!show) return null
  return (
    <button
      type="button"
      onClick={() => {
        onOpen?.()
        openSetupChecklist()
      }}
      aria-label={`Setup: ${complete} of ${total} complete. Open checklist.`}
      className="flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-sidebar-accent/50 hover:text-foreground"
    >
      <ProgressRing value={complete} max={total} />
      <span className="flex-1 text-left">Setup</span>
      <span className="text-xs tabular-nums">{complete}/{total}</span>
    </button>
  )
}

// The list's title, said in the operator's words: what the list is for.
const CHECKLIST_TITLE = 'Set up, in order'

// SetupChecklistPanel — the checklist opened by the sidebar row, beside the
// page. Non-modal: it doesn't block the page.
export function SetupChecklistPanel() {
  const [open, setOpen] = useState(false)
  const { steps, complete, total, show, setDismissed } = useOnboardingSteps()

  useEffect(() => onOpenSetupChecklist(() => setOpen(true)), [])

  // Close on Escape while the panel is open. No focus-trap because the panel
  // is non-modal — it doesn't block page interaction.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])

  if (!show || !open) return null

  return (
    <div
      role="dialog"
      aria-labelledby="onboarding-launcher-title"
      // Sits beside the sidebar (above the mobile menu's overlay).
      className="fixed bottom-4 left-4 z-50 w-[360px] max-w-[calc(100vw-2rem)] rounded-xl border border-border bg-card shadow-2xl md:left-64"
    >
      <ChecklistHeader titleId="onboarding-launcher-title" complete={complete} total={total}>
        <button
          type="button"
          onClick={() => setOpen(false)}
          aria-label="Collapse checklist"
          className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <ChevronUp size={14} className="rotate-180" aria-hidden />
        </button>
        <HideButton onHide={() => { setDismissed(true); setOpen(false) }} />
      </ChecklistHeader>
      <ScrollPane as="ul" className="max-h-[60vh] border-t border-border">
        <StepRows steps={steps} onNavigate={() => setOpen(false)} />
      </ScrollPane>
      <WebhookLine onNavigate={() => setOpen(false)} />
    </div>
  )
}

// SetupChecklistCard — the same list, open in the Home page while setup is
// left and the operator has not hidden it. intro goes above the list (Home's
// welcome on a new account).
export function SetupChecklistCard({ intro, className }: { intro?: ReactNode; className?: string }) {
  const { steps, complete, total, setDismissed } = useOnboardingSteps()
  return (
    <Card role="region" aria-labelledby="setup-card-title" className={cn('gap-0 py-0', className)}>
      {intro && <div className="px-4 pt-4">{intro}</div>}
      <ChecklistHeader titleId="setup-card-title" complete={complete} total={total}>
        <HideButton onHide={() => setDismissed(true)} />
      </ChecklistHeader>
      <ul className="border-t border-border">
        <StepRows steps={steps} />
      </ul>
      <WebhookLine />
    </Card>
  )
}

function ChecklistHeader({ titleId, complete, total, children }: {
  titleId: string; complete: number; total: number; children: ReactNode
}) {
  const { user } = useAuth()
  const percent = total > 0 ? (complete / total) * 100 : 0
  return (
    <>
      <div className="flex items-start gap-3 px-4 pt-4 pb-2">
        <div className="flex-1 min-w-0">
          <p id={titleId} className="text-sm font-semibold text-foreground">
            {CHECKLIST_TITLE}
          </p>
          {user && (
            <p className="mt-0.5 text-xs text-muted-foreground">
              {user.livemode
                ? "You're in live mode, so charges here are real."
                : "You're in test mode, so nothing here charges real money."}
            </p>
          )}
          <p className="mt-0.5 text-xs text-muted-foreground">
            <span className="tabular-nums">{complete} of {total}</span> complete
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">{children}</div>
      </div>

      <div className="px-4 pb-3">
        <div
          className="h-1 overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-valuenow={complete}
          aria-valuemin={0}
          aria-valuemax={total}
          aria-label="Setup progress"
        >
          <div
            className="h-full rounded-full bg-primary transition-[width] duration-300"
            style={{ width: `${percent}%` }}
          />
        </div>
      </div>
    </>
  )
}

function HideButton({ onHide }: { onHide: () => void }) {
  return (
    <button
      type="button"
      onClick={() => {
        onHide()
        // The list can come back, so say where from.
        toast('Setup checklist hidden', { description: 'Show it again from the account menu, under your email.' })
      }}
      aria-label="Hide setup checklist"
      className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
    >
      <X size={14} aria-hidden />
    </button>
  )
}

// The steps in order; the first one not done is marked as next.
function StepRows({ steps, onNavigate }: { steps: SetupStep[]; onNavigate?: () => void }) {
  const next = steps.find(s => !s.done)
  return steps.map(step => (
    <StepRow key={step.key} step={step} isNext={step === next} onNavigate={onNavigate} />
  ))
}

// Webhooks left the list: an operator can bill without one. The low-credit
// event is the reason this operator wants one.
function WebhookLine({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">
      To be told when a customer&apos;s credit runs low,{' '}
      <Link to="/webhooks" onClick={onNavigate} className="font-medium text-primary hover:underline">
        add a webhook endpoint
      </Link>.
    </p>
  )
}

function StepRow({
  step, isNext, onNavigate,
}: {
  step: SetupStep; isNext: boolean; onNavigate?: () => void
}) {
  const indicator = step.done ? (
    <span
      aria-label="done"
      className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
    >
      <Check size={12} aria-hidden />
    </span>
  ) : (
    <span
      aria-hidden
      className={cn(
        'flex h-5 w-5 shrink-0 items-center justify-center rounded-full border',
        isNext ? 'border-primary bg-primary/10' : 'border-border',
      )}
    >
      {isNext && <span className="h-1.5 w-1.5 rounded-full bg-primary" />}
    </span>
  )

  // One action: the whole row is its link. More: each is its own link, and
  // the row is not one.
  const only = step.actions.length === 1 ? step.actions[0] : undefined

  const body = (
    <div
      className={cn(
        'flex items-center gap-3 border-b border-border px-4 py-3 transition-colors last:border-b-0',
        !step.done && only && 'hover:bg-accent/40',
        isNext && 'bg-primary/[0.04]',
      )}
    >
      {indicator}
      <div className="min-w-0 flex-1">
        <p className={cn(
          'text-sm font-medium',
          step.done ? 'text-muted-foreground line-through decoration-muted-foreground/40' : 'text-foreground',
        )}>
          {step.label}
          {step.optional && <span className="ml-1.5 text-xs font-normal text-muted-foreground">Optional</span>}
        </p>
        {/* Every step says what it is for, not only the next one. */}
        <p className="mt-0.5 text-xs text-muted-foreground">{step.desc}</p>
        {!step.done && !only && (
          <p className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-xs font-medium">
            {step.actions.map(a => (
              <Link key={a.to} to={a.to} onClick={onNavigate} className="text-primary hover:underline">
                {a.label}
              </Link>
            ))}
          </p>
        )}
      </div>
      {isNext && only && (
        <span className="flex shrink-0 items-center gap-1 text-xs font-medium text-primary">
          {only.label}
          <ArrowRight size={12} aria-hidden />
        </span>
      )}
    </div>
  )

  return !step.done && only ? (
    <li>
      <Link to={only.to} onClick={onNavigate}>{body}</Link>
    </li>
  ) : (
    <li>{body}</li>
  )
}

function ProgressRing({
  value, max, size = 18, stroke = 2,
}: {
  value: number; max: number; size?: number; stroke?: number
}) {
  const radius = (size - stroke) / 2
  const circumference = 2 * Math.PI * radius
  const progress = max > 0 ? value / max : 0
  const offset = circumference * (1 - progress)
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="shrink-0" aria-hidden>
      <circle
        cx={size / 2}
        cy={size / 2}
        r={radius}
        fill="none"
        strokeWidth={stroke}
        className="stroke-muted"
      />
      <circle
        cx={size / 2}
        cy={size / 2}
        r={radius}
        fill="none"
        strokeWidth={stroke}
        strokeLinecap="round"
        strokeDasharray={circumference}
        strokeDashoffset={offset}
        className="stroke-primary transition-[stroke-dashoffset] duration-300"
        transform={`rotate(-90 ${size / 2} ${size / 2})`}
      />
    </svg>
  )
}
