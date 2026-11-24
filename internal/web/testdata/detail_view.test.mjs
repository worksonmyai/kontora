import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraDetailView } from '../ui/detail_view.js';
import { kontoraSources } from '../ui/sources.js';
import { kontoraBoard } from '../ui/board.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraMobile } from '../ui/mobile.js';
import { kontoraActivity } from '../ui/activity.js';

function app() {
  const scroller = { scrollTop: 85, getClientRects: () => [1] };
  const backgrounds = [{ inert: false }];
  globalThis.document = { activeElement: null,
    querySelector: () => ({ querySelectorAll: () => [scroller], querySelector: () => null, contains: () => false }),
    querySelectorAll: () => backgrounds };
  globalThis.getComputedStyle = () => ({ visibility: 'visible' });
  globalThis.requestAnimationFrame = fn => fn();
  globalThis.Alpine = { watch: () => () => {} };
  const calls = [];
  const state = Object.assign(kontoraSources(), kontoraDetailView(), {
    selectedTicket: { id: 'kon-test', status: 'paused', history: [
      { stage: 'code', run: 0 }, { stage: 'code', run: 0 },
    ] },
    isMobile: true, currentView: 'board', detailLoading: false, editing: false,
    detailTab: 'notes', activeTab: 'notes', activityStage: 'code', activityRun: 1,
    _activityExplicit: true, logFollow: false, logViewStage: 'code',
    $nextTick: fn => { fn?.(); return Promise.resolve(); },
    mobileHasRun: t => !!t.history?.length, executionActive: t => t?.status === 'in_progress',
    runningRun: () => null, editorCanEdit: () => false,
    mobileSwitchTab(mode) { this.detailTab = mode; calls.push(['mobile', mode]); },
    switchTab(mode) { this.activeTab = mode; calls.push(['desktop', mode]); },
    openActivity(stage, run) { this.activeTab = 'activity'; calls.push(['run', stage, run]); },
    fetchStageLogs(id, stage) { calls.push(['logs', id, stage]); },
    _mobileFocusAvailable: () => false,
  });
  return { state, calls, scroller, backgrounds };
}

for (const [mode, mobile, expected] of [
  ['notes', true, ['mobile', 'notes']],
  ['diff', true, ['mobile', 'diff']],
  ['notes', false, ['desktop', 'notes']],
  ['diff', false, ['desktop', 'diff']],
  ['activity', false, ['run', 'code', 1]],
  ['logs', false, ['run', 'code', 1]],
  ['terminal', true, ['mobile', 'activity']],
]) test(`local detail restoration: ${mode}, ${mobile ? 'phone' : 'desktop'}`, async () => {
  const { state, calls, scroller } = app();
  state.detailTab = mode;
  state.rememberDetailView();
  state.isMobile = mobile;
  state.detailTab = 'ticket';
  state.activeTab = 'ticket';
  scroller.scrollTop = 0;
  assert.equal(await state.restoreDetailView(), true);
  assert.deepEqual(calls, [expected]);
  if (mode !== 'terminal' && mode !== 'logs') assert.equal(scroller.scrollTop, 85);
  assert.equal(state.activityRun, 1);
});

test('named detail scrollers restore by role when another region appears', () => {
  const { state, scroller } = app();
  scroller.dataset = { detailScroll: 'activity' };
  const added = { dataset: { detailScroll: 'question' }, scrollTop: 30 };
  state.detailTab = 'activity';
  state.detailViewScrollers = () => [scroller];
  state.rememberDetailView();
  state.detailViewScrollers = () => [added, scroller];
  scroller.scrollTop = 0;
  state.restoreDetailScroll();
  assert.equal(added.scrollTop, 0);
  assert.equal(scroller.scrollTop, 85);
});

for (const source of ['secondary', 'other']) test(`local restoration does not substitute ${source} namesakes`, async () => {
  const { state } = app();
  state.rememberDetailView();
  state.selectedTicket = { ...state.selectedTicket, source };
  assert.equal(await state.restoreDetailView(), false);
});

for (const [kind, expected] of [['first', true], ['saved', false], ['unchanged', false], ['new notes', true], ['hidden', false]]) {
  test(`Notes follow preserves local restoration: ${kind}`, () => {
    const { state, scroller } = app();
    if (kind === 'saved') state.rememberDetailView();
    if (['unchanged', 'new notes'].includes(kind)) state.detailNotesShouldFollow(scroller);
    if (kind === 'new notes') state.selectedTicket.notes = [{ id: 'new', text: 'New note' }];
    if (kind === 'hidden') state.detailTab = 'ticket';
    assert.equal(state.detailNotesShouldFollow(scroller), expected);
  });
}

