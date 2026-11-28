import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraSettings } from '../ui/settings.js';
import { kontoraAutomations } from '../ui/automations.js';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraSources } from '../ui/sources.js';

// Use the shipped Alpine proxies: a guard target read back from component
// state is not the plain destination object passed into gotoView.
const alpine = vm.createContext({
  window: { Element: class {} },
  document: { createElement: () => ({ style: {} }), addEventListener() {} },
  MutationObserver: class { observe() {} }, queueMicrotask() {},
});
vm.runInContext(readFileSync(new URL('../static/vendor/alpinejs@3.14.8/cdn.min.js', import.meta.url), 'utf8'), alpine);
alpine.queueMicrotask = queueMicrotask;

// Both native go() AND assigning location.hash emit popstate + hashchange in
// Chromium. pushState/replaceState emit neither. Replace cannot masquerade as
// a successful restoration of a refused Back.
function harness(hash = '#/', { reactive = false, nativeNavigation = false } = {}) {
  const listeners = new Map(), events = [], traversals = [];
  let cursor = 0;
  const entries = [{ hash, state: { unrelated: { keep: true } } }];
  const location = { get hash() { return entries[cursor].hash; }, set hash(value) {
    entries.splice(++cursor); entries.push({ hash: value, state: null });
    events.push(() => { emit('popstate'); emit('hashchange'); });
  } };
  const emit = type => { for (const fn of listeners.get(type) || []) fn({ type, state: history.state }); };
  const history = {
    get length() { return entries.length; },
    get state() { return entries[cursor].state; },
    pushState(state, _, hash) { entries.splice(++cursor); entries.push({ hash, state: structuredClone(state) }); },
    replaceState(state, _, hash) { entries[cursor] = { hash, state: structuredClone(state) }; },
    go(delta) {
      traversals.push(delta);
      assert.notEqual(delta, 0, 'go(0) reloads instead of traversing');
      events.push(() => {
        const next = cursor + delta;
        if (next < 0 || next >= entries.length) return;
        const oldHash = location.hash;
        cursor = next; emit('popstate');
        if (oldHash !== location.hash) emit('hashchange');
      });
    },
    back() { this.go(-1); }, forward() { this.go(1); },
  };
  const storage = new Map();
  globalThis.sessionStorage = {
    getItem: key => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, value),
  };
  globalThis.location = location;
  globalThis.history = history;
  globalThis.window = {
    // Begin at a nonzero native index, independent of the adapter's indices.
    navigation: nativeNavigation ? { get currentEntry() { return { index: cursor + 7 }; } } : undefined,
    addEventListener(type, fn) { if (!listeners.has(type)) listeners.set(type, new Set()); listeners.get(type).add(fn); },
    removeEventListener(type, fn) { listeners.get(type)?.delete(fn); },
  };
  const raw = Object.assign({}, kontoraSettings(), kontoraDetail(), kontoraAutomations(), {
    currentView: 'board', _navigationSeq: 0, selectedTicket: null, selectedEpic: null,
    sheet: null, dirty: false, savingResult: true, applied: [],
    settingsDirty() { return this.dirty; },
    discardSettings() { this.dirty = false; },
    async saveSettings() { if (this.savingResult) this.dirty = false; return this.savingResult; },
    async gotoView(view, navigation, destination) {
      if (this.currentView === 'settings' && view !== 'settings' && this.dirty) {
        this.settingsGuard = true; this.settingsGuardTarget = destination; return false;
      }
      this.currentView = view; return true;
    },
    closeAgentPicker() {}, closeTerminal() {},
    isSecondary() { return false; }, terminalWanted() { return false; },
    archiveDetailOpen() { return false; },
    async settingsApplyRoute(route, navigation) { this.applied.push({ route, navigation }); },
  });
  const state = reactive ? alpine.window.Alpine.reactive(raw) : raw;
  async function flush() {
    for (let n = 0; n < 30; n++) {
      while (events.length) events.shift()();
      await new Promise(resolve => setImmediate(resolve));
      if (!events.length && !state._settingsHistory?.routing) return;
    }
    throw new Error('history did not settle');
  }
  const go = (section, options = {}) => state.navigateDestination({ view: 'settings', ticketId: null,
    settingsSection: section, settingsQuery: options.query || '', settingsField: options.field || '', replace: !!options.replace });
  return { state, history, location, entries, listeners, flush, go, events, traversals, cursor: () => cursor };
}

test('settings routes default to General, preserve all section keys, and encode only navigation context', () => {
  const { state } = harness();
  assert.deepEqual(state.parseHash('#/settings'), { view: 'settings', ticketId: null,
    settingsSection: 'general', settingsQuery: '', settingsField: '' });
  for (const section of state.settingsSections.map(s => s.key)) {
    state.currentView = 'settings'; state.settingsSection = section;
    state.settingsQuery = 'timeout & café / #'; state.settingsField = '["stages","a/b","timeout"]';
    const hash = state.routeHash();
    assert.ok(hash.startsWith('#/settings/' + section + '?q='));
    const parsed = state.parseHash(hash);
    assert.equal(parsed.settingsSection, section);
    assert.equal(parsed.settingsQuery, state.settingsQuery);
    assert.equal(parsed.settingsField, state.settingsField);
  }
  for (const hash of ['#/settings/no-such-section', '#/settings/%', '#/settings/general/extra']) {
    assert.equal(state.parseHash(hash).settingsSection, 'general', hash);
  }
});

test('sign-in preserves the original encoded Settings deep link before history applies it', () => {
  const hash = '#/settings/stages?q=deploy%20%2F%20release&field=%5B%22stages%22%2C%22a%2Fb%22%5D';
  const h = harness(hash);
  let navigation;
  window.location = { assign: url => { navigation = new URL(url, 'https://kontora.invalid'); } };
  kontoraTickets().submitToken.call({ currentView: 'board', tokenInput: ' fixture+token ' });
  assert.equal(navigation.searchParams.get('token'), 'fixture+token');
  assert.equal(navigation.hash, hash);
  assert.equal(h.state.currentView, 'board');
});

