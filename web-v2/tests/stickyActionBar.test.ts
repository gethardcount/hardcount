// The onboarding pill (fixed bottom-right) covered Settings' Save Changes
// button at 390px. It hides while a sticky action bar is shown; these pin the
// bookkeeping that decides "shown".
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { showStickyActionBar, stickyActionBarShown, subscribeStickyActionBar } from '../src/lib/stickyActionBar.ts'

test('shown while a bar is shown, not after it is removed', () => {
  assert.equal(stickyActionBarShown(), false)
  const remove = showStickyActionBar()
  assert.equal(stickyActionBarShown(), true)
  remove()
  assert.equal(stickyActionBarShown(), false)
})

test('a second bar keeps it shown until both are removed', () => {
  const a = showStickyActionBar()
  const b = showStickyActionBar()
  a()
  assert.equal(stickyActionBarShown(), true)
  b()
  assert.equal(stickyActionBarShown(), false)
})

test('removing the same bar twice does not hide another bar', () => {
  const a = showStickyActionBar()
  const b = showStickyActionBar()
  a()
  a()
  assert.equal(stickyActionBarShown(), true)
  b()
  assert.equal(stickyActionBarShown(), false)
})

test('subscribers hear each change', () => {
  let calls = 0
  const unsubscribe = subscribeStickyActionBar(() => { calls++ })
  const remove = showStickyActionBar()
  remove()
  unsubscribe()
  showStickyActionBar()()
  assert.equal(calls, 2)
})
