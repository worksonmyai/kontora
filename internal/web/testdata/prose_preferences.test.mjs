import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraAssistantMarkup } from '../ui/assistant_markup.js';
import { DETAIL_NOTES } from '../ui/detail_markup/notes.js';
import { kontoraMarkdown } from '../ui/markdown.js';
import { kontoraNotify } from '../ui/notify.js';
import { kontoraRelations } from '../ui/relations.js';
import { kontoraSources } from '../ui/sources.js';

const response = data => ({ ok: true, json: async () => structuredClone(data) });
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
function preferences(fields) {
  const ticket = { id: 'kon-1234', status: 'open', body: 'current body', notes: ['current note'], ...fields };
  return Object.assign({}, kontoraSources(), kontoraNotify(), {
    selectedTicket: structuredClone(ticket), tickets: [structuredClone(ticket)],
    _ticketDetailSeq: 1, _selectedTicketUpdateSeq: 0,
    configCache: { default_channels: ['ops'] },
    executionActive: () => false, projectForPath: () => null,
  });
}

for (const source of ['primary', 'team']) for (const [name, fields] of [
  ['absent', {}], ['null', { notify: null }], ['explicit off', { notify: [] }],
]) test(`channel-only sessions preserve ${name}, source=${source}`, async t => {
  const state = preferences({ ...fields, source, can_edit: true });
  const posts = [];
  t.mock.method(globalThis, 'setTimeout', () => 0);
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    const payload = JSON.parse(options.body);
    posts.push({ url, payload });
    return response({ ...fields, notify_channels: payload.notify_channels, body: 'stale body', notes: [], status: 'done', revision: 'unowned' });
  });
  if (source === 'team') { state.secondaryTickets = state.tickets; state.tickets = []; }
  for (const channels of [['ops'], ['chat'], []]) {
    state.openNotifyEditor();
    assert.equal(state.notifyDraft.notifyUnset, name !== 'explicit off');
    assert.equal(await state.flushNotifySave(), true, 'reopening does not write an unchanged preference');
    state.notifyDraft.notifyChannels = channels;
    assert.equal(await state.saveNotify(), true);
    assert.equal(await state.flushNotifySave(), true);
    state.closeNotifyEditor();
    assert.deepEqual(state.selectedTicket.notify, fields.notify);
    const entry = (source === 'team' ? state.secondaryTickets : state.tickets)[0];
    assert.deepEqual(entry.notify, fields.notify);
    assert.deepEqual(entry.notify_channels, channels);
    assert.equal(state.selectedTicket.body, 'current body');
    assert.deepEqual(state.selectedTicket.notes, ['current note']);
    assert.equal(state.selectedTicket.status, 'open');
    assert.equal(state.selectedTicket.revision, undefined);
  }
  assert.equal(posts.length, 3);
  for (const { url, payload } of posts) {
    assert.equal(url, source === 'team' ? '/api/sources/team/tickets/kon-1234' : '/api/tickets/kon-1234');
    assert.equal(Object.hasOwn(payload, 'notify'), name === 'explicit off');
    if (name === 'explicit off') assert.deepEqual(payload.notify, []);
  }
});

for (const [name, before, newer] of [
  ['absent to off', {}, { notify: [] }],
  ['null to off', { notify: null }, { notify: [] }],
  ['off to absent', { notify: [] }, {}],
  ['off to null', { notify: [] }, { notify: null }],
]) test(`notification conflict signature distinguishes ${name}`, async t => {
  const state = preferences(before);
  const gate = deferred();
  t.mock.method(globalThis, 'setTimeout', () => 0);
  t.mock.method(globalThis, 'fetch', async () => { await gate.promise; return response({ notify: ['done'], notify_channels: ['chat'] }); });
  state.openNotifyEditor();
  state.notifyDraft.notify = ['done']; state.notifyDraft.notifyUnset = false;
  state.notifyDraft.notifyChannels = ['chat'];
  const saving = state.saveNotify();
  const latest = { ...state.selectedTicket, ...newer, notify_channels: undefined };
  if (!Object.hasOwn(newer, 'notify')) delete latest.notify;
  state.selectedTicket = latest; state.tickets[0] = structuredClone(latest);
  gate.resolve();
  assert.equal(await saving, true);
  assert.deepEqual(state.selectedTicket, latest);
  assert.deepEqual(state.tickets[0], latest);
});