test('bare and invalid settings links canonicalize in place; section routes retain the draft', async () => {
  const h = harness('#/settings'); h.state.setupSettingsHistory();
  await h.state.applyRoute();
  assert.equal(h.location.hash, '#/settings/general'); assert.equal(h.entries.length, 1);
  assert.deepEqual(h.history.state.unrelated, { keep: true });
  h.state.dirty = true;
  await h.go('web', { query: 'bind address', field: 'web.host' });
  assert.equal(h.state.settingsSection, 'web'); assert.equal(h.state.settingsQuery, 'bind address');
  assert.equal(h.state.settingsField, 'web.host'); assert.equal(h.state.dirty, true);
  assert.equal(h.state.settingsGuard, false); assert.equal(h.entries.length, 2);
  await h.go('web', { query: 'host', replace: true });
  assert.equal(h.entries.length, 2); assert.equal(h.state.settingsField, '');
});

for (const direction of ['back', 'forward', 'multi-entry back']) {
  test(`refused ${direction} restores the exact entry without destroying Forward; Keep editing cancels continuation`, async () => {
    const h = harness(); h.state.setupSettingsHistory();
    if (direction === 'multi-entry back') await h.state.navigateDestination({ view: 'stats', ticketId: null });
    await h.go('general'); await h.go('stages');
    if (direction === 'forward') {
      await h.state.navigateDestination({ view: 'stats', ticketId: null });
      h.history.back(); await h.flush();
    }
    const before = h.entries.map(e => structuredClone(e));
    const origin = h.cursor(); const seq = h.state._navigationSeq;
    h.state.dirty = true;
    if (direction === 'forward') h.history.forward();
    else h.history.go(direction === 'back' ? -2 : -3);
    await h.flush();
    assert.equal(h.cursor(), origin); assert.deepEqual(h.entries, before);
    assert.equal(h.state.settingsSection, 'stages'); assert.equal(h.state.settingsGuard, true);
    assert.equal(h.state._navigationSeq, seq + 1, 'paired events and restoration do not start new navigation');
    h.state.settingsGuard = false; h.state.settingsGuardTarget = null;
    h.state.cancelSettingsHistory();
    h.state.dirty = false;
    if (direction === 'forward') h.history.forward(); else h.history.back();
    await h.flush();
    assert.equal(h.cursor(), origin + (direction === 'forward' ? 1 : -1));
    assert.equal(h.entries.length, before.length);
  });
}

for (const action of ['discard', 'save', 'failed save']) {
  test(`${action} resumes the original multi-entry traversal, not a new push`, async () => {
    const h = harness(); h.state.setupSettingsHistory();
    await h.go('general'); await h.go('plannotator'); await h.go('display', { query: 'theme' });
    h.state.dirty = true; const before = structuredClone(h.entries);
    h.history.go(-3); await h.flush();
    assert.equal(h.cursor(), 3); assert.equal(h.state.settingsGuard, true);
    if (action === 'failed save') h.state.savingResult = false;
    const resume = action === 'discard' ? h.state.settingsGuardDiscard() : h.state.settingsGuardSave();
    await h.flush(); await resume;
    assert.equal(h.cursor(), action === 'failed save' ? 3 : 0);
    assert.equal(h.state.currentView, action === 'failed save' ? 'settings' : 'board');
    assert.deepEqual(h.entries, before);
    if (action !== 'failed save') { h.history.forward(); await h.flush(); assert.equal(h.state.settingsSection, 'general'); }
  });
}

test('manual hash edits are stamped, paired traversal events are deduplicated, and teardown is repeatable', async () => {
  const h = harness(); h.state.setupSettingsHistory(); h.state.setupSettingsHistory();
  h.location.hash = '#/settings/notifications?q=retry'; await h.flush();
  assert.equal(h.state.settingsSection, 'notifications'); assert.ok(h.history.state.__kontoraHistory);
  await h.go('agents');
  const seq = h.state._navigationSeq, applied = h.state.applied.length;
  h.history.back(); await h.flush();
  assert.equal(h.state._navigationSeq, seq + 1); assert.equal(h.state.applied.length, applied + 1);
  h.state.destroySettingsHistory(); h.state.destroySettingsHistory();
  assert.equal([...h.listeners.values()].reduce((n, set) => n + set.size, 0), 0);
  const destroyedSeq = h.state._navigationSeq;
  h.history.forward(); await h.flush(); assert.equal(h.state._navigationSeq, destroyedSeq);
});

test('Discard clicked before compensating traversal completes waits, then resumes exactly once', async () => {
  const h = harness(); h.state.setupSettingsHistory();
  await h.go('general'); await h.go('stages'); h.state.dirty = true;
  h.history.go(-2); h.events.shift()();
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.equal(h.state.settingsGuard, true);
  const resume = h.state.settingsGuardDiscard();
  await h.flush(); await resume;
  assert.equal(h.cursor(), 0); assert.equal(h.entries.length, 3);
});

test('rapid refused Back requests restore once and retain the most recent traversal destination', async () => {
  const h = harness(); h.state.setupSettingsHistory();
  await h.state.navigateDestination({ view: 'stats', ticketId: null });
  await h.go('stages'); h.state.dirty = true;
  const before = structuredClone(h.entries);
  h.history.back();
  h.events.shift()(); // first traversal; allow its compensating go to queue
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  h.history.back();
  // Browser input already queued before compensation is processed.
  h.events.unshift(h.events.pop());
  await h.flush();
  assert.equal(h.cursor(), 2); assert.deepEqual(h.entries, before);
  assert.equal(h.state._settingsHistory.motion, null, 'restoration has no phantom traversal waiting for an event');
  assert.equal(h.state.settingsGuardTarget.view, 'board');
  const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
  assert.equal(h.cursor(), 0); assert.deepEqual(h.entries, before);
  h.history.forward(); await h.flush(); assert.equal(h.state.currentView, 'stats');
  h.history.forward(); await h.flush(); assert.equal(h.state.settingsSection, 'stages');
  assert.deepEqual(h.entries, before);
});

