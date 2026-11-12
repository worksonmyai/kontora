import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import * as yaml from '../static/vendor/yaml@2.8.1/yaml.mjs';

// Like index_html.test.mjs, exercise the compiled IIFE, not a parallel picker.
const bundlePath = process.env.KONTORA_BUNDLE;
if (!bundlePath) throw new Error('KONTORA_BUNDLE is unset; supply the compiled UI bundle');
const bundle = fs.readFileSync(bundlePath, 'utf8');
const plain = value => JSON.parse(JSON.stringify(value));

function harness() {
  const elements = new Map(), watchers = new Map(), writes = [];
  const context = {
    console, setTimeout, clearTimeout,
    window: { innerWidth: 1280, matchMedia: () => ({ matches: false }), addEventListener() {}, removeEventListener() {} },
    document: { getElementById: id => elements.get(id), addEventListener() {}, removeEventListener() {} },
    localStorage: { getItem: () => null, setItem: (...args) => writes.push(args) },
    navigator: { platform: '' }, getStoredTheme: () => 'dark',
    requestAnimationFrame: () => 1, cancelAnimationFrame() {},
  };
  vm.createContext(context);
  vm.runInContext(bundle, context);
  const state = context.kontora();
  state.$nextTick = cb => cb ? Promise.resolve().then(cb) : Promise.resolve();
  state.$watch = (key, cb) => { watchers.set(key, cb); return () => watchers.delete(key); };
  state.currentView = 'settings';
  state.settingsSection = 'general';
  state.settingsConfig = {
    general: { default_agent: '' },
    assistant: { agent: '', model: 'keep-model', effort: 'keep-effort' },
    agents: Object.assign(Object.create(null), {
      claude: { binary: '/bin/claude', args: '', effort: '' },
      wrapped: { binary: '/bin/nono', args: '--sandbox\n--\n/bin/pi', effort: 'high' },
      codex: { binary: 'codex', args: '', effort: '' },
      ' exact ': { binary: 'pi', args: '', effort: '' },
      'a/b': { binary: 'pi', args: '', effort: '' },
      a_2Fb: { binary: 'pi', args: '', effort: '' },
      constructor: { binary: 'pi', args: '', effort: '' },
      ['__proto__']: { binary: 'claude', args: '', effort: '' },
    }),
  };
  state.configCache = { agents: ['cached'], default_agent: 'cached', agent_infos: [{ name: 'cached', model: 'cache-model' }] };
  state.createForm = { agent: 'keep-create', pipeline: '' };
  state.createTouched = { agent: false };
  state.initForm = { agent: 'keep-init' };
  state.editForm = { agent: 'keep-edit' };
  state.recentAgents = ['cached'];
  state.saveEdit = () => { throw new Error('settings must not save tickets'); };
  return { state, elements, watchers, writes };
}

for (const host of ['settings-default', 'settings-assistant']) {
  test(`${host} uses the exact draft names, never operational metadata or recents`, () => {
    const { state } = harness();
    assert.deepEqual(plain(state.agentPickerAgents(host)).map(a => a.name), Object.keys(state.settingsConfig.agents));
    assert.equal(state.agentPickerHasFilter(host), true);
    state.settingsSection = host === 'settings-default' ? 'general' : 'assistant';
    const rows = state.agentPickerRows(host);
    assert.equal(rows.some(row => row.group === 'recent' || row.value === 'cached'), false);
    assert.equal(rows.find(row => row.value === 'wrapped').meta.includes('cache-model'), false);
    assert.equal(rows.find(row => row.value === 'constructor').disabled, false);
    assert.equal(rows.find(row => row.value === '__proto__').disabled, false);
    state.agentPickerChoose(host, ' exact ', false);
    assert.equal(state.agentPickerValue(host), ' exact ');
  });

  test(`${host} mutates only its reference and preserves blank semantics`, () => {
    const { state, writes } = harness();
    const before = plain(state.settingsConfig);
    state.settingsSection = host === 'settings-default' ? 'general' : 'assistant';
    state.agentPickerChoose(host, 'wrapped', false);
    const expected = structuredClone(before);
    if (host === 'settings-default') expected.general.default_agent = 'wrapped';
    else expected.assistant.agent = 'wrapped';
    assert.deepEqual(plain(state.settingsConfig), expected);
    state.agentPickerChoose(host, '', false);
    assert.deepEqual(plain(state.settingsConfig), before);
    assert.deepEqual(plain(state.configCache.agents), ['cached']);
    assert.deepEqual(plain(state.recentAgents), ['cached']);
    assert.deepEqual(writes, []);
    assert.equal(state.createForm.agent, 'keep-create');
    assert.equal(state.createTouched.agent, false);
    assert.equal(state.editForm.agent, 'keep-edit');
    assert.equal(state.initForm.agent, 'keep-init');
  });

  test(`${host} rejects stale, unknown, non-string, inherited and save-locked choices`, () => {
    const { state } = harness();
    const before = plain(state.settingsConfig);
    state.settingsSection = host === 'settings-default' ? 'general' : 'assistant';
    for (const value of ['cached', 'missing', 'toString', null, 42]) state.agentPickerChoose(host, value, false);
    delete state.settingsConfig.agents.wrapped;
    state.agentPickerChoose(host, 'wrapped', false);
    state.settingsSaving = true;
    state.agentPickerChoose(host, 'claude', false);
    state.agentPickerChoose(host, '', false);
    delete before.agents.wrapped;
    assert.deepEqual(plain(state.settingsConfig), before);
    assert.equal(state.agentPickerDisabled(host), true);
  });
}

