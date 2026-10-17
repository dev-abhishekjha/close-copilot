import AxeBuilder from '@axe-core/playwright';
import { viewports } from '../playwright.config';
import { authFile, type Role } from '../users';
import { expect, test } from '@playwright/test';
import { forRole, pages } from './pages';

const roles: Role[] = ['viewer', 'maker', 'checker'];
const tags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'];

// axe injects style elements into the page, which the app's strict CSP would
// block, so this spec bypasses the CSP; csp.spec.ts and the walkthroughs keep
// it on and watch for violations.
test.use({ bypassCSP: true });

// No rule is disabled. color-contrast in particular stays on.
async function scan(page: import('@playwright/test').Page) {
  const { violations } = await new AxeBuilder({ page }).withTags(tags).analyze();
  return violations.map((v) => ({
    rule: v.id,
    impact: v.impact,
    nodes: v.nodes.slice(0, 4).map((n) => `${n.target.join(' ')} :: ${(n.failureSummary ?? '').split('\n').slice(0, 2).join(' | ')}`),
  }));
}

for (const size of ['desktop', 'tablet'] as const) {
  test.describe(`axe at ${size}`, () => {
    test.use({ viewport: viewports[size] });

    test('login page', async ({ page }) => {
      await page.goto('/login');
      expect(await scan(page)).toEqual([]);
    });

    for (const role of roles) {
      test.describe(`as ${role}`, () => {
        test.use({ storageState: authFile(role) });
        for (const p of forRole(role)) {
          test(p.name, async ({ page }) => {
            await page.goto(p.path);
            await expect(page.locator('main')).toBeVisible();
            expect(await scan(page)).toEqual([]);
          });
        }
      });
    }
  });
}

test('every page of the table is scanned for at least one role', () => {
  expect(pages.filter((p) => p.roles.length === 0)).toEqual([]);
});
