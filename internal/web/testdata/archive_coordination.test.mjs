import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import vm from 'node:vm';

// These tests run the daemon's compiled IIFE without DOM rendering or real network requests.
const bundle = fs.readFileSync(process.env.KONTORA_BUNDLE, 'utf8');
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const tick = () => new Promise(resolve => setTimeout(resolve, 0));
const row = (id, title = id) => ({ id, title, status: 'done' });
const response = tickets => ({ ok: true, status: 200, json: async () => ({ tickets }) });
const ids = state => [...state.archiveRows].map(r => r.id);

function harness(reactive = false) {
  const requests = [];
  const ctx = {
    console, setTimeout, clearTimeout, structuredClone, TextEncoder,
    setInterval() {}, clearInterval() {},
    localStorage: { getItem() { return null; }, setItem() {} },
    getStoredTheme() { return null; }, setStoredTheme() {}, applyTheme() {},
    window: { innerWidth: 1200, addEventListener() {} },
    document: { documentElement: { style: {} }, getElementById() { return null; } },
    navigator: {}, location: { protocol: 'http:', host: 'localhost' },
    fetch(url, options) {
      const request = { url, options, ...deferred() };
      requests.push(request);
      return request.promise;
    },
  };
  vm.createContext(ctx);
  if (reactive) {
    ctx.window.Element = class {};
    ctx.MutationObserver = class { observe() {} };
    // The coordinator needs Alpine's proxies, not its document startup.
    ctx.queueMicrotask = () => {};
    ctx.document.createElement = () => ({ style: {} });
    ctx.document.addEventListener = () => {};
    const vendor = path.join(path.dirname(fileURLToPath(import.meta.url)), '../static/vendor/alpinejs@3.14.8/cdn.min.js');
    vm.runInContext(fs.readFileSync(vendor, 'utf8'), ctx);
  }
  vm.runInContext(bundle, ctx);
  const raw = ctx.kontora();
  const state = reactive ? ctx.window.Alpine.reactive(raw) : raw;
  state.currentView = 'archive';
  const applied = [];
  state.applyTicketUpdate = ticket => applied.push(ticket);
  state.recomputeBoard = () => {};
  state.showToast = () => {};
  return { state, requests, applied };
}

function reopen(state) {
  state.closeArchive();
  state.currentView = 'board';
  state.currentView = 'archive';
  state.openArchive();
}

test('archive requests are single-flight with one trailing refresh per burst', async () => {
  const { state, requests } = harness();
  const first = state.archiveLoad();
  for (let i = 0; i < 20; i++) state.archiveLoad();
  assert.equal(requests.length, 1);
  requests[0].resolve(response([row('old')]));
  await first;
  assert.equal(requests.length, 2);
  requests[1].resolve(response([row('fresh')]));
  await tick();
  assert.deepEqual(ids(state), ['fresh']);
  assert.equal(requests.length, 2);
  assert.equal(state.archiveLoading, false);
});

test('pending json decode, catch and finalizer are fenced across close/reopen', async () => {
  for (const fail of [false, true]) {
    const { state, requests } = harness();
    const decoding = deferred();
    const first = state.archiveLoad();
    requests[0].resolve({ ok: true, json: () => decoding.promise });
    await tick();
    state.archiveLoad(); // obsolete trailing refresh must also be discarded
    reopen(state);
    assert.equal(requests.length, 1, 'reopened epoch waits for the previous list decode');
    assert.equal(state.archiveLoading, true);
    if (fail) decoding.reject(new Error('obsolete decode'));
    else decoding.resolve({ tickets: [row('obsolete')] });
    await first;
    assert.deepEqual(ids(state), []);
    assert.equal(state.archiveError, null);
    assert.equal(state.archiveLoading, true);
    assert.equal(requests.length, 2);
    requests[1].resolve(response([row('current')]));
    await tick();
    assert.deepEqual(ids(state), ['current']);
    assert.equal(state.archiveLoading, false);
  }
});

test('obsolete HTTP errors and authentication cannot mutate reopened view', async () => {
  for (const status of [401, 500]) {
    const { state, requests } = harness();
    const first = state.archiveLoad();
    reopen(state);
    requests[0].resolve({ ok: false, status });
    await first;
    assert.equal(state.needsAuth, false);
    assert.equal(state.archiveError, null);
    assert.equal(state.archiveLoading, true);
    requests[1].resolve(response([]));
    await tick();
  }
});

