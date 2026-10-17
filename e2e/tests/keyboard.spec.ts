import type { Page } from '@playwright/test';
import { authFile, type Role } from '../users';
import { expect, test } from './guard';
import { id } from './pages';

// Pages walked with the Tab key, with the role that sees the most controls.
const walked: { name: string; path: string; role: Role }[] = [
  { name: 'dashboard', path: '/', role: 'checker' },
  { name: 'runs', path: '/runs', role: 'maker' },
  { name: 'run findings', path: `/runs/${id.runDone}/findings`, role: 'viewer' },
  { name: 'findings list', path: '/findings', role: 'maker' },
  { name: 'finding with actions', path: `/findings/${id.findEntry}`, role: 'maker' },
  { name: 'ask', path: '/ask', role: 'viewer' },
  { name: 'documents', path: '/documents', role: 'viewer' },
  { name: 'system', path: '/system', role: 'viewer' },
  { name: 'approvals', path: '/approvals', role: 'checker' },
  { name: 'audit', path: '/audit', role: 'checker' },
];

const INTERACTIVE =
  'a[href], button:not([disabled]), input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex="0"]';

// markInteractive tags the controls a keyboard user can reach with
// data-kb="<n>" in document order and returns how many there are.
async function markInteractive(page: Page, selector: string): Promise<number> {
  return page.evaluate((sel) => {
    const seenRadio = new Set<string>();
    const els = [...document.querySelectorAll<HTMLElement>(sel)].filter((e) => {
      if (!e.checkVisibility({ checkVisibilityCSS: true })) return false;
      if (e.closest('dialog:not([open]), [inert]')) return false;
      if (e instanceof HTMLInputElement && e.type === 'radio') {
        if (seenRadio.has(e.name)) return false;
        seenRadio.add(e.name);
      }
      return true;
    });
    els.forEach((e, i) => e.setAttribute('data-kb', String(i)));
    return els.length;
  }, selector);
}

// indicator reports how the focused element shows focus.
async function indicator(page: Page) {
  return page.evaluate(() => {
    const el = document.activeElement as HTMLElement;
    const toRGBA = (css: string): [number, number, number, number] => {
      const c = document.createElement('canvas').getContext('2d')!;
      c.clearRect(0, 0, 1, 1);
      c.fillStyle = css;
      c.fillRect(0, 0, 1, 1);
      const d = c.getImageData(0, 0, 1, 1).data;
      return [d[0], d[1], d[2], d[3] / 255];
    };
    const lum = ([r, g, b]: number[]) => {
      const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4));
      return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
    };
    const bgOf = (e: Element | null): [number, number, number] => {
      for (let n = e; n; n = n.parentElement) {
        const c = toRGBA(getComputedStyle(n).backgroundColor);
        if (c[3] > 0.99) return [c[0], c[1], c[2]];
      }
      return [255, 255, 255];
    };
    const cs = getComputedStyle(el);
    const width = parseFloat(cs.outlineWidth) || 0;
    const hasOutline = cs.outlineStyle !== 'none' && width > 0;
    const hasShadow = cs.boxShadow !== 'none';
    const col = toRGBA(hasOutline ? cs.outlineColor : '#000');
    const bg = bgOf(el.parentElement);
    const [l1, l2] = [lum(col), lum(bg)].sort((a, b) => b - a);
    return {
      label: `${el.tagName.toLowerCase()}${el.className ? '.' + String(el.className).split(' ')[0] : ''} "${(el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 30)}"`,
      width,
      hasOutline,
      hasShadow,
      ratio: (l1 + 0.05) / (l2 + 0.05),
      kb: el.getAttribute('data-kb'),
    };
  });
}

