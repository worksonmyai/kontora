// Run through make test-browser against embedded assets, never a daemon.
import assert from 'node:assert/strict';
import { after, before, describe, test } from 'node:test';
import { createRequire } from 'node:module';
import { chromium, webkit } from 'playwright';

const require = createRequire(import.meta.url);
assert.equal(require('playwright/package.json').version, '1.65.0');
const base = process.env.KONTORA_BROWSER_URL;
assert.ok(base, 'Run make test-browser (the Go test provides embedded loopback assets)');
assert.equal(new URL(base).hostname, '127.0.0.1');
const assets = new Set(JSON.parse(process.env.KONTORA_BROWSER_ASSETS));
const widths = [320, 390, 767, 768, 769, 1023, 1024, 1280];
const themes = ['dark', 'light'];
const source = 'secondary';
const sourcePath = '/fixtures/team-tickets';
const unavailablePath = '/fixtures/' + 'unavailable-folder-'.repeat(12);
const longID = 'kon-' + 'long-identifier-'.repeat(12);
const tail = '\n## Notes\n\nProtected note identity: browser-note-1\n\n## Audit\nDo not edit this tail.\n';
const stamp = '2026-10-01T10:00:00Z';
const liveTitle = 'Running fixture with a long title explaining terminal keyboard layout and lifecycle behavior';
const liveBranch = 'fixture/mobile-terminal-keyboard-layout-and-input-lifecycle-with-long-metadata';
const liveAgent = 'fixture-agent-with-a-long-descriptive-name';
const supervisorID = 'abcdef-abcd';
let browser;
const engine = process.env.KONTORA_BROWSER_ENGINE;
assert.ok(['chromium', 'webkit'].includes(engine), 'The Go runner must select a browser engine');
before(async () => { browser = await ({ chromium, webkit })[engine].launch({ headless: true }); });
after(async () => { await browser?.close(); });

function ticket(id, title, extra = {}) {
  const description = `# ${title}\n\nFixture description.\n\n`;
  return { id, title, status: 'open', kontora: true, source: 'primary',
    path: '/fixtures/repository-with-a-long-name', pipeline: 'standard', agent: 'fixture-agent',
    stages: ['plan', 'code'], stage: 'plan', created_at: stamp, updated_at: stamp,
    history: [], dependencies: [], children: [], links: [],
    description, description_tail: tail, body: description + tail,
    can_edit: true, can_control: true, ...extra };
}

// Keys include source and id so a primary PUT cannot save a secondary namesake.
function fixtures() {
  const records = new Map();
  const key = (folder, id) => `${folder}/${id}`;
  const put = t => records.set(key(t.source, t.id), t);
  put(ticket('kon-draft', 'Primary draft'));
  put(ticket(longID, 'Long identifier fixture'));
  put(ticket('kon-archived', 'Archived fixture', { status: 'done', archived_at: stamp }));
  put(ticket('kon-draft', 'Secondary namesake', { source, kontora: false,
    can_control: false, can_claim: false, refusal_reason: 'Fixture is edit-only',
    description: 'Secondary-only description.\n', body: 'Secondary-only description.\n' + tail }));
  put(ticket('kon-live', liveTitle, { status: 'in_progress', stage: 'code', stages: ['plan', 'code', 'test'], started_at: stamp,
    branch: liveBranch, agent: liveAgent, can_edit: false,
    history: [{ stage: 'plan', run: 0, agent: liveAgent, started_at: stamp, completed_at: stamp, exit_code: 0 }] }));
  put(ticket(supervisorID, 'Supervised fixture epic', { kind: 'epic', epic_mode: 'agent', epic_execution: 'agent',
    status: 'in_progress', stage: 'code', started_at: stamp, can_edit: false }));
  put(ticket('kon-done', 'Completed fixture', { status: 'done', stage: 'code', can_edit: false,
    summary: 'Second code run complete.', branch: 'fixture/done', history: [
      { stage: 'code', run: 0, agent: 'fixture-agent', started_at: stamp, completed_at: stamp,
        exit_code: 1, run_outcome: 'failed', summary: 'First code run needs changes.' },
      { stage: 'code', run: 1, agent: 'fixture-agent', started_at: stamp, completed_at: stamp,
        exit_code: 0, run_outcome: 'complete', summary: 'Second code run complete.' },
    ] }));
  put(ticket('kon-epic', 'Passive fixture epic', { kind: 'epic', epic_mode: 'passive' }));
  const config = { pipelines: ['standard'], default_pipeline: 'standard',
    pipeline_infos: [{ name: 'standard', stages: ['plan', 'code'] }],
    agents: ['fixture-agent'], agent_infos: [{ name: 'fixture-agent', binary: 'pi' }],
    projects: [{ name: 'Fixture repository', path: '/fixtures/repository-with-a-long-name', agent: 'fixture-agent', pipeline: 'standard' }],
    custom_statuses: [], stages: ['plan', 'code'] };
  const requests = [], mutations = [], sockets = [], socketRoutes = [], messages = [], violations = [], expectedHTTPFailures = [];
  const behavior = { createFailure: null, createWait: null, saveFailure: null, outputRevision: 0, unavailableFolder: false, sourcesFailure: false };
  let rawConfig = 'tickets_dir: /fixtures/primary\nauto_pick_up: false\nmax_concurrent_agents: 3\nagents:\n  fixture-agent:\n    binary: pi\n';
  const list = folder => [...records.values()].filter(t => t.source === folder && !t.archived_at);
  const json = (route, value, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });

  async function api(route, url) {
    const request = route.request(), method = request.method(), path = url.pathname;
    requests.push(`${method} ${path}`);
    const allowedQuery = path === '/api/stats' ? ['range', 'project', 'pipeline']
      : /\/(logs|activity)$/.test(path) ? ['stage', 'run', 'offset', 'limit', 'after'] : [];
    assert.ok([...url.searchParams.keys()].every(k => allowedQuery.includes(k)), `Unexpected query: ${url}`);
    if (method === 'GET') {
      switch (path) {
        case '/api/config': return json(route, config);
        case '/api/ticket-sources':
          if (behavior.sourcesFailure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: 'Sources offline' }, 503);
          }
          return json(route, [
            { id: 'primary', path: '/fixtures/primary', available: true },
            { id: source, path: sourcePath, available: true },
            ...(behavior.unavailableFolder ? [{ id: 'unavailable', path: unavailablePath, available: false, error: 'Fixture folder unavailable' }] : []),
          ]);
        case '/api/tickets': return json(route, { tickets: list('primary'), running_agents: 1 });
        case `/api/sources/${source}/tickets`: return json(route, list(source));
        case '/api/assistant': return json(route, { enabled: true, hint: '', autonomy: 'ask' });
        case '/api/assistant/threads': return json(route, { threads: [] });
        case '/api/assistant/companion': return json(route, { enabled: true, name: 'Kotik', health: 'ok', ai_available: false, items: [], revision: 1 });
        case '/api/events': return route.fulfill({ contentType: 'text/event-stream', body: 'retry: 3600000\n\n: offline fixture\n\n' });
        case '/api/config/raw': return json(route, { content: rawConfig });
        case '/api/stats': return json(route, { totals: { done: 1, runs: 2, first_pass_pct: 50 }, live: {}, days: [], buckets: [], stages: [], agents: [], projects: [] });
        case '/api/tickets/archived': return json(route, { tickets: [records.get('primary/kon-archived')] });
      }
    }
    if (method === 'PUT' && path === '/api/config/raw') {
      if (behavior.saveFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.saveFailure }, 503);
      }
      rawConfig = request.postDataJSON().content;
      return route.fulfill({ status: 204 });
    }
    if (method === 'POST' && path === '/api/tickets') {
      if (behavior.createWait) await behavior.createWait;
      if (behavior.createFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.createFailure }, 503);
      }
      const body = request.postDataJSON();
      assert.ok(body.title && body.path, 'Creation requires title and path');
      const created = ticket(`kon-created-${mutations.length}`, body.title, { ...body, description: body.body || '', description_tail: '', body: body.body || '' });
      put(created);
      mutations.push({ method, path, body, ticket: structuredClone(created) });
      return json(route, created, 201);
    }
    const match = path.match(/^\/api(?:\/sources\/([^/]+))?\/tickets\/([^/]+)(?:\/(cost|chain|changes|logs|activity|move|pause|retry|init))?$/);
    if (match) {
      const folder = match[1] ? decodeURIComponent(match[1]) : 'primary';
      const id = decodeURIComponent(match[2]), action = match[3];
      const record = records.get(key(folder, id));
      assert.ok(record, `Unknown source-qualified ticket: ${folder}/${id}`);
      if (method === 'GET' && !action) return json(route, record);
      if (method === 'PUT' && !action) {
        const body = request.postDataJSON();
        assert.ok(!('body' in body), 'Description edits must not replace the protected tail');
        Object.assign(record, body);
        record.body = record.description + record.description_tail;
        mutations.push({ method, path, body, ticket: structuredClone(record) });
        return json(route, record);
      }
      assert.equal(folder, 'primary', 'Secondary tickets cannot use control/output endpoints');
      if (method === 'GET') {
        switch (action) {
          case 'cost': if (id === 'kon-live') return json(route, { id, cost_usd: '0.42', tracked_runs: 3, priced_runs: 2,
            stages: [{ name: 'plan', cost_usd: '0.30', tracked_runs: 1, priced_runs: 1 },
              { name: 'code', cost_usd: '0.12', tracked_runs: 2, priced_runs: 1 }], runs: [] });
            return json(route, { id, cost_usd: record.history.length || record.status === 'in_progress' ? '0.12' : null,
            tracked_runs: record.history.length || (record.status === 'in_progress' ? 1 : 0),
            priced_runs: record.history.length || (record.status === 'in_progress' ? 1 : 0), stages: [], runs: [] });
          case 'chain': return json(route, { nodes: [], verdict: '' });
          case 'changes': return json(route, { base: 'main', files: [], commits: [] });
          case 'logs': return json(route, { content: `${id} ${url.searchParams.get('stage') || 'code'} fixture log revision ${behavior.outputRevision}\n` });
          case 'activity': return json(route, { live: record.status === 'in_progress', source: 'events',
            tape: { events: [{ kind: 'text', text: `Fixture activity revision ${behavior.outputRevision}` }] } });
        }
      }
      if (method === 'POST' && ['move', 'pause', 'retry', 'init'].includes(action)) {
        const body = request.postData() ? request.postDataJSON() : {};
        record.status = action === 'move' ? body.status : action === 'pause' ? 'paused' : 'todo';
        mutations.push({ method, path, body });
        return json(route, record);
      }
    }
    throw new Error(`Unmodelled API request: ${method} ${url}`);
  }

  async function install(context) {
    // Install BOTH interception layers before creating/navigating the page.
    // No catch-all API success, external fallthrough, service worker or live WS.
    await context.route('**/*', async route => {
      const url = new URL(route.request().url());
      try {
        assert.equal(url.origin, base, `External request: ${url}`);
        if (url.pathname.startsWith('/api/')) return await api(route, url);
        assert.equal(route.request().method(), 'GET');
        assert.ok(assets.has(url.pathname) && !url.search, `Unknown asset: ${url}`);
        return await route.continue();
      } catch (error) {
        violations.push(error.message);
        await route.abort('blockedbyclient');
      }
    });
    await context.routeWebSocket('**/*', ws => {
      try {
        const url = new URL(ws.url());
        assert.equal(url.origin, base.replace('http:', 'ws:'));
        assert.ok(['/ws/terminal/kon-live', '/ws/terminal/' + supervisorID].includes(url.pathname));
        assert.ok([...url.searchParams.keys()].every(k => ['cols', 'rows', 'rw'].includes(k)));
        assert.ok(Number(url.searchParams.get('cols')) > 0 && Number(url.searchParams.get('rows')) > 0);
        sockets.push(url);
        socketRoutes.push(ws);
        // Do not call connectToServer(): there is no terminal server at all.
        ws.send(Buffer.from('Offline fixture terminal\r\n$ '));
        ws.onMessage(message => {
          try {
            const data = JSON.parse(String(message));
            assert.ok(['resize', 'input'].includes(data.type));
            messages.push({ url, ...data });
          } catch (error) { violations.push(error.message); }
        });
      } catch (error) {
        violations.push(error.message);
        ws.close({ code: 1008, reason: 'Unknown fixture socket' });
      }
    });
    // Routed WebSockets open immediately. Hold the application's open callback
    // and readyState together to exercise a connection still in progress.
    await context.addInitScript(() => {
      Object.defineProperty(navigator, 'clipboard', { value: { writeText: async text => { window.__fixtureCopied = text; } } });
      const Socket = window.WebSocket;
      window.__fixtureSockets = [];
      window.__fixturePendingOpens = [];
      window.WebSocket = class extends Socket {
        constructor(...args) {
          super(...args);
          window.__fixtureSockets.push(this);
          const descriptor = Object.getOwnPropertyDescriptor(this, 'onopen');
          let handler;
          const deliver = event => {
            if (window.__fixtureHoldOpen) {
              const readyState = Object.getOwnPropertyDescriptor(this, 'readyState');
              Object.defineProperty(this, 'readyState', { configurable: true, value: Socket.CONNECTING });
              window.__fixturePendingOpens.push(() => {
                if (readyState) Object.defineProperty(this, 'readyState', readyState);
                else delete this.readyState;
                handler?.call(this, event);
              });
            } else handler?.call(this, event);
          };
          if (descriptor?.set) {
            Object.defineProperty(this, 'onopen', { configurable: true, set(fn) { handler = fn; descriptor.set.call(this, deliver); } });
          } else {
            Object.defineProperty(this, 'onopen', { configurable: true, set(fn) { handler = fn; } });
            this.addEventListener('open', deliver);
          }
        }
      };
    });
  }
  return { install, records, requests, mutations, sockets, socketRoutes, messages, violations, behavior, expectedHTTPFailures };
}

