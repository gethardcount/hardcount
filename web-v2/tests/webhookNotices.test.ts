// Replay toast, paused-endpoint notice and where an endpoint save error goes:
// each has two versions.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { endpointSaveErrorTarget, pausedEndpointNotice, replayToast, isAllEvents } from '../src/lib/webhookNotices.ts'

test('replay with no listening endpoint: warning, says nothing was sent', () => {
  const t = replayToast('no_matching_endpoints')
  assert.equal(t.level, 'warning')
  assert.equal(t.title, 'Event copied, not sent')
  assert.match(t.description, /No active endpoint listens/)
})

test('replay queued: success, no clone id in the text', () => {
  const t = replayToast('queued')
  assert.equal(t.level, 'success')
  assert.equal(t.title, 'Event resent')
  assert.doesNotMatch(t.title + t.description, /vlx_|evt_|not sent/)
})

test('paused endpoint gets the notice; an active one does not', () => {
  const n = pausedEndpointNotice(false)
  assert.ok(n)
  assert.equal(n.title, 'Endpoint paused')
  assert.match(n.body, /until you resume it/)
  assert.equal(pausedEndpointNotice(true), null)
})

test('paused notice waits for the endpoint to load', () => {
  assert.equal(pausedEndpointNotice(undefined), null)
})

test('a refused URL goes under the URL field; other errors are a toast', () => {
  const urlErr = Object.assign(new Error('must start with https://'), { status: 422, field: 'url' })
  const otherField = Object.assign(new Error('too long'), { status: 422, field: 'description' })
  assert.equal(endpointSaveErrorTarget(urlErr), 'url')
  assert.equal(endpointSaveErrorTarget(otherField), 'toast')
  assert.equal(endpointSaveErrorTarget(new TypeError('Failed to fetch')), 'toast')
  assert.equal(endpointSaveErrorTarget(null), 'toast')
})

test('replay: only the explicit no-match status warns; any other status reads as resent', () => {
  // webhook.Service.Replay returns 'queued' or 'no_matching_endpoints'; a
  // missing status (older server) must not claim nothing was sent.
  assert.equal(replayToast(undefined).level, 'success')
})

test('isAllEvents: empty, missing and the * wildcard all mean every event', () => {
  assert.equal(isAllEvents(undefined), true)
  assert.equal(isAllEvents([]), true)
  assert.equal(isAllEvents(['*']), true)
  assert.equal(isAllEvents(['invoice.paid']), false)
})
