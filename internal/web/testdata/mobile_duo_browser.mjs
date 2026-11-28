import assert from 'node:assert/strict';
import { expect } from 'playwright/test';
import { openTicket, ticket, withInsets } from './browser_fixture.mjs';

const setInsets = (page, left, right) => page.evaluate(({ left, right }) => {
  const root = document.documentElement;
  root.style.setProperty('--mobile-safe-left', `${left}px`);
  root.style.setProperty('--mobile-safe-right', `${right}px`);
  window.dispatchEvent(new Event('resize'));
}, { left, right });

async function visibleTapPoint(button, { top = 0, right = 0, bottom = 0, left = 0 }) {
  await expect(button).toBeEnabled();
  return button.evaluate((el, insets) => {
    const rect = el.getBoundingClientRect();
    const x = rect.left + rect.width / 2;
    const y = rect.top + rect.height / 2;
    if (rect.width < 44 || rect.height < 44 || rect.top < insets.top || rect.left < insets.left ||
        rect.right > innerWidth - insets.right || rect.bottom > innerHeight - insets.bottom) return null;
    return [[x, y], [rect.left + 8, rect.top + 8], [rect.right - 8, rect.bottom - 8]]
      .every(([x, y]) => el.contains(document.elementFromPoint(x, y))) ? { x, y } : null;
  }, { top, right, bottom, left });
}

const listAnchor = list => list.evaluate(el => {
  const { top, bottom } = el.getBoundingClientRect();
  const firstRow = [...el.querySelectorAll('[data-mobile-ticket-key]')].find(row => {
    const bounds = row.getBoundingClientRect();
    return bounds.height && bounds.top < bottom && bounds.bottom > top;
  });
  return firstRow && [firstRow.dataset.mobileTicketKey, Math.round(firstRow.getBoundingClientRect().top - top)];
});

