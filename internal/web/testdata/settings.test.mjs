import assert from 'node:assert/strict';
import test from 'node:test';
import * as yaml from '../static/vendor/yaml@2.8.1/yaml.mjs';
import { deferred, settingsFixture, settingsNotificationMergeCases, settingsState } from './settings_fixtures.mjs';

for (const value of ['""', 'null', 'kept']) {
  test(`removing baseline environment value ${value} survives save and reopen`, async () => {
    let raw = settingsFixture.replace('PRIVATE: never-search-this-value', `PRIVATE: never-search-this-value\n  EMPTY: ${value} # remove this entry`);
    const state = await settingsState(raw, { fetch: async (url, options) => {
      if (url === '/api/config') return { ok: true, json: async () => ({}) };
      if (!options) return { ok: true, json: async () => ({ content: raw }) };
      const body = JSON.parse(options.body);
      assert.equal(body.expected_content, raw);
      raw = body.content;
      return { status: 204 };
    } });
    state.settingsRemoveEnv('EMPTY');
    state.settingsConfig.general.branch_prefix = 'another-edit';
    assert.ok(state.settingsChangedPaths().includes('environment.EMPTY'));
    assert.equal(await state.saveSettings(), true);
    assert.equal(Object.hasOwn(yaml.parse(raw).environment, 'EMPTY'), false);
    assert.match(raw, /# Preserve this comment/);
    assert.equal(state.settingsDirty(), false);
    const reopened = await settingsState(raw);
    assert.equal(Object.hasOwn(reopened.settingsConfig.environment, 'EMPTY'), false);
  });
}

test('unfinished new blank environment rows remain unchanged and unwritten', async () => {
  const state = await settingsState();
  state.settingsNewEnvKey = 'UNFINISHED';
  state.settingsAddEnv();
  assert.equal(state.settingsDirty(), false);
  state.settingsConfig.general.branch_prefix = 'another-edit';
  assert.equal(state.settingsChangedPaths().includes('environment.UNFINISHED'), false);
  assert.equal(Object.hasOwn(yaml.parse(await state._settingsWrite()).environment, 'UNFINISHED'), false);
});

for (const boundary of ['headers', 'body']) {
  for (const older of ['_settingsRefreshConfigCache', 'refreshConfigCache']) {
    test(`Settings shares config request ordering: older ${older} at ${boundary}`, async () => {
      const gate = deferred(), entered = deferred();
      let reads = 0;
      const state = await settingsState(settingsFixture, { fetch: async url => {
        assert.equal(url, '/api/config');
        const first = ++reads === 1;
        if (first && boundary === 'headers') { entered.resolve(); await gate.promise; }
        return { ok: true, json: async () => {
          if (first && boundary === 'body') { entered.resolve(); await gate.promise; }
          return { custom_statuses: [first ? 'obsolete' : 'newer'] };
        } };
      } });
      state.configCache = { custom_statuses: ['initial'] };
      state.tickets = [{ id: 'keep', status: 'newer' }];
      const tickets = state.tickets;
      const pending = state[older]();
      await entered.promise;
      await state[older === 'refreshConfigCache' ? '_settingsRefreshConfigCache' : 'refreshConfigCache']();
      gate.resolve();
      await pending;
      assert.deepEqual([...state.configCache.custom_statuses], ['newer']);
      assert.equal(state.tickets, tickets);
      Object.assign(state, { _builtinColumns: [{ key: 'done', statuses: ['done'] }], _knownCustomStatuses: {},
        allBoardTickets: () => state.tickets, mobileColumn: () => null, setActiveColumn() {} });
      assert.ok(state._deriveColumns().some(column => column.key === 'newer'));
      assert.equal(state.columns.some(column => column.key === 'obsolete'), false);
    });
  }
}

for (const failure of ['http', 'network', 'decode']) {
  test(`confirmed save remains confirmed after cache ${failure} failure`, async () => {
    let raw = settingsFixture, puts = 0, refreshes = 0;
    const state = await settingsState(raw, { fetch: async (url, options) => {
      if (url === '/api/config') {
        refreshes++;
        if (failure === 'network') throw new Error('offline');
        return { ok: failure !== 'http', status: 503, json: async () => { throw new Error('bad JSON'); } };
      }
      if (!options) return { ok: true, json: async () => ({ content: raw }) };
      puts++;
      raw = JSON.parse(options.body).content;
      return { status: 204 };
    } });
    const cached = state.configCache = { custom_statuses: ['retained'] };
    state.settingsConfig.general.branch_prefix = 'confirmed';
    assert.equal(await state.saveSettings(), true);
    assert.equal(puts, 1);
    assert.equal(refreshes, 1);
    assert.equal(state.configCache, cached);
    assert.equal(state.settingsDirty(), false);
    assert.equal(state.settingsErrors.length, 0);
    assert.equal(state.settingsSavedAt, '12:00');
    assert.equal(state.settingsReloadWarning, '');
  });
}

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

for (const [raw, mode, statuses] of [
  ['', 'unset', null],
  ['  default_statuses: null\n', 'unset', null],
  ['  default_statuses: []\n', 'off', []],
  ['  default_statuses: [waiting, human_review, paused]\n', 'needs', ['waiting', 'human_review', 'paused']],
  ['  default_statuses: [human_review, done]\n', 'done', ['human_review', 'done']],
  ['  default_statuses: [qa, waiting]\n', 'custom', ['qa', 'waiting']],
]) {
  test(`global notification statuses hydrate as ${mode}: ${raw.trim() || 'missing'}`, async () => {
    const text = `statuses: [qa]\nnotifications:\n${raw}  all_changes: true\n`;
    const state = await settingsState(text);
    assert.equal(state.settingsNotifStatusMode(), mode);
    assert.equal(state.settingsNotifStatusesOpen, mode === 'custom');
    assert.deepEqual(state.settingsConfig.notifications.default_statuses == null ? null
      : Array.from(state.settingsConfig.notifications.default_statuses), statuses);
    assert.equal(state.settingsDirty(), false);
    state.settingsConfig.general.branch_prefix = 'edited';
    const written = await state._settingsWrite();
    const doc = yaml.parseDocument(written);
    const key = ['notifications', 'default_statuses'];
    assert.equal(doc.hasIn(key), !!raw);
    if (raw) {
      assert.equal(doc.getIn(key, true).toString(), state._settingsDoc.getIn(key, true).toString());
      assert.deepEqual(doc.toJS().notifications.default_statuses, statuses);
    } else assert.match(state.settingsNotifStatusesEcho(), /Unset/);
  });
}

for (const [name, text, statuses] of settingsNotificationMergeCases) {
  test(`global notification statuses resolve ${name} without rewriting YAML`, async () => {
    const state = await settingsState(text);
    const selected = () => state.settingsConfig.notifications.default_statuses == null
      ? null : Array.from(state.settingsConfig.notifications.default_statuses);
    assert.deepEqual(selected(), statuses);
    assert.equal(state.settingsNotifStatusMode() === 'unset', statuses === null);
    assert.equal(state.settingsDirty(), false);
    state.settingsConfig.general.branch_prefix = 'edited';
    assert.deepEqual(Array.from(state.settingsChangedPaths()), ['branch_prefix']);
    const written = await state._settingsWrite();
    const after = yaml.parseDocument(written);
    after.delete('branch_prefix');
    assert.equal(String(after), String(yaml.parseDocument(text)));
    assert.deepEqual(yaml.parse(written, { merge: true }).notifications.default_statuses, statuses);
    await state._settingsRebase(written);
    assert.deepEqual(selected(), statuses);
    assert.equal(state.settingsDirty(), false);
  });

  for (const mode of ['needs', 'off']) {
    test(`global notification ${mode} edit overrides ${name} without changing merge sources`, async () => {
      const state = await settingsState(text);
      const before = yaml.parseDocument(text);
      state.settingsNotifPickStatusMode(mode);
      const expected = Array.from(state.settingsConfig.notifications.default_statuses);
      const written = await state._settingsWrite();
      const after = yaml.parseDocument(written);
      assert.deepEqual(yaml.parse(written, { merge: true }).notifications.default_statuses, expected);
      if (state.settingsDirty()) {
        assert.deepEqual(after.getIn(['notifications', 'default_statuses'], true).toJSON(), expected);
      }
      after.deleteIn(['notifications', 'default_statuses']);
      before.deleteIn(['notifications', 'default_statuses']);
      assert.equal(String(after), String(before));
      await state._settingsRebase(written);
      assert.equal(state.settingsNotifStatusMode(), mode);
      assert.equal(state.settingsDirty(), false);
    });
  }
}

test('saving a delivery edit retains merged status defaults through rebase and reopen', async () => {
  let raw = 'notifications: { <<: { default_statuses: [done] }, enabled: true }\n';
  let puts = 0;
  const state = await settingsState(raw, { fetch: async (_url, options) => {
    if (!options) return { ok: true, json: async () => ({ content: raw }) };
    const body = JSON.parse(options.body);
    assert.equal(body.expected_content, raw);
    raw = body.content;
    puts++;
    return { status: 204 };
  } });
  state.settingsConfig.notifications.enabled = false;
  assert.deepEqual(Array.from(state.settingsChangedPaths()), ['notifications.enabled']);
  assert.equal(await state.saveSettings(), true);
  assert.equal(puts, 1);
  assert.deepEqual(Array.from(state.settingsConfig.notifications.default_statuses), ['done']);
  assert.equal(state.settingsDirty(), false);
  assert.equal(yaml.parseDocument(raw).hasIn(['notifications', 'default_statuses']), false);
  const reopened = await settingsState(raw);
  assert.deepEqual(Array.from(reopened.settingsConfig.notifications.default_statuses), ['done']);
});

test('status edits still refuse to strand aliases to an explicitly anchored sequence', async () => {
  const text = 'notifications: { <<: { enabled: true }, default_statuses: &choice [done] }\nprojects: { repo: { path: /repo, notify: *choice } }\n';
  let puts = 0;
  const state = await settingsState(text, { fetch: async (_url, options) => {
    if (options?.method === 'PUT') puts++;
    return { ok: true, json: async () => ({ content: text }) };
  } });
  state.settingsNotifPickStatusMode('off');
  await assert.rejects(state._settingsWrite(), /Edit the file directly/);
  assert.equal(await state.saveSettings(), false);
  assert.equal(puts, 0);
  assert.equal(state._settingsRawText, text);
  assert.deepEqual(yaml.parse(text, { merge: true }).projects.repo.notify, ['done']);
});

test('global status presets save, reload, and never call ticket autosave', async () => {
  let raw = settingsFixture;
  const state = await settingsState(raw, { fetch: async (_url, options) => {
    if (!options) return { ok: true, json: async () => ({ content: raw }) };
    const body = JSON.parse(options.body);
    assert.equal(body.expected_content, raw);
    raw = body.content;
    return { status: 204 };
  }, state: { notifyChanged() { throw new Error('Settings must not autosave a ticket'); } } });
  for (const mode of state.settingsNotifStatusModes()) {
    state.settingsNotifPickStatusMode(mode.key);
    assert.deepEqual(Array.from(state.settingsConfig.notifications.default_statuses), Array.from(mode.statuses));
    assert.notEqual(state.settingsConfig.notifications.default_statuses, mode.statuses);
    assert.deepEqual(Array.from(state.settingsChangedPaths()), ['notifications.default_statuses']);
    assert.equal(state.settingsPathDirty('notifications'), true);
    assert.equal(state.settingsRestartPending(), 0);
    assert.equal(await state.saveSettings(), true);
    assert.equal(state.settingsDirty(), false);
    const reopened = await settingsState(raw);
    assert.equal(reopened.settingsNotifStatusMode(), mode.key);
    assert.deepEqual(yaml.parse(raw).notifications.default_statuses, Array.from(mode.statuses));
  }
});

test('opening custom controls leaves missing defaults unset; Off writes an explicit empty list', async () => {
  const state = await settingsState();
  state.settingsNotifStatusesOpen = true;
  state.settingsNotifPickStatusMode('custom');
  assert.equal(state.settingsConfig.notifications.default_statuses, null);
  assert.equal(state.settingsDirty(), false);
  state.settingsNotifPickStatusMode('off');
  assert.equal(state.settingsNotifStatusMode(), 'off');
  assert.deepEqual(Array.from(state.settingsChangedPaths()), ['notifications.default_statuses']);
  assert.match(state.settingsNotifStatusesEcho(), /default_statuses: \[\]/);
  assert.deepEqual(yaml.parse(await state._settingsWrite()).notifications.default_statuses, []);
  state.discardSettings();
  assert.equal(state.settingsNotifStatusMode(), 'unset');
  assert.equal(state.settingsNotifStatusesOpen, false);
  assert.equal(state.settingsDirty(), false);
});

test('notification routing and channel edits leave missing status defaults unset', async () => {
  const state = await settingsState();
  state.settingsNotifToggleDefault('hook');
  state.settingsNotifChannel('hook').url = 'https://changed.invalid';
  state.settingsConfig.notifications.enabled = false;
  const written = await state._settingsWrite();
  assert.equal(yaml.parseDocument(written).hasIn(['notifications', 'default_statuses']), false);
  assert.equal(state.settingsNotifStatusMode(), 'unset');
  assert.equal(state.settingsChangedPaths().includes('notifications.default_statuses'), false);
});

test('clearing all default status chips persists Off instead of removing the preference', async () => {
  const state = await settingsState('notifications: { default_statuses: [waiting] }\n');
  state.settingsNotifToggleStatus('waiting');
  assert.equal(state.settingsNotifStatusMode(), 'off');
  const doc = yaml.parseDocument(await state._settingsWrite());
  assert.equal(doc.hasIn(['notifications', 'default_statuses']), true);
  assert.deepEqual(doc.toJS().notifications.default_statuses, []);
});

test('custom status chips use edited statuses, retain unknown selections for removal, and honor save locks', async () => {
  const state = await settingsState('notifications: { default_statuses: [old_status] }\n');
  state.configCache = { custom_statuses: ['stale_status'] };
  state.settingsNewStatus = 'fresh_status';
  state.settingsAddStatus();
  assert.ok(state.settingsNotifStatusChoices().includes('fresh_status'));
  assert.ok(state.settingsNotifStatusChoices().includes('old_status'));
  assert.equal(state.settingsNotifStatusChoices().includes('stale_status'), false);
  state.settingsNotifToggleStatus('old_status');
  state.settingsNotifToggleStatus('fresh_status');
  state.settingsNotifToggleStatus('waiting');
  assert.equal(state.settingsNotifStatusMode(), 'custom');
  assert.equal(state.settingsNotifStatusOn('fresh_status'), true);
  assert.deepEqual(Array.from(state.settingsClientErrors()), []);
  const written = await state._settingsWrite();
  const reopened = await settingsState(written);
  assert.equal(reopened.settingsNotifStatusMode(), 'custom');
  assert.deepEqual(Array.from(reopened.settingsConfig.notifications.default_statuses), ['fresh_status', 'waiting']);
  state.settingsSaving = true;
  state.settingsNotifPickStatusMode('off');
  state.settingsNotifToggleStatus('waiting');
  assert.deepEqual(Array.from(state.settingsConfig.notifications.default_statuses), ['fresh_status', 'waiting']);
});

test('global status edits preserve unrelated notification and project YAML nodes', async () => {
  const text = `# retain root comment
projects:
  repo: # keep repo
    path: /repo
    notify: [] # project Off
    future: { unknown: keep }
notifications:
  default_statuses: [qa]
  default: [hook] # destinations
  enabled: false # delivery stays off
  all_changes: true # preserve origins
  channels:
    hook: # channel comment
      type: webhook
      url: 'https://example.invalid'
      headers: { X-Trace: keep } # header comment
  future: { untouched: yes }
statuses: [qa]
`;
  const state = await settingsState(text);
  const before = state._settingsDoc;
  state.settingsNotifPickStatusMode('needs');
  const written = await state._settingsWrite();
  const after = yaml.parseDocument(written);
  assert.deepEqual(Array.from(state.settingsChangedPaths()), ['notifications.default_statuses']);
  for (const key of [['projects'], ['statuses'], ...['default', 'enabled', 'all_changes', 'channels', 'future'].map(key => ['notifications', key])]) {
    assert.equal(after.getIn(key, true).toString(), before.getIn(key, true).toString(), key.join('.'));
  }
  for (const comment of ['retain root comment', 'keep repo', 'project Off', 'destinations', 'delivery stays off', 'preserve origins', 'channel comment', 'header comment']) {
    assert.ok(written.includes('# ' + comment), comment);
  }
  assert.deepEqual(after.toJS().notifications.default_statuses, ['paused', 'human_review', 'waiting']);
});

for (const statuses of [['none'], ['missing_status'], ['paused', 'none']]) {
  test(`global notification statuses reject ${statuses.join(', ')} without sending a save`, async () => {
    let requests = 0;
    const state = await settingsState('statuses: [none]\n', { fetch: async () => { requests++; } });
    state.settingsConfig.notifications.default_statuses = statuses;
    const issue = state.settingsClientIssues().find(issue => issue.key.join('.') === 'notifications.default_statuses');
    assert.equal(issue?.blocking, true);
    assert.match(issue.message, /notifications.default_statuses/);
    assert.equal(state.settingsFieldIssue('notif-default_statuses'), issue.message);
    assert.equal(await state.saveSettings(), false);
    assert.equal(requests, 0);
  });
}

test('global default validation accepts all known statuses without destinations or delivery', async () => {
  const state = await settingsState('statuses: [qa]\nnotifications: { enabled: false }\n');
  state.settingsConfig.notifications.default_statuses = [...state.settingsBuiltinStatuses, 'archived', 'waiting', 'qa'];
  assert.deepEqual(Array.from(state.settingsClientErrors()), []);
  state.settingsRemoveStatus('qa');
  assert.match(state.settingsClientErrors().join(' '), /notifications.default_statuses.*qa/);
});

test('notification default search and field reveal focus the status control', async () => {
  let focused = false;
  const lookedUp = [];
  const state = await settingsState(settingsFixture, { document: { getElementById(id) {
    lookedUp.push(id);
    return id === 'notif-default_statuses' ? {
      getClientRects: () => [{}], focus() { focused = true; }, scrollIntoView() {},
    } : null;
  } } });
  const field = state.settingsFieldCatalogue().find(field => field.configKey === 'notifications.default_statuses');
  assert.equal(field.section, 'notifications');
  assert.equal(field.target, 'notif-default_statuses');
  assert.match(field.help, /live Pi questions/);
  for (const query of ['default_statuses', 'Default notification statuses', 'explicit starts']) {
    state.settingsQuery = query;
    assert.ok(state.settingsSearchResults().some(result => result.id === field.id));
  }
  await state.settingsRevealField(field.id, false);
  assert.equal(state.settingsSection, 'notifications');
  assert.equal(focused, true);
  assert.ok(lookedUp.includes('notif-default_statuses'));
});

for (const [value, valid] of [['0', true], ['+0', true], ['-0', true], ['1h30m', true], ['-.5h', true], ['1h+30m', false], ['.5ms', true], ['-5m', true]]) {
  test(`duration advisory ${value}`, async () => assert.equal((await settingsState()).settingsDurationValid(value), valid));
}