test('a late rapid Back refusal does not compensate again into existing Forward entries', async () => {
  const h = harness(); h.state.setupSettingsHistory();
  await h.state.navigateDestination({ view: 'stats', ticketId: null });
  await h.go('stages');
  await h.state.navigateDestination({ view: 'stats', ticketId: null });
  await h.state.navigateDestination({ view: 'archive', ticketId: null });
  h.history.go(-2); await h.flush(); h.state.dirty = true;
  const before = structuredClone(h.entries);
  const gotoView = h.state.gotoView;
  let release;
  h.state.gotoView = async function(view, navigation, destination) {
    const result = await gotoView.call(this, view, navigation, destination);
    if (view === 'board' && result === false) await new Promise(resolve => { release = resolve; });
    return result;
  };
  h.history.back(); h.events.shift()();
  // Wait for the first refusal to queue its compensation.
  for (let n = 0; n < 6; n++) await Promise.resolve();
  assert.equal(h.events.length, 1);
  h.history.back(); h.events.unshift(h.events.pop());
  h.events.shift()();
  for (let n = 0; n < 6; n++) await Promise.resolve();
  assert.equal(typeof release, 'function');
  // Finish both compensation hops while the latest refusal is still awaiting.
  assert.equal(h.events.length, 1); h.events.shift()();
  assert.equal(h.events.length, 1); h.events.shift()();
  assert.equal(h.cursor(), 2);
  release(); await h.flush();
  assert.equal(h.cursor(), 2); assert.deepEqual(h.entries, before);
  assert.equal(h.state.settingsGuardTarget.view, 'board');
  assert.equal(h.state._settingsHistory.motion, null);
  const resume = h.state.settingsGuardSave(); await h.flush(); await resume;
  assert.equal(h.cursor(), 0); assert.deepEqual(h.entries, before);
});

test('rapid refused Forward requests restore once and Keep editing drops the latest continuation', async () => {
  const h = harness('#/settings/stages'); h.state.setupSettingsHistory(); await h.state.applyRoute();
  await h.state.navigateDestination({ view: 'stats', ticketId: null });
  await h.state.navigateDestination({ view: 'board', ticketId: null });
  h.history.go(-2); await h.flush(); h.state.dirty = true;
  const before = structuredClone(h.entries);
  h.history.forward(); h.events.shift()();
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  h.history.forward(); h.events.unshift(h.events.pop());
  await h.flush();
  assert.equal(h.cursor(), 0); assert.deepEqual(h.entries, before);
  assert.equal(h.state.settingsGuardTarget.view, 'board');
  assert.equal(h.state._settingsHistory.motion, null);
  h.state.settingsGuardKeepEditing();
  assert.equal(h.state._settingsHistory.pending, null);
  h.state.dirty = false;
  h.history.forward(); await h.flush();
  assert.equal(h.cursor(), 1); assert.equal(h.state.currentView, 'stats');
  assert.deepEqual(h.entries, before);
});

test('a newer section navigation owns state when an older reveal finishes late', async () => {
  const h = harness(); h.state.setupSettingsHistory();
  await h.go('general'); await h.go('stages');
  let release;
  h.state.settingsApplyRoute = async route => {
    if (route.settingsSection === 'general' && !release) await new Promise(resolve => { release = resolve; });
  };
  h.history.back(); h.events.shift()();
  await Promise.resolve(); await Promise.resolve();
  await h.go('web');
  release(); await h.flush();
  assert.equal(h.state.settingsSection, 'web'); assert.equal(h.location.hash, '#/settings/web');
  const seq = h.state._navigationSeq;
  h.history.back(); await h.flush();
  assert.equal(h.state._navigationSeq, seq + 1);
  assert.equal(h.state.settingsSection, 'general');
});

test('destroy cancels pending route canonicalization and releases traversal continuations', async () => {
  const h = harness(); h.state.setupSettingsHistory(); await h.go('stages');
  let release;
  h.state.settingsApplyRoute = () => new Promise(resolve => { release = resolve; });
  h.location.hash = '#/settings'; h.events.shift()();
  await Promise.resolve(); await Promise.resolve();
  h.state.destroySettingsHistory(); release(); await h.flush();
  assert.equal(h.location.hash, '#/settings');
  assert.equal([...h.listeners.values()].reduce((n, set) => n + set.size, 0), 0);
});

for (const reactive of [false, true]) {
  test(`native hash entries retain their indices and Forward through Keep editing and Discard (Alpine=${reactive})`, async () => {
    const h = harness('#/', { reactive }); h.state.setupSettingsHistory();
    for (const hash of ['#/archive', '#/settings/general']) {
      h.location.hash = hash; await h.flush();
    }
    await h.state.settingsNavigate('environment');
    assert.deepEqual(h.entries.map(e => e.state.__kontoraHistory.index), [0, 1, 2, 3]);
    const before = structuredClone(h.entries);
    h.state.dirty = true;
    h.history.back(); await h.flush();
    h.history.back(); await h.flush();
    assert.equal(h.cursor(), 2, 'refusal traverses back to the actual General entry');
    assert.deepEqual(h.entries, before, 'Archive must not be replaced by a duplicate General entry');
    assert.ok(h.state._settingsHistory.pending, 'the proxied guard retains its continuation');
    h.state.settingsGuardCancel();
    assert.equal(h.state._settingsHistory.pending, null);
    h.history.forward(); await h.flush();
    assert.equal(h.state.settingsSection, 'environment');
    assert.equal(h.state.dirty, true);
    h.history.back(); await h.flush();
    h.history.back(); await h.flush();
    const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
    assert.equal(h.cursor(), 1); assert.equal(h.state.currentView, 'archive');
    h.history.forward(); await h.flush();
    assert.equal(h.state.settingsSection, 'general');
    h.history.forward(); await h.flush();
    assert.equal(h.state.settingsSection, 'environment');
    assert.deepEqual(h.entries, before);
  });
}

