import assert from 'node:assert/strict';
import { inspect } from 'node:util';
import { expect, test } from 'playwright/test';
import { fixtureSession, openTicket, populatedStats, ready, routeTo, settingsRaw, stamp } from './browser_fixture.mjs';
import {
  assistantSurface, automationDetailReady, detailPanel, inViewport, openAssistantControl,
  parityAssistantOptions, parityChild, parityEpicRecords, phoneOutputView, polishRecords, terminalGrid,
} from './browser_scenes.mjs';

// Keep relative labels stable without pausing any timers or animation frames.
const fixedTime = new Date('2026-11-09T12:00:00Z');
async function prepareCapture(page, base) {
  await page.clock.setFixedTime(fixedTime);
  await page.addInitScript(origin => {
    if (location.origin !== origin) return;
    // The fixture seeds the theme, sidebar and saved Assistant thread separately.
    localStorage.setItem('kontora-collapsed-cols', '["cancelled"]');
    localStorage.setItem('kontora-show-badges', '1');
    localStorage.setItem('kontora-show-agent-meta', '1');
    localStorage.setItem('kontora-stats-range', '90d');
    localStorage.setItem('kontora-stats-mode', 'time');
  }, base);
}

async function settleCapture(page) {
  const tips = page.locator('[role="tooltip"]');
  const openTips = page.locator('[role="tooltip"][aria-hidden="false"]');
  // Escape without an active tip could dismiss the scene itself. Dismiss before
  // moving away so Escape does not race the delayed mouseleave dismissal.
  for (let remaining = await tips.count(); remaining > 0 && await openTips.count(); remaining--) {
    await page.keyboard.press('Escape');
  }
  await page.mouse.move(0, page.viewportSize().height - 1);
  await page.waitForFunction(() => [...document.querySelectorAll('[role="tooltip"]')]
    .every(tip => tip.getAttribute('aria-hidden') === 'true' && getComputedStyle(tip).opacity === '0'));
  await ready(page);
}

async function attachError(testInfo, name, error) {
  // Reporting problems must not obscure the original error either.
  await testInfo.attach(name, { body: inspect(error), contentType: 'text/plain' }).catch(() => {});
}

