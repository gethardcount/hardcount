import { Activity, Shield, type LucideIcon } from 'lucide-react'
import type { APIKeyKeyType } from '@/lib/gen/schemas/aPIKeyKeyType'

/** The key types in the dashboard's words, in the order the create dialog offers them. */
export const KEY_TYPES: readonly { value: APIKeyKeyType; label: string; noun: string; hint: string; icon: LucideIcon }[] = [
  { value: 'secret', label: 'Secret', noun: 'secret key', hint: 'Full access. Keep it on your server.', icon: Shield },
  { value: 'ingest', label: 'Usage only', noun: 'usage-only key', hint: 'Can only send usage events. Use it in LiteLLM or another gateway.', icon: Activity },
]

/** The type as the list names it. A type this table lacks shows as the server sent it. */
export function keyTypeLabel(type: string): string {
  return KEY_TYPES.find(t => t.value === type)?.label ?? type
}

/** The type as a noun for running text: "a new usage-only key". */
export function keyTypeNoun(type: string): string {
  return KEY_TYPES.find(t => t.value === type)?.noun ?? `${type} key`
}