test('covered board siblings become inert, not the detail ancestor', () => {
  const { state, backgrounds } = app();
  state.syncDetailCoverage();
  assert.equal(backgrounds[0].inert, true);
  state.selectedTicket = null;
  state.syncDetailCoverage();
  assert.equal(backgrounds[0].inert, false);
});

// Keep the real tab handlers and loaders: restoration must wait for their reads,
// not only for the first frame after a pane opens.
function integratedApp() {
  const fixture = app();
  const { state, scroller } = fixture;
  const ticks = [], frames = [], watches = new Set(), reads = [];
  for (const mixin of [kontoraBoard(), kontoraDetail(), kontoraMobile(), kontoraActivity()]) {
    Object.defineProperties(state, Object.getOwnPropertyDescriptors(mixin));
  }
  Object.assign(state, {
    selectedTicket: { id: 'kon-test', status: 'paused', stage: 'code', history: [
      { stage: 'plan' }, { stage: 'code' }, { stage: 'code' },
    ] },
    activityStage: 'code', activityRun: 1, _activityExplicit: true,
    activity: { tape: { events: [] } }, ticketChanges: { files: [] }, logViewContent: 'saved logs',
    logViewStage: 'plan', logFollow: false, _activityLoadSeq: 0, _activityPoll: null,
    _ticketDetailSeq: 1, _appDestroyed: false, terminalOpen: false,
    $nextTick(fn) { if (fn) ticks.push(fn); return Promise.resolve(); },
    _invalidateRawLog() {}, _armActivityPoll() {},
    activityTarget: () => ({ stage: 'code', run: 1 }),
    importedRun: () => false, tapeEvents: () => [],
    outputSubject() { return this.isSecondary(this.selectedTicket) ? null : this.selectedTicket; },
    outputTab() { return this.isMobile ? this.detailTab === 'terminal' ? 'session' : this.detailTab : this.activeTab; },
    terminalWanted: () => false, closeTerminal() {}, _destroyTerminal() {},
    mobileBreakpointChanged() {}, closeAgentPicker() {}, _mountBoard() {},
    mobileFinishDescriptionEdit() {},
    startEditing() {},
  });
  globalThis.Alpine.watch = (value, callback) => {
    const watch = { value, callback, previous: JSON.stringify(value()) };
    watches.add(watch);
    return () => watches.delete(watch);
  };
  globalThis.requestAnimationFrame = fn => { frames.push(fn); };
  globalThis.fetch = url => new Promise(resolve => reads.push({ url, resolve }));
  let top = 85;
  Object.defineProperty(scroller, 'scrollTop', {
    get: () => top,
    set(value) {
      const mode = state.detailMode();
      const rendered = mode === 'activity' ? state.activity && !state.activityLoading
        : mode === 'diff' ? state.ticketChanges && !state.ticketChangesLoading
        : mode === 'logs' ? state.logViewContent !== null && !state.logViewLoading : true;
      top = rendered ? value : 0;
    },
  });
  return { ...fixture, reads, watches,
    async flush() {
      for (let i = 0; i < 4; i++) {
        await new Promise(resolve => setImmediate(resolve));
        for (const watch of [...watches]) {
          const next = JSON.stringify(watch.value());
          if (next !== watch.previous) { watch.previous = next; watch.callback(); }
        }
        ticks.splice(0).forEach(fn => fn());
        frames.splice(0).forEach(fn => fn());
      }
    },
    completeReads() {
      reads.forEach(read => read.resolve({ ok: true, status: 200,
        headers: { get: () => null },
        json: async () => read.url.includes('/activity') ? { tape: { events: [] } }
          : read.url.includes('/changes') ? { files: [] } : { content: 'restored logs' },
      }));
    },
  };
}

for (const route of ['reopen', 'resize']) {
  for (const mode of ['activity', 'diff', 'logs', 'terminal', 'notes', 'ticket']) {
    test(`secondary phone ${mode} uses a supported desktop pane on ${route}`, async () => {
      const fixture = integratedApp();
      const { state, reads } = fixture;
      state.selectedTicket.source = 'secondary';
      state.detailTab = mode;
      if (route === 'reopen') state.rememberDetailView();
      state.isMobile = false;
      if (route === 'reopen') {
        state.activeTab = 'ticket';
        state.detailTab = 'ticket';
        await state.restoreDetailView();
      } else state._onBreakpointChange();
      await fixture.flush();
      assert.equal(state.activeTab, mode === 'notes' ? 'notes' : 'ticket');
      assert.deepEqual(reads, [], 'secondary tickets must not read primary evidence endpoints');
    });
  }
}

