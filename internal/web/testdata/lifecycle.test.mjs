import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraLifecycle } from '../ui/lifecycle.js';
import { kontoraSources } from '../ui/sources.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraEpics } from '../ui/epics.js';
import { kontoraNotify } from '../ui/notify.js';

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
async function until(predicate) {
  for (let n = 0; n < 100; n++) { if (predicate()) return; await Promise.resolve(); }
  assert.ok(predicate(), 'expected asynchronous work to start');
}
function app(extra = {}) {
  return Object.assign({}, kontoraSources(), kontoraTickets(), kontoraDetail(), kontoraEpics(), kontoraLifecycle(), {
    tickets: [{ id: 'a', status: 'paused', kontora: true, stage: 'code', attempt: 2 }, { id: 'b', status: 'paused', kontora: true }],
    selectedTicket: null, selectedEpic: null, configCache: { custom_statuses: ['waiting_external'] },
    _editorSaves: {}, _editorAckVersions: Object.create(null), plannotatorInFlight: {}, editing: false,
    recomputeBoard() {}, applyTicketUpdate(t) { this._touchTicket(t); const i = this.tickets.findIndex(x => x.id === t.id); if (i >= 0) this.tickets[i] = t; },
    needsInitModal: () => false,
    boardEntry: t => t, ...extra,
  });
}

test('destinations are configured, not inferred from observed board columns', () => {
  const a = app();
  a.tickets.push({ id: 'unknown', status: 'alien', execution_active: true });
  const destinations = a.lifecycleDestinations('a');
  assert.deepEqual(destinations.map(d => d.status), ['open', 'todo', 'human_review', 'done', 'cancelled', 'waiting_external']);
  assert.equal(destinations.find(d => d.status === 'open').id, 'move:open');
  assert.equal(a.lifecycleDestinations({ source: 'secondary', id: 'a' }).length, 0);
});

test('Paused destinations require running execution or workflow wait authority', async () => {
  const cases = [
    ...['open', 'todo', 'human_review', 'paused', 'done', 'cancelled', 'waiting_external'].map(status => [{ status }, false]),
    [{ status: 'in_progress' }, true],
    [{ status: 'waiting_external', execution_active: true }, true],
    [{ status: 'paused', graph_execution: { wait: { id: 'wait-1' } } }, true],
  ];
  for (const [fields, canPause] of cases) {
    const a = app(); let moves = 0;
    a.tickets[0] = { ...a.tickets[0], ...fields };
    a._moveTaskTransport = async () => { moves++; return { outcome: 'success' }; };
    const canMove = canPause && fields.status !== 'paused';
    const label = JSON.stringify(fields);
    assert.equal(a.lifecycleDestinations('a').some(d => d.status === 'paused'), canMove, label);
    assert.equal(!!a._lifecycleAllowed(a.tickets[0], a.lifecycleCommand('a', 'pause')), canPause, label);
    assert.equal((await a.dispatchLifecycle('a', 'move:paused')).outcome, canMove ? 'success' : 'refused', label);
    assert.equal(moves, canMove ? 1 : 0, label);
  }
});

test('paused to Open is one move and preserves execution fields', async () => {
  const a = app(); const calls = [];
  const oldFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => { calls.push([url, JSON.parse(options.body)]); return { ok: true, json: async () => ({ ...a.tickets[0], status: 'open', stage: 'code', attempt: 2 }) }; };
  try {
    assert.equal((await a.moveTask('a', 'open')).outcome, 'success');
    assert.deepEqual(calls, [['/api/tickets/a/move', { status: 'open' }]]);
    assert.equal(a.tickets[0].attempt, 2);
  } finally { globalThis.fetch = oldFetch; }
});

test('same-ticket conflict is refused while another ticket remains usable', async () => {
  const a = app(); const wait = deferred();
  a.lifecycleDraftBarrier = key => key === 'a' ? wait.promise : Promise.resolve(true);
  a._lifecyclePost = async () => ({ outcome: 'success' });
  const first = a.dispatchLifecycle('a', 'retry');
  assert.equal(a.lifecycleBusy('a'), true);
  assert.equal((await a.dispatchLifecycle('a', 'pause')).outcome, 'busy');
  assert.equal((await a.dispatchLifecycle('b', 'retry')).outcome, 'success');
  wait.resolve(true); assert.equal((await first).outcome, 'success');
  assert.equal(a.lifecycleBusy('a'), false);
});