for (const w of walked) {
  test.describe(`tab order and focus ring: ${w.name}`, () => {
    test.use({ storageState: authFile(w.role) });
    test(`${w.name}`, async ({ page }) => {
      await page.goto(w.path);
      const total = await markInteractive(page, INTERACTIVE);
      expect(total).toBeGreaterThan(3);
      expect(await page.locator('[tabindex]:not([tabindex="0"]):not([tabindex="-1"])').count(), 'positive tabindex').toBe(0);

      const order: number[] = [];
      const weak: string[] = [];
      for (let i = 0; i < total + 12 && order.length < total; i++) {
        await page.keyboard.press('Tab');
        const ind = await indicator(page);
        if (ind.kb === null) continue; // the document itself or an untagged element
        const n = Number(ind.kb);
        if (order.length === 0) {
          // The first stop is the skip link.
          await expect(page.locator('a.skip-link')).toBeFocused();
        }
        if (order.at(-1) === n) continue;
        order.push(n);
        const visible = (ind.hasOutline && ind.width >= 2 && ind.ratio >= 3) || (ind.hasShadow && !ind.hasOutline);
        if (!visible) weak.push(`${ind.label}: outline ${ind.hasOutline ? ind.width + 'px' : 'none'}, ratio ${ind.ratio.toFixed(2)}`);
      }
      expect(order, 'every control is reached once, in document order').toEqual([...Array(total).keys()]);
      expect(weak, 'focus indicator').toEqual([]);
    });
  });
}

test.describe('dialogs', () => {
  test.use({ storageState: authFile('checker') });

  // A native modal dialog makes the page behind it inert. Tab past its last
  // control leaves the page for the browser's own controls (the document then
  // has no focused element) and comes back to the first control; focus never
  // lands on the page behind.
  const inside = (page: Page, sel: string) =>
    page.evaluate(
      (s) => {
        const a = document.activeElement;
        return !a || a === document.body || a.closest(s) !== null;
      },
      sel,
    );
  const trap = async (page: Page, dialogSel: string) => {
    for (let i = 0; i < 14; i++) {
      await page.keyboard.press('Tab');
      expect(await inside(page, dialogSel), `Tab ${i + 1} stays in the dialog`).toBe(true);
    }
    for (let i = 0; i < 4; i++) {
      await page.keyboard.press('Shift+Tab');
      expect(await inside(page, dialogSel), `Shift+Tab ${i + 1} stays in the dialog`).toBe(true);
    }
  };

  for (const [name, opener, sel] of [
    ['command palette', 'button.shell-palette-btn', '#ui-palette'],
    ['shortcuts', 'button.shell-shortcuts-btn', '#ui-shortcuts'],
  ] as const) {
    test(`${name} traps focus and gives it back`, async ({ page }) => {
      await page.goto('/');
      const btn = page.locator(opener);
      await btn.focus();
      await page.keyboard.press('Enter');
      await expect(page.locator(sel)).toBeVisible();
      await trap(page, sel);
      await page.keyboard.press('Escape');
      await expect(page.locator(sel)).toBeHidden();
      await expect(btn).toBeFocused();
    });
  }

  test('a confirmation dialog traps focus and gives it back', async ({ page }) => {
    await page.goto('/approvals');
    const opener = page.getByRole('button', { name: 'Reject' }).first();
    await opener.focus();
    await page.keyboard.press('Enter');
    const dlg = page.getByRole('dialog', { name: /Reject proposal/ });
    await expect(dlg).toBeVisible();
    await trap(page, 'dialog[open]');
    await page.keyboard.press('Escape');
    await expect(dlg).toBeHidden();
    await expect(opener).toBeFocused();
  });
});

test.describe('live regions', () => {
  test.use({ storageState: authFile('viewer') });

  test('toasts and announcements', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#ui-toasts')).toHaveAttribute('aria-live', 'polite');
    await expect(page.locator('#ui-announce')).toHaveAttribute('aria-live', 'polite');
  });

  test('run progress', async ({ page }) => {
    await page.goto(`/runs/${id.runQueued}`);
    const panel = page.locator('#run-progress');
    await expect(panel).toBeVisible();
    // The panel is swapped whole, so the live region is the status line inside
    // it (role=status is a polite live region), not the replaced wrapper.
    await expect(panel.locator('[role=status]')).toContainText('Current step');
  });
});
