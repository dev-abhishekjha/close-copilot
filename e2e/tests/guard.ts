import { test as base, expect } from '@playwright/test';

// Every test runs behind a guard that collects what must never happen: a
// Content-Security-Policy violation, a console error, an uncaught page error,
// a failed same-origin request, or a request to another origin. The guard
// fails the test at the end if it saw any.
export const test = base.extend<{ guard: string[] }>({
  guard: [
    async ({ page, baseURL }, use) => {
      const problems: string[] = [];
      const origin = new URL(baseURL as string).origin;
      await page.addInitScript(() => {
        document.addEventListener('securitypolicyviolation', (e) => {
          console.error(`CSP violation: ${e.violatedDirective} blocked ${e.blockedURI}`);
        });
      });
      page.on('console', (m) => {
        const text = m.text();
        if (m.type() === 'error' || /content security policy/i.test(text)) {
          problems.push(`console ${m.type()}: ${text} (${m.location().url})`);
        }
      });
      page.on('pageerror', (e) => problems.push(`page error: ${e.message}`));
      page.on('request', (r) => {
        const u = new URL(r.url());
        if ((u.protocol === 'http:' || u.protocol === 'https:') && u.origin !== origin) {
          problems.push(`request to another origin: ${r.url()}`);
        }
      });
      await use(problems);
      expect(problems, 'console errors, CSP violations or foreign requests').toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };
