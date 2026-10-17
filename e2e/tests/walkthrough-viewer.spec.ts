import { users } from '../users';
import { expect, test } from './guard';
import { navTo, pressKeys, signIn } from './helpers';

test.use({ storageState: { cookies: [], origins: [] } });

test('viewer reads every page and sees no write control', async ({ page }) => {
  await signIn(page, 'viewer');
  await expect(page.getByRole('heading', { name: 'Dashboard', level: 1 })).toBeVisible();
  await expect(page.locator('.user')).toHaveText(users.viewer.name);
  await expect(page.locator('.role')).toHaveText('viewer');

  // Runs: the list is there, the start form is not.
  await navTo(page, 'Close runs');
  await expect(page.getByRole('heading', { name: 'Close runs', level: 1 })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Start close' })).toHaveCount(0);

  // Run detail, then its findings and metrics.
  await page.getByRole('link', { name: 'mehta' }).first().click();
  await expect(page.getByRole('heading', { name: 'mehta 2026-09', level: 1 })).toBeVisible();
  await page.getByRole('link', { name: 'Metrics' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('mehta 2026-09');
  await page.goBack();
  await page.getByRole('main').getByRole('link', { name: 'Findings' }).click();
  await expect(page.getByRole('heading', { name: /Findings: mehta 2026-09/ })).toBeVisible();

  // Finding detail: evidence is shown, no decision button.
  await page.getByRole('link', { name: 'Details' }).first().click();
  await expect(page.getByRole('region', { name: 'Evidence' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Citations' })).toBeVisible();
  for (const name of ['Mark accepted', 'Dismiss', 'Propose entry', 'Approve', 'Reject']) {
    await expect(page.getByRole('button', { name })).toHaveCount(0);
  }

  // The findings list of the company and month.
  await navTo(page, 'Findings');
  await expect(page.getByRole('heading', { name: 'Findings', level: 1 })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Mark selected accepted' })).toHaveCount(0);

  // Ask: an answer with citations, then a refusal.
  await navTo(page, 'Ask');
  await page.getByRole('textbox', { name: 'Question' }).fill('Why are there bank charges this month?');
  await page.getByRole('button', { name: 'Ask', exact: true }).click();
  await expect(page.getByText('three unbooked bank charges')).toBeVisible();
  await expect(page.getByText('POL-001').first()).toBeVisible();
  await page.getByRole('textbox', { name: 'Question' }).fill('What is the weather in Pune?');
  await page.getByRole('button', { name: 'Ask', exact: true }).click();
  await expect(page.getByText('I can only answer questions about this company')).toBeVisible();

  // Documents: filter by type, open one.
  await navTo(page, 'Documents');
  await page.getByRole('link', { name: 'Policy', exact: true }).click();
  await page.getByRole('link', { name: 'Bank charges policy' }).click();
  await expect(page.getByRole('heading', { name: 'Bank charges policy' }).first()).toBeVisible();

  // System.
  await navTo(page, 'System');
  await expect(page.getByRole('heading', { name: 'System', level: 1 })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Postgres' })).toBeVisible();

  // Approvals and audit are for checkers: no link, and a direct visit is refused.
  await expect(page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Approvals' })).toHaveCount(0);
  await expect(page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Audit log' })).toHaveCount(0);

  // Keyboard: go-to shortcut.
  await pressKeys(page, 'g d');
  await expect(page).toHaveURL(/\/$/);
});

test('viewer cannot open checker pages', async ({ page }) => {
  await signIn(page, 'viewer');
  for (const path of ['/approvals', '/audit']) {
    const res = await page.request.get(path);
    expect(res.status(), path).toBe(403);
  }
});
