// Whether a page is showing a sticky action bar (a fixed bottom bar holding
// Save / Discard). The onboarding pill is also fixed bottom-right, and at
// phone width it covered Settings' Save Changes button; a reserved right
// edge left the bar too narrow. The pill hides while any bar is shown.
//
// A count, not a flag: two bars mounted at once, or one remounting before the
// other unmounts, must not leave the pill hidden or shown wrongly.
// Dependency-free so node --test can import it; the hooks live in
// hooks/useStickyActionBar.

let count = 0
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

// showStickyActionBar records one shown bar and returns the call that removes
// it. Calling the returned function twice removes it only once.
export function showStickyActionBar(): () => void {
  count++
  emit()
  let removed = false
  return () => {
    if (removed) return
    removed = true
    count--
    emit()
  }
}

export function stickyActionBarShown(): boolean {
  return count > 0
}

export function subscribeStickyActionBar(listener: () => void): () => void {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}
