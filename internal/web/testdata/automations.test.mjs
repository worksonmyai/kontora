import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { kontoraAutomations } from '../ui/automations.js';
import { kontoraDetail } from '../ui/detail.js';
import { deferred } from './settings_fixtures.mjs';

function previewState() {
  const state = kontoraAutomations();
  Object.assign(state, { automationsAvailable: true, automationsEditing: true,
    automationsNotify() {}, automationsSelect: async () => false });
  Object.assign(state.automationsDraft, { name: 'Report', path: '/repo', agent: 'coder', prompt: 'Report changes' });
  return state;
}

for (const [schedule, valid] of [['1.5h', true], ['0.5h', true], ['1.5h30m', true], ['1.0m', true],
  ['0 9 * * 1-5', true], ['0 9 * * MON', false], ['1.5m', false], ['nonsense', false], ['60 * * * *', false]]) {
  test(`authoritative preview decides nonempty schedule ${schedule}`, async t => {
    t.mock.timers.enable({ apis: ['setTimeout'] });
    const state = previewState(), response = deferred();
    t.after(() => state.automationsCancelPreview());
    state.automationsDraft.schedule = schedule;
    const requests = [];
    state.automationsRequest = async (path, options) => {
      requests.push({ path, body: JSON.parse(options.body) });
      return response.promise;
    };
    state.automationsSchedulePreview();
    assert.equal(state.automationsPreviewError, '', 'only the server interprets nonempty schedules');
    assert.equal(Boolean(state.automationsCanSave()), false);
    await state.automationsSaveDraft();
    assert.equal(requests.length, 0);
    t.mock.timers.tick(250);
    assert.deepEqual(requests, [{ path: '/preview', body: { schedule, timezone: 'UTC' } }]);
    assert.equal(Boolean(state.automationsCanSave()), false, 'pending preview cannot authorize a save');
    if (valid) response.resolve({ occurrences: ['2030-01-01T00:00:00Z'], human: 'authoritative' });
    else response.resolve(Promise.reject(new Error('server rejected schedule')));
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(Boolean(state.automationsCanSave()), valid);
    assert.equal(state.automationsPreviewError, valid ? '' : 'server rejected schedule');
    if (valid) {
      assert.equal(state.automationsPreviewHuman, 'authoritative');
      state.automationsDraft.timezone = 'Europe/Berlin';
      assert.equal(Boolean(state.automationsCanSave()), false, 'timezone must match the successful preview');
      state.automationsDraft.timezone = 'UTC';
      state.automationsDraft.schedule += ' ';
      assert.equal(Boolean(state.automationsCanSave()), false, 'schedule must match the successful preview');
    }
  });
}

for (const schedule of ['', '   ']) {
  test(`empty schedule ${JSON.stringify(schedule)} is required without preview`, t => {
    t.mock.timers.enable({ apis: ['setTimeout'] });
    const state = previewState();
    state.automationsDraft.schedule = schedule;
    state.automationsRequest = () => assert.fail('empty schedule must not request preview');
    state.automationsSchedulePreview();
    t.mock.timers.tick(250);
    assert.equal(state.automationsPreviewError, 'A schedule is required.');
    assert.equal(Boolean(state.automationsCanSave()), false);
  });
}

test('obsolete preview success cannot authorize a newer draft or timezone', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const state = previewState(), old = deferred(), current = deferred();
  t.after(() => state.automationsCancelPreview());
  let requests = 0;
  state.automationsRequest = () => ++requests === 1 ? old.promise : current.promise;
  state.automationsSchedulePreview();
  t.mock.timers.tick(250);
  state.automationsDraft.timezone = 'Europe/Berlin';
  state.automationsSchedulePreview();
  t.mock.timers.tick(250);
  current.resolve(Promise.reject(new Error('timezone refused')));
  await new Promise(resolve => setImmediate(resolve));
  old.resolve({ occurrences: [], human: 'obsolete' });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(state.automationsPreviewError, 'timezone refused');
  assert.equal(state.automationsPreviewValidKey, '');
  assert.equal(Boolean(state.automationsCanSave()), false);
});

for (const kind of ['new', 'update']) {
  test(`${kind} save requires matching preview and sends the previewed definition`, async () => {
    const state = previewState(), requests = [];
    if (kind === 'update') Object.assign(state.automationsDraft, { id: 'auto-a', revision: 4 });
    state.automationsDraft.schedule = '1.5h';
    state.automationsRequest = async (path, options) => {
      requests.push({ path, method: options.method, body: JSON.parse(options.body) });
      return { id: 'auto-a', revision: 5, ...JSON.parse(options.body).definition };
    };
    await state.automationsSaveDraft();
    assert.equal(requests.length, 0);
    state.automationsPreviewValidKey = JSON.stringify({ schedule: '1.5h', timezone: 'UTC' });
    await state.automationsSaveDraft();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].method, kind === 'new' ? 'POST' : 'PUT');
    assert.equal(requests[0].body.definition.schedule, '1.5h');
    assert.equal(requests[0].body.definition.timezone, 'UTC');
  });
}

