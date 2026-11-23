// Drift-guard for the relative-time anchor (ADR-086 Phase 4).
//
// The branded EffectiveNow type + required-anchor helper signatures are the
// compile-time gate (you cannot call a relative-time helper without an anchor).
// This is the runtime companion: it proves each helper measures against the
// ANCHOR, not wall-clock. If a helper ever leaked Date.now() back in, the frozen
// 2027 assertions below would flip to real-time deltas (years, or "just now")
// and fail — that's the mutation-verify. Pure functions, no DOM: runs under
// `npm test` (Vitest).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  wallClockNow,
  timeAgo,
  daysUntil,
  isOlderThan,
  sinceMs,
  untilMs,
  cycleProgress,
  rollingWindow,
  type EffectiveNow,
} from '../src/lib/effectiveNow.ts'

const at = (iso: string) => Date.parse(iso) as EffectiveNow

test('timeAgo anchors on the frozen instant, not wall-clock', () => {
  const now = at('2027-08-08T00:00:00.000Z')
  // Three days before the anchor reads "3d ago" no matter what the real clock
  // says. A leaked Date.now() would make this a multi-year delta today.
  assert.equal(timeAgo('2027-08-05T00:00:00.000Z', now), '3d ago')
  assert.equal(timeAgo('2027-08-07T22:30:00.000Z', now), '1h ago')
  // A timestamp after the anchor clamps to "just now", never a negative age.
  assert.equal(timeAgo('2027-09-01T00:00:00.000Z', now), 'just now')
  // Unparseable input is swallowed (empty string), never "NaNd ago".
  assert.equal(timeAgo('not-a-date', now), '')
})

test('daysUntil is ceil-days relative to the anchor', () => {
  const now = at('2027-08-08T00:00:00.000Z')
  assert.equal(daysUntil('2027-08-11T00:00:00.000Z', now), 3)
  assert.equal(daysUntil('2027-08-05T00:00:00.000Z', now), -3)
})

test('isOlderThan gates on the anchor, guards NaN', () => {
  const now = at('2027-08-08T00:00:00.000Z')
  assert.equal(isOlderThan('2027-08-06T00:00:00.000Z', 24 * 60 * 60 * 1000, now), true)
  assert.equal(isOlderThan('2027-08-07T12:00:00.000Z', 24 * 60 * 60 * 1000, now), false)
  assert.equal(isOlderThan('garbage', 1000, now), false)
})

test('sinceMs / untilMs have opposite signs around the anchor', () => {
  const now = at('2027-08-08T00:00:00.000Z')
  assert.equal(sinceMs('2027-08-07T00:00:00.000Z', now), 86_400_000)
  assert.equal(untilMs('2027-08-09T00:00:00.000Z', now), 86_400_000)
})

test('cycleProgress measures against the anchor and clamps to 100%', () => {
  const now = at('2027-08-20T00:00:00.000Z')
  const p = cycleProgress('2027-08-01T00:00:00.000Z', '2027-08-31T00:00:00.000Z', now)
  assert.equal(p.totalDays, 30)
  assert.equal(p.daysIn, 19)
  const over = cycleProgress('2027-08-01T00:00:00.000Z', '2027-08-10T00:00:00.000Z', now)
  assert.equal(over.pct, 100)
  assert.equal(over.daysIn, over.totalDays)
})

test('rollingWindow includes the anchor instant (to is exclusive on the server)', () => {
  // The server keeps `timestamp < to`, so `to` must be past the anchor or the
  // 30d window misses an event stamped at the anchor.
  const now = at('2026-11-15T23:00:00.000Z')
  const w = rollingWindow(30, now)
  const event = Date.parse('2026-11-15T23:00:00.000Z')
  assert.ok(event < Date.parse(w.to), 'an event stamped at the anchor is inside the window')
  assert.ok(event >= Date.parse(w.from))
  assert.equal(w.to, '2026-11-15T23:00:00.001Z')
  assert.equal(w.from, '2026-10-16T23:00:00.000Z')
})

test('wallClockNow() is real time', () => {
  const before = Date.now()
  const now = wallClockNow()
  assert.ok(now >= before && now <= Date.now())
})
