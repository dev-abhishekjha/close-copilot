import path from 'node:path';
import { authFile } from '../users';
import { test } from '@playwright/test';
import { pages } from './pages';

// One full-page screenshot per page, at desktop, written to
// e2e/screenshots/<page>.png. The images are documentation: nothing diffs
// against them. The page's data is synthetic (e2e/fixture). Refresh with
// `make e2e-shots`.
const FIXED_NOW = new Date('2026-10-01T09:00:00Z');

for (const p of pages) {
  // The role that sees the most of the page: checker sees every page.
  const role = p.roles.includes('checker') ? 'checker' : p.roles[0];
  test(`screenshot ${p.name}`, async ({ browser }) => {
    const ctx = await browser.newContext({
      storageState: authFile(role),
      viewport: { width: 1280, height: 800 },
      colorScheme: 'light',
      reducedMotion: 'reduce',
      // The stylesheet below is injected, which the policy would block.
      bypassCSP: true,
    });
    await ctx.clock.install({ time: FIXED_NOW });
    const page = await ctx.newPage();
    await page.goto(p.path);
    await page.addStyleTag({
      content: '*, *::before, *::after { animation: none !important; transition: none !important; caret-color: transparent !important; }',
    });
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: path.join('screenshots', `${p.name}.png`), fullPage: true });
    await ctx.close();
  });
}
