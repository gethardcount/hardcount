// Pins the lint rule in eslint.config.js that keeps screens off an invoice's
// raw payment_status and amount_due_cents. A rule that misses a way of reading
// the field lets the next screen show a voided invoice as owed, and nothing
// fails. Each snippet is linted as if it lived in src/pages.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { fileURLToPath } from 'node:url'
import { ESLint } from 'eslint'

const cwd = fileURLToPath(new URL('..', import.meta.url))
const eslint = new ESLint({ cwd })

async function rawReads(code: string, filePath = 'src/pages/Example.tsx'): Promise<number> {
  const [result] = await eslint.lintText(code, { filePath })
  return result.messages.filter(m => m.ruleId === 'no-restricted-syntax' && /payment_status or amount_due_cents/.test(m.message)).length
}

test('every way of reading the raw fields fails outside src/lib', { timeout: 30_000 }, async () => {
  const reads = {
    member: 'export const f = (inv: { amount_due_cents: number }) => inv.amount_due_cents\n',
    optional: 'export const f = (inv?: { payment_status: string }) => inv?.payment_status\n',
    computed: "export const f = (inv: { amount_due_cents: number }) => inv['amount_due_cents']\n",
    destructured: 'export const f = (inv: { payment_status: string }) => { const { payment_status } = inv; return payment_status }\n',
    renamed: 'export const f = (inv: { amount_due_cents: number }) => { const { amount_due_cents: due } = inv; return due }\n',
    param: 'export const f = ({ payment_status }: { payment_status: string }) => payment_status\n',
  }
  const missed = []
  for (const [how, code] of Object.entries(reads)) {
    if (await rawReads(code) !== 1) missed.push(how)
  }
  assert.deepEqual(missed, [], 'reads the rule did not catch')
})

test('negative controls: writing the field, other fields, and src/lib pass', { timeout: 30_000 }, async () => {
  // An object literal sends the field (a request body, a test fixture); it reads nothing.
  assert.equal(await rawReads('export const body = { amount_due_cents: 0, payment_status: "pending" }\n'), 0)
  assert.equal(await rawReads('export const f = (inv: { amount_paid_cents: number }) => inv.amount_paid_cents\n'), 0)
  assert.equal(await rawReads('export const f = (inv: { amount_due_cents: number }) => inv.amount_due_cents\n', 'src/lib/example.ts'), 0)
})
