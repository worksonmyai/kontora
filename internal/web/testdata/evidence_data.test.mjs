import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraActivity } from '../ui/activity.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraNotes } from '../ui/notes.js';
import { kontoraNotify } from '../ui/notify.js';
import { kontoraSources } from '../ui/sources.js';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraMobile } from '../ui/mobile.js';

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const tick = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const response = data => ({ ok: true, headers: { get: () => null }, json: async () => structuredClone(data) });
function fixture(ticket = {}) {
  const state = Object.assign({}, kontoraSources(), kontoraActivity(), kontoraDetail(), kontoraNotes(), kontoraNotify(), { mobileSwitchTab: kontoraMobile().mobileSwitchTab }, {
    selectedTicket: { id: 'one', status: 'open', notify: [], notify_channels: [], notes: [], description: 'Description\n', description_tail: '', ...ticket },
    tickets: [], secondaryTickets: [], _selectedTicketUpdateSeq: 0, _ticketDetailSeq: 1, _editorSaves: {}, _editorAckVersions: Object.create(null),
    _activityLoadSeq: 0, _activityPoll: null, activityStage: null, activityRun: 0, activity: null,
    configCache: { author: 'me', default_channels: ['ops'] }, error: null, activeTab: 'ticket',
    outputSubject() { return this.isSecondary(this.selectedTicket) ? null : this.selectedTicket; },
    outputTab() { return this.isMobile ? this.detailTab : this.activeTab; },
    executionActive(t) { return !!t?.execution_active; },
    historyLabel(h) { return 'visit ' + h.node_visit_id; },
    descriptionText(t) { return t.description || ''; }, descriptionTail(t) { return t.description_tail || ''; },
    _touchTicket() {}, boardEntry(t) { return { ...t }; }, recomputeBoard() {}, projectForPath() { return null; },
    _invalidateStageLogs() {}, _invalidateRawLog() {}, rememberDetailView() {}, restoreDetailScroll() {},
    closeTerminal() { this.terminalOpen = false; },
  });
  state.tickets = [structuredClone(state.selectedTicket)];
  return state;
}

for (const [name, history, expected] of [
  ['legacy repeats', [{ stage: 'code', run: 0 }, { stage: 'review' }, { stage: 'code', run: 0 }], ['code#0', 'review#0', 'code#1']],
  ['annotation and import', [{ stage: 'code', imported: true }, { stage: 'code', annotation: 'review' }, { stage: 'code', run: 0 }], ['code#0', 'code#1', 'code#2']],
  ['graph visits', [{ stage: 'code', node_visit_id: 'v1', run: 0 }, { stage: 'code', node_visit_id: 'v2', run: 0 }], ['code#0', 'code#1']],
]) test(`artifact occurrences: ${name}`, async t => {
  const state = fixture({ history, stages: ['code'] });
  state.currentTicketCost = () => null;
  state.ticketStageCost = () => null;
  const rows = state.historyArtifactRuns();
  assert.deepEqual(rows.map(r => r.key), expected);
  assert.deepEqual(state.outputRuns().map(r => r.key), expected.toReversed());
  assert.deepEqual(rows.map(r => r.historyIndex), history.map((_, i) => i));
  assert.equal(state.historyArtifactRun(history.length - 1), rows.at(-1).run);
  assert.deepEqual(state.latestCompletedRun(), { stage: 'code', run: rows.at(-1).run });
  assert.equal(state.stageRibbon().find(r => r.name === 'code').run, rows.at(-1).run);
  const calls = [];
  t.mock.method(globalThis, 'fetch', async url => { calls.push(url); return response({ content: 'tape' }); });
  for (const row of rows) {
    state.selectOutputRun(row.key);
    await tick();
    assert.equal(state.activityHistoryRecord(), history[row.historyIndex]);
  }
  assert.equal(calls.length, history.filter(h => !h.imported).length);
  assert.ok(calls.includes('/api/tickets/one/activity?stage=code&run=' + rows.at(-1).run + '&log_tail_bytes=262144'));
  state.selectedTicket.execution_active = true;
  state.selectedTicket.stage = 'code';
  assert.equal(state.runningRun().run, history.filter(h => h.stage === 'code').length);
  assert.deepEqual(state.selectedTicket.history, history, 'never rewrite persisted run numbers');
});

for (const mobile of [false, true]) test(`explicit run opens Output and fetches once: mobile=${mobile}`, async t => {
  const state = fixture({ history: [{ stage: 'code', imported: true, summary: 'Imported result' }, { stage: 'code', summary: 'Local result' }] });
  state.isMobile = mobile;
  state.terminalOpen = mobile;
  let reads = 0;
  t.mock.method(globalThis, 'fetch', async () => { reads++; return response({ content: 'local' }); });
  state.openActivity('code', 1);
  await tick();
  assert.equal(state.outputTab(), 'activity');
  if (mobile) assert.equal(state.terminalOpen, false, 'mobile input/terminal cleanup follows the normal tab handler');
  assert.equal(reads, 1);
  state.openActivity('code', 0);
  await tick();
  assert.equal(state.outputRunKey(), 'code#0');
  assert.equal(state.activityHistoryRecord().summary, 'Imported result');
  assert.equal(state.activity, null);
  assert.equal(state.activityLoading, false);
  assert.equal(reads, 1, 'no local artifacts for imported entries');
});