test('uncertain creation retries captured receipt even after draft changes without a new preview', async () => {
  const state = previewState(), requests = [];
  state.automationsDraft.schedule = '1.5h';
  state.automationsPreviewValidKey = JSON.stringify({ schedule: '1.5h', timezone: 'UTC' });
  state.automationsRequest = async (path, options) => {
    requests.push({ path, body: JSON.parse(options.body) });
    if (requests.length === 1) throw new Error('acknowledgement lost');
    return { id: 'created', revision: 1, ...requests[0].body.definition };
  };
  await state.automationsSaveDraft();
  assert.ok(state.automationsCreate);
  state.automationsDraft.schedule = 'not validated';
  state.automationsDraft.prompt = 'new unsaved text';
  state.automationsPreviewValidKey = '';
  assert.equal(Boolean(state.automationsCanSave()), true, 'captured creation is reconciliation, not a new draft save');
  await state.automationsSaveDraft();
  assert.equal(requests.length, 2);
  assert.deepEqual(requests[1], requests[0]);
  assert.equal(requests[1].body.definition.schedule, '1.5h');
  assert.equal(state.automationsCreate, null);
});

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

test('Duplicate opens an unsaved paused definition without copying runtime identity', () => {
  const h = harness();
  const source = {
    id: 'auto-original', key: 'original', revision: 12, name: 'Original', enabled: true,
    prompt: 'Inspect {{ .Run.Number }}', path: '/repo', agent: 'coder', model: 'model-a',
    timeout: '30m', schedule: '0 9 * * 1-5', timezone: 'Europe/Berlin',
    workspace: 'worktree', overlap: 'queue', running: true, runs_total: 8,
    current_run: { n: 9 }, spend_30d: 4.5,
  };
  const saved = structuredClone(source);
  let requests = 0;
  h.state.automationsRequest = () => { ++requests; throw new Error('Duplicate must not mutate'); };
  assert.equal(h.state.automationsNew(source), true);
  assert.equal(h.state.automationsSelected, null);
  assert.equal(h.state.automationsEditing, true);
  assert.equal(h.state.automationsDetail, false);
  assert.equal(h.state.automationsDraft.name, 'Original (copy)');
  assert.equal(h.state.automationsDraft.enabled, false);
  for (const key of ['prompt', 'path', 'agent', 'model', 'timeout', 'schedule', 'timezone', 'workspace', 'overlap']) {
    assert.equal(h.state.automationsDraft[key], source[key], key);
  }
  for (const key of ['id', 'key', 'revision', 'running', 'runs_total', 'current_run', 'spend_30d']) {
    assert.equal(h.state.automationsDraft[key], undefined, key);
  }
  assert.equal(h.state.automationsCreate, null, 'no creation receipt before explicit save');
  assert.equal(requests, 0);
  assert.equal(h.location.hash, '#/automations/new');
  assert.deepEqual(source, saved);
});

test('automation rail retains configuration, timezone, copy values and cost coverage', () => {
  const h = harness();
  h.state.configCache = { config_path: '/config/kontora.yaml', logs_dir: '/logs/' };
  const row = { id: 'auto-a', key: 'a', path: '/repo', timezone: 'Europe/Berlin', created_at: '2026-01-01T00:00:00Z' };
  const details = Object.fromEntries(h.state.automationsRailDetails(row));
  assert.equal(details.config, '/config/kontora.yaml');
  assert.equal(details.timezone, 'Europe/Berlin');
  assert.equal(details.logs, '/logs/automations/a/');
  assert.equal(details.id, 'auto-a');
  const html = readFileSync(new URL('../static/index.html', import.meta.url), 'utf8');
  const css = readFileSync(new URL('../static/automations.css', import.meta.url), 'utf8');
  const rail = html.slice(html.indexOf('<details open class="automations-rail'), html.indexOf('</details>', html.indexOf('<details open class="automations-rail')));
  assert.match(rail, /<summary[^>]*>Schedule and configuration<\/summary>/);
  assert.match(rail, /next_runs/);
  assert.match(rail, /automationsDateLabel\(fire, automationsRow\(\).timezone\)/);
  assert.match(rail, /copyCmd\(pair\[1\]\)/);
  assert.match(rail, /spend_30d/);
  assert.match(rail, /priced_runs/);
  assert.match(rail, /tracked_runs/);
  assert.doesNotMatch(css, /automations-rail\s*\{[^}]*display:\s*none/);
  assert.doesNotMatch(css, /automations-log-line[^}]*nth-child\(2\)[^}]*display:\s*none/);
  assert.match(css, /@container automations \(max-width: 1100px\)/);
  assert.match(css, /automations-detail-body[^}]*flex-direction: column/);
  assert.match(css, /automations-table[^}]*last-child[^}]*position: sticky; right: 0/);
  const mobile = html.match(/class="automations-mobile-actions[\s\S]*?class="automations-detail-title/)[0];
  assert.match(mobile, /role="menuitem" tabindex="-1" @click="automationsNew\(automationsRow\(\)\)"[^>]*>Duplicate/);
  assert.match(mobile, /x-show="automationsCanStop\(\)" @click="automationsStop\(\)"/);
  assert.match(html, /x-text="line.source"/);
});

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