test('failed saves block execution and changed state invalidates captured commands', async () => {
  for (const kind of ['failure', 'status', 'ownership', 'claim', 'stage']) {
    const a = app(); let posted = false;
    a._lifecyclePost = async () => { posted = true; return { outcome: 'success' }; };
    a.lifecycleDraftBarrier = async () => {
      if (kind === 'status') a.tickets[0].status = 'done';
      if (kind === 'ownership') a.tickets[0].can_control = false;
      if (kind === 'claim') a.tickets[0].claimed_by = 'another-daemon';
      if (kind === 'stage') a.tickets[0].stage = 'review';
      return kind !== 'failure';
    };
    const result = await a.dispatchLifecycle('a', a.lifecycleCommand('a', 'retry'));
    assert.equal(result.outcome, kind === 'failure' ? 'save-failed' : 'obsolete', kind);
    assert.equal(posted, false, kind);
    if (kind === 'failure') {
      assert.equal(a.error, 'Latest ticket changes have not been saved successfully.');
      assert.equal(result.error, a.error);
    }
  }
});

test('missing or secondary subjects never fall back to selected primary namesake', async () => {
  const a = app(); a.selectedTicket = a.tickets[0];
  a.secondaryTickets = [{ id: 'a', source: 'other', status: 'paused', can_control: true }];
  for (const key of ['missing', 'other/a']) assert.equal((await a.dispatchLifecycle(key, 'retry')).outcome, 'refused');
});

test('draft barrier drains latest ordinary draft, epic debounce and notification queue', async () => {
  for (const surface of ['ordinary', 'epic', 'notifications']) {
    const a = app(); const calls = [];
    const state = { ack: { description: 'old' }, draft: { description: 'new' }, pending: 0, tail: Promise.resolve(true) };
    a._editorSaves.a = state;
    if (surface === 'ordinary') {
      a.selectedTicket = a.tickets[0]; a.editing = true; a._editorTicketKey = 'a';
      a._ordinaryEditorDraft = () => ({ description: 'newest' });
      a.flushEditSave = async () => { calls.push('ordinary'); state.ack = state.draft = { description: 'newest' }; return true; };
    } else {
      state.ack = state.draft;
    }
    if (surface === 'epic') {
      a.selectedEpic = { id: 'a', kind: 'epic' };
      a.epicFlushBrief = async () => { calls.push('epic'); return true; };
    }
    a.flushNotifySave = async key => { calls.push('notify:' + key); return surface !== 'notifications'; };
    assert.equal(await a.lifecycleDraftBarrier('a'), surface !== 'notifications');
    assert.ok(calls.includes('notify:a'));
    if (surface !== 'notifications') assert.ok(calls.includes(surface));
  }
});

test('new keystrokes during an ordinary save are acknowledged before execution', async () => {
  const a = app(); const calls = []; const first = deferred();
  a.selectedTicket = { ...a.tickets[0], description: 'old' };
  a.editing = true; a._editorGeneration = 1; a._editorTicketKey = 'a'; a._bodyLead = '';
  a.editForm = { body: 'old', path: '', agent: '', branch: '', base_branch: '', pipeline: '' };
  a._loadEditorDraft(a.selectedTicket, a._ordinaryEditorDraft());
  a.editForm.body = 'first';
  const oldFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push([options.method, url, body]);
    if (calls.length === 1) await first.promise;
    return { ok: true, json: async () => ({ ...a.tickets[0], description: body?.description || 'latest', status: options.method === 'POST' ? 'todo' : 'paused' }) };
  };
  try {
    const save = a.saveEdit();
    await until(() => calls.length === 1);
    const execution = a.dispatchLifecycle('a', 'retry');
    a.editForm.body = 'latest';
    first.resolve(); await save;
    assert.equal((await execution).outcome, 'success');
    assert.deepEqual(calls.map(([method, url]) => [method, url]), [['PUT', '/api/tickets/a'], ['PUT', '/api/tickets/a'], ['POST', '/api/tickets/a/retry']]);
    assert.equal(calls[1][2].description, 'latest');
    assert.equal(a._editorSaves.a.ack.description, 'latest');
  } finally { globalThis.fetch = oldFetch; }
});