test('assistant eligibility reuses wrapper-aware kind detection and updates with the draft', () => {
  const { state } = harness();
  state.settingsSection = 'assistant';
  assert.equal(state.agentPickerOptions('settings-assistant').find(row => row.value === 'wrapped').disabled, false);
  const unsupported = state.agentPickerOptions('settings-assistant').find(row => row.value === 'codex');
  assert.equal(unsupported.disabled, true);
  assert.match(unsupported.reason, /claude.*pi/i);
  state.agentPickerChoose('settings-assistant', 'codex', false);
  assert.equal(state.settingsConfig.assistant.agent, '');
  state.settingsConfig.agents.wrapped.args = '--\ncodex';
  state.agentPickerChoose('settings-assistant', 'wrapped', false);
  assert.equal(state.settingsConfig.assistant.agent, '');
  assert.equal(state.agentPickerOptions('settings-default').find(row => row.value === 'codex').disabled, false);
});

test('selected missing and unsupported values stay visible in native and custom controls with reasons', () => {
  const { state } = harness();
  for (const name of ['missing ', 'codex']) {
    state.settingsConfig.assistant.agent = name;
    assert.equal(state.agentPickerValue('settings-assistant'), name);
    assert.equal(state.agentPickerLabel('settings-assistant'), name);
    const custom = state.agentPickerRows('settings-assistant').find(row => row.value === name);
    const native = state.agentPickerOptions('settings-assistant').find(row => row.value === name);
    assert.equal(custom.disabled, true);
    assert.equal(native.disabled, true);
    assert.equal(custom.selected, true);
    assert.ok(custom.reason);
    assert.equal(custom.reason, native.reason);
    assert.match(state.agentPickerHint('settings-assistant'), /not configured|claude.*pi/i);
    assert.ok(state.agentPickerOptionLabel(native).includes(native.reason));
  }
  state.settingsConfig.general.default_agent = 'removed';
  assert.equal(state.agentPickerRows('settings-default').find(row => row.value === 'removed').disabled, true);
});

test('blank default describes inference without materializing it; assistant blank disables AI turns', () => {
  const { state } = harness();
  assert.match(state.agentPickerHint('settings-default'), /claude/);
  assert.match(state.agentPickerBlankLabel('settings-assistant'), /disabl/i);
  delete state.settingsConfig.agents.claude;
  state.settingsConfig.agents = { only: { binary: 'pi' } };
  assert.match(state.agentPickerHint('settings-default'), /only/);
  state.settingsConfig.agents.second = { binary: 'pi' };
  assert.match(state.agentPickerHint('settings-default'), /explicit/i);
  state.settingsConfig.agents = {};
  assert.match(state.agentPickerHint('settings-default'), /explicit/i);
  assert.equal(state.settingsConfig.general.default_agent, '');
});

