// Module-resolution hook for `node --test`: resolve the vite `@/…` alias so pure
// modules under src/ can be unit-tested without vitest. The heavy `@/lib/api`
// client (react-query, browser globals, generated types) is swapped for a tiny
// stub — a pure module like lib/dates only imports it for a fallback getter that
// tests never hit (they pass an explicit timezone). Extend api-stub.ts if another
// pure module needs a different named export. Extensionless relative imports
// get a .ts suffix, as vite would resolve them.
export async function resolve(specifier, context, nextResolve) {
  if (specifier === '@/lib/api') {
    return nextResolve(new URL('./api-stub.ts', import.meta.url).href, context)
  }
  // Vite resolves extensionless relative imports ('./money'); node does not.
  // A pure module that imports a sibling the way the app does gets the .ts.
  if (/^\.\.?\//.test(specifier) && !/\.[cm]?[jt]sx?$/.test(specifier)) {
    return nextResolve(specifier + '.ts', context)
  }
  return nextResolve(specifier, context)
}
