import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { kontoraCreate, newCreateForm } from '../ui/create.js';
import { kontoraMobile } from '../ui/mobile.js';
import { kontoraSettings } from '../ui/settings.js';
import { kontoraFilter } from '../ui/filter.js';
import { kontoraArchive } from '../ui/archive.js';
import { localScheduleText } from '../ui/format.js';
import { deferred } from './settings_fixtures.mjs';

const html = readFileSync(new URL('../static/index.html', import.meta.url), 'utf8');
const alpine = vm.createContext({
  window: { Element: class {} },
  document: { createElement: () => ({ style: {} }), addEventListener() {} },
  MutationObserver: class { observe() {} }, queueMicrotask() {},
});
vm.runInContext(readFileSync(new URL('../static/vendor/alpinejs@3.14.8/cdn.min.js', import.meta.url), 'utf8'), alpine);

function harness({ reactive = false } = {}) {
  const state = {};
  for (const mixin of [kontoraSettings(), kontoraFilter(), kontoraMobile(), kontoraCreate()]) {
    Object.defineProperties(state, Object.getOwnPropertyDescriptors(mixin));
  }
  const hashes = [];
  Object.assign(state, {
    currentView: 'board', _navigationSeq: 0, selectedTicket: null, selectedEpic: null,
    createForm: newCreateForm(), createTouched: { pipeline: false, agent: false }, createSubmitting: false,
    configCache: { projects: [{ name: 'My project', path: '/repo', pipeline: 'build', agent: 'coder' }], agents: ['coder'] },
    tickets: [], automations: [], archiveRows: [], sheet: null, sourceFilter: 'secondary',
    closeAgentPicker() {}, _invalidateStageLogs() {}, _stopActivityPoll() {}, closeTerminal() {},
    flushEditSave() {}, closeStats() {}, closeArchive() {}, rememberDetailView() {},
    mobileResetTerminalInput() {}, closeSheet() { this.sheet = null; },
    writeHash() { hashes.push(this.currentView); },
    closeDetail() { this.selectedTicket = null; }, async closeEpic() { this.selectedEpic = null; },
    openStats() {}, openArchive() {}, async openSettings() { this.currentView = 'settings'; },
    automationsConfirmReplace: () => true, automationsLeave() {},
    settingsDirty: () => false, _settingsHasDrafts: () => false,
    projectDefaultsFor(path, epicChild) { return { pipeline: epicChild ? 'child' : this.projectForPath(path)?.pipeline || '', pipelineSource: 'pipeline' }; },
    pathBasename: path => path.split('/').at(-1),
    $nextTick: callback => callback(),
  });
  return { state: reactive ? alpine.window.Alpine.reactive(state) : state, hashes };
}

function fill(state) {
  Object.assign(state.createForm, { title: 'Keep this draft', path: '/repo', body: 'Description', branch: 'work', base_branch: 'main' });
}

for (const entry of ['route', 'mobile adapter']) {
  test(`${entry}: New is a full, guarded route and keeps the existing draft`, async () => {
    const { state } = harness();
    const enter = () => entry === 'route' ? state.gotoView('new') : state.openNewSheet();
    assert.equal(await enter(), true);
    fill(state);
    state.createForm.pipelineChoice = 'none';
    state.createTouched.agent = true;
    const form = state.createForm;
    await enter();
    assert.equal(state.createForm, form);
    state.closeCreateModal();
    await enter();
    assert.equal(state.createForm, form);
    assert.equal(state.createForm.title, 'Keep this draft');
    assert.equal(state.createForm.pipelineChoice, 'none');
    assert.equal(state.createTouched.agent, true);
    assert.equal(state.currentView, 'new');
    assert.equal(state.sheet, null);
    assert.equal(state.sourceFilter, 'secondary');
  });
}

