// CSV formula injection (ADR-090 §7). A cell beginning with = + - @ (or a leading
// TAB/CR) is EXECUTED as a formula by Excel, Google Sheets and LibreOffice — and
// customer display names flow into these exports. The CSV is the artifact an
// operator hands an auditor; a file that runs code when opened is not evidence.
//
// The Go exporter neutralizes the server-side streams (internal/api/exports.go,
// TestCSVSafe); this pins the client-side builder, which is the other half.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { csvSafeCell } from '../src/lib/csv.ts'

test('formula-leading cells are quote-prefixed so the spreadsheet renders them as text', () => {
  assert.equal(csvSafeCell('=HYPERLINK("http://evil.test","click")'), '\'=HYPERLINK("http://evil.test","click")')
  assert.equal(csvSafeCell('@SUM(A1:A9)'), "'@SUM(A1:A9)")
  assert.equal(csvSafeCell('-2+3+cmd|\' /C calc\'!A0'), "'-2+3+cmd|' /C calc'!A0")
  assert.equal(csvSafeCell('+HYPERLINK("x")'), '\'+HYPERLINK("x")')
  assert.equal(csvSafeCell('\tTabbed'), "'\tTabbed")
  assert.equal(csvSafeCell('\rCarriage'), "'\rCarriage")
})

test('ordinary values pass through untouched', () => {
  assert.equal(csvSafeCell(''), '')
  assert.equal(csvSafeCell('Acme Inc'), 'Acme Inc')
  assert.equal(csvSafeCell('cus_123'), 'cus_123')
  assert.equal(csvSafeCell('user@example.com'), 'user@example.com') // @ is not the FIRST char
  assert.equal(csvSafeCell('{"amount_cents":100}'), '{"amount_cents":100}')
})

test('cells that are numbers stay numbers — the finance columns must still SUM()', () => {
  // Blanket-prefixing would turn every negative amount in an invoice/credit export
  // into text. A pure number is not an injection: Excel renders it as that number
  // whether or not it carries a leading sign.
  assert.equal(csvSafeCell('-1250'), '-1250')
  assert.equal(csvSafeCell('+3.5'), '+3.5')
  assert.equal(csvSafeCell('-0.0003'), '-0.0003')
  // …but a number-ISH expression is not a number, and stays neutralized.
  assert.equal(csvSafeCell('-2+3'), "'-2+3")
  assert.equal(csvSafeCell('=1+1'), "'=1+1")
  // A tab in front of digits is never a legitimate numeric cell here.
  assert.equal(csvSafeCell('\t5'), "'\t5")
})
