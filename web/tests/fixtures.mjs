import { execFileSync } from 'node:child_process';
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, '..', '..');

/** Where the generated logs live. Gitignored; safe to delete. */
export const FIXTURES = path.join(here, '..', '.playwright', 'logs');

/**
 * Where the single merged log lives, for the tests in merged.spec.js.
 *
 * A directory of one file, because that is the shape the bug was about: every
 * format in one stream, which is what journalctl, a Kubernetes collector, and
 * `cat *.log > combined.log` all produce, and which the "point loupe at the
 * directory" remedy cannot help with.
 */
export const MERGED = path.join(here, '..', '.playwright', 'merged');

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

/**
 * The id that appears once as a correlation_id field and five times as plain
 * text. `loupe trace` has to find all six.
 */
export const TRACE_ID = 'req-7f3c9a2e';

/** How many of those six carry the id in a field rather than in their text. */
export const TRACE_FIELD_HOPS = 1;
export const TRACE_TEXT_HOPS = 5;

/**
 * A nonsense token spelled three ways, so smart case can be asserted exactly
 * rather than against whatever the generated traffic happens to contain.
 *
 * All-lowercase matches all three; a spelling with a capital in it matches only
 * itself. The lowercase spelling is the one that is supposed to hang.
 */
export const CASE_TOKEN = {
  lower: 'zebrafish',
  mixed: 'Zebrafish',
  upper: 'ZEBRAFISH',
  /** Matches for the lowercase spelling: every variant. */
  insensitiveMatches: 3,
  /** Matches for a spelling carrying a capital: itself alone. */
  sensitiveMatches: 1,
};

/** The fields-bag key holding the original bytes of a repaired line. */
export const RAW_HEX_FIELD = 'loupe_raw_hex';

/**
 * Build the merged log the mixed-format tests run against.
 *
 * It reuses the files ensureFixtures already generated rather than running the
 * blaster twice, interleaves them with a seeded shuffle, and plants the two
 * failures the bug report was about:
 *
 *   - one line of invalid UTF-8, which used to abort the whole ingest and take
 *     every other line in the file down with it;
 *   - a correlation id that is a field on one record and plain text on five,
 *     which used to be reported as absent entirely.
 *
 * Deterministic throughout: same inputs, byte-identical output, so an assertion
 * about an exact count stays true. Assembled as buffers because one of these
 * lines is deliberately not valid text and must reach disk intact.
 */
export function ensureMergedFixture() {
  const recipe = 'merged v1 seed 11';
  const stamp = path.join(MERGED, '.recipe');
  const target = path.join(MERGED, 'platform-mixed.log');

  if (existsSync(stamp) && readFileSync(stamp, 'utf8') === recipe && existsSync(target)) {
    return;
  }

  rmSync(MERGED, { recursive: true, force: true });
  mkdirSync(MERGED, { recursive: true });

  const pools = readdirSync(FIXTURES)
    .filter((name) => !name.startsWith('.') && name !== 'manifest.json')
    .sort()
    .map((name) => splitLines(readFileSync(path.join(FIXTURES, name))))
    .filter((lines) => lines.length > 0);

  if (pools.length < 3) {
    throw new Error(`expected several formats in ${FIXTURES}, found ${pools.length}`);
  }

  const rand = mulberry32(11);
  const cursors = new Array(pools.length).fill(0);
  const lines = [];

  // Draw from a random pool each time and cycle within it, so every format is
  // represented throughout the file rather than in one block at the top.
  const total = pools.reduce((n, pool) => n + pool.length, 0);
  for (let i = 0; i < total; i++) {
    const pick = Math.floor(rand() * pools.length);
    const pool = pools[pick];
    lines.push(pool[cursors[pick] % pool.length]);
    cursors[pick]++;
  }

  splice(lines, Math.floor(total * 0.6), [invalidUTF8Line()]);
  splice(lines, Math.floor(total * 0.3), traceLines());
  splice(lines, Math.floor(total * 0.15), caseLines());

  writeFileSync(target, Buffer.concat(lines.map((line) => Buffer.concat([line, NEWLINE]))));
  writeFileSync(stamp, recipe);
}

const NEWLINE = Buffer.from('\n');

/** Insert lines at a position, clamped into range. */
function splice(lines, at, inserted) {
  lines.splice(Math.min(Math.max(at, 0), lines.length), 0, ...inserted);
}

/**
 * A logfmt record carrying bytes that are not valid UTF-8, of the kind a
 * service emits when it logs a payload it failed to decode.
 *
 * 0x9c, 0xff and 0xfe are not legal anywhere in UTF-8, so this is unambiguous
 * rather than merely unusual.
 */
function invalidUTF8Line() {
  return Buffer.concat([
    Buffer.from('ts=2026-08-13T21:02:00Z level=error service=search-svc msg="decode failed" raw="'),
    Buffer.from([0x9c, 0xff, 0xfe]),
    Buffer.from('"'),
  ]);
}

/** One record carrying the id as a field, and five that only mention it. */
function traceLines() {
  const out = [
    Buffer.from(
      JSON.stringify({
        ts: '2026-08-13T21:30:00Z',
        level: 'error',
        msg: 'payment declined',
        correlation_id: TRACE_ID,
      }),
    ),
  ];

  for (let i = 0; i < TRACE_TEXT_HOPS; i++) {
    out.push(
      Buffer.from(`2026-08-13 21:30:0${i} ERROR gateway upstream failed for ${TRACE_ID} retrying`),
    );
  }
  return out;
}

/** The same token in three cases, for smart case. */
function caseLines() {
  return [CASE_TOKEN.upper, CASE_TOKEN.mixed, CASE_TOKEN.lower].map((spelling, i) =>
    Buffer.from(`ts=2026-08-13T21:1${i}:00Z level=warn service=case-test msg="${spelling} sighted"`),
  );
}

/** Split a file into lines, dropping a trailing empty one. */
function splitLines(buf) {
  const out = [];
  let start = 0;

  for (let i = 0; i < buf.length; i++) {
    if (buf[i] === 0x0a) {
      out.push(buf.subarray(start, i));
      start = i + 1;
    }
  }
  if (start < buf.length) out.push(buf.subarray(start));
  return out;
}

/** A small seeded PRNG, so the shuffle is the same on every machine. */
function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