test('a channel-only acknowledgement cannot adopt a status preference it did not write', async t => {
  const state = preferences({ notify: null });
  t.mock.method(globalThis, 'setTimeout', () => 0);
  t.mock.method(globalThis, 'fetch', async () => response({ notify: ['done'], notify_channels: ['chat'] }));
  state.openNotifyEditor(); state.notifyDraft.notifyChannels = ['chat'];
  assert.equal(await state.saveNotify(), true);
  assert.equal(state.selectedTicket.notify, null);
  assert.deepEqual(state.selectedTicket.notify_channels, ['chat']);
});

// Deliberately tiny DOM: parsing/sanitizing are identity functions, but entity
// matching, tooltip attributes, event closures and node replacement are real.
function proseDocument() {
  const doc = {
    createTextNode(nodeValue) { return { nodeType: 3, nodeValue, get parentElement() { return this.parentNode; } }; },
    createDocumentFragment() { return { nodeType: 11, childNodes: [], appendChild(n) { this.childNodes.push(n); return n; } }; },
    createElement(tag) {
      return {
        nodeType: 1, tagName: tag.toUpperCase(), childNodes: [], attrs: {}, events: {}, writes: 0, className: '',
        get textContent() { return this.childNodes.map(n => n.nodeType === 3 ? n.nodeValue : n.textContent).join(''); },
        set textContent(value) { this.writes++; this.childNodes = [doc.createTextNode(value)]; this.childNodes[0].parentNode = this; },
        set innerHTML(value) { this.textContent = value; },
        closest() { return null; },
        setAttribute(k, v) { this.attrs[k] = String(v); },
        addEventListener(k, v) { this.events[k] = v; },
        appendChild(n) { n.parentNode = this; this.childNodes.push(n); return n; },
        replaceChild(fresh, old) {
          const kids = fresh.nodeType === 11 ? fresh.childNodes : [fresh];
          kids.forEach(n => { n.parentNode = this; });
          this.childNodes.splice(this.childNodes.indexOf(old), 1, ...kids);
        },
      };
    },
    createTreeWalker(root, _show, filter) {
      const nodes = [];
      const walk = n => (n.childNodes || []).forEach(c => { if (c.nodeType !== 3) walk(c); else if (filter.acceptNode(c) === 1) nodes.push(c); });
      walk(root);
      return { nextNode: () => nodes.shift() };
    },
  };
  return doc;
}
function prose(t) {
  const replaceGlobal = (key, value) => {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, key);
    Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
    t.after(() => { if (descriptor) Object.defineProperty(globalThis, key, descriptor); else delete globalThis[key]; });
  };
  const doc = proseDocument();
  replaceGlobal('document', doc);
  const counts = { parse: 0, sanitize: 0 };
  replaceGlobal('marked', { parse: text => { counts.parse++; return text; } });
  replaceGlobal('DOMPurify', { sanitize: text => { counts.sanitize++; return text; } });
  const state = Object.assign({}, kontoraSources(), kontoraRelations(), kontoraMarkdown(), {
    tickets: [{ id: 'kon-1234', title: '[web] First title', status: 'open' }],
    selectedTicket: { id: 'kon-5678', branch: 'topic', deps: [] },
    ticketChanges: { base: 'main', remote: 'https://github.com/a/repo', commits: [{ sha: 'abc1234', subject: 'First subject' }], files: [{ path: 'src/app.js', added: 2, deleted: 1 }] },
    _paletteStatusMarks: { open: { cls: 'open-hue' }, done: { cls: 'done-hue' } },
    splitTitleTag(title = '') { const m = /^\[([^\]]+)\] ?(.*)$/.exec(title); return m ? { tag: m[1], rest: m[2] } : { tag: '', rest: title }; },
    pipelineColorByName: tag => 'color-' + tag,
    paletteStatusLabel: status => status,
    async openTicketRef(ref) { this.opened = ref; },
  });
  return { state, doc, counts };
}
const chips = el => el.childNodes.filter(n => n.nodeType === 1);
const ticketText = 'Waiting on kon-1234.';
for (const method of ['setProse', 'setNoteText', 'setSummaryProse']) for (const [name, mutate, check] of [
  ['same-size board replacement', s => { s.tickets = [{ id: 'kon-9999', title: 'Other', status: 'open' }]; }, el => assert.equal(chips(el).length, 0)],
  ['title', s => { s.tickets[0].title = '[api] New title'; }, el => assert.equal(chips(el)[0].attrs['data-tip-e'], 'New title')],
  ['status', s => { s.tickets[0].status = 'done'; }, el => assert.match(chips(el)[0].className, /done-hue/)],
  ['source', s => { s.selectedTicket.source = 'team'; s.secondaryTickets = [{ id: 'kon-1234', source: 'team', title: 'Secondary', status: 'done' }]; }, el => assert.equal(chips(el)[0].attrs['data-tip-e'], 'Secondary')],
  ['relation metadata', s => { s.tickets = []; s.selectedTicket.deps = [{ id: 'kon-1234', title: 'Archived relation', status: 'archived' }]; }, el => assert.equal(chips(el)[0].attrs['data-tip-e'], 'Archived relation')],
]) test(`${method} invalidates on ${name}`, t => {
  const { state, doc } = prose(t); const el = doc.createElement('div');
  state[method](el, ticketText); const old = el.childNodes[0];
  mutate(state); state[method](el, ticketText);
  assert.notEqual(el.childNodes[0], old); check(el);
});

