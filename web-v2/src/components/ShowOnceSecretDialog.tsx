import type { ReactNode } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

/**
 * Shows a secret the server returns once (a new API key, a webhook signing
 * secret). It closes only from "I've copied it": Esc, a click outside and a
 * close button used to dismiss it, and the secret was gone for good (B-08).
 */
export function ShowOnceSecretDialog({ title, label, secret, children, onDone }: {
  title: string
  label: string
  secret: string
  children?: ReactNode
  onDone: () => void
}) {
  return (
    // Ignores every close request but the button's.
    <Dialog open onOpenChange={() => {}}>
      <DialogContent className="sm:max-w-md" showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>Copy it now. Hardcount shows it only this once.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="rounded-lg border border-border bg-muted/40 p-3">
            <p className="text-xs font-medium text-muted-foreground">{label}</p>
            <div className="mt-1.5 flex items-start gap-2">
              <code className="flex-1 break-all select-all rounded bg-background px-2 py-1 font-mono text-sm text-foreground">{secret}</code>
              <CopyButton text={secret} />
            </div>
          </div>
          {children}
        </div>
        <DialogFooter>
          <Button onClick={onDone}>I&apos;ve copied it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  return (
    <Button
      variant="outline"
      size="sm"
      className="shrink-0"
      onClick={() => {
        void navigator.clipboard.writeText(text)
        toast.success('Copied to clipboard')
      }}
    >
      {label}
    </Button>
  )
}