async function withPage(t, width, theme, run) {
  const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: theme,
    reducedMotion: 'reduce', serviceWorkers: 'block', locale: 'en-US', timezoneId: 'UTC' });
  const fixture = fixtures(), errors = [];
  await fixture.install(context);
  await context.addInitScript(theme => {
    localStorage.setItem('kontora-theme', theme);
    localStorage.setItem('kontora-assistant-open', '0');
    localStorage.setItem('kontora-sidebar-hidden', '1');
  }, theme);
  const page = await context.newPage();
  page.setDefaultTimeout(4000);
  page.setDefaultNavigationTimeout(15000);
  page.on('pageerror', error => errors.push(error.stack || error.message));
  page.on('console', msg => {
    if (msg.type() !== 'error') return;
    if (msg.text().startsWith('Failed to load resource:') && fixture.expectedHTTPFailures.includes(msg.location().url)) return;
    errors.push(msg.text());
  });
  try {
    await page.goto(base, { waitUntil: 'domcontentloaded' });
    await ready(page);
    await run(page, fixture);
  } finally {
    await context.close();
    assert.deepEqual(fixture.violations, [], 'Unexpected requests (all must be explicitly modelled)');
    assert.deepEqual(errors, [], 'Browser/Alpine/console errors');
  }
}

// Bindings are used only to locate existing controls; tests never call mutating
// component methods or replace application state. :visible excludes x-show's inactive UI.
const model = (page, name) => page.locator(`[x-model="${name}"]:visible`);
async function visible(locator) { await locator.waitFor({ state: 'visible' }); }
async function inViewport(locator) {
  await visible(locator);
  const box = await locator.boundingBox();
  const size = locator.page().viewportSize();
  assert.ok(box && box.width > 0 && box.height > 0 && box.x >= -1 && box.y >= -1 &&
    box.x + box.width <= size.width + 1 && box.y + box.height <= size.height + 1,
  `Control outside viewport: ${JSON.stringify(box)} in ${JSON.stringify(size)}`);
}
async function noPageOverflow(page) {
  const duplicateIDs = await page.evaluate(() => {
    const seen = new Set();
    return [...document.querySelectorAll('[id]')].map(el => el.id).filter(id => {
      if (!id) return false;
      if (seen.has(id)) return true;
      seen.add(id);
      return false;
    });
  });
  assert.deepEqual(duplicateIDs, [], 'Shared layouts must not duplicate DOM IDs');
  const dimensions = await page.evaluate(() => ({ viewport: innerWidth,
    html: document.documentElement.scrollWidth, body: document.body.scrollWidth }));
  assert.ok(dimensions.html <= dimensions.viewport + 1 && dimensions.body <= dimensions.viewport + 1,
    `Page-level horizontal overflow: ${JSON.stringify(dimensions)}`);
}
async function ready(page) {
  await page.waitForFunction(() => {
    if (!window.Alpine) return false;
    const app = Alpine.$data(document.body);
    return !app.loading && app._applyingRoute === null && !app.detailLoading && !app.settingsLoading;
  });
  await page.evaluate(() => document.fonts.ready);
}
async function routeTo(page, route) {
  await ready(page);
  await page.evaluate(route => { location.hash = route; }, route);
  await page.waitForFunction(route => {
    const app = Alpine.$data(document.body);
    return location.hash === route && app.routeHash() === route && app._applyingRoute === null;
  }, route);
  await ready(page);
}
async function openTicket(page, id, folder = 'primary') {
  await routeTo(page, folder === 'primary' ? `#/t/${id}` : `#/s/${folder}/t/${id}`);
  await page.waitForFunction(({ id, folder }) => {
    const app = Alpine.$data(document.body);
    return app.selectedTicket?.id === id && (app.selectedTicket.source || 'primary') === folder && !app.detailLoading;
  }, { id, folder });
}
async function backToBoard(page) {
  await routeTo(page, '#/');
  await page.waitForFunction(() => !Alpine.$data(document.body).selectedTicket && !Alpine.$data(document.body).selectedEpic);
}

