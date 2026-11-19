import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Startup ordering in the compiled IIFE, with fake network, timers and stream.
const bundle = fs.readFileSync(process.env.KONTORA_BUNDLE, 'utf8');
const themeScript = fs.readFileSync(new URL('../static/theme.js', import.meta.url), 'utf8');

for (const [preference, prefersDark, palette] of [
  [undefined, false, 'light'],
  ['system', false, 'light'],
  ['system', true, 'dark'],
  ['system', undefined, 'dark'],
  ['light', true, 'light'],
  ['dark', false, 'dark'],
]) {
  test(`before Alpine: ${preference} with device dark=${prefersDark} applies ${palette}`, () => {
    const root = { dataset: { themePreference: preference }, style: {} };
    const globals = { document: { documentElement: root }, localStorage: { getItem: () => palette === 'light' ? 'dark' : 'light' } };
    if (prefersDark !== undefined) globals.matchMedia = query => {
      assert.equal(query, '(prefers-color-scheme: dark)');
      return { matches: prefersDark };
    };
    vm.runInNewContext(themeScript, globals);
    assert.equal(root.dataset.theme, palette);
    assert.equal(root.style.colorScheme, palette);
  });
}
const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve)); };

function themeSaved(config) {
  return { status: 204, headers: new Headers({
    'X-Kontora-Theme': config.theme, 'X-Kontora-Config-Revision': String(config.revision),
  }) };
}

function harness({ status = 200, preference = 'system', dark = true, saveTheme, readConfig } = {}) {
  const root = { dataset: { themePreference: preference }, style: {} };
  const mediaListeners = new Set();
  const media = {
    matches: dark,
    addEventListener(type, fn) { if (type === 'change') mediaListeners.add(fn); },
    removeEventListener(type, fn) { if (type === 'change') mediaListeners.delete(fn); },
    change(matches) {
      this.matches = matches;
      for (const fn of mediaListeners) fn({ matches });
    },
  };
  const writes = [];
  const requests = [];
  const streams = [];
  const timers = [];
  const listeners = new Map();
  const location = { protocol: 'http:', host: 'localhost', search: '', hash: '#/' };
  const history = {
    state: { unrelated: 'preserved' },
    replaceState(state, _title, hash) { this.state = state; location.hash = hash; },
  };
  const bodies = {
    '/api/config': { theme: preference, revision: 1 },
    '/api/tickets': { tickets: [{ id: 'kon-1', title: 'Primary', status: 'open' }] },
    '/api/ticket-sources': [{ id: 'primary' }, { id: 'team', available: true }],
    '/api/sources/team/tickets': [{ id: 'sec-1', title: 'Secondary', status: 'open' }],
  };
  const ctx = {
    console, structuredClone, TextEncoder,
    setTimeout(fn, ms) { timers.push({ fn, ms }); return timers.length; },
    clearTimeout(id) { if (timers[id - 1]) timers[id - 1].cleared = true; },
    setInterval() {}, clearInterval() {},
    requestAnimationFrame() {}, cancelAnimationFrame() {},
    localStorage: { getItem() { return null; }, setItem() {} },
    matchMedia() { return media; },
    window: {
      innerWidth: 1200,
      addEventListener(type, listener) {
        if (!listeners.has(type)) listeners.set(type, new Set());
        listeners.get(type).add(listener);
      },
      removeEventListener(type, listener) { listeners.get(type)?.delete(listener); },
    },
    document: { documentElement: root, getElementById() { return null; } },
    navigator: {}, location, history,
    URLSearchParams,
    async fetch(url, options) {
      requests.push(url);
      if (status !== 200) return { ok: false, status, json: async () => ({}) };
      if (url === '/api/config/theme') {
        writes.push(options);
        const mode = JSON.parse(options.body).theme;
        if (saveTheme) return saveTheme(mode, bodies['/api/config']);
        bodies['/api/config'].theme = mode;
        bodies['/api/config'].revision++;
        return themeSaved(bodies['/api/config']);
      }
      if (url === '/api/config' && readConfig) return readConfig(bodies[url]);
      return { ok: true, status: 200, json: async () => structuredClone(bodies[url]) };
    },
    EventSource: class {
      constructor(url) { this.url = url; this.closed = false; this.listeners = {}; this.onerror = null; streams.push(this); }
      addEventListener(type, handler) { (this.listeners[type] ||= []).push(handler); }
      close() { this.closed = true; }
      emit(type) { for (const h of this.listeners[type] || []) h({ data: '{}' }); }
      fail() { this.onerror?.({}); }
    },
  };
  vm.createContext(ctx);
  vm.runInContext(themeScript, ctx);
  vm.runInContext(bundle, ctx);
  const state = ctx.kontora();
  Object.assign(state, {
    $watch() {}, $nextTick(cb) { cb?.(); return Promise.resolve(); },
    initMobile() {}, _bindGlobalEvents() {}, setupAgentPicker() {}, initAssistant() {},
    companionInit() {}, companionRefresh() {}, async applyRoute() {}, _mountBoard() {},
    recomputeBoard() {}, scheduleBoardFlush() {}, updateFavicon() {},
  });
  const waitTimer = () => timers.find(t => t.ms === 1500 && !t.fired && !t.cleared);
  const fireWait = () => { const t = waitTimer(); t.fired = true; t.fn(); };
  const count = url => requests.filter(r => r === url).length;
  return { state, requests, streams, timers, listeners, location, history, fireWait, count,
    root, media, mediaListeners, writes, config: bodies['/api/config'] };
}

