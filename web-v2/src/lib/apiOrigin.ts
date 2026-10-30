// The public address of the Hardcount API, for what the dashboard shows an
// operator to copy (the API keys quick start, the Stripe webhook URL).
// VITE_API_URL wins when set: in dev the browser loads from Vite at :5173
// while the Go API runs on :8080. Otherwise the dashboard's own origin, which
// serves the API in a production deployment (and proxies /v1 in dev).
export const API_ORIGIN = import.meta.env.VITE_API_URL || window.location.origin

// Hardcount's guides, on GitHub, for links from the dashboard.
export const DOCS_BASE = 'https://github.com/gethardcount/hardcount/blob/main/docs'
