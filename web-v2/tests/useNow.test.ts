// What useNowFor and useNowResolver answer: real time for a loaded record,
// and not known (undefined) while it loads.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { useNowFor, useNowResolver, type ClockPinned } from '../src/hooks/useNow.ts'

interface Row {
  name: string
  record: ClockPinned | undefined
  // 'real' is real time, 'unknown' is undefined.
  want: 'real' | 'unknown'
}

const ROWS: Row[] = [
  { name: 'record not loaded', record: undefined, want: 'unknown' },
  { name: 'record loaded', record: {}, want: 'real' },
]

// answer renders the hook and returns the "now" it gave, in epoch ms, or
// undefined when it gave none.
function answer(row: Row, hook: 'useNowFor' | 'useNowResolver'): number | undefined {
  function Probe() {
    const resolve = useNowResolver()
    const one = useNowFor(row.record)
    const now = hook === 'useNowFor' ? one : resolve(row.record)
    return now === undefined ? 'unknown' : String(now)
  }
  const out = renderToStaticMarkup(createElement(Probe))
  return out === 'unknown' ? undefined : Number(out)
}

for (const row of ROWS) {
  for (const hook of ['useNowFor', 'useNowResolver'] as const) {
    const said = { real: 'real time', unknown: 'not known' }[row.want]
    test(`${hook}: ${row.name} is ${said}`, () => {
      const before = Date.now()
      const now = answer(row, hook)
      if (row.want === 'unknown') {
        assert.equal(now, undefined)
      } else {
        assert.ok(now !== undefined)
        assert.ok(now >= before && now <= Date.now(), `${new Date(now).toISOString()} is not real time`)
      }
    })
  }
}