async function withCapturePage(context, testInfo, name, theme, options, run) {
  const { KONTORA_BROWSER_URL: base, KONTORA_BROWSER_ASSETS: manifest, KONTORA_BROWSER_ENGINE: engine } = process.env;
  assert.ok(base && manifest, 'Missing the embedded asset server URL or asset manifest. Run `make test-visual`.');
  assert.equal(new URL(base).hostname, '127.0.0.1');
  assert.equal(engine, 'chromium', 'The visual Go harness must select Chromium');
  const stopSetup = new AbortController();
  const session = fixtureSession(context, { base, assets: new Set(JSON.parse(manifest)), engine }, {
    ...options,
    beforeNavigation: async page => {
      await prepareCapture(page, base);
      stopSetup.signal.throwIfAborted();
    },
  });
  let page, setup, pending = false, failed = false;
  try {
    // Leave time in the test budget to capture failure evidence and unload.
    await test.step('Set up scene', () => {
      pending = true;
      setup = (async () => {
        page = await session.preparePage(theme);
        stopSetup.signal.throwIfAborted();
        await session.navigate(page);
        stopSetup.signal.throwIfAborted();
        await run(page, session.fixture);
        stopSetup.signal.throwIfAborted();
        await settleCapture(page);
      })().catch(error => {
        if (!stopSetup.signal.aborted) throw error;
      }).finally(() => { pending = false; });
      return setup;
    }, { timeout: 20_000 });
    session.check();
    expect(session.fixture.mutations).toEqual([]);
    await expect(page).toHaveScreenshot(`${name}.png`);
    session.check();
  } catch (error) {
    failed = true;
    stopSetup.abort();
    const evidencePage = page ?? context.pages()[0];
    if (evidencePage && !evidencePage.isClosed()) {
      try {
        const path = testInfo.outputPath('pre-cleanup.png');
        await evidencePage.screenshot({ path, animations: 'disabled', caret: 'hide', scale: 'css', timeout: 3000 });
        await testInfo.attach('pre-cleanup', { path, contentType: 'image/png' });
      } catch (evidenceError) {
        await attachError(testInfo, 'Pre-cleanup screenshot error', evidenceError);
      }
    }
    throw error;
  } finally {
    let cleanupError;
    async function cleanup(name, run) {
      try {
        await run();
      } catch (error) {
        cleanupError ??= error;
        await attachError(testInfo, name, error);
      }
    }
    for (const page of context.pages()) page.setDefaultNavigationTimeout(5000);
    await cleanup('Fixture cleanup error', session.unload);
    if (pending) {
      // A step timeout does not cancel its body. After unloading with routing
      // intact, close pages to unblock remaining waits and join the setup task.
      await cleanup('Scene setup cleanup error', async () => {
        const closed = await Promise.allSettled(context.pages().map(page => page.close()));
        await setup;
        const errors = closed.filter(result => result.status === 'rejected').map(result => result.reason);
        if (errors.length) throw new AggregateError(errors, 'Could not close visual scene pages');
      });
    }
    await cleanup('Escaped browser requests', async () => {
      // Use Playwright's request client, not a request from the page or its routes.
      const response = await context.request.get(new URL('/__visual/escaped-requests', base).href, {
        headers: { 'X-Kontora-Visual-Diagnostics': '1' },
        timeout: 3000,
        maxRedirects: 0,
      });
      assert.equal(response.status(), 200, 'Could not read the embedded asset server’s escaped-request log');
      assert.deepEqual(await response.json(), [], 'Requests escaped browser interception');
    });
    if (cleanupError && !failed) throw cleanupError;
  }
}

async function populatedBoard(page) {
  const phone = page.viewportSize().width < 768;
  if (phone) {
    const board = page.locator('.mobile-board-layer');
    await expect(board.getByText('Scheduled pickup with long pipeline metadata', { exact: true })).toBeInViewport();
    await expect(board.getByText('Primary draft', { exact: true })).toBeInViewport();
  } else {
    await expect(page.getByRole('listitem', { name: /^Ticket kon-scheduled:/ })).toBeInViewport();
    await expect(page.getByRole('listitem', { name: /^Ticket kon-done:/ })).toBeInViewport();
  }
}

async function emptyBoard(page) {
  await expect(page.locator('#board-empty-mobile')).toBeVisible();
}

async function ticketPanel(page) {
  await openTicket(page, 'kon-draft');
  await detailPanel(page, 'Ticket');
  const detail = page.locator('[data-ticket-detail]');
  await expect(detail.getByRole('heading', { name: 'Primary draft', exact: true }).first()).toBeVisible();
  await expect(detail.getByText('Fixture description.', { exact: true }).filter({ visible: true })).toBeVisible();
}

async function archiveConfirmation(page) {
  const card = page.locator('[data-ticket-id="kon-done"]');
  const menu = card.locator('.card-menu-btn');
  await menu.focus();
  await menu.press('Enter');
  await card.locator('.card-menu [data-act="archive"]').click();

  const dialog = page.getByRole('dialog', { name: 'Archive kon-done', exact: true });
  const heading = dialog.getByRole('heading', { name: 'Archive kon-done', exact: true });
  const note = dialog.getByPlaceholder(/^Why \(optional\)/);
  const cancel = dialog.getByRole('button', { name: 'cancel', exact: true });
  const archive = dialog.getByRole('button', { name: 'archive', exact: true });
  await expect(cancel).toBeFocused();
  // Pointer activation hides Chromium's keyboard focus outline.
  await page.keyboard.press('Tab');
  await page.keyboard.press('Shift+Tab');
  await expect(cancel.and(page.locator(':focus-visible'))).toBeFocused();
  await expect(note).toHaveValue('');
  for (const control of [heading, note, cancel, archive]) {
    await expect(control).toBeInViewport({ ratio: 1 });
  }
}

