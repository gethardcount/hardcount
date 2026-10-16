import * as React from 'react'
import { Clock } from 'lucide-react'
import { cn } from '@/lib/utils'
import { DatePicker } from '@/components/ui/date-picker'

// DateTimePicker — branded date+time picker.
//
// Date is a typeable input with a calendar popover (DatePicker).
// Time is a typeable HH:mm input — the operator types "11:58" or
// "1158" and the field formats on blur. Both halves accept keyboard
// input; click-to-pick remains for the date via the calendar
// popover.
//
// Time is held as the operator-typed string in tenant TZ ("HH:mm").
// Caller is responsible for combining `date` + `time` into a UTC
// timestamp via fromZonedTime — same contract as DatePicker. Keeping
// the picker timezone-agnostic means it stays a pure UI primitive and
// doesn't need to read the tenant TZ itself.
//
// Hour granularity is 1h, minute granularity is 1min. Test-clock and
// billing-period flows need minute precision so the input doesn't
// snap to 15-minute increments.

interface DateTimePickerProps {
  date: string                       // yyyy-MM-dd
  time: string                       // HH:mm (24h, zero-padded)
  onDateChange: (date: string) => void
  onTimeChange: (time: string) => void
  className?: string
  minDate?: Date
  // datePlaceholder is forwarded to DatePicker for the empty-date state.
  datePlaceholder?: string
}

// parseTypedTime accepts the strings users type and returns canonical
// HH:mm or null if invalid.
//
// Accepted shapes:
//   "11:58", "1:5", "01:58", "9:5"   → with separator
//   "1158", "905"                    → 4 or 3 digits, no separator
//   ""                               → "" (clear)
// Rejected:
//   - 1-2 digit bare numbers ("5", "12") — too easy to typo and
//     accidentally submit; user must commit minutes explicitly
//   - hours > 23 or minutes > 59
//   - non-numeric input
function parseTypedTime(input: string): string | null {
  const trimmed = input.trim()
  if (!trimmed) return ''

  const compact = trimmed.replace(/\s+/g, '')

  let hh: number, mm: number
  if (compact.includes(':')) {
    const parts = compact.split(':')
    if (parts.length !== 2) return null
    if (!/^\d{1,2}$/.test(parts[0]) || !/^\d{1,2}$/.test(parts[1])) return null
    hh = parseInt(parts[0], 10)
    mm = parseInt(parts[1], 10)
  } else if (/^\d{4}$/.test(compact)) {
    // "1158" → 11:58
    hh = parseInt(compact.slice(0, 2), 10)
    mm = parseInt(compact.slice(2, 4), 10)
  } else if (/^\d{3}$/.test(compact)) {
    // "905" → 09:05 (last two digits are minutes)
    hh = parseInt(compact.slice(0, 1), 10)
    mm = parseInt(compact.slice(1, 3), 10)
  } else {
    return null
  }

  if (isNaN(hh) || isNaN(mm)) return null
  if (hh < 0 || hh > 23 || mm < 0 || mm > 59) return null
  return String(hh).padStart(2, '0') + ':' + String(mm).padStart(2, '0')
}

export function DateTimePicker({
  date,
  time,
  onDateChange,
  onTimeChange,
  className,
  minDate,
  datePlaceholder,
}: DateTimePickerProps) {
  const [inputText, setInputText] = React.useState(time || '')
  const [hasError, setHasError] = React.useState(false)
  const inputRef = React.useRef<HTMLInputElement>(null)
  const errorId = React.useId()

  // Sync displayed text when the canonical time changes externally
  // (caller resets, parent commits a different value). Three things
  // must be true to skip the sync:
  //  1. The user is mid-typing — never overwrite their draft.
  //  2. The field is currently in an error state — the parent's time
  //     just got cleared by our own commit() failure path; mirroring
  //     that empty back into inputText would erase the user's bad
  //     input that they need to see in red to fix.
  //  3. The text already matches the canonical value — avoid a
  //     pointless setState that could re-trigger the effect.
  React.useEffect(() => {
    if (inputRef.current && document.activeElement === inputRef.current) return
    if (hasError) return
    setInputText(time || '')
  }, [time, hasError])

  const commit = () => {
    const parsed = parseTypedTime(inputText)
    if (parsed === null) {
      // Invalid: surface a red border AND clear the parent's time so
      // any submit gating tied to a valid HH:mm value flips to
      // disabled. Without this, the parent keeps the previous valid
      // value and a button reading the form's combined date+time
      // would happily submit stale-but-valid data while the user
      // sees their bad input.
      setHasError(true)
      if (time !== '') onTimeChange('')
      return
    }
    setHasError(false)
    if (parsed !== time) {
      onTimeChange(parsed)
    }
    // Reformat to canonical so "1158" snaps to "11:58" on blur even
    // if it round-trips to the same parsed value.
    if (parsed !== inputText) setInputText(parsed)
  }

  return (
    <div className={cn('flex flex-col gap-1', className)}>
      {/* Wraps the time field under the date on narrow containers (a dialog
          on a phone), instead of pushing it past the dialog's edge. */}
      <div className="flex flex-wrap items-center gap-2">
        <DatePicker
          value={date}
          onChange={onDateChange}
          placeholder={datePlaceholder}
          minDate={minDate}
          className="flex-1 min-w-0"
        />
        <div
          className={cn(
            'flex shrink-0 items-center gap-1.5 rounded-md border bg-transparent dark:bg-input/30 px-2.5 h-9 transition-colors cursor-text',
            'has-[input:focus-visible]:border-ring has-[input:focus-visible]:ring-3 has-[input:focus-visible]:ring-ring/50',
            hasError ? 'border-destructive' : 'border-input',
          )}
          onClick={(e) => { if (e.target === e.currentTarget) inputRef.current?.focus() }}
        >
          <Clock className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
          <input
            ref={inputRef}
            type="text"
            inputMode="numeric"
            value={inputText}
            onChange={(e) => { setInputText(e.target.value); if (hasError) setHasError(false) }}
            onBlur={commit}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commit() } }}
            placeholder="HH:mm"
            aria-invalid={hasError || undefined}
            aria-describedby={hasError ? errorId : undefined}
            aria-label="Time (24-hour, HH:mm)"
            autoComplete="off"
            spellCheck={false}
            className="w-14 bg-transparent outline-none text-sm font-mono placeholder:text-muted-foreground"
          />
        </div>
      </div>
      {hasError && (
        <p id={errorId} role="alert" className="text-xs text-destructive">Enter a 24-hour time as <span className="font-mono">HH:mm</span> (e.g. <span className="font-mono">09:30</span> or <span className="font-mono">0930</span>).</p>
      )}
    </div>
  )
}
