import assert from 'node:assert/strict';
import { expect } from 'playwright/test';
import { deferred, source, ticket, withInsets } from './browser_fixture.mjs';
import { iosPwa, iosPwaOptions } from './browser_scenes.mjs';

export function registerIOSPwaTests(test, withPage) {
  // The browser's local calendar is UTC; recent fixture rows belong to Today at noon.
  const now = new Date('2026-11-09T12:00:00Z');
  const options = () => ({ ...iosPwaOptions(now), height: 844, scheduleNow: +now,
    beforeNavigation: page => page.clock.setFixedTime(now) });
  const row = (page, key) => page.locator(`.mobile-ios-row:has([data-mobile-ticket-key=${JSON.stringify(key)}])`);
  const opener = (page, key) => row(page, key).locator('.mobile-ios-row-open');
  const action = (page, name) => page.getByRole('button', { name, exact: true });
  const requestPaths = (fixture, action) => fixture.requests.filter(req => req.startsWith('POST ') && req.endsWith('/' + action)).map(req => req.slice(5));
  const record = (fixture, id) => fixture.records.get('primary/' + id);

  async function openRun(page, key) {
    await row(page, key).getByRole('button', { name: /^Run / }).press('Enter');
    const dialog = page.getByRole('dialog', { name: 'Run ' + key, exact: true });
    await expect(dialog).toBeVisible();
    return dialog;
  }

  async function selectOpen(page) {
    await action(page, 'Select').click();
    await action(page, 'Select All').click();
    await expect(page.getByText('3 selected', { exact: true })).toBeVisible();
  }

  test('iOS PWA review before Run preserves saved settings and excludes ineligible rows', async t => {
    const data = options();
    data.records.push(ticket('ios-import', 'Imported task', { source, status: 'open', updated_at: now.toISOString() }));
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const preview = await openRun(page, 'ios-112');
      assert.deepEqual(requestPaths(fixture, 'run'), []);
      await preview.getByRole('button', { name: 'Cancel', exact: true }).click();
      await expect(row(page, 'ios-112').getByRole('button', { name: /^Run / })).toBeFocused();
      assert.deepEqual(requestPaths(fixture, 'run'), []);
      await (await openRun(page, 'ios-112')).getByRole('button', { name: 'Run', exact: true }).click();
      await expect(opener(page, 'ios-112')).toContainText('Queued');
      assert.equal(new URL(page.url()).hash, '#/');
      assert.deepEqual(requestPaths(fixture, 'run'), ['/api/tickets/ios-112/run']);
      assert.deepEqual(fixture.mutations[0].body, {});
      await action(page, 'Select').click();
      await expect(opener(page, 'ios-114')).toBeDisabled();
      await expect(opener(page, 'ios-115')).toBeDisabled();
      await expect(opener(page, source + '/ios-import')).toBeDisabled();
    }, data);
  });

  test('iOS PWA Queue does not replace an earlier running event with its acknowledgement', async t => {
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const key = 'ios-112', ack = deferred();
      fixture.behavior.lifecycleAckWait = ack.promise;
      try {
        await (await openRun(page, key)).getByRole('button', { name: 'Run', exact: true }).click();
        await expect.poll(() => fixture.mutations.length).toBe(1);
        const running = { ...record(fixture, key), status: 'in_progress', execution_active: true, started_at: now.toISOString() };
        await page.evaluate(ticket => window.__fixtureSSE.at(-1).dispatchEvent(new MessageEvent('ticket_updated', { data: JSON.stringify(ticket) })), running);
        await expect(opener(page, key).locator('.mobile-ios-stage')).toBeVisible();
        ack.resolve();
        await expect(page.getByRole('status').filter({ hasText: 'Run requested' })).toBeVisible();
        await expect(opener(page, key).locator('.mobile-ios-stage')).toBeVisible();
        assert.deepEqual(requestPaths(fixture, 'run'), [`/api/tickets/${key}/run`]);
      } finally { ack.resolve(); }
    }, options());
  });

  test('iOS PWA batch keeps refusals, reconciles lost acknowledgements, and never retries successes', async t => {
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const [first, second, third] = [...iosPwa.queue].sort((a, b) => record(fixture, a).title.localeCompare(record(fixture, b).title));
      fixture.behavior.lifecycleRefusals.set(`/api/tickets/${first}/run`, 'Pickup refused by fixture');
      fixture.behavior.lifecyclePostWriteFailures.set(`/api/tickets/${second}/run`, { error: 'Lost acknowledgement' });
      await selectOpen(page);
      await action(page, 'Queue 3').click();
      const dialog = page.getByRole('dialog', { name: 'Queue 3 tickets' });
      await dialog.getByRole('button', { name: 'Queue 3 tickets' }).click();
      await expect(dialog.getByText('Pickup refused by fixture', { exact: true })).toBeVisible();
      await expect(dialog.getByText(/Lost acknowledgement/)).toBeVisible();
      await expect(dialog.getByText('Request accepted', { exact: true })).toBeVisible();
      await expect(dialog.getByRole('button', { name: /^Retry / })).toHaveCount(1);
      assert.deepEqual(new Set(requestPaths(fixture, 'run')), new Set(iosPwa.queue.map(id => `/api/tickets/${id}/run`)));
      assert.equal(record(fixture, third).status, 'todo');
      fixture.behavior.lifecycleRefusals.clear();
      await dialog.getByRole('button', { name: /^Retry / }).click();
      await expect(dialog.getByText('Request accepted', { exact: true })).toHaveCount(2);
      assert.equal(record(fixture, first).status, 'todo');
      assert.equal(requestPaths(fixture, 'run').filter(path => path.includes(second)).length, 1);
      assert.equal(requestPaths(fixture, 'run').filter(path => path.includes(third)).length, 1);
    }, options());
  });

  test('iOS PWA Schedule sends one reviewed instant for all selected tickets', async t => {
    await withPage(t, 390, 'light', async (page, fixture) => {
      await selectOpen(page);
      await action(page, 'Schedule').click();
      const dialog = page.getByRole('dialog', { name: 'Schedule 3 tickets' });
      await dialog.getByRole('textbox', { name: 'Pickup date and time or delay', exact: true }).fill('24h');
      await dialog.getByRole('button', { name: 'Schedule 3 tickets' }).click();
      await expect(dialog).toBeHidden();
      const actions = fixture.mutations.filter(mutation => mutation.path.endsWith('/schedule'));
      assert.equal(actions.length, 3);
      assert.deepEqual(actions.map(action => action.body.scheduled_at), Array(3).fill('2026-11-10T12:00:00Z'));
      assert.equal(requestPaths(fixture, 'run').length, 0);
    }, options());
  });

  test('iOS PWA a lost Schedule acknowledgement blocks pickup after closing the sheet', async t => {
    const key = 'ios-112';
    await withPage(t, 390, 'dark', async (page, fixture) => {
      fixture.behavior.lifecyclePostWriteFailures.set(`/api/tickets/${key}/schedule`, { error: 'Lost acknowledgement' });
      await action(page, 'Select').click();
      await opener(page, key).click();
      await action(page, 'Schedule').click();
      const dialog = page.getByRole('dialog', { name: 'Schedule 1 ticket' });
      await dialog.getByRole('textbox', { name: 'Pickup date and time or delay' }).fill('24h');
      const read = fixture.holdDetail('primary', key);
      read.release({ failure: 'http' });
      await dialog.getByRole('button', { name: 'Schedule 1 ticket' }).click();
      await expect(dialog.getByText('Result unknown: Lost acknowledgement')).toBeVisible();
      await expect(dialog.getByRole('button', { name: 'Refresh ticket' })).toBeEnabled();
      await dialog.getByRole('button', { name: 'Close' }).click();
      await action(page, 'Cancel').click();
      const refresh = row(page, key).getByRole('button', { name: 'Refresh ticket' });
      await expect(refresh).toBeVisible();
      await expect(row(page, key).getByRole('button', { name: /^Run / })).toHaveCount(0);
      await action(page, 'Select').click();
      await action(page, 'Select All').click();
      await expect(page.getByText('2 selected', { exact: true })).toBeVisible();
      await expect(opener(page, key)).toBeDisabled();
      await action(page, 'Cancel').click();
      await refresh.click();
      await expect(refresh).toHaveCount(0);
      await expect(row(page, key).getByRole('button', { name: /^Run / })).toHaveCount(0);
      assert.equal(record(fixture, key).scheduled_at, '2026-11-10T12:00:00Z');
      assert.deepEqual(requestPaths(fixture, 'run'), []);
    }, options());
  });

  test('iOS PWA long reviewed ticket settings fit the 320px Queue sheet', async t => {
    const data = options();
    const key = 'ios-112';
    const ticket = data.records.find(record => record.id === key);
    ticket.title = 'LongTitle'.repeat(30);
    ticket.pipeline = 'long-pipeline-name'.repeat(20);
    await withPage(t, 320, 'dark', async page => {
      await action(page, 'Select').click();
      await opener(page, key).click();
      await action(page, 'Queue 1').click();
      const dialog = page.getByRole('dialog', { name: 'Queue 1 ticket' });
      const atTime = dialog.getByRole('button', { name: 'At a time' });
      const bounds = await dialog.boundingBox();
      for (const control of [atTime, dialog.getByRole('button', { name: 'Queue 1 ticket' })]) {
        const box = await control.boundingBox();
        assert.ok(box && box.x >= bounds.x && box.x + box.width <= bounds.x + bounds.width);
      }
      await atTime.click();
      await expect(page.getByRole('dialog', { name: 'Schedule 1 ticket' }).getByRole('textbox', { name: 'Pickup date and time or delay' })).toBeVisible();
    }, data);
  });

  test('iOS PWA Home search leaves independently searchable lists reachable', async t => {
    await withPage(t, 390, 'dark', async page => {
      const homeSearch = page.getByRole('searchbox', { name: 'Search tickets' });
      const link = page.locator('.mobile-ios-tiles').getByRole('button', { name: '0 Human review', exact: true });
      await homeSearch.fill('no tickets match this search');
      await link.click();
      await expect(page).toHaveURL(/#\/column\/human_review$/);
      const listSearch = page.getByRole('searchbox', { name: 'Search this list' });
      await expect(listSearch).toHaveValue('');
      await listSearch.fill('checkout');
      await expect(opener(page, iosPwa.firstReview)).toBeVisible();
      await action(page, 'Back to Tickets').click();
      await expect(homeSearch).toHaveValue('no tickets match this search');
      await expect(link).toBeFocused();
    }, options());
  });

  test('iOS PWA Home returns focus to a visible control when its opener leaves the search results', async t => {
    const data = options();
    data.records.push(...Array.from({ length: 30 }, (_, index) => ticket(`return-${index}`, `Return target ${index}`, { status: 'human_review' })));
    const key = 'return-27';
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const list = page.locator('.mobile-ticket-list');
      await page.getByRole('searchbox', { name: 'Search tickets' }).fill('Return target');
      await opener(page, key).scrollIntoViewIfNeeded();
      await expect.poll(() => list.evaluate(el => el.scrollTop)).toBeGreaterThan(100);
      await opener(page, key).click();
      await expect(page).toHaveURL(new RegExp(`#/t/${key}$`));
      const updated = { ...record(fixture, key), title: 'Outside the search' };
      await page.evaluate(ticket => window.__fixtureSSE.at(-1).dispatchEvent(new MessageEvent('ticket_updated', { data: JSON.stringify(ticket) })), updated);
      await page.goBack();
      await expect(opener(page, key)).toHaveCount(0);
      await expect.poll(() => list.evaluate(el => {
        const focus = document.activeElement;
        const control = focus.getBoundingClientRect(), list = el.getBoundingClientRect();
        return { onControl: focus !== el && el.contains(focus), scrolled: el.scrollTop > 0,
          visible: control.top < list.bottom && control.bottom > list.top,
          focused: focus.tagName };
      })).toEqual({ onControl: true, scrolled: true, visible: true, focused: 'BUTTON' });
    }, data);
  });

  for (const hash of ['#/', '#/column/human_review']) {
    test(`iOS PWA refresh Retry remains below the top safe area on ${hash}`, async t => {
      const data = options();
      await withPage(t, 390, 'dark', async (page, fixture) => {
        const failed = fixture.holdSnapshot();
        failed.release({ failure: 'http' });
        await action(page, 'Filters').click();
        await action(page, 'Refresh tickets').click();
        const retry = action(page, 'Retry');
        await expect(retry).toBeVisible();
        const box = await retry.boundingBox();
        assert.ok(box.y >= 59);
        await retry.click({ trial: true });
      }, { ...data, hash, beforeNavigation: async page => {
        await data.beforeNavigation(page);
        await withInsets({ top: 59 })(page);
      } });
    });
  }

  test('iOS PWA leaving a running batch cancels unsent requests', async t => {
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const ack = deferred();
      fixture.behavior.lifecycleAckWait = ack.promise;
      try {
        await selectOpen(page);
        await action(page, 'Queue 3').click();
        const dialog = page.getByRole('dialog', { name: 'Queue 3 tickets' });
        await dialog.getByRole('button', { name: 'Queue 3 tickets' }).click();
        await expect.poll(() => fixture.mutations.length).toBe(1);
        await page.goBack();
        await expect(dialog).toBeHidden();
        const key = fixture.mutations[0].before.id;
        ack.resolve();
        await expect(opener(page, key)).toContainText('Queued');
        assert.equal(requestPaths(fixture, 'run').length, 1);
      } finally { ack.resolve(); }
    }, options());
  });

  async function drag(page, button, dx, dy) {
    await button.scrollIntoViewIfNeeded();
    const box = await button.boundingBox();
    const x = box.x + box.width * (dx < 0 ? 0.75 : 0.28), y = box.y + box.height / 2;
    await page.mouse.move(x, y);
    await page.mouse.down();
    await page.mouse.move(x + dx, y + dy, { steps: 6 });
    await page.mouse.up();
  }

  test('iOS PWA review swipe reveals authorized commands without opening the ticket', async t => {
    const data = options();
    data.hash = '#/column/human_review';
    data.records.push(ticket('ios-import-review', 'Imported review', { source, status: 'human_review', finished_at: now.toISOString() }));
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const imported = opener(page, source + '/ios-import-review');
      await drag(page, imported, -60, 0);
      await expect(page.locator('.mobile-ios-swipes')).toHaveCount(0);
      // Browser click semantics for a non-mutating row may open it.
      if (new URL(page.url()).hash !== '#/column/human_review') await page.goBack();
      const subject = opener(page, iosPwa.firstReview);
      await drag(page, subject, 5, 22);
      await expect(page.locator('.mobile-ios-swipes')).toHaveCount(0);
      if (new URL(page.url()).hash !== '#/column/human_review') await page.goBack();
      await drag(page, subject, -75, 0);
      const revealed = row(page, iosPwa.firstReview).getByRole('group', { name: 'Review shortcuts' });
      await expect(revealed.getByRole('button')).toHaveText(['Run again from plan', 'Mark done']);
      assert.equal(new URL(page.url()).hash, '#/column/human_review');
      assert.equal(fixture.mutations.length, 0);
    }, data);
  });

  test('iOS PWA Open swipe queues once without following the ticket link', async t => {
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const key = 'ios-112';
      await drag(page, opener(page, key), 100, 0);
      await expect(opener(page, key)).toContainText('Queued');
      assert.equal(new URL(page.url()).hash, '#/');
      assert.deepEqual(requestPaths(fixture, 'run'), [`/api/tickets/${key}/run`]);
    }, options());
  });

  test('iOS PWA Reply on a second ticket does not unguard the first ticket’s draft', async t => {
    const data = options(), first = iosPwa.running, second = 'ios-105';
    Object.assign(data.records.find(record => record.id === second), {
      waiting_for_input: true, waiting_question: 'Should checkout verify the delivery address?',
    });
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const detail = page.locator('.mobile-detail');
      const reply = key => row(page, key).getByRole('button', { name: 'Reply', exact: true });
      const composer = detail.getByRole('textbox', { name: 'Message the agent' });
      await reply(first).click();
      await expect(composer).toBeFocused();
      await composer.fill('Keep offset pagination');
      await page.goBack();
      await reply(second).click();
      await expect(composer).toBeFocused();
      await composer.fill('Verify the address');
      await page.goBack();
      const updated = record(fixture, first);
      updated.waiting_question = 'What page size should we use?';
      await page.evaluate(ticket => window.__fixtureSSE.at(-1).dispatchEvent(new MessageEvent('ticket_updated', { data: JSON.stringify(ticket) })), updated);
      await opener(page, first).click();
      await expect(page).toHaveURL(new RegExp(`#/t/${first}$`));
      const terminal = detail.getByRole('button', { name: 'Terminal', exact: true });
      await expect(composer.or(terminal).filter({ visible: true }).first()).toBeVisible();
      if (!(await composer.isVisible())) await terminal.click();
      await composer.focus();
      await expect(composer).toHaveValue('Keep offset pagination');
      const send = detail.getByRole('button', { name: 'Send', exact: true });
      await expect(send).toBeEnabled();
      await send.click();
      await expect(detail.getByRole('status').filter({ hasText: 'Reply not sent.' })).toBeVisible();
      assert.deepEqual(fixture.terminalInputs, []);
    }, data);
  });

  for (const [from, changed] of [['home', false], ['detail', false], ['detail', true]]) {
    test(`iOS PWA Reply from ${from} writes the existing stream: changed question=${changed}`, async t => {
      const message = 'Keep offset pagination for older clients.';
      await withPage(t, 390, 'dark', async (page, fixture) => {
        const detail = page.locator('.mobile-detail');
        if (from === 'detail') {
          await opener(page, iosPwa.running).click();
          await expect(detail.getByRole('region', { name: 'Running ticket overview' }).getByRole('heading', { name: /Add cursor pagination/ })).toBeVisible();
          assert.equal(fixture.sockets.length, 0, 'The overview does not open Terminal');
        }
        const replySurface = from === 'home' ? row(page, iosPwa.running) : detail;
        await replySurface.getByRole('button', { name: 'Reply', exact: true }).click();
        const composer = detail.getByRole('textbox', { name: 'Message the agent' });
        await expect(composer).toBeFocused();
        await composer.fill(message);
        const send = detail.getByRole('button', { name: 'Send', exact: true });
        await expect(send).toBeEnabled();
        const nextQuestion = 'Use a separate endpoint instead?';
        if (changed) {
          const next = { ...record(fixture, iosPwa.running), waiting_question: nextQuestion };
          await page.evaluate(({ next, message }) => {
            // The terminal can replace its socket after the Send button becomes enabled.
            const sockets = window.WebSocket.prototype, originalSend = sockets.send;
            sockets.send = function(data) {
              const result = originalSend.call(this, data);
              if (typeof data === 'string' && data.includes(message)) {
                sockets.send = originalSend;
                for (const stream of window.__fixtureSSE) {
                  if (stream.url === '/api/events') stream.emit('ticket_updated', next);
                }
              }
              return result;
            };
          }, { next, message });
        }
        await send.click();
        if (changed) await expect(detail.getByText(nextQuestion, { exact: true })).toBeVisible();
        const result = changed ? 'Enter wasn’t sent. Your draft was kept. Check Terminal before opening Reply again.' : 'Sent to terminal';
        await expect(detail.getByRole('status').filter({ hasText: result })).toBeVisible();
        assert.ok(fixture.terminalInputs[0].data.includes(message));
        if (changed) {
          assert.equal(fixture.terminalInputs.length, 1, 'Do not submit an answer to a stale question');
          await expect(composer).toHaveValue(message);
        } else {
          assert.equal(fixture.terminalInputs.at(-1).data, '\r');
          await expect(composer).toHaveValue('');
          const answered = { ...record(fixture, iosPwa.running), waiting_for_input: false };
          await page.evaluate(ticket => window.__fixtureSSE.at(-1).dispatchEvent(new MessageEvent('ticket_updated', { data: JSON.stringify(ticket) })), answered);
          await composer.fill('Follow-up terminal message');
          await send.click();
          await expect.poll(() => fixture.terminalInputs.length).toBe(4);
          assert.equal(fixture.terminalInputs[2].data, 'Follow-up terminal message');
          assert.equal(fixture.terminalInputs[3].data, '\r');
        }
      }, options());
    });
  }
}
