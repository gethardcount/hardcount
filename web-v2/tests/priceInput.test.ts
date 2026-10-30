// Guards the dashboard's price-input parser.
//
// The server accepts a rate of 0 as a deliberate free rate, so it cannot tell
// a blank price box from an operator who meant "free". dollarsToRateCents used
// to turn blank or unreadable input into "0", and the rule saved as free. It
// must return null for those, so the form can refuse.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { amountInputError, dollarsToRateCents, dollarsToWholeCents, percentRate, wholeNumberIn, wholeUnits } from '../src/lib/priceInput.ts'

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

// Trial days take 0 (no trial) to 730. parseInt cut "2.7" to 2 and sent "-5"
// as -5; a blank box is the caller's choice, never read as 0 here.
test('a whole number in a range takes 0 where the range does, and refuses the rest', () => {
  assert.equal(wholeNumberIn('0', 0, 730), 0)
  assert.equal(wholeNumberIn(' 14 ', 0, 730), 14)
  assert.equal(wholeNumberIn('730', 0, 730), 730)
  for (const bad of ['', ' ', '731', '-5', '2.7', '1e2', '1,000', 'abc']) {
    assert.equal(wholeNumberIn(bad, 0, 730), null, `"${bad}" must be refused`)
  }
  assert.equal(wholeNumberIn('0', 1, 10), null)
})

// The tax rate box read a blank as 0 (tax off) and "8,5" as 8. The server
// stores four decimals, so a fifth would be rounded away without a word.
test('a percent is 0 to 100 with at most four decimals, or refused', () => {
  assert.equal(percentRate('0'), 0)
  assert.equal(percentRate('8.875'), 8.875)
  assert.equal(percentRate('8.8750'), 8.875)
  assert.equal(percentRate('.5'), 0.5)
  assert.equal(percentRate('100'), 100)
  for (const bad of ['', ' ', '.', '8,5', '8.87501', '100.0001', '101', '-1', '1e1', 'abc']) {
    assert.equal(percentRate(bad), null, `"${bad}" must be refused`)
  }
})

// The line under a refused box says what to change, not only that it is wrong.
test('a refused amount says why', () => {
  assert.equal(amountInputError('', '25.00', true), 'Enter an amount, for example 25.00.')
  assert.equal(amountInputError('9,50', '9.50', true), 'Use a point for decimals, not a comma: for example 9.50.')
  assert.equal(amountInputError('-5', '49.00', true), "Enter 0 or more. An amount can't be negative.")
  assert.equal(amountInputError('2.505', '25.00', true), 'Enter at most two decimals, for example 25.00.')
  assert.equal(amountInputError('99999999999999', '25.00', true), 'That amount is too large.')
  assert.equal(amountInputError('abc', '25.00', true), 'Enter a plain number, for example 25.00.')
  // A rate takes sub-cent decimals, so many decimals are not the reason.
  assert.equal(amountInputError('1e-7', '0.01', false), 'Enter a plain number, for example 0.01.')
})