for (const kind of ['Add', 'Reply', 'Edit']) for (const boundary of ['typing', 'reset', 'source']) {
  test(`note ${kind} draft ownership after ${boundary}`, async t => {
    const state = fixture({ notes: [{ id: 'n', author: 'me', text: 'old' }] });
    const gate = deferred();
    t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return response(state.selectedTicket); });
    const field = kind === 'Add' ? 'noteDraft' : 'note' + kind + 'Draft';
    const submitting = kind === 'Add' ? 'noteSubmitting' : 'note' + kind + 'Submitting';
    if (kind === 'Reply') state.noteOpenReply(state.selectedTicket.notes[0]);
    if (kind === 'Edit') state.noteOpenEdit(state.selectedTicket.notes[0]);
    state[field] = 'submitted';
    const request = kind === 'Add' ? state.submitNote() : kind === 'Reply' ? state.submitReply() : state.saveNoteEdit();
    await tick();
    if (boundary === 'reset') {
      state.noteResetDrafts();
      if (kind === 'Reply') state.noteOpenReply(state.selectedTicket.notes[0]);
      if (kind === 'Edit') state.noteOpenEdit(state.selectedTicket.notes[0]);
    }
    if (boundary === 'source') { state.selectedTicket = { ...state.selectedTicket, source: 'team', can_edit: true }; state.noteResetDrafts(); }
    state[field] = boundary === 'typing' ? 'newer text' : 'submitted';
    gate.resolve();
    await request;
    assert.equal(state[field], boundary === 'typing' ? 'newer text' : 'submitted');
    assert.equal(!!state[submitting], false);
  });
}

test('stale note ack never adopts old notes, revision or permissions; refresh is read-only', async t => {
  const state = fixture({ revision: 'r1', can_edit: true, notes: [{ id: 'old' }] });
  const stale = structuredClone(state.selectedTicket), ack = deferred(), fresh = deferred();
  const calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push({ url, method: opts?.method || 'GET' });
    if (opts) { await ack.promise; return response(stale); }
    await fresh.promise;
    return response(state.selectedTicket);
  });
  state.noteDraft = 'sent';
  const request = state.submitNote();
  await tick();
  state.selectedTicket = { ...state.selectedTicket, revision: 'r2', can_edit: false, notes: [{ id: 'old' }, { id: 'new' }], description: 'New description\n', description_tail: 'New tail' };
  state._selectedTicketUpdateSeq++;
  ack.resolve();
  await tick();
  assert.equal(state.selectedTicket.can_edit, false);
  assert.equal(state.selectedTicket.revision, 'r2');
  assert.deepEqual(state.selectedTicket.notes.map(n => n.id), ['old', 'new']);
  fresh.resolve();
  await request;
  assert.deepEqual(calls.map(c => c.method), ['POST', 'GET']);
  assert.equal(state.selectedTicket.body, undefined, 'non-editing fresh snapshot is retained as returned');
  assert.equal(state.selectedTicket.description, 'New description\n');
});

for (const permission of ['not author', 'revoked']) test(`note permissions: ${permission}`, async t => {
  const state = fixture({ can_edit: permission !== 'revoked', notes: [{ id: 'n', author: permission === 'not author' ? 'other' : 'me', text: 'note' }] });
  let writes = 0;
  t.mock.method(globalThis, 'fetch', async () => { writes++; return response(state.selectedTicket); });
  const note = state.selectedTicket.notes[0];
  state.noteOpenEdit(note);
  state.noteAskDelete(note);
  assert.equal(state.noteEditOn, null);
  assert.equal(state.noteDeleteTarget, null);
  state.noteEditOn = 'n'; state.noteEditDraft = 'force'; state.noteDeleteTarget = note;
  await state.saveNoteEdit(); await state.confirmDeleteNote();
  if (permission === 'revoked') {
    state.noteDraft = 'force'; await state.submitNote(); await state.toggleReaction(note, '👍');
    state.noteOpenReply(note); assert.equal(state.noteReplyOn, null);
  }
  assert.equal(writes, 0);
});