test('startup snapshots wait for the stream and the first open does not take them again', async () => {
  const h = harness();
  const init = h.state.init();
  await settle();
  assert.deepEqual(h.requests, ['/api/config'], 'snapshots started before the stream subscribed');
  assert.equal(h.streams.length, 1);
  h.streams[0].emit('open');
  await init;
  await settle();
  assert.equal(h.count('/api/tickets'), 1);
  assert.equal(h.count('/api/ticket-sources'), 1);
  assert.equal(h.count('/api/sources/team/tickets'), 1);
  assert.equal(h.state.loading, false);
  assert.deepEqual([...h.state.tickets].map(t => t.id), ['kon-1']);
  assert.deepEqual([...h.state.secondaryTickets].map(t => t.id), ['sec-1']);

  // A later open is a reconnect, which may have missed events.
  h.streams[0].emit('open');
  await settle();
  assert.equal(h.count('/api/tickets'), 2);
  assert.equal(h.count('/api/sources/team/tickets'), 2);
});

for (const [name, release] of [
  ['a stream that does not open in time', h => h.fireWait()],
  ['a stream that fails', h => h.streams[0].fail()],
]) {
  test(`${name} does not hold the board, and its next open resynchronizes`, async () => {
    const h = harness();
    const init = h.state.init();
    await settle();
    release(h);
    await init;
    await settle();
    assert.equal(h.state.loading, false);
    assert.equal(h.count('/api/tickets'), 1);
    h.streams.at(-1).emit('open');
    await settle();
    assert.equal(h.count('/api/tickets'), 2, 'snapshots taken before the subscription were not refreshed');
  });
}

