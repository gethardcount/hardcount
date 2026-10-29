import { useEffect } from 'react'

// useCustomerSurface marks a page the operator's customers see (the hosted
// invoice, adding a card). Those pages speak for the operator's business, not
// Hardcount's, so index.css gives them neutral colours instead of Hardcount's
// paper and copper; the operator's brand_color still applies where the page
// uses it. The class goes on <html> so dialogs and the page edge match.
export function useCustomerSurface() {
  useEffect(() => {
    const root = document.documentElement
    root.classList.add('customer-surface')
    return () => {
      root.classList.remove('customer-surface')
    }
  }, [])
}
