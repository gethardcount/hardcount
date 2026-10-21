import { Fragment } from 'react'
import { Link } from 'react-router-dom'
import { useTenantTimezone } from '@/hooks/useTenantTimezone'
import type { ActivityLine } from '@/lib/gen/schemas/activityLine'
import type { ActivityPart } from '@/lib/gen/schemas/activityPart'
import {
  TONES, actorText, groupByDay, lineTimes, linkRoute, money, partTime, recordedTime, timeOfDay, toneOf, zoneHeader,
} from '@/lib/activityLine'
import { cn } from '@/lib/utils'

// The shared activity list (ADR-128): rows grouped under day headers on the
// entity's calendar, in the order the server sent them (timeline.Order).
// Every word comes from the line; this component formats money and times,
// draws links and the tone's dot, and lays the row out. Every date and time
// is a <time> element, which the e2e text-parity check relies on.
//
// Row shape, the same on both clocks:
//   ● Title · amount                                  10:00 AM
//     Problem line (failure and warning rows)
//     Detail · Detail · Actor · Recorded Oct 18, 8:14 PM
//
// The zone is the account's (useTenantTimezone, which re-renders when
// /v1/settings arrives on a cold deep link), never the browser's.

export function ActivityZone({ lines }: { lines: readonly ActivityLine[] }) {
  // Every time the card shows, not only the row times: a test clock months
  // ahead puts the Recorded times on the other side of a summer-time change.
  const text = zoneHeader(lineTimes(lines), useTenantTimezone())
  if (!text) return null
  return <span className="text-xs font-normal text-muted-foreground">{text}</span>
}

export function ActivityList({ lines }: { lines: readonly ActivityLine[] }) {
  const tz = useTenantTimezone()
  const days = groupByDay(lines, tz)
  return (
    <ol className="space-y-4">
      {days.map((day, d) => (
        <li key={`${day.key}:${d}`}>
          <p data-activity-day className="mb-2 text-xs font-medium text-muted-foreground">
            <time dateTime={day.key}>{day.label}</time>
          </p>
          <ol>
            {day.lines.map((line, i) => (
              <ActivityRow key={line.id} line={line} tz={tz} last={i === day.lines.length - 1} />
            ))}
          </ol>
        </li>
      ))}
    </ol>
  )
}

function ActivityRow({ line, tz, last }: { line: ActivityLine; tz: string; last: boolean }) {
  const tone = TONES[toneOf(line.tone)]
  const actor = actorText(line.actor)
  const segments = line.detail.filter(s => s.length > 0)
  return (
    <li data-activity-row data-tone={toneOf(line.tone)} className="flex gap-3 pb-3 last:pb-0">
      <div className="flex flex-col items-center" aria-hidden>
        <div className={cn('mt-1.5 h-2.5 w-2.5 shrink-0 rounded-full', tone.dot)} />
        {!last && <div className="mt-1 w-px flex-1 bg-border" />}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex items-baseline justify-between gap-3">
          <p className="min-w-0 text-sm text-foreground [overflow-wrap:anywhere]">
            <span>{line.title}</span>
            {line.amount && (
              <>
                {' · '}
                <span className="tabular-nums">{money(line.amount)}</span>
              </>
            )}
          </p>
          <time dateTime={line.occurred_at} className="shrink-0 whitespace-nowrap text-xs text-muted-foreground tabular-nums">
            {timeOfDay(line.occurred_at, tz)}
          </time>
        </div>
        {line.problem && (
          <p className={cn('mt-0.5 text-xs [overflow-wrap:anywhere]', tone.problem)}>{line.problem}</p>
        )}
        {(segments.length > 0 || actor || line.recorded_at) && (
          <p className="mt-0.5 text-xs text-muted-foreground [overflow-wrap:anywhere]">
            {segments.map((seg, s) => (
              <Fragment key={`seg:${s}`}>
                {s > 0 && ' · '}
                {seg.map((part, p) => <PartView key={`part:${p}`} part={part} tz={tz} />)}
              </Fragment>
            ))}
            {actor && (
              <>
                {segments.length > 0 && ' · '}
                {actor}
              </>
            )}
            {line.recorded_at && (
              <>
                {(segments.length > 0 || actor) && ' · '}
                <span data-activity-recorded>
                  Recorded <time dateTime={line.recorded_at}>{recordedTime(line.recorded_at, tz)}</time>
                </span>
              </>
            )}
          </p>
        )}
      </div>
    </li>
  )
}

function PartView({ part, tz }: { part: ActivityPart; tz: string }) {
  if (part.money) return <span className="tabular-nums">{money(part.money)}</span>
  if (part.time) return <time dateTime={part.time}>{partTime(part.time, tz)}</time>
  if (part.link) {
    const to = linkRoute(part.link)
    if (!to) return <>{part.link.label}</>
    return (
      <Link to={to} data-activity-link={part.link.kind} className="text-primary underline underline-offset-2 hover:text-primary/80">
        {part.link.label}
      </Link>
    )
  }
  return <>{part.text ?? ''}</>
}
