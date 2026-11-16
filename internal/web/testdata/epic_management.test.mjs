import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { kontoraEpics } from '../ui/epics.js';
import { kontoraPalette } from '../ui/palette.js';
import { kontoraSources } from '../ui/sources.js';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraLifecycle } from '../ui/lifecycle.js';

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
async function until(predicate) {
  for (let i = 0; i < 100 && !predicate(); i++) await Promise.resolve();
  assert.ok(predicate(), 'expected asynchronous request');
}
function app() {
  const epic = { id: 'epic-a', kind: 'epic', status: 'open', path: '/repo', child_order: ['stale', 'a', 'review', 'b', 'elsewhere'] };
  const child = (id, status, created_at = '') => ({ id, title: id, status, created_at, kontora: true, parent: { id: epic.id } });
  const a = Object.assign({}, kontoraSources(), kontoraTickets(), kontoraEpics(), kontoraPalette(), kontoraLifecycle(), {
    tickets: [epic, { id: 'epic-b', kind: 'epic', status: 'open', child_order: ['other'] },
      child('a', 'open'), child('review', 'human_review'), child('b', 'open'),
      child('z', 'open', '2025'), child('c', 'open', '2025'),
      { id: 'solo', title: 'Search needle', agent: 'worker', status: 'open', kontora: true }],
    selectedEpic: epic, selectedTicket: null, _navigationSeq: 1, configCache: {}, _editorSaves: {},
    childRowFromEvent: t => t, statusLabel: s => s,
    ticketMatchesQuery: (t, q, fields) => [t.id, t.title, ...fields.map(f => t[f])].join(' ').toLowerCase().includes(q),
    recomputeBoard() { this._epicRecompute(this.tickets.filter(t => t.kind === 'epic')); },
    async fetchTasks() {},
  });
  a.recomputeBoard();
  return a;
}
async function withFetch(fn, run) {
  const old = globalThis.fetch;
  globalThis.fetch = fn;
  try { await run(); } finally { globalThis.fetch = old; }
}
const ok = () => ({ ok: true });

test('shared epic candidates search identity, stage and agent and reject invalid members', () => {
  const a = app();
  a.tickets.push({ id: 'foreign', source: 'remote', status: 'open' },
    { id: 'forbidden', status: 'open', can_control: false }, { id: 'archived', status: 'archived' });
  assert.deepEqual(a.epicCandidates(a.selectedEpic).map(t => t.id), ['solo']);
  for (const q of ['needle', 'solo', 'worker']) assert.deepEqual(a.epicCandidates(a.selectedEpic, q).map(t => t.id), ['solo']);
  assert.deepEqual(a.epicCandidates(a.selectedEpic, 'absent'), []);
  assert.deepEqual(a.epicCandidates({ ...a.selectedEpic, source: 'remote' }), []);
});

test('move within a displayed group preserves raw nonmembers, other groups and membership', async () => {
  const a = app(), calls = [], parents = a.tickets.map(t => t.parent);
  await withFetch(async (url, options) => { calls.push([url, options]); return ok(); }, async () => {
    assert.equal(await a.epicMoveChild('b', -1), true);
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], '/api/tickets/epic-a/children');
  assert.deepEqual(JSON.parse(calls[0][1].body), { children: ['stale', 'b', 'review', 'a', 'elsewhere', 'c', 'z'] });
  assert.deepEqual(a.tickets.map(t => t.parent), parents);
  assert.deepEqual(a.epicRetainedOrderRefs(), ['stale', 'elsewhere']);
});

test('desktop drop uses the same safe raw order and refuses changed group membership', () => {
  const a = app();
  assert.deepEqual(a._epicDropOrder(a.selectedEpic, 'open', ['z', 'b', 'c', 'a']),
    ['stale', 'z', 'review', 'b', 'elsewhere', 'c', 'a']);
  assert.equal(a._epicDropOrder(a.selectedEpic, 'open', ['a', 'b']), null);
  assert.equal(a._epicDropOrder(a.selectedEpic, 'open', ['a', 'b', 'c', 'review']), null);
  const list = { querySelectorAll(selector) { assert.equal(selector, '[data-child-id]'); return [{ dataset: { childId: 'b' } }, { dataset: { childId: 'a' } }]; } };
  assert.deepEqual(a._epicOrderAfterDrop(list), ['b', 'a']);
});

