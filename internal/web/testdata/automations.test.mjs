import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraAutomations } from '../ui/automations.js';
import { kontoraDetail } from '../ui/detail.js';
import { deferred } from './settings_fixtures.mjs';

const rows = [{ id: 'auto-a', revision: 1, name: 'A' }, { id: 'auto-b', revision: 1, name: 'B' }];
const actions = [
  { name: 'Edit', hash: '#/automations/auto-a/edit', run: (state, writeRoute) => state.automationsEdit(rows[0], writeRoute) },
  { name: 'New', hash: '#/automations/new', run: (state, writeRoute) => state.automationsNew(null, writeRoute) },
  { name: 'Select', hash: '#/automations/auto-b', run: (state, writeRoute) => state.automationsSelect(rows[1], writeRoute) },
  { name: 'Back', hash: '#/automations', run: (state, writeRoute) => state.automationsBack(writeRoute, true) },
];

function harness() {
  const listeners = new Map(), events = [], traversals = [], prompts = [];
  const entries = [{ hash: '#/automations', state: null }];
  let cursor = 0;
  const location = { get hash() { return entries[cursor].hash; } };
  const emit = type => { for (const listener of listeners.get(type) || []) listener({ type }); };
  const history = {
    get state() { return entries[cursor].state; },
    get length() { return entries.length; },
    pushState(state, _, hash) { entries.splice(++cursor); entries.push({ hash, state: structuredClone(state) }); },
    replaceState(state, _, hash) { entries[cursor] = { hash, state: structuredClone(state) }; },
    go(delta) {
      assert.notEqual(delta, 0);
      traversals.push(delta);
      events.push(() => {
        const next = cursor + delta;
        if (next < 0 || next >= entries.length) return;
        cursor = next;
        emit('popstate');
        emit('hashchange');
      });
    },
  };
  globalThis.location = location;
  globalThis.history = history;
  globalThis.window = {
    confirm(message) { prompts.push(message); return true; },
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(listener);
    },
    removeEventListener(type, listener) { listeners.get(type)?.delete(listener); },
  };
  const state = Object.assign({}, kontoraDetail(), kontoraAutomations(), {
    currentView: 'automations', _navigationSeq: 0, selectedTicket: null, selectedEpic: null,
    automations: rows.map(row => ({ ...row })),
    closeAgentPicker() {}, closeTerminal() {}, automationsDraftChanged() {},
    $nextTick: callback => callback(), $refs: {},
    automationsRequest: async path => ({ ...rows.find(row => path === '/' + row.id) }),
    automationsLoadHistory: async () => {},
  });
  async function flush() {
    for (let n = 0; n < 30; n++) {
      while (events.length) events.shift()();
      await new Promise(resolve => setImmediate(resolve));
      if (!events.length && state._settingsHistory?.routing == null) return;
    }
    throw new Error('automation navigation did not settle');
  }
  state.setupSettingsHistory();
  return { state, history, location, entries, events, traversals, prompts, flush, cursor: () => cursor };
}

for (const action of actions) {
  for (const mode of ['direct', 'route-driven', 'discard refused', 'pending write']) {
    test(`${action.name}: ${mode} navigation ownership`, async () => {
      const h = harness();
      if (mode === 'discard refused') {
        h.state.automationsEditing = true;
        h.state.automationsDraft.name = 'Unsaved';
        window.confirm = () => false;
      }
      if (mode === 'pending write') h.state.automationsPending = true;
      const navigation = h.state._navigationSeq;
      const editor = h.state.automationsEditorSeq;
      const refused = mode === 'discard refused' || mode === 'pending write';
      assert.equal(await action.run(h.state, mode !== 'route-driven'), !refused);
      assert.equal(h.state._navigationSeq, navigation + (mode === 'direct' ? 1 : 0));
      if (refused) {
        assert.equal(h.state.automationsEditorSeq, editor);
        assert.equal(h.entries.length, 1);
      }
    });
  }

  for (const phase of ['detail', 'history']) {
    for (const outcome of ['success', 'failure']) {
      test(`browser Back ${phase} ${outcome} cannot undo newer ${action.name}`, async () => {
        const h = harness();
        await h.state.automationsSelect(rows[0]);
        await h.state.automationsBack(true, true);
        const gate = deferred(), started = deferred();
        const wait = async () => {
          started.resolve();
          await gate.promise;
          if (outcome === 'failure') throw new Error('Obsolete read failed');
        };
        const request = h.state.automationsRequest;
        h.state.automationsRequest = async path => {
          if (phase === 'detail' && path === '/auto-a') await wait();
          return request(path);
        };
        h.state.automationsLoadHistory = async () => {
          if (phase === 'history' && h.state.automationsSelected === 'auto-a') await wait();
        };
        h.history.go(-1);
        h.events.shift()();
        await started.promise;
        await action.run(h.state, true);
        if (h.state.automationsEditing) h.state.automationsDraft.prompt = 'Keep this newer draft';
        const before = structuredClone(h.entries);
        const cursor = h.cursor();
        gate.resolve();
        await h.flush();
        assert.equal(h.state.routeHash(), action.hash);
        assert.equal(h.location.hash, action.hash);
        assert.deepEqual(h.prompts, [], 'an obsolete read must not ask to discard a newer draft');
        if (h.state.automationsEditing) assert.equal(h.state.automationsDraft.prompt, 'Keep this newer draft');
        assert.equal(h.state.automationsError, '');
        assert.deepEqual(h.traversals, [-1], 'superseded reads must not compensate browser history');
        assert.deepEqual(h.entries, before);
        assert.equal(h.cursor(), cursor);
        assert.equal(h.state._settingsHistory.current.hash, action.hash);
      });
    }
  }
}

for (const writeRoute of [true, false]) {
  test(`Back from Edit retains its navigation owner (writeRoute=${writeRoute})`, async () => {
    const h = harness();
    h.state.automationsEdit(rows[0], false);
    const navigation = h.state._navigationSeq;
    assert.equal(await h.state.automationsBack(writeRoute), true);
    assert.equal(h.state._navigationSeq, navigation + (writeRoute ? 1 : 0));
    assert.equal(h.state.routeHash(), '#/automations/auto-a');
    assert.equal(h.state.automationsEditing, false);
  });
}

test('Edit without a row does not take navigation ownership', () => {
  const h = harness();
  const navigation = h.state._navigationSeq;
  assert.equal(h.state.automationsEdit(), false);
  assert.equal(h.state._navigationSeq, navigation);
});

test('a current failed detail read still restores the accepted list and preserves Forward', async () => {
  const h = harness();
  await h.state.automationsSelect(rows[0]);
  await h.state.automationsBack(true, true);
  const before = structuredClone(h.entries);
  h.state.automationsRequest = async () => { throw new Error('Detail failed'); };
  h.history.go(-1);
  await h.flush();
  assert.equal(h.location.hash, '#/automations');
  assert.equal(h.state.routeHash(), h.location.hash);
  assert.equal(h.state.automationsError, 'Detail failed');
  assert.equal(h.state.automationsSelected, null);
  assert.equal(h.cursor(), 2);
  assert.deepEqual(h.traversals, [-1, 1]);
  assert.deepEqual(h.entries, before);
});
