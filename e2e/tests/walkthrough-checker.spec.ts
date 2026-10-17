import { expect, test } from './guard';
import { navTo, pressKeys, signIn } from './helpers';

test.use({ storageState: { cookies: [], origins: [] } });

test('checker approves one proposal, rejects another and reads the audit log', async ({ page }) => {
  await signIn(page, 'checker');

  await navTo(page, 'Approvals');
  await expect(page.getByRole('heading', { name: 'Pending proposals', level: 1 })).toBeVisible();
  const rows = page.locator('tbody tr[data-proposal], tbody > tr').filter({ has: page.getByRole('button', { name: 'Approve' }) });
  const before = await rows.count();
  expect(before).toBeGreaterThanOrEqual(2);

  // Approve the first: a confirmation dialog, then the posted notice.
  await rows.first().getByRole('button', { name: 'Approve' }).click();
  const approve = page.getByRole('dialog', { name: /Approve proposal/ });
  await expect(approve).toBeVisible();
  await approve.getByRole('button', { name: 'Approve' }).click();
  await expect(page.getByRole('status').filter({ hasText: /posted as journal entry/ })).toBeVisible();
  await expect(rows).toHaveCount(before - 1);

  // Reject the next one: a reason is required.
  await rows.first().getByRole('button', { name: 'Reject' }).click();
  const reject = page.getByRole('dialog', { name: /Reject proposal/ });
  await expect(reject).toBeVisible();
  await reject.getByLabel('Reason').fill('Duplicate of an entry booked in July');
  await reject.getByRole('button', { name: 'Reject' }).click();
  await expect(rows).toHaveCount(before - 2);

  // Audit: both decisions are in the log, and the filters narrow it.
  await navTo(page, 'Audit log');
  await expect(page.getByRole('heading', { name: 'Audit log', level: 1 })).toBeVisible();
  const table = page.getByRole('table', { name: 'Audit log' });
  await expect(table.getByRole('row').filter({ hasText: 'approve_proposal' }).first()).toContainText('ravi');
  await expect(table.getByRole('row').filter({ hasText: 'reject_proposal' }).first()).toContainText('ravi');

  await page.getByLabel('Actor').selectOption('agent');
  await page.getByRole('button', { name: 'Filter' }).click();
  await expect(table.getByRole('row').filter({ hasText: 'approve_proposal' })).toHaveCount(0);
  await expect(table.getByRole('row').filter({ hasText: 'get_bank_transactions' })).toHaveCount(1);

  await page.getByLabel('Actor').selectOption('');
  await page.getByLabel('Tool').selectOption('approve_proposal');
  await page.getByRole('button', { name: 'Filter' }).click();
  await expect(table.getByRole('row').filter({ hasText: 'get_gl_entries' })).toHaveCount(0);
  await expect(table.getByRole('row').filter({ hasText: 'approve_proposal' }).first()).toBeVisible();

  // The checker has no propose control on a finding.
  await navTo(page, 'Findings');
  await page.getByRole('link', { name: 'Details' }).last().click();
  await expect(page.getByRole('button', { name: 'Propose entry' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Mark accepted' })).toHaveCount(0);
});

test('command palette and shortcuts', async ({ page }) => {
  await signIn(page, 'checker');

  await page.keyboard.press('Control+k');
  const palette = page.getByRole('dialog', { name: 'Command palette' });
  await expect(palette).toBeVisible();
  await page.keyboard.type('audit');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/audit$/);

  await page.keyboard.press('?');
  const help = page.getByRole('dialog', { name: 'Keyboard shortcuts' });
  await expect(help).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(help).toBeHidden();

  await pressKeys(page, 'g d');
  await expect(page).toHaveURL(/\/$/);
});
