// How a Combobox option becomes a cmdk item, and how the picker searches.
interface PickerOption {
  value: string
  label: string
  keywords?: string[]
}

/**
 * Returns the cmdk `value` and `keywords` props for one option. Options must
 * have distinct values; Combobox dedupes them.
 *
 * The cmdk value is the option's value (an id or a code), quoted. It must
 * differ from every other item's, because cmdk highlights every item whose
 * value equals the highlighted one, and Enter picks the first of them. So it
 * is never the label, which two customers can share. The quotes keep it
 * non-empty: cmdk never highlights or matches an item whose value is '', and
 * an option such as "No test clock" has that value. cmdk also trims values,
 * and the quotes leave nothing to trim.
 *
 * The label, the option's value and its keywords go into `keywords`, the only
 * text comboboxFilter searches.
 */
export function comboboxItem(option: PickerOption): { value: string; keywords: string[] } {
  return {
    value: JSON.stringify(option.value),
    keywords: [option.label, option.value, ...(option.keywords ?? [])],
  }
}

/**
 * The cmdk filter for Combobox: 1 when the search text appears, in any case,
 * in an item's keywords, else 0. It ignores cmdk's value, which is only the
 * item's quoted key (see comboboxItem).
 */
export function comboboxFilter(_value: string, search: string, keywords?: string[]): number {
  const haystack = (keywords ?? []).join(' ').toLowerCase()
  return haystack.includes(search.toLowerCase()) ? 1 : 0
}