test('epic execution drains the actual brief debounce and retains failed drafts', async () => {
  for (const ok of [true, false]) {
    const a = app(); const calls = [];
    a.tickets[0] = { ...a.tickets[0], kind: 'epic', epic_execution: 'agent' };
    a.selectedEpic = a._epicDetail = { ...a.tickets[0], description: 'old' };
    a.editForm = { body: 'new brief' }; a._editorGeneration = 1;
    a._loadEditorDraft(a.selectedEpic, { description: 'old' });
    a._epicBriefDebounce = setTimeout(() => assert.fail('debounce was not drained'), 1000);
    const oldFetch = globalThis.fetch;
    globalThis.fetch = async (url, options) => {
      calls.push([url, options.method]);
      return { ok, json: async () => ok ? { ...a.tickets[0], description: 'new brief', status: options.method === 'POST' ? 'todo' : 'paused' } : { error: 'save denied' } };
    };
    try {
      const result = await a.dispatchLifecycle('a', 'retry');
      assert.equal(result.outcome, ok ? 'success' : 'save-failed');
      assert.equal(a._epicBriefDebounce, null);
      assert.deepEqual(calls.map(c => c[1]), ok ? ['PUT', 'POST'] : ['PUT']);
      assert.equal(a.editForm.body, 'new brief');
    } finally { clearTimeout(a._epicBriefDebounce); globalThis.fetch = oldFetch; }
  }
});

for (const boundary of ['initially read-only', 'permission revoked', 'detail permission revoked', 'supervisor started']) {
  test(`epic brief retains forbidden drafts without trapping navigation: ${boundary}`, async t => {
    const a = app();
    a.tickets[0] = { ...a.tickets[0], kind: 'epic', epic_execution: 'agent', can_edit: boundary !== 'initially read-only' };
    a.selectedEpic = a._epicDetail = { ...a.tickets[0], description: 'old' };
    a.editForm = { body: 'old' }; a._editorGeneration = 1;
    a._loadEditorDraft(a.selectedEpic, { description: 'old' });
    a._blockAnchor = () => null; a._anchorEditor = () => {}; a.$nextTick = fn => fn();
    a.epicBeginBriefEdit({ target: null });
    assert.equal(a.epicEditingBrief, boundary !== 'initially read-only');
    a.editForm.body = 'retained brief';
    a._epicBriefDebounce = setTimeout(() => assert.fail('forbidden debounce ran'), 1000);
    t.after(() => clearTimeout(a._epicBriefDebounce));
    let writes = 0;
    t.mock.method(globalThis, 'fetch', async (_url, options) => {
      if (!options) return { ok: true, json: async () => ({ ...a.tickets[0], description: 'old' }) };
      writes++;
      return { ok: false, json: async () => ({ error: 'forbidden' }) };
    });
    if (boundary === 'supervisor started') a.tickets[0] = { ...a.tickets[0], status: 'in_progress', execution_active: true };
    else a.tickets[0] = { ...a.tickets[0], can_edit: false };
    if (boundary === 'detail permission revoked') {
      a.boardEntryChanged = () => false; a.reconcileOutput = () => {};
      await a.epicFetchDetail('a');
    } else a.epicRefreshSelected();
    assert.equal(a._epicBriefDebounce, null);
    assert.equal(a.epicEditingBrief, false);
    assert.equal(await a.lifecycleDraftBarrier('a'), false);
    a.closeTerminal = () => {}; a._resetActivity = () => {};
    assert.equal(await a.closeEpic(false), true);
    assert.equal(a.selectedEpic, null);
    assert.equal(a._editorSaves.a.draft.description, 'retained brief');
    assert.equal(writes, 0);
    a._loadEditorDraft({ id: 'a' }, { description: 'old' });
    assert.equal(a._editorSaves.a.draft.description, 'retained brief');
    a._destroyEditorRetirements();
  });
}