for (const nativeNavigation of [false, true]) {
  for (const forwardEntries of [1, 3]) {
    test(`a new hash replacing ${forwardEntries} Forward entries gets its physical index (Navigation API=${nativeNavigation})`, async () => {
      const h = harness('#/', { reactive: true, nativeNavigation }); h.state.setupSettingsHistory();
      h.location.hash = '#/archive'; await h.flush();
      await h.go('general');
      for (const section of ['environment', 'agents', 'stages'].slice(0, forwardEntries)) await h.go(section);
      h.history.go(-forwardEntries); await h.flush();
      const length = h.history.length;
      h.location.hash = '#/settings/web'; await h.flush();
      assert.equal(h.history.length, length - forwardEntries + 1);
      assert.deepEqual(h.entries.map(entry => entry.state.__kontoraHistory.index), [0, 1, 2, 3]);
      const before = structuredClone(h.entries); h.state.dirty = true;
      h.history.go(-2); await h.flush();
      assert.equal(h.cursor(), 3); assert.deepEqual(h.entries, before);
      assert.equal(h.state.settingsSection, 'web'); assert.equal(h.state._settingsHistory.motion, null);
      h.state.settingsGuardKeepEditing();
      assert.equal(h.state.dirty, true);
      h.history.go(-2); await h.flush();
      const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
      assert.equal(h.cursor(), 1); assert.equal(h.state.currentView, 'archive');
      h.history.forward(); await h.flush();
      assert.equal(h.state.settingsSection, 'general');
      h.history.forward(); await h.flush();
      assert.equal(h.state.settingsSection, 'web'); assert.deepEqual(h.entries, before);
      assert.ok(h.traversals.every(delta => delta !== 0));
    });
  }
}

test('native position can restore a refused traversal to an entry from before adapter setup', async () => {
  const h = harness('#/', { nativeNavigation: true });
  h.location.hash = '#/settings/general'; await h.flush();
  h.state.setupSettingsHistory(); await h.state.applyRoute(); h.state.dirty = true;
  h.history.back(); await h.flush();
  assert.equal(h.cursor(), 1); assert.equal(h.state._settingsHistory.motion, null);
  assert.deepEqual(h.entries.map(e => e.hash), ['#/', '#/settings/general']);
  const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
  assert.equal(h.cursor(), 0); assert.equal(h.state.currentView, 'board');
  h.history.forward(); await h.flush();
  assert.equal(h.state.settingsSection, 'general');
});

for (const ownedForward of [false, true]) {
  test(`an unknown non-growing popstate does not invent compensation (owned Forward=${ownedForward})`, async () => {
    const h = harness();
    h.location.hash = '#/settings/general'; await h.flush();
    h.state.setupSettingsHistory(); await h.state.applyRoute();
    if (ownedForward) {
      await h.go('stages'); h.history.back(); await h.flush();
    }
    h.state.dirty = true;
    const traversals = h.traversals.length;
    h.history.back(); await h.flush();
    assert.equal(h.state.settingsGuard, true);
    assert.equal(h.state._settingsHistory.motion, null);
    assert.equal(h.state._settingsHistory.pending.traversable, false);
    assert.equal(h.entries.length, ownedForward ? 3 : 2);
    assert.equal(h.traversals.length, traversals + 1, 'only the requested Back traverses');
  });
}

for (const action of ['discard', 'save']) {
  test(`Alpine-proxied ${action} resumes a managed traversal without pushing`, async () => {
    const h = harness('#/', { reactive: true }); h.state.setupSettingsHistory();
    await h.go('general'); await h.state.settingsNavigate('environment');
    const before = structuredClone(h.entries); h.state.dirty = true;
    h.history.go(-2); await h.flush();
    assert.ok(h.state._settingsHistory.pending, 'plain and proxied destinations identify the same traversal');
    const resume = action === 'discard' ? h.state.settingsGuardDiscard() : h.state.settingsGuardSave();
    await h.flush(); await resume;
    assert.equal(h.cursor(), 0); assert.deepEqual(h.entries, before);
    h.history.forward(); await h.flush();
    assert.equal(h.state.settingsSection, 'general');
    h.history.forward(); await h.flush();
    assert.equal(h.state.settingsSection, 'environment');
    assert.deepEqual(h.entries, before);
  });
}

test('a failed automation read restores the list view as well as its history entry', async () => {
  const h = harness(); h.state.setupSettingsHistory();
  h.state.automations = [{ id: 'auto-a', revision: 1 }];
  h.state.automationsRequest = async () => ({ id: 'auto-a', revision: 1 });
  h.state.automationsLoadHistory = async () => {};
  const list = { view: 'automations', ticketId: null, automationsMode: 'list' };
  const detail = { ...list, automationsMode: 'detail', automationsId: 'auto-a' };
  await h.state.navigateDestination(list);
  await h.state.navigateDestination(detail);
  await h.state.navigateDestination(list);
  const before = structuredClone(h.entries);
  h.state.automationsRequest = async () => { throw new Error('Detail failed'); };
  h.history.back(); await h.flush();
  assert.equal(h.cursor(), 3);
  assert.equal(h.location.hash, '#/automations');
  assert.equal(h.state.routeHash(), h.location.hash);
  assert.equal(h.state.automationsDetail, false);
  assert.equal(h.state.automationsSelected, null);
  assert.equal(h.state.automationsError, 'Detail failed');
  assert.deepEqual(h.entries, before);
  h.state.automationsRequest = async () => ({ id: 'auto-a', revision: 1 });
  h.history.back(); await h.flush();
  assert.equal(h.state.automationsSelected, 'auto-a');
  assert.equal(h.state.automationsDetail, true);
  h.history.back(); await h.flush();
  assert.equal(h.state.automationsDetail, false);
  assert.deepEqual(h.entries, before);
});