test('duplicate retained references are surfaced rather than silently removed', async () => {
  const a = app(); a.selectedEpic.child_order.push('stale');
  let called = false;
  await withFetch(async () => { called = true; return ok(); }, async () => {
    assert.equal(await a.epicMoveChild('b', -1), false);
  });
  assert.equal(called, false);
  assert.match(a.epicManagementErrors['epic-a'], /duplicate references/);
  assert.equal(a.selectedEpic.child_order.filter(id => id === 'stale').length, 2);
});

test('serialized order writes recompute from the acknowledged raw order', async () => {
  const a = app(), wait = deferred(), calls = [];
  await withFetch(async (url, options) => { calls.push(JSON.parse(options.body).children); if (calls.length === 1) await wait.promise; return ok(); }, async () => {
    const first = a.epicMoveChild('b', -1);
    const second = a.epicMoveChild('b', 1);
    await until(() => calls.length === 1);
    assert.equal(a.epicMutationBusy(a.selectedEpic), true);
    wait.resolve();
    assert.deepEqual(await Promise.all([first, second]), [true, true]);
    assert.deepEqual(calls[1], ['stale', 'a', 'review', 'b', 'elsewhere', 'c', 'z']);
    assert.equal(a.epicMutationBusy(a.selectedEpic), false);
  });
});

for (const refreshTiming of ['before the write', 'during the write']) {
  test(`an older refresh started ${refreshTiming} cannot undo an acknowledged order or corrupt the next queued move`, async () => {
    const a = app(), snapshot = deferred(), acknowledgement = deferred(), nextAcknowledgement = deferred(), calls = [];
    a.fetchTasks = kontoraTickets().fetchTasks;
    a.updateFavicon = () => {};
    a._epicDetail = { ...a.selectedEpic, body: 'Epic brief' };
    a.selectedEpic = { ...a._epicDetail };
    const staleTickets = structuredClone(a.tickets);
    staleTickets.find(t => t.id === 'solo').title = 'Refreshed title';
    const acknowledgedOrder = ['stale', 'b', 'review', 'a', 'elsewhere', 'c', 'z'];
    await withFetch(async (url, options) => {
      if (url === '/api/tickets') return snapshot.promise;
      assert.equal(url, '/api/tickets/epic-a/children');
      calls.push(JSON.parse(options.body).children);
      await (calls.length === 1 ? acknowledgement.promise : nextAcknowledgement.promise);
      return ok();
    }, async () => {
      let refresh;
      if (refreshTiming === 'before the write') refresh = a.fetchTasks();
      const first = a.epicMoveChild('b', -1);
      await until(() => calls.length === 1);
      if (refreshTiming === 'during the write') refresh = a.fetchTasks();
      // Let the pending refresh finish between the two queued moves, before any stream event.
      const refreshed = a._epicEnqueue('epic-a', () => refresh);
      const second = a.epicMoveChild('b', 1);
      acknowledgement.resolve();
      assert.equal(await first, true);
      snapshot.resolve({ ok: true, json: async () => ({ tickets: staleTickets }) });
      await refreshed;
      await until(() => calls.length === 2);
      for (const epic of [a._epicOwner('epic-a'), a.selectedEpic, a._epicDetail]) {
        assert.deepEqual(epic.child_order, acknowledgedOrder);
      }
      assert.equal(a._epicDetail.body, 'Epic brief');
      assert.equal(a.tickets.find(t => t.id === 'solo').title, 'Refreshed title');
      assert.deepEqual(a.epicRollup('epic-a').children.map(t => t.id), ['b', 'review', 'a', 'c', 'z']);
      assert.deepEqual(calls[1], ['stale', 'a', 'review', 'b', 'elsewhere', 'c', 'z']);
      nextAcknowledgement.resolve();
      assert.equal(await second, true);
    });
  });
}