for (const method of ['setProse', 'setNoteText', 'setSummaryProse']) test(`${method} observes relation-only updates and resolution`, t => {
  const { state, doc } = prose(t); const el = doc.createElement('div'); state.tickets = [];
  state.selectedTicket.deps = [{ id: 'kon-1234' }]; state[method](el, ticketText);
  assert.equal(chips(el).length, 0);
  for (const [title, status] of [['Resolved', 'open'], ['Renamed', 'done']]) {
    state.selectedTicket.deps = [{ id: 'kon-1234', title, status }];
    state[method](el, ticketText);
    assert.equal(chips(el)[0].attrs['data-tip-e'], title);
    assert.equal(chips(el)[0].attrs['data-tip-e-body'], status);
  }
});

for (const [name, mutate, token, expected] of [
  ['branch', s => { s.selectedTicket.branch = 'other'; }, 'topic', null],
  ['base', s => { s.ticketChanges.base = 'develop'; }, 'topic', 'Branched from develop'],
  ['commit sha', s => { s.ticketChanges.commits[0].sha = 'def5678'; }, 'abc1234', null],
  ['commit subject', s => { s.ticketChanges.commits[0].subject = 'New subject'; }, 'abc1234', 'New subject'],
  ['file path', s => { s.ticketChanges.files[0].path = 'src/other.js'; }, 'app.js', undefined],
  ['file added', s => { s.ticketChanges.files[0].added = 9; }, 'app.js', '+9/-1 on this branch'],
  ['file deleted', s => { s.ticketChanges.files[0].deleted = 8; }, 'app.js', '+2/-8 on this branch'],
  ['remote', s => { s.ticketChanges.remote = 'https://github.com/b/next'; }, '#12', 'b/next'],
]) test(`summary context includes ${name}`, t => {
  const { state, doc } = prose(t); const el = doc.createElement('div');
  state.setSummaryProse(el, token); const old = el.childNodes[0];
  mutate(state); state.setSummaryProse(el, token);
  assert.notEqual(el.childNodes[0], old);
  if (expected === null) assert.equal(chips(el).length, 0);
  else assert.equal(chips(el)[0].attrs['data-tip-e-body'], expected);
});

