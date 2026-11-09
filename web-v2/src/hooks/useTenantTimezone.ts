import { useTenantSettings } from '@/hooks/useTenantSettings'

// useTenantTimezone returns the account's time zone from the settings query.
// Every day or time an operator picks is sent as an instant in this zone, so
// a write site takes the zone from here, and a memo that turns days into
// instants lists it as a dependency. A Settings save updates the query, so
// the next render has the new zone.
//
// Signed-in pages render only once the settings have loaded
// (AccountSettingsGate), so there is always a zone here. Called before that,
// it throws: a guessed zone would be sent with no error.
export function useTenantTimezone(): string {
  const { data } = useTenantSettings()
  if (!data?.timezone) throw new Error('useTenantTimezone: the account settings have not loaded. Render under AccountSettingsGate.')
  return data.timezone
}