function notesRecoveryOptions() {
  const author = 'Fixture author';
  return { config: { author }, ticket: { notes: [
    { id: 'parent-note', at: stamp, author, text: 'Review the release checklist before publishing.', reactions: [] },
  ] } };
}

async function notesRecovery(page) {
  const detail = page.locator('.mobile-detail');
  const notes = detail.locator('.detail-notes');
  const draft = 'I checked the release notes. The rollout checklist still needs a second look.';
  await openTicket(page, 'kon-draft');
  await detailPanel(page, 'Notes');
  await notes.getByRole('button', { name: 'Reply', exact: true }).click();
  await notes.getByRole('textbox', { name: 'Reply', exact: true }).fill(draft);
  await detail.getByRole('button', { name: 'Back', exact: true }).filter({ visible: true }).click();
  await openTicket(page, 'kon-draft');

  const recovery = notes.getByRole('region', { name: 'Recoverable note draft', exact: true });
  const reason = recovery.getByRole('status');
  const retained = recovery.getByLabel('Retained note text', { exact: true });
  const restore = recovery.getByRole('button', { name: 'Restore to editor (not sent)', exact: true });
  const discard = recovery.getByRole('button', { name: 'Discard', exact: true });
  const composer = notes.getByRole('textbox', { name: 'Add a note', exact: true });
  await expect(reason).toHaveText('Unsent draft retained from this ticket.');
  await expect(retained).toHaveText(draft);
  await expect(restore).toBeEnabled();
  // The detail view restores its saved scroll offset on the next animation frame after Alpine updates.
  await page.evaluate(() => new Promise(resolve => {
    Alpine.nextTick(() => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  }));
  await recovery.scrollIntoViewIfNeeded();
  for (const control of [recovery, reason, retained, restore, discard, composer]) {
    await expect(control).toBeInViewport({ ratio: 1 });
  }
}

async function epicOverview(page) {
  await routeTo(page, '#/e/kon-order');
  await expect(page.locator('.mobile-epic-resting-header').getByRole('heading', { name: 'Release epic with retained order', exact: true }))
    .toBeVisible();
  await expect(parityChild(page, 'kon-open-a')).toBeVisible();
  await expect(parityChild(page, 'kon-review')).toBeVisible();
}

async function recordedAssistant(page) {
  await openTicket(page, 'kon-draft');
  await openAssistantControl(page);
  const surface = assistantSurface(page);
  await expect(surface.getByText('fixture-reasoning-model', { exact: true })).toBeVisible();
  const results = [];
  for (const [tool, result] of [
    ['read_ticket', 'Recorded tool result: primary draft remains open.'],
    ['inspect_missing', 'Recorded tool error: ticket does not exist.'],
  ]) {
    const row = surface.locator('.assistant-tool-row').filter({ hasText: tool });
    await row.getByRole('button').click();
    const output = row.getByText(result, { exact: true });
    await expect(output).toBeVisible();
    results.push(output);
  }
  const proposal = surface.locator('[id$="-proposal"]');
  const approve = proposal.getByRole('button', { name: 'Approve', exact: true });
  await approve.scrollIntoViewIfNeeded();
  await expect(approve).toBeInViewport();
  await expect(proposal.getByText('move_ticket', { exact: true })).toBeInViewport();
  for (const output of results) await expect(output).toBeInViewport();
}

async function statsView(page) {
  await routeTo(page, '#/stats');
  await page.waitForFunction(() => {
    const app = Alpine.$data(document.body);
    return !app.statsLoading && !app.statsError && app.stats?.totals?.runs > 0;
  });
  // The Stats data can be ready before Alpine renders the weekly bars as HTML elements.
  const stats = page.locator('#stats-view');
  const bars = stats.locator('.stats-activity [data-tip-t]').filter({ has: page.locator('[style*="height"]') });
  await expect(bars).toHaveCount(populatedStats().buckets.length);
  await expect(bars.last()).toBeVisible();
  await expect(stats.getByRole('button', { name: /^Fixture repository · \d+/ })).toBeVisible();
}

async function settingsGeneral(page) {
  await routeTo(page, '#/settings/general');
  const general = page.locator('#settings-section-general');
  await expect(general.getByRole('heading', { name: 'Execution', exact: true })).toBeVisible();
  await expect(general.locator('#gen-max_concurrent_agents')).toHaveValue('3');
}

async function automationDetail(page) {
  await routeTo(page, '#/automations/auto-review');
  await automationDetailReady(page, 'auto-review');
  await expect(page.getByRole('button', { name: 'Hide sidebar', exact: true })).toBeVisible();
  const detail = page.locator('.automations-detail');
  await expect(detail.getByRole('heading', { name: '[Fixture repository] Repository review', exact: true })).toBeVisible();
  const rail = detail.locator('.automations-rail');
  await expect(rail.locator('time')).toHaveCount(4);
  await expect(rail.getByText('19 of 20 runs priced', { exact: true })).toBeVisible();
}

async function automationCompletedLog(page) {
  await routeTo(page, '#/automations/auto-review');
  await automationDetailReady(page, 'auto-review');
  const detail = page.locator('.automations-detail');
  await detail.getByRole('button', { name: '#29', exact: true }).filter({ visible: true }).click();
  await expect(detail.getByText('run #29', { exact: true })).toBeInViewport({ ratio: 1 });

  const lines = detail.locator('.automations-log-line');
  const expected = [
    ['kontora', 'automation auto-review · run #29 · trigger schedule'],
    ['agent', 'Inspect repository'],
    ['agent', 'No regressions <script>not markup</script>'],
  ];
  await expect(lines).toHaveCount(expected.length);
  for (const [index, [source, message]] of expected.entries()) {
    const row = lines.nth(index);
    await expect(row.getByText(source, { exact: true })).toBeInViewport({ ratio: 1 });
    await expect(row.getByText(message, { exact: true })).toBeInViewport({ ratio: 1 });
  }
}

async function readOnlyTerminal(page, fixture) {
  await openTicket(page, 'kon-live');
  await phoneOutputView(page, 'Terminal');
  await expect(page.locator('.mobile-terminal-status')).toHaveText('Connected');
  await expect.poll(() => page.evaluate(() => {
    const { term, ws, requestedDimensions, historyReady } = window.termState;
    if (!term) return null;
    const buffer = term.buffer.active;
    return {
      connected: ws?.readyState === WebSocket.OPEN,
      readOnly: term.options.disableStdin,
      fitted: term.cols === requestedDimensions?.cols && term.rows === requestedDimensions?.rows,
      historyReady,
      lines: [0, 1].map(i => buffer.getLine(buffer.viewportY + i)?.translateToString(true).trimEnd()),
    };
  })).toEqual({ connected: true, readOnly: true, fitted: true, historyReady: true, lines: ['Offline fixture terminal', '$'] });
  await page.waitForFunction(terminalGrid, { minRows: 2 });
  expect(fixture.sockets.length).toBeGreaterThan(0);
  expect(fixture.sockets.every(url => !url.searchParams.has('rw'))).toBe(true);
  expect(fixture.messages.filter(message => message.type === 'input')).toEqual([]);
}

async function terminalCompose(page, fixture) {
  await readOnlyTerminal(page, fixture);
  const detail = page.locator('.mobile-detail');
  const composer = detail.getByRole('textbox', { name: 'Message the agent', exact: true });
  const draft = 'Check the notes.\nKeep the patch small.';
  await composer.click();
  await composer.fill(draft);
  await page.waitForFunction(() => {
    const { ws } = window.termState;
    return ws?.readyState === WebSocket.OPEN && new URL(ws.url).searchParams.get('rw') === '1';
  });
  await page.waitForFunction(terminalGrid, { minRows: 2 });
  const { url, rows } = await page.evaluate(() => ({ url: window.termState.ws.url, rows: window.termState.term.rows }));
  const index = fixture.sockets.findLastIndex(socket => socket.href === url);
  assert.ok(index >= 0, 'The writable socket must be intercepted');
  const route = fixture.socketRoutes[index];
  const marker = 'last-row';
  fixture.terminalMarkers.set(route, marker);
  route.send(Buffer.from(`\x1b[2J\x1b[${rows};1H${marker}`));
  await page.waitForFunction(terminalGrid, { marker, minRows: 2 });

  await expect(composer).toHaveValue(draft);
  await expect(composer).toBeFocused();
  const done = detail.locator('.mobile-terminal-bar').getByRole('button', { name: 'Done', exact: true });
  const send = detail.getByRole('button', { name: 'Send', exact: true });
  await expect(send).toBeEnabled();
  for (const control of [composer, done, send]) await inViewport(control);
  await expect(detail.getByRole('group', { name: 'Terminal keys', exact: true, includeHidden: true })).toBeHidden();
  expect(fixture.messages.filter(message => message.type === 'input')).toEqual([]);
}

const scenes = [
  { name: 'board-desktop-dark', width: 1280, theme: 'dark', options: () => ({ records: polishRecords() }), run: populatedBoard },
  { name: 'board-mobile-dark', width: 390, theme: 'dark', options: () => ({ records: polishRecords() }), run: populatedBoard },
  { name: 'board-empty-mobile-light', width: 390, theme: 'light', options: () => ({ empty: true }), run: emptyBoard },
  { name: 'ticket-panel-desktop-light', width: 1280, theme: 'light', run: ticketPanel },
  { name: 'archive-confirm-short-desktop-light', width: 1280, height: 293, theme: 'light', run: archiveConfirmation },
  { name: 'notes-recovery-mobile-light', width: 390, theme: 'light', options: notesRecoveryOptions, run: notesRecovery },
  { name: 'epic-overview-mobile-dark', width: 390, theme: 'dark', options: () => ({ records: parityEpicRecords() }), run: epicOverview },
  { name: 'assistant-recorded-desktop-light', width: 1280, theme: 'light', options: parityAssistantOptions, run: recordedAssistant },
  { name: 'assistant-recorded-mobile-dark', width: 390, theme: 'dark', options: parityAssistantOptions, run: recordedAssistant },
  { name: 'stats-desktop-light', width: 1280, theme: 'light', options: () => ({ populatedStats: true }), run: statsView },
  { name: 'settings-general-mobile-light', width: 390, theme: 'light', options: () => ({ rawConfig: settingsRaw }), run: settingsGeneral },
  { name: 'automation-desktop-sidebar-dark', width: 1321, theme: 'dark', options: () => ({ sidebar: true }), run: automationDetail },
  { name: 'automation-completed-log-mobile-light', width: 390, theme: 'light', run: automationCompletedLog },
  { name: 'terminal-read-only-mobile-dark', width: 390, theme: 'dark', run: readOnlyTerminal },
  { name: 'terminal-read-only-mobile-short-dark', width: 390, height: 360, theme: 'dark', run: readOnlyTerminal },
  { name: 'terminal-compose-mobile-dark', width: 390, height: 555, theme: 'dark', run: terminalCompose },
];

for (const { name, width, height = 900, theme, options, run } of scenes) {
  test.describe(() => {
    test.use({ viewport: { width, height }, colorScheme: theme, hasTouch: width < 768 });
    test(name, async ({ context }, testInfo) => {
      await withCapturePage(context, testInfo, name, theme, options?.(), run);
    });
  });
}
