import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = name => fs.readFileSync(path.join(web, name), 'utf8');
const assistant = source('ui/assistant.js').replaceAll('export ', '');
const companion = source('ui/companion.js').replaceAll('export ', '');
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const response = (data = {}, etag = '', status = 200) => ({ ok: status === 200, status, headers: { get: () => etag }, json: async () => data });
const flush = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
const plain = value => JSON.parse(JSON.stringify(value));

function harness(fetcher = async () => response()) {
  const timers = new Map(), intervals = new Map(), frames = new Map(), listeners = new Set(), streams = [], requests = [], ticks = [];
  let serial = 0;
  const scroll = { scrollHeight: 1000, scrollTop: 940, clientHeight: 60 };
  const context = vm.createContext({
    AbortController, TextEncoder, Date, console,
    localStorage: { getItem: () => null, setItem() {} },
    document: { getElementById: () => scroll },
    window: { addEventListener: (name, cb) => listeners.add(cb), removeEventListener: (name, cb) => listeners.delete(cb) },
    setTimeout: (cb, ms) => { timers.set(++serial, { cb, ms }); return serial; }, clearTimeout: id => timers.delete(id),
    setInterval: (cb, ms) => { intervals.set(++serial, { cb, ms }); return serial; }, clearInterval: id => intervals.delete(id),
    requestAnimationFrame: cb => { frames.set(++serial, cb); return serial; }, cancelAnimationFrame: id => frames.delete(id),
    fetch: (url, options) => { requests.push({ url, options }); return fetcher(url, options); },
    EventSource: class {
      handlers = new Map(); closed = false;
      constructor() { streams.push(this); }
      addEventListener(name, cb) { this.handlers.set(name, cb); }
      close() { this.closed = true; }
      emit(name, data) { this.handlers.get(name)?.({ data: JSON.stringify(data) }); }
    },
  });
  vm.runInContext(assistant + '\n' + companion + '\nthis.state = Object.assign(kontoraAssistant(), kontoraCompanion()); this.stream = assistantStream;', context);
  const state = context.state;
  Object.assign(state, { assistantOpen: true, assistantAgent: { enabled: true }, assistantThread: { id: 'a' }, isSecondary: () => false,
    $nextTick: cb => ticks.push(cb), tickets: [], selectedTicket: null });
  return { state, requests, timers, intervals, frames, listeners, streams, context, stream: context.stream, scroll,
    tick() { for (const cb of ticks.splice(0)) cb?.(); },
    poll() { const entry = [...timers.entries()][0]; assert.ok(entry); timers.delete(entry[0]); entry[1].cb(); } };
}

for (const view of ['history', 'inbox', 'chat']) {
  test(`switching to ${view} retires the old activity flight without blocking new polls`, async () => {
    const old = deferred(), body = deferred(), next = deferred();
    const h = harness(url => url.includes('/activity')
      ? h.requests.filter(r => r.url.includes('/activity')).length === 1 ? old.promise : next.promise
      : response({ enabled: true, threads: [] }));
    h.state.companionSnapshot = { enabled: true };
    h.state.assistantView = view === 'chat' ? 'history' : 'thread';
    const pending = h.state._loadAssistantActivity();
    old.resolve({ ...response({}, 'stale'), json: () => body.promise });
    await flush();
    if (view === 'history') h.state.assistantShowHistory();
    else if (view === 'inbox') h.state.companionOpenInbox();
    else h.state.assistantShowChat();
    assert.equal(h.requests[0].options.signal.aborted, true);
    body.resolve({ running: false, messages: [{ n: 1, text: 'obsolete' }] });
    await pending;
    next.resolve(response({ running: true, messages: [{ n: 1, text: 'current' }] }, 'accepted'));
    await flush();
    assert.equal(h.state._assistantETag, 'accepted');
    assert.equal(h.state.assistantMessages[0].text, 'current');
    assert.equal(h.state.assistantView, view === 'chat' ? 'thread' : view);
    assert.equal(h.timers.size, 1);
    h.state.closeAssistant();
  });
}

for (const phase of ['headers', 'body', 'body failure', 'http failure']) {
  test(`retired ${phase} cannot touch a replacement flight or its validator`, async () => {
    const old = deferred(), body = deferred(), next = deferred();
    const h = harness(url => url.includes('/a/') ? old.promise : next.promise);
    h.state.assistantStreaming = true;
    h.state._assistantETag = 'a-tag';
    const pending = h.state._loadAssistantActivity();
    if (phase.startsWith('body')) { old.resolve({ ...response({}, 'stale'), json: () => body.promise }); await flush(); }
    h.state._assistantRetireActivity();
    h.state.assistantThread = { id: 'b' };
    h.state._assistantETag = 'b-tag';
    const replacement = h.state._loadAssistantActivity();
    assert.equal(h.requests[0].options.signal.aborted, true);
    if (phase === 'body failure') body.reject(new Error('decode'));
    else if (phase === 'body') body.resolve({ running: false, messages: [{ n: 1, text: 'OLD' }], gate: { id: 'old' } });
    else old.resolve(response({ error: 'old unauthorized' }, 'stale', phase === 'http failure' ? 401 : 200));
    await pending;
    assert.equal(h.state._assistantETag, 'b-tag');
    assert.equal(h.state.assistantMessages.length, 0);
    assert.equal(h.state.assistantGate, null);
    assert.equal(h.state.needsAuth, undefined);
    assert.equal(h.timers.size, 0);
    assert.equal(h.state._loadAssistantActivity(), replacement, 'old finalizer must not clear the replacement flight');
    next.resolve(response({ running: true }, 'accepted'));
    await replacement;
    assert.equal(h.state._assistantETag, 'accepted');
    assert.equal(h.timers.size, 1);
    h.state.closeAssistant();
  });
}