test('unknown hosts cannot alias create state or select even configured operational values', async () => {
  const { state } = harness();
  for (const host of ['unknown', '__proto__', 'constructor', 'toString']) {
    state.agentPickerChoose(host, 'cached', false);
    state.agentPickerChoose(host, '', false);
    await state.openAgentPicker(host);
    assert.equal(state.agentPickerIsOpen(host), false);
    assert.notEqual(state.agentPickerState(host), state.agentPickerState('create'));
  }
  assert.equal(state.createForm.agent, 'keep-create');
  assert.equal(state.createTouched.agent, false);
});

test('row IDs are injective for percent/underscore, Unicode, empty and malformed UTF-16 keys', () => {
  const { state } = harness();
  const keys = ['agent:a/b', 'agent:a_2Fb', 'agent:%', 'agent:_25', 'agent:💬', 'agent:\ud800', 'agent:\ud801', 'inherit', ''];
  const ids = keys.flatMap(key => ['settings-default', 'settings-assistant', 'create'].map(host => state.agentPickerRowID(host, { key })));
  assert.equal(new Set(ids).size, ids.length);
  assert.equal(ids.some(id => /\s/.test(id)), false);
});

test('keyboard and hover skip unavailable rows, including an unavailable selected value', async () => {
  const { state } = harness();
  state.settingsSection = 'assistant';
  const host = 'settings-assistant';
  state.settingsConfig.assistant.agent = 'codex';
  const rows = state.agentPickerRows(host);
  const disabled = rows.findIndex(row => row.value === 'codex');
  state.agentPickerState(host).active = disabled;
  assert.notEqual(state.agentPickerActiveIndex(host), disabled);
  for (let n = 0; n < rows.length * 2; n++) {
    state.agentPickerMove(host, 1);
    assert.equal(rows[state.agentPickerActiveIndex(host)].disabled, false);
  }
  const active = state.agentPickerActiveIndex(host);
  state.agentPickerHover(host, rows[disabled].key);
  assert.equal(state.agentPickerActiveIndex(host), active);
  state.agentPickerSetQuery(host, 'codex');
  assert.equal(state.agentPickerActiveIndex(host), -1);
  state.agentPickerPickActive(host);
  assert.equal(state.settingsConfig.assistant.agent, 'codex');
  state.agentPickerMove(host, -1);
  assert.equal(state.agentPickerActiveID(host), null);
  await state.$nextTick();
});

test('native markup shares eligibility, IDs, lock and post-option selection synchronization', async () => {
  const { state } = harness();
  const markup = state.agentPickerControl;
  assert.match(markup, /agentPickerOptions\(pickerHost\)/);
  assert.match(markup, /:disabled="row.disabled"/);
  assert.match(markup, /:aria-disabled="row.disabled"/);
  assert.match(markup, /agentPickerHasFilter\(pickerHost\)/);
  assert.match(markup, /:aria-describedby="agentPickerDescribedBy\(pickerHost\)"/);
  assert.match(markup, /x-text="agentPickerHint\(pickerHost\)"/);
  assert.match(markup, /agentPickerSyncNative\(pickerHost, \$el\)/);
  assert.match(markup, /:disabled="agentPickerDisabled\(pickerHost\)"/);
  let value = 'not-yet-mounted';
  const element = { isConnected: true, get value() { return value; }, set value(v) { value = v; } };
  state.settingsConfig.assistant.agent = ' exact ';
  const syncing = state.agentPickerSyncNative('settings-assistant', element);
  assert.equal(value, 'not-yet-mounted');
  await syncing;
  assert.equal(value, ' exact ');
  state.settingsConfig.assistant.agent = 'wrapped';
  await state.agentPickerSyncNative('settings-assistant', element);
  assert.equal(value, 'wrapped');
});

function controlDiagnostics(state, host) {
  return [...state.agentPickerControl.matchAll(/<(button|select)\s[^>]*>/g)].map(([tag]) => {
    const bindings = {};
    for (const attribute of ['aria-invalid', 'aria-describedby', 'style']) {
      const expression = tag.match(new RegExp(`:${attribute}="([^"]*)"`))?.[1];
      bindings[attribute] = expression === undefined ? undefined
        : Function('state', 'pickerHost', `with (state) { return (${expression}); }`)(state, host);
    }
    return bindings;
  });
}