for (const [explicit, savedStage] of [[true, 'code'], [false, 'code'], [false, null], [true, 'removed']]) {
  test(`phone Stage logs restored on desktop loads Activity (explicit=${explicit}, saved stage=${savedStage})`, async () => {
    const fixture = integratedApp();
    const { state, reads } = fixture;
    state.detailTab = 'logs';
    state._activityExplicit = explicit;
    state.activityStage = savedStage;
    state.rememberDetailView();
    state.isMobile = false;
    state.activity = null;
    state.activityStage = null;
    state.activeTab = 'ticket';
    await state.restoreDetailView();
    await fixture.flush();
    assert.equal(state.activeTab, 'activity');
    assert.deepEqual(reads.map(read => read.url), ['/api/tickets/kon-test/activity?stage=code&run=1&log_tail_bytes=262144']);
    fixture.completeReads();
    await fixture.flush();
  });
}

for (const mobile of [true, false]) {
  for (const mode of ['notes', 'activity', 'diff']) {
    test(`tab switches preserve each panel's scroll on ${mobile ? 'phone' : 'desktop'}: ${mode}`, async () => {
      const fixture = integratedApp();
      const { state, scroller } = fixture;
      state.isMobile = mobile;
      state.detailTab = state.activeTab = mode;
      const switchTab = tab => mobile ? state.mobileSwitchTab(tab) : state.switchTab(tab);
      switchTab('ticket');
      await fixture.flush();
      scroller.scrollTop = 23;
      switchTab(mode);
      fixture.completeReads();
      await fixture.flush();
      assert.equal(scroller.scrollTop, 85, 'returning restores the outgoing panel');
      switchTab('ticket');
      await fixture.flush();
      assert.equal(scroller.scrollTop, 23, 'panels keep independent offsets');
      state.rememberDetailView();
      scroller.scrollTop = 0;
      await state.restoreDetailView();
      await fixture.flush();
      assert.equal(scroller.scrollTop, 23);
      switchTab(mode);
      fixture.completeReads();
      await fixture.flush();
      assert.equal(scroller.scrollTop, 85, 'reopening retains the inactive panel');
    });
  }
  for (const mode of ['activity', 'diff']) {
    test(`delayed ${mode} restores scroll after rendering on ${mobile ? 'phone' : 'desktop'}`, async () => {
      const fixture = integratedApp();
      const { state, scroller, reads, watches } = fixture;
      state.isMobile = mobile;
      state.detailTab = state.activeTab = mode;
      state.rememberDetailView();
      state.activity = state.ticketChanges = null;
      scroller.scrollTop = 0;
      state.detailTab = state.activeTab = 'ticket';
      await state.restoreDetailView();
      await fixture.flush();
      assert.equal(reads.length, 1);
      assert.equal(scroller.scrollTop, 0, 'loading placeholders clamp the saved offset');
      fixture.completeReads();
      await fixture.flush();
      assert.equal(scroller.scrollTop, 85);
      assert.equal(watches.size, 0, 'restoration must release its watches');
    });
  }
}

for (const change of ['ticket', 'source', 'mode', 'width', 'reopen', 'destroy']) {
  test(`delayed scroll restoration cancels after ${change} changes`, async () => {
    const fixture = integratedApp();
    const { state, scroller, watches } = fixture;
    state.detailTab = 'diff';
    state.rememberDetailView();
    state.ticketChanges = null;
    scroller.scrollTop = 0;
    state.detailTab = 'ticket';
    await state.restoreDetailView();
    await fixture.flush();
    if (change === 'ticket') state.selectedTicket = { ...state.selectedTicket, id: 'other' };
    if (change === 'source') state.selectedTicket = { ...state.selectedTicket, source: 'secondary' };
    if (change === 'mode') state.detailTab = 'notes';
    if (change === 'width') state.isMobile = false;
    if (change === 'reopen') ++state._ticketDetailSeq;
    if (change === 'destroy') state._appDestroyed = true;
    await fixture.flush();
    fixture.completeReads();
    await fixture.flush();
    assert.equal(scroller.scrollTop, 0);
    assert.equal(watches.size, 0);
  });
}

test('phone Stage logs restore the inspected stage rather than the current stage', async () => {
  const fixture = integratedApp();
  const { state, reads } = fixture;
  state.detailTab = 'logs';
  state.rememberDetailView();
  state.logViewStage = null;
  state.detailTab = 'ticket';
  await state.restoreDetailView();
  await fixture.flush();
  assert.equal(state.detailTab, 'logs');
  assert.deepEqual(reads.map(read => read.url), ['/api/tickets/kon-test/logs?stage=plan']);
  fixture.completeReads();
  await fixture.flush();
  assert.equal(state.logViewStage, 'plan');
});
