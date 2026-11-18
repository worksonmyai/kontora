import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { kontoraAssistant } from '../ui/assistant.js';
import { kontoraAssistantMarkup } from '../ui/assistant_markup.js';

const html = fs.readFileSync(new URL('../static/index.html', import.meta.url), 'utf8');
const fragments = kontoraAssistantMarkup();
const assistant = () => Object.assign(kontoraAssistant(), {
  companionEnabled: () => false,
  companionThread: () => false,
  parseFilterQuery: value => value,
  ticketMatchesQuery: (ticket, query) => ticket.title.includes(query),
});
const alpine = vm.createContext({
  window: { Element: class {} },
  document: { createElement: () => ({ style: {} }), addEventListener() {} },
  MutationObserver: class { observe() {} }, queueMicrotask() {},
});
vm.runInContext(fs.readFileSync(new URL('../static/vendor/alpinejs@3.14.8/cdn.min.js', import.meta.url), 'utf8'), alpine);
alpine.queueMicrotask = queueMicrotask;

const press = (state, init = {}) => {
  const result = { prevented: false, stopped: false };
  state.assistantComposerKey({ key: 'Enter', preventDefault() { result.prevented = true; }, stopPropagation() { result.stopped = true; }, ...init });
  return result;
};

test('Assistant shells mount constant fragments and keep exclusive scroll hosts', () => {
  const hosts = [...html.matchAll(/x-html="(assistant\w+Markup)"/g)].map(match => match[1]);
  assert.deepEqual(hosts, [
    'assistantContextMarkup', 'assistantHistoryMarkup', 'assistantTranscriptMarkup', 'assistantConfigureMarkup', 'assistantComposerMarkup',
    'assistantContextMarkup', 'assistantHistoryMarkup', 'assistantSheetTranscriptMarkup', 'assistantSheetConfigureMarkup', 'assistantSheetComposerMarkup',
  ]);
  for (const name of hosts) assert.equal(typeof fragments[name], 'string', name);
  for (const descriptor of Object.values(Object.getOwnPropertyDescriptors(fragments))) assert.equal(descriptor.get, undefined);
  assert.match(html, /x-if="!isMobile && assistantOpen && !assistantRail"/);
  assert.match(html, /x-if="isMobile && sheet.type==='assistant'"/);
  for (const [id, name] of [['assistant-thread', 'assistantTranscriptMarkup'], ['assistant-sheet-thread', 'assistantSheetTranscriptMarkup']]) {
    const host = html.match(new RegExp(`<div id="${id}"[^>]*>[\\s\\S]*?</div>`))?.[0];
    assert.ok(host, id);
    assert.match(host, /@scroll\.passive="assistantThreadScrolled\(\)"/);
    assert.match(host, new RegExp(`x-html="${name}"`));
  }
  assert.doesNotMatch(html, /x-for="row in assistantThreadRows\(\)"/);
});

