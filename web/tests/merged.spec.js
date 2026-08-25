import { test, expect } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { CASE_TOKEN, RAW_HEX_FIELD, TRACE_ID, TRACE_TEXT_HOPS } from './fixtures.mjs';

// The UI against one file holding every format at once, with two damaged
// things planted in it: a line of invalid UTF-8, and a correlation id that is a
// field on one record and plain text on five.
//
// This is the shape a merged stream has — journalctl, a Kubernetes collector,
// `cat *.log > combined.log` — and the shape that used to defeat loupe outright.
// A file-level parser choice left most of such a file unparsed and therefore
// off the timeline; one bad byte in it aborted the ingest of every other line.
//
// These run against their own server on their own corpus, so the record counts
// the other specs assert cannot move under them.

const filterBox = '.filterbar input';
const rows = '.rows .row';
const footer = 'footer';

/**
 * Screenshots, when asked for.
 *
 * Set LOUPE_SHOTS to a directory to regenerate the images in
 * docs/verification/. Off by default: a test suite that writes into the docs
 * on every run makes every run look like a change.
 */
const SHOTS = process.env.LOUPE_SHOTS;
if (SHOTS) mkdirSync(SHOTS, { recursive: true });

async function shot(page, name) {
  if (!SHOTS) return;
  await page.screenshot({ path: path.join(SHOTS, `${name}.png`) });
}

/** Run a filter through the same API the UI calls, and return what it matched. */
async function count(page, filter) {
  const res = await page.request.post('/api/query', {
    data: { filter, limit: 1 },
  });
  expect(res.ok(), `${filter} was rejected: ${await res.text()}`).toBeTruthy();
  return (await res.json()).total;
}

/**
 * Type a filter and wait for the result that actually belongs to it.
 *
 * The applied-term chip appears as soon as the query is accepted, which is
 * before its rows arrive. Waiting on the chip alone leaves the previous result
 * set on screen — fine for an assertion about the chip, wrong for one about the
 * rows, and actively misleading in a screenshot.
 */
async function applyFilter(page, filter, total) {
  await page.fill(filterBox, filter);
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 30_000 });
  await expect(page.locator(footer)).toContainText(`of ${total} records`, { timeout: 30_000 });
}

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 60_000 });
});

// The baseline: this file could not be opened at all. One line of invalid UTF-8
// aborted the ingest with a DuckDB appender error and produced no records.
test('a file containing invalid UTF-8 still loads', async ({ page }) => {
  await expect(page.locator(rows).first()).toBeVisible();
  expect(await page.locator(rows).count()).toBeGreaterThan(0);

  await shot(page, '01-loads');
});

// Repairing the line must not lose it. The bytes are kept, hex-encoded, in the
// fields bag — findable, and one query away from being decoded.
test('the repaired line keeps its original bytes', async ({ page }) => {
  expect(await count(page, `${RAW_HEX_FIELD}:*`)).toBe(1);

  // The hex lives in the fields bag rather than in a column of its own, so it
  // costs nothing on the records that are fine and needs no schema change. That
  // also means selecting it by name would not bind — take the bag and read it.
  const res = await page.request.post('/api/query', {
    data: { filter: `${RAW_HEX_FIELD}:*`, limit: 1, columns: 'raw, fields' },
  });
  expect(res.ok(), await res.text()).toBeTruthy();

  const { columns, rows: got } = await res.json();
  const raw = got[0][columns.indexOf('raw')];
  const fields = JSON.parse(got[0][columns.indexOf('fields')]);

  expect(raw, 'the stored text should carry replacement characters').toContain('�');

  // Round-tripping the hex has to give back the line as it was written, bad
  // bytes and all. Nothing is lost; it is only no longer pretending to be text.
  const original = Buffer.from(fields[RAW_HEX_FIELD], 'hex');
  expect(original.toString('binary')).toContain('decode failed');
  expect(original.includes(Buffer.from([0x9c, 0xff, 0xfe]))).toBeTruthy();
});

