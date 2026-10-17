import path from 'node:path';
import { defineConfig } from '@playwright/test';

export const PORT = 8099;
export const BASE_URL = `http://127.0.0.1:${PORT}`;

// The three viewports of the suite. Specs pick one with test.use({ viewport }).
export const viewports = {
  desktop: { width: 1280, height: 800 },
  tablet: { width: 820, height: 1180 },
  phone: { width: 390, height: 844 },
} as const;

export default defineConfig({
  testDir: './tests',
  // One server holds the state, so specs run one after the other. File names
  // order them: the read-only specs first, the walkthroughs (which write) last.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 7_000 },
  reporter: [['list'], ['html', { open: 'never' }]],
  globalSetup: './global-setup.ts',
  use: {
    baseURL: BASE_URL,
    viewport: viewports.desktop,
    browserName: 'chromium',
    trace: 'retain-on-failure',
    // Same-origin only: the suite fails on any other request.
    acceptDownloads: false,
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
  webServer: {
    // From the repository root, so the module resolves.
    command: 'go run ./e2e/cmd/e2eserver',
    cwd: path.resolve(__dirname, '..'),
    url: `${BASE_URL}/login`,
    env: { E2E_ADDR: `127.0.0.1:${PORT}` },
    reuseExistingServer: false,
    timeout: 120_000,
    stdout: 'ignore',
    stderr: 'ignore',
  },
});
