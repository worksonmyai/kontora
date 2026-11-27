import assert from 'node:assert/strict';
import test from 'node:test';
import '../ui/index.js';

const holder = { id: 'prereq', direction: 'upstream', holds_chain: true, status: 'open' };
const savedPreview = { stages: ['code', 'push'], agent: 'stage-agent', branch: 'feature', repo: '/repo' };

globalThis.window = { innerWidth: 1280, matchMedia: () => ({ matches: false }) };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.document = { documentElement: { dataset: {} }, getElementById: () => null };

function setup(fields = {}, { nodes = [], tickets = [], secondary = [] } = {}) {
  const app = globalThis.kontora();
  const ticket = { id: 'subject', status: 'open', kontora: true, title: 'Main ticket', run_preview: savedPreview, ...fields };
  Object.assign(app, {
    selectedTicket: { ...ticket }, tickets: [ticket, ...tickets], secondaryTickets: secondary,
    chain: { id: ticket.id, nodes },
    configCache: { default_agent: 'daemon-agent', projects: [{ name: 'Repo', path: '/repo', agent: 'project-agent' }] },
    $refs: {}, $nextTick: async () => {},
    recomputeBoard() {}, showToast() {},
  });
  return app;
}

function stubFetch(t, app, reply) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    const method = options?.method || 'GET';
    requests.push({ url, method, body: options?.body });
    const id = url.split('/')[3];
    const ticket = app.lifecycleResolve(id);
    assert.ok(ticket, 'request uses a known primary identity: ' + url);
    const response = reply ? reply(ticket, method) : ticket;
    return { ok: true, json: async () => response };
  });
  return requests;
}

const writes = requests => requests.filter(({ method }) => method !== 'GET');

test('queue reason identifies blockers without treating readiness as scheduler admission', () => {
  const cases = [
    ['upstream wins', { blockers: ['direct'] }, [holder], 'blocked', 'prereq'],
    ['downstream holder does not block', { blockers: ['direct'] }, [{ ...holder, direction: 'downstream' }], 'blocked', 'direct'],
    ['payload before chain loads', { blockers: ['direct'], ready: true }, [], 'blocked', 'direct'],
    ['ready', { ready: true }, [], 'queued'],
    ['not ready', {}, [], 'queued'],
    ['stale holder after blockers clear', {}, [holder], 'queued'],
    ['not queued', { status: 'open', ready: true }, [holder], undefined],
    ['execution already active', { execution_active: true, ready: true }, [holder], undefined],
  ];
  for (const [name, fields, nodes, kind, id] of cases) {
    const app = setup({ status: 'todo', ...fields }, { nodes });
    const reason = app.queueReason(app.selectedTicket);
    assert.equal(reason?.kind, kind, name);
    assert.equal(reason?.id, id, name);
  }
});

test('blocker references preserve source and ignore another ticket’s chain', () => {
  const primary = { id: 'prereq', status: 'open', kontora: true };
  const external = { id: 'prereq', source: 'other', status: 'paused', title: 'External blocker' };
  const app = setup({ blockers: ['prereq'], deps: [external] }, { tickets: [primary], secondary: [external] });
  assert.equal(app.ticketKey(app.ticketBlocker(app.selectedTicket).ref), 'other/prereq');
  assert.equal(app.ticketBlocker(app.selectedTicket).status, 'paused');
  app.chain.nodes = [{ ...holder, source: 'other', status: 'paused' }];
  assert.equal(app.ticketKey(app.ticketBlocker(app.selectedTicket).ref), 'other/prereq');
  app.chain.nodes = [holder];
  assert.equal(app.ticketKey(app.ticketBlocker(app.selectedTicket).ref), 'prereq');
  const unrelated = { ...app.selectedTicket, source: 'other', blockers: ['unknown'], deps: [] };
  const blocker = app.ticketBlocker(unrelated);
  assert.equal(app.ticketKey(blocker.ref), 'other/unknown');
  assert.equal(app.relationKnown(blocker.ref), false);
});

test('run preview shows daemon selection and never enables a broken pipeline', async t => {
  const cases = [
    ['legacy', savedPreview, ''],
    ['check', { ...savedPreview, kind: 'check', stages: ['resume-check'], agent: undefined }, 'Workflow graph · check (no agent)'],
    ['rework', { ...savedPreview, kind: 'rework', stages: ['rework'], agent: 'review-agent' }, 'Plannotator rework'],
    ['missing pipeline', { error: 'unknown pipeline' }, ''],
  ];
  for (const [name, run_preview, kind] of cases) {
    await t.test(name, async st => {
      const app = setup({ run_preview });
      const requests = stubFetch(st, app);
      await app.openRunConfirm(app.selectedTicket);
      const preview = app.runConfirmView();
      assert.equal(preview.kind, kind);
      assert.deepEqual(preview.stages, run_preview.stages || []);
      assert.equal(preview.agent, run_preview.agent || '');
      assert.equal(preview.repo, run_preview.repo || '');
      assert.equal(preview.branch, run_preview.branch || '');
      assert.equal(preview.error, run_preview.error || '');
      if (run_preview.error) {
        assert.equal(await app.confirmRun(), undefined);
        assert.deepEqual(writes(requests), []);
      }
    });
  }
});