export function registerDuoTests(test, withPage) {
  for (const { width, height, insets, layout, rail } of [
    { width: 838, height: 390, insets: {}, layout: 'phone' },
    { width: 956, height: 440, insets: { right: 59, bottom: 21 }, layout: 'phone' },
    { width: 951, height: 570, insets: { bottom: 21 }, layout: 'phone' },
    { width: 951, height: 571, insets: { bottom: 21 }, layout: 'duo-split', rail: 'right' },
    { width: 466, height: 570, insets: { left: 84, bottom: 21 }, layout: 'phone' },
    { width: 466, height: 571, insets: { left: 84, bottom: 21 }, layout: 'phone', rail: 'left' },
  ]) {
    test(`Duo uses ${layout} ${rail ? `with ${rail} rail` : 'without rail'} at ${width} x ${height}`, async t => {
      await withPage(t, width, 'dark', async page => {
        const body = page.locator('body');
        await expect(body).toHaveAttribute('data-mobile-layout', layout);
        if (rail) await expect(body).toHaveAttribute('data-mobile-rail', rail);
        else await expect(body).not.toHaveAttribute('data-mobile-rail', /.+/);
        await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible();
      }, { height, hasTouch: true, beforeNavigation: withInsets(insets) });
    });
  }

  for (const { width, height, insets } of [
    { width: 838, height: 390, insets: {} },
    { width: 956, height: 440, insets: { top: 12, left: 59, right: 24, bottom: 21 } },
  ]) {
    test(`Duo short landscape ticket controls are tappable at ${width} x ${height}`, async t => {
      await withPage(t, width, 'dark', async (page, fixture) => {
        await openTicket(page, 'kon-live');
        const detail = page.locator('.mobile-detail');
        await expect(detail.getByRole('button', { name: 'Ticket', exact: true })).toHaveAttribute('aria-pressed', 'true');
        await expect(page.getByRole('complementary', { name: 'Vertical controls' })).toHaveCount(0);
        const points = new Map();
        for (const name of ['Back', 'Ticket menu', 'Pause', 'Watch live']) {
          const point = await visibleTapPoint(detail.getByRole('button', { name, exact: true }), insets);
          assert.ok(point, `${name} must be fully visible and tappable before scrolling`);
          points.set(name, point);
        }
        assert.equal(fixture.sockets.length, 0);
        const { x, y } = points.get('Watch live');
        await page.touchscreen.tap(x, y);
        await expect(detail.getByRole('button', { name: 'Output', exact: true })).toHaveAttribute('aria-pressed', 'true');
        await expect(detail.getByRole('button', { name: 'Live', exact: true })).toHaveAttribute('aria-pressed', 'true');
        await expect(detail.getByRole('button', { name: 'Type in terminal', exact: true })).toBeVisible();
        await expect(detail.getByRole('button', { name: 'Send', exact: true })).toBeDisabled();
        await expect.poll(() => fixture.sockets.length).toBe(1);
        assert.notEqual(fixture.sockets[0].searchParams.get('rw'), '1');
        assert.deepEqual(fixture.mutations, []);
      }, { height, hasTouch: true, beforeNavigation: withInsets(insets) });
    });
  }

  for (const { width, height, left, right, bottom, rail } of [
    { width: 466, height: 678, left: 84, right: 0, bottom: 0, rail: 'left' },
    { width: 466, height: 678, left: 0, right: 84, bottom: 0, rail: 'right' },
    { width: 466, height: 678, left: 28, right: 0, bottom: 0 },
    { width: 466, height: 678, left: 0, right: 28, bottom: 0 },
    { width: 956, height: 440, left: 59, right: 24, bottom: 21 },
  ]) {
    test(`Duo reserves side insets ${left}/${right}px once at ${width} x ${height}`, async t => {
      await withPage(t, width, 'light', async page => {
        const body = page.locator('body');
        if (rail) await expect(body).toHaveAttribute('data-mobile-rail', rail);
        else await expect(body).not.toHaveAttribute('data-mobile-rail', /.+/);
        const list = page.locator('.mobile-ticket-list');
        const search = list.getByRole('searchbox', { name: 'Search tickets' });
        const nav = page.getByRole('navigation', { name: 'Main navigation' });
        const contentEdge = { x: left, width: width - left - right };
        expect(await page.locator('.mobile-board-layer').boundingBox()).toMatchObject(contentEdge);
        expect(await list.boundingBox()).toMatchObject(contentEdge);
        // WebKit's scrollbar can take up space inside the list border box.
        const clientWidth = await list.evaluate(el => el.clientWidth);
        expect(await search.boundingBox()).toMatchObject({ x: left + 16, width: clientWidth - 32 });
        if (!rail) {
          const first = await nav.getByRole('button').first().boundingBox();
          const last = await nav.getByRole('button').last().boundingBox();
          expect(first.x).toBeCloseTo(left, 0);
          expect(last.x + last.width).toBeCloseTo(width - right, 0);
          expect(last.y + last.height).toBeCloseTo(height - bottom, 0);
        }
        if (width >= 768) {
          const banner = page.locator('.ticket-refresh-banner');
          const status = await banner.getByRole('status').boundingBox();
          const retry = await banner.getByRole('button', { name: 'Retry' }).boundingBox();
          expect(status.x).toBe(left);
          expect(retry.x + retry.width).toBeCloseTo(width - right, 0);
        }
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth), width);
      }, { height, hasTouch: true, beforeNavigation: async page => {
        await withInsets({ left, right, bottom })(page);
        if (width >= 768) await page.route('**/api/ticket-sources', route => route.fulfill({ json: [
          { id: 'primary', path: '/fixtures/primary', available: true },
          { id: 'offline', path: '/fixtures/offline', available: false, error: 'Fixture folder unavailable' },
        ] }));
      } });
    });
  }

  test('Duo keeps Live, its socket and the Tickets anchor across the height cutoff', async t => {
    const width = 951;
    const records = Array.from({ length: 24 }, (_, i) => ticket(`kon-duo-${i}`, `Duo ticket ${i}`));
    await withPage(t, width, 'dark', async (page, fixture) => {
      const body = page.locator('body');
      const list = page.locator('.mobile-ticket-list');
      const rail = page.getByRole('complementary', { name: 'Vertical controls' });
      const detail = page.locator('.mobile-detail');
      const pause = detail.getByRole('button', { name: 'Pause', exact: true });
      const railPause = rail.getByRole('button', { name: 'Pause', exact: true });
      const terminal = detail.locator('.xterm');
      const live = detail.getByRole('button', { name: 'Live', exact: true });
      const output = detail.getByRole('button', { name: 'Output', exact: true });
      const search = list.getByRole('searchbox', { name: 'Search tickets' });
      await openTicket(page, 'kon-live');
      await expect(body).toHaveAttribute('data-mobile-layout', 'duo-split');
      await expect(list).toBeVisible();
      await expect(list.locator('[data-mobile-ticket-key="kon-live"]')).toHaveAttribute('aria-current', 'true');
      await rail.getByRole('button', { name: 'Watch live', exact: true }).click();
      await expect(terminal).toBeVisible();
      const originalTerminal = await terminal.elementHandle();
      await search.fill('Duo');
      await list.locator('[data-mobile-ticket-key="kon-duo-12"]').scrollIntoViewIfNeeded();
      await expect.poll(() => list.evaluate(el => el.scrollTop)).toBeGreaterThan(0);
      const anchor = await listAnchor(list);
      assert.ok(anchor);
      await railPause.focus();
      await expect(railPause).toBeFocused();
      const checkLive = async () => {
        await expect(page).toHaveURL(/\/t\/kon-live$/);
        await expect(output).toHaveAttribute('aria-pressed', 'true');
        await expect(live).toHaveAttribute('aria-pressed', 'true');
        await expect(terminal).toBeVisible();
        assert.ok(await terminal.evaluate((el, original) => el === original, originalTerminal));
        assert.equal(await page.evaluate(() => window.__fixtureSockets[0]?.readyState), 1);
        assert.equal(fixture.sockets.length, 1);
      };

      await page.setViewportSize({ width, height: 570 });
      await expect(body).toHaveAttribute('data-mobile-layout', 'phone');
      await expect(list).toBeHidden();
      await expect(pause).toBeFocused();
      await checkLive();

      await page.setViewportSize({ width, height: 571 });
      await expect(body).toHaveAttribute('data-mobile-layout', 'duo-split');
      await expect(railPause).toBeFocused();
      await expect(list).toBeVisible();
      await expect.poll(() => listAnchor(list)).toEqual(anchor);
      await checkLive();

      await search.focus();
      await page.keyboard.press('End');
      await page.evaluate(() => {
        const viewport = window.visualViewport;
        Object.defineProperty(viewport, 'height', { configurable: true, value: 360 });
        viewport.dispatchEvent(new Event('resize'));
      });
      await page.keyboard.type(' ticket');
      await expect(search).toHaveValue('Duo ticket');
      await expect.poll(() => page.evaluate(() => document.documentElement.style.getPropertyValue('--mobile-vv-height'))).toBe('360px');
      await expect(body).toHaveAttribute('data-mobile-layout', 'duo-split');
      await expect(list).toBeVisible();
      await checkLive();

      const notes = detail.getByRole('button', { name: 'Notes', exact: true });
      await notes.click();
      await rail.getByRole('button', { name: 'Watch live', exact: true }).focus();
      await page.setViewportSize({ width, height: 570 });
      await expect(notes).toBeFocused();
      await expect(notes).toHaveAttribute('aria-pressed', 'true');
    }, { height: 571, records, hasTouch: true, beforeNavigation: withInsets({ bottom: 21 }) });
  });

  test('Duo restores the scrolled Tickets list after crossing the cutoff with Ticket menu open', async t => {
    const width = 951;
    const records = Array.from({ length: 24 }, (_, i) => ticket(`kon-duo-${i}`, `Duo ticket ${i}`));
    await withPage(t, width, 'dark', async page => {
      const body = page.locator('body');
      const list = page.locator('.mobile-ticket-list');
      const menu = page.getByRole('complementary', { name: 'Vertical controls' }).getByRole('button', { name: 'Ticket menu' });
      const sheet = page.getByRole('dialog', { name: 'Ticket actions', exact: true });
      await openTicket(page, 'kon-live');
      await list.getByRole('searchbox', { name: 'Search tickets' }).fill('Duo');
      await list.locator('[data-mobile-ticket-key="kon-duo-12"]').scrollIntoViewIfNeeded();
      assert.ok(await list.evaluate(el => el.scrollTop) > 0);
      const anchor = await listAnchor(list);
      assert.ok(anchor);
      await menu.press('Enter');
      await expect(sheet).toBeVisible();

      await page.setViewportSize({ width, height: 570 });
      await expect(body).toHaveAttribute('data-mobile-layout', 'phone');
      await expect(list).toHaveCount(0);
      await page.setViewportSize({ width, height: 571 });
      await expect(body).toHaveAttribute('data-mobile-layout', 'duo-split');
      await expect(list).toBeVisible();
      await expect(sheet).toBeVisible();
      await expect.poll(() => listAnchor(list)).toEqual(anchor);
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
      await expect.poll(() => listAnchor(list)).toEqual(anchor);
    }, { height: 571, records, hasTouch: true, beforeNavigation: withInsets({ bottom: 21 }) });
  });

  test('Duo keeps inline Filters in view when the cover rail disappears', async t => {
    const width = 466;
    await withPage(t, width, 'dark', async page => {
      const body = page.locator('body');
      const list = page.locator('.mobile-ticket-list');
      const filters = page.getByRole('complementary', { name: 'Vertical controls' }).getByRole('button', { name: 'Filters' });
      const inline = list.getByRole('button', { name: 'Filters' });
      await expect(list.locator('[data-mobile-ticket-key]').first()).toBeInViewport();
      await filters.focus();
      await expect(filters).toBeFocused();
      await expect.poll(() => list.evaluate(el => el.scrollTop)).toBe(0);

      await page.setViewportSize({ width, height: 570 });
      await expect(body).not.toHaveAttribute('data-mobile-rail', /.+/);
      await expect(inline).toBeFocused();
      await expect(inline).toBeInViewport({ ratio: 1 });
      await page.keyboard.press('Enter');
      await expect(page.getByRole('dialog', { name: 'Ticket filters', exact: true })).toBeVisible();
    }, { height: 571, hasTouch: true, beforeNavigation: withInsets({ left: 84, bottom: 21 }) });
  });

  test('Duo returns keyboard focus to Ticket menu after crossing the height cutoff', async t => {
    const width = 951;
    await withPage(t, width, 'dark', async page => {
      const body = page.locator('body');
      const rail = page.getByRole('complementary', { name: 'Vertical controls' });
      const detail = page.locator('.mobile-detail');
      const sheet = page.getByRole('dialog', { name: 'Ticket actions', exact: true });
      await openTicket(page, 'kon-live');
      for (const { from, to, height, layout } of [
        { from: rail, to: detail, height: 570, layout: 'phone' },
        { from: detail, to: rail, height: 571, layout: 'duo-split' },
      ]) {
        await from.getByRole('button', { name: 'Ticket menu' }).press('Enter');
        await expect(sheet).toBeVisible();
        await page.setViewportSize({ width, height });
        await expect(body).toHaveAttribute('data-mobile-layout', layout);
        await page.keyboard.press('Escape');
        await expect(sheet).toBeHidden();
        await expect(to.getByRole('button', { name: 'Ticket menu' })).toBeFocused();
        await page.keyboard.press('Enter');
        await expect(sheet).toBeVisible();
        await page.keyboard.press('Escape');
        await expect(sheet).toBeHidden();
        await expect(page).toHaveURL(/\/t\/kon-live$/);
      }
    }, { height: 571, hasTouch: true, beforeNavigation: withInsets({ bottom: 21 }) });
  });

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
      await controls.getByRole('button', { name: 'Ticket menu' }).focus();
      await page.setViewportSize({ width: 951, height: 549 });
      await expect(page.locator('.mobile-detail').getByRole('button', { name: 'Ticket menu' })).toBeFocused();
      await expect(notes).toHaveAttribute('aria-pressed', 'true');

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