test('epic permission loss during a brief write retains later typing without another PUT', async t => {
  const a = app(), ack = deferred(), calls = [];
  a.tickets[0] = { ...a.tickets[0], kind: 'epic', epic_execution: 'agent', can_edit: true };
  a.selectedEpic = a._epicDetail = { ...a.tickets[0], description: 'old' };
  a.editForm = { body: 'submitted' }; a._editorGeneration = 1;
  a._loadEditorDraft(a.selectedEpic, { description: 'old' });
  t.mock.method(globalThis, 'fetch', async (_url, options) => {
    calls.push(options?.method || 'GET');
    if (options) await ack.promise;
    return { ok: true, json: async () => ({ ...a.tickets[0], description: 'submitted' }) };
  });
  const save = a.epicFlushBrief();
  await until(() => calls.length === 1);
  a.editForm.body = 'newer retained text';
  a.tickets[0] = { ...a.tickets[0], can_edit: false };
  a._epicUpdateSeq++;
  a.epicRefreshSelected();
  ack.resolve();
  assert.equal(await save, false);
  assert.deepEqual(calls, ['PUT', 'GET']);
  assert.equal(a._editorSaves.a.draft.description, 'newer retained text');
  assert.equal(a.selectedEpic.can_edit, false);
  a._destroyEditorRetirements();
});

test('notification barrier waits a targeted pending save and blocks a failed acknowledgement', async () => {
  for (const [ok, message] of [[true, null], [false, 'Notification save denied'], [false, null]]) {
    const a = Object.assign(app(), kontoraNotify()); const pending = deferred();
    a.notifyError = message;
    a._notifySaves.a = { promise: pending.promise, ok: true };
    let posts = 0; a._lifecyclePost = async () => { posts++; return { outcome: 'success' }; };
    const execution = a.dispatchLifecycle('a', 'retry');
    await Promise.resolve(); assert.equal(posts, 0);
    a._notifySaves.a.ok = ok; a._notifySaves.a.promise = null; pending.resolve();
    const result = await execution;
    assert.equal(result.outcome, ok ? 'success' : 'save-failed');
    assert.equal(posts, ok ? 1 : 0);
    if (!ok) {
      assert.equal(a.error, message || 'Latest ticket changes have not been saved successfully.');
      assert.equal(result.error, a.error);
    }
  }
});

test('optimistic movement cannot overwrite newer SSE or a different selection', async () => {
  const a = app(); const pending = deferred(); let posted = false;
  const oldFetch = globalThis.fetch;
  globalThis.fetch = async () => { posted = true; await pending.promise; return { ok: true, json: async () => ({ id: 'a', status: 'open' }) }; };
  try {
    const move = a.moveTask('a', 'open');
    await until(() => posted);
    assert.equal(a.tickets[0].status, 'open');
    a.selectedTicket = a.tickets[1];
    a.applyTicketUpdate({ ...a.tickets[0], status: 'done' });
    pending.resolve();
    assert.equal((await move).outcome, 'success');
    assert.equal(a.tickets[0].status, 'done');
    assert.equal(a.selectedTicket.id, 'b');
  } finally { globalThis.fetch = oldFetch; }
});

test('network-ambiguous commands refresh but never replay, and failed moves roll back', async () => {
  for (const kind of ['network', 'rejection', 'invalid-ack']) {
    const a = app(); let posts = 0; let refreshes = 0;
    a.resyncTickets = async () => { refreshes++; };
    const oldFetch = globalThis.fetch;
    globalThis.fetch = async () => {
      posts++;
      if (kind === 'network') throw new Error('connection lost');
      return { ok: kind !== 'rejection', json: async () => kind === 'invalid-ack' ? { id: 'wrong' } : { error: 'denied' } };
    };
    try {
      const result = await a.moveTask('a', 'open');
      assert.equal(result.outcome, kind === 'rejection' ? 'failed' : 'uncertain');
      assert.equal(a.tickets[0].status, 'paused');
      assert.equal(posts, 1);
      assert.equal(refreshes, kind === 'rejection' ? 0 : 1);
      assert.equal(a.lifecycleBusy('a'), false);
    } finally { globalThis.fetch = oldFetch; }
  }
});

