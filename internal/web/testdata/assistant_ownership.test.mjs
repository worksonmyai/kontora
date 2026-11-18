import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';

// Exercise the production mixins together, without importing the shared fixtures.
// The parent Go wrapper can run this file directly with node --test.
const source = ['assistant', 'companion', 'assistant_markup'].map(name =>
  fs.readFileSync(new URL(`../ui/${name}.js`, import.meta.url), 'utf8').replaceAll('export ', '')).join('\n');
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const response = (data = {}, status = 200, tag = '') => ({ ok: status >= 200 && status < 300, status, headers: { get: () => tag }, json: async () => data });
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };

function harness(fetcher = () => response(), { storageFails = false, stored = {} } = {}) {
  const timers = new Map(), intervals = new Map(), frames = new Map(), streams = [], requests = [];
  let serial = 0;
  const context = vm.createContext({
    AbortController, TextEncoder, Date, console,
    localStorage: { getItem(key) { if (storageFails) throw Error('private'); return stored[key] ?? null; }, setItem(key, value) { if (storageFails) throw Error('private'); stored[key] = value; } },
    document: { getElementById: () => null }, window: { addEventListener() {}, removeEventListener() {} },
    setTimeout: (cb, ms) => { timers.set(++serial, { cb, ms }); return serial; }, clearTimeout: id => timers.delete(id),
    setInterval: cb => { intervals.set(++serial, cb); return serial; }, clearInterval: id => intervals.delete(id),
    requestAnimationFrame: cb => { frames.set(++serial, cb); return serial; }, cancelAnimationFrame: id => frames.delete(id),
    fetch: (url, options) => { requests.push({ url, options }); return fetcher(url, options); },
    EventSource: class {
      handlers = new Map(); closed = false;
      constructor() { streams.push(this); }
      addEventListener(name, cb) { this.handlers.set(name, cb); }
      close() { this.closed = true; }
      emit(name, data = {}) { this.handlers.get(name)?.({ data: JSON.stringify(data) }); }
    },
  });
  vm.runInContext(source + '\nthis.state = Object.assign(kontoraAssistant(), kontoraCompanion(), kontoraAssistantMarkup()); this.stream = assistantStream;', context);
  const state = context.state;
  Object.assign(state, { assistantOpen: true, assistantAgent: { enabled: true }, assistantThread: { id: 'a', autonomy: 'ask' },
    isSecondary: () => false, $nextTick() {}, tickets: [], selectedTicket: null });
  const h = { state, stream: context.stream, requests, streams, timers, frames, intervals,
    fire() { const entry = [...timers.entries()][0]; assert.ok(entry, 'expected scheduled read'); timers.delete(entry[0]); entry[1].cb(); },
    edit(text) { state.assistantDraft = text; state.assistantDraftChanged(); },
    live(text = 'accepted') { state._assistantSetStreaming(true); streams.at(-1).emit('reset', { gen: 1, text }); },
  };
  return h;
}

for (const edit of ['newer', 'identical', 'suggestion']) {
  test(`audit32-ownership first send captures text and preserves ${edit} draft revision`, async () => {
    const create = deferred();
    const h = harness((url, options) => url.endsWith('/threads') && options?.method === 'POST'
      ? create.promise : response(url.endsWith('/threads') ? { threads: [] } : { running: true }));
    h.state.assistantThread = null;
    h.edit('  original  ');
    const pending = h.state.sendAssistantMessage();
    const duplicate = h.state.sendAssistantMessage();
    const creates = () => h.requests.filter(r => r.options?.method === 'POST' && r.url.endsWith('/threads'));
    assert.equal(creates().length, 1, 'first-send token is acquired before creation');
    if (edit === 'newer') h.edit('new draft');
    else if (edit === 'identical') { h.edit(''); h.edit('  original  '); }
    else {
      h.edit('/status');
      h.state.acceptAssistantMenu();
    }
    const retained = h.state.assistantDraft;
    create.resolve(response({ id: 'new', autonomy: 'ask' }, 201));
    await pending; await duplicate;
    const send = h.requests.find(r => r.url.endsWith('/messages'));
    assert.equal(JSON.parse(send.options.body).text, 'original');
    assert.equal(h.state.assistantDraft, retained);
    assert.equal(h.state.assistantSending, false);
    assert.doesNotMatch(h.state.assistantComposerMarkup.match(/<textarea[^>]*>/)[0], /disabled/);
    assert.match(h.state.assistantComposerMarkup, /:disabled="[^"]*assistantSending/);
    h.state.assistantDestroy();
  });
}

