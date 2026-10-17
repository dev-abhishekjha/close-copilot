import type { Page } from '@playwright/test';
import { users, type Role } from '../users';

// signIn goes through the login form with the keyboard, the way a person does.
export async function signIn(page: Page, role: Role) {
  await page.goto('/login');
  await page.getByLabel('Username').fill(users[role].name);
  await page.getByLabel('Password').fill(users[role].password);
  await page.getByLabel('Password').press('Enter');
  await page.waitForURL((u) => u.pathname === '/');
}

// navLink clicks a link of the sidebar. On a narrow screen the menu is closed
// first, so it is opened.
export async function navTo(page: Page, name: string) {
  const toggle = page.locator('summary.shell-menu-toggle');
  if (await toggle.isVisible()) {
    const menu = page.locator('details.shell-menu');
    if (!(await menu.evaluate((d) => (d as HTMLDetailsElement).open))) {
      await toggle.click();
    }
  }
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name, exact: true }).click();
}

// pressKeys sends a sequence such as "g d" one key at a time.
export async function pressKeys(page: Page, seq: string) {
  for (const k of seq.split(' ')) {
    await page.keyboard.press(k);
  }
}
