// mount renders a React tree into the DOM of a test file that runs in
// happy-dom (`// @vitest-environment happy-dom` on its first line), so its
// effects run, its queries fetch and it re-renders, unlike a server render.
// Each helper wraps its work in act, so React has finished rendering when it
// returns.
import { act, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'

// React warns about updates outside act unless it knows it runs in tests.
;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

export interface Mounted {
  // text is everything the page shows, dialogs (portaled to body) included.
  text: () => string
  render: (tree: ReactNode) => Promise<void>
  unmount: () => Promise<void>
}

export async function mount(tree: ReactNode): Promise<Mounted> {
  const container = document.createElement('div')
  document.body.appendChild(container)
  let root: Root | undefined
  await act(async () => {
    root = createRoot(container)
    root.render(tree)
  })
  return {
    text: () => (document.body.textContent ?? '').replace(/\s+/g, ' '),
    render: async next => { await act(async () => root!.render(next)) },
    unmount: async () => {
      await act(async () => root!.unmount())
      document.body.innerHTML = ''
    },
  }
}

// click clicks the button or link whose text is exactly `label`.
export async function click(label: string): Promise<void> {
  const target = [...document.querySelectorAll('button, a')].find(el => el.textContent?.trim() === label)
  if (!target) throw new Error(`no button or link "${label}" in: ${document.body.textContent}`)
  await act(async () => { (target as HTMLElement).click() })
}

// until waits for check to pass, letting fetches settle between tries.
export async function until(check: () => void, tries = 50): Promise<void> {
  for (let i = 0; ; i++) {
    try {
      check()
      return
    } catch (err) {
      if (i >= tries) throw err
      await act(async () => { await new Promise(r => setTimeout(r, 10)) })
    }
  }
}