test('audit32-ownership unchanged raw first-send revision clears, failed creation unlocks without replay', async () => {
  let fail = true;
  const h = harness((url, options) => options?.method === 'POST' && url.endsWith('/threads')
    ? response(fail ? { error: 'creation failed' } : { id: 'new', autonomy: 'ask' }, fail ? 503 : 201) : response({ running: false }));
  h.state.assistantThread = null; h.edit('  send me  ');
  await h.state.sendAssistantMessage();
  assert.equal(h.state.assistantDraft, '  send me  ');
  assert.equal(h.state.assistantSending, false);
  assert.equal(h.requests.length, 1);
  fail = false; await h.state.sendAssistantMessage();
  assert.equal(h.state.assistantDraft, '');
  h.state.assistantDestroy();
});

for (const revision of ['unchanged', 'retyped', 'suggestion']) {
  test(`audit32-ownership companion followup clears only unchanged revision: ${revision}`, async () => {
    const write = deferred();
    const h = harness(url => url.endsWith('/companion/messages') ? write.promise : response({ running: true }));
    h.state.assistantThread.purpose = 'companion';
    h.state.companionSnapshot = { enabled: true, ai_available: true };
    h.edit('question');
    const pending = h.state.sendAssistantMessage();
    if (revision === 'retyped') { h.edit(''); h.edit('question'); }
    if (revision === 'suggestion') {
      h.state.assistantMenu = { from: 0, index: 0, rows: [{ insert: 'question' }] };
      h.state.acceptAssistantMenu();
    }
    write.resolve(response({ id: 'a', purpose: 'companion', running: true }));
    await pending;
    assert.equal(h.state.assistantDraft, revision === 'unchanged' ? '' : 'question');
    h.state.assistantDestroy();
  });
}

for (const storageFails of [false, true]) {
  test(`audit32-ownership future-chat preference is independent of selected autonomy (private=${storageFails})`, async () => {
    const h = harness((url, options) => options?.method === 'POST' && url.endsWith('/threads')
      ? response({ id: 'new', autonomy: JSON.parse(options.body).autonomy }) : response({}), { storageFails });
    h.state.assistantAgent.autonomy = 'auto'; h.state._assistantSeedAutonomy();
    assert.equal(h.state.assistantAutonomy, 'ask');
    h.state._mergeAssistantActivity({ autonomy: 'read' });
    await h.state.assistantNewThread();
    assert.equal(JSON.parse(h.requests.find(r => r.options?.method === 'POST').options.body).autonomy, 'auto');
    h.state.setAssistantAutonomy('read');
    h.state._assistantSeedAutonomy(); // same seed must not erase an explicit pick, even without storage
    h.state._assistantAdopt({ id: 'old', autonomy: 'ask' }, []);
    await h.state.assistantNewThread();
    assert.equal(JSON.parse(h.requests.filter(r => r.options?.method === 'POST').at(-1).options.body).autonomy, 'read');
    h.state.assistantDestroy();
  });
}

