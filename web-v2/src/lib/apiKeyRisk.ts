// Whether revoking a key needs the typed word. Friction follows risk: a live
// key runs production integrations, so revoking it breaks them at once. That
// holds for a usage-only key too: revoking it stops the usage LiteLLM or
// another gateway sends, and that usage is what gets billed. A test key gets
// the plain confirm.
export function revokeNeedsTypedWord(key: { livemode: boolean }): boolean {
  return key.livemode
}
