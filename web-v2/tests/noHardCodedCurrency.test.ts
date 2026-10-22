// The dashboard has no default currency: formatCents and getCurrencySymbol
// require one, so the type checker lists any call that would guess. A symbol
// typed straight into the markup is invisible to it. Two slipped through: the
// Home chart axis built "$5K" for every workspace, and a voided invoice read
// "Amount due $0.00" in any currency. This scans the source for a literal "$"
// before an amount or before an interpolated one.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const SRC = fileURLToPath(new URL('../src', import.meta.url))

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap(e => {
    const p = join(dir, e.name)
    if (e.isDirectory()) return e.name === 'gen' ? [] : sourceFiles(p)
    return /\.tsx?$/.test(e.name) && e.name !== 'api.gen.ts' ? [p] : []
  })
}

// Comments quote examples like "$0.00" on purpose; only code counts. A block
// comment keeps its line breaks so the reported line numbers stay right.
function stripComments(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, c => c.replace(/[^\n]/g, ''))
    .replace(/(^|[^:])\/\/.*$/gm, '$1')
}

// "$0.00" or "$1,200.00" as text, or `$${…}` in a template.
const HARD_CODED = /\$\d[\d,]*\.\d\d|`\$\$\{/

test('no amount is written with a literal "$"', () => {
  const hits: string[] = []
  for (const file of sourceFiles(SRC)) {
    stripComments(readFileSync(file, 'utf8')).split('\n').forEach((line, i) => {
      if (HARD_CODED.test(line)) hits.push(`${file.slice(SRC.length + 1)}:${i + 1}: ${line.trim()}`)
    })
  }
  assert.deepEqual(hits, [])
})
