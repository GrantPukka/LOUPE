import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, '..', '..');

/** Where the generated logs live. Gitignored; safe to delete. */
export const FIXTURES = path.join(here, '..', '.playwright', 'logs');

/** The binary under test. */
export const BINARY = path.join(repo, 'loupe');

/**
 * Generate the log files the suite runs against, and check the binary the
 * webServer is about to launch actually exists.
 *
 * This is called while the Playwright config is being evaluated rather than
 * from globalSetup, because Playwright starts webServer first and globalSetup
 * second — the server would have nothing to open.
 *
 * A fixed seed makes the blaster's output byte-identical run to run, so an
 * assertion about a specific record stays stable. Four minutes of traffic is
 * enough to contain the incident and small enough to ingest instantly.
 */
export function ensureFixtures() {
  if (!existsSync(BINARY)) {
    throw new Error(
      `no loupe binary at ${BINARY}\n` +
        'Build it first — these tests drive the real one, not a mock:\n\n' +
        '  make web && make build\n',
    );
  }

  const args = [
    'run', './cmd/blaster',
    '-out', FIXTURES,
    '-seed', '42',
    '-scenario', 'incident',
    '-duration', '4m',
    '-rotate=false',
  ];

  // Regenerate whenever the recipe changes, not merely when the directory is
  // empty.
  //
  // CI always starts empty and so always generates fresh; a developer's
  // machine keeps whatever was there. When the two diverge, local runs test
  // different data from CI — which is how a suite passes here and fails there,
  // and the failure looks like flakiness rather than a stale fixture.
  const stamp = path.join(FIXTURES, '.recipe');
  const recipe = args.join(' ');

  if (existsSync(FIXTURES) && readdirSync(FIXTURES).length > 0) {
    if (existsSync(stamp) && readFileSync(stamp, 'utf8') === recipe) {
      return;
    }
    rmSync(FIXTURES, { recursive: true, force: true });
  }

  execFileSync('go', args, { cwd: repo, stdio: 'inherit' });
  writeFileSync(stamp, recipe);
}
