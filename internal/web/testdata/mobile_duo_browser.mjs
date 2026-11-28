import assert from 'node:assert/strict';
import { expect } from 'playwright/test';
import { ticket } from './browser_fixture.mjs';

const setInsets = (page, left, right) => page.evaluate(({ left, right }) => {
  const root = document.documentElement;
  root.style.setProperty('--mobile-safe-left', `${left}px`);
  root.style.setProperty('--mobile-safe-right', `${right}px`);
  window.dispatchEvent(new Event('resize'));
}, { left, right });

export function registerDuoTests(test, withPage) {
  test('Duo keeps the ticket, tab, and Tickets scroll position while folding', async t => {
    const records = Array.from({ length: 24 }, (_, i) => ticket(`kon-duo-${i}`, `Duo ticket ${i}`));
    await withPage(t, 466, 'dark', async page => {
      const body = page.locator('body');
      const list = page.locator('.mobile-ticket-list');
      const nav = page.getByRole('navigation', { name: 'Main navigation' });
      const controls = page.getByRole('complementary', { name: 'Vertical controls' });
      await setInsets(page, 0, 60);
      await expect(body).toHaveAttribute('data-mobile-rail', 'right');
      await expect(controls.getByRole('button', { name: 'Filters' })).toBeVisible();
      const rail = await controls.boundingBox();
      assert.equal(rail.width, 60);
      assert.equal(rail.x, 406);
      await page.getByRole('searchbox', { name: 'Search tickets' }).fill('Duo ticket');
      const key = await list.locator('[data-mobile-ticket-key]').last().getAttribute('data-mobile-ticket-key');
      const ticket = list.locator(`[data-mobile-ticket-key=${JSON.stringify(key)}]`);
      await ticket.scrollIntoViewIfNeeded();
      assert.ok(await list.evaluate(el => el.scrollTop) > 0);
      await ticket.click();
      await expect(page).toHaveURL(new RegExp(`/t/${key}$`));
      await expect(list).toBeHidden();
      await expect(nav).toBeHidden();
      await expect(controls.getByRole('button', { name: 'Ticket menu' })).toBeVisible();
      const notes = page.locator('.mobile-detail-tabs').getByRole('button', { name: 'Notes', exact: true });
      await notes.click();
      await expect(notes).toHaveAttribute('aria-pressed', 'true');

      await setInsets(page, 0, 0);
      await page.setViewportSize({ width: 951, height: 669 });
      await expect(body).toHaveAttribute('data-mobile-layout', 'duo-split');
      await expect(list).toBeVisible();
      await expect(ticket).toHaveAttribute('aria-current', 'true');
      await expect(nav).toBeVisible();
      await expect.poll(() => list.evaluate(el => el.scrollTop)).toBeGreaterThan(0);
      await expect(notes).toHaveAttribute('aria-pressed', 'true');
      assert.equal((await list.boundingBox()).width, 475);

      await setInsets(page, 60, 0);
      await page.setViewportSize({ width: 466, height: 678 });
      await expect(body).toHaveAttribute('data-mobile-rail', 'left');
      await expect(list).toBeHidden();
      await expect(notes).toHaveAttribute('aria-pressed', 'true');
      expect(await page.locator('.mobile-detail').boundingBox()).toMatchObject({ x: 60, width: 406 });
      await expect(page).toHaveURL(new RegExp(`/t/${key}$`));
    }, { records, onlyRecords: true, hasTouch: true, height: 678 });
  });

  for (const { width, height, hasTouch, layout } of [
    { width: 669, height: 951, hasTouch: true, layout: 'duo-portrait' },
    { width: 951, height: 669, hasTouch: false, layout: 'desktop' },
    { width: 1100, height: 669, hasTouch: true, layout: 'desktop' },
  ]) {
    test(`Duo uses ${layout} at ${width}px with ${hasTouch ? 'coarse' : 'fine'} input`, async t => {
      await withPage(t, width, 'light', async page => {
        await expect(page.locator('body')).toHaveAttribute('data-mobile-layout', layout);
        if (layout === 'desktop') {
          await expect(page.locator('#board-cols')).toBeVisible();
          await expect(page.locator('.mobile-bottom-nav')).toHaveCount(0);
          return;
        }
        const tiles = page.locator('.mobile-ios-tile');
        await expect(tiles).toHaveCount(4);
        const y = (await tiles.first().boundingBox()).y;
        for (const tile of await tiles.all()) assert.equal((await tile.boundingBox()).y, y);
        const progress = await page.locator('[data-mobile-section=in_progress]').boundingBox();
        const open = await page.locator('[data-mobile-section=open]').boundingBox();
        assert.ok(open.x >= progress.x + progress.width);
        await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible();
        await expect(page.getByRole('complementary', { name: 'Vertical controls' })).toHaveCount(0);
      }, { height, hasTouch });
    });
  }
}
