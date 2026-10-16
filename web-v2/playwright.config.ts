import { defineConfig, devices } from '@playwright/test'
import { env, authFile } from './e2e/fixtures/env'

// End-to-end tests for the dashboard, run against a real API with seeded data.
// How to run them: e2e/README.md.
export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // One retry in CI, so a test that passes on retry is reported as flaky
  // instead of hiding. None locally: a flaky test should be seen and fixed.
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: env.baseURL,
    trace: 'on-first-retry',
    timezoneId: 'UTC',
    locale: 'en-US',
  },
  projects: [
    { name: 'setup', testMatch: /global\.setup\.ts/ },
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'], storageState: authFile },
      dependencies: ['setup'],
    },
  ],
})