for (const scenario of [
  { host: 'settings-default', name: 'missing agent', key: ['default_agent'], breakDraft: cfg => { delete cfg.agents.pi; } },
  { host: 'settings-assistant', name: 'missing agent', key: ['assistant', 'agent'], breakDraft: cfg => { delete cfg.agents.pi; } },
  { host: 'settings-assistant', name: 'unsupported agent', key: ['assistant', 'agent'], breakDraft: cfg => { cfg.agents.pi.binary = 'custom-runner'; } },
]) {
  test(`${scenario.host} binds and clears ${scenario.name} diagnostics in both renderers`, async () => {
    const { state } = harness();
    state._settingsYAML = yaml;
    await state._settingsParse('agents:\n  pi: { binary: pi }\ndefault_agent: pi\nassistant: { agent: pi }\n');
    state.settingsSection = scenario.host === 'settings-default' ? 'general' : 'assistant';
    const hintID = state.agentPickerHintID(scenario.host);
    const valid = { 'aria-invalid': null, 'aria-describedby': hintID, style: {} };
    assert.deepEqual(controlDiagnostics(state, scenario.host), [valid, valid]);

    scenario.breakDraft(state.settingsConfig);
    const issue = state.settingsClientIssues().find(issue => JSON.stringify(issue.key) === JSON.stringify(scenario.key));
    assert.equal(issue.blocking, true);
    assert.equal(state.agentPickerIssue(scenario.host), issue.message);
    const invalid = {
      'aria-invalid': true,
      'aria-describedby': hintID + ' ' + state.settingsFieldID(scenario.key, 'issue'),
      style: { border: '2px solid rgb(var(--settings-error))' },
    };
    assert.deepEqual(controlDiagnostics(state, scenario.host), [invalid, invalid]);
    assert.match(state.agentPickerHint(scenario.host), /not configured|claude.*pi/i);

    state.settingsConfig.agents.pi = { binary: 'pi', args: '', effort: '' };
    assert.deepEqual(controlDiagnostics(state, scenario.host), [valid, valid]);
    assert.equal(state.agentPickerIssue(scenario.host), '');
    state.settingsConfig.agents.pi.binary = '';
    assert.ok(state.settingsClientErrors().length > 0);
    assert.deepEqual(controlDiagnostics(state, scenario.host), [valid, valid], 'unrelated binary errors do not invalidate the picker');
  });
}

test('operational picker diagnostics remain absent without reading settings issues', () => {
  const { state } = harness();
  state.settingsFieldIssue = () => { throw new Error('operational picker read settings diagnostics'); };
  const expected = { 'aria-invalid': null, 'aria-describedby': null, style: {} };
  for (const host of ['init', 'create', 'edit', 'mobile-create', 'automations']) {
    assert.deepEqual(controlDiagnostics(state, host), [expected, expected], host);
    assert.equal(state.agentPickerIssue(host), '');
  }
});

test('settings hosts have stable labels and unique IDs and close on navigation/save/hide', () => {
  const { state, watchers, elements } = harness();
  state.setupAgentPicker();
  for (const host of ['settings-default', 'settings-assistant']) {
    assert.equal(state.agentPickerTriggerID(host), `${host}-agent`);
    assert.equal(state.agentPickerLabelID(host), `${host}-agent-label`);
    assert.ok(state.agentPickerHostLabel(host).includes(host === 'settings-default' ? 'default' : 'assistant'));
    for (const [key, value] of [['settingsSection', 'agents'], ['settingsSaving', true], ['currentView', 'board']]) {
      state._agentPickerOpenHost = host;
      state.agentPickerState(host).open = true;
      watchers.get(key)(value);
      assert.equal(state.agentPickerIsOpen(host), false, key);
    }
    let focused = 0;
    elements.set(state.agentPickerTriggerID(host), { focus() { focused++; } });
    state._agentPickerOpenHost = host;
    state.agentPickerState(host).open = true;
    state.closeAgentPicker(true);
    assert.equal(focused, 1);
  }
  state.destroyAgentPicker();
  assert.equal(watchers.size, 0);
});

test('settings presentation never reads operational cache, activity or recents', () => {
  const { state } = harness();
  const forbidden = () => { throw new Error('operational data read by a settings host'); };
  Object.defineProperty(state, 'configCache', { get: forbidden });
  Object.defineProperty(state, 'recentAgents', { get: forbidden });
  state.agentRunningCount = forbidden;
  for (const host of ['settings-default', 'settings-assistant']) {
    state.settingsSection = host === 'settings-default' ? 'general' : 'assistant';
    state.agentPickerAgents(host);
    state.agentPickerGroups(host);
    state.agentPickerOptions(host);
    state.agentPickerHint(host);
    state.agentPickerHasFilter(host);
    assert.equal(state.agentPickerRunningTotal(host), 0);
    assert.equal(state.agentPickerDotStyle('claude', host), 'display:none');
    state.agentPickerChoose(host, 'wrapped', false);
  }
});

