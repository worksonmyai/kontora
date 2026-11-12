import assert from 'node:assert/strict';
import test from 'node:test';
import { deferred, settingsFixture, settingsState } from './settings_fixtures.mjs';

for (const boundary of ['stale', 'yaml', 'put', 'rebase', 'cache']) {
  test(`save locks mutations throughout ${boundary}`, async () => {
    const wait = deferred();
    let entered;
    const ready = new Promise(resolve => { entered = resolve; });
    let puts = 0;
    const state = await settingsState(settingsFixture, { fetch: async (_url, options) => {
      if (!options) return { ok: true, json: async () => ({ content: settingsFixture }) };
      puts++;
      if (boundary === 'put') { entered(); await wait.promise; }
      return { status: 204 };
    } });
    state.settingsConfig.general.branch_prefix = 'submitted';
    const method = { stale: '_settingsStale', yaml: '_settingsWrite', rebase: '_settingsRebase', cache: '_settingsRefreshConfigCache' }[boundary];
    if (method) {
      const original = state[method].bind(state);
      state[method] = async (...args) => { entered(); await wait.promise; return original(...args); };
    }
    const save = state.saveSettings();
    await ready;
    assert.equal(state.settingsSaving, true);
    assert.equal(await state.saveSettings(), false);
    state.discardSettings();
    state.settingsNewEnvKey = 'LOCKED';
    state.settingsAddEnv();
    assert.equal(state.settingsConfig.general.branch_prefix, 'submitted');
    assert.equal(Object.hasOwn(state.settingsConfig.environment, 'LOCKED'), false);
    wait.resolve();
    assert.equal(await save, true);
    assert.equal(puts, 1);
    assert.equal(state.settingsSaving, false);
  });
}

test('submission snapshots determine YAML, expected content and restart metadata', async () => {
  const wait = deferred();
  const state = await settingsState(settingsFixture, { fetch: async (_url, options) => {
    if (!options) { await wait.promise; return { ok: true, json: async () => ({ content: settingsFixture }) }; }
    const sent = JSON.parse(options.body);
    assert.equal(sent.expected_content, settingsFixture);
    assert.match(sent.content, /branch_prefix: submitted/);
    assert.doesNotMatch(sent.content, /late/);
    return { status: 204 };
  } });
  state.settingsConfig.general.branch_prefix = 'submitted';
  const save = state.saveSettings();
  state.settingsConfig.general.branch_prefix = 'late';
  state.settingsConfig.web.host = 'late';
  wait.resolve();
  assert.equal(await save, true);
  assert.equal(state.settingsSavedRestart, false);
});