for (const tick of [1, 2]) {
  test(`rapid Back restores the accepted section and cancels reveal at tick ${tick}`, async () => {
    const h = harness('#/', { reactive: true }); h.state.setupSettingsHistory();
    const ticks = [], focused = [];
    const field = { id: 'worker-limit', key: ['max_concurrent_agents'], section: 'general' };
    const target = { focus() { focused.push('worker-limit'); }, scrollIntoView() {} };
    globalThis.document = { getElementById: () => target };
    h.state.$nextTick = callback => { ticks.push(callback); };
    h.state.settingsFieldCatalogue = () => [field];
    h.state.settingsApplyRoute = kontoraSettings().settingsApplyRoute;
    const settle = async () => { for (let n = 0; n < 10; n++) await Promise.resolve(); };
    const initial = h.go('general', { field: field.id });
    await settle(); ticks.shift()(); await settle(); ticks.shift()(); await initial;
    await h.go('stages'); h.state.dirty = true;
    const before = structuredClone(h.entries); focused.length = 0;
    h.history.back(); h.events.shift()(); await settle();
    if (tick === 2) { ticks.shift()(); await settle(); }
    assert.equal(ticks.length, 1);
    h.history.back(); h.events.shift()(); await settle();
    await h.flush();
    assert.equal(h.location.hash, '#/settings/stages');
    assert.equal(h.state.settingsSection, 'stages');
    assert.equal(h.state.settingsQuery, ''); assert.equal(h.state.settingsField, '');
    assert.equal(h.state.settingsGuard, true);
    while (ticks.length) { ticks.shift()(); await settle(); }
    await h.flush();
    assert.deepEqual(focused, [], 'an obsolete field must not take focus behind the guard');
    assert.deepEqual(h.entries, before);
    assert.equal(h.state._settingsHistory.motion, null);
    h.state.settingsGuardKeepEditing();
    assert.equal(h.state.settingsSection, 'stages'); assert.equal(h.state.dirty, true);
    h.history.back(); h.events.shift()(); await settle();
    while (ticks.length) { ticks.shift()(); await settle(); }
    await h.flush();
    assert.equal(h.state.settingsSection, 'general');
    assert.deepEqual(focused, ['worker-limit'], 'a subsequent deliberate traversal still reveals its field');
  });
}

for (const reload of [false, true]) for (const forwardEntries of [1, 3]) {
  for (const dirtyFragment of [false, true]) {
    test(`replacement hashes finish guarded navigation (reload=${reload}, Forward=${forwardEntries}, dirty fragment=${dirtyFragment})`, async () => {
      const h = harness(); h.state.setupSettingsHistory();
      await h.go('general');
      for (const section of ['environment', 'agents', 'stages'].slice(0, forwardEntries)) await h.go(section);
      h.history.go(-forwardEntries); await h.flush();
      if (reload) { h.state.destroySettingsHistory(); h.state.setupSettingsHistory(); await h.state.applyRoute(); }
      h.state.dirty = dirtyFragment;
      h.location.hash = dirtyFragment ? '#/archive' : '#/settings/web'; await h.flush();
      if (dirtyFragment && forwardEntries === 1) {
        assert.equal(h.state._settingsHistory.pending.traversable, false);
        assert.equal(h.state._settingsHistory.motion, null);
        assert.equal(h.state.dirty, true);
        const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
        assert.equal(h.location.hash, '#/archive');
        assert.equal(h.state.currentView, 'archive');
        return;
      }
      assert.deepEqual(h.entries.map(entry => entry.state.__kontoraHistory.index), [0, 1, 2]);
      if (dirtyFragment) {
        assert.equal(h.cursor(), 1);
        assert.equal(h.state._settingsHistory.motion, null);
        assert.equal(h.state.settingsSection, 'general');
      } else {
        h.state.dirty = true;
        h.history.go(-2); await h.flush();
        assert.equal(h.cursor(), 2);
        assert.equal(h.state._settingsHistory.motion, null);
        assert.equal(h.state.settingsSection, 'web');
      }
      const before = structuredClone(h.entries);
      const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
      assert.equal(h.cursor(), dirtyFragment ? 2 : 0);
      assert.equal(h.state.currentView, dirtyFragment ? 'archive' : 'board');
      assert.deepEqual(h.entries, before);
    });
  }
}

for (const unavailable of ['read', 'write', 'invalid data']) {
  test(`history storage ${unavailable} does not prevent normal managed traversal`, async () => {
    const h = harness();
    if (unavailable === 'read') globalThis.sessionStorage.getItem = () => { throw new Error('Storage denied'); };
    if (unavailable === 'write') globalThis.sessionStorage.setItem = () => { throw new Error('Storage denied'); };
    if (unavailable === 'invalid data') globalThis.sessionStorage.getItem = () => '{invalid';
    h.state.setupSettingsHistory();
    await h.go('general'); await h.go('web');
    h.state.dirty = true;
    h.history.go(-2); await h.flush();
    assert.equal(h.cursor(), 2); assert.equal(h.state._settingsHistory.motion, null);
    const resume = h.state.settingsGuardDiscard(); await h.flush(); await resume;
    assert.equal(h.cursor(), 0); assert.equal(h.state.currentView, 'board');
  });
}