for (const prefix of ['', 'Sheet']) {
  test(`shared ${prefix || 'desktop'} transcript restores recorded details and stable stream blocks`, () => {
    const transcript = fragments[`assistant${prefix}TranscriptMarkup`];
    for (const binding of ['row.msg.error', 'row.event.model', 'assistantEventTime(row.event)', 'assistantGate.tool', 'assistantGate.arg', 'row.event.result || row.event.summary']) {
      assert.ok(transcript.includes(binding), binding);
    }
    assert.match(transcript, /@click="toggleAssistantTool\(row.event, row.index\)"/);
    assert.match(transcript, /:aria-expanded="assistantToolExpanded\(row.event, row.index\)/);
    assert.match(transcript, /<template x-if="assistantToolExpanded\(row.event, row.index\)">/);
    assert.doesNotMatch(transcript, /x-show="assistantToolExpanded/);
    assert.match(transcript, /x-html="companionEvidenceMarkup"/);
    assert.match(transcript, /x-effect="setProse\(\$el, row.event.text\)"/);
    const partial = transcript.indexOf('x-if="assistantPartial"');
    const working = transcript.indexOf('x-if="assistantStreaming && !assistantGate"');
    assert.ok(transcript.indexOf('x-for="row in assistantThreadRows()"') < partial);
    assert.ok(partial < working);
    assert.doesNotMatch(transcript.slice(partial, working), /setProse|x-html|x-for/);
    assert.match(transcript, new RegExp(`id="assistant-${prefix ? 'sheet-' : ''}partial"`));
    assert.match(transcript, new RegExp(`id="assistant-${prefix ? 'sheet-' : ''}partial-tool"`));
    assert.match(transcript, /@click="resolveAssistantGate\(true\)"/);
    assert.match(transcript, /@click="resolveAssistantGate\(false\)"/);
  });

  test(`shared ${prefix || 'desktop'} composer exposes suggestions, autonomy hints and configured agent`, () => {
    const composer = fragments[`assistant${prefix}ComposerMarkup`];
    for (const binding of ['assistantDraftChanged()', 'assistantComposerKey($event)', 'assistantCompositionStart()', 'assistantCompositionEnd()', 'assistantCompositionReset()', 'acceptAssistantMenu(row)', 'mode.hint', 'assistantAgent.agent', 'sendAssistantMessage()', 'stopAssistantTurn()']) {
      assert.ok(composer.includes(binding), binding);
    }
    assert.match(composer, /role="listbox"/);
    assert.match(composer, /role="option"/);
    assert.match(composer, /:aria-activedescendant=/);
    assert.match(composer, new RegExp(`id="assistant-${prefix ? 'sheet-' : ''}input"`));
    const configure = fragments[`assistant${prefix}ConfigureMarkup`];
    assert.match(configure, /assistantAgent.hint/);
    assert.match(configure, /gotoView\('settings'\)/);
  });
}

test('shared history has counts and a visible delete control; context keeps all chips', () => {
  assert.match(fragments.assistantHistoryMarkup, /t.turns/);
  assert.match(fragments.assistantHistoryMarkup, /t.writes/);
  assert.match(fragments.assistantHistoryMarkup, /@click="assistantDeleteThread\(t.id\)"/);
  assert.doesNotMatch(fragments.assistantHistoryMarkup, /opacity-0|hover:opacity/);
  assert.match(fragments.assistantHistoryMarkup, /:aria-label="'Delete ' \+ t.title"/);
  assert.match(fragments.assistantContextMarkup, /x-for="chip in assistantContext"/);
  assert.match(fragments.assistantContextMarkup, /!companionThread\(\)/);
});

test('recorded tool expansion survives late results and streamed text updates', () => {
  const state = assistant();
  const event = { kind: 'tool', id: 'call-1', tool: 'bash', arg: 'false', time: '2026-11-16T10:00:00Z' };
  state.assistantEvents = [event];
  state.toggleAssistantTool(event, 0);
  const keys = state.assistantThreadRows().map(row => row.key);
  state.assistantEvents = [{ ...event, result: 'command failed', is_error: true }];
  state.assistantPartial = 'Still explaining';
  assert.deepEqual(state.assistantThreadRows().map(row => row.key), keys);
  assert.equal(state.assistantToolExpanded(state.assistantEvents[0], 0), true);
  assert.equal(state.assistantToolFailed(state.assistantEvents[0]), true);
  assert.match(state.assistantEventTime(event), /^\d{2}:\d{2}:\d{2}$/);
  state.toggleAssistantTool(event, 0);
  assert.equal(state.assistantToolExpanded(event, 0), false);
  assert.deepEqual(kontoraAssistantMarkup(), fragments);
});

for (const init of [{ isComposing: true }, { keyCode: 229 }, { key: 'Tab', isComposing: true }, { key: 'ArrowDown', keyCode: 229 }]) {
  test(`composition does not send or select suggestions: ${JSON.stringify(init)}`, () => {
    const state = assistant();
    let sent = 0;
    state.sendAssistantMessage = () => sent++;
    for (const draft of ['a message', '/st']) {
      state.assistantDraft = draft;
      state.assistantDraftChanged();
      const before = state.assistantMenu;
      const result = press(state, init);
      assert.equal(sent, 0);
      assert.equal(result.prevented, false, 'the input method keeps its confirmation key');
      assert.equal(state.assistantMenu, before);
      assert.equal(state.assistantDraft, draft);
    }
  });
}

test('composition state covers browsers that end composition before Enter', async () => {
  const state = assistant();
  let sent = 0;
  state.sendAssistantMessage = () => sent++;
  state.assistantDraft = '/st';
  state.assistantDraftChanged();
  state.assistantCompositionStart();
  assert.equal(state.assistantMenu, null);
  state.assistantDraftChanged();
  assert.equal(state.assistantMenu, null);
  assert.equal(press(state).prevented, false);
  state.assistantCompositionEnd();
  assert.equal(press(state).prevented, false);
  assert.equal(sent, 0);
  await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(state.assistantMenu.kind, 'slash');
  press(state);
  assert.equal(sent, 0, 'the first ordinary Enter chooses a suggestion');
  assert.equal(state.assistantMenu, null);
  press(state);
  assert.equal(sent, 1, 'the next ordinary Enter sends');
  state.assistantCompositionStart();
  state.assistantCompositionEnd();
  state.assistantCompositionReset();
  assert.equal(state._assistantCompositionTimer, null);
  assert.equal(state._assistantComposing, false);
  assert.equal(state._assistantCompositionEnding, false);
});

test('suggestion navigation, selection, dismissal and sending stay distinct', () => {
  const state = assistant();
  let sent = 0;
  state.sendAssistantMessage = () => sent++;
  state.assistantDraft = '/st';
  state.assistantDraftChanged();
  assert.equal(press(state, { shiftKey: true }).prevented, false);
  assert.equal(state.assistantDraft, '/st', 'Shift+Enter is not suggestion selection');
  press(state, { key: 'ArrowDown' });
  assert.equal(state.assistantMenu.index, 1);
  press(state, { key: 'ArrowUp' });
  assert.equal(state.assistantMenu.index, 0);
  press(state, { key: 'Tab' });
  assert.match(state.assistantDraft, /running right now/);
  assert.equal(sent, 0);
  state.tickets = [{ id: 'kon-test', title: 'find me' }];
  state.assistantDraft = 'Inspect @find';
  state.assistantDraftChanged();
  state.acceptAssistantMenu(state.assistantMenu.rows[0]);
  assert.equal(state.assistantDraft, 'Inspect @kon-test ');
  assert.equal(sent, 0);
  state.assistantDraft = '/st';
  state.assistantDraftChanged();
  assert.deepEqual(press(state, { key: 'Escape' }), { prevented: true, stopped: true });
  assert.equal(state.assistantMenu, null);
  press(state);
  assert.equal(sent, 1);
});

test('launch labels summarize approval, work and unread Inbox without mutations', () => {
  const state = assistant();
  assert.equal(state.assistantLaunchLabel(), '');
  assert.equal(state.assistantLaunchHint(), 'Open Assistant');
  state.companionEnabled = () => true;
  state.companionSnapshot = { items: [{ read: true }, { read: false }, { read: false, active: false }] };
  assert.equal(state.assistantLaunchLabel(), 'Inbox 2');
  assert.equal(state.assistantLaunchHint(), 'Assistant: Inbox 2 unread');
  state.assistantStreaming = true;
  assert.equal(state.assistantLaunchLabel(), 'Working');
  state.assistantGate = { id: 'gate-1', tool: 'bash' };
  assert.equal(state.assistantLaunchLabel(), 'Approval');
  assert.equal(state.assistantLaunchHint(), 'Assistant: waiting for your approval');
  assert.equal(state.assistantOpen, false);
  assert.equal(state.assistantThread, null);
  state.assistantGate = null;
  state.assistantStreaming = false;
  state.companionEnabled = () => false;
  assert.equal(state.assistantLaunchLabel(), '', 'disabled Inbox does not leak stale counts');
});

test('closing the Assistant leaves the draft, approval and server turn intact', () => {
  const state = assistant();
  state.assistantOpen = true;
  state.assistantStreaming = true;
  state.assistantThread = { id: 'thread-1', running: true };
  state.assistantGate = { id: 'gate-1', tool: 'bash' };
  state.assistantDraft = 'Keep this draft';
  state._assistantFetch = () => assert.fail('closing must not send a stop or approval request');
  state.closeAssistant();
  state._stopAssistantPoll();
  assert.equal(state.assistantOpen, false);
  assert.equal(state.assistantStreaming, true);
  assert.equal(state.assistantThread.running, true);
  assert.equal(state.assistantGate.id, 'gate-1');
  assert.equal(state.assistantDraft, 'Keep this draft');
});

test('touch suggestion selection returns focus to the existing composer', () => {
  const state = assistant();
  state.isMobile = true;
  state.assistantDraft = '/st';
  state.assistantDraftChanged();
  state.$nextTick = fn => fn();
  const original = globalThis.document;
  let focused = '';
  globalThis.document = { getElementById: id => ({ focus() { focused = id; } }) };
  try {
    state.acceptAssistantMenu(state.assistantMenu.rows[0]);
    assert.equal(focused, 'assistant-sheet-input');
  } finally {
    if (original === undefined) delete globalThis.document;
    else globalThis.document = original;
  }
});

for (const isMobile of [true, false]) {
  for (const ending of [false, true]) {
    test(`breakpoint resets ${ending ? 'ending' : 'active'} composition without blur from ${isMobile ? 'phone' : 'desktop'}`, async t => {
      const state = alpine.window.Alpine.reactive(assistant());
      const watches = [];
      state.$watch = (key, callback) => {
        const stop = alpine.window.Alpine.watch(() => state[key], callback);
        watches.push(stop);
        return stop;
      };
      t.after(() => {
        watches.forEach(stop => stop());
        state.assistantCompositionReset();
      });
      state.isMobile = isMobile;
      let finishConfig;
      state._assistantRefreshConfig = () => new Promise(resolve => { finishConfig = resolve; });
      const init = state.initAssistant();
      let sent = 0;
      state.sendAssistantMessage = () => sent++;
      state.assistantDraft = '/st';
      state.assistantCompositionStart();
      if (ending) state.assistantCompositionEnd();
      assert.equal(press(state).prevented, false);
      // Alpine can remove the outgoing blur handler before the textarea loses focus.
      state.isMobile = !isMobile;
      await new Promise(resolve => queueMicrotask(resolve));
      assert.equal(state._assistantComposing, false);
      assert.equal(state._assistantCompositionEnding, false);
      assert.equal(state._assistantCompositionTimer, null);
      assert.equal(state.assistantDraft, '/st');
      assert.equal(sent, 0, 'replacing a composer never sends');
      state.assistantDraftChanged();
      assert.equal(state.assistantMenu?.kind, 'slash');
      assert.equal(press(state).prevented, true);
      assert.equal(sent, 0, 'ordinary Enter selects the suggestion');
      assert.equal(press(state).prevented, true);
      assert.equal(sent, 1, 'ordinary Enter works in the replacement composer');
      finishConfig();
      await init;
    });
  }
}

function activityHarness(t) {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const state = assistant();
  Object.assign(state, {
    assistantOpen: true,
    assistantStreaming: true,
    assistantThread: { id: 'thread-1', running: true },
    _assistantNoteScroll() {},
    _assistantScrollToEnd() {},
  });
  t.after(() => {
    state._appDestroyed = true;
    state._stopAssistantPoll();
    state._stopAssistantTick();
    state._assistantCloseStream();
  });
  const requests = [];
  state._assistantFetch = (path, options) => new Promise((resolve, reject) => {
    requests.push({ path, options, resolve, reject });
  });
  const flush = async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  };
  const reply = (request, data, etag = 'activity-1') => request.resolve({
    status: 200, headers: { get: () => etag }, json: async () => data,
  });
  return { state, requests, flush, reply };
}

for (const gateFirst of [false, true]) {
  test(`closed launch badge observes ${gateFirst ? 'approval then completion' : 'completion'} through read-only polling`, async t => {
    const { state, requests, flush, reply } = activityHarness(t);
    state.assistantDraft = 'Keep this draft';
    state.closeAssistant();
    assert.equal(state.assistantLaunchLabel(), 'Working');
    assert.notEqual(state._assistantPoll, null, 'a closed running thread still has a status poll');
    t.mock.timers.tick(1500);
    assert.equal(requests.length, 1);
    assert.match(requests[0].path, /\/thread-1\/activity\?after=0$/);
    assert.equal(requests[0].options.method, undefined, 'status updates cannot stop or approve a turn');
    if (gateFirst) {
      reply(requests[0], { running: true, gate: { id: 'gate-1', tool: 'bash' } });
      await flush();
      assert.equal(state.assistantLaunchLabel(), 'Approval');
      assert.equal(state.assistantLaunchHint(), 'Assistant: waiting for your approval');
      assert.equal(state.assistantThread.running, true);
      t.mock.timers.tick(1500);
      assert.equal(requests.length, 2);
      assert.equal(requests[1].options.headers['If-None-Match'], 'activity-1');
    }
    reply(requests.at(-1), { running: false, gate: null, messages: [{ n: 1, text: 'Finished' }] });
    await flush();
    assert.equal(state.assistantLaunchLabel(), '');
    assert.equal(state.assistantStreaming, false);
    assert.equal(state.assistantThread.running, false);
    assert.equal(state.assistantGate, null);
    assert.equal(state.assistantMessages[0].text, 'Finished');
    assert.equal(state.assistantOpen, false);
    assert.equal(state.assistantDraft, 'Keep this draft');
    assert.equal(state._assistantPoll, null, 'settled threads do not poll forever');
    t.mock.timers.tick(15000);
    assert.equal(requests.length, gateFirst ? 2 : 1);
  });
}

for (const failure of ['304', 'network', 'json']) {
  test(`closed status poll survives ${failure}`, async t => {
    const { state, requests, flush, reply } = activityHarness(t);
    state.closeAssistant();
    t.mock.timers.tick(1500);
    assert.equal(requests.length, 1);
    if (failure === '304') requests[0].resolve({ status: 304 });
    if (failure === 'network') requests[0].reject(new Error('offline'));
    if (failure === 'json') requests[0].resolve({ status: 200, json: async () => { throw new Error('invalid JSON'); } });
    await flush();
    assert.equal(state.assistantLaunchLabel(), 'Working');
    t.mock.timers.tick(1500);
    assert.equal(requests.length, 2);
    reply(requests[1], { running: false });
    await flush();
    assert.equal(state.assistantLaunchLabel(), '');
    assert.equal(state._assistantPoll, null);
  });
}

test('closing during a slow status request does not stack polls or reopen the stream', async t => {
  const { state, requests, flush, reply } = activityHarness(t);
  const original = globalThis.EventSource;
  let streams = 0;
  globalThis.EventSource = class { constructor() { streams++; } };
  t.after(() => {
    if (original === undefined) delete globalThis.EventSource;
    else globalThis.EventSource = original;
  });
  const load = state._loadAssistantActivity();
  state.closeAssistant();
  assert.equal(requests[0].options.signal.aborted, true, 'dismissal retires the pane request');
  t.mock.timers.tick(15000);
  assert.equal(requests.length, 2, 'one fresh badge request replaces the retired pane request');
  t.mock.timers.tick(15000);
  assert.equal(requests.length, 2, 'the pending badge request prevents stacked polls');
  reply(requests[0], { running: false, gate: { id: 'stale' } });
  await load;
  assert.equal(state.assistantLaunchLabel(), 'Working', 'the retired pane response is ignored');
  reply(requests[1], { running: true, gate: { id: 'gate-1' } });
  await flush();
  assert.equal(state.assistantLaunchLabel(), 'Approval');
  t.mock.timers.tick(1500);
  assert.equal(requests.length, 3);
  reply(requests[2], { running: false });
  await flush();
  assert.equal(state.assistantLaunchLabel(), '');
  assert.equal(streams, 0, 'background status only uses activity reads');
});

test('a forced refresh after a write supersedes an older pending status read', async t => {
  const { state, requests, flush, reply } = activityHarness(t);
  state.assistantOpen = false;
  const oldLoad = state._loadAssistantActivity();
  state._armAssistantPoll(0);
  reply(requests[0], { running: false, gate: null });
  await oldLoad;
  assert.equal(state.assistantStreaming, true, 'a snapshot from before the write cannot settle the turn');
  t.mock.timers.tick(0);
  assert.equal(requests.length, 2, 'the explicit refresh cannot disappear behind an older read');
  reply(requests[1], { running: true, gate: { id: 'new-gate' } });
  await flush();
  assert.equal(state.assistantLaunchLabel(), 'Approval');
  assert.notEqual(state._assistantPoll, null);
});

for (const stage of ['response', 'body', 'error']) {
  for (const change of ['destroy', 'replace', 'reselect']) {
    test(`background ${stage} cannot update or rearm after ${change}`, async t => {
      const { state, requests, flush, reply } = activityHarness(t);
      state.assistantOpen = false;
      const load = state._loadAssistantActivity();
      let finishBody;
      if (stage === 'body') {
        requests[0].resolve({ status: 200, json: () => new Promise(resolve => { finishBody = resolve; }) });
        await flush();
      }
      let selection;
      if (change === 'destroy') state._appDestroyed = true;
      else selection = state.assistantSelectThread(change === 'reselect' ? 'thread-1' : 'thread-2', { quiet: true });
      if (stage === 'body') finishBody({ running: false, gate: { id: 'stale' } });
      else if (stage === 'error') requests[0].reject(new Error('stale failure'));
      else reply(requests[0], { running: false, gate: { id: 'stale' } });
      await load;
      assert.equal(state.assistantStreaming, true);
      assert.equal(state.assistantGate, null);
      assert.equal(state._assistantETag, '');
      assert.equal(state._assistantPoll, null);
      t.mock.timers.tick(1500);
      assert.equal(requests.length, change === 'destroy' ? 1 : 2, 'a stale request cannot schedule another owner’s poll');
      if (selection) {
        requests[1].reject(new Error('selection canceled'));
        assert.equal(await selection, false);
      }
    });
  }
}

for (const change of ['destroy', 'replace', 'reselect']) {
  test(`queued background poll cannot run after ${change}`, async t => {
    const { state, requests } = activityHarness(t);
    state.closeAssistant();
    assert.notEqual(state._assistantPoll, null);
    let selection;
    if (change === 'destroy') state._appDestroyed = true;
    else selection = state.assistantSelectThread(change === 'reselect' ? 'thread-1' : 'thread-2', { quiet: true });
    t.mock.timers.tick(1500);
    assert.equal(requests.length, change === 'destroy' ? 0 : 1, 'only the explicit selection may fetch');
    assert.equal(state._assistantPoll, null);
    if (selection) {
      requests[0].reject(new Error('selection canceled'));
      assert.equal(await selection, false);
    }
  });
}

for (const completion of ['success', 'failure']) {
  test(`selecting another thread invalidates pending background activity before selection ${completion}`, async t => {
    const { state, requests, reply } = activityHarness(t);
    state.assistantOpen = false;
    const oldLoad = state._loadAssistantActivity();
    const selection = state.assistantSelectThread('thread-2', { quiet: true });
    reply(requests[0], { running: false, gate: { id: 'stale' } });
    await oldLoad;
    assert.equal(state.assistantStreaming, true);
    assert.equal(state.assistantGate, null);
    assert.equal(state._assistantPoll, null);
    if (completion === 'failure') {
      requests[1].reject(new Error('missing thread'));
      assert.equal(await selection, false);
      assert.equal(state.assistantThread, null);
      assert.equal(state.assistantLaunchLabel(), '');
      return;
    }
    reply(requests[1], { id: 'thread-2', running: true });
    await new Promise(resolve => queueMicrotask(resolve));
    reply(requests[2], { running: false });
    assert.equal(await selection, true);
    assert.equal(state.assistantThread.id, 'thread-2');
    assert.equal(state.assistantLaunchLabel(), '');
  });
}

for (const view of ['Chat', 'History']) {
  for (const stage of ['response', 'body', 'error']) {
    test(`${view} restores running thread updates after canceling selection at ${stage}`, async t => {
      const { state, requests, flush, reply } = activityHarness(t);
      const original = globalThis.EventSource;
      const streams = [];
      globalThis.EventSource = class {
        constructor(url) { this.url = url; this.closed = false; streams.push(this); }
        addEventListener() {}
        close() { this.closed = true; }
      };
      t.after(() => {
        if (original === undefined) delete globalThis.EventSource;
        else globalThis.EventSource = original;
      });
      state.assistantView = 'history';
      let historyLoads = 0;
      state.assistantLoadThreads = () => { historyLoads++; };
      state._armAssistantPoll();
      state._assistantOpenStream();
      const selection = state.assistantSelectThread('thread-2');
      assert.equal(state._assistantPoll, null);
      assert.equal(streams[0].closed, true);
      let finishBody;
      if (stage === 'body') {
        requests[0].resolve({ json: () => new Promise(resolve => { finishBody = resolve; }) });
        await flush();
      }
      if (view === 'Chat') state.assistantShowChat();
      else state.assistantShowHistory();
      assert.equal(requests.length, 2, 'presentation immediately refreshes without waiting for the canceled selection');
      assert.match(requests[1].path, /\/thread-1\/activity\?after=0$/);
      reply(requests[1], { running: true, gate: { id: 'approval-1' } });
      await flush();
      const poll = state._assistantPoll;
      assert.notEqual(poll, null, 'the accepted refresh rearms polling');
      assert.equal(state.assistantLaunchLabel(), 'Approval');
      assert.equal(streams.length, 2);
      assert.equal(streams[1].url, '/api/assistant/threads/thread-1/stream');
      assert.equal(streams[1].closed, false);
      assert.equal(historyLoads, view === 'History' ? 1 : 0);
      if (stage === 'body') finishBody({ id: 'thread-2', running: false });
      else if (stage === 'error') requests[0].reject(new Error('stale selection'));
      else reply(requests[0], { id: 'thread-2', running: false });
      assert.equal(await selection, false);
      assert.equal(state.assistantThread.id, 'thread-1');
      assert.equal(state.assistantView, view === 'Chat' ? 'thread' : 'history');
      assert.equal(state.assistantError, null);
      assert.equal(state._assistantPoll, poll, 'stale callbacks cannot replace the recovered poll');
      assert.equal(streams.length, 2);
      t.mock.timers.tick(1500);
      assert.equal(requests.length, 3);
      reply(requests[2], { running: false, messages: [{ n: 1, text: 'Finished' }] });
      await flush();
      assert.equal(state.assistantStreaming, false);
      assert.equal(state.assistantGate, null);
      assert.equal(state.assistantMessages[0].text, 'Finished');
      assert.equal(state._assistantPoll, null);
      assert.equal(streams[1].closed, true);
    });
  }
}

test('deleting the selected thread clears approval and background polling', async t => {
  const { state, requests } = activityHarness(t);
  state.assistantGate = { id: 'gate-1' };
  state.assistantLoadThreads = () => {};
  state.closeAssistant();
  const deletion = state.assistantDeleteThread('thread-1');
  requests[0].resolve({});
  await deletion;
  t.mock.timers.tick(15000);
  assert.equal(requests.length, 1);
  assert.equal(state.assistantThread, null);
  assert.equal(state.assistantLaunchLabel(), '');
  assert.equal(state._assistantPoll, null);
});

test('a closed approval keeps polling even when running is false', async t => {
  const { state, requests, flush, reply } = activityHarness(t);
  state.assistantStreaming = false;
  state.assistantThread.running = false;
  state.assistantGate = { id: 'gate-1' };
  state.closeAssistant();
  t.mock.timers.tick(1500);
  assert.equal(requests.length, 1);
  reply(requests[0], { running: false });
  await flush();
  assert.equal(state.assistantLaunchLabel(), '');
  assert.equal(state._assistantPoll, null);
});

test('closing an idle Assistant does not start background traffic', t => {
  const { state, requests } = activityHarness(t);
  state.assistantStreaming = false;
  state.assistantThread.running = false;
  state.closeAssistant();
  t.mock.timers.tick(15000);
  assert.equal(requests.length, 0);
  assert.equal(state._assistantPoll, null);
});

test('composer accessibility references use the active exclusive shell', () => {
  const state = assistant();
  assert.equal(state.assistantComposerID('menu'), 'assistant-menu');
  state.isMobile = true;
  assert.equal(state.assistantComposerID('menu'), 'assistant-sheet-menu');
});
