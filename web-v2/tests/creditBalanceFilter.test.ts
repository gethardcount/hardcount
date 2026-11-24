// The Credit page's ?balance= filter, which Home's credit lines open.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { creditBalanceFilter, creditBalanceFilterLabels } from '../src/lib/creditBalanceFilter.ts'

test('the URL names out or low; anything else is no filter', () => {
  assert.equal(creditBalanceFilter('out'), 'out')
  assert.equal(creditBalanceFilter('low'), 'low')
  assert.equal(creditBalanceFilter(''), undefined)
  assert.equal(creditBalanceFilter('OUT'), undefined)
})

test('the chip and the empty table say which customers the filter shows', () => {
  assert.deepEqual(creditBalanceFilterLabels('out'), { chip: 'Out of credit', empty: 'No customer is out of credit.' })
  assert.deepEqual(creditBalanceFilterLabels('low'), { chip: 'Low on credit', empty: 'No customer is low on credit.' })
})