test('flat default-agent drafts update only the flat reference', () => {
  const { state } = harness();
  state.settingsConfig.default_agent = '';
  state.agentPickerChoose('settings-default', ' exact ', false);
  assert.equal(state.settingsConfig.default_agent, ' exact ');
  assert.equal(state.settingsConfig.general.default_agent, '');
  assert.equal(state.agentPickerValue('settings-default'), ' exact ');
});

test('stale settings hosts reject selections immediately, before navigation watchers run', () => {
  const { state } = harness();
  state.settingsSection = 'agents';
  state.agentPickerChoose('settings-default', 'claude', false);
  assert.equal(state.settingsConfig.general.default_agent, '');
  state.settingsSection = 'assistant';
  state.currentView = 'board';
  state.agentPickerChoose('settings-assistant', 'wrapped', false);
  assert.equal(state.settingsConfig.assistant.agent, '');
});

test('native synchronization waits for asynchronous options and uses the latest exact selection', async () => {
  const { state } = harness();
  let complete;
  state.$nextTick = () => new Promise(resolve => { complete = resolve; });
  const options = new Set(['']);
  let selected = '';
  const element = {
    isConnected: true,
    set value(value) { selected = options.has(value) ? value : ''; },
  };
  state.settingsConfig.assistant.agent = 'claude';
  const syncing = state.agentPickerSyncNative('settings-assistant', element);
  state.settingsConfig.assistant.agent = ' exact ';
  options.add(' exact ');
  complete();
  await syncing;
  assert.equal(selected, ' exact ');
});

test('settings saving blocks opening and cancels an opening suspended at nextTick', async () => {
  const { state } = harness();
  state._agentPickerPopoverSupported = true;
  state.settingsSaving = true;
  await state.openAgentPicker('settings-default');
  assert.equal(state._agentPickerOpenHost, null);
  state.settingsSaving = false;
  let complete;
  state.$nextTick = () => new Promise(resolve => { complete = resolve; });
  const opening = state.openAgentPicker('settings-default');
  assert.equal(state._agentPickerOpenHost, 'settings-default');
  state.settingsSaving = true;
  complete();
  await opening;
  assert.equal(state._agentPickerOpenHost, null);
  assert.equal(state.settingsConfig.general.default_agent, '');
});

test('operational stale values and disabled automation hosts are centrally rejected', () => {
  const { state, writes } = harness();
  state.agentPickerChoose('create', 'removed', false);
  assert.equal(state.createForm.agent, 'keep-create');
  assert.equal(state.createTouched.agent, false);
  state.automationsDraft = { agent: '' };
  state.automationsDraftChanged = () => { throw new Error('disabled automation mutation'); };
  for (const key of ['automationsPending', 'automationsCreate']) {
    state[key] = true;
    state.agentPickerChoose('automations', 'cached', false);
    state.agentPickerChoose('automations', '', false);
    state[key] = false;
  }
  assert.equal(state.automationsDraft.agent, '');
  assert.deepEqual(writes, []);
});

test('operational adapters retain cache metadata and their mutation side effects', () => {
  const { state, writes } = harness();
  let saved = 0, automationChanges = 0;
  state.saveEdit = () => saved++;
  state.automationsDraft = { agent: '' };
  state.automationsDraftChanged = () => automationChanges++;
  for (const host of ['create', 'mobile-create', 'init', 'edit', 'automations']) {
    state.agentPickerChoose(host, 'cached', false);
    assert.equal(state.agentPickerValue(host), 'cached');
    assert.equal(state.agentPickerAgents(host)[0].model, 'cache-model');
  }
  assert.equal(saved, 1);
  assert.equal(automationChanges, 1);
  assert.equal(state.createTouched.agent, true);
  assert.equal(state._initAgentFollowsProject, false);
  assert.equal(state._editAgentFollowsProject, false);
  assert.ok(writes.length > 0);
});