test('managed Open uses the popover, restores focus, and posts bodyless /run from saved settings', async t => {
  for (const blocked of [true, false]) {
    await t.test(blocked ? 'blocked' : 'ready', async st => {
      const fields = { blockers: blocked ? ['prereq'] : [] };
      const app = setup(fields);
      const requests = stubFetch(st, app, (ticket, method) => method === 'POST' ? { ...ticket, status: 'todo' } : ticket);
      const focus = [];
      app.$refs = { runConfirmTrigger: { focus: () => focus.push('trigger') }, runConfirmPrimary: { focus: () => focus.push('primary') } };
      await app.startEditing();
      await app.openRunConfirm(app.selectedTicket);
      assert.equal(app.runConfirmOpen, true);
      assert.deepEqual(focus, ['primary']);
      assert.deepEqual(writes(requests), []);
      app.closeRunConfirm(true);
      assert.equal(app.runConfirmOpen, false);
      assert.deepEqual(focus, ['primary', 'trigger']);
      await app.openRunConfirm(app.selectedTicket);
      const result = await app.confirmRun();
      assert.equal(result.outcome, 'success');
      assert.equal(result.command, 'queue-saved');
      assert.deepEqual(writes(requests), [{ url: '/api/tickets/subject/run', method: 'POST', body: undefined }]);
      assert.equal(app.initModal, false);
      assert.equal(app.runConfirmOpen, false);
      assert.equal(app.queueReason(app.selectedTicket).kind, blocked ? 'blocked' : 'queued');
    });
  }
});

test('Confirm waits for pending changes and reports why a reviewed run was not sent', async t => {
  for (const [name, change] of [
    ['stage', { stage: 'plan' }],
    ['branch', { run_preview: { ...savedPreview, branch: 'other' } }],
    ['repo', { run_preview: { ...savedPreview, repo: '/other' } }],
    ['agent', { run_preview: { ...savedPreview, agent: 'other' } }],
    ['blockers', { blockers: ['prereq'] }],
  ]) {
    await t.test(name, async st => {
      const app = setup();
      const requests = stubFetch(st, app);
      await app.openRunConfirm(app.selectedTicket);
      app.lifecycleDraftBarrier = async () => {
        Object.assign(app.tickets[0], change);
        return true;
      };
      const result = await app.confirmRun();
      assert.equal(result.outcome, 'obsolete');
      assert.equal(app.runConfirmOpen, true);
      assert.match(app.runConfirmView().error, /ticket changed/i);
      assert.deepEqual(writes(requests), []);
    });
  }
});

test('imported, startable epic and scheduled tickets open the setup form', async t => {
  const cases = [
    ['imported', { kontora: false }],
    ['epic', { kind: 'epic' }],
    ['scheduled', { scheduled_at: '2030-01-02T03:04:05Z' }],
  ];
  for (const [name, fields] of cases) {
    await t.test(name, async st => {
      const app = setup(fields);
      const requests = stubFetch(st, app);
      const result = await app.openRunConfirm(app.selectedTicket);
      assert.equal(result.command, 'start');
      assert.equal(result.outcome, 'initialized');
      assert.equal(app.runConfirmOpen, false);
      assert.equal(app.initModal, true);
      assert.equal(app.initForm.ticketId, 'subject');
      assert.deepEqual(writes(requests), []);
    });
  }
});

test('Run blocker first uses the saved-settings, start-form and retry paths', async t => {
  const cases = [
    ['managed Open', {}, '/run', 'queue-saved', 'success'],
    ['imported Open', { kontora: false }, '', 'start', 'initialized'],
    ['scheduled Open', { scheduled_at: '2030-01-02T03:04:05Z' }, '', 'start', 'initialized'],
    ['paused', { status: 'paused' }, '/retry', 'retry', 'success'],
  ];
  for (const [name, fields, endpoint, command, outcome] of cases) {
    await t.test(name, async st => {
      const blocker = { id: holder.id, status: 'open', kontora: true, ...fields };
      const app = setup({ blockers: [holder.id] }, { nodes: [{ ...holder, status: blocker.status }], tickets: [blocker] });
      const requests = stubFetch(st, app, (ticket, method) => method === 'POST' ? { ...ticket, status: 'todo' } : ticket);
      await app.openRunConfirm(app.selectedTicket);
      const result = await app.runBlockerFirst();
      assert.equal(result.command, command);
      assert.equal(result.outcome, outcome);
      assert.equal(requests.at(-1).url, '/api/tickets/prereq' + endpoint);
      assert.equal(app.selectedTicket.status, 'open');
      assert.equal(app.runConfirmOpen, false);
      assert.equal(app.initModal, !endpoint);
      if (!endpoint) assert.equal(app.initForm.ticketId, 'prereq');
    });
  }
});

test('a stale, missing or non-primary chain holder cannot start a primary namesake', async t => {
  const primary = { id: holder.id, kontora: true };
  const cases = [
    [holder, 'done'],
    [{ ...holder, source: 'other' }, 'open'],
    [{ ...holder, missing: true }, 'open'],
    [{ ...holder, direction: 'downstream' }, 'open'],
  ];
  for (const [node, status] of cases) {
    const app = setup({ blockers: [holder.id] }, { nodes: [node], tickets: [{ ...primary, status }] });
    const dispatch = t.mock.method(app, 'dispatchLifecycle', () => assert.fail('unexpected lifecycle command'));
    t.mock.method(app, '_readRunPreview', async () => app.selectedTicket);
    await app.openRunConfirm(app.selectedTicket);
    if (node.missing) assert.match(app.runConfirmView().blocker.body, /Couldn’t resolve ticket prereq/);
    assert.equal(await app.runBlockerFirst(), undefined);
    assert.equal(dispatch.mock.callCount(), 0);
  }
});
