import type { FieldValues, Path, UseFormReturn } from 'react-hook-form'
import { toast } from 'sonner'
import { ApiError } from './api'
import { emailNotConfiguredOutcome, isEmailNotConfigured } from './actionOutcomes'
import { showOutcome } from './outcomeToast'

/**
 * Routes a server error onto a react-hook-form instance.
 *
 * - If the server returned a field-scoped error (ApiError.field maps to a form
 *   field), the message is attached to that input via form.setError — the user
 *   sees an inline highlight under the offending field. The input is also
 *   focused so keyboard users land on the problem immediately.
 * - If the error is field-less (InvalidState, NotFound, 500s, unexpected
 *   errors) or the field isn't rendered in this form, the message shows as a
 *   toast instead of silently disappearing. When a Request ID is available,
 *   it's attached as the toast description with a Copy action — operators can
 *   paste it straight into a support email and we can trace it in logs.
 *
 * Pass `fields` as either:
 *   - `string[]` of form field names when server and form names match, or
 *   - `Record<string, string>` mapping server-field → form-field when they
 *     differ (e.g. backend `percent_off` maps to form `discountValue`).
 *
 * @example
 * applyApiError(form, err, ['external_id', 'display_name', 'email'])
 *
 * @example
 * applyApiError(form, err, {
 *   percent_off: 'discountValue',
 *   amount_off: 'discountValue',
 *   max_redemptions: 'maxRedemptions',
 * })
 */
export function applyApiError<T extends FieldValues>(
  form: UseFormReturn<T>,
  err: unknown,
  fields: readonly string[] | Record<string, string>,
  opts?: { toastTitle?: string },
): void {
  if (!(err instanceof ApiError)) {
    toast.error(opts?.toastTitle ?? (err instanceof Error ? err.message : 'Something went wrong'))
    return
  }

  const formField = resolveFormField(err.field, fields)
  if (formField) {
    form.setError(formField as Path<T>, {
      type: 'server',
      message: err.message,
    })
    form.setFocus(formField as Path<T>)
    return
  }

  toastApiError(err)
}

/**
 * Show a user-facing error toast. When the server returned a Request ID, it is
 * included as the toast description with a Copy action — operators can paste
 * it into a support email and the server-side trace is discoverable in logs.
 *
 * Prefer this helper over raw `toast.error(err.message)` for every mutation
 * catch/onError path, even when you don't need form-field routing.
 */
export function showApiError(err: unknown, fallback: string): void {
  if (err instanceof ApiError) {
    toastApiError(err)
    return
  }
  toast.error(err instanceof Error ? err.message : fallback)
}

function toastApiError(err: ApiError): void {
  if (isEmailNotConfigured(err)) {
    showOutcome(emailNotConfiguredOutcome())
    return
  }
  if (!err.requestId) {
    toast.error(err.message)
    return
  }
  const requestId = err.requestId
  toast.error(err.message, {
    description: `Request ID: ${requestId}`,
    action: {
      label: 'Copy ID',
      onClick: () => {
        void navigator.clipboard.writeText(requestId)
      },
    },
  })
}

function resolveFormField(
  serverField: string | undefined,
  fields: readonly string[] | Record<string, string>,
): string | undefined {
  if (!serverField) return undefined
  if (Array.isArray(fields)) {
    return fields.includes(serverField) ? serverField : undefined
  }
  return (fields as Record<string, string>)[serverField]
}
