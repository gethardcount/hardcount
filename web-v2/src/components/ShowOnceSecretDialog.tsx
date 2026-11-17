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
 * Shows a secret the server returns only once. It closes only from "I've
 * copied it", so Esc or a click outside cannot lose the secret.
 */
export function ShowOnceSecretDialog({ title, label, secret, children, onDone }: {
  title: string
  label: string
  secret: string
  children?: ReactNode
  onDone: () => void
}) {
  return (
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
              <CopyButton text={secret} name={`Copy ${label.toLowerCase()}`} />
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

/**
 * Copies text and says whether it worked. name is its accessible name where a
 * dialog has more than one; it starts with the visible "Copy".
 */
export function CopyButton({ text, name }: { text: string; name?: string }) {
  return (
    <Button
      variant="outline"
      size="sm"
      aria-label={name}
      className="shrink-0"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          toast.success('Copied to clipboard')
        } catch {
          toast.error("Couldn't copy. Select the text and copy it yourself.")
        }
      }}
    >
      Copy
    </Button>
  )
}