test('note writes serialize per source-qualified ticket', async t => {
  const state = fixture({ source: 'team', can_edit: true, notes: [{ id: 'n', author: 'me' }] });
  const gate = deferred(), tail = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push([url, opts.method]);
    await (calls.length === 1 ? gate.promise : tail.promise);
    return response(state.selectedTicket);
  });
  state.noteDraft = 'hello';
  const adding = state.submitNote();
  const reacting = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  const key = state.ticketKey(state.selectedTicket);
  await tick(); assert.equal(calls.length, 1);
  gate.resolve(); await adding;
  assert.equal(state._noteHasTail(key), true);
  let drained = false;
  const waiting = state._noteWaitForTail(key).then(() => { drained = true; });
  await tick(); assert.equal(drained, false);
  tail.resolve(); await Promise.all([reacting, waiting]);
  assert.equal(state._noteHasTail(key), false);
  assert.equal(drained, true);
  assert.deepEqual(calls, [['/api/sources/team/tickets/one/note', 'POST'], ['/api/sources/team/tickets/one/notes/n/reactions', 'POST']]);
});

// Accepted commands outlive the editor that submitted them. No mutation retries.
for (const outcome of ['saved', 'refused', 'uncertain']) test(`queued note keeps captured source and text after navigation: ${outcome}`, async t => {
  const state = fixture({ source: 'team', can_edit: true, notes: [{ id: 'n', text: 'original' }] });
  const original = structuredClone(state.selectedTicket), gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push({ url, method: opts?.method, body: opts?.body });
    if (calls.length === 1) { await gate.promise; return response(original); }
    if (outcome === 'uncertain') throw new Error('lost acknowledgement');
    if (outcome === 'refused') return { ok: false, json: async () => ({ error: 'permission refused' }) };
    return response({ ...original, notes: [...original.notes, { id: 'added', text: 'captured' }] });
  });
  const reacting = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  state.noteDraft = ' captured ';
  const adding = state.submitNote();
  state.noteDraft = 'newer draft';
  state.noteResetDrafts();
  state.selectedTicket = { id: 'one', source: 'other', can_edit: true, notes: [] };
  state._ticketDetailSeq++;
  state.noteDraft = 'namesake draft';
  gate.resolve(); await Promise.all([reacting, adding]);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].url, '/api/sources/team/tickets/one/note');
  assert.deepEqual(JSON.parse(calls[1].body), { text: 'captured' });
  assert.equal(state.noteDraft, 'namesake draft');
  assert.equal(state.error, null, 'no old error over the new ticket');
  state.selectedTicket = original;
  const retained = state.noteRecoveryItems();
  assert.ok(retained.some(r => r.text === 'newer draft'));
  assert.equal(retained.some(r => r.text === ' captured '), outcome !== 'saved');
  if (outcome !== 'saved') assert.match(retained.find(r => r.text === ' captured ').reason, outcome === 'uncertain' ? /uncertain/i : /permission refused/);
  await state._noteWaitForTail(state.ticketKey(original));
  assert.equal(calls.length, 2, 'waiting/reopening never retries a mutation');
});

for (const kind of ['Reply', 'Edit']) for (const target of ['same', 'other']) for (const boundary of ['navigation', 'unresolved']) {
  test(`pending note ${kind} retains identical unsent ${target} editor after ${boundary}`, async t => {
    const state = fixture({ notes: [{ id: 'a', author: 'me', text: 'A' }, { id: 'b', author: 'me', text: 'B' }] });
    const original = structuredClone(state.selectedTicket), gate = deferred();
    let writes = 0;
    t.mock.method(globalThis, 'fetch', async (_url, options) => { if (options?.method) writes++; await gate.promise; return response(original); });
    state['noteOpen' + kind](state.selectedTicket.notes[0]);
    state['note' + kind + 'Draft'] = 'identical';
    const pending = kind === 'Reply' ? state.submitReply() : state.saveNoteEdit();
    state['noteOpen' + kind](state.selectedTicket.notes[target === 'same' ? 0 : 1]);
    state['note' + kind + 'Draft'] = 'identical';
    if (boundary === 'navigation') state.noteResetDrafts();
    else state._noteApplyTicket('one', { ...original, notes: [] });
    const recovery = state.noteRecoveryItems().find(r => r.text === 'identical');
    assert.ok(recovery, 'a different editor is not the accepted command');
    assert.equal(recovery.kind, kind);
    assert.equal(recovery.unresolved, boundary === 'unresolved');
    if (boundary === 'navigation') {
      assert.equal(recovery.target, target === 'same' ? 'a' : 'b');
      state.noteRestoreRecovery(recovery.id);
      assert.equal(state['note' + kind + 'Draft'], 'identical');
      assert.equal(state['note' + kind + 'On'], target === 'same' ? 'a' : 'b');
    }
    gate.resolve(); await pending;
    assert.equal(writes, 1, 'retaining or restoring does not replay a write');
    if (boundary === 'navigation') assert.equal(state['note' + kind + 'Draft'], 'identical');
  });
}

