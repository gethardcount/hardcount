// downloadServerCSV hits a server-side streaming CSV endpoint
// (/v1/exports/*.csv) and triggers a browser download. Used for
// "all rows" exports where the dataset is too large to load
// client-side. The dashboard rides cookie auth (credentials:
// 'include'); SDK callers can use the underlying URL directly with
// a Bearer header.
//
// The endpoint sets Content-Disposition with a timestamped filename;
// the browser respects that, so we don't need to set `download` on
// the anchor — but we do for older browsers that ignore the header.
export async function downloadServerCSV(path: string, defaultFilename: string): Promise<void> {
  const API_BASE = (window as { __VELOX_API_BASE?: string }).__VELOX_API_BASE || ''
  const url = `${API_BASE}${path}`
  const res = await fetch(url, { credentials: 'include' })
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText)
    throw new Error(`Export failed (${res.status}): ${text}`)
  }
  // Honour the server's filename if present.
  let filename = defaultFilename
  const cd = res.headers.get('Content-Disposition') || ''
  const match = /filename="([^"]+)"/.exec(cd)
  if (match) filename = match[1]
  const blob = await res.blob()
  const blobUrl = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = blobUrl
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(blobUrl)
}

// csvSafeCell neutralizes spreadsheet formula injection. Excel, Google Sheets and
// LibreOffice EXECUTE a cell whose first character is = + - @ (or a leading TAB /
// CR), so a customer-controlled value like `=HYPERLINK("http://evil","click")` or
// `@SUM(...)` runs when the operator opens the export. CSV quoting does not save
// you — the parser strips the quotes before the formula engine sees the cell.
// Prefixing with a single quote forces the value to render as literal text.
//
// The property being protected: the CSV is the artifact an operator hands an
// AUDITOR. A file that executes code when opened is not evidence. Customer
// display names flow into these exports, so the payload is one API call away.
//
// The numeric escape hatch is deliberate: a cell that IS a number ("-1250",
// "+3.5", "1e3") is not a formula in any sense that matters — Excel renders it as
// that number either way — and blanket-prefixing would turn every negative amount
// in a finance export into TEXT, silently breaking SUM() on the columns operators
// reconcile with. Leading TAB/CR are excluded from that escape hatch because
// Number() TRIMS whitespace — Number("\t5") is 5 — so a tab-prefixed cell would
// otherwise slip through un-neutralized.
// (The Go exporter is stricter — it prefixes any leading formula char — because
// it applies csvSafe only to columns it KNOWS are free text, so it never sees a
// number. Here the builder is generic over string cells and has no such type.)
const CSV_FORMULA_PREFIXES = ['=', '+', '-', '@', '\t', '\r']

export function csvSafeCell(value: string): string {
  if (!value) return value
  const first = value[0]
  if (!CSV_FORMULA_PREFIXES.includes(first)) return value
  if (first !== '\t' && first !== '\r' && value.trim() !== '' && Number.isFinite(Number(value))) return value
  return `'${value}`
}

export function downloadCSV(filename: string, headers: string[], rows: string[][]) {
  const csv = [headers.join(','), ...rows.map(r => r.map(cell => `"${csvSafeCell(String(cell)).replace(/"/g, '""')}"`).join(','))].join('\n')
  const blob = new Blob([csv], { type: 'text/csv' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