test('root startup owns settings history and unload listeners and root destruction releases them', async () => {
  const h = harness();
  const init = h.state.init();
  await settle();
  h.streams[0].emit('open');
  await init;

  assert.equal(h.mediaListeners.size, 1);
  h.media.change(false);
  assert.equal(h.root.style.colorScheme, 'light');
  assert.equal(h.state.lightTheme, true);
  assert.equal(h.writes.length, 0);
  assert.equal(h.history.state.unrelated, 'preserved');
  assert.equal(h.location.hash, '#/');
  for (const type of ['beforeunload', 'popstate', 'hashchange']) {
    assert.equal(h.listeners.get(type)?.size, 1, `${type} must have one owner`);
  }
  h.state.setupSettingsLifecycle();
  h.state.setupSettingsHistory();
  for (const type of ['beforeunload', 'popstate', 'hashchange']) {
    assert.equal(h.listeners.get(type)?.size, 1, `${type} setup must be idempotent`);
  }

  const unload = () => {
    const event = { prevented: false, preventDefault() { this.prevented = true; } };
    for (const listener of h.listeners.get('beforeunload')) listener(event);
    return event;
  };
  assert.equal(unload().prevented, false, 'a clean board must not warn');
  h.state.settingsNewStage = 'unfinished-stage';
  assert.equal(unload().prevented, true, 'an unfinished add-row draft must warn');
  h.state.settingsNewStage = '';
  h.state.settingsSaving = true;
  assert.equal(unload().returnValue, '', 'a pending save must request a native warning');
  h.state.settingsSaving = false;
  h.state.settingsDirty = () => true;
  assert.equal(unload().prevented, true, 'dirty settings must warn');

  // Only unrelated subsystem teardowns are doubled; run the real root and
  // settings lifecycle/history disposal, including cancellation of stale work.
  for (const name of ['destroyMobile', 'destroyAgentPicker', 'closeStats', 'automationsDispose',
    'closeTerminal', '_destroyTerminal', '_resetActivity', 'companionDestroy',
    '_stopAssistantPoll', '_stopAssistantTick', '_assistantCloseStream']) h.state[name] = () => {};
  let aborted = false;
  h.state._settingsReadController = { abort() { aborted = true; } };
  const readSeq = h.state._settingsRefreshSeq;
  const revealSeq = h.state._settingsRevealSeq;
  h.state.destroy();
  assert.equal(aborted, true);
  assert.ok(h.state._settingsRefreshSeq > readSeq);
  assert.ok(h.state._settingsRevealSeq > revealSeq);
  assert.equal(h.state._settingsHistory, null);
  for (const type of ['beforeunload', 'popstate', 'hashchange']) {
    assert.equal(h.listeners.get(type)?.size, 0, `${type} leaked after destroy`);
  }
  assert.equal(h.streams[0].closed, true);
  assert.equal(h.mediaListeners.size, 0);
  h.media.change(true);
  assert.equal(h.root.style.colorScheme, 'light');
});

test('quick theme cycles the saved mode even when the device already shows that palette', async () => {
  const h = harness({ dark: false });
  assert.equal(h.state.lightTheme, true);
  const saving = h.state.toggleTheme();
  await h.state.toggleTheme();
  await saving;
  assert.equal(h.writes.length, 1, 'only one theme save may be in flight');
  assert.equal(h.state.themePreference, 'light');
  h.media.change(true);
  assert.equal(h.root.dataset.theme, 'light');
  await h.state.toggleTheme();
  assert.equal(h.root.dataset.theme, 'dark');
  await h.state.toggleTheme();
  assert.equal(h.state.themePreference, 'system');
  assert.equal(h.root.dataset.theme, 'dark');
  assert.deepEqual(h.writes.map(request => [request.method, JSON.parse(request.body)]), [
    ['PUT', { theme: 'light' }], ['PUT', { theme: 'dark' }], ['PUT', { theme: 'system' }],
  ]);
  assert.equal(h.count('/api/config/raw'), 0);
});

for (const [name, saveTheme, applied, error] of [
  ['refused', () => ({ status: 409, json: async () => ({ error: 'Config changed' }) }), 'system', /Config changed/],
  ['saved but not applied', () => ({ status: 500, json: async () => ({ error: 'Theme saved, but reload failed', code: 'config_saved_reload_failed' }) }), 'system', /reload failed/],
  ['unclassified server error', () => ({ status: 500, json: async () => ({ error: 'Directory sync failed' }) }), 'system', /Check config.yaml before retrying/],
  ['lost response', (mode, config) => { config.theme = mode; throw new Error('network error'); }, 'light', /Check config.yaml before retrying/],
]) {
  test(`a ${name} theme save reports the error and reads the running mode without retrying`, async () => {
    const h = harness({ saveTheme });
    const select = { value: 'light' };
    await h.state.changeThemePreference(select);
    await settle();
    assert.equal(select.value, 'system', 'selection is controlled by the saved preference');
    assert.equal(h.state.themePreference, applied);
    assert.equal(h.root.style.colorScheme, applied === 'light' ? 'light' : 'dark');
    assert.match(h.state.error, error);
    assert.equal(h.count('/api/config/theme'), 1);
    assert.equal(h.count('/api/config'), 1);
    assert.equal(h.state.themeSaving, false);
  });
}

