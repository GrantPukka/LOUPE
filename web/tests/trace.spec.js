import { test, expect } from '@playwright/test';

// The trace view, against the real binary and a real ingest. The demo corpus's
// shared trace_id is the fixture, which is the point of it existing.

const rows = '.rows .row';
const filterBox = '.filterbar input';

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });
});

// Every payment-worker record that carries a trace id belongs to a trace that
// also touched auth-svc and checkout-api, so this reliably opens one with hops
// to measure between. A bare trace_id:* would often land on a single-hop trace
// and there would be no wait to draw.
const MULTI_HOP = 'source:payment-worker trace_id:*';

/** Open a record from a trace that crossed several services, and return its id. */
async function openATrace(page) {
  await page.fill(filterBox, MULTI_HOP);
  await expect(page.locator('.terms .term')).toHaveCount(2, { timeout: 20_000 });

  await page.locator(rows).first().click();

  const button = page.locator('.v-trace').first();
  await expect(button).toBeVisible({ timeout: 20_000 });

  const id = await page
    .locator('.detail .kv', { has: page.locator('.v-trace') })
    .locator('.v')
    .first()
    .textContent();

  await button.click();
  await expect(page.locator('.modal.trace')).toBeVisible({ timeout: 20_000 });
  // The hops arrive after the panel does; wait for it to have settled.
  await expect(page.locator('.rail-loading')).toHaveCount(0, { timeout: 20_000 });

  return id.trim();
}

// The affordance appears on the correlation field, and nowhere else.
test('the trace button is offered on the correlation field only', async ({ page }) => {
  await page.fill(filterBox, 'trace_id:*');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });
  await page.locator(rows).first().click();

  await expect(page.locator('.detail')).toBeVisible({ timeout: 20_000 });

  // Exactly one field in the record detail offers it.
  await expect(page.locator('.detail .v-trace')).toHaveCount(1);

  const labelled = page.locator('.detail .kv', { has: page.locator('.v-trace') }).locator('.k');
  await expect(labelled).toHaveText('trace_id');
});

test('clicking it opens the request timeline', async ({ page }) => {
  const id = await openATrace(page);

  await expect(page.locator('.modal-head')).toContainText(id);
  await expect(page.locator('.modal-head')).toContainText('trace_id');

  // At least one hop, each with a source.
  const hops = page.locator('.hop');
  await expect(hops.first()).toBeVisible();
  expect(await hops.count()).toBeGreaterThan(0);
  await expect(hops.first().locator('.hop-src')).not.toBeEmpty();
});

// A trace is a claim about where a request went, so what could not be checked
// has to be on screen with it.
test('the timeline states which sources could not answer', async ({ page }) => {
  await openATrace(page);

  const footer = page.locator('.trace-foot');
  await expect(footer).toBeVisible();
  await expect(footer).toContainText('never record trace_id');
  await expect(footer).toContainText('cannot say whether the request reached them');
});

// The waits are the finding, so they are drawn rather than only listed.
test('the waits between hops are shown', async ({ page }) => {
  await openATrace(page);

  const gaps = await page.locator('.hop-gap').allTextContents();
  const measured = gaps.filter((g) => g.trim().startsWith('+'));
  expect(measured.length).toBeGreaterThan(0);

  // The longest wait is marked, and only one is.
  await expect(page.locator('.hop.slow')).toHaveCount(1);
  await expect(page.locator('.trace-foot')).toContainText('Span');
});

test('the trace closes and can hand its records back to the table', async ({ page }) => {
  const id = await openATrace(page);

  await page.locator('.modal.trace .clear', { hasText: 'show these records' }).click();

  await expect(page.locator('.modal.trace')).toHaveCount(0);
  await expect(page.locator(filterBox)).toHaveValue(`trace_id:${id}`);
});

test('escape closes the trace', async ({ page }) => {
  await openATrace(page);

  await page.keyboard.press('Escape');
  await expect(page.locator('.modal.trace')).toHaveCount(0);

  // And the filter behind it is untouched.
  await expect(page.locator(filterBox)).toHaveValue(MULTI_HOP);
});