test('notifications coalesce behind one GET and follow up with accepted cursor and ETag', async () => {
  const first = deferred(), second = deferred();
  const h = harness(() => h.requests.length === 1 ? first.promise : second.promise);
  h.state._assistantETag = 'initial';
  const pending = h.state._loadAssistantActivity();
  for (let i = 0; i < 5; i++) assert.equal(h.state._loadAssistantActivity({ fresh: true }), pending);
  assert.equal(h.requests.length, 1);
  first.resolve(response({ running: true, tape: { events: [{ kind: 'text', text: 'first' }] } }, 'one'));
  await flush();
  assert.equal(h.requests.length, 2);
  assert.match(h.requests[1].url, /after=1$/);
  assert.equal(h.requests[1].options.headers['If-None-Match'], 'one');
  second.resolve(response({}, '', 304));
  await pending;
  assert.equal(h.state._assistantETag, 'one');
  assert.equal(h.timers.size, 1);
  h.state.closeAssistant();
});

for (const kind of ['send', 'approval', 'stop']) {
  test(`${kind} success refreshes after the write, not just the older GET`, async () => {
    const read = deferred(), write = deferred();
    const h = harness((url, options) => options?.method === 'POST' ? write.promise : h.requests.filter(r => !r.options?.method).length === 1 ? read.promise : response({ running: true }, 'new'));
    h.state.assistantDraft = 'hello';
    h.state.assistantGate = { id: 'g' };
    const pending = h.state._loadAssistantActivity();
    const writing = kind === 'send' ? h.state.sendAssistantMessage() : kind === 'approval' ? h.state.resolveAssistantGate(true) : h.state.stopAssistantTurn();
    await flush();
    assert.equal(h.requests.length, 2);
    write.resolve(response());
    await flush();
    assert.equal(h.requests.length, 2);
    read.resolve(response({ running: true, gate: { id: 'g' } }, 'old'));
    await Promise.all([pending, writing]);
    assert.equal(h.requests.length, 3);
    assert.equal(h.requests[2].options.headers['If-None-Match'], 'old');
    if (kind === 'approval') assert.equal(h.state.assistantGate, null);
    h.state.closeAssistant();
  });
}

for (const changed of [false, true]) {
  test(`approval failure restores actionable state only in original context (changed=${changed})`, async () => {
    const write = deferred();
    const h = harness(() => write.promise);
    const gate = { id: 'g', tool: 'write' };
    h.state.assistantGate = gate;
    const pending = h.state.resolveAssistantGate(false);
    assert.equal(h.state.assistantGate, null);
    h.state._mergeAssistantActivity({ running: true, gate });
    assert.equal(h.state.assistantGate, null, 'a pre-write GET cannot resurrect the card');
    if (changed) { h.state.closeAssistant(); h.state.assistantThread = { id: 'b' }; }
    write.reject(new Error('write failed'));
    await pending;
    assert.equal(h.state.assistantGate, changed ? null : gate);
    assert.equal(h.state.assistantError, changed ? null : 'write failed');
    h.state.closeAssistant();
  });
}

for (const failure of ['fetch', 'http', 'body']) {
  test(`failed New chat (${failure}) resumes the retained running lifecycle through 304`, async () => {
    const write = deferred();
    const h = harness((url, options) => options?.method === 'POST' ? write.promise : response({}, '', 304));
    h.state.assistantThread.running = true;
    h.state._assistantETag = 'running';
    h.state._assistantCursor = 4;
    h.state._assistantSetStreaming(true, Date.now());
    h.state._armAssistantPoll();
    const started = h.state._assistantTurnStart;
    const pending = h.state.assistantNewThread();
    if (failure === 'fetch') write.reject(new Error('creation failed'));
    else if (failure === 'http') write.resolve(response({ error: 'creation failed' }, '', 503));
    else write.resolve({ ...response(), json: async () => { throw new Error('creation failed'); } });
    assert.equal(await pending, null);
    await flush();
    assert.equal(h.state.assistantThread.id, 'a');
    assert.equal(h.state.assistantError, 'creation failed');
    assert.equal(h.state._assistantTurnStart, started);
    assert.equal(h.state.assistantStreaming, true);
    assert.equal(h.intervals.size, 1);
    assert.equal(h.streams.filter(s => !s.closed).length, 1);
    assert.equal(h.timers.size, 1);
    h.poll(); await flush();
    assert.equal(h.requests.at(-1).options.headers['If-None-Match'], 'running');
    assert.match(h.requests.at(-1).url, /after=4$/);
    assert.equal(h.timers.size, 1);
    h.state.closeAssistant();
  });
}

for (const reopenFirst of [false, true]) {
  test(`failed send after dismissal recovers through unchanged 304 (reopenFirst=${reopenFirst})`, async () => {
    const write = deferred();
    const h = harness((url, options) => options?.method === 'POST' ? write.promise : response({}, '', 304));
    h.state.assistantThread.running = false;
    h.state._assistantETag = 'idle';
    h.state.assistantDraft = 'not accepted';
    const pending = h.state.sendAssistantMessage();
    h.state.closeAssistant();
    if (reopenFirst) { h.state.toggleAssistant(); await flush(); }
    write.reject(new Error('send failed')); await pending;
    if (!reopenFirst) { h.state.toggleAssistant(); await flush(); }
    assert.equal(h.state.assistantStreaming, false);
    assert.equal(h.state.assistantThread.running, false);
    assert.equal(h.state.assistantError, 'send failed');
    assert.equal(h.state._assistantETag, 'idle');
    assert.ok(h.requests.filter(r => r.url.includes('/activity')).every(r => r.options.headers['If-None-Match'] === 'idle'));
    assert.equal(h.intervals.size, 0);
    assert.equal(h.streams.filter(s => !s.closed).length, 0);
    assert.equal(h.timers.size, 0);
    h.state.closeAssistant();
  });
}