const legacy = (id, text = 'legacy') => ({ id, text, at: 'handwritten byline', author: 'me', author_kind: 'human' });
for (const kind of ['Reply', 'Edit', 'Delete', 'Reaction']) test(`queued ${kind} resolves legacy minting at dispatch`, async t => {
  const state = fixture({ notes: [legacy('#0')] });
  const gate = deferred(), calls = [];
  const minted = { ...state.selectedTicket, notes: [legacy('mint')] };
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push([url, opts.method, opts.body && JSON.parse(opts.body)]);
    if (calls.length === 1) await gate.promise;
    return response(minted);
  });
  const first = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  let second;
  if (kind === 'Reply') { state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'reply'; second = state.submitReply(); }
  if (kind === 'Edit') { state.noteOpenEdit(state.selectedTicket.notes[0]); state.noteEditDraft = 'edited'; second = state.saveNoteEdit(); }
  if (kind === 'Delete') { state.noteAskDelete(state.selectedTicket.notes[0]); second = state.confirmDeleteNote(); }
  if (kind === 'Reaction') second = state.toggleReaction(state.selectedTicket.notes[0], '👀');
  gate.resolve(); await Promise.all([first, second]);
  assert.equal(calls.length, 2);
  if (kind === 'Reply') assert.deepEqual(calls[1], ['/api/tickets/one/note', 'POST', { text: 'reply', parent: 'mint' }]);
  else assert.equal(calls[1][0], '/api/tickets/one/notes/mint' + (kind === 'Reaction' ? '/reactions' : ''));
});

for (const [name, before, after, expected] of [
  ['shift is not positional', [legacy('#0', 'removed'), legacy('#1')], [legacy('#0')], '#0'],
  ['ambiguous duplicates', [legacy('#0'), legacy('#1')], [legacy('aaaa'), legacy('bbbb')], null],
  ['new duplicate is ambiguous', [legacy('#0')], [legacy('#0'), legacy('new1')], null],
  ['changed content cannot be guessed', [legacy('#0')], [legacy('#0', 'replacement')], null],
  ['stable anchors disambiguate', [legacy('#0'), legacy('anchor', 'anchor'), legacy('#2')], [legacy('left'), legacy('anchor', 'anchor'), legacy('right')], 'left'],
  ['stable ID survives changed content', [legacy('keep')], [legacy('keep', 'changed')], 'keep'],
  ['reordered anchors are uncertain', [legacy('#0'), legacy('left', 'left'), legacy('right', 'right')], [legacy('mint'), legacy('right', 'right'), legacy('left', 'left')], null],
]) test(`legacy draft correspondence: ${name}`, async t => {
  const state = fixture({ notes: before });
  state.noteOpenReply(name === 'shift is not positional' ? before[1] : before[0]); state.noteReplyDraft = 'retained reply';
  state._noteApplyTicket('one', { ...state.selectedTicket, notes: after });
  assert.equal(state.noteReplyOn, expected);
  let writes = 0;
  t.mock.method(globalThis, 'fetch', async () => { writes++; return response(state.selectedTicket); });
  if (!expected) {
    const item = state.noteRecoveryItems().find(r => r.text === 'retained reply');
    assert.ok(item); assert.equal(item.unresolved, true);
    await state.submitReply();
    assert.equal(writes, 0, 'unresolved work cannot target a positional namesake');
  } else assert.equal(state.noteReplyDraft, 'retained reply');
});

test('accepted tail is awaited by detail hydration, not by leaving', async t => {
  const state = fixture({ source: 'team', can_edit: true });
  const gate = deferred(), calls = [];
  const { ticketRead, readTicketJSON } = kontoraTickets();
  Object.assign(state, {
    ticketRead, readTicketJSON,
    _navigationSeq: 0, currentView: 'board', _editorGeneration: 0,
    _releaseEditorBody() {}, _retireEditorState() {}, closeAgentPicker() {}, flushEditSave() {},
    syncDetailCoverage() {}, _pushRecentTicket() {}, fetchTicketCost() {},
    _resetActivity() {}, _resetTicketCost() {}, writeHash() {},
    restoreDetailView: async () => true, startEditing: async () => {},
  });
  const original = structuredClone(state.selectedTicket);
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push(opts?.method || 'GET');
    if (opts?.method === 'POST') await gate.promise;
    return response(original);
  });
  state.noteDraft = 'accepted'; const adding = state.submitNote();
  state.closeDetail();
  assert.equal(state.selectedTicket, null);
  const opening = state.selectTicket(original);
  await tick(); assert.deepEqual(calls, ['POST']);
  gate.resolve(); await Promise.all([adding, opening]);
  assert.deepEqual(calls, ['POST', 'GET']);
});