test('delayed writes retain epic owners and do not replace the new epic order', async () => {
  const a = app(), wait = deferred(), calls = [];
  await withFetch(async (url, options) => { calls.push([url, JSON.parse(options.body)]); if (url.includes('epic-a')) await wait.promise; return ok(); }, async () => {
    const first = a.epicMoveChild('b', -1);
    const second = a.epicMoveChild('c', -1);
    await until(() => calls.length === 1);
    a.selectedEpic = a.tickets.find(t => t.id === 'epic-b');
    ++a._navigationSeq;
    wait.resolve();
    await Promise.all([first, second]);
    assert.equal(calls.every(([url]) => url === '/api/tickets/epic-a/children'), true);
    assert.deepEqual(a.selectedEpic.child_order, ['other']);
  });
});

test('newer stream state wins over an old order acknowledgement', async () => {
  const a = app(), wait = deferred(); let started = false;
  await withFetch(async () => { started = true; await wait.promise; return ok(); }, async () => {
    const pending = a.epicMoveChild('b', -1);
    await until(() => started);
    a._touchTicket('epic-a');
    a.selectedEpic.child_order = ['new-stream-order'];
    wait.resolve(); await pending;
    assert.deepEqual(a.selectedEpic.child_order, ['new-stream-order']);
  });
});

test('membership validation is rechecked when queued work starts', async () => {
  const a = app(), wait = deferred(), calls = [];
  await withFetch(async (url) => { calls.push(url); await wait.promise; return ok(); }, async () => {
    const first = a.epicMoveChild('b', -1);
    const pull = a.epicPullIn('solo');
    await until(() => calls.length === 1);
    a.tickets.find(t => t.id === 'solo').parent = { id: 'epic-a' };
    wait.resolve(); await first;
    assert.equal(await pull, false);
    assert.equal(calls.length, 1);
  });
});

test('confirmed removal unparents only the captured child and keeps its ticket', async () => {
  const a = app(), calls = [];
  a.epicConfirmRemove('a');
  assert.deepEqual(a.epicRemoveTarget, { owner: 'epic-a', id: 'a' });
  await withFetch(async (url, options) => { calls.push([url, options]); return ok(); }, async () => {
    assert.equal(calls.length, 0, 'opening confirmation sends no request');
    a.selectedEpic = a.tickets.find(t => t.id === 'epic-b');
    assert.equal(await a.epicRemoveConfirmed(), true);
  });
  assert.equal(calls[0][0], '/api/tickets/a/unparent');
  assert.equal(calls[0][1].method, 'POST');
  assert.equal(calls[0][1].body, undefined);
  assert.ok(a.tickets.find(t => t.id === 'a'));
});

test('palette membership activation uses the row owner rather than current selection', async () => {
  const a = app(), calls = [];
  a.closePalette = () => {};
  a.epicPullIn = async (...args) => calls.push(args);
  a.selectedEpic = a.tickets.find(t => t.id === 'epic-b');
  await a.paletteRun({ kind: 'epicpull', ticketId: 'solo', epicOwner: 'epic-a' });
  assert.deepEqual(calls, [['solo', 'epic-a']]);
});

test('child commands and holder approval use lifecycle semantics', async () => {
  const a = app(), calls = [];
  a.dispatchLifecycle = async (...args) => calls.push(args);
  await a.epicChildCommand(a.tickets.find(t => t.id === 'a'), 'move:open');
  await a.epicApproveHolder();
  assert.deepEqual(calls, [['a', 'move:open'], ['review', 'move:done']]);
});

test('uncertain writes refresh once, are not replayed, and fence off-owner errors', async () => {
  const a = app(), wait = deferred(); let calls = 0, refreshes = 0;
  a.fetchTasks = async () => { refreshes++; };
  await withFetch(async () => { calls++; await wait.promise; throw new Error('lost response'); }, async () => {
    const pending = a.epicMoveChild('b', -1);
    await until(() => calls === 1);
    a.selectedEpic = a.tickets.find(t => t.id === 'epic-b');
    a.error = 'new epic error';
    wait.resolve();
    assert.equal(await pending, false);
    assert.equal(calls, 1); assert.equal(refreshes, 1);
    assert.equal(a.error, 'new epic error');
    assert.match(a.epicManagementErrors['epic-a'], /uncertain/);
  });
});

