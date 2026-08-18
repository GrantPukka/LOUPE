import { test, expect } from '@playwright/test';

// The pattern rail, against the real binary and a real ingest. Nothing mocked,
// for the same reason the Go tests use a real DuckDB: a UI test against a fake
// API passes while the query path is broken.

const filterBox = '.filterbar input';
const rows = '.rows .row';
const railToggle = '.filterbar .clear:has-text("patterns")';
const railItem = '.rail-item';

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });
});

// The rail costs a grouping query and takes width from the message column, so
// it stays off until asked for.
test('the rail is off until asked for', async ({ page }) => {
  await expect(page.locator('.rail')).toHaveCount(0);

  await page.locator(railToggle).click();
  await expect(page.locator('.rail')).toBeVisible();

  await page.locator('.rail-x').click();
  await expect(page.locator('.rail')).toHaveCount(0);
});

// The rail's own head is its column header. A second "patterns" label above it
// read as two stacked headers.
test('the rail heads its own column, once', async ({ page }) => {
  await page.locator(railToggle).click();
  await expect(page.locator('.rail-head')).toBeVisible();

  await expect(page.locator('.colhead')).not.toContainText('patterns');
  await expect(page.locator('.rail-head')).toContainText('patterns');

  // And the record list keeps its own columns.
  await expect(page.locator('.colhead')).toContainText('message');
});

test('the rail groups messages into templates with counts', async ({ page }) => {
  await page.locator(railToggle).click();

  const items = page.locator(railItem);
  await expect(items.first()).toBeVisible({ timeout: 20_000 });

  // The demo corpus is six formats of repetitive traffic, so grouping it must
  // produce far fewer templates than records.
  const count = await items.count();
  expect(count).toBeGreaterThan(1);

  // Most frequent first.
  const numbers = await page.locator('.rail-n').allTextContents();
  const parsed = numbers.map((n) => Number(n.replace(/[^0-9]/g, '')));
  for (let i = 1; i < parsed.length; i++) {
    expect(parsed[i]).toBeLessThanOrEqual(parsed[i - 1]);
  }

  // A masked template proves the collapse rule ran rather than the messages
  // happening to be identical.
  const templates = await page.locator('.rail-t').allTextContents();
  expect(templates.some((t) => t.includes('<'))).toBe(true);

  // Nothing unrenderable reaches the screen. The corpus is full of lines the
  // blaster corrupted with NUL bytes, and a NUL draws as a replacement box,
  // which reads as a broken font rather than a broken log line.
  for (const t of templates) {
    // eslint-disable-next-line no-control-regex
    expect(t).not.toMatch(/[\x00-\x08\x0b-\x1f\x7f]/);
  }
});

// Clicking a template writes a real DSL term into the filter box — the same
// principle as the timeline drag, so the interaction teaches the syntax.
test('clicking a template filters by it and writes the DSL term', async ({ page }) => {
  await page.locator(railToggle).click();
  await expect(page.locator(railItem).first()).toBeVisible({ timeout: 20_000 });

  // Both the rail and the footer group their thousands through the same
  // helper, so the rendered text can be compared directly.
  const expected = (
    await page.locator(railItem).first().locator('.rail-n').textContent()
  ).trim();

  await page.locator(railItem).first().click();

  await expect(page.locator(filterBox)).toHaveValue(/pattern:[0-9a-f]+/, { timeout: 20_000 });
  await expect(page.locator('.terms .term')).toHaveCount(1);

  // The listed count and the filtered count must agree, or the rail is
  // summarising something other than what it selects.
  await expect(page.locator('footer')).toContainText(`of ${expected} records`, {
    timeout: 20_000,
  });

  // And the selected template is marked.
  await expect(page.locator('.rail-item.on')).toHaveCount(1);
});

test('clicking the selected template again clears the filter', async ({ page }) => {
  await page.locator(railToggle).click();
  await expect(page.locator(railItem).first()).toBeVisible({ timeout: 20_000 });

  await page.locator(railItem).first().click();
  await expect(page.locator(filterBox)).toHaveValue(/pattern:/, { timeout: 20_000 });

  await page.locator('.rail-item.on').click();
  await expect(page.locator(filterBox)).not.toHaveValue(/pattern:/, { timeout: 20_000 });
});

// The rail summarises what the filter selected, not the whole dataset.
test('the rail narrows with the filter', async ({ page }) => {
  await page.locator(railToggle).click();
  await expect(page.locator(railItem).first()).toBeVisible({ timeout: 20_000 });

  const all = await page.locator(railItem).count();

  await page.fill(filterBox, 'level:error');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });

  await expect
    .poll(async () => page.locator(railItem).count(), { timeout: 20_000 })
    .toBeLessThan(all);
});

// What the rail is not showing has to be stated, or it understates the data.
test('the rail declares what it left out', async ({ page }) => {
  await page.locator(railToggle).click();
  await expect(page.locator('.rail-foot')).toBeVisible({ timeout: 20_000 });

  await expect(page.locator('.rail-foot')).toContainText('templates');
  await expect(page.locator('.rail-foot')).toContainText('records');
});

test('p toggles the rail from the keyboard', async ({ page }) => {
  await page.locator('.rows').click();
  await page.keyboard.press('p');
  await expect(page.locator('.rail')).toBeVisible();

  await page.keyboard.press('p');
  await expect(page.locator('.rail')).toHaveCount(0);
});
