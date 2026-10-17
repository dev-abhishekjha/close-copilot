import { expect, test } from './guard';
import { navTo, signIn } from './helpers';

test.use({ storageState: { cookies: [], origins: [] } });

test('maker starts a run, decides a finding and proposes an entry', async ({ page }) => {
  await signIn(page, 'maker');

  // Start a run and watch it finish: the page follows the progress panel to
  // the findings list on its own.
  await navTo(page, 'Close runs');
  await page.locator('#company').selectOption('sharma');
  await page.getByRole('button', { name: 'Start close' }).click();
  await expect(page.getByRole('heading', { name: 'Progress' })).toBeVisible();
  await expect(page.getByRole('heading', { name: /Findings: sharma 2026-09/ })).toBeVisible({ timeout: 15_000 });

  // Open a finding of the run and accept it (the medium one is unverified:
  // take the first, the high one).
  await page.getByRole('link', { name: 'Details' }).first().click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('Unbooked bank charge');
  await page.getByRole('button', { name: 'Mark accepted' }).click();
  await expect(page.getByText('Accepted', { exact: true }).first()).toBeVisible();

  // Propose the entry of the seeded finding with a preview.
  await navTo(page, 'Findings');
  await page.getByRole('link', { name: 'Details' }).last().click();
  await expect(page.getByRole('region', { name: 'Proposed entry' })).toBeVisible();
  await page.getByRole('button', { name: 'Propose entry' }).click();
  await expect(page.getByText(/Status: proposed/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Propose entry' })).toHaveCount(0);

  // The proposal waits for a checker, and the maker has no decision control.
  await expect(page.getByRole('button', { name: 'Approve' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Reject' })).toHaveCount(0);
  await expect(page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Approvals' })).toHaveCount(0);
});