test('ambiguous queued target stays visible and never dispatches, even after a later unique snapshot', async t => {
  const state = fixture({ notes: [legacy('#0'), legacy('#1')] });
  const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => { calls.push([url, opts.method]); await gate.promise; return response({ ...state.selectedTicket, notes: [legacy('aaaa'), legacy('bbbb')] }); });
  const first = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  state.noteOpenReply(state.selectedTicket.notes[1]); state.noteReplyDraft = 'must survive';
  const second = state.submitReply();
  gate.resolve(); await Promise.all([first, second]);
  assert.equal(calls.length, 1);
  assert.ok(state.noteRecoveryItems().some(r => r.text === 'must survive' && r.unresolved));
  state._noteApplyTicket('one', { ...state.selectedTicket, notes: [legacy('bbbb')] });
  const item = state.noteRecoveryItems().find(r => r.text === 'must survive');
  state.noteRestoreRecovery(item.id);
  await state.submitReply();
  assert.equal(calls.length, 1, 'an unresolved identity cannot silently become a different note later');
});

for (const source of ['primary', 'team']) test(`accepted SSE snapshots remap only their source-qualified draft: ${source}`, () => {
  const state = fixture({ source, can_edit: true, notes: [legacy('#0')] });
  const store = kontoraTickets();
  Object.assign(state, { applyTicketUpdate: store.applyTicketUpdate, _mergeDetailProjection: store._mergeDetailProjection,
    refreshInitContext() {}, reconcileOutput() {}, logTabActive: () => false, createForm: {}, secondaryTickets: [] });
  state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'source-owned';
  const namesake = { ...state.selectedTicket, source: source === 'primary' ? 'other' : 'primary', notes: [legacy('wrong')] };
  state.applyTicketUpdate(namesake);
  assert.equal(state.noteReplyOn, '#0');
  state.applyTicketUpdate({ ...state.selectedTicket, notes: [legacy('right')] });
  assert.equal(state.noteReplyOn, 'right');
  assert.equal(state.noteReplyDraft, 'source-owned');
});

for (const source of ['primary', 'team']) test(`note acknowledgement preserves description editor ownership: ${source}`, () => {
  const state = fixture({ source, can_edit: true, revision: 'r1', notes: [legacy('#0')], description_tail: 'old tail' });
  state.editing = true; state.editForm = { body: 'newer unsaved editor text' };
  state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'reply draft';
  const key = state.ticketKey(state.selectedTicket);
  assert.equal(state._noteApplyTicket(key, { ...state.selectedTicket, revision: 'r2', description: 'remote description', description_tail: 'new tail', notes: [legacy('mint')] }), true);
  assert.equal(state.selectedTicket.description, 'Description\n');
  assert.equal(state.selectedTicket.description_tail, 'new tail');
  assert.equal(state.selectedTicket.body, 'Description\nnew tail');
  assert.equal(state.selectedTicket.revision, source === 'team' ? 'r1' : 'r2');
  assert.equal(state.editForm.body, 'newer unsaved editor text');
  assert.equal(state.noteReplyOn, 'mint');
});

for (const source of ['primary', 'team']) for (const kind of ['Reply', 'Edit', 'Reaction']) {
  test(`failed delete acknowledgement invalidates queued legacy ${kind}: ${source}`, async t => {
    const state = fixture({ source, can_edit: true, notes: [legacy('#0', 'removed'), legacy('#1', 'intended'), legacy('#2', 'wrong')] });
    const gate = deferred(), calls = [];
    let persisted = structuredClone(state.selectedTicket);
    t.mock.method(globalThis, 'fetch', async (url, opts) => {
      calls.push([url, opts?.method || 'GET']);
      if (calls.length === 1) {
        await gate.promise;
        persisted.notes = [legacy('#0', 'intended'), legacy('#1', 'wrong')];
        return { ok: false, status: 500, json: async () => ({ error: 'ticket read failed' }) };
      }
      return response(persisted);
    });
    state.noteAskDelete(state.selectedTicket.notes[0]);
    const first = state.confirmDeleteNote();
    const target = state.selectedTicket.notes[1];
    let second;
    if (kind === 'Reply') { state.noteOpenReply(target); state.noteReplyDraft = 'queued'; second = state.submitReply(); }
    if (kind === 'Edit') { state.noteOpenEdit(target); state.noteEditDraft = 'queued'; second = state.saveNoteEdit(); }
    if (kind === 'Reaction') second = state.toggleReaction(target, '👀');
    gate.resolve(); await Promise.all([first, second]);
    assert.deepEqual(persisted.notes.map(n => n.text), ['intended', 'wrong']);
    assert.equal(calls.length, 1, 'no queued mutation uses the old positional ID');
    const recoveries = state.noteRecoveryItems();
    assert.ok(recoveries.some(r => r.kind === 'Delete' && /Outcome uncertain/.test(r.reason)));
    assert.ok(recoveries.some(r => r.kind === kind && r.unresolved && /Not sent/.test(r.reason)));
    state._noteApplyTicket(state.ticketKey(persisted), persisted);
    assert.ok(state.noteRecoveryItems().some(r => r.kind === kind && r.unresolved), 'a later snapshot cannot revive an invalidated target');
  });
}

