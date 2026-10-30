// Whether revoking a key needs the typed word. Friction follows risk: a live
// key runs production integrations, so revoking it breaks them at once. That
// holds for a usage-only key too: revoking it stops the usage LiteLLM or
// another gateway sends, and that usage is what gets billed. A test key gets
// the plain confirm. A prefix with no mode is a legacy key, which the server
// treats as live (internal/auth/service.go).
export function revokeNeedsTypedWord(key: { key_prefix: string }): boolean {
  return !key.key_prefix.includes('_test_')
}
