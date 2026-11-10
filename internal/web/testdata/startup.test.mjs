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
    window: { innerWidth: 1200, addEventListener() {}, removeEventListener() {} },
    document: { documentElement: { style: {} }, getElementById() { return null; } },
    navigator: {}, location: { protocol: 'http:', host: 'localhost', search: '' },
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
  return { state, requests, streams, timers, fireWait, count };
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
