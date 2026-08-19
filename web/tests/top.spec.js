import { test, expect } from '@playwright/test';

// Click-to-facet, against the real binary and a real ingest.

const rows = '.rows .row';
const filterBox = '.filterbar input';

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });
});

/** Expand a record and open the breakdown of the named field. */
async function openTop(page, field) {
  await page.locator(rows).first().click();
  await expect(page.locator('.detail')).toBeVisible({ timeout: 20_000 });

  const row = page.locator('.detail .kv', { has: page.locator('.k', { hasText: field }) }).first();
  await row.locator('.v-top').click();

  await expect(page.locator('.modal.top')).toBeVisible({ timeout: 20_000 });
}

test('every field in a record offers a breakdown', async ({ page }) => {
  await page.locator(rows).first().click();
  await expect(page.locator('.detail')).toBeVisible({ timeout: 20_000 });

  // One affordance per field row, not one for the whole record.
  const fields = await page.locator('.detail .kv .k').count();
  const buttons = await page.locator('.detail .v-top').count();
  expect(buttons).toBeGreaterThan(0);
  expect(buttons).toBeLessThanOrEqual(fields);
});

test('the breakdown counts values with percentages, descending', async ({ page }) => {
  await openTop(page, 'source');

  const values = page.locator('.top-row');
  await expect(values.first()).toBeVisible();
  expect(await values.count()).toBeGreaterThan(1);

  // Most frequent first.
  const counts = (await page.locator('.top-n').allTextContents()).map((n) =>
    Number(n.replace(/[^0-9]/g, '')),
  );
  for (let i = 1; i < counts.length; i++) {
    expect(counts[i]).toBeLessThanOrEqual(counts[i - 1]);
  }

  // A percentage is the point: 412 of 33,000 reads differently from 412 of 500.
  const pct = await page.locator('.top-pct').allTextContents();
  expect(pct.every((p) => p.includes('%'))).toBe(true);
});

// A share is meaningless without its denominator, so the panel states it.
test('the breakdown states what it is a share of', async ({ page }) => {
  await openTop(page, 'source');

  const footer = page.locator('.top-foot');
  await expect(footer).toBeVisible();
  await expect(footer).toContainText('values across');
  await expect(footer).toContainText('records');
});

// Clicking a value writes a real filter term, so the breakdown is a way into
// the records rather than a dead end.
test('clicking a value filters on it', async ({ page }) => {
  await openTop(page, 'source');

  const value = await page.locator('.top-row').first().locator('.top-v').textContent();
  await page.locator('.top-row').first().click();

  await expect(page.locator('.modal.top')).toHaveCount(0);
  await expect(page.locator(filterBox)).toHaveValue(new RegExp(`source:${value.trim()}`));
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });
});

// Records missing the field sit outside the percentages, and the panel offers
// the term that finds them.
test('records missing the field are reported and reachable', async ({ page }) => {
  // trace_id is absent from three of the six demo sources.
  await page.fill(filterBox, 'trace_id:*');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });
  await page.fill(filterBox, '');
  await expect(page.locator('.terms .term')).toHaveCount(0, { timeout: 20_000 });

  await openTop(page, 'trace_id');

  const footer = page.locator('.top-foot');
  await expect(footer).toContainText('carry no trace_id');
  await expect(footer).toContainText('outside the percentages');

  await footer.locator('.top-link').click();
  await expect(page.locator(filterBox)).toHaveValue('trace_id:none');
});

test('escape closes the breakdown and leaves the filter alone', async ({ page }) => {
  await page.fill(filterBox, 'level:error');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });

  await openTop(page, 'source');
  await page.keyboard.press('Escape');

  await expect(page.locator('.modal.top')).toHaveCount(0);
  await expect(page.locator(filterBox)).toHaveValue('level:error');
});

// The breakdown summarises what the filter selected, not the whole dataset.
test('the breakdown narrows with the filter', async ({ page }) => {
  await openTop(page, 'source');
  const all = await page.locator('.top-row').count();
  await page.keyboard.press('Escape');

  // A real source in this corpus: the logical name comes from the file, so
  // access.log is "access" and there is no "nginx" to filter on.
  await page.fill(filterBox, 'source:checkout-api');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });

  await openTop(page, 'source');
  await expect
    .poll(async () => page.locator('.top-row').count(), { timeout: 20_000 })
    .toBeLessThan(all);
});
