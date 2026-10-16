// Webhook texts that come in two versions: the replay toast (sent to at
// least one endpoint, or to none), the paused-endpoint notice (shown or not)
// and where an endpoint save error goes (under the URL field, or a toast).
// Kept here so a test covers each version.
//
// Dependency-free (duck-typed errors) so the unit tests can load it.

export interface ToastCopy {
  level: 'success' | 'warning'
  title: string
  description: string
}

// webhook.Service.Replay always copies the event, then queues it for every
// active endpoint in the event's mode that listens for its type. status is
// 'no_matching_endpoints' when none matched (nothing was sent), 'queued'
// otherwise.
export function replayToast(status: string | undefined): ToastCopy {
  if (status === 'no_matching_endpoints') {
    return {
      level: 'warning',
      title: 'Event copied, not sent',
      description: 'No active endpoint listens for this event. Add or resume one, then resend.',
    }
  }
  return {
    level: 'success',
    title: 'Event resent',
    description: 'A copy is queued for every active endpoint that listens for it. Its deliveries show below.',
  }
}

// A paused endpoint gets no new events (dispatch and event replay skip
// inactive endpoints) and refuses per-delivery replay (webhook.Service
// ReplayDelivery: "endpoint is paused"). Null when active: no notice.
export function pausedEndpointNotice(active: boolean | undefined): { title: string; body: string } | null {
  if (active !== false) return null
  return {
    title: 'Endpoint paused',
    body: 'No events are sent here, and Replay is off, until you resume it.',
  }
}

// Where a failed endpoint save is shown. A refused URL (the server names the
// field) goes under the URL field like the create dialog; anything else is a
// toast.
export function endpointSaveErrorTarget(err: unknown): 'url' | 'toast' {
  const field = (err as { field?: unknown } | null)?.field
  return field === 'url' ? 'url' : 'toast'
}

// isAllEvents: an endpoint listens to every event when its list is empty or
// holds the "*" wildcard (what the dashboard sends for "All events").
export function isAllEvents(events: readonly string[] | null | undefined): boolean {
  return !events || events.length === 0 || events.includes('*')
}