test('delayed creation cannot clear another epic draft', async () => {
  const a = app(), wait = deferred(); let calls = 0;
  a.epicAddDraft = 'Original child';
  await withFetch(async () => { calls++; await wait.promise; return ok(); }, async () => {
    const pending = a.epicAddChild();
    await until(() => calls === 1);
    a.selectedEpic = a.tickets.find(t => t.id === 'epic-b'); ++a._navigationSeq;
    a.epicAddDraft = 'Another draft';
    wait.resolve(); await pending;
    assert.equal(a.epicAddDraft, 'Another draft');
  });
});

test('backend refusal retains raw references and reports the authoritative error', async () => {
  const a = app(), before = [...a.selectedEpic.child_order];
  await withFetch(async () => ({ ok: false, json: async () => ({ error: 'Unknown retained reference: stale' }) }), async () => {
    assert.equal(await a.epicMoveChild('b', -1), false);
  });
  assert.deepEqual(a.selectedEpic.child_order, before);
  assert.equal(a.epicManagementErrors['epic-a'], 'Unknown retained reference: stale');
});

test('secondary epic namesakes cannot mutate primary epic ownership', async () => {
  const a = app(); a.selectedEpic = { ...a.selectedEpic, source: 'remote' };
  let called = false;
  await withFetch(async () => { called = true; return ok(); }, async () => {
    assert.equal(await a.epicMoveChild('b', -1), false);
    assert.equal(await a.epicPullIn('solo'), false);
    assert.equal(await a.epicMoveOut('a'), false);
    a.epicAddDraft = 'Do not create'; await a.epicAddChild();
  });
  assert.equal(called, false);
});

test('prototype-named epic owners have independent queues and pending state', async () => {
  const a = app();
  a.tickets.push({ id: 'constructor', kind: 'epic', status: 'open' });
  assert.equal(a.epicMutationBusy('constructor'), false);
  let called = false;
  await withFetch(async (url, options) => {
    called = true;
    assert.equal(JSON.parse(options.body).parent, 'constructor');
    return ok();
  }, async () => { assert.equal(await a.epicPullIn('solo', 'constructor'), true); });
  assert.equal(called, true);
  assert.equal(a.epicMutationBusy('constructor'), false);
});

test('failed uncertainty refresh blocks later queued writes until a read succeeds', async () => {
  const a = app(); let writes = 0, reads = 0;
  a.fetchTasks = async () => { reads++; throw new Error('offline'); };
  await withFetch(async () => { writes++; throw new Error('lost response'); }, async () => {
    const first = a.epicMoveChild('b', -1), second = a.epicMoveChild('c', -1);
    assert.deepEqual(await Promise.all([first, second]), [false, false]);
  });
  assert.equal(writes, 1);
  assert.equal(reads, 2);
  assert.equal(a._epicNeedsRefresh['epic-a'], true);
});

test('both shells expose shared picker and child controls with explicit confirmation copy', () => {
  const html = fs.readFileSync(new URL('../static/index.html', import.meta.url), 'utf8');
  assert.equal(html.match(/x-html="epicPickerMarkup"/g).length, 2);
  assert.equal(html.match(/x-html="epicChildActionsMarkup"/g).length, 2);
  for (const helper of ['childStageLine(r)', "r.agent || '—'", 'childElapsed(r)']) assert.ok(html.includes(helper));
  const a = app();
  assert.match(a.epicChildActionsMarkup, /The ticket itself is kept/);
  assert.match(a.epicChildActionsMarkup, /epicRemoveConfirmed\(\)/);
  assert.match(a.epicChildActionsMarkup, /lifecycleDestinations\(r\)/);
});
