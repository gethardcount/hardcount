// The sidebar and the Ctrl+K palette read one navigation config.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { allNavItems, isNavActive, isTestMode, NAV_SECTIONS, visibleNav } from '../src/lib/navigation.ts'

test('every routed operator page is in the navigation', () => {
  const main = readFileSync(new URL('../src/main.tsx', import.meta.url), 'utf8')
  const routed = [...main.matchAll(/<Route path="(\/[a-z-]+)" element=\{<ProtectedRoute>/g)].map(m => m[1])
  const nav = new Set(allNavItems(true).map(i => i.to))
  // Sub-pages reached from their parent page, not the menu.
  const reachedFromParent = new Set(['/webhooks/events'])
  for (const r of routed) {
    if (reachedFromParent.has(r)) continue
    assert.ok(nav.has(r), `route ${r} is not in the navigation`)
  }
})

test('test clocks are listed in test mode only', () => {
  assert.ok(allNavItems(true).some(i => i.to === '/test-clocks'))
  assert.ok(!allNavItems(false).some(i => i.to === '/test-clocks'))
  assert.ok(visibleNav(false).every(s => s.items.length > 0))
})

test('test clocks stay hidden while the user is still loading', () => {
  // A live-mode operator must not see Test clocks flash in before /whoami answers.
  const shown = (user: { livemode: boolean } | null | undefined) =>
    allNavItems(isTestMode(user)).some(i => i.to === '/test-clocks')
  assert.equal(shown(undefined), false)
  assert.equal(shown(null), false)
  assert.equal(shown({ livemode: true }), false)
  assert.equal(shown({ livemode: false }), true)
})

test('labels are sentence case and icons are not shared', () => {
  const items = NAV_SECTIONS.flatMap(s => s.items)
  for (const i of items) {
    const words = i.label.split(' ')
    for (const w of words.slice(1)) {
      if (w === 'API') continue
      assert.equal(w, w.toLowerCase(), `"${i.label}" is not sentence case`)
    }
  }
  const icons = items.map(i => i.icon)
  assert.equal(new Set(icons).size, icons.length, 'two nav items share an icon')
})

test('detail pages keep their section highlighted', () => {
  const pricing = allNavItems(true).find(i => i.to === '/pricing')!
  assert.ok(isNavActive(pricing, '/plans/vlx_pln_1'))
  assert.ok(isNavActive(pricing, '/meters/vlx_mtr_1'))
  const home = allNavItems(true).find(i => i.to === '/')!
  assert.ok(isNavActive(home, '/'))
  assert.ok(!isNavActive(home, '/customers'))
})

test('the Ctrl+K palette builds its pages from the shared config', () => {
  const palette = readFileSync(new URL('../src/components/CommandPalette.tsx', import.meta.url), 'utf8')
  assert.ok(palette.includes('allNavItems('), 'CommandPalette must list pages from lib/navigation')
  assert.ok(!/const NAV_ITEMS\s*:/.test(palette), 'CommandPalette must not keep its own page list')
  const layout = readFileSync(new URL('../src/components/Layout.tsx', import.meta.url), 'utf8')
  assert.ok(layout.includes('visibleNav('), 'Layout must render the sidebar from lib/navigation')
})

test('Dunning policies sit directly under Dunning', () => {
  const items = allNavItems(true)
  const dunning = items.findIndex(i => i.to === '/dunning')
  assert.equal(items[dunning + 1]?.to, '/dunning-policies')
})
