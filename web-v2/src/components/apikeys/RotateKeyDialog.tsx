import { useState } from 'react'
import { Loader2 } from 'lucide-react'
import { api } from '@/lib/api'
import type { APIKey } from '@/lib/gen/schemas/aPIKey'
import { showApiError } from '@/lib/formErrors'
import { cn } from '@/lib/utils'
import { keyTypeNoun } from '@/components/apikeys/keyTypes'
import { PresetButtons } from '@/components/apikeys/PresetButtons'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'

// Seconds the old key keeps working after the rotation. 0 revokes it as the
// new key is made.
const GRACE_PRESETS = [
  { value: 0, label: 'Now' },
  { value: 60 * 60, label: 'In 1 hour' },
  { value: 24 * 60 * 60, label: 'In 24 hours' },
  { value: 7 * 24 * 60 * 60, label: 'In 7 days' },
]

/**
 * Asks when the old key stops working, rotates the key, and gives onRotated
 * the new full key and the old key as the server left it.
 */
export function RotateKeyDialog({ target, onClose, onRotated }: {
  target: APIKey
  onClose: () => void
  onRotated: (rawKey: string, oldKey: APIKey) => void
}) {
  const [grace, setGrace] = useState(24 * 60 * 60)
  const [submitting, setSubmitting] = useState(false)

  const rotate = async () => {
    setSubmitting(true)
    try {
      const res = await api.rotateApiKey(target.id, grace)
      onRotated(res.raw_key, res.old_key)
    } catch (err) {
      showApiError(err, 'Failed to rotate key')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !submitting) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Rotate API key</DialogTitle>
          <DialogDescription>
            Rotating "{target.name}" issues a new {keyTypeNoun(target.key_type)} with the same
            name{target.expires_at ? ' and expiration' : ''}. The old key stops working at the time
            you pick{target.expires_at ? ', or when it expires if that is sooner' : ''}. Anything
            still using it must switch to the new key by then.
          </DialogDescription>
        </DialogHeader>

        <div>
          <Label className="mb-2 block">Old key stops working</Label>
          <PresetButtons presets={GRACE_PRESETS} value={grace} onChange={setGrace} />
          {grace === 0 && (
            <p className={cn('text-sm mt-2', target.livemode ? 'text-destructive' : 'text-muted-foreground')}>
              The old key stops working as soon as you click Rotate key. Anything still using it
              fails until it has the new key.
            </p>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={submitting}>Cancel</Button>
          <Button onClick={rotate} disabled={submitting}>
            {submitting ? <><Loader2 size={14} className="animate-spin mr-2" /> Rotating…</> : 'Rotate key'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
