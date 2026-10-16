// The Upcoming invoice card tells "nothing billed yet", "nothing to preview"
// and "couldn't load" apart (M-SUB-2, M-SUB-8).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { previewView } from '../src/lib/invoicePreview.ts'

const base = {
  billing_period_start: '2026-10-13T00:00:00Z',
  billing_period_end: '2026-11-01T00:00:00Z',
  generated_at: '2026-10-16T10:00:00Z',
  warnings: [],
}

test('a preview with lines shows its total', () => {
  const v = previewView({ ...base, lines: [{ amount_cents: 7355, currency: 'USD' }], totals: [{ currency: 'USD', amount_cents: 7355 }] }, null, 'USD')
  assert.deepEqual(v, { kind: 'lines', total: 7355, currency: 'USD', multiCurrency: false })
})

test('a preview with no lines is $0.00, not an empty table', () => {
  assert.deepEqual(previewView({ ...base, lines: [], totals: [] }, null, 'EUR'), { kind: 'empty', total: 0, currency: 'EUR' })
})

test('a server refusal says why; a failure offers retry', () => {
  assert.deepEqual(previewView(undefined, { status: 422, message: 'subscription has no billing period set' }, 'USD'), {
    kind: 'none', message: 'subscription has no billing period set',
  })
  assert.deepEqual(previewView(undefined, { status: 500, message: 'boom' }, 'USD'), { kind: 'error' })
  assert.deepEqual(previewView(undefined, new TypeError('Failed to fetch'), 'USD'), { kind: 'error' })
  assert.deepEqual(previewView(undefined, { status: 429, message: 'slow down' }, 'USD'), { kind: 'error' })
})

test('still loading is its own state', () => {
  assert.deepEqual(previewView(undefined, null, 'USD'), { kind: 'loading' })
})