for (const approve of [false, true]) {
  for (const intervening of [false, true]) {
    for (const reopenFirst of [false, true]) {
      test(`failed ${approve ? 'approve' : 'skip'} after dismissal restores pending gate through 304 (intervening200=${intervening}, reopenFirst=${reopenFirst})`, async () => {
        const write = deferred(), read = deferred();
        let held = false;
        const gate = { id: 'g', tool: 'write' };
        const h = harness((url, options) => {
          if (options?.method === 'POST') return write.promise;
          if (url.includes('/activity')) {
            if (held) { held = false; return read.promise; }
            return response({ running: true, gate }, 'gate-tag', options.headers['If-None-Match'] === 'gate-tag' ? 304 : 200);
          }
          return response({ id: 'a', running: true });
        });
        await h.state._loadAssistantActivity();
        let selecting;
        if (intervening) {
          held = true;
          selecting = h.state.assistantSelectThread('a');
          await flush();
        }
        const pending = h.state.resolveAssistantGate(approve);
        assert.equal(JSON.parse(h.requests.find(r => r.options?.method === 'POST').options.body).decision, approve ? 'approve' : 'skip');
        if (intervening) {
          read.resolve(response({ running: true, gate }, 'gate-tag'));
          await selecting;
          assert.equal(h.state.assistantGate, null, 'the older 200 must not resurrect a pending decision');
        }
        h.state.closeAssistant();
        if (reopenFirst) { h.state.toggleAssistant(); await flush(); }
        write.reject(new Error('decision failed')); await pending;
        if (!reopenFirst) { h.state.toggleAssistant(); await flush(); }
        assert.equal(h.state.assistantGate?.id, 'g');
        assert.equal(h.state.assistantError, 'decision failed');
        assert.equal(h.state._assistantETag, 'gate-tag');
        for (let i = 0; i < 2; i++) { h.poll(); await flush(); }
        assert.equal(h.state.assistantGate?.id, 'g');
        assert.equal(h.requests.filter(r => r.url.includes('/activity')).at(-1).options.headers['If-None-Match'], 'gate-tag');
        assert.equal(h.timers.size, 1);
        h.state.closeAssistant();
      });
    }
  }
}

for (const kind of ['new chat', 'send', 'approve', 'skip']) {
  for (const replacement of ['different thread', 'same thread']) {
    test(`failed ${kind} cannot modify a ${replacement} replacement`, async () => {
      const write = deferred(), read = deferred();
      const h = harness((url, options) => {
        if (options?.method === 'POST') return write.promise;
        if (url.includes('/activity')) return read.promise;
        return response({ id: replacement === 'same thread' ? 'a' : 'b', running: true, messages: [{ n: 1, text: 'replacement' }] });
      });
      h.state.assistantDraft = 'old send';
      h.state.assistantGate = { id: 'g' };
      const pending = kind === 'new chat' ? h.state.assistantNewThread() : kind === 'send' ? h.state.sendAssistantMessage() : h.state.resolveAssistantGate(kind === 'approve');
      h.state.closeAssistant();
      h.state.assistantOpen = true;
      const selecting = h.state.assistantSelectThread(replacement === 'same thread' ? 'a' : 'b');
      await flush();
      h.state.assistantError = 'replacement error';
      h.state.assistantGate = { id: 'replacement-gate' };
      h.state._assistantETag = 'replacement-tag';
      write.reject(new Error('stale failure')); await pending;
      assert.equal(h.state.assistantError, 'replacement error');
      assert.equal(h.state.assistantGate.id, 'replacement-gate');
      assert.equal(h.state.assistantStreaming, true);
      assert.equal(h.state.assistantMessages[0].text, 'replacement');
      assert.equal(h.state._assistantETag, 'replacement-tag');
      assert.equal(h.timers.size, 0, 'the pending replacement GET still owns the poll lane');
      read.resolve(response({}, '', 304)); await selecting;
      assert.equal(h.timers.size, 1);
      h.state.closeAssistant();
    });
  }
}

for (const approve of [false, true]) {
  for (const selected of ['a', 'b']) {
    for (const snapshot of ['same gate', 'new gate', 'no gate']) {
      test(`failed ${approve ? 'approve' : 'skip'} reconciles a reselected ${selected} with ${snapshot}`, async () => {
        const write = deferred();
        const gate = { id: 'g', tool: 'write' };
        let activity = { running: true, gate }, etag = 'original';
        const h = harness((url, options) => {
          if (options?.method === 'POST') return write.promise;
          if (!url.includes('/activity')) return response({ id: selected, running: true });
          return response(activity, etag, options.headers['If-None-Match'] === etag ? 304 : 200);
        });
        await h.state._loadAssistantActivity();
        const pending = h.state.resolveAssistantGate(approve);
        activity = { running: true, gate: snapshot === 'same gate' ? gate : snapshot === 'new gate' ? { id: 'new' } : null };
        etag = 'reselected';
        await h.state.assistantSelectThread(selected);
        h.state.assistantError = 'selected error';
        write.reject(new Error('old decision failed'));
        await pending;
        const expected = snapshot === 'same gate' ? 'g' : snapshot === 'new gate' ? 'new' : undefined;
        assert.equal(h.state.assistantGate?.id, expected);
        assert.equal(h.state.assistantError, 'selected error');
        assert.equal(h.state._assistantETag, 'reselected');
        for (let i = 0; i < 2; i++) { h.poll(); await flush(); }
        assert.equal(h.state.assistantGate?.id, expected);
        assert.equal(h.requests.at(-1).options.headers['If-None-Match'], 'reselected');
        h.state.closeAssistant();
      });
    }
  }
}