for (const reset of ['delete', 'failed selection']) {
  test(`audit32-ownership ${reset} displays and submits the future permission`, async () => {
    const h = harness((url, options) => url.endsWith('/missing') ? response({ error: 'missing' }, 404)
      : options?.method === 'POST' && url.endsWith('/threads')
        ? response({ id: 'new', autonomy: JSON.parse(options.body).autonomy }) : response({}));
    h.state.assistantFutureAutonomy = 'auto';
    h.state._assistantAdopt({ id: 'a', autonomy: 'read' }, []);
    assert.equal(h.state.assistantAutonomyLabel(), 'read only');
    if (reset === 'delete') await h.state.assistantDeleteThread('a');
    else await h.state.assistantSelectThread('missing');
    assert.equal(h.state.assistantThread, null);
    assert.equal(h.state.assistantAutonomyLabel(), 'auto');
    assert.equal(h.state.assistantEffectiveAutonomy(), 'auto');
    assert.match(h.state.assistantComposerMarkup, /:class="assistantEffectiveAutonomy\(\) === mode.key/);
    h.edit('send with displayed permission');
    await h.state.sendAssistantMessage();
    for (const request of h.requests.filter(r => r.options?.method === 'POST')) {
      assert.equal(JSON.parse(request.options.body).autonomy, 'auto');
    }
    h.state.assistantDestroy();
  });
}

for (const origin of ['ordinary', 'companion']) {
  test(`audit32-ownership pending ${origin} send locks only its chat and cannot unlock a replacement`, async () => {
    const first = deferred(), second = deferred();
    const h = harness((url, options) => options?.method === 'POST' && url.endsWith('/messages')
      ? (url.includes('/companion/') || url.includes('/threads/a/') ? first.promise : second.promise)
      : response(url.endsWith('/b') ? { id: 'b', autonomy: 'ask' }
        : url.endsWith('/a') ? { id: 'a', autonomy: 'ask', ...(origin === 'companion' ? { purpose: 'companion' } : {}) } : {}));
    h.state.companionSnapshot = { enabled: true, ai_available: true };
    if (origin === 'companion') h.state.assistantThread.purpose = 'companion';
    h.edit('first');
    const pendingA = h.state.sendAssistantMessage();
    assert.equal(h.state.assistantSending, true);
    await h.state.assistantSelectThread('b');
    assert.equal(h.state.assistantSending, false, 'an idle chat stays sendable');
    await h.state.assistantSelectThread('a');
    assert.equal(h.state.assistantSending, true, 'returning to a pending chat retains its lock');
    await h.state.assistantSelectThread('b');
    h.edit('second');
    const pendingB = h.state.sendAssistantMessage();
    assert.equal(h.state.assistantSending, true);
    first.resolve(response({ id: 'a', purpose: origin === 'companion' ? 'companion' : undefined }));
    await pendingA;
    assert.equal(h.state.assistantSending, true, 'old completion cannot unlock B');
    second.resolve(response()); await pendingB;
    assert.equal(h.state.assistantSending, false);
    assert.equal(h.requests.filter(r => r.options?.method === 'POST' && r.url.endsWith('/messages')).length, 2);
    h.state.assistantDestroy();
  });
}

test('audit32-ownership reversed config decoding cannot roll back the agent or future preference', async () => {
  const body = deferred(); let reads = 0;
  const h = harness(() => ++reads === 1 ? { ...response(), json: () => body.promise } : response({ enabled: true, agent: 'new', autonomy: 'auto' }));
  const old = h.state._assistantRefreshConfig(); await flush();
  await h.state._assistantRefreshConfig();
  body.resolve({ enabled: false, agent: 'old', autonomy: 'read' }); await old;
  assert.equal(h.state.assistantAgent.agent, 'new');
  await h.state.assistantNewThread();
  const create = h.requests.find(r => r.options?.method === 'POST');
  assert.equal(JSON.parse(create.options.body).autonomy, 'auto');
  h.state.assistantDestroy();
});

test('audit32-ownership current selection failure empties the whole transcript but retains draft and error', async () => {
  const h = harness(() => response({ error: 'missing selected chat' }, 404));
  Object.assign(h.state, { assistantDraft: 'keep', assistantMessages: [{ text: 'old' }], assistantEvents: [{ kind: 'text', text: 'old' }],
    assistantGate: { id: 'gate' }, assistantExpanded: { old: true }, assistantWindow: 999, _assistantCursor: 9, _assistantETag: 'old', _assistantActivityLoaded: true });
  h.live();
  await h.state.assistantSelectThread('missing');
  assert.equal(h.state.assistantThread, null);
  for (const name of ['assistantMessages', 'assistantEvents', 'assistantThreadRows']) {
    assert.equal((typeof h.state[name] === 'function' ? h.state[name]() : h.state[name]).length, 0, name);
  }
  assert.equal(h.state.assistantPartial, ''); assert.equal(h.state.assistantPartialGen, 0);
  assert.equal(h.state.assistantPartialTool, ''); assert.equal(h.state.assistantPartialTyping, false);
  assert.equal(h.state.assistantGate, null); assert.equal(h.state._assistantCursor, 0);
  assert.equal(h.state._assistantETag, ''); assert.equal(h.state._assistantActivityLoaded, false);
  assert.equal(Object.keys(h.state.assistantExpanded).length, 0); assert.equal(h.state.assistantWindow, 200);
  assert.equal(h.state.assistantDraft, 'keep'); assert.match(h.state.assistantError, /missing selected/);
  assert.equal(h.timers.size, 0); assert.equal(h.frames.size, 0);
  h.state.assistantDestroy();
});

test('audit32-ownership obsolete selection failure cannot empty its replacement', async () => {
  const old = deferred();
  const h = harness(url => url.endsWith('/old') ? old.promise : response(url.endsWith('/b') ? { id: 'b', messages: [{ n: 1, text: 'B' }] } : {}));
  const pending = h.state.assistantSelectThread('old'); await h.state.assistantSelectThread('b');
  old.resolve(response({ error: 'gone' }, 404)); await pending;
  assert.equal(h.state.assistantThread.id, 'b'); assert.equal(h.state.assistantMessages[0].text, 'B');
  assert.equal(h.state.assistantError, null); h.state.assistantDestroy();
});

for (const choice of ['chat', 'history', 'inbox', 'draft', 'none']) {
  test(`audit32-ownership late companion init and mobile open preserve ${choice} intent`, async () => {
    const snapshot = deferred();
    const h = harness(url => url.endsWith('/companion') ? snapshot.promise : response({ threads: [] }));
    h.state.assistantThread = null;
    h.state.assistantView = choice === 'inbox' ? 'inbox' : 'thread';
    h.state.companionSnapshot = { enabled: true };
    const init = h.state.companionInit();
    if (choice === 'chat') h.state.assistantShowChat(); // already active is still explicit
    if (choice === 'history') h.state.assistantShowHistory();
    if (choice === 'inbox') h.state.companionOpenInbox();
    if (choice === 'draft') h.edit('unsent');
    snapshot.resolve(response({ enabled: true })); await init;
    h.state.openAssistantSheet();
    assert.equal(h.state.assistantView, choice === 'history' ? 'history' : choice === 'inbox' || choice === 'none' ? 'inbox' : 'thread');
    h.state.companionDestroy(); h.state.assistantDestroy();
  });
}

test('audit32-ownership activity flushes accepted frames and excludes SSE through body decoding and coalescing', async () => {
  const first = deferred(), body = deferred(), second = deferred(); let reads = 0;
  const h = harness(() => ++reads === 1 ? first.promise : second.promise);
  h.live('long old turn'); const old = h.streams[0];
  old.emit('delta', { gen: 1, text: ' tail' });
  const pending = h.state._loadAssistantActivity();
  assert.equal(h.state.assistantPartial, 'long old turn tail', 'accepted frames flush before read');
  assert.equal(old.closed, true); assert.equal(h.frames.size, 0);
  old.emit('delta', { gen: 1, text: ' obsolete' });
  h.state._assistantOpenStream(); assert.equal(h.streams.length, 1);
  first.resolve({ ...response({}, 200, 'one'), json: () => body.promise }); await flush();
  h.state._loadAssistantActivity({ fresh: true });
  body.resolve({ running: true, partial: 'new', partial_gen: 1 }); await flush();
  assert.equal(h.state.assistantPartial, 'new', 'snapshot is authoritative even at reused generation');
  assert.equal(h.streams.length, 1, 'no reopen between coalesced reads');
  old.emit('reset', { gen: 1, text: 'obsolete reset' });
  assert.equal(h.state.assistantPartial, 'new');
  second.resolve(response({ running: true, partial: 'new tail', partial_gen: 1 })); await pending;
  assert.equal(h.streams.length, 2); assert.equal(h.state.assistantPartial, 'new tail');
  const current = h.streams[1]; current.emit('reset', { gen: 1, text: 'new tail' }); current.emit('delta', { gen: 1, text: '!' });
  h.state._assistantFlushFrame(); assert.equal(h.state.assistantPartial, 'new tail!');
  h.state.assistantDestroy();
});

for (const running of [false, true]) {
  test(`audit32-ownership settled empty partial does not reconnect or replay landed text (running=${running})`, async () => {
    const h = harness(() => response({ running, tape: { events: [{ kind: 'text', text: 'landed' }] } }));
    h.live('landed'); await h.state._loadAssistantActivity();
    assert.equal(h.state.assistantPartial, ''); assert.equal(h.streams.filter(s => !s.closed).length, 0);
    assert.equal(h.streams.length, 1); h.state.assistantDestroy();
  });
}

test('audit32-ownership completed stream stays closed through cached revalidation', async () => {
  const h = harness(() => response({}, 304));
  h.live('completed'); h.streams[0].emit('done');
  await h.state._loadAssistantActivity();
  assert.equal(h.state.assistantPartialTyping, false);
  assert.equal(h.streams.length, 1); assert.equal(h.streams[0].closed, true);
  h.state.assistantDestroy();
});

test('audit32-ownership serialized reads preserve exhausted reconnect budget until actual stream progress', async () => {
  const h = harness(() => response({ running: true, partial: 'live', partial_gen: 1 }));
  h.live('live');
  for (let i = 0; i < 5; i++) {
    h.streams.at(-1).onerror();
    if (i < 4) h.fire();
  }
  assert.equal(h.streams.length, 5);
  for (let i = 0; i < 3; i++) await h.state._loadAssistantActivity();
  assert.equal(h.streams.length, 5, 'polling must not grant fresh reconnect attempts');
  h.state.assistantDestroy();
});

for (const recovered of [200, 304]) {
  test(`audit32-ownership read failure retains transcript and errors; ${recovered} clears recovery`, async () => {
    let fail = true;
    const h = harness(() => fail ? response({ error: 'transcript unavailable' }, 503) : response({ messages: [{ n: 1, text: 'saved' }], running: true, partial: 'live' }, recovered));
    h.state.assistantMessages = [{ n: 1, text: 'saved' }]; h.state.assistantError = 'send outcome uncertain'; h.state._assistantETag = 'good';
    await h.state._loadAssistantActivity();
    assert.equal(h.state.assistantMessages[0].text, 'saved'); assert.equal(h.state.assistantError, 'send outcome uncertain');
    assert.match(h.state.assistantReadError, /transcript unavailable/); assert.equal(h.state._assistantETag, 'good');
    assert.equal([...h.timers.values()][0].ms, 800);
    fail = false; h.fire(); await flush();
    assert.equal(h.state.assistantReadError, ''); assert.equal(h.state.assistantReadRetries, 0);
    assert.equal(h.state.assistantError, 'send outcome uncertain'); h.state.assistantDestroy();
  });
}

test('audit32-ownership four automatic read retries then manual Retry, with independent backoff', async () => {
  const h = harness(() => response({ error: 'busy transcript' }, 503));
  await h.state._loadAssistantActivity();
  for (const wait of [800, 1600, 3200, 5000]) {
    assert.equal(h.timers.size, 1); assert.equal([...h.timers.values()][0].ms, wait);
    h.fire(); await flush();
  }
  assert.equal(h.requests.length, 5); assert.equal(h.timers.size, 0);
  await h.state._loadAssistantActivity({ fresh: true }); assert.equal(h.requests.length, 5);
  assert.match(h.state.assistantTranscriptMarkup, /assistantReadError/);
  assert.match(h.state.assistantTranscriptMarkup, /assistantRetryActivity\(\)/);
  await h.state.assistantRetryActivity(); assert.equal(h.requests.length, 6);
  assert.equal([...h.timers.values()][0].ms, 800); h.state.assistantDestroy();
});

for (const status of [401, 403, 404, 410]) {
  test(`audit32-ownership terminal read status ${status} cannot create automatic retry or stream loops`, async () => {
    const h = harness(() => response({ error: 'unavailable owner' }, status));
    h.live(); await h.state._loadAssistantActivity();
    assert.equal(h.timers.size, 0); assert.equal(h.streams.filter(s => !s.closed).length, 0);
    await h.state._loadAssistantActivity({ fresh: true }); assert.equal(h.requests.length, 1);
    h.state.assistantDestroy();
  });
}

test('audit32-ownership backoff cannot be bypassed by notifications or reopening', async () => {
  const h = harness(() => response({ error: 'busy transcript' }, 503));
  await h.state._loadAssistantActivity();
  for (let i = 0; i < 10; i++) await h.state._loadAssistantActivity({ fresh: true });
  await h.state._assistantResume();
  assert.equal(h.requests.length, 1); assert.equal(h.timers.size, 1);
  assert.equal([...h.timers.values()][0].ms, 800); h.state.assistantDestroy();
});

test('audit32-ownership terminal status with JSON null retains its HTTP classification', async () => {
  const h = harness(() => response(null, 404));
  await h.state._loadAssistantActivity();
  assert.equal(h.timers.size, 0); assert.equal(h.state.assistantReadRetryable, false);
  h.state.assistantDestroy();
});

for (const view of ['chat', 'inbox']) {
  test(`audit32-ownership explicit current ${view} retires an unfinished selection`, async () => {
    const selected = deferred();
    const h = harness(url => url.endsWith('/b') ? selected.promise : response({ enabled: true }));
    h.state.companionSnapshot = { enabled: true };
    h.state.assistantView = view === 'chat' ? 'thread' : 'inbox';
    const pending = h.state.assistantSelectThread('b', { quiet: true });
    if (view === 'chat') h.state.assistantShowChat(); else h.state.companionOpenInbox();
    selected.resolve(response({ id: 'b', messages: [{ n: 1, text: 'wrong owner' }] }));
    assert.equal(await pending, false); assert.equal(h.state.assistantThread.id, 'a');
    h.state.assistantDestroy();
  });
}

for (const intent of ['chat', 'draft']) {
  test(`audit32-ownership late assistant init cannot restore a remembered thread over ${intent}`, async () => {
    const config = deferred();
    const h = harness(url => url === '/api/assistant' ? config.promise : response({}), { stored: { 'kontora-assistant-thread': 'remembered' } });
    h.state.assistantThread = null;
    const pending = h.state.initAssistant();
    if (intent === 'chat') h.state.assistantShowChat(); else h.edit('my draft');
    config.resolve(response({ enabled: true, autonomy: 'auto' })); await pending;
    assert.equal(h.requests.filter(r => r.url.includes('/remembered')).length, 0);
    assert.equal(h.state.assistantThread, null); h.state.assistantDestroy();
  });
}

for (const failure of ['network', 'decode']) {
  test(`audit32-ownership ${failure} failures also exhaust the dedicated read budget`, async () => {
    const h = harness(() => failure === 'network' ? Promise.reject(Error('network'))
      : { ...response(), json: () => Promise.reject(Error('decode')) });
    h.state.assistantEvents = [{ kind: 'text', text: 'retained' }];
    await h.state._loadAssistantActivity();
    for (let i = 0; i < 4; i++) { h.fire(); await flush(); }
    assert.equal(h.timers.size, 0); assert.equal(h.requests.length, 5);
    assert.equal(h.state.assistantEvents[0].text, 'retained'); h.state.assistantDestroy();
  });
}

test('audit32-ownership tool-only partials reopen streaming but settled snapshots clear generation too', async () => {
  let data = { running: true, partial_gen: 8, partial_tool: 'read' };
  const h = harness(() => response(data));
  await h.state._loadAssistantActivity();
  assert.equal(h.state.assistantPartialTool, 'read'); assert.equal(h.streams.filter(s => !s.closed).length, 1);
  data = { running: true, partial_gen: 8 }; await h.state._loadAssistantActivity();
  assert.equal(h.state.assistantPartialGen, 0); assert.equal(h.state.assistantPartialTool, '');
  assert.equal(h.streams.filter(s => !s.closed).length, 0); h.state.assistantDestroy();
});

test('audit32-ownership ordinary send keeps edits during the message POST without permitting duplicate sends', async () => {
  const write = deferred();
  const h = harness((url, options) => options?.method === 'POST' ? write.promise : response({ running: true }));
  h.edit('first'); const pending = h.state.sendAssistantMessage();
  h.edit('second'); await h.state.sendAssistantMessage();
  assert.equal(h.requests.filter(r => r.options?.method === 'POST').length, 1);
  write.resolve(response()); await pending;
  assert.equal(h.state.assistantDraft, 'second'); h.state.assistantDestroy();
});

test('audit32-ownership first send captures future autonomy and context before creation', async () => {
  const create = deferred();
  const h = harness((url, options) => url.endsWith('/threads') && options?.method === 'POST' ? create.promise : response({}));
  h.state.assistantThread = null; h.state.setAssistantAutonomy('auto');
  h.state.detailPageContext = () => ['accepted context']; h.edit('first');
  const pending = h.state.sendAssistantMessage();
  h.state.setAssistantAutonomy('read'); h.state.detailPageContext = () => ['later context'];
  create.resolve(response({ id: 'new', autonomy: 'auto' })); await pending;
  const payload = JSON.parse(h.requests.find(r => r.url.endsWith('/messages')).options.body);
  assert.equal(payload.autonomy, 'auto'); assert.equal(payload.context, 'accepted context');
  h.state.assistantDestroy();
});

test('audit32-ownership explicit same-thread reselection gets a fresh read owner after authentication', async () => {
  let authorized = false;
  const h = harness(url => url.endsWith('/a') ? response({ id: 'a' })
    : authorized ? response({ messages: [{ n: 1, text: 'authorized tape' }] }) : response({}, 401));
  await h.state._loadAssistantActivity(); assert.equal(h.state.assistantReadRetryable, false);
  authorized = true; await h.state.assistantSelectThread('a');
  assert.equal(h.state.assistantMessages[0]?.text, 'authorized tape');
  assert.equal(h.state.assistantReadError, ''); h.state.assistantDestroy();
});

test('audit32-ownership retired read decode failure cannot start recovery for a replacement', async () => {
  const body = deferred();
  const h = harness(url => url.includes('/a/') ? { ...response(), json: () => body.promise } : response({ messages: [{ n: 1, text: 'B' }] }));
  const old = h.state._loadAssistantActivity(); await flush();
  h.state._assistantRetireActivity(); h.state._assistantAdopt({ id: 'b' }, []);
  await h.state._loadAssistantActivity(); body.reject(Error('obsolete decode')); await old;
  assert.equal(h.state.assistantMessages[0].text, 'B'); assert.equal(h.state.assistantReadError, '');
  assert.equal(h.timers.size, 0); h.state.assistantDestroy();
});