test('a stalled resolved-config read does not hold the theme controls', async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const h = harness({ readConfig: () => pending });
  try {
    const save = h.state.toggleTheme();
    await settle();
    assert.equal(h.state.themeSaving, false);
    assert.equal(h.state.themePreference, 'light');
    await h.state.toggleTheme();
    assert.equal(h.writes.length, 2);
    assert.equal(h.state.themePreference, 'dark');
    await save;
  } finally {
    release({ ok: false, status: 503 });
  }
});

for (const [name, duringSave, expected, nextMode] of [
  ['a pre-commit config read cannot hide a successful theme save', { theme: 'system', revision: 1 }, 'light', 'dark'],
  ['a late theme save response does not replace a newer running theme', { theme: 'dark', revision: 3 }, 'dark', 'system'],
  ['a newer reload can restore the original preference', { theme: 'system', revision: 3 }, 'system', 'light'],
]) {
  test(name, async () => {
    let acknowledge;
    const response = new Promise(resolve => { acknowledge = resolve; });
    let configReadFails = false;
    const h = harness({
      saveTheme: () => response,
      readConfig: config => configReadFails ? { ok: false, status: 503 }
        : { ok: true, json: async () => structuredClone(config) },
    });
    await h.state.refreshConfigCache();
    const save = h.state.toggleTheme();
    Object.assign(h.config, duringSave);
    await h.state.refreshConfigCache();
    assert.equal(h.state.themePreference, duringSave.theme);
    configReadFails = true;
    acknowledge(themeSaved({ theme: 'light', revision: 2 }));
    await save;
    await settle();
    assert.equal(h.state.themePreference, expected);
    assert.equal(h.root.style.colorScheme, expected === 'system' ? 'dark' : expected);
    await h.state.toggleTheme();
    assert.deepEqual(h.writes.map(request => JSON.parse(request.body).theme), ['light', nextMode]);
  });
}

test('a pending Settings save asks the user to wait before changing the theme', async () => {
  const h = harness();
  h.state.settingsSaving = true;
  await h.state.toggleTheme();
  assert.equal(h.state.error, 'Wait for the Settings save to finish.');
  assert.equal(h.writes.length, 0);
});

test('a dirty Settings draft blocks a theme save but does not block an external theme update', async () => {
  const h = harness();
  const init = h.state.init();
  h.streams[0].emit('open');
  await init;
  h.state.settingsNewStage = 'unfinished-stage';
  await h.state.toggleTheme();
  assert.match(h.state.error, /Save or discard your Settings changes/);
  assert.equal(h.writes.length, 0);
  assert.equal(h.state.themePreference, 'system');
  h.config.theme = 'light';
  h.streams[0].emit('config_reloaded');
  await settle();
  assert.equal(h.state.themePreference, 'light');
  assert.equal(h.root.style.colorScheme, 'light');
  assert.equal(h.state.settingsNewStage, 'unfinished-stage');
  assert.equal(h.count('/api/config/raw'), 0);
});

test('an unauthorized daemon stops the stream instead of retrying it', async () => {
  const h = harness({ status: 401 });
  const init = h.state.init();
  await settle();
  h.streams[0].fail();
  await init;
  assert.equal(h.state.needsAuth, true);
  assert.equal(h.streams[0].closed, true);
  assert.ok(h.timers.every(t => t.ms !== 3000 || t.cleared), 'a reconnect is still scheduled');
  h.state.connectSSE();
  assert.equal(h.streams.length, 1, 'connectSSE opened a stream that can only answer 401');
});
