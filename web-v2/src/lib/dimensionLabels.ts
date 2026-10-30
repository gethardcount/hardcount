// labelRowsToMatch turns the rows of a price's label editor into the
// price's dimension_match, or the error to show on each row that is wrong.
//
// A dimension_match is a JSON object, so it cannot hold one key twice. The
// form used to write each row into the object in turn, so a second "model"
// row replaced the first and the price saved with one label where the
// operator typed two. The server cannot see that: it gets the object after
// the rows were merged. So the form refuses it.
//
// A row with both boxes blank is an empty row and is left out. A row with a
// value and no name used to be left out too; it is refused now, as is a name
// with no value (the server refuses that one as well).
//
// This module imports nothing so the unit tests can load it
// (tests/dimensionLabels.test.ts).

export interface LabelRow { key: string; value: string }
export interface LabelRowError { key?: string; value?: string }

export function labelRowsToMatch(rows: LabelRow[]):
  { match: Record<string, string> } | { errors: Record<number, LabelRowError> } {
  const match: Record<string, string> = {}
  const firstRow = new Map<string, number>()
  const errors: Record<number, LabelRowError> = {}
  rows.forEach((row, i) => {
    const k = row.key.trim()
    const v = row.value.trim()
    if (!k && !v) return
    const err: LabelRowError = {}
    if (!k) err.key = 'Enter a label name, or remove this row.'
    else if (firstRow.has(k)) err.key = `"${k}" is already label ${firstRow.get(k)! + 1}. A price matches each label name once.`
    if (!v) err.value = 'Enter a value.'
    if (err.key || err.value) {
      errors[i] = err
    }
    if (k && !firstRow.has(k)) firstRow.set(k, i)
    if (!err.key && !err.value) match[k] = v
  })
  return Object.keys(errors).length > 0 ? { errors } : { match }
}