test('initialization opens without optimistic movement or an execution POST', async () => {
  for (const navigated of [false, true]) {
    const a = app(); let renders = 0;
    a._navigationSeq = 0;
    a.tickets[0].status = 'open'; a.needsInitModal = () => true;
    a.lifecycleDraftBarrier = async () => { if (navigated) a._navigationSeq++; return true; };
    a.openInitModal = async () => { a.initModal = true; };
    a.recomputeBoard = () => { renders++; };
    a._moveTaskTransport = () => assert.fail('must not move before initialization');
    assert.equal((await a.moveTask('a', 'todo')).outcome, navigated ? 'obsolete' : 'initialized');
    assert.equal(!!a.initModal, !navigated);
    assert.equal(a.tickets[0].status, 'open');
    assert.ok(renders > 0, 'a drop must be repaired');
  }
});

test('unsupported observed statuses and endpoint overrides cannot authorize commands', async () => {
  const a = app();
  assert.equal((await a.dispatchLifecycle('a', 'move:in_progress')).outcome, 'refused');
  assert.equal((await a.dispatchLifecycle('a', 'move:alien')).outcome, 'refused');
  const command = a.lifecycleCommand('a', { id: 'retry', kind: 'move', endpoint: 'run', status: 'done' });
  assert.equal(command.kind, 'retry'); assert.equal(command.endpoint, 'retry');
  assert.equal(command.status, undefined);
});

test('schedule changes share conflict ownership with execution', async () => {
  const a = app(); a.tickets[0].status = 'open';
  const pending = deferred(); let posts = 0;
  a._lifecyclePost = async () => { posts++; await pending.promise; return { outcome: 'success' }; };
  const scheduling = a.dispatchLifecycle('a', { id: 'schedule', body: { scheduled_at: '2099-01-01T00:00:00Z' } });
  await until(() => posts === 1);
  assert.equal((await a.dispatchLifecycle('a', 'start')).outcome, 'busy');
  pending.resolve(); assert.equal((await scheduling).outcome, 'success');
});

test('semantic command transport table preserves distinct endpoints', async () => {
  const cases = [
    ['start', 'open', 'run', 'POST', null],
    ['start', 'todo', 'run', 'POST', null],
    ['retry', 'paused', 'retry', 'POST', null],
    ['pause', 'in_progress', 'pause', 'POST', null],
    ['move:open', 'paused', 'move', 'POST', { status: 'open' }],
    ['set-stage:review', 'paused', 'set-stage', 'POST', { stage: 'review' }],
    ['skip', 'paused', 'skip', 'POST', null],
    [{ id: 'archive', confirmed: true, note: 'closed' }, 'done', 'archive', 'POST', { note: 'closed' }],
    [{ id: 'delete', confirmed: true }, 'paused', '', 'DELETE', null],
    ['annotate', 'open', 'plannotator-annotate', 'POST', null],
    ['review', 'human_review', 'plannotator-review', 'POST', null],
    [{ id: 'schedule', body: { clear: true } }, 'open', 'schedule', 'POST', { clear: true }],
  ];
  for (const [command, status, endpoint, method, body] of cases) {
    const a = app(); const calls = [];
    a.tickets[0] = { ...a.tickets[0], status, can_annotate: true, stages: ['code', 'review'] };
    a.updateFavicon = () => {};
    const oldFetch = globalThis.fetch;
    globalThis.fetch = async (url, options) => {
      calls.push([url, options.method, options.body ? JSON.parse(options.body) : null]);
      return { ok: true, json: async () => ({ ...a.tickets[0], status: 'done' }) };
    };
    try {
      if (command === 'start') assert.ok(a.lifecycleActions('a').some(c => c.id === 'start'), status);
      assert.equal((await a.dispatchLifecycle('a', command)).outcome, 'success', String(command.id || command));
      assert.deepEqual(calls, [['/api/tickets/a' + (endpoint ? '/' + endpoint : ''), method, body]]);
    } finally { globalThis.fetch = oldFetch; }
  }
});

test('delete from a card opens its guarded target before confirmation', async () => {
  for (const allowed of [true, false]) {
    const a = app(); a.selectedTicket = a.tickets[1];
    a._paletteOpenTicket = async ticket => { if (allowed) a.selectedTicket = ticket; return allowed; };
    const result = await a.dispatchLifecycle('a', 'delete');
    assert.equal(result.outcome, allowed ? 'confirmation' : 'refused');
    assert.equal(!!a.deleteModal, allowed);
    assert.equal(a.selectedTicket.id, allowed ? 'a' : 'b');
  }
});

