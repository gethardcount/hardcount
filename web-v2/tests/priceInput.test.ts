// Guards the dashboard's price-input parser.
//
// The server accepts a rate of 0 as a deliberate free rate, so it cannot tell
// a blank price box from an operator who meant "free". dollarsToRateCents used
// to turn blank or unreadable input into "0", and the rule saved as free. It
// must return null for those, so the form can refuse.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { dollarsToRateCents, dollarsToWholeCents, wholeUnits } from '../src/lib/priceInput.ts'

test('a blank price is not a price — it must never become a free rate', () => {
  assert.equal(dollarsToRateCents(''), null)
  assert.equal(dollarsToRateCents('   '), null)
})

test('input that is not a plain decimal number is refused, not read as 0', () => {
  // A number input reports "1e-7" for scientific notation; reading that as 0
  // would save a free rate in place of a tiny one.
  assert.equal(dollarsToRateCents('1e-7'), null)
  assert.equal(dollarsToRateCents('abc'), null)
  assert.equal(dollarsToRateCents('1,000'), null)
  // Sign or point with no digit at all.
  assert.equal(dollarsToRateCents('.'), null)
  assert.equal(dollarsToRateCents('-'), null)
})

test('an explicit zero is still a free rate', () => {
  assert.equal(dollarsToRateCents('0'), '0')
  assert.equal(dollarsToRateCents('0.00'), '0')
})

test('dollars become cents by moving the decimal point, with no float rounding', () => {
  assert.equal(dollarsToRateCents('0.01'), '1')
  assert.equal(dollarsToRateCents(' 12.5 '), '1250')
  // 0.000003 * 100 in floating point is 0.00030000000000000003.
  assert.equal(dollarsToRateCents('0.000003'), '0.0003')
  // A negative price keeps its sign so the server can reject it.
  assert.equal(dollarsToRateCents('-5'), '-500')
})

// The New invoice form typed cents under a "Unit ($)" header, so a price
// could be off by 100×. It now takes currency units, through this parser.
test('an amount in currency units becomes whole cents, exactly', () => {
  assert.equal(dollarsToWholeCents('25'), 2500)
  assert.equal(dollarsToWholeCents('25.5'), 2550)
  assert.equal(dollarsToWholeCents('25.50'), 2550)
  assert.equal(dollarsToWholeCents('0.29'), 29)
  assert.equal(dollarsToWholeCents('.5'), 50)
  assert.equal(dollarsToWholeCents(' 7 '), 700)
  assert.equal(dollarsToWholeCents('0'), 0)
})

test('an amount the form cannot read for sure is refused, never guessed', () => {
  for (const bad of ['', ' ', '.', '9,50', '1,000', '-5', '2.505', '1e3', 'abc', '$5', '99999999999999']) {
    assert.equal(dollarsToWholeCents(bad), null, `"${bad}" must be refused`)
  }
})

// A tier limit or package size the form cannot read is refused. parseInt
// turned a blank package size into 1 and "1000.5" into 1000, and saved them.
test('a unit count is a whole number above 0, or refused', () => {
  assert.equal(wholeUnits('1000'), 1000)
  assert.equal(wholeUnits(' 100 '), 100)
  for (const bad of ['', '0', '-1', '1000.5', '1e3', '1,000', 'abc']) {
    assert.equal(wholeUnits(bad), null, `"${bad}" must be refused`)
  }
})

