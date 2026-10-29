// Whether revoking a key needs the typed word. Friction follows risk: a live
// secret or platform key runs production integrations, so revoking it breaks
// them at once. A test key, or a publishable key (it carries no
// permissions), gets the plain confirm. A prefix with no mode is a legacy
// key, which the server treats as live (internal/auth/service.go).
export function revokeNeedsTypedWord(key: { key_prefix: string; key_type: string }): boolean {
  return !key.key_prefix.includes('_test_') && key.key_type !== 'publishable'
}