test('review availability and dispatch require Human Review and a controllable execution kind', async () => {
  for (const status of ['open', 'todo', 'paused', 'in_progress', 'human_review', 'done', 'cancelled', 'archived']) {
    for (const kind of ['task', 'agent-epic', 'orchestrator-epic', 'secondary', 'read-only']) {
      const a = app(); let posts = 0;
      const ticket = a.tickets[0] = { ...a.tickets[0], status,
        ...(kind.endsWith('-epic') ? { kind: 'epic', epic_execution: kind === 'agent-epic' ? 'agent' : 'orchestrator' } : {}),
        ...(kind === 'secondary' ? { source: 'other' } : {}),
        ...(kind === 'read-only' ? { can_control: false } : {}) };
      if (kind === 'secondary') { a.tickets = []; a.secondaryTickets = [ticket]; }
      a._startPlannotatorTransport = async () => { posts++; return { outcome: 'success' }; };
      const allowed = status === 'human_review' && ['task', 'agent-epic'].includes(kind);
      const command = a.lifecycleCommand(ticket, 'review');
      assert.equal(a._lifecycleAllowed(ticket, command), allowed, status + '/' + kind);
      assert.equal((await a.dispatchLifecycle(ticket, command)).outcome, allowed ? 'success' : 'refused', status + '/' + kind);
      assert.equal(posts, allowed ? 1 : 0);
    }
  }
});

test('prototype-named ticket IDs own only their own pending and Plannotator markers', async () => {
  for (const id of ['constructor', 'toString', '__proto__']) {
    const a = app(); const pending = deferred();
    a.tickets[0] = { ...a.tickets[0], id, status: 'human_review' };
    a.lifecycleDraftBarrier = () => pending.promise;
    a._lifecyclePost = async () => ({ outcome: 'success' });
    assert.equal(a.lifecycleBusy(id), false, id);
    const execution = a.dispatchLifecycle(id, 'retry');
    assert.equal(a.lifecycleBusy(id), true, id);
    assert.equal(Object.hasOwn(a.lifecyclePending, id), true, id);
    assert.equal((await a.dispatchLifecycle(id, 'review')).outcome, 'busy', id);
    pending.resolve(true);
    assert.equal((await execution).outcome, 'success', id);
    assert.equal(a.lifecycleBusy(id), false, id);
    a.lifecycleDraftBarrier = kontoraLifecycle().lifecycleDraftBarrier;
    assert.equal((await a.dispatchLifecycle(id, 'retry')).outcome, 'success', id);
    assert.equal(a.canRestartGraph({ ...a.tickets[0], graph_definition: {} }), true, id);
    const oldFetch = globalThis.fetch;
    globalThis.fetch = async () => ({ ok: true });
    try {
      assert.equal((await a.startPlannotatorReview(a.tickets[0])).outcome, 'success', id);
      assert.equal(Object.hasOwn(a.plannotatorInFlight, id), true, id);
      assert.equal(a.canRestartGraph({ ...a.tickets[0], graph_definition: {} }), false, id);
      assert.equal((await a.dispatchLifecycle(id, 'retry')).outcome, 'refused', id);
    } finally { globalThis.fetch = oldFetch; }
  }
});

test('Plannotator events use own markers for prototype-named ticket IDs', () => {
  for (const id of ['constructor', 'toString', '__proto__']) {
    const a = app(); const handlers = new Map();
    const oldEventSource = globalThis.EventSource;
    globalThis.EventSource = class { addEventListener(name, fn) { handlers.set(name, fn); } close() {} };
    const emit = (name, extra = {}) => handlers.get(name)({ data: JSON.stringify({ ticket_id: id, ...extra }) });
    try {
      a.connectSSE();
      emit('plannotator_finished', { outcome: 'error' });
      assert.equal(a.error, 'Plannotator failed', id);
      emit('plannotator_started');
      assert.equal(Object.hasOwn(a.plannotatorInFlight, id), true, id);
      assert.equal(a.plannotatorInFlight[id], 'Plannotator', id);
      a.plannotatorInFlight[id] = 'Review';
      emit('plannotator_started');
      emit('plannotator_finished', { outcome: 'error', message: 'denied' });
      assert.equal(a.error, 'Review failed: denied', id);
      assert.equal(Object.hasOwn(a.plannotatorInFlight, id), false, id);
    } finally { globalThis.EventSource = oldEventSource; }
  }
});

