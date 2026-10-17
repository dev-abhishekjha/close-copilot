import fs from 'node:fs';
import { chromium, type FullConfig } from '@playwright/test';
import { authFile, users, type Role } from './users';

// One real sign-in per role through the login form; the specs reuse the
// resulting cookies (the walkthroughs sign in again by hand).
export default async function globalSetup(config: FullConfig) {
  const baseURL = config.projects[0].use.baseURL as string;
  fs.mkdirSync('.auth', { recursive: true });
  const browser = await chromium.launch();
  try {
    for (const role of Object.keys(users) as Role[]) {
      const ctx = await browser.newContext({ baseURL });
      const page = await ctx.newPage();
      await page.goto('/login');
      await page.getByLabel('Username').fill(users[role].name);
      await page.getByLabel('Password').fill(users[role].password);
      await page.getByRole('button', { name: /sign in/i }).click();
      await page.waitForURL(`${baseURL}/`);
      await ctx.storageState({ path: authFile(role) });
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
}
