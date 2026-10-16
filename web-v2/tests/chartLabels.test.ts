// Chart bucket keys name a UTC calendar day or month. They are labelled as
// that day, never converted into the tenant timezone (which moved every label
// one day back west of UTC). Locks the Dashboard revenue chart and the
// customer Activity "Daily usage" chart labels.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { revenueBucketLabel, utcBucketLabel } from '../src/lib/chartLabels.ts'

test('revenue day bucket "2026-10-15" reads Oct 15, not Oct 14', () => {
  assert.equal(revenueBucketLabel('2026-10-15'), 'Oct 15')
  assert.equal(revenueBucketLabel('2026-01-01'), 'Jan 1')
})

test('revenue month bucket "2026-10" reads Oct 2026, not Sep 30', () => {
  assert.equal(revenueBucketLabel('2026-10'), 'Oct 2026')
  assert.equal(revenueBucketLabel('2027-01'), 'Jan 2027')
})

test('usage bucket at UTC midnight Oct 1 reads Oct 1, not Sep 30', () => {
  assert.equal(utcBucketLabel('2026-10-01T00:00:00Z', 'MMM d'), 'Oct 1')
  assert.equal(utcBucketLabel('2026-10-01T00:00:00Z', 'EEE, MMM d'), 'Thu, Oct 1')
})

test('an unparseable key is shown as itself', () => {
  assert.equal(revenueBucketLabel('garbage'), 'garbage')
})
