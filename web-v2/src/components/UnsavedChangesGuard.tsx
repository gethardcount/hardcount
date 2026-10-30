import { useEffect } from 'react'
import { useBlocker } from 'react-router-dom'
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

// How many mounted guards hold unsaved changes. The Test/Live switch reads
// it: switching mode remounts every page without a navigation, so the
// router never sees it leave.
let dirtyGuards = 0

/** True while a page form on screen has unsaved changes. */
export function hasUnsavedChanges(): boolean {
  return dirtyGuards > 0
}

/**
 * Asks "Discard changes?" before the operator leaves a page form with
 * unsaved changes: a link or Back inside the dashboard opens this dialog, and
 * a reload or closed tab gets the browser's own prompt. For page forms only;
 * a dialog's edits don't ask.
 *
 * Changing the query (a Settings tab) stays on the page, so it never asks.
 * Going to sign-in always goes through: signing out, or a session that ended,
 * must not leave the operator on a form they can no longer save.
 */
export function UnsavedChangesGuard({ dirty }: { dirty: boolean }) {
  const blocker = useBlocker(({ currentLocation, nextLocation }) =>
    dirty
    && nextLocation.pathname !== currentLocation.pathname
    && nextLocation.pathname !== '/login')

  useEffect(() => {
    if (!dirty) return
    dirtyGuards++
    const onBeforeUnload = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = '' }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => {
      dirtyGuards--
      window.removeEventListener('beforeunload', onBeforeUnload)
    }
  }, [dirty])

  return (
    <DiscardChangesDialog
      open={blocker.state === 'blocked'}
      onKeep={() => blocker.reset?.()}
      onDiscard={() => blocker.proceed?.()}
    />
  )
}

/** The question both the guard and the Test/Live switch ask. */
export function DiscardChangesDialog({ open, onKeep, onDiscard, why = 'If you leave now, they are lost.' }: {
  open: boolean
  onKeep: () => void
  onDiscard: () => void
  why?: string
}) {
  return (
    <AlertDialog open={open} onOpenChange={o => { if (!o) onKeep() }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Discard changes?</AlertDialogTitle>
          <AlertDialogDescription>
            You have unsaved changes on this page. {why}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Keep editing</AlertDialogCancel>
          <AlertDialogAction onClick={onDiscard}>Discard changes</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
