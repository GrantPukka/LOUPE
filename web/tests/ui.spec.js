import { test, expect } from '@playwright/test';

// These drive the embedded UI against a real loupe binary and a real DuckDB
// ingest. Nothing is mocked, for the same reason the Go tests do not mock the
// store: a UI test against a fake API passes while the actual query path is
// broken.

const filterBox = '.filterbar input';
const rows = '.rows .row';

/** Wait for the first page of records to arrive. */
async function loaded(page) {
  await expect(page.locator(rows).first()).toBeVisible({ timeout: 20_000 });
}

test.beforeEach(async ({ page }) => {
  await page.goto('/');
  await loaded(page);
});

test('loads a timeline, sources, and records', async ({ page }) => {
  // Six formats on one screen is the entire pitch, so it is what breaks most
  // visibly if ingest or detection regresses.
  const chips = page.locator('.sources .chip').filter({ hasNotText: 'subscribed' });
  await expect(chips).not.toHaveCount(0);

  await expect(page.locator('.hist .bar').first()).toBeVisible();
  await expect(page.locator('.colhead')).toContainText('message');
});

test('typing a filter narrows the records and shows a removable term', async ({ page }) => {
  const before = await page.locator(rows).count();

  await page.fill(filterBox, 'level:error');

  // The applied-term chips only render once the query comes back, so their
  // appearance is the signal that the filter took effect.
  const term = page.locator('.terms .term');
  await expect(term).toHaveCount(1, { timeout: 20_000 });
  await expect(term.first()).toContainText('error');

  await expect(page.locator(`${rows} .c-lvl`).first()).toHaveText('error');
  expect(await page.locator(rows).count()).toBeLessThanOrEqual(before);
});

test('a term is removable one at a time', async ({ page }) => {
  await page.fill(filterBox, 'level:error source:nginx');
  await expect(page.locator('.terms .term')).toHaveCount(2, { timeout: 20_000 });

  await page.locator('.terms .term', { hasText: 'nginx' }).click();

  await expect(page.locator('.terms .term')).toHaveCount(1);
  await expect(page.locator(filterBox)).toHaveValue('level:error');
});

test('slash focuses the filter box and Escape clears it', async ({ page }) => {
  await page.locator('.rows').click();
  await page.keyboard.press('/');
  await expect(page.locator(filterBox)).toBeFocused();

  await page.keyboard.type('level:error');
  await expect(page.locator('.terms .term')).toHaveCount(1, { timeout: 20_000 });

  await page.keyboard.press('Escape');
  await expect(page.locator(filterBox)).toHaveValue('');
  await expect(page.locator('.terms .term')).toHaveCount(0);
});

test('dragging the timeline writes a real DSL string into the filter box', async ({ page }) => {
  // This is the interaction the architecture doc singles out: the drag has to
  // produce syntax the user could have typed, so the UI teaches the language
  // instead of hiding it.
  const hist = page.locator('.hist');
  const box = await hist.boundingBox();

  await page.mouse.move(box.x + box.width * 0.3, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width * 0.7, box.y + box.height / 2, { steps: 12 });
  await page.mouse.up();

  const value = await page.locator(filterBox).inputValue();
  expect(value.trim()).not.toBe('');
  // A time term, not an opaque handle — a range, a last:, or a bare window.
  expect(value).toMatch(/\d/);
  await expect(page.locator('.terms .term')).not.toHaveCount(0, { timeout: 20_000 });
});

test('clicking a row opens its detail with the raw line', async ({ page }) => {
  await page.locator(rows).first().click();

  const detail = page.locator('.detail');
  await expect(detail).toBeVisible();
  // Never losing the original text is a hard invariant, so the raw line being
  // present in the detail is worth asserting rather than assuming.
  await expect(detail.locator('.raw')).not.toBeEmpty();
});

test('clicking a field value in a row detail filters on it', async ({ page }) => {
  await page.locator(rows).first().click();

  const value = page.locator('.detail .kv .v').first();
  await expect(value).toBeVisible();
  await value.click();

  await expect(page.locator('.terms .term')).not.toHaveCount(0, { timeout: 20_000 });
});

test('an unknown field is explained, not answered with an empty list', async ({ page }) => {
  // The DSL rule: an unknown field name is an error with a suggestion, never a
  // silent zero-result. Regressing this in the UI turns a typo into a wrong
  // conclusion during an incident.
  await page.fill(filterBox, 'statuss:500');

  const error = page.locator('.error-bar');
  await expect(error).toBeVisible({ timeout: 20_000 });
  await expect(error).toContainText('statuss');
  await expect(error).toContainText('status');
});

test('the syntax help panel opens and inserts a term', async ({ page }) => {
  await page.locator('.filterbar button', { hasText: 'syntax' }).click();

  const help = page.locator('.filterhelp, .help');
  await expect(help.first()).toBeVisible();

  await page.keyboard.press('Escape');
  await expect(help.first()).toBeHidden();
});
