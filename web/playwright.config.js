import { defineConfig, devices } from '@playwright/test';
import { ensureFixtures, ensureMergedFixture, FIXTURES, MERGED, BINARY } from './tests/fixtures.mjs';

// Generated here, not in globalSetup: Playwright starts webServer before
// globalSetup runs, so anything the server needs has to exist by now.
ensureFixtures();
ensureMergedFixture();

// Ports of their own, so a `loupe serve` you already have open on the default
// 7717 is neither used by accident nor killed by the run.
const PORT = 7799;
const BASE = `http://127.0.0.1:${PORT}`;

// The merged corpus gets a server of its own rather than another file in the
// directory above. Dropping a single log holding every format into FIXTURES
// would move every record count the other specs assert, and those assertions
// would start failing for a reason that has nothing to do with what they test.
const MERGED_PORT = 7801;
const MERGED_BASE = `http://127.0.0.1:${MERGED_PORT}`;

const MERGED_SPEC = /merged\.spec\.js/;

/** One server per corpus. Both run the real binary over the real query path. */
const serve = (dir, port, base) => ({
  command: `"${BINARY}" serve "${dir}" --addr 127.0.0.1:${port} --no-cache --quiet`,
  url: `${base}/api/health`,
  reuseExistingServer: !process.env.CI,
  timeout: 120_000,
  stdout: 'pipe',
  stderr: 'pipe',
});

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

  projects: [
    {
      name: 'chromium',
      testIgnore: MERGED_SPEC,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      // The mixed-format corpus, on its own server.
      name: 'merged',
      testMatch: MERGED_SPEC,
      use: { ...devices['Desktop Chrome'], baseURL: MERGED_BASE },
    },
  ],

  // The binaries under test, serving the generated fixtures. This is the real
  // embedded UI talking to the real Go API over the real query path — the same
  // rule the UI itself lives under, applied to its tests.
  //
  // --no-cache because a fixture directory regenerated between runs would
  // otherwise be served from a stale ingest.
  webServer: [serve(FIXTURES, PORT, BASE), serve(MERGED, MERGED_PORT, MERGED_BASE)],
});
