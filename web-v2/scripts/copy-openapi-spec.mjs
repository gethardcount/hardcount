// Copies api/openapi.yaml (the single source of truth at the repo root)
// into web-v2/public/openapi.yaml so:
//
// 1. The built SPA ships the spec at a stable same-origin URL
//    (`/openapi.yaml`) for anyone who wants to fetch it. (The embedded
//    Scalar viewer that used to render it at /docs/api was cut
//    2026-04-29 — no in-app viewer exists today.)
// 2. Local TS codegen (orval) reads the same file every other consumer
//    reads.
//
// Wired as a `predev` / `prebuild` hook in package.json so the served
// copy is always fresh — and as part of `npm run gen` so the codegen
// pipeline (and the CI drift check that runs it) operates on the
// canonical source. The copy is not committed (.gitignore); these hooks
// make it.
//
// Symlinking would be more elegant but is fragile across hosts that
// untar the repo onto Windows or that flatten symlinks (Vite's static
// asset pipeline included). An explicit copy is portable and makes the
// dependency obvious to readers of the package.json scripts list.
import { copyFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const src = resolve(here, '../../api/openapi.yaml')
const dest = resolve(here, '../public/openapi.yaml')

mkdirSync(dirname(dest), { recursive: true })
copyFileSync(src, dest)
console.log(`copied ${src} → ${dest}`)
