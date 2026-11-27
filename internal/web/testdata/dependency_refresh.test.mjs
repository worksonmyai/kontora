import assert from 'node:assert/strict';
import test from 'node:test';
import { setImmediate } from 'node:timers/promises';
import '../ui/index.js';

globalThis.window = { innerWidth: 1280 };
globalThis.document = { documentElement: { dataset: {} }, getElementById: () => null };
globalThis.localStorage = { getItem: () => null };

const subjectURL = '/api/tickets/subject';
const chainURL = subjectURL + '/chain';
const prerequisite = { id: 'prereq', status: 'open' };
const chainNode = ref => ({ ...ref, direction: 'upstream', holds_chain: true });

function setup({ status, dep = prerequisite, chainLoaded = true, ready = false, editing = false }) {
  const handlers = new Map();
  const frames = new Map();
  let frameId = 0;
  globalThis.EventSource = class {
    addEventListener(name, fn) { handlers.set(name, fn); }
    close() {}
  };
  globalThis.requestAnimationFrame = fn => {
    frames.set(++frameId, fn);
    return frameId;
  };
  globalThis.cancelAnimationFrame = id => frames.delete(id);
  const ticket = { id: 'subject', kontora: true, status, body: 'Current body', deps: [dep],
    blockers: ready ? [] : [dep.id], ready };
  const app = globalThis.kontora();
  Object.assign(app, {
    selectedTicket: { ...ticket }, tickets: [app.boardEntry(ticket)], editing,
    chain: chainLoaded ? { id: ticket.id, nodes: ready ? [] : [chainNode(dep)] } : null,
    editForm: { body: 'Unsaved local draft' },
    fetchTicketCost() {}, recomputeBoard() {}, updateFavicon() {},
  });
  (app.isSecondary(dep) ? app.secondaryTickets : app.tickets).push(dep);
  app.connectSSE();
  return {
    app, ticket,
    event: (name, data) => handlers.get(name)({ data: JSON.stringify(data) }),
    flush: () => { for (const fn of [...frames.values()]) fn(); },
  };
}

const reply = data => ({ ok: true, json: async () => data });

for (const [status, chainLoaded] of [['open', false], ['todo', true]]) {
  test(`completing the last prerequisite refreshes the selected ${status} ticket`, async t => {
    const { app, ticket, event, flush } = setup({ status, chainLoaded, editing: true });
    const done = { ...prerequisite, status: 'done' };
    const requests = [];
    t.mock.method(globalThis, 'fetch', async url => {
      requests.push(url);
      if (url === chainURL) return reply({ id: ticket.id, verdict: 'ready', nodes: [] });
      assert.equal(url, subjectURL);
      return reply({ ...ticket, deps: [done], blockers: undefined, ready: true });
    });

    assert.equal(app.dependsOn().done, 0);
    assert.equal(app.ticketBlocker(app.selectedTicket)?.id, prerequisite.id);
    event('ticket_updated', { ...prerequisite, status: 'in_progress' });
    event('ticket_updated', done);
    flush();
    await setImmediate();

    assert.deepEqual(requests.sort(), [subjectURL, chainURL].sort(), 'one detail and one chain read per batch');
    assert.equal(app.dependsOn().done, 1);
    assert.equal(app.ticketBlocker(app.selectedTicket), null);
    if (status === 'todo') assert.notEqual(app.queueReason(app.selectedTicket)?.kind, 'blocked');
    assert.equal(app.selectedTicket.ready, true);
    assert.equal(app.selectedTicket.blockers, undefined);
    assert.equal(app.editForm.body, 'Unsaved local draft');
    assert.equal(app.selectedTicket.body, ticket.body);
  });
}

test('an external dependency deletion invalidates an in-flight detail read', async t => {
  const dep = { ...prerequisite, source: 'external', status: 'done' };
  const { app, ticket, event, flush } = setup({ status: 'todo', dep, ready: true });
  const missing = { id: dep.id, missing: true };
  let detailReads = 0;
  let resolveStale;
  const stale = new Promise(resolve => { resolveStale = resolve; });
  t.mock.method(globalThis, 'fetch', async url => {
    if (url === chainURL) return reply({ id: ticket.id, verdict: 'blocked', nodes: [chainNode(missing)] });
    assert.equal(url, subjectURL);
    if (++detailReads === 1) return stale;
    return reply({ ...ticket, deps: [missing], blockers: [dep.id], ready: false });
  });

  const refresh = app.refreshSelectedTicket();
  event('secondary_ticket_deleted', { id: dep.id, source: dep.source });
  flush();
  resolveStale(reply(ticket));
  await refresh;

  assert.equal(detailReads, 2, 'retry the superseded detail read without starting a competing flight');
  assert.equal(app.dependsOn().done, 0);
  assert.equal(app.selectedTicket.ready, false);
  assert.deepEqual(app.selectedTicket.blockers, [dep.id]);
  assert.equal(app.queueReason(app.selectedTicket)?.id, dep.id);
  assert.equal(app.queueReason(app.selectedTicket)?.missing, true);
});
