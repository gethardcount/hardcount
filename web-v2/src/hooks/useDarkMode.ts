import { useEffect, useState } from 'react'

const STORAGE_KEY = 'hardcount-theme'

function safeGetItem(key: string): string | null {
  try { return localStorage.getItem(key) }
  catch { return null }
}

function safeSetItem(key: string, value: string) {
  try { localStorage.setItem(key, value) }
  catch { /* Private browsing mode, silently fail */ }
}

function getInitialTheme(): boolean {
  const stored = safeGetItem(STORAGE_KEY)
  if (stored === 'dark') return true
  if (stored === 'light') return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

export function useDarkMode() {
  const [dark, setDark] = useState(getInitialTheme)

  useEffect(() => {
    const root = document.documentElement
    if (dark) {
      root.classList.add('dark')
    } else {
      root.classList.remove('dark')
    }
    safeSetItem(STORAGE_KEY, dark ? 'dark' : 'light')
  }, [dark])

  const toggle = () => setDark(prev => !prev)

  return { dark, toggle }
}
