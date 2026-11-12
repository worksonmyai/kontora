import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Startup ordering in the compiled IIFE, with fake network, timers and stream.
const bundle = fs.readFileSync(process.env.KONTORA_BUNDLE, 'utf8');
const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve)); };

function harness({ status = 200 } = {}) {
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
    '/api/config': {},
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
    getStoredTheme() { return null; }, setStoredTheme() {}, applyTheme() {},
    window: {
      innerWidth: 1200,
      addEventListener(type, listener) {
        if (!listeners.has(type)) listeners.set(type, new Set());
        listeners.get(type).add(listener);
      },
      removeEventListener(type, listener) { listeners.get(type)?.delete(listener); },
    },
    document: { documentElement: { style: {} }, getElementById() { return null; } },
    navigator: {}, location, history,
    URLSearchParams,
    async fetch(url) {
      requests.push(url);
      if (status !== 200) return { ok: false, status, json: async () => ({}) };
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
  return { state, requests, streams, timers, listeners, location, history, fireWait, count };
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
