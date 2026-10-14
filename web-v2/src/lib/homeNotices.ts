// Notes on the Dashboard and Usage pages that depend on state. Plain
// functions so node --test can check each version.

// Every Dashboard total leaves out test-clock customers and their
// subscriptions, invoices and usage (internal/analytics/simfilter.go), while
// Recent Invoices lists their invoices. Say so only where it can matter:
// test mode, with at least one test clock. Live mode has no test clocks.
export const SIMULATED_TOTALS_TITLE = 'Test clocks not counted'
export const SIMULATED_TOTALS_SENTENCE =
  'These totals leave out customers on a test clock. Their invoices still appear in lists, marked Simulated.'

export function showSimulatedTotalsNote(livemode: boolean, testClockCount: number): boolean {
  return !livemode && testClockCount > 0
}

// The Usage Events page when no event has ever arrived. Events name a meter,
// so a workspace with no meter is told to make one; one that has meters is
// told how events arrive instead of being sent to make another. null = the
// meter list hasn't loaded: no button rather than a wrong one.
export function usageNoneYet(meterCount: number | null): { description: string; setUpMeter: boolean } {
  return meterCount === 0
    ? { description: 'Send usage from your app through the API. Each event names a meter, so create one first.', setUpMeter: true }
    : { description: 'Send usage from your app through the API. Each event names one of your meters.', setUpMeter: false }
}
