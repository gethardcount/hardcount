// RetryKeys holds the Idempotency-Key of a money action, one per target, such
// as Issue credit note per invoice. The page that opens the action's dialog
// keeps it, so the key outlives the dialog. A try after closing and reopening
// the dialog then sends the same key, and the server returns what the first
// try did instead of doing it again. The server keeps a key's answer for 24
// hours (docs/api/idempotency.md).
//
// settle drops the target's key once an answer says what became of the try:
// a success, or a failure that is not a result unknown. The next try then
// gets a new key and is a new action.
export interface RetryKeys {
  keyFor: (target: string) => string
  settle: (target: string) => void
}

export function newRetryKeys(): RetryKeys {
  const keys = new Map<string, string>()
  return {
    keyFor: (target) => {
      let key = keys.get(target)
      if (!key) {
        key = crypto.randomUUID()
        keys.set(target, key)
      }
      return key
    },
    settle: (target) => { keys.delete(target) },
  }
}