async function swipeBoard(page, dx) {
  await page.locator('.mobile-board-layer [\\@touchend]').evaluate((el, dx) => {
    for (const [name, x] of [['touchstart', 160], ['touchend', 160 + dx]]) {
      const event = new Event(name, { bubbles: true });
      Object.defineProperty(event, 'changedTouches', { value: [{ clientX: x, clientY: 300 }] });
      el.dispatchEvent(event);
    }
  }, dx);
}

async function visualViewport(page, height, top = 0) {
  await page.evaluate(({ height, top }) => {
    Object.defineProperties(window.visualViewport, {
      height: { configurable: true, value: height }, offsetTop: { configurable: true, value: top },
    });
    window.visualViewport.dispatchEvent(new Event('resize'));
    window.visualViewport.dispatchEvent(new Event('scroll'));
  }, { height, top });
}

async function terminalRows(page, height, top = 0) {
  await page.waitForFunction(({ height, top }) => {
    const host = document.getElementById('terminal-container-mobile');
    const term = window.termState.term;
    if (!host || !term?.element) return false;
    const overlay = host.closest('.mobile-detail').getBoundingClientRect();
    if (Math.abs(overlay.height - height) > 1 || Math.abs(overlay.top - top) > 1) return false;
    const box = host.getBoundingClientRect();
    const cell = term._core._renderService.dimensions.css.cell.height;
    const controls = [...document.querySelectorAll('.mobile-detail-header button, .mobile-terminal-pane button, .mobile-detail-actions button')]
      .map(el => el.getBoundingClientRect()).filter(rect => rect.width && rect.height);
    return cell > 0 && term.rows >= 8 && box.height >= term.rows * cell &&
      box.bottom <= height + top + 1 && term.element.getBoundingClientRect().top + term.rows * cell <= box.bottom + 1 &&
      controls.every(rect => rect.width >= 44 && rect.height >= 44 && rect.top >= top && rect.bottom <= height + top + 1);
  }, { height, top }).catch(async error => {
    const geometry = await page.evaluate(() => {
      const host = document.getElementById('terminal-container-mobile');
      const term = window.termState.term;
      return { host: host?.getBoundingClientRect().toJSON(), rows: term?.rows,
        cell: term?._core._renderService.dimensions.css.cell.height,
        terminal: term?.element?.getBoundingClientRect().toJSON(),
        overlay: host?.closest('.mobile-detail').getBoundingClientRect().toJSON(),
        controls: [...document.querySelectorAll('.mobile-detail-header button, .mobile-terminal-pane button, .mobile-detail-actions button')]
          .map(el => ({ text: el.textContent, box: el.getBoundingClientRect().toJSON() })).filter(el => el.box.width && el.box.height) };
    });
    throw new Error(`${error.message}: ${JSON.stringify(geometry)}`, { cause: error });
  });
  assert.equal(await page.locator('#terminal-container-mobile .xterm').count(), 1);
}