for (const action of ['section', 'view']) {
  test(`a new ${action} navigation writes its route after Keep editing compensation`, async () => {
    const h = harness(); h.state.setupSettingsHistory();
    await h.go('general'); await h.go('stages'); h.state.dirty = true;
    h.history.go(-2); h.events.shift()();
    for (let n = 0; n < 8; n++) await Promise.resolve();
    assert.ok(h.state._settingsHistory.motion);
    h.state.settingsGuardKeepEditing();
    if (action === 'section') await h.go('web');
    else {
      h.state.dirty = false;
      await h.state.navigateDestination({ view: 'stats', ticketId: null });
    }
    await h.flush();
    assert.equal(h.state._settingsHistory.motion, null);
    assert.equal(h.location.hash, action === 'section' ? '#/settings/web' : '#/stats');
    assert.equal(h.state.routeHash(), h.location.hash);
    assert.equal(h.cursor(), 3);
    h.history.back(); await h.flush();
    assert.equal(h.location.hash, '#/settings/stages');
    assert.equal(h.state.settingsSection, 'stages');
  });
}

function columnHarness(hash = '#/', { reactive = false } = {}) {
  const h = harness(hash, { reactive });
  const { state } = h;
  Object.defineProperties(state, Object.getOwnPropertyDescriptors(kontoraTickets()));
  Object.assign(state, kontoraSources(), {
    isMobile: true, outputSubject: () => null,
    tickets: [], ticketsLastLoaded: 1, ticketSourcesLoaded: true, configCache: {},
    _builtinColumns: ['open', 'human_review', 'done'].map(key => ({ key, statuses: [key] })),
    _knownCustomStatuses: {}, mobileStatusFilter: ':all', _editorSaves: {},
    rememberDetailView() {}, noteResetDrafts() {}, syncDetailCoverage() {}, _resetActivity() {}, _resetTicketCost() {},
  });
  return h;
}

function openPhoneTicket(state, ticket) {
  state.selectedTicket = ticket;
  state.acceptMobileDetailRoute(ticket);
}

function ticketEvents(state) {
  const listeners = new Map();
  const original = globalThis.EventSource;
  globalThis.EventSource = class {
    addEventListener(name, handler) { listeners.set(name, handler); }
  };
  try { state.connectSSE(); } finally { globalThis.EventSource = original; }
  return (name, ticket) => listeners.get(name)({ data: JSON.stringify(ticket) });
}

test('column keys encode as one segment; ticket and Epic routes take precedence', () => {
  const { state } = columnHarness();
  state.mobileColumnKey = 'QA / café?#';
  assert.equal(state.routeHash(), '#/column/QA%20%2F%20caf%C3%A9%3F%23');
  assert.deepEqual(state.parseHash(state.routeHash()), { view: 'board', ticketId: null, columnKey: 'QA / café?#' });
  state.selectedTicket = { id: 'kon-1' };
  assert.equal(state.routeHash(), '#/t/kon-1');
  state.selectedTicket.source = 'team/one';
  assert.equal(state.routeHash(), '#/s/team%2Fone/t/kon-1');
  state.selectedTicket = null;
  state.selectedEpic = { id: 'kon-epic' };
  assert.equal(state.routeHash(), '#/e/kon-epic');
});

for (const hash of ['#/column/%', '#/column/', '#/column/open/extra', '#/column/open?q=filter', '#/column/unknown']) {
  test(`invalid column link ${hash} replaces the current entry with Tickets`, async () => {
    const h = columnHarness(hash);
    h.state.setupSettingsHistory();
    await h.state.applyRoute();
    assert.equal(h.location.hash, '#/');
    assert.equal(h.entries.length, 1);
    assert.equal(h.state.mobileColumnKey, '');
    assert.equal(h.state.mobileColumnRouteError, '');
  });
}

test('desktop keeps a column URL, and on-screen Back from a direct column replaces it', async () => {
  const h = columnHarness('#/column/done');
  h.state.isMobile = false;
  h.state.setupSettingsHistory();
  await h.state.applyRoute();
  assert.equal(h.state.currentView, 'board');
  assert.equal(h.state.mobileColumnKey, 'done');
  assert.equal(h.location.hash, '#/column/done');
  h.state.isMobile = true;
  assert.equal(h.state.mobileColumnKey, 'done');
  await h.state.mobileColumnBack();
  assert.equal(h.location.hash, '#/');
  assert.equal(h.entries.length, 1);
  assert.deepEqual(h.traversals, []);
});

test('Open Select intent is applied only after Settings accepts the destination', async () => {
  const h = columnHarness('#/', { reactive: true });
  const { state } = h;
  const events = [];
  state.mobileResetListInteraction = () => { if (state.sheet) state.closeSheet(); events.push('reset'); };
  state.mobileStartSelection = () => events.push('select');
  state.mobileRestoreList = position => events.push(['restore', position]);
  state.closeSheet = () => { events.push('close'); state.sheet = null; };
  state.setupSettingsHistory();
  await h.go('general');
  events.length = 0;
  state.dirty = true;
  state.sheet = { type: 'filters' };
  assert.equal(await state.openMobileColumn('open', true, 'open-select'), false);
  assert.equal(state.mobileColumnKey, '');
  assert.deepEqual(events, []);
  state.settingsGuardKeepEditing();
  assert.equal(h.location.hash, '#/settings/general');
  assert.equal(await state.openMobileColumn('open', true, 'open-select'), false);
  await state.settingsGuardDiscard();
  assert.equal(h.location.hash, '#/column/open');
  assert.deepEqual(events, ['close', 'reset', ['restore', null], 'select']);
  assert.equal(h.entries.length, 3);
});