test('beforeunload listener protects pending saves and teardown invalidates reads', async () => {
  const listeners = new Map();
  const state = await settingsState(settingsFixture, { window: {
    addEventListener: (name, fn) => listeners.set(name, fn),
    removeEventListener: name => listeners.delete(name),
  } });
  state.setupSettingsLifecycle();
  state.setupSettingsLifecycle();
  let prevented = false;
  state.settingsSaving = true;
  listeners.get('beforeunload')({ preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  state.settingsGuard = true;
  state.settingsGuardKeepEditing();
  assert.equal(state.settingsGuard, true);
  const sequence = state._settingsRefreshSeq;
  state.destroySettingsLifecycle();
  assert.equal(listeners.size, 0);
  assert.ok(state._settingsRefreshSeq > sequence);
});

test('unfinished drafts protect leaving and block a write until added or cancelled', async () => {
  let requests = 0;
  const state = await settingsState(settingsFixture, { fetch: async () => { requests++; } });
  state.settingsNewStage = 'unfinished';
  assert.equal(state.settingsNeedsProtection(), true);
  assert.equal(await state.saveSettings(), false);
  assert.equal(requests, 0);
  state.discardSettings();
  assert.equal(state._settingsHasDrafts(), false);
});

for (const outcome of ['partial', 'uncertain', 'success']) {
  test(`save distinguishes ${outcome} confirmation`, async () => {
    let puts = 0;
    const state = await settingsState(settingsFixture, { fetch: async (_url, options) => {
      if (!options) return { ok: true, json: async () => ({ content: settingsFixture }) };
      puts++;
      if (outcome === 'uncertain') throw new Error('connection lost');
      return outcome === 'success' ? { status: 204 } : { status: 500, json: async () => ({ code: 'config_saved_reload_failed', error: 'config saved but reload failed: conflict' }) };
    } });
    state.settingsConfig.general.branch_prefix = 'new';
    assert.equal(await state.saveSettings(), outcome === 'success');
    assert.equal(state.settingsDirty(), outcome === 'uncertain');
    assert.equal(puts, 1);
    if (outcome === 'partial') assert.match(state.settingsReloadWarning, /reload failed/);
    if (outcome === 'uncertain') assert.match(state.settingsErrors.join(' '), /confirm/i);
  });
}

test('explicit retry applies only the persisted baseline and remains conditional', async () => {
  let raw = settingsFixture, puts = 0;
  const state = await settingsState(raw, { fetch: async (_url, options) => {
    if (!options) return { ok: true, json: async () => ({ content: raw }) };
    const body = JSON.parse(options.body);
    assert.equal(body.expected_content, raw);
    raw = body.content;
    puts++;
    return puts === 1 ? { status: 500, json: async () => ({ code: 'config_saved_reload_failed', error: 'saved but reload failed' }) } : { status: 204 };
  } });
  state.settingsConfig.general.branch_prefix = 'persisted';
  assert.equal(await state.saveSettings(), false);
  state.settingsConfig.general.branch_prefix = 'new draft';
  assert.equal(await state.settingsRetryApply(), false);
  assert.equal(puts, 1);
  state.discardSettings();
  assert.match(state.settingsReloadWarning, /reload failed/);
  assert.equal(await state.settingsRetryApply(), true);
  assert.equal(puts, 2);
  assert.equal(state.settingsReloadWarning, '');
  assert.equal(state.settingsConfig.general.branch_prefix, 'persisted');
});

test('partial Save & Leave cannot repeat the write without explicit retry', async () => {
  let raw = settingsFixture, puts = 0, navigations = 0;
  const state = await settingsState(raw, { fetch: async (_url, options) => {
    if (!options) return { ok: true, json: async () => ({ content: raw }) };
    raw = JSON.parse(options.body).content;
    puts++;
    return puts === 1 ? { status: 500, json: async () => ({ code: 'config_saved_reload_failed', error: 'saved but reload failed' }) } : { status: 204 };
  } });
  state._resumeSettingsDestination = () => { navigations++; };
  state.settingsGuard = true;
  state.settingsGuardTarget = { view: 'archive' };
  state.settingsConfig.general.branch_prefix = 'persisted';
  await state.settingsGuardSave();
  assert.equal(state.settingsGuard, true);
  assert.equal(state.settingsDirty(), false);
  assert.equal(puts, 1);
  await state.settingsGuardSave();
  assert.equal(puts, 1);
  assert.equal(navigations, 0);
  assert.equal(await state.settingsRetryApply(), true);
  assert.equal(puts, 2);
  assert.equal(state.settingsReloadWarning, '');
});

test('exact integers survive load, changed-node write and rebase', async () => {
  const text = '# keep\nmax_concurrent_agents: 9007199254740993\nextra: &n 9007199254740995\nother: *n\n';
  const state = await settingsState(text);
  assert.equal(state.settingsConfig.general.max_concurrent_agents, '9007199254740993');
  state.settingsConfig.general.max_concurrent_agents = '9007199254740997';
  const written = await state._settingsWrite();
  assert.match(written, /max_concurrent_agents: 9007199254740997/);
  assert.match(written, /extra: &n 9007199254740995/);
  assert.match(written, /other: \*n/);
  await state._settingsRebase(written);
  assert.equal(state.settingsConfig.general.max_concurrent_agents, '9007199254740997');
});

for (const raw of ['010', '-010', '+010', '!!int "010"', "!!int '010'", '!!int "-010"', '!!int "+010"']) {
  test(`legacy numeric ${raw} refuses serialization without changing raw text`, async () => {
    const text = `max_concurrent_agents: ${raw}\n`;
    let puts = 0;
    const state = await settingsState(text, { fetch: async (_url, options) => {
      if (options?.method === 'PUT') puts++;
      return { ok: true, json: async () => ({ content: text }) };
    } });
    state.settingsConfig.general.branch_prefix = 'new';
    await assert.rejects(state._settingsWrite(), /edit.*file|file.*direct/i);
    assert.equal(await state.saveSettings(), false);
    assert.equal(puts, 0);
    assert.equal(state._settingsRawText, text);
  });
}

for (const action of ['discard', 'stale save']) {
  test(`${action} clears loading when invalidating a background read`, async () => {
    const wait = deferred();
    const state = await settingsState(settingsFixture, { fetch: async () => {
      await wait.promise;
      return { ok: true, json: async () => ({ content: settingsFixture }) };
    } });
    const refresh = state._settingsRefreshForm();
    assert.equal(state.settingsLoading, true);
    state.settingsConfig.general.branch_prefix = 'edited';
    if (action === 'discard') state.discardSettings();
    else {
      state._settingsStale = async () => true;
      assert.equal(await state.saveSettings(), false);
    }
    assert.equal(state.settingsLoading, false);
    wait.resolve();
    await refresh;
    assert.equal(state.settingsLoading, false);
  });
}

test('legacy built-in custom status has one catalogue identity without rewriting YAML', async () => {
  const text = 'statuses: [human_review, needs_review]\n';
  const state = await settingsState(text);
  const fields = state.settingsFieldCatalogue();
  assert.equal(new Set(fields.map(f => f.id)).size, fields.length);
  state.settingsQuery = 'human_review';
  assert.equal(state.settingsSearchResults().length, 1);
  assert.equal(state.settingsConfig.statuses.includes('human_review'), false);
  state.settingsConfig.general.branch_prefix = 'edited';
  assert.match(await state._settingsWrite(), /statuses: \[\s*human_review, needs_review\s*\]/);
});

test('summary overrides keep setting labels and help while agent fields use their own context', async () => {
  const state = await settingsState('summary_model: { worker: model }\nsummary_effort: { worker: high }\nassistant: { agent: worker }\nprojects: { repo: { path: /repo, agent: worker } }\npipelines: { work: [{ stage: code, agent: worker }] }\n');
  state.settingsQuery = 'worker';
  const summaries = state.settingsSearchResults().filter(f => f.entity === 'worker');
  assert.deepEqual(Array.from(summaries, f => f.label), ['Final summary model', 'Final summary effort']);
  assert.ok(summaries.every(f => f.help && f.entity === 'worker'));
  for (const field of state.settingsFieldCatalogue().filter(f => f.key.at(-1) === 'agent')) {
    assert.equal(field.label, field.section === 'assistant' ? 'Chat agent' : 'Agent');
  }
});

for (const supersede of ['new navigation', 'leave guard']) {
  test(`pending field reveal cannot focus after ${supersede}`, async () => {
    const wait = deferred();
    let lookups = 0;
    const state = await settingsState(settingsFixture, { document: {
      getElementById() { lookups++; return null; },
    } });
    state.$nextTick = fn => wait.promise.then(fn);
    state.settingsConfig.general.max_concurrent_agents = '4';
    const reveal = state.settingsRevealField(state.settingsFieldIdentity(['max_concurrent_agents']), false);
    if (supersede === 'new navigation') ++state._navigationSeq;
    else assert.equal(await state.gotoView('board'), false);
    wait.resolve();
    await reveal;
    assert.equal(lookups, 0);
  });
}

for (const outcome of ['visible', 'new navigation', 'leave guard', 'destroyed', 'still hidden']) {
  test(`field reveal waits for Alpine visibility and checks ownership: ${outcome}`, async () => {
    const frames = [], focused = [], scrolled = [];
    let visible = false;
    const target = {
      getClientRects: () => visible ? [{}] : [],
      focus: () => focused.push(true), scrollIntoView: () => scrolled.push(true),
    };
    const state = await settingsState(settingsFixture, {
      document: { getElementById: () => target },
      requestAnimationFrame: callback => frames.push(callback),
    });
    const reveal = state.settingsRevealField(state.settingsFieldIdentity(['max_concurrent_agents']), false);
    for (let n = 0; n < 8; n++) await Promise.resolve();
    assert.equal(frames.length, 1);
    assert.deepEqual(focused, []);
    frames.shift()();
    assert.equal(frames.length, 1);
    visible = outcome !== 'still hidden';
    if (outcome === 'new navigation') ++state._navigationSeq;
    if (outcome === 'leave guard') state.settingsGuard = true;
    if (outcome === 'destroyed') state._appDestroyed = true;
    frames.shift()();
    await reveal;
    assert.deepEqual(focused, outcome === 'visible' ? [true] : []);
    assert.deepEqual(scrolled, focused);
  });
}

test('companion diagnostics accept its visible target and segmented identity', async () => {
  const state = await settingsState();
  state.settingsConfig.assistant.companion.name = 'a'.repeat(65);
  const message = state.settingsFieldIssue(state.settingsFieldIdentity(['assistant', 'companion', 'name']));
  assert.match(message, /1-64/);
  assert.equal(state.settingsFieldIssue('companion-name'), message);
});

for (const [key, getValue] of [
  [['max_concurrent_agents'], state => state.settingsConfig.general],
  [['max_concurrent_epic_supervisors'], state => state.settingsConfig.general],
  [['web', 'port'], state => state.settingsConfig.web],
  [['notifications', 'attempts'], state => state.settingsConfig.notifications],
]) {
  test(`integer syntax advisory for ${key.join('.')}`, async () => {
    const state = await settingsState();
    const target = getValue(state);
    target[key.at(-1)] = 'abc';
    const issue = state.settingsClientIssues().find(i => i.key.join('.') === key.join('.'));
    assert.equal(issue?.blocking, false);
    assert.match(state.settingsFieldIssue(state.settingsFieldIdentity(key)), /integer/i);
    for (const value of ['', '0', '-1', '+3', '9007199254740993']) {
      target[key.at(-1)] = value;
      assert.equal(state.settingsClientIssues().some(i => !i.blocking && i.key.join('.') === key.join('.')), false);
    }
  });
}

test('catalogue identities are unique and search never includes private values', async () => {
  const state = await settingsState();
  const fields = state.settingsFieldCatalogue();
  assert.equal(new Set(fields.map(f => f.id)).size, fields.length);
  for (const field of fields) {
    for (const query of [field.label, field.configKey].filter(Boolean)) {
      state.settingsQuery = query;
      assert.ok(state.settingsSearchResults().some(result => result.id === field.id), field.id + ' / ' + query);
    }
  }
  assert.notEqual(state.settingsFieldID(['a.b', 'c']), state.settingsFieldID(['a', 'b.c']));
  for (const query of ['never-search-this-value', 'never-search-this-header', 'Keep this prompt private']) {
    state.settingsQuery = query;
    assert.equal(state.settingsSearchResults().length, 0);
  }
  state.settingsQuery = 'timeout';
  assert.ok(state.settingsSearchResults().some(f => f.section === 'stages' && f.entity === 'implement'));
  assert.ok(state.settingsSearchResults().some(f => f.section === 'assistant'));
});

test('diagnostics contain trusted field identities instead of parsing error strings', async () => {
  const state = await settingsState();
  state.settingsConfig.agents.claude.binary = '';
  const issue = state.settingsClientIssues().find(i => i.key.join('.') === 'agents.claude.binary');
  assert.ok(issue?.blocking);
  assert.ok(state.settingsFieldCatalogue().some(f => f.id === issue.field));
});

for (const [value, valid] of [['0', true], ['+0', true], ['-0', true], ['1h30m', true], ['-.5h', true], ['1h+30m', false], ['.5ms', true], ['-5m', true]]) {
  test(`duration advisory ${value}`, async () => assert.equal((await settingsState()).settingsDurationValid(value), valid));
}