for (const reactive of [false, true]) {
  for (const visible of [false, true]) {
    for (const result of ['success', 'http', 'network']) {
      test(`pending creation owns the draft: reactive=${reactive}, visible=${visible}, ${result}`, async () => {
        const { state } = harness({ reactive });
        await state.gotoView('new');
        fill(state);
        const form = state.createForm;
        const gate = deferred();
        let posts = 0;
        const previousFetch = globalThis.fetch;
        globalThis.fetch = async (url, options) => {
          assert.equal(url, '/api/tickets');
          assert.equal(options.method, 'POST');
          ++posts;
          await gate.promise;
          if (result === 'network') throw new Error('offline');
          return { ok: result === 'success', json: async () => ({ error: 'Rejected draft' }) };
        };
        try {
          const pending = state.submitCreateTicket();
          assert.equal(state.createSubmitting, true);
          assert.equal(state.resetCreateForm(), false);
          assert.equal(state.invalidateCreateForm(), false);
          state.closeCreateModal();
          assert.equal(state.createSubmitting, true, 'leaving must not claim cancellation');
          await state.gotoView('new');
          assert.equal(state.createForm, form);
          assert.equal(state.createForm.title, 'Keep this draft');
          assert.equal(state.createSubmitting, true);
          assert.equal(await state.submitCreateTicket(), false);
          if (!visible) await state.gotoView('stats');
          gate.resolve();
          assert.equal(await pending, result === 'success');
          assert.equal(posts, 1);
          assert.equal(state.createSubmitting, false);
          assert.equal(state._createRequest, null);
          assert.equal(state.currentView, visible ? result === 'success' ? 'board' : 'new' : 'stats');
          if (result === 'success') {
            await state.gotoView('new');
            assert.equal(state.createForm.title, '');
            assert.notEqual(state.createForm, form);
          } else {
            await state.gotoView('new');
            assert.equal(state.createForm, form);
            assert.match(state.createError, result === 'http' ? /Rejected draft/ : /result unknown.*Check the board/);
          }
          assert.equal(posts, 1, 'entry and completion never replay creation');
        } finally { globalThis.fetch = previousFetch; }
      });
    }
  }
}

test('creation stays pending through a delayed HTTP error body, even while hidden', async () => {
  const { state } = harness();
  await state.gotoView('new');
  fill(state);
  const body = deferred();
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: false, json: () => body.promise });
  try {
    const pending = state.submitCreateTicket();
    await Promise.resolve();
    state.closeCreateModal();
    await state.gotoView('new');
    assert.equal(await state.submitCreateTicket(), false);
    assert.equal(state.createSubmitting, true);
    body.resolve({ error: 'Invalid repository' });
    assert.equal(await pending, false);
    assert.equal(state.createError, 'Invalid repository');
    assert.equal(state.createSubmitting, false);
  } finally { globalThis.fetch = previousFetch; }
});

test('a late config read cannot modify a submitted draft or rewrite a newer route', async () => {
  const { state, hashes } = harness();
  state.configCache = null;
  const config = deferred(), post = deferred();
  state.loadConfigCache = async () => {
    await config.promise;
    state.configCache = { projects: [{ name: 'Other', path: '/repo', agent: 'other', pipeline: 'other' }] };
  };
  const entry = state.gotoView('new');
  fill(state);
  state.createForm.agent = 'chosen';
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => { await post.promise; return { ok: true }; };
  try {
    const pending = state.submitCreateTicket();
    await state.gotoView('stats');
    config.resolve();
    await entry;
    assert.equal(state.createForm.agent, 'chosen');
    assert.equal(hashes.at(-1), 'stats');
    post.resolve();
    assert.equal(await pending, true);
    assert.equal(state.currentView, 'stats');
  } finally { globalThis.fetch = previousFetch; }
});

