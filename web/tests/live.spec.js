import { test, expect } from '@playwright/test';
import { appendFileSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { FIXTURES } from './fixtures.mjs';

// The live path, end to end: a line written to a file on disk, through the
// follower and the SSE endpoint, into the table without a reload.
//
// Serial, and against a log file of its own. The suite runs fully parallel and
// these tests write to the fixture directory the other tests are reading, so a
// shared file would make record counts elsewhere move under them.
test.describe.configure({ mode: 'serial' });

const LOG = path.join(FIXTURES, 'live-test.log');
const filterBox = '.filterbar input';
const liveButton = '.filterbar .live';
const rows = '.rows .row';

/** A record only this test writes, so finding it proves it streamed. */
function line(message, at) {
  return JSON.stringify({
    ts: at,
    level: 'error',
    msg: message,
    service: 'live-test',
    status: 503,
  });
}

// Stamps are ahead of the generated fixtures, so a streamed record sorts to
// the top of a newest-first list where the assertions can see it.
const base = new Date(Date.now() + 60_000);
const stamp = (n) => new Date(base.getTime() + n * 1000).toISOString();

test.beforeAll(() => {
  // The file has to exist before the page loads: a source that appears later
  // is picked up too, but that is a different behaviour and has its own test
  // in the Go suite.
  writeFileSync(LOG, `${line('live fixture opened', stamp(0))}\n`);
});

test.afterAll(() => {
  rmSync(LOG, { force: true });
});

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });
});

test('live is off until asked for', async ({ page }) => {
  await expect(page.locator(liveButton)).toHaveText('○ live');
  await expect(page.locator('footer .live-mark')).toHaveCount(0);

  await page.fill(filterBox, 'source:live-test');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });

  const before = await page.locator(rows).count();
  appendFileSync(LOG, `${line('written while not following', stamp(1))}\n`);

  // Nothing is watching, so nothing should change. This is the invariant that
  // keeps an open tab from polling somebody's log directory unasked.
  await page.waitForTimeout(2000);
  expect(await page.locator(rows).count()).toBe(before);
});

test('a record written after the page loaded appears without a reload', async ({ page }) => {
  await page.fill(filterBox, 'source:live-test');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });

  await page.locator(liveButton).click();
  await expect(page.locator(liveButton)).toHaveText('● live');
  await expect(page.locator('footer .live-mark')).toHaveText('● following');

  const message = `streamed at ${Date.now()}`;
  appendFileSync(LOG, `${line(message, stamp(2))}\n`);

  // Newest first, so an arrival belongs at the top.
  await expect(page.locator(`${rows} .c-msg`).first()).toContainText(message, {
    timeout: 20_000,
  });

  // And exactly once: the follower re-reads the boundary record to complete
  // it, so a stream that did not exclude it would show every line twice.
  await page.waitForTimeout(1500);
  await expect(page.locator(rows, { hasText: message })).toHaveCount(1);
});

test('the filter applies to streamed records too', async ({ page }) => {
  await page.fill(filterBox, 'source:live-test level:error');
  await expect(page.locator('.terms .term')).toHaveCount(2, { timeout: 20_000 });

  await page.locator(liveButton).click();
  await expect(page.locator(liveButton)).toHaveText('● live');

  const ignored = `filtered out ${Date.now()}`;
  const kept = `kept ${Date.now()}`;

  appendFileSync(
    LOG,
    `${JSON.stringify({ ts: stamp(3), level: 'info', msg: ignored, service: 'live-test' })}\n` +
      `${line(kept, stamp(4))}\n`,
  );

  await expect(page.locator(`${rows} .c-msg`).first()).toContainText(kept, { timeout: 20_000 });
  await expect(page.locator(rows, { hasText: ignored })).toHaveCount(0);
});

test('scrolling away holds new records instead of moving the list', async ({ page }) => {
  // No filter, so the list is long enough to scroll.
  await page.locator(liveButton).click();
  await expect(page.locator(liveButton)).toHaveText('● live');

  await page.locator('.rows').evaluate((el) => {
    el.scrollTop = 400;
  });

  const message = `held ${Date.now()}`;
  appendFileSync(LOG, `${line(message, stamp(5))}\n`);

  // Offered, not inserted: shuffling rows under a reader mid-incident is how
  // they lose the line they were looking at.
  const banner = page.locator('.pending');
  await expect(banner).toBeVisible({ timeout: 20_000 });
  await expect(banner).toContainText('new record');
  await expect(page.locator(rows, { hasText: message })).toHaveCount(0);

  await banner.click();
  await expect(banner).toHaveCount(0);
  await expect(page.locator(`${rows} .c-msg`).first()).toContainText(message);
});