for (const failure of ['lost status', 'lost body', 'failed refresh']) test(`unavailable acknowledgement never dispatches a stale legacy target: ${failure}`, async t => {
  const state = fixture({ notes: [legacy('#0')] });
  const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (_url, opts) => {
    calls.push(opts?.method || 'GET');
    if (!opts) return { ok: false };
    await gate.promise;
    if (failure === 'lost status') throw new Error('offline');
    if (failure === 'lost body') return { ok: true, json: async () => { throw new Error('decode failed'); } };
    return response({ ...state.selectedTicket, notes: [legacy('mint')] });
  });
  const first = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'queued reply';
  const second = state.submitReply();
  if (failure === 'failed refresh') { state._selectedTicketUpdateSeq++; }
  gate.resolve(); await Promise.all([first, second]);
  assert.deepEqual(calls, failure === 'failed refresh' ? ['POST', 'GET'] : ['POST']);
  assert.ok(state.noteRecoveryItems().some(r => r.text === 'queued reply' && r.unresolved));
});

test('source-mismatched HTTP acknowledgement retains text without adopting the namesake', async t => {
  const state = fixture({ source: 'team', can_edit: true });
  t.mock.method(globalThis, 'fetch', async () => response({ id: 'one', source: 'other', notes: [legacy('wrong')] }));
  state.noteDraft = 'original source'; await state.submitNote();
  assert.equal(state.selectedTicket.source, 'team');
  assert.equal(state.noteDraft, 'original source');
  assert.ok(state.noteRecoveryItems().some(r => r.text === 'original source'));
});

test('navigation during stale acknowledgement refresh still resolves the next accepted target', async t => {
  const state = fixture({ notes: [legacy('#0')] });
  const mutation = deferred(), read = deferred(), calls = [];
  const full = { ...state.selectedTicket, notes: [legacy('mint')] };
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push([url, opts?.method || 'GET']);
    if (calls.length === 1) { await mutation.promise; return response(full); }
    if (!opts) return { ok: true, json: async () => { await read.promise; return structuredClone(full); } };
    return response(full);
  });
  const first = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'captured reply';
  const second = state.submitReply();
  state._selectedTicketUpdateSeq++;
  mutation.resolve(); await tick();
  state.noteResetDrafts(); state.selectedTicket = { id: 'other', notes: [] }; state._ticketDetailSeq++;
  read.resolve(); await Promise.all([first, second]);
  assert.deepEqual(calls.map(c => c[1]), ['POST', 'GET', 'POST']);
  assert.equal(calls.at(-1)[0], '/api/tickets/one/note');
  state.selectedTicket = full;
  assert.equal(state.noteRecoveryItems().some(r => r.text === 'captured reply'), false);
});

test('independent source-qualified queues do not block each other or adopt each other\'s note IDs', async t => {
  const state = fixture({ source: 'team', can_edit: true, notes: [legacy('#0')] });
  const team = structuredClone(state.selectedTicket), other = { ...team, source: 'other' };
  const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => {
    calls.push([url, opts.body && JSON.parse(opts.body)]);
    if (url.includes('/team/')) { await gate.promise; return response({ ...team, notes: [legacy('team-id')] }); }
    return response({ ...other, notes: [legacy('other-id')] });
  });
  const first = state.toggleReaction(team.notes[0], '👍');
  state.noteOpenReply(team.notes[0]); state.noteReplyDraft = 'team reply'; const reply = state.submitReply();
  state.noteResetDrafts(); state.selectedTicket = other; state._ticketDetailSeq++;
  await state.toggleReaction(other.notes[0], '👀');
  assert.equal(calls.length, 2);
  gate.resolve(); await Promise.all([first, reply]);
  assert.equal(calls[2][0], '/api/sources/team/tickets/one/note');
  assert.equal(calls[2][1].parent, 'team-id');
  assert.equal(state.selectedTicket.notes[0].id, 'other-id');
});

test('recovery restoration never overwrites newer text and requires explicit submission', async t => {
  const state = fixture(); let calls = 0;
  t.mock.method(globalThis, 'fetch', async () => { calls++; throw new Error('offline'); });
  state.noteDraft = 'submitted'; await state.submitNote();
  const retained = state.noteRecoveryItems()[0];
  state.noteDraft = 'newer'; state.noteRestoreRecovery(retained.id);
  assert.equal(state.noteDraft, 'newer');
  state.noteDraft = ''; state.noteRestoreRecovery(retained.id);
  assert.equal(state.noteDraft, 'submitted');
  assert.equal(calls, 1);
});