for (const kind of ['send', 'approve', 'skip']) {
  test(`failed ${kind} after dismissal cannot overwrite newer accepted server state`, async () => {
    const write = deferred();
    const h = harness((url, options) => options?.method === 'POST' ? write.promise : response({ running: true }, 'new-server-state'));
    h.state.assistantThread.running = false;
    h.state.assistantDraft = 'hello';
    h.state.assistantGate = { id: 'g' };
    const pending = kind === 'send' ? h.state.sendAssistantMessage() : h.state.resolveAssistantGate(kind === 'approve');
    h.state.closeAssistant();
    h.state.toggleAssistant(); await flush();
    write.reject(new Error('ambiguous failure')); await pending;
    assert.equal(h.state.assistantStreaming, true, 'an accepted running turn is not just optimistic state');
    assert.equal(h.state.assistantGate, null, 'a newer snapshot without the gate prevents restoration');
    assert.equal(h.state._assistantETag, 'new-server-state');
    assert.equal(h.streams.filter(s => !s.closed).length, 1);
    assert.equal(h.timers.size, 1);
    h.state.closeAssistant();
  });
}

for (const kind of ['send', 'approve', 'skip']) {
  for (const retire of ['selection pending', 'destroy']) {
    test(`failed ${kind} cannot mutate state after ${retire}`, async () => {
      const write = deferred(), selection = deferred();
      const h = harness((url, options) => options?.method === 'POST' ? write.promise : selection.promise);
      h.state.assistantDraft = 'hello';
      h.state.assistantGate = { id: 'g' };
      const pending = kind === 'send' ? h.state.sendAssistantMessage() : h.state.resolveAssistantGate(kind === 'approve');
      if (retire === 'destroy') h.state.assistantDestroy();
      else h.state.assistantSelectThread('b');
      h.state.assistantError = 'keep error';
      const retainedGate = h.state.assistantGate;
      write.reject(new Error('stale failure')); await pending;
      assert.equal(h.state.assistantError, 'keep error');
      assert.equal(h.state.assistantGate, retainedGate);
      assert.equal(h.timers.size, 0);
      h.state.assistantDestroy();
      selection.resolve(response({ id: 'b' })); await flush();
    });
  }
}

test('an older failed send cannot clear a newer optimistic send on the retained thread', async () => {
  const first = deferred(), second = deferred();
  let writes = 0;
  const h = harness((url, options) => options?.method === 'POST' ? (++writes === 1 ? first.promise : second.promise) : response({ running: false }, 'idle'));
  h.state.assistantDraft = 'first';
  const old = h.state.sendAssistantMessage();
  h.state.closeAssistant();
  h.state.toggleAssistant(); await flush();
  h.state.assistantDraft = 'second';
  const next = h.state.sendAssistantMessage();
  first.reject(new Error('old failure')); await old;
  assert.equal(h.state.assistantStreaming, true);
  assert.equal(h.state.assistantError, null);
  assert.equal(h.intervals.size, 1);
  second.reject(new Error('current failure')); await next;
  assert.equal(h.state.assistantStreaming, false);
  assert.equal(h.state.assistantError, 'current failure');
  h.state.closeAssistant();
});

for (const failure of ['fetch', 'body']) {
  test(`${failure} failure keeps polling with the old validator`, async () => {
    const h = harness(async () => {
      if (failure === 'fetch') throw new Error('network');
      return { ...response({}, 'bad'), json: async () => { throw new Error('invalid JSON'); } };
    });
    h.state.assistantStreaming = true;
    h.state._assistantETag = 'good';
    await h.state._loadAssistantActivity();
    assert.equal(h.state._assistantETag, 'good');
    assert.equal(h.timers.size, 1);
    h.poll(); await flush();
    assert.equal(h.requests.length, 2);
    assert.equal(h.timers.size, 1);
    h.state.closeAssistant();
  });
}

for (const action of ['closeAssistant', 'assistantDestroy', 'assistantEscape']) {
  test(`${action} retires poll, tick, stream, retry, frame, resize and late JSON without stopping the turn`, async () => {
    const body = deferred();
    const h = harness(async () => ({ ...response({}, 'late'), json: () => body.promise }));
    h.state.assistantDraft = 'keep draft';
    h.state._assistantSetStreaming(true, Date.now());
    h.state.assistantGate = { id: 'keep-approval' };
    h.state.startAssistantResize({ clientX: 10 });
    h.state._armAssistantPoll();
    h.streams[0].emit('delta', { gen: 0, text: 'queued' });
    h.streams[0].onerror();
    const pending = h.state._loadAssistantActivity(); await flush();
    assert.equal(h.frames.size, 1);
    h.state[action]();
    assert.equal(h.requests[0].options.signal.aborted, true);
    for (const resource of [h.timers, h.intervals, h.frames, h.listeners]) assert.equal(resource.size, 0);
    body.resolve({ running: true, partial: 'late', gate: { id: 'late' } });
    await pending; h.tick();
    assert.equal(h.state.assistantDraft, 'keep draft');
    assert.equal(h.state.assistantThread.id, 'a');
    assert.equal(h.state.assistantStreaming, true);
    assert.equal(h.state.assistantGate.id, 'keep-approval');
    assert.equal(h.state.assistantPartial, '');
    assert.equal(h.requests.length, 1, 'closing must send no server mutation');
    for (const resource of [h.timers, h.intervals, h.frames, h.listeners]) assert.equal(resource.size, 0);
  });
}

