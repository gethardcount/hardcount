// RetryKeys holds the Idempotency-Key of a money action, one per target, such
// as Issue credit note per invoice. The page that opens the action's dialog
// keeps it, so the key outlives the dialog. A try after closing and reopening
// the dialog then sends the same key, and the server returns what the first
// try did instead of doing it again. The server keeps a key's answer for 24
// hours (docs/api/idempotency.md).
//
// settle drops the target's key once an answer says what became of the tries
// under it. The next try then gets a new key and is a new action.
//
// markUnknown records that a try under the key may have taken effect, and
// unknown reads it back. A later answer that says nothing about that try,
// such as a 429, then still reads as "Result unknown".
export interface RetryKeys {
  keyFor: (target: string) => string
  settle: (target: string) => void
  markUnknown: (target: string) => void
  unknown: (target: string) => boolean
}

export function newRetryKeys(): RetryKeys {
  const keys = new Map<string, { key: string; unknown: boolean }>()
  return {
    keyFor: (target) => {
      let entry = keys.get(target)
      if (!entry) {
        entry = { key: crypto.randomUUID(), unknown: false }
        keys.set(target, entry)
      }
      return entry.key
    },
    settle: (target) => { keys.delete(target) },
    // No key, no mark: another try settled the key while this one ran, and
    // a mark would not reach the key the next try sends.
    markUnknown: (target) => {
      const entry = keys.get(target)
      if (entry) entry.unknown = true
    },
    unknown: (target) => keys.get(target)?.unknown ?? false,
  }
}