for (const mode of ['todo', 'open', 'scheduled', 'epic', 'inherit', 'none', 'explicit', 'epic child']) {
  test(`full creation preserves ${mode} request semantics`, async () => {
    const { state } = harness();
    await state.gotoView('new');
    fill(state);
    if (mode === 'todo' || mode === 'scheduled') state.createForm.status = 'todo';
    if (mode === 'scheduled' || mode === 'epic') {
      state.setCreateScheduleMode('later');
      state.createForm.scheduled_at = '2099-09-01 09:00';
    }
    if (mode === 'epic') state.createForm.kind = 'epic';
    if (mode === 'none') state.createForm.pipelineChoice = 'none';
    if (mode === 'explicit') state.createForm.pipelineChoice = 'pipeline:custom';
    if (mode === 'epic child') {
      state.createForm.parent = 'epic-a';
      state.tickets = [{ id: 'epic-a', kind: 'epic' }];
      state.syncCreatePipeline();
      assert.equal(state.createForm.pipeline, 'child');
    }
    const writes = [];
    const preview = state.createPreviewYaml;
    const previousFetch = globalThis.fetch;
    globalThis.fetch = async (url, options) => { writes.push([url, options.method, JSON.parse(options.body)]); return { ok: true }; };
    try {
      assert.equal(await state.submitCreateTicket(), true);
      assert.equal(state.createForm.title, '', 'successful ticket and epic creation must clear the draft');
      assert.equal(writes.length, 1);
      const [url, method, body] = writes[0];
      assert.equal(url, '/api/tickets');
      assert.equal(method, 'POST');
      assert.equal(body.status, mode === 'todo' ? 'todo' : 'open');
      assert.match(preview, new RegExp('^status: ' + body.status + '$', 'm'));
      if (mode !== 'scheduled') assert.equal(body.scheduled_at, undefined);
      assert.equal(body.branch, 'work');
      assert.equal(body.base_branch, 'main');
      if (mode === 'epic') {
        assert.equal(body.kind, 'epic');
        assert.equal(body.scheduled_at, undefined);
        assert.equal(body.pipeline, undefined);
      } else if (mode === 'scheduled') assert.match(body.scheduled_at, /^2099-/);
      if (mode === 'none') assert.equal(body.pipeline, 'none');
      else if (mode === 'explicit') assert.equal(body.pipeline, 'custom');
      else assert.equal(body.pipeline, undefined, 'inheritance must not become a stored override');
      if (mode === 'epic child') assert.equal(body.parent, 'epic-a');
    } finally { globalThis.fetch = previousFetch; }
  });
}

test('New refuses an absolute schedule if it becomes due between preview and submit', async t => {
  const { state } = harness();
  await state.gotoView('new');
  fill(state);
  const deadline = new Date(2099, 0, 1, 12).getTime();
  t.mock.timers.enable({ apis: ['Date'], now: deadline - 1 });
  const fetch = t.mock.method(globalThis, 'fetch', async () => ({ ok: true }));
  state.setCreateScheduleMode('later');
  state.createForm.scheduled_at = localScheduleText(new Date(deadline));
  assert.equal(state.createBlocked(), false);
  t.mock.timers.tick(1);
  assert.equal(await state.submitCreateTicket(), false, 'a time equal to now is not future');
  assert.match(state.createError, /already passed/);
  t.mock.timers.tick(30_000);
  assert.equal(await state.submitCreateTicket(), false, 'no browser grace period for creation');
  assert.equal(fetch.mock.callCount(), 0);
  assert.equal(state.currentView, 'new');
});

test('New passes the one resolved schedule to the POST even if the clock advances', async t => {
  const { state } = harness();
  await state.gotoView('new');
  fill(state);
  const deadline = new Date(2099, 0, 1, 12);
  t.mock.timers.enable({ apis: ['Date'], now: deadline.getTime() - 1 });
  state.setCreateScheduleMode('later');
  state.createForm.scheduled_at = localScheduleText(deadline);
  state.closeAgentPicker = () => t.mock.timers.tick(60_001);
  const fetch = t.mock.method(globalThis, 'fetch', async () => ({ ok: true }));
  assert.equal(await state.submitCreateTicket(), true);
  assert.equal(fetch.mock.callCount(), 1);
  const body = JSON.parse(fetch.mock.calls[0].arguments[1].body);
  assert.equal(body.status, 'open');
  assert.equal(body.scheduled_at, deadline.toISOString().replace('.000Z', 'Z'));
});