test('current errors settle and permit another request', async () => {
  const { state, requests } = harness();
  const first = state.archiveLoad();
  requests[0].reject(new Error('offline'));
  await first;
  assert.equal(state.archiveError, 'offline');
  assert.equal(state.archiveLoading, false);
  const next = state.archiveLoad();
  requests[1].resolve(response([]));
  await next;
  assert.equal(state.archiveError, null);
});

test('SSE patches and drops, including absent IDs, beat an in-flight snapshot', async () => {
  const { state, requests } = harness();
  state.archiveRows = [row('patch'), row('delete')];
  const first = state.archiveLoad();
  const decoding = deferred();
  requests[0].resolve({ ok: true, json: () => decoding.promise });
  await tick();
  state.archivePatchRow({ id: 'patch', title: 'updated', archived_from: 'cancelled' });
  const patched = state.archiveRows[0];
  state.archiveDropRow('delete');
  state.archiveDropRow('absent');
  decoding.resolve({ tickets: [row('patch', 'stale'), row('delete'), row('absent'), row('other')] });
  await first;
  assert.deepEqual(ids(state).sort(), ['other', 'patch']);
  assert.equal(state.archiveRows.find(r => r.id === 'patch'), patched);
  assert.equal(patched.title, 'updated');
});

test('unknown archive patches coalesce a trailing list refresh', async () => {
  const { state, requests } = harness();
  const first = state.archiveLoad();
  for (let i = 0; i < 20; i++) state.archivePatchRow({ id: 'new' });
  assert.equal(requests.length, 1);
  requests[0].resolve(response([]));
  await first;
  assert.equal(requests.length, 2);
  requests[1].resolve(response([row('new')]));
  await tick();
  assert.deepEqual(ids(state), ['new']);
});

test('optimistic restore IDs survive refreshes; a refusal rolls back once', async () => {
  const { state, requests } = harness();
  const original = row('restore');
  state.archiveRows = [original, row('other')];
  const restoring = state.archiveRestore('restore');
  await state.archiveRestore('restore');
  assert.equal(requests.length, 1);
  const loading = state.archiveLoad();
  requests[1].resolve(response([row('restore', 'stale'), row('other')]));
  await loading;
  assert.deepEqual(ids(state), ['other']);
  requests[0].resolve({ ok: false, status: 409, json: async () => ({ error: 'refused' }) });
  await restoring;
  assert.deepEqual(ids(state), ['restore', 'other']);
  assert.equal(state.archiveRows[0], original);
  assert.equal(state.error, 'refused');
  assert.equal(state._archiveRestores.size, 0);
});

test('successful restore cannot be resurrected by a list already in flight', async () => {
  const { state, requests, applied } = harness();
  state.archiveRows = [row('restore')];
  const restoring = state.archiveRestore('restore');
  const loading = state.archiveLoad();
  requests[0].resolve({ ok: true, json: async () => ({ id: 'restore', status: 'done' }) });
  await restoring;
  requests[1].resolve(response([row('restore')]));
  await loading;
  assert.deepEqual(ids(state), []);
  assert.equal(applied.length, 1);
});

test('delete while restoring forbids rollback of the captured row', async () => {
  const { state, requests } = harness();
  state.archiveRows = [row('restore')];
  const restoring = state.archiveRestore('restore');
  state.archiveDropRow('restore');
  requests[0].reject(new Error('late refusal'));
  await restoring;
  assert.deepEqual(ids(state), []);
  assert.equal(requests.length, 2);
  requests[1].resolve(response([]));
  await tick();
  assert.deepEqual(ids(state), []);
});

