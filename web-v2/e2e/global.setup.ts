import fs from 'node:fs'
import path from 'node:path'
import { test as setup, expect } from '@playwright/test'
import { env, authFile, seedFile } from './fixtures/env'
import { seedWorld } from './fixtures/seed'

// Runs once before the specs (the "setup" project in playwright.config.ts).

setup('seed data through the API', async () => {
  const seed = await seedWorld()
  fs.mkdirSync(path.dirname(seedFile), { recursive: true })
  fs.writeFileSync(seedFile, JSON.stringify(seed, null, 2))
})

setup('sign in through the login page', async ({ page }) => {
  if (!env.email || !env.password) throw new Error('E2E_EMAIL and E2E_PASSWORD must be set')
  await page.goto('/login')
  await page.getByLabel('Email').fill(env.email)
  await page.getByLabel('Password', { exact: true }).fill(env.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Home' })).toBeVisible()
  await page.context().storageState({ path: authFile })
})
