import { env } from './env'

// A thin client for the Velox API, authenticated with the bootstrap test key.
// Any status outside 2xx throws with the response body, so a seeding problem
// stops the run at the call that caused it.
export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (!env.apiKey) throw new Error('E2E_API_KEY is not set (use the vlx_secret_test_ key from velox-bootstrap)')
  const res = await fetch(`${env.apiURL}/v1${path}`, {
    method,
    headers: { Authorization: `Bearer ${env.apiKey}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${method} /v1${path} returned ${res.status}: ${text}`)
  return (text ? JSON.parse(text) : undefined) as T
}

// The money format the dashboard uses for USD: "$1,234.50".
export function usd(cents: number): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents)
  return `${sign}$${Math.floor(abs / 100).toLocaleString('en-US')}.${String(abs % 100).padStart(2, '0')}`
}

// A short suffix that keeps names and codes unique across runs against the
// same database.
export function uniq(): string {
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`
}