test('reopening a running thread reconciles exactly one tick, stream and poll', async () => {
  const h = harness(async url => response(url.endsWith('/threads') ? { threads: [] } : { running: true }));
  h.state._assistantSetStreaming(true, Date.now());
  h.state.closeAssistant();
  h.state.toggleAssistant();
  await flush();
  assert.equal(h.intervals.size, 1);
  assert.equal(h.streams.filter(s => !s.closed).length, 1);
  assert.equal(h.timers.size, 1);
  h.state._assistantResume(); await flush();
  assert.equal(h.intervals.size, 1);
  assert.equal(h.streams.filter(s => !s.closed).length, 1);
  h.state.assistantDestroy();
});

for (const selection of ['thread', 'new thread']) {
  test(`delayed ${selection} selection cannot adopt after dismissal`, async () => {
    const body = deferred();
    const h = harness(async () => ({ ...response(), json: () => body.promise }));
    const pending = selection === 'thread' ? h.state.assistantSelectThread('b') : h.state.assistantNewThread();
    await flush(); h.state.closeAssistant();
    body.resolve({ id: 'b', running: true });
    await pending;
    assert.equal(h.state.assistantThread.id, 'a');
    assert.equal(h.streams.length, 0);
    assert.equal(h.timers.size, 0);
  });
}

test('companion refresh preserves the validator and shares the activity lane', async () => {
  const read = deferred();
  const h = harness(async url => url.includes('/activity') ? read.promise : response({ enabled: true, revision: 2 }));
  h.state.assistantThread.purpose = 'companion';
  h.state._assistantETag = 'evidence';
  const pending = h.state._loadAssistantActivity();
  await h.state.companionRefresh();
  assert.equal(h.requests.filter(r => r.url.includes('/activity')).length, 1);
  read.resolve(response({}, '', 304));
  await pending;
  const reads = h.requests.filter(r => r.url.includes('/activity'));
  assert.equal(reads.length, 2);
  assert.ok(reads.every(r => r.options.headers['If-None-Match'] === 'evidence'));
  h.state.closeAssistant();
});

test('companion followup shares a pending GET and preserves loaded history, cursor and validator', async () => {
  const read = deferred(), write = deferred();
  const h = harness((url, options) => {
    if (options?.method === 'POST') return write.promise;
    if (url.includes('/activity')) return h.requests.filter(r => r.url.includes('/activity')).length === 1 ? read.promise : Promise.resolve(response({ offset: 1, running: true }, 'after-write'));
    return Promise.resolve(response({ threads: [] }));
  });
  h.state.companionSnapshot = { enabled: true, ai_available: true };
  h.state.assistantThread.purpose = 'companion';
  h.state.assistantEvents = [{ kind: 'tool', id: 'keep' }];
  h.state._assistantCursor = 1;
  h.state._assistantETag = 'before-write';
  h.state.assistantWindow = 600;
  h.state.assistantExpanded.keep = true;
  const pending = h.state._loadAssistantActivity();
  const sending = h.state.companionSend('followup', 'continue', []);
  write.resolve(response({ id: 'a', purpose: 'companion', running: true }));
  await flush();
  assert.equal(h.requests.filter(r => r.url.includes('/activity')).length, 1);
  assert.equal(h.requests[0].options.signal.aborted, false);
  read.resolve(response({ offset: 1, running: true }, 'accepted-before-write'));
  await Promise.all([pending, sending]);
  const reads = h.requests.filter(r => r.url.includes('/activity'));
  assert.equal(reads.length, 2);
  assert.match(reads[1].url, /after=1$/);
  assert.equal(reads[1].options.headers['If-None-Match'], 'accepted-before-write');
  assert.equal(h.state.assistantEvents.length, 1);
  assert.equal(h.state.assistantWindow, 600);
  assert.equal(h.state.assistantExpanded.keep, true);
  h.state.closeAssistant();
});

for (const kind of ['send', 'approval']) {
  test(`${kind} refresh starts after a slow write even when the older GET already settled`, async () => {
    const write = deferred();
    const h = harness(async (url, options) => options?.method === 'POST' ? write.promise : response({ running: true }, 'accepted'));
    h.state.assistantDraft = 'hello';
    h.state.assistantGate = { id: 'g' };
    const read = h.state._loadAssistantActivity();
    const pending = kind === 'send' ? h.state.sendAssistantMessage() : h.state.resolveAssistantGate(true);
    await read;
    assert.equal(h.requests.length, 2);
    write.resolve(response()); await pending;
    assert.equal(h.requests.length, 3);
    assert.equal(h.requests[2].options.headers['If-None-Match'], 'accepted');
    h.state.closeAssistant();
  });
}

test('a retired stream frame cannot clear a replacement stream buffer', () => {
  const h = harness();
  h.state._assistantSetStreaming(true);
  h.streams[0].emit('delta', { gen: 0, text: 'old' });
  const stale = [...h.frames.values()][0];
  h.state.closeAssistant();
  h.state.assistantOpen = true;
  h.state._assistantOpenStream();
  h.streams[1].emit('delta', { gen: 0, text: 'new' });
  stale();
  assert.equal(h.stream.buf, 'new');
  assert.equal(h.stream.queued, true);
  h.state.closeAssistant();
});