test('New previews a delay and resolves it from submission, not from typing', async t => {
  const { state } = harness();
  await state.gotoView('new');
  fill(state);
  t.mock.timers.enable({ apis: ['Date'], now: new Date('2099-01-01T12:00:00Z') });
  state.setCreateScheduleMode('later');
  state.createForm.scheduled_at = '24h';
  assert.equal(state.createScheduleEcho().long, '24h after submission');
  assert.match(state.createPreviewYaml, /^# scheduled_at: 24h after submission$/m);
  assert.doesNotMatch(state.createPreviewYaml, /^scheduled_at:/m);
  t.mock.timers.tick(10 * 60 * 1000);
  const fetch = t.mock.method(globalThis, 'fetch', async () => ({ ok: true }));
  assert.equal(await state.submitCreateTicket(), true);
  const body = JSON.parse(fetch.mock.calls[0].arguments[1].body);
  assert.equal(body.status, 'open');
  assert.equal(body.scheduled_at, '2099-01-02T12:10:00Z');
});

test('dirty Settings blocks mobile New before draft cleanup and keeps its exact destination', async () => {
  const { state } = harness();
  state.currentView = 'settings';
  state.settingsDirty = () => true;
  state.sheet = { type: 'navigation' };
  fill(state);
  const form = state.createForm;
  assert.equal(await state.openNewSheet(), false);
  assert.equal(state.currentView, 'settings');
  assert.equal(state.settingsGuard, true);
  assert.equal(state.settingsGuardTarget, 'new');
  assert.equal(state.createForm, form);
  state.discardSettings = () => { state.settingsDirty = () => false; };
  await state.settingsGuardDiscard();
  assert.equal(state.currentView, 'new');
});

test('phone shortcuts retain the existing exact-match grammar and source identity', () => {
  const { state } = harness();
  state.toggleFilterToken('project', 'My project');
  assert.equal(state.searchQuery, 'project:="My project"');
  assert.equal(state.filterTokenActive('project', 'My project'), true);
  state.toggleFilterToken('agent', 'coder');
  const ticket = { id: 'same-id', source: 'secondary', path: '/repo', agent: 'coder' };
  assert.equal(state.ticketMatchesQuery(ticket, state.searchQuery), true);
  assert.equal(ticket.source, 'secondary');
  assert.equal(state.sourceFilter, 'secondary');
  state.toggleFilterToken('agent', 'coder');
  assert.equal(state.searchQuery, '');
  state.tickets = [ticket];
  state.automations = [{ id: 'auto-a' }, { id: 'auto-b' }];
  state.archiveRows = [{ id: 'archived' }];
  assert.equal(state.mobileNavigationCount('board'), 1);
  assert.equal(state.mobileNavigationCount('automations'), 2);
  assert.equal(state.mobileNavigationCount('archive'), 1);
  state._settingsHasDrafts = () => true;
  assert.equal(state.mobileNavigationCount('settings'), 'Unsaved changes');
});

test('New confirms the imported tickets without changing the draft or source scope', async () => {
  const { state } = harness({ reactive: true });
  await state.openCreateModal();
  fill(state);
  const draft = state.createForm;
  const original = { ...draft };
  let sent;
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    sent = { url, options };
    return { ok: true, json: async () => ({ tickets: [{ id: 'kon-imported' }], errors: null }) };
  };
  try {
    await state.handleUpload([new File(['# Ticket'], 'ticket.MD'), new File(['skip'], 'readme.txt')]);
    assert.equal(sent.url, '/api/tickets/upload');
    assert.equal(sent.options.method, 'POST');
    assert.equal(sent.options.headers['X-Kontora-Confirm'], 'upload-tickets');
    assert.deepEqual(sent.options.body.getAll('files').map(file => file.name), ['ticket.MD']);
    assert.equal(state.createUploadReceipt, 'Imported ticket: kon-imported.');
    assert.equal(state.currentView, 'new');
    assert.equal(state.createForm, draft);
    assert.deepEqual({ ...state.createForm }, original);
    assert.equal(state.sourceFilter, 'secondary');
  } finally { globalThis.fetch = previousFetch; }
});

