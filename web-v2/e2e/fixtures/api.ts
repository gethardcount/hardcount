import { env } from './env'

// A thin client for the Hardcount API, authenticated with the bootstrap test key.
// Any status outside 2xx throws with the response body, so a seeding problem
// stops the run at the call that caused it.
export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (!env.apiKey) throw new Error('E2E_API_KEY is not set (use the hc_secret_test_ key from hardcount-bootstrap)')
  const res = await fetch(`${env.apiURL}/v1${path}`, {
    method,
    headers: { Authorization: `Bearer ${env.apiKey}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${method} /v1${path} returned ${res.status}: ${text}`)
  return (text ? JSON.parse(text) : undefined) as T
}

// apiListAll reads every page of a list route (limit 100, the API's largest
// page) and returns all its rows.
export async function apiListAll<T>(path: string): Promise<T[]> {
  const out: T[] = []
  const sep = path.includes('?') ? '&' : '?'
  for (let offset = 0; ; offset += 100) {
    const page = await api<{ data: T[]; total: number }>('GET', `${path}${sep}limit=100&offset=${offset}`)
    out.push(...page.data)
    if (page.data.length < 100 || out.length >= page.total) return out
  }
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

// usdBalance is the USD row of GET /v1/credits/balance/{id}, which returns
// one balance per currency (ADR-129); a customer with no USD credit reads 0.
export function usdBalance(res: { data: { currency: string; balance_cents: number }[] }): { balance_cents: number } {
  return res.data.find(b => b.currency === 'USD') ?? { balance_cents: 0 }
}