test('a companion write failure cannot clear a newer send or report into its context', async () => {
  const first = deferred(), second = deferred();
  const h = harness(() => h.requests.length === 1 ? first.promise : second.promise);
  h.state.companionSnapshot = { enabled: true, ai_available: true };
  h.state.assistantThread.purpose = 'companion';
  const old = h.state.companionSend('followup', 'old', []);
  h.state.closeAssistant();
  h.state.assistantOpen = true;
  const next = h.state.companionSend('followup', 'next', []);
  first.reject(new Error('old error')); await old;
  assert.equal(h.state.companionBusy, true);
  assert.equal(h.state.assistantError, null);
  assert.equal(h.state.companionError, '');
  second.reject(new Error('current error')); await next;
  assert.equal(h.state.companionBusy, false);
  assert.equal(h.state.assistantError, 'current error');
  h.state.closeAssistant();
});

test('history windows 200 mixed rows at a time without changing absolute keys or full arrays', () => {
  const h = harness();
  const events = Array.from({ length: 500 }, (_, i) => ({ kind: 'text', text: String(i), time: new Date(10000 + i * 1000).toISOString() }));
  const messages = Array.from({ length: 10 }, (_, i) => ({ n: i + 1, text: String(i), at: new Date(10000 + i * 50000).toISOString() }));
  h.state.assistantEvents = events; h.state.assistantMessages = messages;
  const first = h.state.assistantThreadRows();
  assert.equal(first.length, 200);
  assert.equal(h.state.assistantThreadRows(), first, 'same identities reuse the window');
  h.state.assistantPartial = 'tokens';
  assert.equal(h.state.assistantThreadRows(), first, 'partials do not rebuild history');
  const last = first.at(-1);
  h.scroll.scrollTop = 100;
  h.state.assistantLoadEarlier(); h.scroll.scrollHeight += 600; h.tick();
  assert.equal(h.scroll.scrollTop, 700);
  const second = h.state.assistantThreadRows();
  assert.equal(second.length, 400);
  assert.equal(second.at(-1), last);
  h.state.assistantLoadEarlier();
  assert.equal(h.state.assistantThreadRows().length, 510);
  assert.equal(h.state.assistantThreadRows().at(-1).key, 'e499');
  assert.equal(h.state.assistantEvents, events);
  assert.equal(h.state.assistantMessages, messages);
});

test('untimed fallback and optimistic messages retain ordering; late tool results keep keys and expansion', () => {
  const h = harness();
  h.state.assistantEvents = [{ kind: 'tool', id: 'call', result: '' }, { kind: 'text', text: 'answer' }];
  h.state.assistantMessages = [{ n: 1, text: 'old' }, { n: 2, text: 'pending' }];
  assert.deepEqual(plain(h.state.assistantThreadRows()).map(r => r.key), ['m1-0', 'm2-1', 'e0', 'e1']);
  h.state.toggleAssistantTool(h.state.assistantEvents[0], 0);
  h.state._mergeAssistantActivity({ offset: 0, tape: { events: [{ kind: 'tool', id: 'call', result: 'done' }, { kind: 'text', text: 'answer' }] }, messages: [{ n: 1, text: 'old' }] });
  assert.equal(h.state.assistantToolExpanded(h.state.assistantEvents[0], 0), true);
  assert.equal(h.state.assistantMessages[1].text, 'pending');
  assert.equal(h.state.assistantThreadRows()[2].key, 'e0');
  const rows = h.state.assistantThreadRows();
  h.state._mergeAssistantActivity({ offset: 2, messages: [{ n: 1, text: 'old' }] });
  assert.equal(h.state.assistantThreadRows(), rows, 'empty suffix and unchanged messages reuse arrays');
  h.state._mergeAssistantActivity({ offset: 2, messages: [{ n: 1, text: 'old', error: 'failed' }] });
  assert.equal(h.state.assistantMessages[0].error, 'failed');
});

test('a reader holding older history keeps the visible start when new rows arrive', () => {
  const h = harness();
  h.state.assistantEvents = Array.from({ length: 500 }, () => ({ kind: 'text' }));
  const start = h.state.assistantThreadRows()[0].key;
  h.state._assistantActivityLoaded = true;
  h.scroll.scrollTop = 10;
  h.state._mergeAssistantActivity({ offset: 500, tape: { events: [{ kind: 'tool', id: 'late' }] } });
  assert.equal(h.state.assistantThreadRows()[0].key, start);
  assert.equal(h.state.assistantThreadRows().length, 201);
});