for (const [status, body, message] of [
  [502, undefined, 'Upload failed (HTTP 502)'],
  [502, null, 'Upload failed (HTTP 502)'],
  [502, { error: 'Upload is disabled' }, 'Upload failed (HTTP 502): Upload is disabled'],
  [502, { errors: [{ file: 'ticket.md', error: 'Invalid title' }] }, 'Upload failed (HTTP 502): ticket.md: Invalid title'],
  [201, { tickets: [{ id: 'kon-a' }, { id: 'kon-b' }], errors: [{ file: 'ticket.md', error: 'Invalid title' }] }, 'Created tickets: kon-a, kon-b. Upload errors: ticket.md: Invalid title'],
]) test(`upload reports results without resubmitting (HTTP ${status}): ${JSON.stringify(body)}`, async () => {
  const { state } = harness();
  const previousFetch = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = async () => {
    requests++;
    return { ok: status < 400, status, json: async () => {
      if (body === undefined) throw new SyntaxError('Response is not JSON');
      return body;
    } };
  };
  try {
    await state.handleUpload([new File(['# Ticket'], 'ticket.md')]);
    assert.equal(state.error, message);
    assert.equal(requests, 1);
  } finally { globalThis.fetch = previousFetch; }
});

test('an upload that finishes after leaving New confirms the import outside the draft', async () => {
  const { state } = harness();
  await state.openCreateModal();
  fill(state);
  const response = deferred();
  const previousFetch = globalThis.fetch;
  globalThis.fetch = () => response.promise;
  const receipts = [];
  state.showToast = message => receipts.push(message);
  try {
    const pending = state.handleUpload([new File(['# Ticket'], 'ticket.md')]);
    state.closeCreateModal();
    response.resolve({ ok: true, json: async () => ({ tickets: [{ id: 'kon-a' }, { id: 'kon-b' }] }) });
    await pending;
    assert.deepEqual(receipts, ['Imported tickets: kon-a, kon-b.']);
    assert.equal(state.currentView, 'board');
    assert.equal(state.createForm.title, 'Keep this draft');
    assert.equal(state.createUploadReceipt, '');
  } finally { globalThis.fetch = previousFetch; }
});

test('archive row actions keep the clicked owner, not the current selection', () => {
  const state = kontoraArchive();
  const calls = [];
  state.selectedTicket = { id: 'different-ticket' };
  state.archiveOpenRow = row => calls.push(['open', row.id]);
  state.archiveRestore = id => calls.push(['restore', id]);
  state.archiveCopyRow = row => calls.push(['copy', row.id]);
  for (const action of ['restore', 'copy', 'open']) {
    state.archiveRowClick({ id: 'clicked-ticket' }, { target: { closest: () => ({ getAttribute: () => action }) } });
  }
  assert.deepEqual(calls, [['restore', 'clicked-ticket'], ['copy', 'clicked-ticket'], ['open', 'clicked-ticket']]);
});

test('phone navigation markup uses the shared route, upload input, filters and settings context', () => {
  assert.doesNotMatch(html, /@click="openNewSheet\(\)"/);
  assert.doesNotMatch(html, /sheet.type\s*===\s*'new'/);
  assert.doesNotMatch(html, /submitCreateTicketMobile|onMobileCreateSchedulePicked/);
  assert.match(html, /@click="mobileNavigate\('new'\)" class="mobile-primary" aria-label="New ticket"/);
  assert.match(html, /@click="\$refs.uploadInput.click\(\)"[^>]*>Import tickets from \.md/);
  assert.match(html, /Valid files are imported immediately as Open tickets\. This form stays unchanged\./);
  assert.match(html, /x-model="searchQuery"[^>]*aria-label="Search tickets"/);
  assert.match(html, /x-text="mobileFilterSummary\(\)"/);
  assert.match(html, /mobileNavigationCount\(view.key\)/);
  assert.match(html, /settings-mobile-context[\s\S]{0,600}configCache\?\.config_path/);
  const creation = html.slice(html.indexOf('id="new-view"'), html.indexOf('id="automations-view"'));
  assert.match(creation, /fieldset :disabled="createSubmitting"/);
  assert.match(creation, /Leaving this page does not cancel the request/);
  assert.match(creation, /Create and queue/);
  assert.match(creation, /Draft \(Open\)/);
  assert.doesNotMatch(creation, /Cancel and return|>\s*cancel\s*</);
  assert.match(html, /@click.stop="archiveOpenRow\(row\)" class="archive-row-title/);
});
