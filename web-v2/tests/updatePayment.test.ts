// The update-payment page and the hosted invoice speak to the end customer:
// they name the company the customer pays, never a "billing provider", and
// never show a server message or the word "token".
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { sellerContact, UNNAMED_SELLER } from '../src/lib/sellerContact.ts'
import { checkoutErrorCopy, linkErrorCopy } from '../src/lib/updatePayment.ts'

test('the contact is the company, linked to its support page first', () => {
  assert.deepEqual(
    sellerContact({ company_name: 'Acme', company_email: 'billing@acme.test', support_url: 'https://acme.test/help' }),
    { name: 'Acme', href: 'https://acme.test/help' },
  )
})

test('without a support page the contact mails the company', () => {
  assert.deepEqual(
    sellerContact({ company_name: 'Acme', company_email: 'billing@acme.test' }),
    { name: 'Acme', href: 'mailto:billing@acme.test' },
  )
})

test('with nothing set the contact is plain text', () => {
  assert.deepEqual(sellerContact(undefined), { name: UNNAMED_SELLER })
  assert.deepEqual(sellerContact({ company_name: '  ', support_url: ' ' }), { name: UNNAMED_SELLER })
})

test('a link with no code asks the customer to open it from the email', () => {
  const c = linkErrorCopy('missing')
  assert.equal(`${c.title}. ${c.body}`, 'This link is incomplete. Open it again from your email.')
  assert.equal(c.askForNewLink, false)
})

test('an expired or used link asks for a new one; a server fault does not', () => {
  assert.equal(linkErrorCopy(401).askForNewLink, true)
  assert.equal(linkErrorCopy(404).askForNewLink, true)
  const fault = linkErrorCopy(500)
  assert.equal(fault.askForNewLink, false)
  assert.match(fault.body, /try again/)
})

test('no customer copy says token or billing provider', () => {
  const all = [
    ...(['missing', 401, 404, 500, 0] as const).flatMap(s => {
      const c = linkErrorCopy(s)
      return [c.title, c.body]
    }),
    checkoutErrorCopy('invalid_token', 'Acme'),
    checkoutErrorCopy('stripe_error', 'Acme'),
    checkoutErrorCopy(undefined, 'Acme'),
  ]
  for (const line of all) {
    assert.doesNotMatch(line, /token|billing provider|billing administrator/i, line)
  }
})

test('a failed card form names the company', () => {
  assert.match(checkoutErrorCopy('stripe_error', 'Acme'), /contact Acme\.$/)
  assert.match(checkoutErrorCopy('invalid_token', 'Acme'), /Ask Acme for a new one\.$/)
})
