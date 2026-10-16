import path from 'node:path'

// Everything the suite needs to know about the servers under test. The
// defaults match CI; locally, point them at your own API and preview server.
export const env = {
  baseURL: process.env.E2E_BASE_URL ?? 'http://127.0.0.1:4173',
  apiURL: process.env.E2E_API_URL ?? 'http://127.0.0.1:8080',
  apiKey: process.env.E2E_API_KEY ?? '',
  email: process.env.E2E_EMAIL ?? '',
  password: process.env.E2E_PASSWORD ?? '',
}

// Files the setup project writes and the specs read. Git ignores this folder.
const stateDir = path.join(import.meta.dirname, '..', '.state')
export const authFile = path.join(stateDir, 'user.json')
export const seedFile = path.join(stateDir, 'seed.json')
