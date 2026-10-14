import type { FieldValues, Path, UseFormReturn } from 'react-hook-form'
import { toast } from 'sonner'
import { ApiError } from './api'
import { errorToast, type ErrorToast } from './errorCopy'

/**
 * Routes a server error onto a react-hook-form instance.
 *
 * - If the server returned a field-scoped error (ApiError.field maps to a form
 *   field), the message is attached to that input via form.setError — the user
 *   sees an inline highlight under the offending field. The input is also
 *   focused so keyboard users land on the problem immediately.
 * - If the error is field-less (InvalidState, NotFound, 500s, unexpected
 *   errors) or the field isn't rendered in this form, it shows as a toast
 *   (see showApiError) instead of silently disappearing.
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
    showErrorToast(errorToast(err, opts?.toastTitle))
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

  showErrorToast(errorToast(err, opts?.toastTitle))
}

/**
 * Show an error toast for a failed action. The caller's title names what
 * failed ("Couldn't attach meter"); the body is one sentence from
 * lib/errorCopy: the server's own explanation for a request it refused, or
 * plain words for a network failure, a rate limit or a server fault. A
 * server fault also offers [Copy ID] so the operator can hand the request
 * id to support; a validation error does not, the sentence says what to fix.
 * A 401 shows nothing: the session handler sends the operator to sign-in.
 *
 * Prefer this helper over raw `toast.error(err.message)` for every mutation
 * catch/onError path, even when you don't need form-field routing.
 */
export function showApiError(err: unknown, title: string): void {
  showErrorToast(errorToast(err, title))
}

function showErrorToast(t: ErrorToast): void {
  if (!t.show) return
  const copyId = t.copyId
  const show = t.tone === 'warning' ? toast.warning : toast.error
  show(t.title, {
    description: t.description,
    action: copyId
      ? {
          label: 'Copy ID',
          onClick: () => {
            void navigator.clipboard.writeText(copyId)
          },
        }
      : undefined,
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
