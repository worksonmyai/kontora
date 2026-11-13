import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const script = fs.readFileSync(path.join(web, 'ui/assistant.js'), 'utf8').replaceAll('export ', '');
function fixture() {
  const ticks = [];
  const scroll = { scrollHeight: 1000, clientHeight: 100, scrollTop: 100 };
  const context = vm.createContext({ localStorage: { getItem: () => null, setItem() {} },
    document: { getElementById: () => scroll }, clearTimeout() {}, clearInterval() {} });
  vm.runInContext(script + '\nthis.state = kontoraAssistant();', context);
  const state = context.state;
  Object.assign(state, { companionThread: () => false, assistantOpen: true, assistantThread: { id: 'old' },
    $nextTick: cb => ticks.push(cb) });
  return { state, scroll, ticks };
}
const events = count => Array.from({ length: count }, (_, i) => ({ kind: 'text', text: String(i) }));
const plain = value => JSON.parse(JSON.stringify(value));

for (const messageCount of [0, 1200]) {
  test(`initial snapshot stays at 200 rows after a scrolled-up previous thread (${messageCount} messages)`, () => {
    const { state } = fixture();
    state.assistantEvents = events(700);
    state.assistantWindow = 600;
    state._assistantActivityLoaded = true;
    state._assistantAdopt({ id: 'new', running: false }, Array.from({ length: messageCount }, (_, i) => ({ n: i + 1, text: 'message' })));
    state.assistantEvents = [];
    state._mergeAssistantActivity({ tape: { events: events(3000) } });
    assert.equal(state.assistantThreadRows().length, 200);
    assert.equal(state.assistantHiddenRows(), 2800 + messageCount);
    assert.equal(state._assistantCursor, 3000);
    assert.equal(state.assistantEvents.length, 3000);
    assert.equal(state.assistantMessages.length, messageCount);
    assert.equal(state.assistantThreadRows().at(-1).key, 'e2999');
  });
}

test('timed interleaving groups untimed events and leaves an optimistic turn after the recorded tape', () => {
  const { state } = fixture();
  state.assistantMessages = [
    { n: 1, text: 'one', at: '2026-01-01T10:00:00Z' },
    { n: 2, text: 'two', at: '2026-01-01T11:00:00Z' },
    { n: 3, text: 'pending', at: '2026-01-01T12:00:00Z' },
  ];
  state.assistantEvents = [
    { kind: 'text', time: '2026-01-01T10:01:00Z' },
    { kind: 'tool' },
    { kind: 'text', time: '2026-01-01T11:01:00Z' },
  ];
  assert.deepEqual(plain(state.assistantThreadRows()).map(r => r.key), ['m1-0', 'e0', 'e1', 'm2-1', 'e2', 'm3-2']);
  const messages = state.assistantMessages;
  state._mergeAssistantActivity({ offset: 3, messages: messages.slice(0, 2), partial: 'stream text' });
  assert.equal(state.assistantMessages, messages);
  const rows = state.assistantThreadRows();
  state._assistantAdoptPartial({ partial: 'longer stream text' });
  assert.equal(state.assistantThreadRows(), rows);
});

test('partial-only polls retain history identity even when the daemon rewinds an unchanged tool suffix', () => {
  const { state } = fixture();
  const tape = events(1000);
  tape.push({ kind: 'tool', id: 'pending', usage: { input: 12, output: 2 }, result: '' });
  state._mergeAssistantActivity({ tape: { events: tape } });
  const array = state.assistantEvents;
  const rows = state.assistantThreadRows();
  for (const partial of ['a', 'ab', 'abc']) {
    state._mergeAssistantActivity({ offset: 1000, tape: { events: plain(tape.slice(1000)) }, partial });
    assert.equal(state.assistantEvents, array);
    assert.equal(state.assistantThreadRows(), rows);
  }
  state.toggleAssistantTool(tape[1000], 1000);
  state._mergeAssistantActivity({ offset: 1000, tape: { events: [{ ...tape[1000], result: 'done' }] } });
  assert.notEqual(state.assistantEvents, array);
  assert.equal(state.assistantToolExpanded(state.assistantEvents[1000], 1000), true);
  assert.equal(state.assistantThreadRows().at(-1).key, 'e1000');
  const settled = state.assistantEvents;
  state._mergeAssistantActivity({ offset: 1000, tape: { events: [{ ...tape[1000], result: 'done', usage: { input: 12, output: 3 } }] } });
  assert.notEqual(state.assistantEvents, settled);
});

test('close fences an already queued load-earlier scroll compensation', () => {
  const { state, scroll, ticks } = fixture();
  state.assistantEvents = events(500);
  state.assistantLoadEarlier();
  state.closeAssistant();
  scroll.scrollHeight += 700;
  for (const tick of ticks) tick();
  assert.equal(scroll.scrollTop, 100);
  assert.equal(state.assistantWindow, 400);
});
