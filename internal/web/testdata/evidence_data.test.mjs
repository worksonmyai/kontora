import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraActivity } from '../ui/activity.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraNotes } from '../ui/notes.js';
import { kontoraNotify } from '../ui/notify.js';
import { kontoraSources } from '../ui/sources.js';
import { kontoraMobile } from '../ui/mobile.js';

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const tick = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const response = data => ({ ok: true, headers: { get: () => null }, json: async () => structuredClone(data) });
function fixture(ticket = {}) {
  const state = Object.assign({}, kontoraSources(), kontoraActivity(), kontoraDetail(), kontoraNotes(), kontoraNotify(), { mobileSwitchTab: kontoraMobile().mobileSwitchTab }, {
    selectedTicket: { id: 'one', status: 'open', notify: [], notify_channels: [], notes: [], description: 'Description\n', description_tail: '', ...ticket },
    tickets: [], secondaryTickets: [], _selectedTicketUpdateSeq: 0, _ticketDetailSeq: 1, _editorSaves: {},
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
  assert.ok(calls.includes('/api/tickets/one/activity?stage=code&run=' + rows.at(-1).run));
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
  const gate = deferred(), calls = [];
  t.mock.method(globalThis, 'fetch', async (url, opts) => { calls.push([url, opts.method]); if (calls.length === 1) await gate.promise; return response(state.selectedTicket); });
  state.noteDraft = 'hello';
  const adding = state.submitNote();
  const reacting = state.toggleReaction(state.selectedTicket.notes[0], '👍');
  await tick(); assert.equal(calls.length, 1);
  gate.resolve(); await Promise.all([adding, reacting]);
  assert.deepEqual(calls, [['/api/sources/team/tickets/one/note', 'POST'], ['/api/sources/team/tickets/one/notes/n/reactions', 'POST']]);
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