for (const delayed of ['headers', 'json', 'failure']) test(`Changes request ownership: delayed ${delayed}`, async t => {
  const state = fixture({ branch: 'feature', summary: '' });
  const gate = deferred(); let reads = 0;
  t.mock.method(globalThis, 'fetch', async () => {
    if (++reads !== 1) return response({ commits: [{ sha: 'new' }], files: [] });
    if (delayed === 'headers') { await gate.promise; return response({ commits: [{ sha: 'old' }] }); }
    if (delayed === 'failure') { await gate.promise; throw new Error('offline'); }
    return { ok: true, json: async () => { await gate.promise; return { commits: [{ sha: 'old' }] }; } };
  });
  state.detailLoading = true;
  const first = state.fetchChanges('one');
  await tick();
  assert.equal(state.ticketChangesLoading, true);
  assert.equal(await state.fetchChanges('one'), true);
  gate.resolve(); await first;
  assert.equal(state.ticketChanges.commits[0].sha, 'new');
  assert.equal(state.ticketChangesError, null);
  assert.equal(state.ticketChangesLoading, false);
  assert.equal(state.detailLoading, true, 'Changes never completes detail hydration');
});

test('Changes distinguishes error, retry and empty without a summary', async t => {
  const state = fixture({ branch: 'branch' }); let reads = 0;
  t.mock.method(globalThis, 'fetch', async () => ++reads === 1 ? { ok: false, json: async () => ({ error: 'git unavailable' }) } : response({ commits: [], files: [] }));
  assert.equal(await state.fetchChanges('one'), false);
  assert.equal(state.ticketChangesError, 'git unavailable');
  assert.equal(state.ticketChangesLoading, false);
  assert.equal(await state.fetchChanges('one'), true);
  assert.equal(state.ticketChangesError, null);
  assert.equal(state.diffEmpty(), true);
});

for (const boundary of ['reset', 'selection', 'secondary', 'reopen']) test(`Changes rejects response after ${boundary}`, async t => {
  const state = fixture(); const gate = deferred();
  t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return response({ commits: [{ sha: 'stale' }] }); });
  const request = state.fetchChanges('one');
  if (boundary === 'reset') state.resetChanges();
  if (boundary === 'selection') state.selectedTicket = { id: 'other' };
  if (boundary === 'secondary') state.selectedTicket = { id: 'one', source: 'team' };
  if (boundary === 'reopen') state._ticketDetailSeq++;
  gate.resolve();
  assert.equal(await request, false);
  assert.equal(state.ticketChanges, null);
});

test('notify saves serialize latest draft and only acknowledge notification fields', async t => {
  const state = fixture({ revision: 'r1' });
  t.mock.method(globalThis, 'setTimeout', () => 1);
  const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (_url, opts) => {
    const payload = JSON.parse(opts.body); calls.push(payload);
    if (calls.length === 1) await gate.promise;
    return response({ id: 'one', status: 'open', revision: 'obsolete', description: 'OLD', notes: [], ...payload });
  });
  state.openNotifyEditor();
  state.notifyDraft.notify = ['done'];
  const first = state.saveNotify();
  state.notifyDraft.notify = ['paused']; const second = state.saveNotify();
  state.notifyDraft.notify = ['human_review']; const third = state.saveNotify();
  assert.equal(calls.length, 1);
  state.selectedTicket = { ...state.selectedTicket, description: 'newer', notes: [{ id: 'new' }], status: 'paused', revision: 'r2' };
  state.tickets[0] = { ...state.selectedTicket };
  state._selectedTicketUpdateSeq++;
  gate.resolve(); await Promise.all([first, second, third]);
  assert.deepEqual(calls.map(c => c.notify), [['done'], ['human_review']]);
  assert.equal(state.selectedTicket.description, 'newer');
  assert.equal(state.selectedTicket.revision, 'r2');
  assert.equal(state.selectedTicket.notes[0].id, 'new');
  assert.equal(state.selectedTicket.status, 'paused');
  assert.equal(state.tickets[0].status, 'paused');
  assert.deepEqual(state.selectedTicket.notify, ['human_review']);
  assert.equal(state.notifySaving, false);
  assert.equal(await state.flushNotifySave('one'), true);
});

test('failed notification draft blocks lifecycle without automatic replay', async t => {
  const state = fixture(); let writes = 0;
  t.mock.method(globalThis, 'fetch', async () => { writes++; throw new Error('offline'); });
  state.openNotifyEditor(); state.notifyDraft.notify = ['done'];
  assert.equal(await state.saveNotify(), false);
  assert.equal(await state.flushNotifySave('one'), false);
  assert.equal(writes, 1);
  assert.deepEqual(state.notifyDraft.notify, ['done']);
  assert.match(state.notifyError, /offline/);
});

test('uncertain notification failure does not replay a queued draft and reports its unsent ownership', async t => {
  const state = fixture(); const gate = deferred(); let writes = 0;
  t.mock.method(globalThis, 'fetch', async () => { writes++; await gate.promise; throw new Error('offline'); });
  state.openNotifyEditor(); state.notifyDraft.notify = ['done']; const first = state.saveNotify();
  state.notifyDraft.notify = ['paused']; const second = state.saveNotify();
  gate.resolve(); assert.equal(await first, false); assert.equal(await second, false);
  assert.equal(writes, 1); assert.deepEqual(state.notifyDraft.notify, ['paused']);
  assert.match(state.notifyError, /newer draft has not been sent/);
  assert.equal(await state.flushNotifySave('one'), false);
  assert.equal(writes, 1);
});