test('ticket Back follows its column entry after a status move; Forward preserves the canonical ticket URL', async () => {
  const h = columnHarness('#/', { reactive: true });
  const { state } = h;
  const ticket = { id: 'kon-1', source: 'team/one', status: 'human_review' };
  state.secondaryTickets = [ticket];
  const restored = [];
  let top = 21;
  state.mobileCaptureList = opener => ({ top, opener, filter: 'filter', anchor: 'another-row', offset: -13 });
  state.mobileRestoreList = position => { restored.push(position); if (position) top = position.top; };
  state.closeDetail = () => { state.selectedTicket = null; };
  state.openMobileTicket = row => { state.selectedTicket = row; state.acceptMobileDetailRoute(row); };
  state.setupSettingsHistory();
  await state.openMobileColumn('human_review', false, 'review-tile');
  assert.equal(h.entries[0].state.__kontoraHistory.mobileList.opener, 'review-tile');
  top = 182;
  state.openMobileTicket(ticket);
  const ticketHash = '#/s/team%2Fone/t/kon-1';
  assert.equal(h.location.hash, ticketHash);
  const saved = { top: 182, opener: 'team%2Fone/kon-1', filter: 'filter', anchor: 'another-row', offset: -13 };
  assert.deepEqual(h.entries[1].state.__kontoraHistory.mobileList, saved);

  ticket.status = 'done';
  state.mobileDetailBack(); await h.flush();
  assert.equal(h.location.hash, '#/column/human_review');
  assert.deepEqual(restored.at(-1), saved, 'the mount receives the original source-qualified opener and scroll');
  h.history.forward(); await h.flush();
  assert.equal(h.location.hash, ticketHash);
  assert.equal(state.selectedTicket.status, 'done');
  state.mobileDetailBack(); await h.flush();
  state.mobileColumnBack(); await h.flush();
  assert.equal(h.location.hash, '#/');
  assert.equal(restored.at(-1).opener, 'review-tile');
  assert.equal(top, 21);
  assert.equal(h.entries.length, 3);
});

test('dismissing a phone ticket on desktop returns to its original column entry', async () => {
  const h = columnHarness();
  const { state } = h;
  state.setupSettingsHistory();
  await state.openMobileColumn('human_review');
  openPhoneTicket(state, { id: 'kon-1' });

  state.isMobile = false;
  state.closeDetail();
  await h.flush();
  assert.equal(state.selectedTicket, null);
  assert.equal(h.location.hash, '#/column/human_review');
  assert.equal(h.cursor(), 1);
  assert.equal(h.entries.length, 3);

  state.isMobile = true;
  state.mobileColumnBack(); await h.flush();
  assert.equal(h.location.hash, '#/');
  assert.equal(state.selectedTicket, null);
});

test('column Back never uses a preceding ticket as its parent', async () => {
  const h = columnHarness();
  const { state } = h;
  state.setupSettingsHistory();
  openPhoneTicket(state, { id: 'kon-1' });
  await state.openMobileColumn('done');
  await state.mobileColumnBack(); await h.flush();
  assert.equal(h.location.hash, '#/');
  assert.equal(state.selectedTicket, null);
  assert.deepEqual(h.traversals, []);
});

test('Escape in phone Settings dismisses the retained detail without changing routes', async () => {
  const h = columnHarness();
  const { state } = h;
  state.setupSettingsHistory();
  openPhoneTicket(state, { id: 'kon-1' });
  await h.go('web');
  assert.equal(state.selectedTicket.id, 'kon-1');

  await state.mobileDetailBack(); await h.flush();
  assert.equal(state.selectedTicket, null);
  assert.equal(state.currentView, 'settings');
  assert.equal(h.location.hash, '#/settings/web');
  assert.equal(h.entries.length, 3);
  assert.deepEqual(h.traversals, []);
});

for (const event of ['ticket_updated', 'ticket_deleted']) {
  for (const columnKey of ['', 'human_review']) {
    test(`${event} restores the ${columnKey || 'filtered home'} position when the open phone ticket disappears`, async () => {
      const h = columnHarness();
      const { state } = h;
      const ticket = { id: 'kon-1', status: 'human_review' };
      const restored = [];
      Object.assign(state, {
        tickets: [ticket], createForm: {}, _selectedTicketUpdateSeq: 0,
        refreshInitContext() {}, archiveDropRow() {}, scheduleBoardFlush() {},
        mobileCaptureList: opener => ({ top: 182, opener, filter: 'project:repo', anchor: 'kon-2', offset: -13 }),
        mobileRestoreList: position => restored.push(position),
      });
      const emit = ticketEvents(state);
      state.setupSettingsHistory();
      if (columnKey) await state.openMobileColumn(columnKey);
      const list = h.location.hash;
      restored.length = 0;
      openPhoneTicket(state, ticket);

      emit(event, { ...ticket, status: 'archived' });
      await h.flush();
      assert.equal(state.selectedTicket, null);
      assert.deepEqual(state.tickets, []);
      assert.equal(h.location.hash, list);
      assert.deepEqual(restored, [{ top: 182, opener: 'kon-1', filter: 'project:repo', anchor: 'kon-2', offset: -13 }]);
      assert.deepEqual(h.traversals, [-1]);
    });
  }
}

test('positions belong to history entries, not the column key', async () => {
  const h = columnHarness();
  const { state } = h;
  let top = 10;
  state.mobileCaptureList = () => ({ top });
  state.mobileRestoreList = position => { top = position?.top || 0; };
  state.setupSettingsHistory();
  await state.openMobileColumn('human_review');
  top = 11;
  await state.openMobileColumn('done');
  top = 22;
  await state.openMobileColumn('human_review');
  top = 33;
  h.history.go(-2); await h.flush();
  assert.equal(state.mobileColumnKey, 'human_review');
  assert.equal(top, 11);
  h.history.go(2); await h.flush();
  assert.equal(state.mobileColumnKey, 'human_review');
  assert.equal(top, 33);
});