test('old restore callbacks refresh the reopened archive, never insert stale rows', async () => {
  for (const fail of [false, true]) {
    const { state, requests, applied } = harness();
    state.archiveRows = [row('restore', 'captured stale row')];
    const restoring = state.archiveRestore('restore');
    const decoding = deferred();
    requests[0].resolve({ ok: !fail, status: fail ? 409 : 200, json: () => decoding.promise });
    await tick();
    reopen(state);
    // Even a newly opened list must suppress the optimistic ID until its POST settles.
    requests[1].resolve(response([row('restore', 'snapshot while restoring'), row('other')]));
    await tick();
    assert.deepEqual(ids(state), ['other']);
    decoding.resolve(fail ? { error: 'old refusal' } : { id: 'restore', status: 'done' });
    await restoring;
    assert.deepEqual(ids(state), ['other']);
    assert.equal(applied.length, 0);
    assert.equal(requests.length, 3);
    requests[2].resolve(response(fail ? [row('restore', 'current server row')] : []));
    await tick();
    assert.deepEqual(ids(state), fail ? ['restore'] : []);
    if (fail) assert.equal(state.archiveRows[0].title, 'current server row');
  }
});

test('pending IDs remain fenced if an old restore settles during the reopened list', async () => {
  for (const fail of [false, true]) {
    const { state, requests, applied } = harness();
    state.archiveRows = [row('restore', 'captured')];
    const restoring = state.archiveRestore('restore');
    reopen(state);
    const decoding = deferred();
    requests[1].resolve({ ok: true, json: () => decoding.promise });
    await tick();
    if (fail) requests[0].reject(new Error('old refusal'));
    else requests[0].resolve({ ok: true, json: async () => ({ id: 'restore', status: 'done' }) });
    await restoring;
    assert.equal(requests.length, 2, 'refresh coalesces behind the reopened list');
    decoding.resolve({ tickets: [row('restore', 'stale snapshot')] });
    await tick();
    assert.deepEqual(ids(state), [], 'settled optimistic ID remains fenced until trailing refresh');
    assert.equal(applied.length, 0);
    assert.equal(requests.length, 3);
    requests[2].resolve(response(fail ? [row('restore', 'current')] : []));
    await tick();
    assert.deepEqual(ids(state), fail ? ['restore'] : []);
  }
});

test('closing without reopening leaves no retained list or stale errors', async () => {
  const { state, requests } = harness();
  const loading = state.archiveLoad();
  state.closeArchive();
  state.currentView = 'board';
  requests[0].reject(new Error('late failure'));
  await loading;
  assert.deepEqual(ids(state), []);
  assert.equal(state.archiveError, null);
  assert.equal(state.archiveLoading, false);
  assert.equal(requests.length, 1);
});

for (const outcome of ['success', 'HTTP failure', 'decode failure']) {
  test(`shipped Alpine archive finalizes ${outcome}, drains bursts and reopens`, async () => {
    const { state, requests } = harness(true);
    state.openArchive();
    for (let i = 0; i < 20; i++) state.archivePatchRow({ id: 'unknown-' + i });
    assert.equal(requests.length, 1);
    if (outcome === 'HTTP failure') requests[0].resolve({ ok: false, status: 500 });
    else if (outcome === 'decode failure') requests[0].resolve({ ok: true, json: async () => { throw new Error('decode'); } });
    else requests[0].resolve(response([row('first')]));
    await tick();
    assert.equal(requests.length, 2, 'reactive finalizer starts one trailing request');
    requests[1].resolve(response([row('fresh')]));
    await tick();
    assert.deepEqual(ids(state), ['fresh']);
    assert.equal(state.archiveLoading, false);
    assert.equal(state.archiveError, null);
    assert.equal(state._archiveFlight, null);
    reopen(state);
    assert.equal(requests.length, 3);
    requests[2].resolve(response([row('reopened')]));
    await tick();
    assert.deepEqual(ids(state), ['reopened']);
    assert.equal(state.archiveLoading, false);
  });
}

test('shipped Alpine archive serializes a reopened epoch behind a pending decode', async () => {
  const { state, requests } = harness(true);
  const decode = deferred();
  state.openArchive();
  requests[0].resolve({ ok: true, json: () => decode.promise });
  await tick();
  reopen(state);
  assert.equal(requests.length, 1);
  decode.resolve({ tickets: [row('obsolete')] });
  await tick();
  assert.deepEqual(ids(state), []);
  assert.equal(state.archiveLoading, true);
  assert.equal(requests.length, 2);
  state.archiveDropRow('deleted');
  requests[1].resolve(response([row('deleted'), row('current')]));
  await tick();
  assert.deepEqual(ids(state), ['current']);
  assert.equal(state.archiveLoading, false);
});
