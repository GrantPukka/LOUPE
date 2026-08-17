import { defineConfig, devices } from '@playwright/test';
import { ensureFixtures, FIXTURES, BINARY } from './tests/fixtures.mjs';

// Generated here, not in globalSetup: Playwright starts webServer before
// globalSetup runs, so anything the server needs has to exist by now.
ensureFixtures();

// A port of its own, so a `loupe serve` you already have open on the default
// 7717 is neither used by accident nor killed by the run.
const PORT = 7799;
const BASE = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? 'list' : [['list'], ['html', { open: 'never' }]],

  use: {
    baseURL: BASE,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },

  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],

  // The binary under test, serving the generated fixtures. This is the real
  // embedded UI talking to the real Go API over the real query path — the same
  // rule the UI itself lives under, applied to its tests.
  //
  // --no-cache because a fixture directory regenerated between runs would
  // otherwise be served from a stale ingest.
  webServer: {
    command: `"${BINARY}" serve "${FIXTURES}" --addr 127.0.0.1:${PORT} --no-cache --quiet`,
    url: `${BASE}/api/health`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