function columnStartup(hash) {
  const h = columnHarness(hash, { reactive: true });
  const { state } = h;
  const config = { custom_statuses: ['release'] };
  const folder = [{ id: 'kon-1', status: 'external_status' }];
  const response = data => ({ ok: true, json: async () => data });
  const requests = {
    '/api/config': () => response(config),
    '/api/ticket-sources': () => response([{ id: 'team', available: true }]),
    '/api/sources/team/tickets': () => response(folder),
  };
  globalThis.fetch = url => requests[url]();
  globalThis.document = {};
  const noop = () => {};
  Object.assign(state, {
    loading: true, ticketSourcesLoaded: false, ticketsLastLoaded: null, configCache: null,
    initTheme: noop, initMobile: noop, setupSettingsLifecycle: noop,
    _bindGlobalEvents: noop, setupAgentPicker: noop, setupTicketRefresh: noop, applyConfigTheme: noop,
    _mountBoard: noop, automationsRefresh: noop, initAssistant: noop, companionInit: noop,
    $watch(key, callback) {
      const read = () => key.endsWith('()') ? this[key.slice(0, -2)]() : this[key];
      return alpine.window.Alpine.watch(read, callback);
    },
    $nextTick: callback => callback?.(),
    recomputeBoard() { this._deriveColumns(); },
    scheduleBoardFlush() { this.recomputeBoard(); },
    connectSSE() { this._releaseInitialSnapshot(); },
    async fetchTasks() { this.ticketsLastLoaded = 1; },
  });
  return { ...h, config, folder, requests, response, async start() {
    try { await state.init(); } finally { clearInterval(state._nowTimer); }
  } };
}

for (const [key, delayedRead] of [['release', '/api/config'], ['external_status', '/api/sources/team/tickets']]) {
  test(`startup waits for ${delayedRead} before validating ${key}`, async () => {
    const h = columnStartup('#/column/' + key);
    const read = h.requests[delayedRead];
    let release;
    h.requests[delayedRead] = () => new Promise(resolve => { release = () => resolve(read()); });
    const startup = h.start();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(h.state.loading, true);
    assert.equal(h.state.mobileColumnKey, '');
    assert.equal(h.location.hash, '#/column/' + key);
    release();
    await startup;
    assert.equal(h.state.loading, false);
    assert.equal(h.state.mobileColumnKey, key);
    assert.equal(h.state.mobileColumnRouteError, '');
    assert.equal(h.location.hash, '#/column/' + key);
  });
}

for (const [failedRead, recoveredKey] of [['/api/config', 'release'], ['/api/ticket-sources', 'external_status'],
  ['/api/sources/team/tickets', 'external_status'], ['/api/config', '']]) {
  test(`failed ${failedRead} keeps the column URL until Retry ${recoveredKey ? 'confirms it' : 'rules it out'}`, async () => {
    const key = recoveredKey || 'missing';
    // Noncanonical but equivalent escapes must not be rewritten on failure.
    const hash = '#/column/%' + key.charCodeAt(0).toString(16) + key.slice(1);
    const h = columnStartup(hash);
    const read = h.requests[failedRead];
    h.requests[failedRead] = () => ({ ok: false, status: 500 });
    await h.start();
    assert.equal(h.state.mobileColumnKey, key);
    assert.equal(h.state.mobileColumnRouteError, 'Couldn’t verify this list');
    assert.equal(h.location.hash, hash);
    h.requests[failedRead] = read;
    let configReads = 0, ticketReads = 0;
    const readConfig = h.requests['/api/config'];
    h.requests['/api/config'] = () => { ++configReads; return readConfig(); };
    h.state.fetchTasks = async () => { ++ticketReads; };
    await h.state.retryMobileColumnRoute();
    await h.flush();
    assert.equal(configReads, 1);
    assert.equal(ticketReads, 1);
    assert.equal(h.state.mobileColumnRouteError, '');
    assert.equal(h.state.mobileColumnKey, recoveredKey);
    assert.equal(h.location.hash, recoveredKey ? '#/column/' + key : '#/');
    assert.equal(h.entries.length, 1);
  });
}

for (const failedConfig of [false, true]) {
  test(`losing the last inferred status row ${failedConfig ? 'offers list Retry after a failed read' : 'returns to Tickets'}`, async () => {
    const hash = '#/column/external_status';
    const h = columnStartup(hash);
    await h.start();
    assert.equal(h.state.mobileColumnKey, 'external_status');
    h.state.ticketColumnConfigFailed = failedConfig;
    h.state.queueTicketUpdate({ ...h.state.secondaryTickets[0], status: 'done' });
    await h.flush();
    assert.equal(h.location.hash, failedConfig ? hash : '#/');
    assert.equal(h.state.mobileColumnKey, failedConfig ? 'external_status' : '');
    assert.equal(h.state.mobileColumnRouteError, failedConfig ? 'Couldn’t verify this list' : '');
    assert.equal(h.entries.length, 1);
  });
}

test('Retry cannot restore the failed column after a newer navigation', async () => {
  const h = columnStartup('#/column/release');
  h.requests['/api/config'] = () => ({ ok: false, status: 500 });
  await h.start();
  let finish;
  h.requests['/api/config'] = () => new Promise(resolve => { finish = resolve; });
  const retry = h.state.retryMobileColumnRoute();
  await h.state.openMobileColumn('done');
  finish(h.response(h.config));
  await retry;
  assert.equal(h.state.mobileColumnKey, 'done');
  assert.equal(h.location.hash, '#/column/done');
  assert.equal(h.entries.length, 2);
});

test('existing route identities remain compatible', () => {
  const { state } = harness();
  assert.deepEqual(state.parseHash('#/s/team%2Fone/t/kon-1'), { view: 'board', source: 'team/one', ticketId: 'kon-1' });
  assert.equal(state.parseHash('#/t/%').ticketId, '%');
  assert.equal(state.parseHash('#/e/kon-epic').epicId, 'kon-epic');
  assert.equal(state.parseHash('#/archive/t/kon-old').ticketId, 'kon-old');
  assert.equal(state.parseHash('#/automations/a%2Fb/edit?q=timer').automationsId, 'a/b');
});
