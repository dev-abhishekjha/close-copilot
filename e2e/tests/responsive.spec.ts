import type { Page } from '@playwright/test';
import { viewports } from '../playwright.config';
import { authFile } from '../users';
import { expect, test } from './guard';
import { forRole, id } from './pages';

// The checker sees every page, so it walks the whole list.
test.use({ storageState: authFile('checker') });

const narrow = ['tablet', 'phone'] as const;

// Controls a finger has to hit. Links inside running text are exempt (WCAG
// 2.5.8 allows inline targets); everything else must be 40x40 CSS px.
const TAP_TARGETS =
  'button, summary, select, textarea, input:not([type=hidden]):not([type=checkbox]):not([type=radio]), [role=button], .ui-btn, .shell-link, .ui-tab, .skip-link';

async function smallTargets(page: Page): Promise<string[]> {
  return page.evaluate((sel) => {
    const out: string[] = [];
    for (const el of document.querySelectorAll<HTMLElement>(sel)) {
      if (!el.checkVisibility({ checkVisibilityCSS: true }) || el.closest('dialog:not([open])')) continue;
      if (el.classList.contains('skip-link') || el.classList.contains('visually-hidden')) continue;
      const r = el.getBoundingClientRect();
      if (r.width < 39.5 || r.height < 39.5) {
        out.push(`${el.tagName.toLowerCase()}.${String(el.className).split(' ')[0]} "${(el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 24)}" ${Math.round(r.width)}x${Math.round(r.height)}`);
      }
    }
    // A checkbox or radio is hit through its label or its own box.
    for (const el of document.querySelectorAll<HTMLInputElement>('input[type=checkbox], input[type=radio]')) {
      if (!el.checkVisibility({ checkVisibilityCSS: true })) continue;
      const r = (el.closest('label') ?? el).getBoundingClientRect();
      const box = el.getBoundingClientRect();
      const w = Math.max(r.width, box.width);
      const h = Math.max(r.height, box.height);
      if (w < 39.5 || h < 39.5) out.push(`${el.type} "${(el.getAttribute('aria-label') || el.name).slice(0, 24)}" ${Math.round(w)}x${Math.round(h)}`);
    }
    return out;
  }, TAP_TARGETS);
}

for (const size of narrow) {
  test.describe(`at ${size} (${viewports[size].width}px)`, () => {
    test.use({ viewport: viewports[size] });

    test('no page scrolls sideways', async ({ page }) => {
      const wide: string[] = [];
      for (const p of forRole('checker')) {
        await page.goto(p.path);
        const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
        if (sw > cw) wide.push(`${p.name}: ${sw} > ${cw}`);
      }
      expect(wide).toEqual([]);
    });

    test('tables scroll inside their own container or stack', async ({ page }) => {
      const bad: string[] = [];
      for (const p of forRole('checker')) {
        await page.goto(p.path);
        const found = await page.evaluate(() =>
          [...document.querySelectorAll('table')]
            .filter((t) => t.checkVisibility({ checkVisibilityCSS: true }))
            .map((t) => {
              // Stacked: the table no longer lays out as a table.
              if (getComputedStyle(t).display !== 'table') return null;
              // Scrolling: some ancestor below <main> clips and scrolls sideways.
              for (let n = t.parentElement; n && n.tagName !== 'MAIN'; n = n.parentElement) {
                const ox = getComputedStyle(n).overflowX;
                if (ox === 'auto' || ox === 'scroll') return null;
              }
              const r = t.getBoundingClientRect();
              return `${t.getAttribute('aria-label') || t.querySelector('caption')?.textContent || 'table'} (${Math.round(r.width)}px)`;
            })
            .filter(Boolean),
        );
        for (const f of found) bad.push(`${p.name}: ${f}`);
      }
      expect(bad).toEqual([]);
    });

    test('the sidebar is a menu button that works with the keyboard', async ({ page }) => {
      await page.goto('/runs');
      const toggle = page.locator('summary.shell-menu-toggle');
      const nav = page.getByRole('navigation', { name: 'Main' });
      await expect(toggle).toBeVisible();
      await expect(nav).toBeHidden();
      await toggle.focus();
      await page.keyboard.press('Enter');
      await expect(nav).toBeVisible();
      await expect(nav.getByRole('link', { name: 'Findings', exact: true })).toBeVisible();
      await page.keyboard.press('Enter');
      await expect(nav).toBeHidden();
      await page.keyboard.press('Space');
      await expect(nav).toBeVisible();
      await page.keyboard.press('Space');
      await expect(nav).toBeHidden();
      // A link of the opened menu is reachable with Tab and activates with Enter.
      await page.keyboard.press('Enter');
      await page.keyboard.press('Tab');
      await expect(nav.getByRole('link').first()).toBeFocused();
    });

    test('tap targets are at least 40x40', async ({ page }) => {
      const small: string[] = [];
      for (const p of forRole('checker')) {
        await page.goto(p.path);
        // Open the menu so its links count.
        const toggle = page.locator('summary.shell-menu-toggle');
        if (await toggle.isVisible()) await toggle.click();
        for (const s of await smallTargets(page)) small.push(`${p.name}: ${s}`);
      }
      expect([...new Set(small)]).toEqual([]);
    });
  });
}

test.describe('phone', () => {
  test.use({ viewport: viewports.phone, storageState: authFile('maker') });

  test('the finding detail has no side-by-side layout', async ({ page }) => {
    for (const fid of [id.findClean, id.findEntry, id.findReview]) {
      await page.goto(`/findings/${fid}`);
      const rows = await page.evaluate(() => {
        // Two blocks that share a row (overlap vertically and sit at different
        // left edges) are side by side.
        const boxes = [...document.querySelectorAll<HTMLElement>('main section, main .ui-card, main dl > div, main .ui-stat, main .ui-card-body > *')]
          .filter((e) => e.checkVisibility({ checkVisibilityCSS: true }))
          .map((e) => ({ r: e.getBoundingClientRect(), name: `${e.tagName.toLowerCase()}.${String(e.className).split(' ')[0]}` }));
        const side: string[] = [];
        for (let i = 0; i < boxes.length; i++) {
          for (let j = i + 1; j < boxes.length; j++) {
            const a = boxes[i].r;
            const b = boxes[j].r;
            const overlapY = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top);
            const overlapX = Math.min(a.right, b.right) - Math.max(a.left, b.left);
            // Skip nesting (one contains the other) and stacked blocks.
            const nested = (a.left <= b.left + 1 && a.right >= b.right - 1 && a.top <= b.top + 1 && a.bottom >= b.bottom - 1) ||
              (b.left <= a.left + 1 && b.right >= a.right - 1 && b.top <= a.top + 1 && b.bottom >= a.bottom - 1);
            if (!nested && overlapY > 4 && overlapX < 1) side.push(`${boxes[i].name} | ${boxes[j].name}`);
          }
        }
        return side;
      });
      expect(rows, fid).toEqual([]);
    }
  });
});
