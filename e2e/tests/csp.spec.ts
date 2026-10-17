import { viewports } from '../playwright.config';
import { authFile, type Role } from '../users';
import { expect, test } from './guard';
import { forRole } from './pages';

// The guard (guard.ts) runs behind every test and fails on a Content Security
// Policy violation, a console error or a request to another origin. This
// spec makes sure every page, and the overlays on it, is loaded under it.
const roles: Role[] = ['viewer', 'maker', 'checker'];

test('login page loads under the policy', async ({ page, request }) => {
  const res = await request.get('/login');
  expect(res.headers()['content-security-policy']).toContain("default-src 'self'");
  await page.goto('/login');
  await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();
});

for (const role of roles) {
  test.describe(`as ${role}`, () => {
    test.use({ storageState: authFile(role) });

    for (const size of ['desktop', 'phone'] as const) {
      test(`every page at ${size}`, async ({ page }) => {
        await page.setViewportSize(viewports[size]);
        for (const p of forRole(role)) {
          const res = await page.goto(p.path);
          expect(res?.status(), p.path).toBe(200);
          await expect(page.locator('main')).toBeVisible();
        }
      });
    }

    test('overlays and toasts', async ({ page }) => {
      await page.goto('/');
      await page.keyboard.press('Control+k');
      await expect(page.getByRole('dialog', { name: 'Command palette' })).toBeVisible();
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !(document.getElementById('ui-palette') as HTMLDialogElement).open);
      // The browser moves focus out of a closed dialog at the next frame.
      await page.evaluate(() => new Promise((done) => requestAnimationFrame(() => done(true))));
      await page.keyboard.press('?');
      await expect(page.getByRole('dialog', { name: 'Keyboard shortcuts' })).toBeVisible();
      await page.keyboard.press('Escape');
      // A toast is drawn by script, with no inline style.
      await page.evaluate(() =>
        document.dispatchEvent(new CustomEvent('cc:toast', { detail: { kind: 'info', message: 'Checking the policy' } })),
      );
      await expect(page.locator('#ui-toasts [data-toast]')).toContainText('Checking the policy');
    });
  });
}