describe('embedded responsive browser UI', { concurrency: 4 }, () => {
  for (const width of [320, 390]) for (const theme of themes) {
    test(`board chrome and status navigation ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await t.test('running count shares the app row without shrinking actions', async () => {
          const bar = page.locator('.mobile-app-bar');
          const mark = await bar.locator('.mobile-wordmark').boundingBox();
          const pill = await bar.locator('.mobile-running-pill').boundingBox();
          assert.ok(pill.y >= mark.y && pill.y + pill.height <= mark.y + mark.height + 1, 'Running count stays within the wordmark block');
          const running = [...fixture.records.values()].filter(ticket => ticket.source === 'primary' && ticket.status === 'in_progress').length;
          assert.equal(await bar.locator('.mobile-running-pill').getAttribute('aria-label'), `${running} running`);
          for (const name of ['Open navigation', 'Assistant', 'Toggle theme', 'New ticket']) {
            const control = bar.getByRole('button', { name, exact: true });
            await inViewport(control);
            const box = await control.boundingBox();
            assert.ok(box.width >= 44 && box.height >= 44);
            assert.ok(mark.y >= box.y - 1 && mark.y + mark.height <= box.y + box.height + 1, 'Wordmark and count fit in the action row');
          }
          await inViewport(page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true }));
          await noPageOverflow(page);
        });
        await t.test('offscreen activation and swipes reveal the pressed status only', async () => {
          const strip = page.getByRole('navigation', { name: 'Ticket status', exact: true });
          const buttons = strip.getByRole('button');
          assert.equal(await strip.evaluate(el => getComputedStyle(el).backgroundAttachment), 'local, local, scroll, scroll');
          const labels = await page.evaluate(() => Alpine.$data(document.body).columns.map(col => col.label));
          assert.deepEqual(await buttons.locator('span:nth-child(2)').allTextContents(), labels);
          const last = buttons.last();
          const before = await last.boundingBox();
          assert.ok(before.x + before.width > width, 'The last status starts offscreen');
          const y = await page.evaluate(() => scrollY);
          await last.evaluate(el => el.focus({ preventScroll: true }));
          await page.keyboard.press('Enter');
          const selected = async index => {
            await page.waitForFunction(index => {
              const strip = document.querySelector('.mobile-status-tabs');
              const buttons = strip.querySelectorAll('button');
              const button = buttons[index];
              const box = button.getBoundingClientRect(), view = strip.getBoundingClientRect();
              const style = getComputedStyle(strip);
              const left = view.left + strip.clientLeft + parseFloat(style.paddingLeft);
              const right = view.left + strip.clientLeft + strip.clientWidth - parseFloat(style.paddingRight);
              return button.getAttribute('aria-pressed') === 'true' &&
                [...buttons].filter(el => el.getAttribute('aria-pressed') === 'true').length === 1 &&
                box.left >= left - 1 && box.right <= right + 1;
            }, index);
            assert.equal(await page.evaluate(() => scrollY), y, 'Status scrolling must not move the page');
            const count = Number(await buttons.nth(index).locator('span').last().textContent());
            assert.equal(await page.locator('.mobile-board-layer [x-text="ticket.title"]').count(), count, 'Count matches displayed cards');
          };
          await selected(labels.length - 1);
          assert.equal(await last.evaluate(el => document.activeElement === el), true, 'Activation keeps keyboard focus');
          const swipe = dx => swipeBoard(page, dx);
          for (let index = labels.length - 2; index >= 0; --index) {
            await swipe(100);
            await selected(index);
            assert.equal(await last.evaluate(el => document.activeElement === el), true, 'Swipes must not change keyboard focus');
          }
          for (let index = 1; index < labels.length; ++index) {
            await swipe(-100);
            await selected(index);
          }
          await swipe(-100);
          await selected(labels.length - 1);
          await buttons.first().click();
          await selected(0);
          await swipe(100);
          await selected(0);
          await noPageOverflow(page);
        });
        for (const scenario of [
          { name: 'oversized status', status: 'awaiting_external_dependency_approval_review', oversized: true },
          { name: 'safe-area status', status: 'awaiting_user_review', padding: '44px' },
          { name: 'oversized safe-area status', status: 'awaiting_external_dependency_approval_review', padding: '44px', oversized: true },
        ]) {
          await t.test(`${scenario.name} stays aligned after boundary activation and swipes`, async () => {
            const key = `${source}/kon-geometry`;
            fixture.records.set(key, ticket('kon-geometry', 'Status geometry fixture', { source, status: scenario.status, kontora: false }));
            await page.reload({ waitUntil: 'domcontentloaded' });
            await ready(page);
            const strip = page.getByRole('navigation', { name: 'Ticket status', exact: true });
            if (scenario.padding) await strip.evaluate((el, padding) => {
              el.style.paddingLeft = padding;
              el.style.paddingRight = padding;
            }, scenario.padding);
            const last = strip.getByRole('button').last();
            await last.evaluate(el => el.focus({ preventScroll: true }));
            const y = await page.evaluate(() => scrollY);
            await page.keyboard.press('Enter');
            await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button:last-child')?.getAttribute('aria-pressed') === 'true');
            await page.evaluate(() => Alpine.nextTick());
            const geometry = () => last.evaluate(el => {
              const strip = el.parentElement, view = strip.getBoundingClientRect(), box = el.getBoundingClientRect();
              const style = getComputedStyle(strip);
              return { left: box.left, right: box.right, width: box.width, scroll: strip.scrollLeft,
                safeLeft: view.left + strip.clientLeft + parseFloat(style.paddingLeft),
                safeRight: view.left + strip.clientLeft + strip.clientWidth - parseFloat(style.paddingRight) };
            });
            const initial = await geometry();
            assert.equal(initial.width > initial.safeRight - initial.safeLeft, !!scenario.oversized, 'Fixture exercises the intended width');
            if (scenario.oversized) assert.ok(Math.abs(initial.left - initial.safeLeft) <= 1, JSON.stringify(initial));
            else assert.ok(initial.left >= initial.safeLeft - 1 && initial.right <= initial.safeRight + 1, JSON.stringify(initial));
            for (const action of ['swipe', 'activate', 'swipe', 'activate']) {
              if (action === 'swipe') await swipeBoard(page, -100);
              else await page.keyboard.press('Enter');
              await page.evaluate(() => Alpine.nextTick());
              const repeated = await geometry();
              assert.ok(Math.abs(repeated.scroll - initial.scroll) <= 1, `Boundary ${action} moved the strip: ${JSON.stringify({ initial, repeated })}`);
              assert.equal(await last.getAttribute('aria-pressed'), 'true');
              assert.equal(await last.evaluate(el => document.activeElement === el), true);
              assert.equal(await page.evaluate(() => scrollY), y);
            }
            await noPageOverflow(page);
            fixture.records.delete(key);
          });
        }
        await t.test('combined folders retain counts, search and source errors', async () => {
          fixture.behavior.unavailableFolder = true;
          await page.reload({ waitUntil: 'domcontentloaded' });
          await ready(page);
          assert.equal(await page.getByRole('combobox', { name: 'Filter by ticket folder', exact: true }).count(), 0);
          const error = page.locator('.mobile-chrome [role="alert"]');
          await visible(error);
          assert.ok((await error.textContent()).includes(unavailablePath + ': Fixture folder unavailable'));
          const open = page.getByRole('navigation', { name: 'Ticket status', exact: true }).getByRole('button').first();
          const cards = page.locator('.mobile-board-layer [x-text="ticket.title"]');
          assert.equal(Number(await open.locator('span').last().textContent()), 3);
          assert.equal(await cards.count(), 3);
          const titles = await cards.allTextContents();
          for (const title of ['Primary draft', 'Secondary namesake']) assert.ok(titles.includes(title));
          const search = page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true });
          for (const title of ['Primary draft', 'Secondary namesake']) {
            await search.fill(title);
            await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button span:last-child').textContent === '1');
            await page.waitForFunction(title => {
              const cards = document.querySelectorAll('.mobile-board-layer [x-text="ticket.title"]');
              return cards.length === 1 && cards[0].textContent === title;
            }, title);
            assert.deepEqual(await cards.allTextContents(), [title]);
            await visible(error);
            await noPageOverflow(page);
          }
          await search.fill('');
          await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button span:last-child').textContent === '3');
          fixture.behavior.sourcesFailure = true;
          await page.reload({ waitUntil: 'domcontentloaded' });
          await ready(page);
          await visible(error);
          assert.ok((await error.textContent()).includes('Failed to load ticket sources'));
          await noPageOverflow(page);
        });
      });
    });
  }
  for (const width of [320, 390]) for (const theme of themes) {
    test(`terminal visible geometry ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        await visible(page.locator('#terminal-container-mobile .xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        for (const height of [360, 420, 900]) {
          await visualViewport(page, height, height === 900 ? 0 : 12);
          assert.equal(await page.evaluate(() => innerHeight), 900, 'Only the visual viewport shrinks');
          await terminalRows(page, height, height === 900 ? 0 : 12);
          const status = page.locator('.mobile-terminal-toolbar [role="status"]');
          assert.equal(await page.locator('.mobile-detail-actions').isVisible(), height >= 500);
          await page.evaluate(() => { window.__fixtureHoldOpen = true; });
          await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).click();
          await page.waitForFunction(() => window.__fixturePendingOpens.length > 0);
          assert.equal(await status.textContent(), 'Connecting…');
          assert.equal(await page.locator('.mobile-detail-actions').isVisible(), false);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
          const inputs = fixture.messages.filter(message => message.type === 'input').length;
          await page.locator('#terminal-container-mobile .xterm-screen').click();
          await page.keyboard.type('must-not-send');
          assert.equal(fixture.messages.filter(message => message.type === 'input').length, inputs);
          await terminalRows(page, height, height === 900 ? 0 : 12);
          await page.evaluate(() => {
            window.__fixtureHoldOpen = false;
            window.__fixturePendingOpens.splice(0).forEach(open => open());
          });
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), false);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el === document.activeElement), false,
            'A tap during connection must not open the keyboard when readiness changes');
          await terminalRows(page, height, height === 900 ? 0 : 12);
          await page.locator('#terminal-container-mobile .xterm-screen').click();
          await page.keyboard.type('fixture-input');
          await page.waitForFunction(() => document.activeElement?.classList.contains('xterm-helper-textarea'));
          const done = page.getByRole('button', { name: 'Done typing', exact: true }).filter({ visible: true });
          if (height === 420) await page.keyboard.press('Escape');
          else await done.click();
          await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).waitFor();
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el === document.activeElement), false);
          assert.equal(new URL(page.url()).hash, '#/t/kon-live');
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          await fixture.socketRoutes.at(-1).close({ code: 1000, reason: 'Fixture disconnect' });
          await page.waitForFunction(() => !!Alpine.$data(document.body).terminalError);
          assert.equal(await status.textContent(), 'Disconnected');
          await terminalRows(page, height, height === 900 ? 0 : 12);
          await page.getByRole('button', { name: 'retry connection', exact: true }).filter({ visible: true }).click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        }
        assert.ok(fixture.messages.some(message => message.type === 'input' && message.url.searchParams.get('rw') === '1'));
        assert.ok(fixture.messages.some(message => message.type === 'resize' && message.rows >= 8));
        assert.equal(await page.evaluate(() => window.__fixtureSockets.filter(ws => ws.readyState < 2).length), 1);
        for (const height of [360, 420, 900]) {
          await page.setViewportSize({ width, height });
          await visualViewport(page, height);
          await terminalRows(page, height);
        }
        const detail = page.locator('.mobile-detail');
        assert.equal(await detail.locator('.mobile-detail-title').textContent(), liveTitle);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        assert.equal(await detail.getByRole('button', { name: '≡ logs', exact: true }).getAttribute('aria-pressed'), 'true');
        const stages = detail.getByLabel('Stages and cost estimates');
        await visible(stages);
        assert.equal(await stages.getByRole('button').count(), 3, 'Each stage has one control, including unpriced stages');
        assert.match(await stages.textContent(), /1\/2/);
        await stages.getByRole('button', { name: /^test:/ }).click();
        assert.equal(await detail.getByRole('button', { name: '≡ logs', exact: true }).getAttribute('aria-pressed'), 'true');
        await stages.getByRole('button', { name: /^plan:/ }).click();
        await visible(detail.getByText('kon-live plan fixture log revision 0', { exact: true }));
        await stages.getByRole('button', { name: /^code:/ }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).detailTab === 'terminal' && Alpine.$data(document.body).terminalConnected);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        assert.equal(await detail.locator('.mobile-detail-title').textContent(), liveTitle);
        await detail.getByRole('button', { name: '▤ ticket', exact: true }).click();
        assert.equal(await detail.locator('h1[x-text="selectedTicket?.title"]').textContent(), liveTitle);
        await visible(detail.getByText(liveAgent, { exact: true }));
        await visible(detail.getByText(liveBranch, { exact: true }));
        const copy = detail.getByRole('button', { name: 'Copy attach command', exact: true });
        await copy.focus();
        await page.keyboard.press('Enter');
        assert.equal(await page.evaluate(() => window.__fixtureCopied), 'kontora attach kon-live');
        await detail.getByRole('button', { name: '›_ terminal', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
        await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        await detail.getByRole('button', { name: '›_ terminal', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
      });
    });
  }
  for (const width of widths) for (const theme of themes) {
    test(`${width}px / ${theme}`, { timeout: 110000 }, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        // The fixture store deliberately survives to verify persistence.
        t.beforeEach(async () => {
          await page.setViewportSize({ width, height: 900 });
          await page.goto(base, { waitUntil: 'domcontentloaded' });
          await ready(page);
        });
        await t.test('header actions, theme and local assets', async () => {
          const toggle = page.getByRole('button', { name: 'Toggle theme', exact: true }).filter({ visible: true });
          for (const name of ['Toggle theme', 'Assistant', 'New ticket']) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          await inViewport(page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true }));
          assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
          await toggle.click();
          assert.equal(await page.locator('html').getAttribute('data-theme'), theme === 'dark' ? 'light' : 'dark');
          await toggle.click();
          await noPageOverflow(page);
          for (const path of ['/api/config', '/api/tickets', '/api/ticket-sources', `/api/sources/${source}/tickets`, '/api/events']) {
            assert.ok(fixture.requests.includes(`GET ${path}`), `Expected intercepted ${path}`);
          }
        });
        await t.test('long identifiers keep ticket actions reachable', async () => {
          await openTicket(page, longID);
          await noPageOverflow(page);
          for (const name of ['queue', /^(?:✕ )?cancel$/]) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          const more = width < 768 ? page.getByRole('button', { name: 'Actions', exact: true })
            : page.getByRole('banner').getByRole('button', { name: 'More actions', exact: true });
          await inViewport(more);
          await more.click();
          if (width < 768) {
            await visible(page.getByRole('dialog', { name: 'Ticket actions' }));
            await page.keyboard.press('Escape');
          } else {
            await visible(page.getByRole('button', { name: 'delete file', exact: true }));
            await more.click();
          }
          await page.setViewportSize({ width, height: 420 });
          await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '420px');
          for (const name of ['queue', /^(?:✕ )?cancel$/]) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          await inViewport(more);
          await noPageOverflow(page);
        });
        for (const [route, selector] of [
          ['#/new', '[x-model="createForm.title"]'],
          ['#/stats', '.stats-title'],
          ['#/archive', '[aria-label="Filter archive"]'],
          ['#/settings', width <= 1100 ? '#settings-mobile-section' : '[aria-label="Search settings"]'],
        ]) {
          await t.test(`${route} renders and reloads`, async () => {
            await routeTo(page, route);
            await visible(page.locator(`${selector}:visible`).first());
            await noPageOverflow(page);
            await page.reload({ waitUntil: 'domcontentloaded' });
            await ready(page);
            await visible(page.locator(`${selector}:visible`).first());
            assert.equal(new URL(page.url()).hash, route);
            await noPageOverflow(page);
            await page.goBack();
            await page.waitForFunction(() => Alpine.$data(document.body).currentView === 'board');
            await ready(page);
            assert.equal(new URL(page.url()).hash, '#/');
            assert.equal(await page.evaluate(() => Alpine.$data(document.body)._statsTimer), null);
            await page.goForward();
            await visible(page.locator(`${selector}:visible`).first());
            await ready(page);
            assert.equal(new URL(page.url()).hash, route);
            await noPageOverflow(page);
          });
        }
        await t.test('Archive detail keeps its route before opening a running Board ticket', async () => {
          await routeTo(page, '#/archive');
          await page.locator('#archive-view .archive-table-scroll').getByTitle('Archived fixture', { exact: true }).click();
          await visible(page.locator('.archive-detail:visible'));
          await page.waitForFunction(() => !Alpine.$data(document.body).archiveDetailLoading);
          assert.equal(new URL(page.url()).hash, '#/archive');
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).archiveDetailOpen()), true);
          const header = page.locator('.archive-detail-header');
          for (const name of ['restore to board', 'copy id', 'Close']) {
            const control = header.getByRole('button', { name, exact: true });
            await control.scrollIntoViewIfNeeded();
            await inViewport(control);
          }
          assert.ok(await header.evaluate(el => el.scrollWidth <= el.clientWidth + 1), 'Archive controls must wrap instead of scrolling sideways');
          await noPageOverflow(page);
          await openTicket(page, 'kon-live');
          await visible(page.locator('#terminal-session:visible .xterm, #terminal-container-mobile:visible .xterm'));
          assert.equal(await page.evaluate(() => Alpine.$data(document.body)._archiveDetailId), null);
          assert.equal(new URL(page.url()).hash, '#/t/kon-live');
          assert.equal(await page.locator('.xterm:visible').count(), 1);
        });
        await t.test('creation persists the submitted fields', async () => {
          await backToBoard(page);
          await page.getByRole('button', { name: 'New ticket', exact: true }).filter({ visible: true }).click();
          await model(page, 'createForm.title').fill('Created in browser');
          await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
          await model(page, 'createForm.body').fill('Browser acceptance criteria.');
          const submit = page.getByRole('button', { name: 'create ticket', exact: true });
          await submit.scrollIntoViewIfNeeded();
          await inViewport(submit);
          await Promise.all([
            page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST'),
            submit.click(),
          ]);
          const mutation = fixture.mutations.find(m => m.method === 'POST' && m.path === '/api/tickets');
          assert.ok(mutation);
          assert.equal(mutation.body.title, 'Created in browser');
          assert.equal(mutation.body.body, 'Browser acceptance criteria.');
          assert.equal(mutation.body.path, '/fixtures/repository-with-a-long-name');
          await page.waitForURL('**/#/');
          await ready(page);
          await page.reload({ waitUntil: 'domcontentloaded' });
          await openTicket(page, mutation.ticket.id);
          await visible(page.getByRole('heading', { name: /Created in browser$/ }));
        });
        await t.test('scheduled creation validates the input and persists an open ticket', async () => {
          await routeTo(page, '#/new');
          await model(page, 'createForm.title').fill('Scheduled browser fixture');
          await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
          await page.getByRole('button', { name: 'later', exact: true }).click();
          await model(page, 'createForm.scheduled_at').fill('not a time');
          const submit = page.getByRole('button', { name: 'schedule ticket', exact: true });
          assert.ok(await submit.isDisabled(), 'Invalid schedules cannot be submitted');
          await model(page, 'createForm.scheduled_at').fill('2099-09-01 09:00');
          await submit.scrollIntoViewIfNeeded();
          await Promise.all([
            page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST'),
            submit.click(),
          ]);
          const mutation = fixture.mutations.find(m => m.body.title === 'Scheduled browser fixture');
          assert.equal(mutation?.body.status, 'open');
          assert.equal(mutation.body.scheduled_at, '2099-09-01T09:00:00Z');
        });
        await t.test('combined board card selection keeps namesakes distinct', async () => {
          await backToBoard(page);
          assert.equal(await page.getByRole('combobox', { name: 'Filter by ticket folder', exact: true }).count(), 0);
          const namesake = width < 768 ? page.getByRole('button').filter({ hasText: 'Secondary namesake', visible: true })
            : page.locator('[data-ticket-id="secondary/kon-draft"]:visible');
          await visible(namesake);
          const primary = width < 768 ? page.getByRole('button').filter({ hasText: 'Primary draft', visible: true })
            : page.locator('[data-ticket-id="kon-draft"]:visible');
          await visible(primary);
          await namesake.click();
          await page.waitForURL('**/#/s/secondary/t/kon-draft');
          await ready(page);
          assert.equal(new URL(page.url()).hash, '#/s/secondary/t/kon-draft');
          await backToBoard(page);
          await primary.click();
          await page.waitForURL('**/#/t/kon-draft');
          await ready(page);
          assert.equal(new URL(page.url()).hash, '#/t/kon-draft');
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).selectedTicket?.title), 'Primary draft');
          await backToBoard(page);
        });
        await t.test('primary description editing saves without replacing notes', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          const preview = width < 768 ? page.getByRole('button', { name: /^Edit (ticket )?description$/i })
            : page.locator('[x-ref="bodyPreview"]:visible');
          await preview.click();
          const editor = model(page, 'editForm.body');
          await editor.fill('Edited primary description.\n\n' + 'long-token-'.repeat(20));
          await editor.blur();
          await page.waitForFunction(() => Alpine.$data(document.body).selectedTicket?.description?.startsWith('Edited primary description.'));
          assert.equal(fixture.records.get('primary/kon-draft').description_tail, tail);
          assert.ok(fixture.records.get('primary/kon-draft').body.endsWith(tail));
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          await preview.click();
          assert.ok((await editor.inputValue()).startsWith('Edited primary description.'));
          await noPageOverflow(page);
        });
        await t.test('source-qualified description saves preserve the tail across reopen', async () => {
          await backToBoard(page);
          const primaryBefore = structuredClone(fixture.records.get('primary/kon-draft'));
          await openTicket(page, 'kon-draft', source);
          const preview = page.locator('[x-ref="bodyPreview"]:visible');
          const editor = model(page, 'editForm.body');
          await visible(editor.or(preview));
          if (await preview.isVisible()) await preview.click();
          await editor.fill('Edited secondary description.\n\n' + 'long-token-'.repeat(20));
          await editor.blur();
          await page.waitForFunction(() => {
            const app = Alpine.$data(document.body);
            return app.selectedTicket?.description?.startsWith('Edited secondary description.') && !app.editSubmitting;
          });
          assert.deepEqual(fixture.records.get('primary/kon-draft'), primaryBefore);
          const saved = fixture.records.get(`${source}/kon-draft`);
          assert.equal(saved.description_tail, tail);
          assert.ok(saved.body.endsWith(tail));
          assert.ok(fixture.mutations.some(m => m.method === 'PUT' && m.path === `/api/sources/${source}/tickets/kon-draft`));
          await backToBoard(page);
          await openTicket(page, 'kon-draft', source);
          await visible(editor.or(preview));
          if (await preview.isVisible()) await preview.click();
          assert.ok((await editor.inputValue()).startsWith('Edited secondary description.'));
          await noPageOverflow(page);
        });
        await t.test('init dialog owns focus and Escape closes it', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          const queue = page.getByRole('button', { name: /^(queue|initialize|start)$/i }).filter({ visible: true }).first();
          await queue.click();
          const dialog = page.getByRole('dialog', { name: /Primary draft$/ });
          await visible(dialog);
          await inViewport(dialog);
          await page.waitForFunction(() => document.activeElement?.closest('[aria-labelledby="init-modal-title"]'));
          await page.keyboard.press('Escape');
          await dialog.waitFor({ state: 'hidden' });
        });
        await t.test('navigation dialog traps focus and restores its trigger', async () => {
          await backToBoard(page);
          const trigger = width < 768 ? page.getByRole('button', { name: 'Open navigation', exact: true })
            : page.getByRole('button', { name: 'Open command palette', exact: true }).filter({ visible: true });
          await trigger.click();
          const dialog = page.getByRole('dialog', { name: width < 768 ? 'Navigation' : 'Command palette', exact: true });
          await inViewport(dialog);
          await page.waitForFunction(() => document.activeElement?.closest('[role="dialog"]'));
          for (const key of ['Tab', 'Shift+Tab']) for (let i = 0; i < 12; i++) {
            await page.keyboard.press(key);
            assert.ok(await dialog.evaluate(el => el.contains(document.activeElement)), 'Tab must stay in modal');
          }
          // Tab drills into actions/schedule in the desktop palette. Escape
          // pops those scopes before closing; the phone sheet closes in one.
          for (let i = 0; i < (width < 768 ? 1 : 3) && await dialog.isVisible(); i++) {
            await page.keyboard.press('Escape');
          }
          await dialog.waitFor({ state: 'hidden' });
          await page.waitForFunction(label => document.activeElement?.getAttribute('aria-label') === label,
            width < 768 ? 'Open navigation' : 'Open command palette');
          assert.ok(await trigger.evaluate(el => el === document.activeElement), 'Closing restores trigger focus');
        });
        await t.test('repeated ticket panels and offline terminals do not duplicate mounts', async () => {
          for (let repeat = 0; repeat < 3; repeat++) {
            await backToBoard(page);
            await openTicket(page, 'kon-live');
            const terminal = page.locator('#terminal-session:visible, #terminal-container-mobile:visible');
            await visible(terminal.locator('.xterm'));
            await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
            assert.equal(await terminal.locator('.xterm').count(), 1);
            await inViewport(terminal);
            await backToBoard(page);
            await page.locator('#terminal-session:visible .xterm, #terminal-container-mobile:visible .xterm').waitFor({ state: 'hidden' });
            await openTicket(page, 'kon-draft', source);
            await visible(model(page, 'editForm.body').or(page.locator('[x-ref="bodyPreview"]:visible')));
          }
          assert.ok(fixture.sockets.length >= 3, 'Each panel reopened an intercepted terminal');
          await backToBoard(page);
          await noPageOverflow(page);
        });
        await t.test('completed runs and passive epic routes', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-done');
          if (width < 768) {
            await page.getByRole('button', { name: '▤ ticket', exact: true }).click();
            await visible(page.getByText('Second code run complete.', { exact: true }).filter({ visible: true }));
          } else {
            const cards = page.locator('.stage-card-head:visible');
            await visible(cards.first());
            assert.equal(await cards.count(), 2, 'Repeated code runs have independent cards');
            const earlierBody = page.locator('.stage-card-body').last();
            await cards.last().click();
            await earlierBody.waitFor({ state: 'hidden' });
            assert.equal(await page.locator('.stage-card-body:visible').count(), 1);
            await cards.last().click();
            await visible(earlierBody);
            assert.equal(await page.locator('.stage-card-body:visible').count(), 2);
          }
          const socketsBefore = fixture.sockets.length;
          await backToBoard(page);
          await routeTo(page, '#/e/kon-epic');
          await visible(page.getByRole('heading', { name: /Passive fixture epic$/ }).first());
          assert.equal(fixture.sockets.length, socketsBefore, 'Passive epic must never attach a terminal');
          await noPageOverflow(page);
        });
      });
    });
  }
  for (const width of [390, 1024, 1280]) for (const theme of themes) {
    test(`Archive Back resumes retained outputs / ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        const phone = width < 768;
        const id = phone ? 'kon-done' : 'kon-live';
        await openTicket(page, id);
        await ready(page);
        const tab = phone ? page.locator('[x-text="mobileFirstTabLabel()"]:visible')
          : page.getByRole('button', { name: 'activity', exact: true }).filter({ visible: true });
        await tab.click();
        if (phone) await page.getByLabel('Stages and cost estimates').getByRole('button', { name: /^plan:/ }).click();
        const output = revision => page.getByText(phone
          ? `${id} plan fixture log revision ${revision}` : `Fixture activity revision ${revision}`, { exact: true }).filter({ visible: true });
        await visible(output(0));
        const endpoint = `GET /api/tickets/${id}/${phone ? 'logs' : 'activity'}`;
        for (const revision of [1, 2]) {
          await routeTo(page, '#/archive');
          await visible(page.getByRole('textbox', { name: 'Filter archive', exact: true }));
          const before = fixture.requests.filter(request => request === endpoint).length;
          fixture.behavior.outputRevision = revision;
          await page.goBack();
          await visible(output(revision));
          assert.equal(new URL(page.url()).hash, `#/t/${id}`);
          assert.ok(fixture.requests.filter(request => request === endpoint).length > before, 'Return reloads the retained output');
          if (!phone) {
            await page.waitForFunction(() => Alpine.$data(document.body)._activityPoll !== null);
            await page.waitForRequest(request => new URL(request.url()).pathname === `/api/tickets/${id}/activity`, { timeout: 6000 });
          }
        }
        await backToBoard(page);
        await openTicket(page, 'kon-live');
        const terminal = page.locator('#terminal-session:visible, #terminal-container-mobile:visible');
        await visible(terminal.locator('.xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        for (let repeat = 0; repeat < 2; repeat++) {
          await routeTo(page, '#/archive');
          await visible(page.getByRole('textbox', { name: 'Filter archive', exact: true }));
          const sockets = fixture.sockets.length;
          await page.goBack();
          await visible(terminal.locator('.xterm'));
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          assert.ok(fixture.sockets.length > sockets, 'Return reconnects the retained terminal');
          assert.equal(await page.locator('.xterm:visible').count(), 1);
          await inViewport(terminal);
        }
        await noPageOverflow(page);
      });
    });

    test(`filtered compact Settings exposes its query and clear / ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async page => {
        await page.setViewportSize({ width: 1280, height: 900 });
        await routeTo(page, '#/settings');
        await page.getByRole('navigation', { name: 'Settings sections', exact: true }).getByText('general', { exact: true }).click();
        const desktopSearch = page.getByRole('textbox', { name: 'Search settings', exact: true });
        await desktopSearch.fill('branch_prefix');
        await visible(page.locator('#gen-branch_prefix:visible'));
        await page.locator('#gen-tickets_dir').waitFor({ state: 'hidden' });
        for (const compactWidth of [...new Set([width <= 1100 ? width : 1100, 390])]) {
          await page.setViewportSize({ width: compactWidth, height: 900 });
          const search = page.getByRole('searchbox', { name: 'Search settings', exact: true });
          await inViewport(search);
          assert.equal(await search.inputValue(), 'branch_prefix');
          const clear = page.getByRole('button', { name: 'Clear settings search', exact: true });
          await inViewport(clear);
          await clear.click();
          assert.equal(await search.inputValue(), '');
          await visible(page.locator('#gen-tickets_dir:visible'));
          await search.fill('branch_prefix');
          await page.locator('#gen-tickets_dir').waitFor({ state: 'hidden' });
          await noPageOverflow(page);
        }
        await page.setViewportSize({ width: 1280, height: 900 });
        assert.equal(await desktopSearch.inputValue(), 'branch_prefix');
      });
    });
  }
  test('the harness rejects unmodelled API, asset, external HTTP and WebSocket requests', async () => {
    const context = await browser.newContext({ serviceWorkers: 'block' });
    const fixture = fixtures();
    await fixture.install(context);
    const page = await context.newPage();
    try {
      // about:blank has no document CSP, so these exercise the interceptor,
      // not just the application's self-only CSP. Nothing reaches a server.
      for (const url of [base + '/api/not-a-fixture', base + '/not-an-asset', 'https://external.invalid/not-allowed']) {
        await page.evaluate(url => fetch(url).catch(() => {}), url);
      }
      await page.evaluate(() => new Promise(resolve => {
        const ws = new WebSocket('ws://external.invalid/not-allowed');
        ws.onclose = ws.onerror = () => resolve();
      }));
      assert.equal(fixture.violations.length, 4);
      assert.ok(fixture.violations.some(message => message.includes('Unmodelled API')));
      assert.ok(fixture.violations.some(message => message.includes('Unknown asset')));
      assert.ok(fixture.violations.some(message => message.includes('External request')));
      assert.equal(fixture.sockets.length, 0);
    } finally {
      await context.close();
    }
  });
  for (const theme of themes) {
    test(`native phone forms, picker focus and short viewport / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        const opener = page.getByRole('button', { name: 'New ticket', exact: true }).filter({ visible: true });
        await opener.click();
        const dialog = page.getByRole('dialog', { name: 'New ticket', exact: true });
        const submit = dialog.getByRole('button', { name: 'create ticket', exact: true });
        assert.ok(await submit.isDisabled());
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        await dialog.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.equal(await page.locator(':focus').textContent(), 'cancel');
        await page.setViewportSize({ width: 390, height: 900 });
        assert.ok(await model(page, 'createForm.title').getAttribute('required') !== null);
        await model(page, 'createForm.title').fill('Native scheduled fixture');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        const date = page.getByLabel('Start at', { exact: true });
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await model(page, 'createForm.body').fill('Keep this draft while the viewport shrinks.');
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).isMobile), true);
        await dialog.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.ok(await submit.evaluate(el => el === document.activeElement));
        await submit.scrollIntoViewIfNeeded();
        await inViewport(submit);
        assert.equal(await model(page, 'createForm.body').inputValue(), 'Keep this draft while the viewport shrinks.');
        const picker = page.locator('#mobile-new-agent');
        await picker.scrollIntoViewIfNeeded();
        await picker.click();
        const panel = page.locator('#agent-picker-mobile-create');
        await visible(panel);
        await inViewport(panel);
        await page.waitForFunction(() => !!document.activeElement?.closest('[data-agent-picker-panel]'));
        await page.keyboard.press('Escape');
        await page.waitForFunction(() => !document.querySelector('#agent-picker-mobile-create')?.matches(':popover-open'));
        assert.ok(await dialog.isVisible(), 'Escape closes the top-layer picker first');
        assert.ok(await picker.evaluate(el => el === document.activeElement));
        fixture.behavior.createFailure = '<img src=x onerror=alert(1)> Request failed';
        await submit.scrollIntoViewIfNeeded();
        await submit.click();
        const error = dialog.locator('[x-text="createError"]');
        await visible(error);
        await error.scrollIntoViewIfNeeded();
        await inViewport(error);
        assert.equal(await error.textContent(), fixture.behavior.createFailure);
        assert.equal(await error.locator('img').count(), 0, 'Server text must not become markup');
        assert.equal(fixture.mutations.length, 0);
        fixture.behavior.createFailure = null;
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const scheduled = fixture.mutations.find(m => m.body.title === 'Native scheduled fixture');
        assert.equal(scheduled.body.scheduled_at, '2099-09-01T09:00:00Z');
        assert.equal(scheduled.body.status, 'open');
        await page.setViewportSize({ width: 390, height: 900 });
        await opener.click();
        await model(page, 'createForm.title').fill('Cleared phone schedule');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await date.fill('');
        await date.blur();
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const cleared = fixture.mutations.find(m => m.body.title === 'Cleared phone schedule');
        assert.equal(cleared.body.status, 'todo');
        assert.equal('scheduled_at' in cleared.body, false);
        await opener.click();
        await model(page, 'createForm.title').fill('Epic ignores old schedule');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await model(page, 'createForm.kind').selectOption('epic');
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const epic = fixture.mutations.find(m => m.body.title === 'Epic ignores old schedule');
        assert.equal(epic.body.status, 'open');
        assert.equal('scheduled_at' in epic.body, false);
        assert.equal('pipeline' in epic.body, false);
        await opener.click();
        await model(page, 'createForm.title').fill('Pending old form');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        let release;
        fixture.behavior.createWait = new Promise(resolve => { release = resolve; });
        const request = page.waitForRequest(r => r.url() === base + '/api/tickets' && r.method() === 'POST');
        await submit.click();
        await request;
        await dialog.getByRole('button', { name: 'Close', exact: true }).click();
        await opener.click();
        await model(page, 'createForm.title').fill('Replacement draft');
        const response = page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST');
        release();
        await response;
        await page.evaluate(() => new Promise(resolve => Alpine.nextTick(resolve)));
        assert.equal(await model(page, 'createForm.title').inputValue(), 'Replacement draft');
        assert.ok(await dialog.isVisible());
        assert.equal(await error.isVisible(), false);
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        assert.ok(await opener.evaluate(el => el === document.activeElement));
        await page.setViewportSize({ width: 390, height: 900 });
        await openTicket(page, 'kon-draft');
        await page.getByRole('button', { name: 'Actions', exact: true }).click();
        await page.getByRole('dialog', { name: 'Ticket actions' }).getByRole('button', { name: 'Schedule…', exact: true }).click();
        const schedule = page.getByRole('dialog', { name: 'Start later', exact: true });
        const field = schedule.getByRole('textbox', { name: 'Start date or duration', exact: true });
        await field.fill('2h');
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        await field.scrollIntoViewIfNeeded();
        await inViewport(field);
        const saveSchedule = schedule.getByRole('button', { name: 'schedule', exact: true });
        await saveSchedule.scrollIntoViewIfNeeded();
        await inViewport(saveSchedule);
        assert.equal(await saveSchedule.isDisabled(), false);
        await schedule.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.ok(await schedule.getByRole('button', { name: 'run now instead', exact: true }).evaluate(el => el === document.activeElement));
        await page.keyboard.press('Escape');
        await schedule.waitFor({ state: 'hidden' });
        await noPageOverflow(page);
      });
    });

    test(`dirty phone Settings resumes exact routes and handles failed Save / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        let attempt = 0;
        for (const destination of ['#/t/kon-live', '#/s/secondary/t/kon-draft', '#/e/kon-epic']) {
          const prefix = 'unsaved-prefix-' + attempt++;
          await routeTo(page, '#/settings');
          await page.locator('#settings-mobile-section').selectOption('general');
          await page.locator('#gen-branch_prefix').fill(prefix);
          await page.evaluate(route => { location.hash = route; }, destination);
          const guard = page.getByRole('dialog', { name: 'Leave settings?' });
          await visible(guard);
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).currentView), 'settings');
          await guard.getByRole('button', { name: 'keep editing', exact: true }).click();
          await guard.waitFor({ state: 'hidden' });
          assert.equal(new URL(page.url()).hash, '#/settings');
          assert.equal(await page.locator('#gen-branch_prefix').inputValue(), prefix);
          await page.evaluate(route => { location.hash = route; }, destination);
          await visible(guard);
          fixture.behavior.saveFailure = 'Fixture save refused';
          await guard.getByRole('button', { name: 'save & leave', exact: true }).click();
          await visible(guard.getByText('Fixture save refused', { exact: true }));
          assert.equal(new URL(page.url()).hash, '#/settings');
          fixture.behavior.saveFailure = null;
          await guard.getByRole('button', { name: 'save & leave', exact: true }).click();
          await guard.waitFor({ state: 'hidden' });
          await page.waitForURL('**/' + destination);
          assert.equal(new URL(page.url()).hash, destination);
          if (destination.includes('kon-live')) {
            await visible(page.locator('#terminal-container-mobile .xterm'));
            assert.equal(await page.locator('.xterm:visible').count(), 1);
          }
          await backToBoard(page);
        }
      });
    });

    test(`assistant views retain flex space in a short phone viewport / ${theme}`, async t => {
      await withPage(t, 390, theme, async page => {
        await page.getByRole('button', { name: 'Assistant', exact: true }).filter({ visible: true }).click();
        const dialog = page.getByRole('dialog', { name: 'Assistant', exact: true });
        await visible(dialog);
        await page.setViewportSize({ width: 390, height: 420 });
        for (let i = 0; i < 3; i++) {
          await dialog.getByRole('button', { name: 'Chat', exact: true }).click();
          const thread = page.locator('#assistant-sheet-thread');
          await visible(thread);
          assert.equal(await thread.evaluate(el => getComputedStyle(el).display), 'flex');
          assert.ok((await thread.boundingBox()).height > 0);
          const composer = page.getByRole('textbox', { name: 'Message the assistant', exact: true });
          await composer.fill('A recoverable unsent draft');
          await composer.scrollIntoViewIfNeeded();
          await inViewport(composer);
          const send = dialog.getByRole('button', { name: 'send', exact: true });
          await send.scrollIntoViewIfNeeded();
          await inViewport(send);
          await dialog.getByRole('button', { name: 'History', exact: true }).click();
          await visible(dialog.getByText('No chats yet.', { exact: true }));
          await dialog.getByRole('button', { name: 'Inbox', exact: true }).click();
        }
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        await noPageOverflow(page);
      });
    });
  }
  for (const width of [320, 390]) for (const theme of themes) {
    test(`supervisor terminal input lifecycle ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await routeTo(page, '#/e/' + supervisorID);
        const epic = page.locator('.mobile-epic');
        await epic.getByRole('button', { name: 'session', exact: true }).click();
        await visible(page.locator('#epic-terminal-mobile .xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el.readOnly), false);
        await page.locator('#epic-terminal-mobile .xterm-screen').click();
        await page.keyboard.type('supervisor-input');
        for (const height of [420, 360, 900]) {
          const top = height === 900 ? 0 : 12;
          await visualViewport(page, height, top);
          await page.waitForFunction(({ height, top }) => {
            const box = document.querySelector('.mobile-epic').getBoundingClientRect();
            return Math.abs(box.height - height) < 1 && Math.abs(box.top - top) < 1;
          }, { height, top });
          assert.equal(await page.evaluate(() => innerHeight), 900, 'Only the visual viewport shrinks');
          const done = epic.getByRole('button', { name: 'Done typing', exact: true });
          const box = await done.boundingBox();
          assert.ok(box && box.width >= 44 && box.height >= 44 && box.x >= 0 && box.x + box.width <= width &&
            box.y >= top && box.y + box.height <= top + height,
          `Input exit outside visible overlay: ${JSON.stringify(box)} at ${width}x${height}`);
          assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el === document.activeElement), true);
          assert.equal(await done.evaluate(el => {
            const box = el.getBoundingClientRect();
            return el.contains(document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2));
          }), true, 'Input exit must receive touch input');
          await done.click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected && !Alpine.$data(document.body).terminalRW);
          assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el === document.activeElement), false);
          await visualViewport(page, 900);
          await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          await page.locator('#epic-terminal-mobile .xterm-screen').click();
        }
        await page.keyboard.press('Escape');
        assert.equal(new URL(page.url()).hash, '#/e/' + supervisorID);
        await visible(epic.getByRole('button', { name: 'Enable input', exact: true }));
        await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await epic.getByRole('button', { name: 'activity', exact: true }).click();
        await epic.getByRole('button', { name: 'session', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el.readOnly), true);
        assert.ok(fixture.sockets.every(url => url.pathname === '/ws/terminal/' + supervisorID));
        assert.ok(fixture.messages.some(message => message.type === 'input'));
        assert.equal(await page.evaluate(() => window.__fixtureSockets.filter(ws => ws.readyState < 2).length), 1);
      });
    });
  }
  for (const theme of themes) {
    test(`live terminal survives breakpoint roundtrip / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        for (const width of [...widths, 390]) {
          await page.setViewportSize({ width, height: 900 });
          await page.waitForFunction(({ width }) => {
            const app = Alpine.$data(document.body);
            return app.isMobile === (width < 768) && (width >= 768 ||
              Math.abs(parseFloat(document.documentElement.style.getPropertyValue('--mobile-vv-width')) - width) < 1);
          }, { width });
          const terminal = page.locator('#terminal-session:visible, #terminal-container-mobile:visible');
          await visible(terminal.locator('.xterm'));
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          assert.equal(await page.locator('.xterm:visible').count(), 1);
          assert.equal(new URL(page.url()).hash, '#/t/kon-live');
          await inViewport(terminal);
          await noPageOverflow(page);
        }
        assert.ok(fixture.sockets.length >= 3);
      });
    });
  }
});

for (const width of [390, 1280]) {
  test(`Archive settles and reloads after navigation through shipped Alpine / ${width}px`, async t => {
    await withPage(t, width, 'dark', async (page, fixture) => {
      for (let visit = 1; visit <= 3; visit++) {
        await routeTo(page, '#/archive');
        await visible(page.getByText('Archived fixture', { exact: true }));
        await page.waitForFunction(() => !Alpine.$data(document.body).archiveLoading);
        assert.equal(fixture.requests.filter(r => r === 'GET /api/tickets/archived').length, visit);
        assert.equal(await page.evaluate(() => Alpine.$data(document.body)._archiveFlight), null);
        await routeTo(page, '#/');
        await page.locator('#archive-view').waitFor({ state: 'detached' });
      }
      assert.deepEqual(fixture.mutations, []);
    });
  });

  test(`large-board metadata through shipped Alpine / ${width}px`, async t => {
    await withPage(t, width, 'dark', async page => {
      const result = await page.evaluate(() => {
        const state = Alpine.reactive(kontora());
        let scans = 0;
        state.tickets = Array.from({ length: 5 }, (_, i) => ({ id: 'primary-' + i, title: 'Primary', status: 'open' }));
        state.secondaryTickets = Array.from({ length: 2161 }, (_, i) => ({
          id: 'secondary-' + i, source: 'secondary', title: 'Secondary ' + i,
          get status() { scans++; return i < 1742 ? 'archived' : 'open'; },
        }));
        state.recomputeBoard();
        const columns = state.columns;
        state.setActiveColumn(columns.findIndex(c => c.key === 'archived'));
        const initial = state.mobileTickets().length;
        state.mobileLoadMore();
        const expanded = state.mobileTickets().length;
        state.secondaryTickets[0].title = 'Renamed';
        state.recomputeBoard();
        const stable = columns === state.columns;
        scans = 0;
        for (let i = 0; i < 1000; i++) {
          state.columns;
          state.mobileColumn();
          state.mobileTickets();
        }
        const repeatedReads = scans;
        const counts = [state._boardTotal, state.boardTickets('archived').length, state.secondaryTickets.length];
        state.searchQuery = 'secondary-1741';
        state.recomputeBoard();
        return { initial, expanded, stable, repeatedReads, counts,
          filtered: state._boardTotal, visible: state.mobileTickets().length, retained: state.secondaryTickets.length };
      });
      assert.deepEqual(result, { initial: 100, expanded: 200, stable: true, repeatedReads: 0,
        counts: [2166, 1742, 2161], filtered: 1, visible: 1, retained: 2161 });
    });
  });
}