// The worst of them: lowercase is what people type, and it used to pin two
// cores and never return. The corpus contains the token in three cases, so the
// smart-case rule can be asserted exactly rather than against whatever the
// generated traffic happens to hold.
test('a lowercase search returns, and ignores case', async ({ page }) => {
  const started = Date.now();
  const matched = await count(page, CASE_TOKEN.lower);
  const elapsed = Date.now() - started;

  expect(matched).toBe(CASE_TOKEN.insensitiveMatches);
  expect(elapsed, 'a lowercase term used to never return at all').toBeLessThan(10_000);

  await applyFilter(page, CASE_TOKEN.lower, matched);
  await shot(page, '02-lowercase-search');
});

test('a term carrying a capital stays case-sensitive', async ({ page }) => {
  expect(await count(page, CASE_TOKEN.mixed)).toBe(CASE_TOKEN.sensitiveMatches);
  expect(await count(page, CASE_TOKEN.upper)).toBe(CASE_TOKEN.sensitiveMatches);

  await applyFilter(page, CASE_TOKEN.upper, CASE_TOKEN.sensitiveMatches);
  await shot(page, '03-uppercase-search');
});

// One file, one parser, and 84.5% of the records off the timeline. Detection is
// per line now, so the file reports the formats it actually contains.
test('one file reports the several formats it contains', async ({ page }) => {
  const res = await page.request.get('/api/sources');
  expect(res.ok()).toBeTruthy();

  const sources = (await res.json()).sources ?? (await res.json());
  const formats = new Set(sources.map((s) => s.format));
  const files = new Set(sources.map((s) => s.file));

  expect(files.size, 'the corpus is deliberately a single file').toBe(1);
  expect(formats.size).toBeGreaterThan(3);

  await shot(page, '05-sources');
});

// Most of a merged file has to reach the timeline, or the tool is a text search
// wearing a log explorer's clothes.
test('almost every record is parsed and dated', async ({ page }) => {
  const res = await page.request.get('/api/schema');
  const { records, unparsed } = await res.json();

  expect(records).toBeGreaterThan(0);
  expect(unparsed / records, `${unparsed} of ${records} unparsed`).toBeLessThan(0.1);
});

// A field carried by only some of the formats in the file. Before per-line
// detection it did not exist, and this was an unknown-field error rather than a
// filter.
test('a field from one format inside the file is filterable', async ({ page }) => {
  const matched = await count(page, 'status:>=500');
  expect(matched).toBeGreaterThan(0);

  await applyFilter(page, 'status:>=500', matched);
  await shot(page, '04-status-filter');
});

// Detection used to pick the best-covered correlation field and then report
// that no record carried the id — a confidently wrong answer to a question it
// had all the information to answer.
test('a trace follows the field that holds the id, not the best-covered one', async ({ page }) => {
  const res = await page.request.get(`/api/trace?id=${TRACE_ID}`);
  expect(res.ok()).toBeTruthy();

  const trace = await res.json();
  expect(trace.field).toBe('correlation_id');
});

// Five of the six lines mention the id in their text without carrying it as a
// field. A trace that lists only the sixth reads as though the rest never
// happened.
test('a trace includes the records that only mention the id in their text', async ({ page }) => {
  const res = await page.request.get(`/api/trace?id=${TRACE_ID}`);
  const trace = await res.json();

  expect(trace.hops.length).toBe(TRACE_TEXT_HOPS + 1);
  expect(trace.text_only, 'the looser match has to be countable').toBe(TRACE_TEXT_HOPS);
});

// The rest of the screen still works on a corpus of this shape — and the record
// that used to abort the whole ingest is a record like any other, openable, with
// the format that read it and the bytes it arrived as both on screen.
test('the timeline, sources and the repaired record all render', async ({ page }) => {
  await expect(page.locator('.hist .bar').first()).toBeVisible();
  await expect(page.locator('.sources .chip').first()).toBeVisible();

  await applyFilter(page, `${RAW_HEX_FIELD}:*`, 1);
  await page.locator(rows).first().click();

  const detail = page.locator('.detail');
  await expect(detail).toBeVisible();
  await expect(detail).toContainText(RAW_HEX_FIELD);
  // Per-line detection recognised it, rather than leaving it as unparsed text.
  await expect(detail).toContainText('logfmt');

  await shot(page, '06-record-detail');
});