test('shipped desktop tool bodies use x-if, and both histories expose load-earlier controls', () => {
  const html = source('static/index.html');
  assert.match(html, /<template x-if="assistantToolExpanded\(row.event, row.index\)">/);
  assert.doesNotMatch(html, /<div x-show="assistantToolExpanded/);
  assert.equal((html.match(/@click="assistantLoadEarlier\(\)"/g) || []).length, 2);
});

// Opt-in because ordinary Go tests do not install browser dependencies.
test('actual Alpine mounts the shipped pane, memoizes proxy reads and defers tool bodies', { skip: process.env.KONTORA_ASSISTANT_BROWSER !== '1' }, async () => {
  const { chromium } = await import('playwright');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const html = source('static/index.html');
    const begin = html.indexOf('<template x-if="!isMobile && assistantOpen && !assistantRail">');
    const end = html.indexOf('</aside>', begin) + '</aside>'.length;
    const pane = html.slice(begin, end) + '</template>';
    await page.setContent(`<div id="root" x-data="fixture()">${pane}</div>`);
    await page.addScriptTag({ content: assistant + '\n' + companion + `
      window.fixture = () => {
        const state = Object.assign(kontoraAssistant(), kontoraCompanion(), {
          assistantOpen: true, assistantAgent: { enabled: true }, assistantThread: { id: 'a' }, isMobile: false,
          selectedTicket: null, tickets: [], isSecondary: () => false,
          setProse(el, text) { el.textContent = text; },
          assistantEvents: Array.from({ length: 500 }, (_, i) => ({ kind: 'tool', id: 't' + i, tool: 'Read', result: 'RESULT-' + i })),
          assistantMessages: Array.from({ length: 10 }, (_, i) => ({ n: i + 1, text: 'MESSAGE-' + i })),
        });
        return state;
      };
      window.fetch = async () => ({ ok: true, status: 200, json: async () => ({ running: true, offset: 500 }), headers: { get: () => 'tag' } });
      window.EventSource = class { addEventListener() {} close() {} };
    ` });
    const vendor = fs.readdirSync(path.join(web, 'static/vendor')).find(n => n.startsWith('alpinejs@'));
    const alpinePath = path.join(web, 'static/vendor', vendor);
    const alpineFile = fs.readdirSync(alpinePath).find(n => n.endsWith('.js'));
    await page.addScriptTag({ content: fs.readFileSync(path.join(alpinePath, alpineFile), 'utf8') });
    await page.waitForFunction(() => document.querySelectorAll('.assistant-tool-row').length === 200);
    assert.equal(await page.locator('.assistant-tool-row pre').count(), 0);
    const identity = await page.evaluate(() => {
      const a = Alpine.$data(document.getElementById('root'));
      const other = Alpine.mergeProxies([a]);
      const before = a.assistantThreadRows();
      a.assistantPartial = 'token';
      return Alpine.raw(before) === Alpine.raw(other.assistantThreadRows());
    });
    assert.equal(identity, true, 'different expression proxies reuse history by raw array identity');
    await page.locator('.assistant-tool-row button').first().click();
    assert.equal(await page.locator('.assistant-tool-row pre').count(), 1);
    const result = await page.locator('.assistant-tool-row pre').textContent();
    await page.getByRole('button', { name: 'Load earlier messages and events' }).click();
    await page.waitForFunction(() => document.querySelectorAll('.assistant-tool-row').length === 400);
    assert.equal(await page.locator('.assistant-tool-row pre').textContent(), result);
    const aborted = await page.evaluate(async () => {
      const a = Alpine.$data(document.getElementById('root'));
      window.fetch = async (url, options) => {
        window.pendingSignal = options.signal;
        return { ok: true, status: 200, headers: { get: () => 'late' },
          json: () => new Promise(resolve => { window.releaseActivity = resolve; }) };
      };
      a._assistantSetStreaming(true, Date.now());
      window.pendingActivity = a._loadAssistantActivity();
      while (!window.releaseActivity) await Promise.resolve();
      a.closeAssistant();
      return window.pendingSignal instanceof AbortSignal && window.pendingSignal.aborted;
    });
    assert.equal(aborted, true, 'native abort handles remain outside Alpine proxies');
    await page.evaluate(async () => {
      window.releaseActivity({ running: true, gate: { id: 'late' }, partial: 'late' });
      await window.pendingActivity;
    });
    await page.waitForFunction(() => !document.getElementById('assistant-pane'));
    const retired = await page.evaluate(() => {
      const a = Alpine.$data(document.getElementById('root'));
      return [a.assistantStreaming, a._assistantPoll, a._assistantTick, assistantStream.es, assistantStream.raf, assistantStream.retry, a.assistantGate];
    });
    assert.deepEqual(retired, [true, null, null, null, null, null, null]);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});

test('actual Alpine recovers failed writes without changing replacement panes', { skip: process.env.KONTORA_ASSISTANT_BROWSER !== '1' }, async t => {
  const { chromium } = await import('playwright');
  const browser = await chromium.launch({ headless: true });
  const html = source('static/index.html');
  const begin = html.indexOf('<template x-if="!isMobile && assistantOpen && !assistantRail">');
  const pane = html.slice(begin, html.indexOf('</aside>', begin) + '</aside>'.length) + '</template>';
  const alpine = source('static/vendor/alpinejs@3.14.8/cdn.min.js');
  const scenarios = [{ kind: 'new chat' }];
  for (const kind of ['send', 'approve', 'skip']) {
    for (const reopenFirst of [false, true]) {
      for (const intervening of kind === 'send' ? [false] : [false, true]) scenarios.push({ kind, reopenFirst, intervening });
    }
  }
  for (const kind of ['new chat', 'send', 'approve', 'skip']) {
    for (const replacement of ['a', 'b']) scenarios.push({ kind, replacement });
  }
  try {
    for (const scenario of scenarios) {
      await t.test(JSON.stringify(scenario), async () => {
        const page = await browser.newPage();
        const errors = [];
        page.on('pageerror', error => errors.push(error.message));
        try {
          await page.setContent(`<div id="root" x-data="fixture()">${pane}</div>`);
          await page.addScriptTag({ content: assistant + '\n' + companion + `
            window.fixture = () => Object.assign(kontoraAssistant(), kontoraCompanion(), {
              assistantOpen: true, assistantAgent: { enabled: true }, assistantThread: { id: 'a' }, isMobile: false,
              selectedTicket: null, tickets: [], isSecondary: () => false,
              setProse(el, text) { el.textContent = text; },
            });
          ` });
          await page.addScriptTag({ content: alpine });
          const result = await page.evaluate(async ({ kind, reopenFirst, intervening, replacement }) => {
            const s = Alpine.$data(document.getElementById('root'));
            const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
            const response = (data, tag = '', status = 200) => ({ ok: status === 200, status, headers: { get: () => tag }, json: async () => structuredClone(data) });
            const flush = async () => {
              for (let i = 0; i < 20; i++) await Promise.resolve();
              await Alpine.nextTick();
              await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
            };
            const write = deferred(), read = deferred(), timers = new Map(), intervals = new Map(), streams = [], reads = [];
            let serial = 0, held = false;
            const nativeTimeout = window.setTimeout, nativeClear = window.clearTimeout;
            window.setTimeout = (cb, ms, ...args) => {
              if (ms !== 1500) return nativeTimeout(cb, ms, ...args);
              const id = 'poll-' + ++serial; timers.set(id, cb); return id;
            };
            window.clearTimeout = id => { if (!timers.delete(id)) nativeClear(id); };
            window.setInterval = cb => { intervals.set(++serial, cb); return serial; };
            window.clearInterval = id => intervals.delete(id);
            window.EventSource = class {
              closed = false;
              constructor() { streams.push(this); }
              addEventListener() {}
              close() { this.closed = true; }
            };
            const approval = kind === 'approve' || kind === 'skip';
            let tag = 'initial', body = { running: kind !== 'send', messages: [] };
            if (approval) body.gate = { id: 'g', kind: 'write', tool: 'Bash', arg: 'kontora move T done' };
            window.fetch = (url, options) => {
              if (options?.method === 'POST') return write.promise;
              if (url.includes('/activity')) {
                const validator = options.headers['If-None-Match'];
                reads.push({ validator, status: validator === tag ? 304 : 200 });
                if (held) { held = false; return read.promise; }
                return Promise.resolve(response(body, tag, validator === tag ? 304 : 200));
              }
              return Promise.resolve(response(url.endsWith('/threads') ? { threads: [] } : { id: replacement || 'a', running: body.running, messages: [] }));
            };
            await s._loadAssistantActivity();
            if (kind === 'new chat') { s._assistantTurnStart = Date.now(); s._startAssistantTick(); }
            let selecting;
            if (intervening) {
              held = true;
              selecting = s.assistantSelectThread('a'); await flush();
            }
            s.assistantDraft = 'not accepted';
            const pending = kind === 'new chat' ? s.assistantNewThread() : kind === 'send' ? s.sendAssistantMessage() : s.resolveAssistantGate(kind === 'approve');
            let suppressed = s.assistantGate === null;
            if (intervening) {
              read.resolve(response(body, tag)); await selecting;
              suppressed = suppressed && s.assistantGate === null;
            }
            if (replacement) {
              s.closeAssistant();
              tag = 'replacement'; body = { running: true, messages: [{ n: 1, text: 'replacement' }], gate: { id: 'replacement-gate' } };
              s.assistantOpen = true; await s.assistantSelectThread(replacement);
              s.assistantError = 'replacement error';
            } else if (kind !== 'new chat') {
              s.closeAssistant();
              if (reopenFirst) { s.toggleAssistant(); await flush(); }
            }
            write.reject(new TypeError('request failed')); await pending;
            if (!replacement && kind !== 'new chat' && !reopenFirst) s.toggleAssistant();
            await flush();
            for (let i = 0; i < 2; i++) {
              const entry = [...timers.entries()][0];
              if (!entry) break;
              timers.delete(entry[0]); entry[1](); await flush();
            }
            s.assistantDraft = 'retry'; await flush();
            const snapshot = {
              id: s.assistantThread.id, streaming: s.assistantStreaming, gate: s.assistantGate?.id || null,
              error: s.assistantError, tag: s._assistantETag, polls: timers.size, ticks: intervals.size,
              streams: streams.filter(es => !es.closed).length, suppressed, reads,
              sendVisible: document.getElementById('assistant-send').style.display !== 'none',
              sendDisabled: document.getElementById('assistant-send').disabled,
              approvalVisible: !!document.getElementById('assistant-approve'),
            };
            s.assistantDestroy(); await flush();
            snapshot.retired = [timers.size, intervals.size, streams.filter(es => !es.closed).length];
            return snapshot;
          }, scenario);
          const replacement = !!scenario.replacement;
          assert.equal(result.id, scenario.replacement || 'a');
          assert.equal(result.streaming, replacement || scenario.kind !== 'send');
          assert.equal(result.gate, replacement ? 'replacement-gate' : ['approve', 'skip'].includes(scenario.kind) ? 'g' : null);
          assert.equal(result.error, replacement ? 'replacement error' : 'request failed');
          assert.equal(result.tag, replacement ? 'replacement' : 'initial');
          assert.equal(result.polls, result.streaming ? 1 : 0);
          assert.equal(result.streams, result.streaming ? 1 : 0);
          if (scenario.kind === 'new chat' && !replacement) assert.equal(result.ticks, 1);
          assert.equal(result.suppressed, true);
          assert.equal(result.sendVisible, !result.streaming);
          assert.equal(result.sendDisabled, false);
          assert.equal(result.approvalVisible, result.gate !== null);
          assert.deepEqual(result.retired, [0, 0, 0]);
          assert.equal(result.reads.at(-1).status, 304);
          assert.deepEqual(errors, []);
        } finally { await page.close(); }
      });
    }
  } finally { await browser.close(); }
});
