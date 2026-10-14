// One notice shape, three tones. Each coloured tone must carry dark-mode
// classes: two hand-made boxes on the subscription page had none and showed
// as bright light-mode panels in dark mode.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { CALLOUT_TONE_CLASSES, toneForSeverity } from '../src/lib/calloutTone.ts'

test('amber and red tones have dark-mode classes for box and icon', () => {
  for (const tone of ['amber', 'red'] as const) {
    assert.match(CALLOUT_TONE_CLASSES[tone].box, /dark:bg-/)
    assert.match(CALLOUT_TONE_CLASSES[tone].box, /dark:border-/)
    assert.match(CALLOUT_TONE_CLASSES[tone].icon, /dark:text-/)
  }
  // Neutral uses theme tokens, which already follow dark mode.
  assert.doesNotMatch(CALLOUT_TONE_CLASSES.neutral.box, /amber|red/)
})

test('attention severity maps to a tone; unknown reads as information', () => {
  assert.equal(toneForSeverity('critical'), 'red')
  assert.equal(toneForSeverity('warning'), 'amber')
  assert.equal(toneForSeverity('info'), 'neutral')
  assert.equal(toneForSeverity('something_new'), 'neutral')
  assert.equal(toneForSeverity(undefined), 'neutral')
})