for (const method of ['setProse', 'setNoteText', 'setSummaryProse']) for (const owner of ['explicit', 'selected epic']) test(`${method} supports a source-qualified rendering subject: ${owner}`, async t => {
  const { state, doc } = prose(t); const el = doc.createElement('div');
  const subject = { id: 'kon-epic', source: 'team', deps: [{ id: 'kon-1234', title: 'Epic relation', status: 'done' }] };
  if (owner === 'selected epic') { state.selectedTicket = null; state.selectedEpic = subject; }
  const render = () => owner === 'explicit' ? state[method](el, ticketText, subject) : state[method](el, ticketText);
  render();
  const chip = chips(el)[0]; assert.equal(chip.attrs['data-tip-e'], 'Epic relation');
  await chip.events.click(); assert.equal(state.opened.source, 'team');
  const old = el.childNodes[0]; subject.id = 'kon-next'; render();
  assert.notEqual(el.childNodes[0], old, 'rendering subject identity is part of the key');
});

for (const method of ['setProse', 'setNoteText', 'setSummaryProse']) test(`${method} keeps unchanged DOM and excludes clock/unrelated record fields`, t => {
  const { state, doc } = prose(t); const el = doc.createElement('div');
  const text = `${method}: kon-1234 abc1234 app.js topic #12`;
  state[method](el, text); const original = [...el.childNodes]; const writes = el.writes;
  state.now = Date.now(); state.tickets[0].updated_at = 'later';
  state.tickets = structuredClone(state.tickets); state.ticketChanges = structuredClone(state.ticketChanges);
  if (method !== 'setSummaryProse') {
    state.ticketChanges.base = 'unrelated'; state.ticketChanges.remote = 'https://github.com/elsewhere/repo';
    state.ticketChanges.commits[0].subject = 'Not used by authored prose';
    state.ticketChanges.files[0].added++;
  }
  state[method](el, text);
  assert.equal(el.writes, writes); assert.deepEqual(el.childNodes, original);
  original.forEach((node, i) => assert.equal(el.childNodes[i], node));
});

test('context invalidation and separate elements reuse the text-keyed sanitizer cache', t => {
  const { state, doc, counts } = prose(t); const el = doc.createElement('div');
  const text = 'cache regression unique text kon-1234 abc1234';
  state.setSummaryProse(el, text);
  state.ticketChanges.commits[0].subject = 'Changed'; state.setSummaryProse(el, text);
  state.setProse(doc.createElement('div'), text);
  assert.deepEqual(counts, { parse: 1, sanitize: 1 });
});

test('recorded Assistant replies use shared prose styling in both shells without ticket-read', () => {
  const { assistantTranscriptMarkup, assistantSheetTranscriptMarkup } = kontoraAssistantMarkup();
  for (const transcript of [assistantTranscriptMarkup, assistantSheetTranscriptMarkup]) {
    const classes = transcript.match(/<div class="([^"]+)" x-effect="setProse\(\$el, row\.event\.text\)"><\/div>/)?.[1].split(/\s+/) || [];
    assert.ok(classes.includes('ticket-prose'));
    assert.ok(classes.includes('prose-assistant'), 'keeps the Assistant-only styling hook');
    assert.ok(!classes.includes('ticket-read'));
  }
});

test('shared Notes headings, lifecycle and byline metadata use the readable text token', () => {
  const token = /(?:^|\s)!?text-tx-3(?:\s|$)/;
  const spans = [...DETAIL_NOTES.matchAll(/<span\b[^>]*>/g)].map(match => match[0]);
  const readable = tag => token.test(tag?.match(/\bclass="([^"]+)"/)?.[1] || '');
  for (const binding of [
    'noteLastAgo()', 'item.label', 'item.meta', 'noteAgo(item.at)',
    'noteRole(item.note)', 'noteAgo(item.note.at)', 'noteRole(r)', 'noteAgo(r.at)',
  ]) {
    assert.ok(readable(spans.find(span => span.includes(`x-text="${binding}"`))), binding);
  }
  for (const edited of ['item.note.edited', 'r.edited']) {
    assert.ok(readable(spans.find(span => span.includes(`x-show="${edited}"`))), edited);
  }
  for (const heading of ['Notes', 'No notes yet']) {
    const tag = DETAIL_NOTES.match(new RegExp(`<(?:h2|span)\\b[^>]*>(?=${heading}<)`))?.[0];
    assert.ok(readable(tag), heading);
  }
});