test('notification barrier drains a newer unsent draft after the current request', async t => {
  const state = fixture(); const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'setTimeout', () => 1);
  t.mock.method(globalThis, 'fetch', async (_url, opts) => {
    const payload = JSON.parse(opts.body); calls.push(payload);
    if (calls.length === 1) await gate.promise;
    return response({ id: 'one', ...payload });
  });
  state.openNotifyEditor(); state.notifyDraft.notify = ['done'];
  const saving = state.saveNotify();
  const flushing = state.flushNotifySave('one');
  state.notifyDraft.notify = ['paused'];
  gate.resolve();
  await saving;
  assert.equal(await flushing, true);
  assert.deepEqual(calls.map(c => c.notify), [['done'], ['paused']]);
});

test('notification acknowledgement cannot restore revoked permissions or newer notification values', async t => {
  const state = fixture({ can_edit: true, revision: 'r1' }); const gate = deferred();
  t.mock.method(globalThis, 'setTimeout', () => 1);
  t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return response({ id: 'one', can_edit: true, revision: 'r1', notify: ['done'] }); });
  state.openNotifyEditor(); state.notifyDraft.notify = ['done']; const request = state.saveNotify();
  state.selectedTicket = { ...state.selectedTicket, notify: ['waiting'], can_edit: false, revision: 'r2' };
  state.tickets[0] = { ...state.selectedTicket };
  state._selectedTicketUpdateSeq++;
  gate.resolve(); await request;
  assert.equal(state.selectedTicket.can_edit, false);
  assert.equal(state.selectedTicket.revision, 'r2');
  assert.deepEqual(state.selectedTicket.notify, ['waiting']);
  assert.deepEqual(state.tickets[0].notify, ['waiting']);
});

test('old Changes failure cannot finish a newer pending read', async t => {
  const state = fixture(); const old = deferred(), current = deferred(); let reads = 0;
  t.mock.method(globalThis, 'fetch', async () => {
    if (++reads === 1) { await old.promise; throw new Error('old failure'); }
    await current.promise; return response({ files: [], commits: [] });
  });
  const first = state.fetchChanges('one'), second = state.fetchChanges('one');
  old.resolve(); await first;
  assert.equal(state.ticketChangesLoading, true);
  assert.equal(state.ticketChangesError, null);
  current.resolve(); await second;
  assert.equal(state.ticketChangesLoading, false);
});

test('a committed note whose stale refresh fails is not offered as an unsaved draft', async t => {
  const state = fixture(); const gate = deferred(); let writes = 0;
  t.mock.method(globalThis, 'fetch', async (_url, opts) => {
    if (!opts) throw new Error('read failed');
    writes++; await gate.promise; return response({ id: 'one', notes: [{ id: 'sent' }] });
  });
  state.noteDraft = 'sent'; const request = state.submitNote();
  state.selectedTicket = { ...state.selectedTicket, can_edit: false };
  state._selectedTicketUpdateSeq++;
  gate.resolve(); await request;
  assert.equal(state.noteDraft, '');
  assert.match(state.error, /Note saved/);
  assert.equal(writes, 1);
  assert.equal(state.selectedTicket.can_edit, false);
});

test('an old reply failure does not report over a newer composer', async t => {
  const state = fixture({ notes: [{ id: 'n', author: 'me' }] }); const gate = deferred();
  t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return { ok: false, json: async () => ({ error: 'old rejection' }) }; });
  state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'old';
  const request = state.submitReply();
  state.noteCancelReply(); state.noteOpenReply(state.selectedTicket.notes[0]); state.noteReplyDraft = 'new';
  gate.resolve(); await request;
  assert.equal(state.error, null);
  assert.equal(state.noteReplyDraft, 'new');
});

for (const boundary of ['source', 'reopen']) test(`notify feedback belongs to submitted editor after ${boundary}`, async t => {
  const state = fixture(); const gate = deferred();
  t.mock.method(globalThis, 'setTimeout', () => 1);
  t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return response({ id: 'one', notify: ['done'] }); });
  state.openNotifyEditor(); state.notifyDraft.notify = ['done']; const request = state.saveNotify();
  if (boundary === 'source') { state.selectedTicket = { id: 'one', source: 'team', can_edit: true }; state._ticketDetailSeq++; }
  state.openNotifyEditor(); state.notifyDraft.notify = ['paused']; state.notifySaving = true;
  gate.resolve(); await request;
  assert.deepEqual(state.notifyDraft.notify, ['paused']);
  assert.equal(state.notifySaved, false);
  assert.equal(state.notifySaving, true, 'old finalizer cannot clear a new owner');
});