test('uncertain Plannotator starts reconcile through ordinary reads without replay or lost SSE ownership', async () => {
  for (const mode of ['not-delivered', 'pending-refresh', 'refresh-fails', 'confirmed', 'confirmed-during-refresh', 'late-confirmation', 'finished', 'replaced']) {
    const a = app(); let posts = 0; let reads = 0; let pendingOwned; let replacement;
    const oldFetch = globalThis.fetch; const oldEventSource = globalThis.EventSource;
    const handlers = new Map();
    globalThis.EventSource = class { addEventListener(name, fn) { handlers.set(name, fn); } close() {} };
    const emit = name => handlers.get(name)({ data: JSON.stringify({ ticket_id: 'a' }) });
    a.fetchTicketSources = async () => {};
    a.updateFavicon = () => {};
    a.tickets[0] = { ...a.tickets[0], status: 'human_review' };
    globalThis.fetch = async (url, options) => {
      if (options?.method === 'POST') {
        posts++;
        if (mode === 'replaced' && posts === 2) return { ok: true };
        if (mode === 'pending-refresh') {
          await a.fetchTasks();
          pendingOwned = Object.hasOwn(a.plannotatorInFlight, 'a');
        }
        if (mode === 'confirmed') emit('plannotator_started');
        if (mode === 'finished') { emit('plannotator_started'); emit('plannotator_finished'); }
        throw new Error('connection lost');
      }
      assert.equal(url, '/api/tickets'); reads++;
      if (mode === 'refresh-fails' && reads === 1) throw new Error('read failed');
      if (mode === 'confirmed-during-refresh') emit('plannotator_started');
      if (mode === 'replaced' && reads === 1) {
        emit('plannotator_finished');
        replacement = await a._startPlannotatorTransport(a.tickets[0], 'plannotator-review', 'New review');
      }
      return { ok: true, json: async () => ({ tickets: a.tickets.map(t => ({ ...t })) }) };
    };
    try {
      a.connectSSE();
      const result = await a.startPlannotatorReview(a.tickets[0]);
      assert.equal(result.outcome, 'uncertain', mode);
      assert.match(a.error, /result uncertain/, mode);
      if (mode === 'pending-refresh') assert.equal(pendingOwned, true, 'a pending POST retains ownership');
      if (mode === 'replaced') assert.equal(replacement?.outcome, 'success');
      if (mode === 'refresh-fails') {
        assert.equal(Object.hasOwn(a.plannotatorInFlight, 'a'), true);
        await a.resyncTickets();
      }
      if (mode === 'late-confirmation') {
        assert.equal(Object.hasOwn(a.plannotatorInFlight, 'a'), false);
        emit('plannotator_started');
        await a.resyncTickets();
      }
      const active = ['confirmed', 'confirmed-during-refresh', 'late-confirmation', 'replaced'].includes(mode);
      assert.equal(Object.hasOwn(a.plannotatorInFlight, 'a'), active, mode);
      assert.equal(posts, mode === 'replaced' ? 2 : 1, 'refresh must not replay the POST');
      assert.equal(a.lifecycleBusy('a'), false, mode);
      a._moveTaskTransport = async () => ({ outcome: 'success' });
      assert.equal((await a.dispatchLifecycle('a', 'cancel')).outcome, active ? 'refused' : 'success', mode);
    } finally { globalThis.fetch = oldFetch; globalThis.EventSource = oldEventSource; }
  }
});

test('captured destinations cannot be reinterpreted after lifecycle changes', async () => {
  const a = app(); const command = a.lifecycleCommand('a', 'move:open');
  a.tickets[0] = { ...a.tickets[0], status: 'human_review' };
  assert.equal((await a.dispatchLifecycle('a', command)).outcome, 'obsolete');
});
