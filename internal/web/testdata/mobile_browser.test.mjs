// Run through make test-browser against embedded assets, never a daemon.
import assert from 'node:assert/strict';
import { after, before, describe, test } from 'node:test';
import { createRequire } from 'node:module';
import { mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { chromium, webkit } from 'playwright';
import { expect } from 'playwright/test';

const require = createRequire(import.meta.url);
assert.equal(require('playwright/package.json').version, '1.65.0');
const base = process.env.KONTORA_BROWSER_URL;
assert.ok(base, 'Run make test-browser (the Go test provides embedded loopback assets)');
assert.equal(new URL(base).hostname, '127.0.0.1');
const assets = new Set(JSON.parse(process.env.KONTORA_BROWSER_ASSETS));
const widths = [320, 390, 767, 768, 769, 1023, 1024, 1280];
const themes = ['dark', 'light'];
const source = 'secondary';
const sourcePath = '/fixtures/team-tickets';
const unavailablePath = '/fixtures/' + 'unavailable-folder-'.repeat(12);
const longID = 'kon-' + 'long-identifier-'.repeat(12);
const tail = '\n## Notes\n\nProtected note identity: browser-note-1\n\n## Audit\nDo not edit this tail.\n';
const stamp = '2026-10-01T10:00:00Z';
const liveTitle = 'Running fixture with a long title explaining terminal keyboard layout and lifecycle behavior';
const liveBranch = 'fixture/mobile-terminal-keyboard-layout-and-input-lifecycle-with-long-metadata';
const liveAgent = 'fixture-agent-with-a-long-descriptive-name';
const supervisorID = 'abcdef-abcd';
const companionThreadID = 'k0123456789abcdef';
const ordinaryThreadID = 'a0123456789abcdef';
const companionTitle = 'Saved observation';
const ordinaryTitle = 'Saved ordinary chat';
const companionQuestion = 'Explain the saved attention.';
let browser;
const engine = process.env.KONTORA_BROWSER_ENGINE;
assert.ok(['chromium', 'webkit'].includes(engine), 'The Go runner must select a browser engine');
before(async () => { browser = await ({ chromium, webkit })[engine].launch({ headless: true }); });
after(async () => { await browser?.close(); });

function ticket(id, title, extra = {}) {
  const description = `# ${title}\n\nFixture description.\n\n`;
  return { id, title, status: 'open', kontora: true, source: 'primary',
    path: '/fixtures/repository-with-a-long-name', pipeline: 'standard', agent: 'fixture-agent',
    stages: ['plan', 'code'], stage: 'plan', created_at: stamp, updated_at: stamp,
    history: [], dependencies: [], children: [], links: [],
    description, description_tail: tail, body: description + tail,
    can_edit: true, can_control: true, ...extra };
}

// Keys include source and id so a primary PUT cannot save a secondary namesake.
function fixtures(options = {}) {
  const records = new Map();
  const assistantThreads = new Map((options.assistantThreads || []).map(thread => [thread.id, structuredClone(thread)]));
  const key = (folder, id) => `${folder}/${id}`;
  const put = t => records.set(key(t.source, t.id), t);
  put(ticket('kon-draft', 'Primary draft', options.ticket));
  put(ticket(longID, 'Long identifier fixture'));
  put(ticket('kon-archived', 'Archived fixture', { status: 'done', archived_at: stamp }));
  put(ticket('kon-draft', 'Secondary namesake', { source, kontora: false,
    can_control: false, can_claim: false, refusal_reason: 'Fixture is edit-only',
    description: 'Secondary-only description.\n', body: 'Secondary-only description.\n' + tail }));
  put(ticket('kon-live', liveTitle, { status: 'in_progress', stage: 'code', stages: ['plan', 'code', 'test'], started_at: stamp,
    branch: liveBranch, agent: liveAgent, can_edit: false,
    history: [{ stage: 'plan', run: 0, agent: liveAgent, started_at: stamp, completed_at: stamp, exit_code: 0 }] }));
  put(ticket(supervisorID, 'Supervised fixture epic', { kind: 'epic', epic_mode: 'agent', epic_execution: 'agent',
    status: 'in_progress', stage: 'code', started_at: stamp, can_edit: false }));
  put(ticket('kon-done', 'Completed fixture', { status: 'done', stage: 'code', can_edit: false,
    summary: 'Second code run complete.', branch: 'fixture/done', history: [
      { stage: 'code', run: 0, agent: 'fixture-agent', started_at: stamp, completed_at: stamp,
        exit_code: 1, run_outcome: 'failed', summary: 'First code run needs changes.' },
      { stage: 'code', run: 1, agent: 'fixture-agent', started_at: stamp, completed_at: stamp,
        exit_code: 0, run_outcome: 'complete', summary: 'Second code run complete.' },
    ] }));
  put(ticket('kon-epic', 'Passive fixture epic', { kind: 'epic', epic_mode: 'passive' }));
  const config = { config_path: '/fixtures/config.yaml', pipelines: ['standard'], default_pipeline: 'standard',
    pipeline_infos: [{ name: 'standard', stages: ['plan', 'code'] }],
    agents: ['fixture-agent'], agent_infos: [{ name: 'fixture-agent', binary: 'pi' }],
    projects: [{ name: 'Fixture repository', path: '/fixtures/repository-with-a-long-name', agent: 'fixture-agent', pipeline: 'standard' }],
    custom_statuses: [], stages: ['plan', 'code'], channels: [], default_channels: [], ...options.config };
  const requests = [], mutations = [], sockets = [], socketRoutes = [], messages = [], violations = [], expectedHTTPFailures = [];
  const behavior = { createFailure: null, createWait: null, saveFailure: null, outputRevision: 0, activityEvents: null, bracketedPaste: false,
    automationFailure: null, automationWait: null, automationsAvailable: true, loseCreate: false, loseRun: false,
    unavailableFolder: false, sourcesFailure: false, configFailure: options.configFailure || false,
    assistantConfig: options.assistantConfig || { enabled: true, hint: '', autonomy: 'ask' },
    companionSnapshot: structuredClone(options.companionSnapshot || { enabled: true, name: 'Kotik', health: 'ok', ai_available: false, items: [], revision: 1 }),
    companionWait: options.companionWait || null, companionFailure: null, assistantStopFailure: null };
  let releaseConfig = () => {}, configReads = 0;
  const configWait = options.configRetryPending ? new Promise(resolve => { releaseConfig = resolve; }) : null;
  const fires = ['2026-11-10T08:00:00Z', '2026-11-11T08:00:00Z', '2026-11-12T08:00:00Z', '2026-11-13T08:00:00Z'];
  const runs = Array.from({ length: 29 }, (_, i) => ({ automation_id: 'auto-review', key: 'repository-review', n: 29 - i, revision: 3,
    invocation_id: 'fixture-' + i, phase: 'terminal', outcome: ['complete', 'failed', 'skipped'][i % 3], trigger: i % 2 ? 'manual' : 'schedule',
    scheduled_for: fires[0], accepted_at: fires[0], started_at: fires[0], completed_at: '2026-11-10T08:04:12Z',
    exit_code: i % 3 === 1 ? 1 : 0, result: 'Fixture result for run ' + (29 - i), cost: i % 3 === 2 ? null : '0.31' }));
  const definitions = new Map([['auto-review', { id: 'auto-review', key: 'repository-review', revision: 1,
    name: 'Repository review', project: 'Fixture repository', path: config.projects[0].path, agent: 'fixture-agent', model: 'sonnet', prompt: 'Review recent changes. {{ .Run.Number }}',
    schedule: '0 9 * * 1-5', schedule_human: 'Every weekday at 09:00', timezone: 'Europe/Berlin', workspace: 'worktree', overlap: 'skip', timeout: '30m',
    enabled: false, running: false, created_at: stamp, next: fires[0], next_runs: fires, last_run: runs[0], runs_total: 214, success_rate: 95, avg_wall: 252,
    spend_30d: '12.34', priced_runs: 19, tracked_runs: 20 }]]);
  const receipts = new Map(), runReceipts = new Map();
  const specFields = ['agent', 'enabled', 'model', 'name', 'overlap', 'path', 'prompt', 'schedule', 'timeout', 'timezone', 'workspace'];
  const spec = value => assert.deepEqual(Object.keys(value).sort(), specFields);
  let rawConfig = 'tickets_dir: /fixtures/primary\nauto_pick_up: false\nmax_concurrent_agents: 3\nagents:\n  fixture-agent:\n    binary: pi\n';
  const list = folder => [...records.values()].filter(t => t.source === folder && !t.archived_at);
  const json = (route, value, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });

  async function api(route, url) {
    const request = route.request(), method = request.method(), path = url.pathname;
    requests.push(`${method} ${path}`);
    const allowedQuery = path === '/api/stats' ? ['range', 'project', 'pipeline']
      : /\/runs$/.test(path) ? ['offset', 'limit']
      : /\/runs\/\d+\/log$/.test(path) ? ['raw']
      : /^\/api\/assistant\/threads\/[^/]+\/activity$/.test(path) ? ['after']
      : /\/(logs|activity)$/.test(path) ? ['stage', 'run', 'offset', 'limit', 'after'] : [];
    assert.ok([...url.searchParams.keys()].every(k => allowedQuery.includes(k)), `Unexpected query: ${url}`);
    if (path === '/api/automations' && method === 'GET') return json(route, { available: behavior.automationsAvailable,
      error: behavior.automationsAvailable ? undefined : 'Automation store locked', definitions: [...definitions.values()] });
    if (path === '/api/automations/preview' && method === 'POST') {
      const schedule = request.postDataJSON();
      assert.deepEqual(Object.keys(schedule).sort(), ['schedule', 'timezone']);
      assert.equal(typeof schedule.schedule, 'string'); assert.ok(schedule.timezone);
      if (schedule.timezone === 'Invalid/Zone') {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: 'Invalid timezone' }, 400);
      }
      return json(route, { occurrences: fires, human: 'Every weekday at 09:00' });
    }
    if (path === '/api/automations' && method === 'POST') {
      if (behavior.automationWait) await behavior.automationWait;
      if (behavior.automationFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.automationFailure.error }, behavior.automationFailure.status);
      }
      const body = request.postDataJSON();
      assert.deepEqual(Object.keys(body).sort(), ['definition', 'key', 'request_id']);
      assert.ok(body.request_id && body.key); spec(body.definition);
      if (receipts.has(body.request_id)) return json(route, receipts.get(body.request_id));
      const definition = { ...body.definition, key: body.key, id: 'auto-created-' + receipts.size, revision: 1, running: false, created_at: stamp,
        next: fires[0], next_runs: fires, schedule_human: 'Every weekday at 09:00', runs_total: 0, success_rate: null, avg_wall: null, spend_30d: null };
      definitions.set(definition.id, definition); receipts.set(body.request_id, structuredClone(definition));
      mutations.push({ method, path, body });
      if (behavior.loseCreate) { behavior.loseCreate = false; expectedHTTPFailures.push(base + path); return json(route, { error: 'Create response lost after commit' }, 503); }
      return json(route, definition, 201);
    }
    const automationMatch = path.match(/^\/api\/automations\/([^/]+)(?:\/(run|stop|pause|resume|runs)(?:\/(\d+)\/(log|terminal))?)?$/);
    if (automationMatch) {
      const definition = definitions.get(automationMatch[1]), action = automationMatch[2];
      if (!definition) { expectedHTTPFailures.push(base + path); return json(route, { error: 'Automation not found' }, 404); }
      if (method === 'GET' && action === 'runs') {
        const history = runs.filter(run => run.automation_id === definition.id);
        if (automationMatch[3]) {
          const n = Number(automationMatch[3]); assert.ok(history.some(run => run.n === n));
          if (automationMatch[4] === 'terminal') {
            assert.ok(definition.running && definition.current_run.n === n);
            return json(route, { session: 'fixture-automation-session', window: '@7', identity: { window_id: '@7', session_id: '$3' } });
          }
          const lines = [{ at: fires[0], source: 'kontora', kind: 'lifecycle', text: `automation ${definition.id} · run #${n} · trigger schedule` },
            { at: fires[0], source: 'agent', kind: 'tool_call', text: 'Inspect repository' },
            { at: fires[0], source: 'agent', kind: 'success', text: 'No regressions <script>not markup</script>' }];
          const raw = lines.map(line => line.text).join('\n') + '\n';
          if (url.searchParams.get('raw') === '1') return route.fulfill({ contentType: 'text/plain; charset=utf-8', body: raw });
          return json(route, { lines, raw });
        }
        const offset = Number(url.searchParams.get('offset')), limit = Number(url.searchParams.get('limit'));
        assert.ok(limit >= 20 && limit <= 100); return json(route, { runs: history.slice(offset, offset + limit), total: history.length });
      }
      if (method === 'GET' && !action) return json(route, definition);
      const body = request.postDataJSON();
      mutations.push({ method, path, body });
      if (method === 'POST' && action === 'run') {
        assert.deepEqual(Object.keys(body), ['request_id']); assert.ok(body.request_id);
        const key = definition.id + ':' + body.request_id;
        if (!runReceipts.has(key)) {
          const run = { automation_id: definition.id, key: definition.key, n: Math.max(0, ...runs.filter(run => run.automation_id === definition.id).map(run => run.n)) + 1,
            revision: 1, invocation_id: body.request_id, phase: definition.running && definition.overlap === 'skip' ? 'terminal' : definition.running && definition.overlap === 'queue' ? 'queued' : 'running',
            outcome: definition.running && definition.overlap === 'skip' ? 'skipped' : '', trigger: 'manual', accepted_at: fires[0], started_at: fires[0] };
          runs.unshift(run); runReceipts.set(key, run);
          if (run.phase === 'running') definitions.set(definition.id, { ...definition, running: true, current_run: run, runs_total: (definition.runs_total || 0) + 1 });
        }
        if (behavior.loseRun) { behavior.loseRun = false; expectedHTTPFailures.push(base + path); return json(route, { error: 'Run response lost after allocation' }, 503); }
        return json(route, runReceipts.get(key), 202);
      }
      if (method === 'POST' && action === 'stop') {
        assert.deepEqual(body, { n: definition.current_run.n });
        const stopped = { ...definition.current_run, phase: 'terminal', outcome: 'failed', exit_code: 143, completed_at: '2026-11-10T08:04:12Z', result: 'stopped by user' };
        runs.splice(runs.findIndex(run => run.automation_id === definition.id && run.n === body.n), 1, stopped);
        definitions.set(definition.id, { ...definition, running: false, current_run: null, last_run: stopped }); return json(route, stopped, 202);
      }
      if (behavior.automationWait) await behavior.automationWait;
      if (behavior.automationFailure) { expectedHTTPFailures.push(base + path); return json(route, { error: behavior.automationFailure.error }, behavior.automationFailure.status); }
      if (body.revision !== definition.revision) { expectedHTTPFailures.push(base + path); return json(route, { error: 'Revision conflict' }, 409); }
      if (method === 'DELETE' && !action) { assert.deepEqual(Object.keys(body), ['revision']); definitions.delete(definition.id); return route.fulfill({ status: 204 }); }
      if (method === 'POST' && ['pause', 'resume'].includes(action)) {
        assert.deepEqual(Object.keys(body), ['revision']); const saved = { ...definition, enabled: action === 'resume', revision: definition.revision + 1 };
        definitions.set(saved.id, saved); return json(route, saved);
      }
      assert.equal(method, 'PUT'); assert.equal(action, undefined); assert.deepEqual(Object.keys(body).sort(), ['definition', 'revision']); spec(body.definition);
      const saved = { ...definition, ...body.definition, revision: definition.revision + 1 };
      definitions.set(saved.id, saved); return json(route, saved);
    }
    if (method === 'GET') {
      switch (path) {
        case '/api/config':
          if (behavior.configFailure || (options.configRetryPending && ++configReads === 1)) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: 'Configuration temporarily unavailable' }, 503);
          }
          if (configWait) await configWait;
          return json(route, config);
        case '/api/ticket-sources':
          if (behavior.sourcesFailure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: 'Sources offline' }, 503);
          }
          return json(route, [
            { id: 'primary', path: '/fixtures/primary', available: true },
            { id: source, path: sourcePath, available: true },
            ...(behavior.unavailableFolder ? [{ id: 'unavailable', path: unavailablePath, available: false, error: 'Fixture folder unavailable' }] : []),
          ]);
        case '/api/tickets': return json(route, { tickets: list('primary'), running_agents: 1 });
        case `/api/sources/${source}/tickets`: return json(route, list(source));
        case '/api/assistant': return json(route, behavior.assistantConfig);
        case '/api/assistant/threads': return json(route, { threads: [...assistantThreads.values()].map(({ messages, ...thread }) => thread) });
        case '/api/assistant/companion':
          if (behavior.companionWait) await behavior.companionWait;
          if (behavior.companionFailure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: behavior.companionFailure }, 503);
          }
          return json(route, behavior.companionSnapshot);
        case '/api/events': return route.fulfill({ contentType: 'text/event-stream', body: 'retry: 3600000\n\n: offline fixture\n\n' });
        case '/api/config/raw': return json(route, { content: rawConfig });
        case '/api/stats': return json(route, { totals: { done: 1, runs: 2, first_pass_pct: 50 }, live: {}, days: [], buckets: [], stages: [], agents: [], projects: [] });
        case '/api/tickets/archived': return json(route, { tickets: [records.get('primary/kon-archived')] });
      }
    }
    if (method === 'PUT' && path === '/api/config/raw') {
      if (behavior.saveFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.saveFailure }, 503);
      }
      rawConfig = request.postDataJSON().content;
      return route.fulfill({ status: 204 });
    }
    if (method === 'POST' && path === '/api/tickets') {
      if (behavior.createWait) await behavior.createWait;
      if (behavior.createFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.createFailure }, 503);
      }
      const body = request.postDataJSON();
      assert.ok(body.title && body.path, 'Creation requires title and path');
      const created = ticket(`kon-created-${mutations.length}`, body.title, { ...body, description: body.body || '', description_tail: '', body: body.body || '' });
      put(created);
      mutations.push({ method, path, body, ticket: structuredClone(created) });
      return json(route, created, 201);
    }
    const assistantMatch = path.match(/^\/api\/assistant\/threads\/([^/]+)(?:\/(activity|stream|stop))?$/);
    if (assistantMatch) {
      const id = decodeURIComponent(assistantMatch[1]), action = assistantMatch[2];
      const thread = assistantThreads.get(id);
      assert.ok(thread, `Unknown assistant thread: ${id}`);
      if (method === 'GET' && !action) return json(route, thread);
      if (method === 'GET' && action === 'activity') {
        const after = url.searchParams.get('after');
        assert.ok(after !== null && /^\d+$/.test(after), 'Assistant activity requires a numeric cursor');
        return json(route, { running: thread.running, autonomy: thread.autonomy, messages: thread.messages,
          evidence: thread.evidence, offset: 0, tape: { events: [{ kind: 'text', text: `Recorded reply for ${thread.title}.` }] },
          partial: thread.running ? 'An explanation is still running.' : '', partial_gen: thread.running ? 1 : 0 });
      }
      if (method === 'GET' && action === 'stream') {
        return route.fulfill({ contentType: 'text/event-stream', body: thread.running
          ? 'retry: 3600000\n\nevent: reset\ndata: {"gen":1,"text":"An explanation is still running."}\n\n'
          : 'event: done\ndata: {}\n\n' });
      }
      if (method === 'POST' && action === 'stop') {
        assert.ok(thread.running, 'Stop must target a running assistant thread');
        assert.equal(request.postData(), null);
        if (behavior.assistantStopFailure) {
          expectedHTTPFailures.push(base + path);
          return json(route, { error: behavior.assistantStopFailure }, 503);
        }
        thread.running = false;
        mutations.push({ method, path });
        return route.fulfill({ status: 204 });
      }
    }
    const match = path.match(/^\/api(?:\/sources\/([^/]+))?\/tickets\/([^/]+)(?:\/(cost|chain|changes|logs|activity|move|pause|retry|init))?$/);
    if (match) {
      const folder = match[1] ? decodeURIComponent(match[1]) : 'primary';
      const id = decodeURIComponent(match[2]), action = match[3];
      const record = records.get(key(folder, id));
      if (folder === 'primary' && id === 'kon-deleted' && !action && method === 'GET') {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: 'Ticket not found' }, 404);
      }
      assert.ok(record, `Unknown source-qualified ticket: ${folder}/${id}`);
      if (method === 'GET' && !action) return json(route, record.archived_at ? { ...record, status: 'archived', archived_from: record.status } : record);
      if (method === 'PUT' && !action) {
        const body = request.postDataJSON();
        assert.ok(!('body' in body), 'Description edits must not replace the protected tail');
        Object.assign(record, body);
        record.body = record.description + record.description_tail;
        mutations.push({ method, path, body, ticket: structuredClone(record) });
        return json(route, record);
      }
      assert.equal(folder, 'primary', 'Secondary tickets cannot use control/output endpoints');
      if (method === 'GET') {
        switch (action) {
          case 'cost': if (id === 'kon-live') return json(route, { id, cost_usd: '0.42', tracked_runs: 3, priced_runs: 2,
            stages: [{ name: 'plan', cost_usd: '0.30', tracked_runs: 1, priced_runs: 1 },
              { name: 'code', cost_usd: '0.12', tracked_runs: 2, priced_runs: 1 }], runs: [] });
            return json(route, { id, cost_usd: record.history.length || record.status === 'in_progress' ? '0.12' : null,
            tracked_runs: record.history.length || (record.status === 'in_progress' ? 1 : 0),
            priced_runs: record.history.length || (record.status === 'in_progress' ? 1 : 0), stages: [], runs: [] });
          case 'chain': return json(route, { nodes: [], verdict: '' });
          case 'changes': return json(route, { base: 'main', files: [], commits: [] });
          case 'logs': return json(route, { content: `${id} ${url.searchParams.get('stage') || 'code'} fixture log revision ${behavior.outputRevision}\n` });
          case 'activity': return json(route, { live: record.status === 'in_progress', source: 'events',
            tape: { events: behavior.activityEvents || [{ kind: 'text', text: `Fixture activity revision ${behavior.outputRevision}` }] } });
        }
      }
      if (method === 'POST' && ['move', 'pause', 'retry', 'init'].includes(action)) {
        const body = request.postData() ? request.postDataJSON() : {};
        record.status = action === 'move' ? body.status : action === 'pause' ? 'paused' : 'todo';
        mutations.push({ method, path, body });
        return json(route, record);
      }
    }
    throw new Error(`Unmodelled API request: ${method} ${url}`);
  }

  async function install(context) {
    // Install BOTH interception layers before creating/navigating the page.
    // No catch-all API success, external fallthrough, service worker or live WS.
    await context.route('**/*', async route => {
      const url = new URL(route.request().url());
      try {
        assert.equal(url.origin, base, `External request: ${url}`);
        if (url.pathname.startsWith('/api/')) return await api(route, url);
        assert.equal(route.request().method(), 'GET');
        // index.html names its scripts and stylesheets with a ?v= content hash.
        assert.ok(assets.has(url.pathname) && (!url.search || /^\?v=[0-9a-f]+$/.test(url.search)), `Unknown asset: ${url}`);
        return await route.continue();
      } catch (error) {
        violations.push(error.message);
        await route.abort('blockedbyclient');
      }
    });
    await context.routeWebSocket('**/*', ws => {
      try {
        const url = new URL(ws.url());
        assert.equal(url.origin, base.replace('http:', 'ws:'));
        assert.ok(['/ws/terminal/kon-live', '/ws/terminal/' + supervisorID].includes(url.pathname)
          || /^\/ws\/automations\/[^/]+\/\d+$/.test(url.pathname), `Unknown socket: ${url}`);
        const paneStream = !url.pathname.startsWith('/ws/automations/');
        if (!paneStream) {
          const [, , , id, n] = url.pathname.split('/');
          const row = definitions.get(id); assert.ok(row?.running); assert.equal(Number(n), row.current_run.n);
        }
        assert.ok([...url.searchParams.keys()].every(k => ['cols', 'rows', 'rw', 'stream'].includes(k)));
        assert.equal(url.searchParams.get('stream'), paneStream ? 'pane' : null);
        assert.ok(Number(url.searchParams.get('cols')) > 0 && Number(url.searchParams.get('rows')) > 0);
        sockets.push(url);
        socketRoutes.push(ws);
        // Do not call connectToServer(): there is no terminal server at all.
        const snapshot = (cols, rows) => {
          if (paneStream) ws.send(JSON.stringify({ type: 'reset', cols, rows }));
          ws.send(Buffer.from('Offline fixture terminal\r\n$ ' + (behavior.bracketedPaste ? '\x1b[?2004h' : '')));
        };
        ws.onMessage(message => {
          try {
            const data = JSON.parse(String(message));
            assert.ok(['resize', 'input'].includes(data.type));
            messages.push({ url, ...data });
            if (data.type === 'resize') {
              assert.ok(Number.isInteger(data.cols) && data.cols > 0 && Number.isInteger(data.rows) && data.rows > 0);
              snapshot(data.cols, data.rows);
            }
          } catch (error) { violations.push(error.message); }
        });
        snapshot(Number(url.searchParams.get('cols')), Number(url.searchParams.get('rows')));
      } catch (error) {
        violations.push(error.message);
        ws.close({ code: 1008, reason: 'Unknown fixture socket' });
      }
    });
    // Routed WebSockets open immediately. Hold the application's open callback
    // and readyState together to exercise a connection still in progress.
    await context.addInitScript(() => {
      Object.defineProperty(navigator, 'clipboard', { value: { writeText: async text => { window.__fixtureCopied = text; } } });
      const Socket = window.WebSocket;
      window.__fixtureSockets = [];
      window.__fixturePendingOpens = [];
      window.WebSocket = class extends Socket {
        constructor(...args) {
          super(...args);
          window.__fixtureSockets.push(this);
          const descriptor = Object.getOwnPropertyDescriptor(this, 'onopen');
          let handler;
          const deliver = event => {
            if (window.__fixtureHoldOpen) {
              const readyState = Object.getOwnPropertyDescriptor(this, 'readyState');
              Object.defineProperty(this, 'readyState', { configurable: true, value: Socket.CONNECTING });
              window.__fixturePendingOpens.push(() => {
                if (readyState) Object.defineProperty(this, 'readyState', readyState);
                else delete this.readyState;
                handler?.call(this, event);
              });
            } else handler?.call(this, event);
          };
          if (descriptor?.set) {
            Object.defineProperty(this, 'onopen', { configurable: true, set(fn) { handler = fn; descriptor.set.call(this, deliver); } });
          } else {
            Object.defineProperty(this, 'onopen', { configurable: true, set(fn) { handler = fn; } });
            this.addEventListener('open', deliver);
          }
        }
      };
    });
  }
  return { install, records, definitions, runs, receipts, runReceipts, assistantThreads, requests, mutations, sockets, socketRoutes, messages, violations, behavior, expectedHTTPFailures, releaseConfig };
}

async function withPage(t, width, theme, run, options = {}) {
  const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: theme,
    reducedMotion: 'reduce', serviceWorkers: 'block', locale: 'en-US', timezoneId: 'UTC' });
  const fixture = fixtures(options), errors = [];
  await fixture.install(context);
  await context.addInitScript(({ theme, sidebar, savedThread, assistantOpen }) => {
    localStorage.setItem('kontora-theme', theme);
    localStorage.setItem('kontora-assistant-open', assistantOpen ? '1' : '0');
    if (savedThread) localStorage.setItem('kontora-assistant-thread', savedThread);
    localStorage.setItem('kontora-sidebar-hidden', sidebar ? '0' : '1');
    window.__fixtureSSE = [];
    window.EventSource = class extends EventTarget {
      constructor(url) {
        super(); this.url = url; this.closed = false; window.__fixtureSSE.push(this);
        fetch(url).then(() => { if (!this.closed) this.emit('open'); });
      }
      emit(type, data = {}) { if (!this.closed) this.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(data) })); }
      close() { this.closed = true; }
    };
  }, { theme, sidebar: options.sidebar, savedThread: options.savedThread, assistantOpen: options.assistantOpen });
  const page = await context.newPage();
  page.setDefaultTimeout(4000);
  page.setDefaultNavigationTimeout(15000);
  page.on('pageerror', error => errors.push(error.stack || error.message));
  page.on('console', msg => {
    if (msg.type() !== 'error') return;
    if (msg.text().startsWith('Failed to load resource:') && fixture.expectedHTTPFailures.includes(msg.location().url)) return;
    errors.push(msg.text());
  });
  try {
    await page.goto(base, { waitUntil: 'domcontentloaded' });
    await ready(page);
    await run(page, fixture);
  } finally {
    fixture.releaseConfig();
    await context.close();
    assert.deepEqual(fixture.violations, [], 'Unexpected requests (all must be explicitly modelled)');
    assert.deepEqual(errors, [], 'Browser/Alpine/console errors');
  }
}

// Bindings are used only to locate existing controls; tests never call mutating
// component methods or replace application state. :visible excludes x-show's inactive UI.
const model = (page, name) => page.locator(`[x-model="${name}"]:visible`);
async function visible(locator) { await locator.waitFor({ state: 'visible' }); }
async function inViewport(locator) {
  await visible(locator);
  const box = await locator.boundingBox();
  const size = locator.page().viewportSize();
  assert.ok(box && box.width > 0 && box.height > 0 && box.x >= -1 && box.y >= -1 &&
    box.x + box.width <= size.width + 1 && box.y + box.height <= size.height + 1,
  `Control outside viewport: ${JSON.stringify(box)} in ${JSON.stringify(size)}`);
}
async function noPageOverflow(page) {
  const duplicateIDs = await page.evaluate(() => {
    const seen = new Set();
    return [...document.querySelectorAll('[id]')].map(el => el.id).filter(id => {
      if (!id) return false;
      if (seen.has(id)) return true;
      seen.add(id);
      return false;
    });
  });
  assert.deepEqual(duplicateIDs, [], 'Shared layouts must not duplicate DOM IDs');
  const dimensions = await page.evaluate(() => ({ viewport: innerWidth,
    html: document.documentElement.scrollWidth, body: document.body.scrollWidth }));
  assert.ok(dimensions.html <= dimensions.viewport + 1 && dimensions.body <= dimensions.viewport + 1,
    `Page-level horizontal overflow: ${JSON.stringify(dimensions)}`);
}
async function ready(page) {
  await page.waitForFunction(() => {
    if (!window.Alpine) return false;
    const app = Alpine.$data(document.body);
    return !app.loading && app._applyingRoute === null && !app.detailLoading && !app.settingsLoading && !app.automationsLoading && !app.archiveDetailLoading;
  });
  await page.evaluate(() => document.fonts.ready);
}
async function routeTo(page, route) {
  await ready(page);
  await page.evaluate(route => { location.hash = route; }, route);
  await page.waitForFunction(route => {
    const app = Alpine.$data(document.body);
    return location.hash === route && app.routeHash() === route && app._applyingRoute === null;
  }, route);
  await ready(page);
}
async function openTicket(page, id, folder = 'primary') {
  await routeTo(page, folder === 'primary' ? `#/t/${id}` : `#/s/${folder}/t/${id}`);
  await page.waitForFunction(({ id, folder }) => {
    const app = Alpine.$data(document.body);
    return app.selectedTicket?.id === id && (app.selectedTicket.source || 'primary') === folder && !app.detailLoading;
  }, { id, folder });
}
async function backToBoard(page) {
  await routeTo(page, '#/');
  await page.waitForFunction(() => !Alpine.$data(document.body).selectedTicket && !Alpine.$data(document.body).selectedEpic);
}

const automationButton = (page, name) => page.getByRole('button', { name, exact: true }).filter({ visible: true });
async function emitAutomationEvent(page, type = 'automations_updated') {
  await page.evaluate(type => window.__fixtureSSE.filter(stream => !stream.closed && stream.url === '/api/events').forEach(stream => stream.emit(type)), type);
  await ready(page);
}
async function automationPreviewReady(page) {
  await page.waitForFunction(() => { const app = Alpine.$data(document.body); return app.automationsPreview.length === 4 && !app.automationsPreviewLoading; });
}
async function automationSaveReady(page) {
  await page.waitForFunction(() => !!Alpine.$data(document.body).automationsCanSave());
}
async function automationDetailReady(page, id) {
  await page.waitForFunction(id => { const app = Alpine.$data(document.body); return app.automationsSelected === id && app.automationsDetail && !app.automationsEditing && !app.automationsHistoryLoading; }, id);
  await ready(page);
}

describe('API-backed Automations', { concurrency: 2 }, () => {
  for (const width of [320, 390, 767, 768, 1280]) for (const theme of themes) {
    test(`routes, editor controls, retry, conflict and dirty guards / ${width}px / ${theme}`, { timeout: 60000 }, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await openTicket(page, 'kon-draft', source);
        await routeTo(page, '#/automations');
        await automationButton(page, 'new automation').first().click();
        await page.waitForURL('**/#/automations/new');
        const view = page.locator('.automations-view'), editor = page.locator('.automations-editor');
        assert.equal(await view.getByRole('switch', { name: 'Runs on schedule' }).getAttribute('aria-checked'), 'true');
        await model(page, 'automationsDraft.name').fill('Daily fixture review');
        await model(page, 'automationsDraft.prompt').fill('Review with ordinary agent permissions.');
        await model(page, 'automationsDraft.path').fill('/fixtures/repository-with-a-long-name');
        await model(page, 'automationsDraft.model').fill('opus');
        await model(page, 'automationsDraft.timeout').fill('45m');
        const agent = editor.locator('#automation-agent:visible');
        const fallback = editor.locator('select');
        if (await agent.count()) { await agent.click(); await page.getByRole('option', { name: /fixture-agent/ }).click(); }
        else if (await fallback.count()) await fallback.selectOption('fixture-agent');
        for (const name of ['every hour', 'every 6h', 'daily 09:00', 'weekdays 09:00', 'nightly 03:00', 'mondays 10:00']) {
          await automationButton(page, name).click(); assert.equal(await automationButton(page, name).getAttribute('aria-pressed'), 'true');
        }
        await model(page, 'automationsDraft.schedule').fill('0 9 * *');
        await visible(view.getByText(/cron takes 5 fields/));
        await model(page, 'automationsDraft.schedule').fill('30m');
        await model(page, 'automationsDraft.timezone').fill('Invalid/Zone');
        await visible(view.getByText('Invalid timezone', { exact: true }));
        await model(page, 'automationsDraft.schedule').fill('0 9 * * 1-5');
        await model(page, 'automationsDraft.timezone').fill('Europe/Berlin');
        await automationPreviewReady(page);
        assert.match(await editor.locator('ol').innerText(), /09:00/);
        for (const value of ['repo', 'worktree']) { await view.getByRole('group', { name: 'Workspace' }).getByRole('button', { name: value, exact: true }).click(); }
        for (const value of ['queue', 'replace', 'skip']) {
          const button = view.getByRole('group', { name: 'If still running' }).getByRole('button', { name: value, exact: true }); await button.click(); assert.equal(await button.getAttribute('aria-pressed'), 'true');
        }
        await view.getByRole('switch', { name: 'Runs on schedule' }).click();
        await noPageOverflow(page); await automationSaveReady(page);
        fixture.behavior.loseCreate = true;
        await automationButton(page, 'create automation').click();
        await visible(view.getByText(/Create response lost after commit/));
        assert.equal(await model(page, 'automationsDraft.name').isDisabled(), true);
        await automationButton(page, 'retry save').click();
        await automationDetailReady(page, 'auto-created-0'); assert.equal(fixture.definitions.size, 2);
        assert.equal(new URL(page.url()).hash, '#/automations/auto-created-0');
        const saved = fixture.definitions.get('auto-created-0'); assert.equal(saved.model, 'opus'); assert.equal(saved.timeout, '45m'); assert.equal(saved.enabled, false); assert.equal(saved.overlap, 'skip');
        await page.reload({ waitUntil: 'domcontentloaded' }); await automationDetailReady(page, saved.id);
        await automationButton(page, 'edit').click(); await page.waitForURL('**/#/automations/auto-created-0/edit');
        assert.equal(await model(page, 'automationsDraft.prompt').inputValue(), 'Review with ordinary agent permissions.');
        await model(page, 'automationsDraft.prompt').fill('My unsaved instructions');
        page.once('dialog', dialog => dialog.dismiss()); await automationButton(page, 'cancel').click();
        assert.equal(await model(page, 'automationsDraft.prompt').inputValue(), 'My unsaved instructions');
        page.once('dialog', dialog => dialog.dismiss()); await page.evaluate(() => { location.hash = '#/automations'; });
        await page.waitForURL('**/#/automations/auto-created-0/edit');
        fixture.definitions.set(saved.id, { ...saved, revision: 2, prompt: 'Saved by another browser.' });
        await automationSaveReady(page); await automationButton(page, 'save changes').click();
        await visible(automationButton(page, 'reload saved version'));
        assert.equal(await model(page, 'automationsDraft.prompt').inputValue(), 'My unsaved instructions');
        page.once('dialog', dialog => dialog.accept()); await automationButton(page, 'reload saved version').click();
        await page.waitForFunction(() => Alpine.$data(document.body).automationsDraft.prompt === 'Saved by another browser.');
        await model(page, 'automationsDraft.prompt').fill('SSE draft stays local');
        fixture.definitions.set(saved.id, { ...fixture.definitions.get(saved.id), revision: 3, prompt: 'SSE saved prompt' });
        await emitAutomationEvent(page); await visible(automationButton(page, 'reload saved version'));
        await emitAutomationEvent(page, 'open'); assert.equal(await model(page, 'automationsDraft.prompt').inputValue(), 'SSE draft stays local');
        page.once('dialog', dialog => dialog.accept()); await automationButton(page, 'cancel').click(); await automationDetailReady(page, saved.id);
        await noPageOverflow(page);
      });
    });
    test(`run history, logs, meters, session, stop and delete / ${width}px / ${theme}`, { timeout: 45000 }, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await routeTo(page, '#/automations/auto-review'); await automationDetailReady(page, 'auto-review');
        const detail = page.locator('.automations-detail');
        assert.match(await detail.locator('.automations-meters').innerText(), /214[\s\S]*95%[\s\S]*4m 12s/);
        assert.equal(await detail.locator('.automations-run-strip button').count(), 24);
        assert.match(await detail.locator('.automations-run-strip').innerText(), /8 complete · 8 failed · 8 skipped/);
        assert.match(await detail.locator('tbody tr').first().innerText(), /#29[\s\S]*complete[\s\S]*schedule[\s\S]*4m 12s[\s\S]*0[\s\S]*\$0.31[\s\S]*Fixture result for run 29/);
        await automationButton(page, 'show more').click(); await page.waitForFunction(() => Alpine.$data(document.body).automationsHistory.length === 29);
        await noPageOverflow(page);
        await automationButton(page, '#29').click(); await visible(detail.getByText('Inspect repository', { exact: true }));
        await noPageOverflow(page);
        await visible(detail.getByText('No regressions <script>not markup</script>', { exact: true }));
        assert.equal(await detail.locator('script').count(), 0);
        const raw = detail.getByRole('link', { name: '⧉ raw', exact: true });
        assert.equal(await raw.getAttribute('href'), '/api/automations/auto-review/runs/29/log?raw=1');
        const popupPromise = page.waitForEvent('popup'); await raw.click(); const popup = await popupPromise; await popup.waitForLoadState();
        assert.match(await popup.locator('body').innerText(), /automation auto-review · run #29/); await popup.close();
        await automationButton(page, '↓ older').click(); await visible(detail.getByText('run #28', { exact: true }));
        await automationButton(page, '↑ newer').click(); await visible(detail.getByText('run #29', { exact: true }));
        await automationButton(page, '← all runs').click();
        await detail.getByRole('tab', { name: 'prompt', exact: true }).click(); await visible(detail.getByText('Review recent changes. {{ .Run.Number }}', { exact: true }));
        await automationButton(page, width < 768 ? 'resume' : 'resume schedule').click();
        await page.waitForFunction(() => Alpine.$data(document.body).automationsRow()?.enabled);
        await automationButton(page, width < 768 ? 'pause' : 'pause schedule').click();
        await page.waitForFunction(() => !Alpine.$data(document.body).automationsRow()?.enabled);
        fixture.behavior.loseRun = true; await automationButton(page, 'run now').click();
        await visible(page.getByText(/Run response lost after allocation/).filter({ visible: true }).first());
        await automationButton(page, width < 768 ? 'run now' : 'retry run now').click();
        await page.waitForFunction(() => Alpine.$data(document.body).automationsTab === 'session' && Alpine.$data(document.body).terminalOpen);
        await visible(page.locator('#automation-terminal-session .xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await noPageOverflow(page);
        assert.equal(fixture.runReceipts.size, 1); const posts = fixture.mutations.filter(m => m.path.endsWith('/run'));
        assert.equal(posts.length, 2); assert.deepEqual(posts[0].body, posts[1].body);
        assert.ok(fixture.sockets.some(url => url.pathname === '/ws/automations/auto-review/30'));
        await automationButton(page, 'read-only').click(); await visible(automationButton(page, 'read-write'));
        await automationButton(page, 'read-write').click(); await visible(automationButton(page, 'read-only'));
        await automationButton(page, '⇣ follow').click();
        await automationButton(page, 'read-only').click();
        const running = fixture.definitions.get('auto-review');
        const replacement = { ...running.current_run, n: 31, invocation_id: 'fixture-sse-replacement' };
        fixture.runs.unshift(replacement); fixture.definitions.set(running.id, { ...running, current_run: replacement });
        await emitAutomationEvent(page);
        await page.waitForFunction(() => { const app = Alpine.$data(document.body); return app.terminalSubjectKey === 'automation:auto-review:31' && app.terminalConnected; });
        assert.ok(fixture.sockets.some(url => url.pathname === '/ws/automations/auto-review/31'));
        await visible(automationButton(page, 'read-only'));
        await automationButton(page, width < 768 ? 'stop' : '✕ stop run').click();
        await page.waitForFunction(() => !Alpine.$data(document.body).automationsRow()?.running);
        assert.equal(fixture.definitions.get('auto-review').last_run.result, 'stopped by user');
        assert.equal(fixture.mutations.findLast(m => m.path.endsWith('/stop')).body.n, 31);
        if (width >= 768) { await automationButton(page, 'More automation actions').click(); await page.getByRole('menuitem', { name: 'delete automation', exact: true }).click(); }
        else await automationButton(page, 'delete').click();
        const dialog = page.getByRole('dialog', { name: 'Delete automation', exact: true }); await visible(dialog);
        await dialog.getByRole('button', { name: 'cancel', exact: true }).click(); assert.equal(fixture.definitions.size, 1);
        if (width >= 768) { await automationButton(page, 'More automation actions').click(); await page.getByRole('menuitem', { name: 'delete automation', exact: true }).click(); }
        else await automationButton(page, 'delete').click();
        await dialog.getByRole('button', { name: 'delete automation', exact: true }).click();
        await page.waitForURL('**/#/automations'); assert.equal(fixture.definitions.size, 0);
        await visible(page.getByText(/deleted auto-review · logs kept/)); await noPageOverflow(page);
      });
    });
  }
});

async function automationScreenshot(page, label) {
  const directory = process.env.KONTORA_BROWSER_SCREENSHOTS || join(tmpdir(), 'kontora-automation-browser');
  await mkdir(directory, { recursive: true });
  const png = await page.screenshot({ path: join(directory, `${engine}-${label}.png`), animations: 'disabled' });
  assert.equal(png.subarray(1, 4).toString(), 'PNG'); assert.ok(png.length > 10000);
}
async function automationToken(locator, property, token) {
  const colors = await locator.evaluate((el, { property, token }) => {
    const probe = document.createElement('span'); probe.style.color = token.startsWith('--') ? `rgb(var(${token}))` : token; el.parentElement.append(probe);
    const expected = getComputedStyle(probe).color, actual = getComputedStyle(el)[property]; probe.remove(); return { actual, expected };
  }, { property, token });
  assert.equal(colors.actual, colors.expected, `${property} must use ${token}`);
}
function near(actual, expected, label) { assert.ok(Math.abs(actual - expected) <= 1, `${label}: ${actual}, expected ${expected}`); }

describe('desktop automation handoff', { concurrency: 2 }, () => {
  for (const width of [1280, 1600]) for (const theme of themes) {
    test(`visible sidebar, list/detail/editor geometry and tokens / ${width}px / ${theme}`, { timeout: 45000 }, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await routeTo(page, '#/automations');
        const sidebar = page.locator('#main-content > aside:visible'), view = page.locator('.automations-view');
        await inViewport(sidebar); near((await sidebar.boundingBox()).width, 220, 'sidebar');
        assert.equal(await sidebar.locator('nav > button').nth(1).getAttribute('aria-current'), 'page');
        const row = view.locator('.automations-table tbody tr').first(); near((await row.boundingBox()).height, 36, 'list row');
        near((await view.locator('.automations-table thead tr').boundingBox()).height, 28, 'table header');
        const toggle = row.getByRole('switch'); near((await toggle.boundingBox()).width, 1.75 * 17, 'row switch'); near((await toggle.boundingBox()).height, 17, 'row switch');
        await automationToken(view, 'backgroundColor', '--surface-frame');
        assert.match(await row.locator('td').nth(1).evaluate(el => getComputedStyle(el).fontFamily), /JetBrains Mono/);
        await noPageOverflow(page); await automationScreenshot(page, `${width}-${theme}-list`);
        await toggle.click(); await page.waitForFunction(() => Alpine.$data(document.body).automations[0].enabled);
        assert.equal(new URL(page.url()).hash, '#/automations');
        await row.getByRole('button', { name: /Fixture repository.*Repository review/ }).click(); await automationDetailReady(page, 'auto-review');
        const detail = view.locator('.automations-detail'), rail = detail.locator('.automations-rail');
        near((await detail.boundingBox()).x, 220, 'detail retains sidebar'); near((await rail.boundingBox()).width, 308, 'detail rail'); await inViewport(rail);
        near((await detail.locator('h1').evaluate(el => parseFloat(getComputedStyle(el).fontSize))), 19, 'identity title');
        near((await detail.locator('tbody tr').first().boundingBox()).height, 32, 'run row');
        near((await detail.locator('.automations-run-strip button').first().boundingBox()).height, 5, 'run strip');
        await automationToken(detail.locator('tbody tr').nth(0).locator('td').nth(1), 'color', '--ok');
        await automationToken(detail.locator('tbody tr').nth(1).locator('td').nth(1), 'color', 'hsl(var(--st-cancel))');
        await automationToken(detail.locator('tbody tr').nth(2).locator('td').nth(1), 'color', '--tx-faint');
        await automationToken(detail, 'backgroundColor', '--surface-950');
        assert.match(await rail.innerText(), /12.34[\s\S]*19 of 20 runs priced/); await noPageOverflow(page); await automationScreenshot(page, `${width}-${theme}-detail`);
        await automationButton(page, '#29').click(); await visible(detail.getByText('Inspect repository', { exact: true }));
        const log = detail.locator('.automations-log-line').first();
        const grid = await log.evaluate(el => getComputedStyle(el).gridTemplateColumns.split(' ').map(parseFloat));
        near(grid[0], 62, 'log time column'); near(grid[1], 58, 'log source column');
        await automationToken(detail.locator('.automations-log-line').nth(1).locator('span').last(), 'color', '--accent-bright');
        await automationToken(detail.locator('.automations-log-line').nth(2).locator('span').last(), 'color', '--ok');
        await automationButton(page, '← all runs').click();
        await automationButton(page, 'More automation actions').click(); await page.getByRole('menuitem', { name: 'duplicate', exact: true }).click();
        await page.waitForURL('**/#/automations/new');
        assert.equal(await model(page, 'automationsDraft.name').inputValue(), 'Repository review (copy)');
        assert.equal(await view.getByRole('switch', { name: 'Runs on schedule' }).getAttribute('aria-checked'), 'false');
        const editor = view.locator('.automations-editor'), preview = view.locator('aside:visible'); await visible(preview);
        near((await preview.boundingBox()).width, 420, 'YAML preview'); near((await view.boundingBox()).x, 220, 'editor retains sidebar');
        await page.waitForFunction(() => Alpine.$data(document.body).automationsYAML.includes('enabled: false'));
        assert.match(await preview.innerText(), /automations:[\s\S]*repository-review-copy:[\s\S]*prompt:[\s\S]*overlap: skip/);
        await automationToken(preview, 'backgroundColor', '--surface-deep'); await automationToken(model(page, 'automationsDraft.name'), 'backgroundColor', '--surface-800');
        assert.match(await editor.locator('label').first().evaluate(el => getComputedStyle(el).fontFamily), /JetBrains Mono/);
        for (const field of [model(page, 'automationsDraft.name'), model(page, 'automationsDraft.prompt')]) {
          await field.focus();
          const outline = await field.evaluate(el => {
            const style = getComputedStyle(el), probe = document.createElement('span');
            probe.style.color = 'rgba(var(--accent), .4)'; el.parentElement.append(probe);
            const expected = getComputedStyle(probe).color; probe.remove();
            return { style: style.outlineStyle, width: style.outlineWidth, color: style.outlineColor, expected };
          });
          assert.equal(outline.style, 'solid'); assert.equal(outline.width, '1px'); assert.equal(outline.color, outline.expected);
        }
        await page.waitForFunction(() => !Alpine.$data(document.body).automationsToast);
        await noPageOverflow(page); await automationScreenshot(page, `${width}-${theme}-editor`);
        await automationSaveReady(page); await automationButton(page, '▷ save & run now').click();
        await page.waitForFunction(() => Alpine.$data(document.body).automationsTab === 'session' && !Alpine.$data(document.body).automationsEditing);
        assert.equal(fixture.definitions.size, 2); assert.equal(fixture.runReceipts.size, 1);
        for (const phase of ['queued', 'preparing', 'recovery_required']) {
          await page.evaluate(phase => { const row = Alpine.$data(document.body).automationsRow(); row.running = false; row.current_run.phase = phase; }, phase);
          await visible(automationButton(page, '✕ stop run'));
        }
        await automationButton(page, '✕ stop run').click(); await page.waitForFunction(() => !Alpine.$data(document.body).automationsRow()?.running);
        await automationButton(page, 'edit').click(); await page.waitForURL('**/#/automations/auto-created-0/edit');
        await page.reload({ waitUntil: 'domcontentloaded' }); await ready(page); await visible(model(page, 'automationsDraft.name'));
        assert.equal(await model(page, 'automationsDraft.name').inputValue(), 'Repository review (copy)');
        await automationButton(page, 'cancel').click(); await automationDetailReady(page, 'auto-created-0');
        await sidebar.getByRole('button', { name: /Automations/ }).click(); await page.waitForURL('**/#/automations');
      }, { sidebar: true });
    });
  }
});

describe('historical primary ticket navigation from other views', () => {
  for (const width of [320, 1280]) test(`archive route reload and Back/Forward / ${width}px`, async t => {
    await withPage(t, width, 'dark', async (page, fixture) => {
      await openTicket(page, 'kon-draft', source); await routeTo(page, '#/automations');
      await page.evaluate(() => { location.hash = '#/t/kon-deleted'; });
      await visible(page.getByText('Historical ticket kon-deleted is missing. It may have been deleted.', { exact: true }));
      await routeTo(page, '#/automations');
      await page.evaluate(() => { location.hash = '#/archive/t/kon-archived'; });
      await page.waitForURL('**/#/archive/t/kon-archived'); await ready(page); await visible(page.locator('.archive-detail:visible'));
      assert.equal(await page.evaluate(() => Alpine.$data(document.body).selectedTicket.source), 'primary');
      await page.reload({ waitUntil: 'domcontentloaded' }); await ready(page); await visible(page.locator('.archive-detail:visible'));
      await page.goBack(); await page.waitForURL('**/#/automations'); await ready(page);
      await page.goForward(); await page.waitForURL('**/#/archive/t/kon-archived'); await ready(page); await visible(page.locator('.archive-detail:visible'));
      assert.ok(!fixture.requests.some(request => request.includes('/sources/secondary/tickets/kon-archived'))); await noPageOverflow(page);
    });
  });
});
async function swipeBoard(page, dx) {
  await page.locator('.mobile-board-layer [\\@touchend]').evaluate((el, dx) => {
    for (const [name, x] of [['touchstart', 160], ['touchend', 160 + dx]]) {
      const event = new Event(name, { bubbles: true });
      Object.defineProperty(event, 'changedTouches', { value: [{ clientX: x, clientY: 300 }] });
      el.dispatchEvent(event);
    }
  }, dx);
}

async function visualViewport(page, height, top = 0) {
  await page.evaluate(({ height, top }) => {
    Object.defineProperties(window.visualViewport, {
      height: { configurable: true, value: height }, offsetTop: { configurable: true, value: top },
    });
    window.visualViewport.dispatchEvent(new Event('resize'));
    window.visualViewport.dispatchEvent(new Event('scroll'));
  }, { height, top });
}

const terminalSelector = '#terminal-session:visible, #terminal-container-mobile:visible, #epic-terminal-session:visible, #epic-terminal-mobile:visible';
const terminalSubjects = ['kon-live', supervisorID];
let markerSequence = 0;

async function openTerminalSubject(page, id) {
  if (id === supervisorID) {
    await routeTo(page, '#/e/' + id);
    await page.getByRole('button', { name: 'session', exact: true }).filter({ visible: true }).click();
  } else await openTicket(page, id);
}

function terminalGrid({ marker, minRows = 1, diagnostic = false } = {}) {
  const term = window.termState.term;
  const host = document.getElementById(Alpine.$data(document.body).outputContainerID('terminal'));
  const screen = host?.querySelector('.xterm-screen');
  if (!host || !term?.element || !screen) return false;
  const box = host.getBoundingClientRect(), style = getComputedStyle(host);
  const content = {
    left: box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft),
    right: box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight),
    top: box.top + parseFloat(style.borderTopWidth) + parseFloat(style.paddingTop),
    bottom: box.bottom - parseFloat(style.borderBottomWidth) - parseFloat(style.paddingBottom),
  };
  const rect = screen.getBoundingClientRect();
  const cell = term._core._renderService.dimensions.css.cell;
  const grid = { left: rect.left, top: rect.top,
    right: rect.left + term.cols * cell.width, bottom: rect.top + term.rows * cell.height };
  const contains = (outer, inner, x = true, y = true) =>
    (!x || (inner.left >= outer.left - 1 && inner.right <= outer.right + 1)) &&
    (!y || (inner.top >= outer.top - 1 && inner.bottom <= outer.bottom + 1));
  const clips = [];
  for (let el = host; el; el = el.parentElement) {
    const css = getComputedStyle(el), bounds = el.getBoundingClientRect();
    const x = /^(hidden|clip|auto|scroll)$/.test(css.overflowX);
    const y = /^(hidden|clip|auto|scroll)$/.test(css.overflowY);
    if (x || y) clips.push({ id: el.id, x, y,
      left: bounds.left + parseFloat(css.borderLeftWidth), right: bounds.right - parseFloat(css.borderRightWidth),
      top: bounds.top + parseFloat(css.borderTopWidth), bottom: bounds.bottom - parseFloat(css.borderBottomWidth) });
    // The mobile overlay is fixed to the viewport, outside its ancestors' overflow clips.
    if (css.position === 'fixed') break;
  }
  const proposed = window.termState.fit.proposeDimensions();
  const lastRow = term.buffer.active.getLine(term.buffer.active.baseY + term.rows - 1)?.translateToString(true);
  const fits = cell.width > 0 && cell.height > 0 && term.rows >= minRows && term.cols > 0 &&
    rect.width > 0 && rect.height > 0 && content.right > content.left && content.bottom > content.top &&
    term.element.parentElement === host && proposed?.rows === term.rows && proposed?.cols === term.cols &&
    contains(rect, grid) && contains(content, grid) && contains(content, rect) &&
    clips.every(clip => contains(clip, grid, clip.x, clip.y) && contains(clip, rect, clip.x, clip.y)) &&
    contains({ left: 0, top: 0, right: innerWidth, bottom: innerHeight }, rect) &&
    (!marker || (lastRow === marker && term.buffer.active.viewportY === term.buffer.active.baseY));
  return diagnostic ? { fits, host: host.id, content, screen: rect.toJSON(), grid,
    cell, rows: term.rows, cols: term.cols, proposed, clips, minRows, marker, lastRow,
    baseY: term.buffer.active.baseY, viewportY: term.buffer.active.viewportY } : fits;
}

async function terminalGridFits(page, fixture, minRows = 1) {
  const wait = async marker => {
    await page.waitForFunction(terminalGrid, { marker, minRows }).catch(async error => {
      const geometry = await page.evaluate(terminalGrid, { marker, minRows, diagnostic: true });
      throw new Error(`${error.message}: ${JSON.stringify(geometry)}`, { cause: error });
    });
  };
  await visible(page.locator(terminalSelector).locator('.xterm'));
  if (fixture) await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected && window.termState.ws?.readyState === WebSocket.OPEN);
  await wait();
  if (fixture) {
    const { url, rows } = await page.evaluate(() => ({ url: window.termState.ws.url, rows: window.termState.term.rows }));
    const index = fixture.sockets.findLastIndex(socket => socket.href === url);
    assert.ok(index >= 0, 'The connected socket must be intercepted');
    const marker = `last-row-${++markerSequence}`;
    fixture.socketRoutes[index].send(Buffer.from(`\x1b[2J\x1b[${rows};1H${marker}`));
    await wait(marker);
  }
  assert.equal(await page.locator(terminalSelector).locator('.xterm').count(), 1);
  assert.equal(await page.locator('.xterm:visible').count(), 1);
}

async function terminalRows(page, height, top = 0, grid = true) {
  await page.waitForFunction(({ height, top }) => {
    const host = document.getElementById('terminal-container-mobile');
    const term = window.termState.term;
    if (!host || !term?.element) return false;
    const overlay = host.closest('.mobile-detail').getBoundingClientRect();
    if (Math.abs(overlay.height - height) > 1 || Math.abs(overlay.top - top) > 1) return false;
    const box = host.getBoundingClientRect();
    const controls = [...document.querySelectorAll('.mobile-detail-header button, .mobile-terminal-pane button, .mobile-detail-actions button')]
      .map(el => el.getBoundingClientRect()).filter(rect => rect.width && rect.height);
    return box.bottom <= height + top + 1 &&
      controls.every(rect => rect.width >= 44 && rect.height >= 44 && rect.top >= top && rect.bottom <= height + top + 1);
  }, { height, top }).catch(async error => {
    const geometry = await page.evaluate(() => {
      const host = document.getElementById('terminal-container-mobile');
      const term = window.termState.term;
      return { host: host?.getBoundingClientRect().toJSON(), rows: term?.rows,
        cell: term?._core._renderService.dimensions.css.cell.height,
        terminal: term?.element?.getBoundingClientRect().toJSON(),
        overlay: host?.closest('.mobile-detail').getBoundingClientRect().toJSON(),
        controls: [...document.querySelectorAll('.mobile-detail-header button, .mobile-terminal-pane button, .mobile-detail-actions button')]
          .map(el => ({ text: el.textContent, box: el.getBoundingClientRect().toJSON() })).filter(el => el.box.width && el.box.height) };
    });
    throw new Error(`${error.message}: ${JSON.stringify(geometry)}`, { cause: error });
  });
  if (grid) await terminalGridFits(page, undefined, 8);
}

function observationOptions(options = {}) {
  return {
    companionSnapshot: { enabled: false, name: 'Kotik', health: 'ok', ai_available: true, revision: 1,
      items: [{ id: 'attention-1', ticket_id: 'kon-draft', title: 'Saved attention', condition: 'needs_review',
        active: true, read: false, revision: 1, first_seen: stamp }] },
    assistantThreads: [
      { id: companionThreadID, title: companionTitle, purpose: 'companion', autonomy: 'read', running: false,
        created_at: stamp, updated_at: stamp, turns: 1, writes: 0,
        messages: [{ n: 1, text: companionQuestion, at: stamp }],
        evidence: [{ id: 'capture-1', captured_at: stamp, revision: 1, sources: { tickets: 'saved-revision' } }] },
      { id: ordinaryThreadID, title: ordinaryTitle, autonomy: 'ask', running: false,
        created_at: stamp, updated_at: stamp, turns: 1, writes: 0,
        messages: [{ n: 1, text: 'What is on the board?', at: stamp }] },
    ],
    ...options,
  };
}

const assistantSurface = page => page.locator('#assistant-pane:visible, #assistant-sheet:visible');
async function openAssistantControl(page) {
  if (!await assistantSurface(page).isVisible()) {
    await page.getByRole('button', { name: 'Assistant', exact: true }).filter({ visible: true }).click();
  }
  await visible(assistantSurface(page).getByRole('navigation', { name: 'Assistant views', exact: true }));
}
async function observationHidden(page) {
  for (const selector of ['.companion-header', '.companion-cat', '.companion-status', '.companion-count',
    '.companion-rail-cat', '.companion-inbox', '.companion-evidence', '.companion-chat-hint']) {
    await expect(page.locator(`${selector}:visible`), `${selector} must be hidden without observation`).toHaveCount(0);
  }
  await expect(page.getByRole('button', { name: 'Inbox', exact: true }).filter({ visible: true })).toHaveCount(0);
  await expect(page.getByText(companionQuestion, { exact: true }).filter({ visible: true })).toHaveCount(0);
  await expect(page.getByText(`Recorded reply for ${companionTitle}.`, { exact: true }).filter({ visible: true })).toHaveCount(0);
  await expect(page.getByText('An explanation is still running.', { exact: true }).filter({ visible: true })).toHaveCount(0);
}
async function refreshObservation(page, fixture, enabled) {
  fixture.behavior.companionSnapshot = { ...fixture.behavior.companionSnapshot, enabled,
    revision: fixture.behavior.companionSnapshot.revision + 1 };
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.waitForFunction(revision => Alpine.$data(document.body).companionSnapshot?.revision === revision,
    fixture.behavior.companionSnapshot.revision);
}
async function ordinaryChatNavigation(page) {
  const surface = assistantSurface(page);
  const tabs = surface.getByRole('navigation', { name: 'Assistant views', exact: true });
  await tabs.getByRole('button', { name: 'Chat', exact: true }).click();
  await visible(surface.getByRole('textbox', { name: 'Message the assistant', exact: true }));
  assert.equal(await tabs.getByRole('button', { name: 'Chat', exact: true }).getAttribute('aria-pressed'), 'true');
  await tabs.getByRole('button', { name: 'History', exact: true }).click();
  assert.equal(await tabs.getByRole('button', { name: 'History', exact: true }).getAttribute('aria-pressed'), 'true');
  await visible(surface.getByRole('button', { name: new RegExp('^' + ordinaryTitle) }));
  assert.equal(await surface.getByRole('button', { name: new RegExp('^' + companionTitle) }).count(), 0);
  await surface.getByRole('button', { name: new RegExp('^' + ordinaryTitle) }).click();
  await visible(surface.getByText('What is on the board?', { exact: true }));
  await visible(surface.getByText(`Recorded reply for ${ordinaryTitle}.`, { exact: true }));
  await tabs.getByRole('button', { name: 'History', exact: true }).click();
  await tabs.getByRole('button', { name: 'Chat', exact: true }).click();
  await visible(surface.getByText('What is on the board?', { exact: true }));
}
async function observationSettings(page, width) {
  if (width < 768) {
    await page.getByRole('dialog', { name: 'Assistant', exact: true }).getByRole('button', { name: 'Close', exact: true }).click();
    await page.getByRole('button', { name: 'Open navigation', exact: true }).click();
    await page.getByRole('navigation', { name: 'Main navigation', exact: true }).getByRole('button', { name: 'Settings', exact: true }).click();
  } else {
    await page.getByRole('button', { name: 'Close the assistant', exact: true }).click();
    await page.getByRole('button', { name: 'Open command palette', exact: true }).filter({ visible: true }).click();
    await page.locator('#palette-row-nav-settings').click();
  }
  await ready(page);
  assert.equal(new URL(page.url()).hash, '#/settings');
  if (width < 768) await page.locator('#settings-mobile-section').selectOption('assistant');
  else await page.getByRole('navigation', { name: 'Settings sections', exact: true }).getByText('assistant', { exact: true }).click();
  const toggle = page.getByRole('checkbox', { name: 'Enable observation', exact: true });
  await toggle.scrollIntoViewIfNeeded();
  await inViewport(toggle);
  await visible(page.getByRole('region', { name: 'Companion settings', exact: true }));
}

for (const width of [390, 1280]) {
  for (const delayed of [false, true]) {
    test(`Kotik observation disabled at startup${delayed ? ' after an unknown snapshot' : ''} / ${width}px`, async t => {
      let release;
      const options = observationOptions({ savedThread: companionThreadID, assistantOpen: true,
        companionWait: delayed ? new Promise(resolve => { release = resolve; }) : null });
      await withPage(t, width, 'dark', async (page, fixture) => {
        try {
          await openAssistantControl(page);
          await page.waitForFunction(() => !!Alpine.$data(document.body).assistantAgent);
          const surface = assistantSurface(page);
          const tabs = surface.getByRole('navigation', { name: 'Assistant views', exact: true });
          await tabs.getByRole('button', { name: 'History', exact: true }).click();
          await visible(surface.getByRole('button', { name: new RegExp('^' + ordinaryTitle) }));
          assert.equal(await surface.getByRole('button', { name: new RegExp('^' + companionTitle) }).count(), 0);
          await observationHidden(page);
          if (delayed) {
            assert.equal(await page.evaluate(() => Alpine.$data(document.body).companionSnapshot), null);
            assert.ok(fixture.requests.includes(`GET /api/assistant/threads/${companionThreadID}`));
            await tabs.getByRole('button', { name: 'Chat', exact: true }).click();
            await visible(surface.getByRole('textbox', { name: 'Message the assistant', exact: true }));
            await tabs.getByRole('button', { name: 'History', exact: true }).click();
            await observationHidden(page);
            release();
          }
          await page.waitForFunction(() => Alpine.$data(document.body).companionSnapshot?.enabled === false &&
            !Alpine.$data(document.body).companionFetchPending);
          await observationHidden(page);
          assert.ok(!fixture.requests.includes(`GET /api/assistant/threads/${companionThreadID}/activity`),
            'Disabled or unknown observation must not restore the saved companion transcript');
          await ordinaryChatNavigation(page);
          await observationHidden(page);
          await observationSettings(page, width);
          await refreshObservation(page, fixture, true);
          await openAssistantControl(page);
          await assistantSurface(page).getByRole('navigation', { name: 'Assistant views', exact: true })
            .getByRole('button', { name: 'History', exact: true }).click();
          await assistantSurface(page).getByRole('button', { name: new RegExp('^' + companionTitle) }).click();
          await visible(assistantSurface(page).getByText(companionQuestion, { exact: true }));
          await assistantSurface(page).locator('.companion-evidence > summary').click();
          await visible(assistantSurface(page).getByText('Source revisions: {"tickets":"saved-revision"}', { exact: true }));
          assert.deepEqual([...fixture.assistantThreads.values()], options.assistantThreads, 'Re-enabling must preserve saved chats and evidence');
          assert.deepEqual(fixture.mutations, []);
        } finally { release?.(); }
      }, options);
    });
  }

  test(`Kotik observation live disable hides a running chat but retains header Stop / ${width}px`, async t => {
    const options = observationOptions();
    options.companionSnapshot.enabled = true;
    options.assistantThreads[0].running = true;
    await withPage(t, width, 'dark', async (page, fixture) => {
      await openAssistantControl(page);
      const surface = assistantSurface(page);
      await surface.getByRole('navigation', { name: 'Assistant views', exact: true }).getByRole('button', { name: 'History', exact: true }).click();
      await surface.getByRole('button', { name: new RegExp('^' + companionTitle) }).click();
      await visible(surface.getByText(companionQuestion, { exact: true }));
      await visible(surface.getByText('An explanation is still running.', { exact: true }));
      await surface.locator('.companion-evidence > summary').click();
      await visible(surface.getByText('Source revisions: {"tickets":"saved-revision"}', { exact: true }));
      await visible(surface.locator('.companion-header .companion-cat'));
      assert.equal(await surface.locator('.companion-status').innerText(), 'Kotik\nthinking');
      assert.equal(await surface.locator('.companion-count').textContent(), '1 active / 1 unread');
      if (width === 1280) {
        await page.getByRole('button', { name: 'Collapse the assistant to a rail', exact: true }).click();
        await visible(page.locator('#assistant-rail .companion-rail-cat'));
      }
      await refreshObservation(page, fixture, false);
      await observationHidden(page);
      if (width === 1280) {
        await visible(page.locator('#assistant-rail'));
        await page.getByRole('button', { name: 'Expand the assistant', exact: true }).click();
      }
      await observationHidden(page);
      assert.equal(await surface.getByRole('textbox', { name: 'Message the assistant', exact: true }).count(), 0);
      const stop = surface.getByRole('navigation', { name: 'Assistant views', exact: true }).getByRole('button', { name: 'Stop', exact: true });
      await inViewport(stop);
      fixture.behavior.assistantStopFailure = 'The running explanation could not be stopped.';
      await stop.click();
      await visible(surface.getByText(fixture.behavior.assistantStopFailure, { exact: true }));
      assert.equal(await page.evaluate(() => Alpine.$data(document.body).assistantStreaming), true);
      assert.equal(fixture.assistantThreads.get(companionThreadID).running, true);
      await observationHidden(page);
      fixture.behavior.assistantStopFailure = null;
      await stop.click();
      await page.waitForFunction(() => !Alpine.$data(document.body).assistantStreaming);
      await stop.waitFor({ state: 'hidden' });
      assert.equal(await surface.getByText('The running explanation could not be stopped.', { exact: true }).count(), 0);
      await ordinaryChatNavigation(page);
      await observationHidden(page);
      assert.deepEqual(fixture.mutations, [{ method: 'POST', path: `/api/assistant/threads/${companionThreadID}/stop` }]);
      assert.deepEqual(fixture.requests.filter(request => request.startsWith('POST /api/assistant/')), [
        `POST /api/assistant/threads/${companionThreadID}/stop`,
        `POST /api/assistant/threads/${companionThreadID}/stop`,
      ], 'Disabling observation must not start another companion turn');
      await refreshObservation(page, fixture, true);
      await surface.getByRole('navigation', { name: 'Assistant views', exact: true }).getByRole('button', { name: 'History', exact: true }).click();
      await visible(surface.getByRole('button', { name: new RegExp('^' + companionTitle) }));
      await surface.getByRole('button', { name: new RegExp('^' + companionTitle) }).click();
      await visible(surface.getByText(companionQuestion, { exact: true }));
      assert.equal(fixture.assistantThreads.get(companionThreadID).running, false);
    }, options);
  });

  for (const state of ['missing agent', 'degraded', 'disconnected']) {
    test(`Kotik observation enabled with ${state} retains controls / ${width}px`, async t => {
      const options = observationOptions();
      Object.assign(options.companionSnapshot, { enabled: true, ai_available: state !== 'missing agent',
        health: state === 'degraded' ? 'degraded' : 'ok', hint: 'Configure an assistant agent for explanations.' });
      if (state === 'missing agent') options.assistantConfig = { enabled: false, hint: 'Configure an assistant agent.', autonomy: 'ask' };
      await withPage(t, width, 'dark', async (page, fixture) => {
        await page.waitForFunction(() => Alpine.$data(document.body).companionSnapshot?.enabled === true);
        await openAssistantControl(page);
        const surface = assistantSurface(page);
        const tabs = surface.getByRole('navigation', { name: 'Assistant views', exact: true });
        await tabs.getByRole('button', { name: 'Inbox', exact: true }).click();
        if (state === 'disconnected') {
          fixture.behavior.companionFailure = 'Fixture companion offline';
          await page.evaluate(() => window.dispatchEvent(new Event('focus')));
          await page.waitForFunction(() => Alpine.$data(document.body).companionDisconnected);
        }
        await visible(surface.locator('.companion-header .companion-cat'));
        assert.equal(await surface.locator('.companion-status').innerText(), `Kotik\n${state === 'missing agent' ? 'needs you' : state}`);
        assert.equal(await surface.locator('.companion-count').textContent(), '1 active / 1 unread');
        for (const name of ['Inbox', 'Chat', 'History']) await inViewport(tabs.getByRole('button', { name, exact: true }));
        await visible(surface.getByRole('region', { name: 'Shared companion inbox', exact: true }));
        for (const name of ['Refresh', 'Set changes checkpoint', 'Brief me', 'Mark read', 'Investigate']) {
          await visible(surface.getByRole('button', { name, exact: true }));
        }
        assert.equal(await surface.getByRole('button', { name: 'Set changes checkpoint', exact: true }).isDisabled(), state !== 'missing agent');
        assert.equal(await surface.getByRole('button', { name: 'Brief me', exact: true }).isDisabled(), state !== 'degraded');
        await tabs.getByRole('button', { name: 'History', exact: true }).click();
        await visible(surface.getByRole('button', { name: new RegExp('^' + companionTitle) }));
        await visible(surface.getByRole('button', { name: new RegExp('^' + ordinaryTitle) }));
        await tabs.getByRole('button', { name: 'Chat', exact: true }).click();
        if (state === 'missing agent') await visible(surface.getByText('Configure an assistant agent.', { exact: true }));
        else await visible(surface.getByRole('textbox', { name: 'Message the assistant', exact: true }));
        await tabs.getByRole('button', { name: 'Inbox', exact: true }).click();
        if (width === 1280) {
          await page.getByRole('button', { name: 'Collapse the assistant to a rail', exact: true }).click();
          const cat = page.locator('#assistant-rail .companion-rail-cat');
          await inViewport(cat);
          assert.equal(await cat.getAttribute('aria-label'),
            `Kotik: ${state === 'missing agent' ? 'needs you' : state}, 1 active / 1 unread`);
          await cat.click();
          await visible(surface.getByRole('region', { name: 'Shared companion inbox', exact: true }));
        }
        fixture.behavior.companionFailure = null;
        await refreshObservation(page, fixture, false);
        await observationHidden(page);
        await tabs.getByRole('button', { name: 'Chat', exact: true }).click();
        if (state === 'missing agent') await visible(surface.getByText('Configure an assistant agent.', { exact: true }));
        else await visible(surface.getByRole('textbox', { name: 'Message the assistant', exact: true }));
        assert.deepEqual(fixture.mutations, []);
      }, options);
    });
  }
}

describe('embedded responsive browser UI', { concurrency: 4 }, () => {
  for (const width of [320, 390]) for (const theme of themes) {
    test(`board chrome and status navigation ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await t.test('running count shares the app row without shrinking actions', async () => {
          const bar = page.locator('.mobile-app-bar');
          const mark = await bar.locator('.mobile-wordmark').boundingBox();
          const pill = await bar.locator('.mobile-running-pill').boundingBox();
          assert.ok(pill.y >= mark.y && pill.y + pill.height <= mark.y + mark.height + 1, 'Running count stays within the wordmark block');
          const running = [...fixture.records.values()].filter(ticket => ticket.source === 'primary' && ticket.status === 'in_progress').length;
          assert.equal(await bar.locator('.mobile-running-pill').getAttribute('aria-label'), `${running} running`);
          for (const name of ['Open navigation', 'Assistant', 'Toggle theme', 'New ticket']) {
            const control = bar.getByRole('button', { name, exact: true });
            await inViewport(control);
            const box = await control.boundingBox();
            assert.ok(box.width >= 44 && box.height >= 44);
            assert.ok(mark.y >= box.y - 1 && mark.y + mark.height <= box.y + box.height + 1, 'Wordmark and count fit in the action row');
          }
          await inViewport(page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true }));
          await noPageOverflow(page);
        });
        await t.test('offscreen activation and swipes reveal the pressed status only', async () => {
          const strip = page.getByRole('navigation', { name: 'Ticket status', exact: true });
          const buttons = strip.getByRole('button');
          assert.equal(await strip.evaluate(el => getComputedStyle(el).backgroundAttachment), 'local, local, scroll, scroll');
          const labels = await page.evaluate(() => Alpine.$data(document.body).columns.map(col => col.label));
          assert.deepEqual(await buttons.locator('span:nth-child(2)').allTextContents(), labels);
          const last = buttons.last();
          const before = await last.boundingBox();
          assert.ok(before.x + before.width > width, 'The last status starts offscreen');
          const y = await page.evaluate(() => scrollY);
          await last.evaluate(el => el.focus({ preventScroll: true }));
          await page.keyboard.press('Enter');
          const selected = async index => {
            await page.waitForFunction(index => {
              const strip = document.querySelector('.mobile-status-tabs');
              const buttons = strip.querySelectorAll('button');
              const button = buttons[index];
              const box = button.getBoundingClientRect(), view = strip.getBoundingClientRect();
              const style = getComputedStyle(strip);
              const left = view.left + strip.clientLeft + parseFloat(style.paddingLeft);
              const right = view.left + strip.clientLeft + strip.clientWidth - parseFloat(style.paddingRight);
              return button.getAttribute('aria-pressed') === 'true' &&
                [...buttons].filter(el => el.getAttribute('aria-pressed') === 'true').length === 1 &&
                box.left >= left - 1 && box.right <= right + 1;
            }, index);
            assert.equal(await page.evaluate(() => scrollY), y, 'Status scrolling must not move the page');
            const count = Number(await buttons.nth(index).locator('span').last().textContent());
            assert.equal(await page.locator('.mobile-board-layer [x-text="ticket.title"]').count(), count, 'Count matches displayed cards');
          };
          await selected(labels.length - 1);
          assert.equal(await last.evaluate(el => document.activeElement === el), true, 'Activation keeps keyboard focus');
          const swipe = dx => swipeBoard(page, dx);
          for (let index = labels.length - 2; index >= 0; --index) {
            await swipe(100);
            await selected(index);
            assert.equal(await last.evaluate(el => document.activeElement === el), true, 'Swipes must not change keyboard focus');
          }
          for (let index = 1; index < labels.length; ++index) {
            await swipe(-100);
            await selected(index);
          }
          await swipe(-100);
          await selected(labels.length - 1);
          await buttons.first().click();
          await selected(0);
          await swipe(100);
          await selected(0);
          await noPageOverflow(page);
        });
        for (const scenario of [
          { name: 'oversized status', status: 'awaiting_external_dependency_approval_review', oversized: true },
          { name: 'safe-area status', status: 'awaiting_user_review', padding: '44px' },
          { name: 'oversized safe-area status', status: 'awaiting_external_dependency_approval_review', padding: '44px', oversized: true },
        ]) {
          await t.test(`${scenario.name} stays aligned after boundary activation and swipes`, async () => {
            const key = `${source}/kon-geometry`;
            fixture.records.set(key, ticket('kon-geometry', 'Status geometry fixture', { source, status: scenario.status, kontora: false }));
            await page.reload({ waitUntil: 'domcontentloaded' });
            await ready(page);
            const strip = page.getByRole('navigation', { name: 'Ticket status', exact: true });
            if (scenario.padding) await strip.evaluate((el, padding) => {
              el.style.paddingLeft = padding;
              el.style.paddingRight = padding;
            }, scenario.padding);
            const last = strip.getByRole('button').last();
            await last.evaluate(el => el.focus({ preventScroll: true }));
            const y = await page.evaluate(() => scrollY);
            await page.keyboard.press('Enter');
            await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button:last-child')?.getAttribute('aria-pressed') === 'true');
            await page.evaluate(() => Alpine.nextTick());
            const geometry = () => last.evaluate(el => {
              const strip = el.parentElement, view = strip.getBoundingClientRect(), box = el.getBoundingClientRect();
              const style = getComputedStyle(strip);
              return { left: box.left, right: box.right, width: box.width, scroll: strip.scrollLeft,
                safeLeft: view.left + strip.clientLeft + parseFloat(style.paddingLeft),
                safeRight: view.left + strip.clientLeft + strip.clientWidth - parseFloat(style.paddingRight) };
            });
            const initial = await geometry();
            assert.equal(initial.width > initial.safeRight - initial.safeLeft, !!scenario.oversized, 'Fixture exercises the intended width');
            if (scenario.oversized) assert.ok(Math.abs(initial.left - initial.safeLeft) <= 1, JSON.stringify(initial));
            else assert.ok(initial.left >= initial.safeLeft - 1 && initial.right <= initial.safeRight + 1, JSON.stringify(initial));
            for (const action of ['swipe', 'activate', 'swipe', 'activate']) {
              if (action === 'swipe') await swipeBoard(page, -100);
              else await page.keyboard.press('Enter');
              await page.evaluate(() => Alpine.nextTick());
              const repeated = await geometry();
              assert.ok(Math.abs(repeated.scroll - initial.scroll) <= 1, `Boundary ${action} moved the strip: ${JSON.stringify({ initial, repeated })}`);
              assert.equal(await last.getAttribute('aria-pressed'), 'true');
              assert.equal(await last.evaluate(el => document.activeElement === el), true);
              assert.equal(await page.evaluate(() => scrollY), y);
            }
            await noPageOverflow(page);
            fixture.records.delete(key);
          });
        }
        await t.test('overflowing ticket lists keep each entry at its content height', async () => {
          const keys = Array.from({ length: 24 }, (_, i) => {
            const id = `kon-list-${i}`;
            fixture.records.set(`primary/${id}`, ticket(id, `List fixture ${i} ` + 'with a long title that wraps '.repeat(4).trim(),
              { parent: { id: 'kon-epic' } }));
            return `primary/${id}`;
          });
          const clipped = (list, entries) => page.evaluate(({ list, entries }) => {
            const host = document.querySelector(list);
            const items = [...host.querySelectorAll(entries)];
            return { overflows: host.scrollHeight > host.clientHeight, count: items.length,
              shortest: Math.min(...items.map(el => el.scrollHeight)),
              clipped: items.filter(el => el.scrollHeight > el.clientHeight + 1).map(el => [el.clientHeight, el.scrollHeight]) };
          }, { list, entries });
          try {
            await page.reload({ waitUntil: 'domcontentloaded' });
            await ready(page);
            // Only entries taller than the 44px touch minimum, in a list that
            // overflows, can lose height.
            const board = await clipped('.mobile-board-layer > .overflow-y-auto', 'button[data-pipe-color]');
            assert.ok(board.overflows && board.count > keys.length && board.shortest > 44, JSON.stringify(board));
            assert.deepEqual(board.clipped, []);
            await routeTo(page, '#/e/kon-epic');
            const children = await clipped('.mobile-epic > .overflow-y-auto', 'button[x-text]');
            assert.ok(children.overflows && children.count === keys.length && children.shortest > 44, JSON.stringify(children));
            assert.deepEqual(children.clipped, []);
          } finally {
            for (const key of keys) fixture.records.delete(key);
            await routeTo(page, '#/');
          }
        });
        await t.test('combined folders retain counts, search and source errors', async () => {
          fixture.behavior.unavailableFolder = true;
          await page.reload({ waitUntil: 'domcontentloaded' });
          await ready(page);
          assert.equal(await page.getByRole('combobox', { name: 'Filter by ticket folder', exact: true }).count(), 0);
          const error = page.locator('.mobile-chrome [role="alert"]');
          await visible(error);
          assert.ok((await error.textContent()).includes(unavailablePath + ': Fixture folder unavailable'));
          const open = page.getByRole('navigation', { name: 'Ticket status', exact: true }).getByRole('button').first();
          const cards = page.locator('.mobile-board-layer [x-text="ticket.title"]');
          assert.equal(Number(await open.locator('span').last().textContent()), 3);
          assert.equal(await cards.count(), 3);
          const titles = await cards.allTextContents();
          for (const title of ['Primary draft', 'Secondary namesake']) assert.ok(titles.includes(title));
          const search = page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true });
          for (const title of ['Primary draft', 'Secondary namesake']) {
            await search.fill(title);
            await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button span:last-child').textContent === '1');
            await page.waitForFunction(title => {
              const cards = document.querySelectorAll('.mobile-board-layer [x-text="ticket.title"]');
              return cards.length === 1 && cards[0].textContent === title;
            }, title);
            assert.deepEqual(await cards.allTextContents(), [title]);
            await visible(error);
            await noPageOverflow(page);
          }
          await search.fill('');
          await page.waitForFunction(() => document.querySelector('.mobile-status-tabs button span:last-child').textContent === '3');
          fixture.behavior.sourcesFailure = true;
          await page.reload({ waitUntil: 'domcontentloaded' });
          await ready(page);
          await visible(error);
          assert.ok((await error.textContent()).includes('Failed to load ticket sources'));
          await noPageOverflow(page);
        });
      });
    });
  }
  for (const theme of themes) {
    test(`phone compose paste and cancellation / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        fixture.behavior.bracketedPaste = true;
        fixture.socketRoutes.at(-1).send(Buffer.from('\x1b[?2004h'));
        await page.waitForFunction(() => termState.term.modes.bracketedPasteMode);
        await page.clock.install();
        for (const cancel of [false, true]) {
          await page.locator('.mobile-detail .mobile-terminal-keys').getByRole('button', { name: 'Compose', exact: true }).click();
          const dialog = page.getByRole('dialog', { name: 'Compose message', exact: true });
          await visible(dialog);
          await dialog.getByRole('textbox', { name: 'Message', exact: true }).fill('первая\nвторая');
          const before = fixture.messages.filter(message => message.type === 'input').length;
          await dialog.getByRole('button', { name: 'Send', exact: true }).click();
          await dialog.waitFor({ state: 'hidden' });
          await expect.poll(() => fixture.messages.filter(message => message.type === 'input')[before]?.data)
            .toBe('\x1b[200~первая\rвторая\x1b[201~');
          if (cancel) await page.evaluate(() => Alpine.$data(document.body).toggleTerminalRW());
          await page.clock.runFor(150);
          if (!cancel) await expect.poll(() => fixture.messages.filter(message => message.type === 'input').length).toBe(before + 2);
          assert.deepEqual(fixture.messages.filter(message => message.type === 'input').slice(before).map(message => message.data),
            cancel ? ['\x1b[200~первая\rвторая\x1b[201~'] : ['\x1b[200~первая\rвторая\x1b[201~', '\r']);
        }
      });
    });
    test(`phone activity polling, completion and tab reachability / ${theme}`, async t => {
      await withPage(t, 320, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        const detail = page.locator('.mobile-detail');
        const tabs = detail.locator('.mobile-detail-tabs');
        const geometry = await tabs.evaluate(el => ({ scroll: el.scrollWidth, client: el.clientWidth, wrap: getComputedStyle(el).flexWrap }));
        assert.ok(geometry.scroll > geometry.client, JSON.stringify(geometry));
        assert.equal(geometry.wrap, 'nowrap');
        await detail.getByRole('button', { name: '▤ ticket', exact: true }).click();
        assert.ok(await tabs.evaluate(el => el.scrollLeft > 0));
        await page.setViewportSize({ width: 390, height: 900 });
        const activeTab = await page.evaluate(() => Alpine.$data(document.body).activeTab);
        await detail.getByRole('button', { name: 'activity', exact: true }).click();
        await visible(detail.locator('#activity-scroll').getByText('Fixture activity revision 0', { exact: true }));
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).activeTab), activeTab);
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).terminalOpen), false);
        assert.equal(await page.locator('#activity-scroll').count(), 1);
        await page.waitForRequest(request => new URL(request.url()).pathname === '/api/tickets/kon-live/activity', { timeout: 6000 });
        const follow = detail.getByRole('button', { name: /follow/i });
        await visible(follow);
        const before = await follow.getAttribute('aria-pressed');
        await follow.click();
        assert.notEqual(await follow.getAttribute('aria-pressed'), before);
        await follow.click();
        const record = fixture.records.get('primary/kon-live');
        record.status = 'human_review';
        fixture.behavior.outputRevision = 1;
        await page.evaluate(record => Alpine.$data(document.body).applyTicketUpdate(record), record);
        await visible(detail.locator('#activity-scroll').getByText('Fixture activity revision 1', { exact: true }));
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).detailTab), 'activity');
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).activity.live), false);
        await noPageOverflow(page);
      });
    });
    test(`phone activity resumes a new stage and exports its Raw log / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        const detail = page.locator('.mobile-detail');
        await detail.getByRole('button', { name: 'activity', exact: true }).click();
        await visible(detail.locator('#activity-scroll').getByText('Fixture activity revision 0', { exact: true }));
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        await detail.getByRole('button', { name: /^plan:/ }).click();
        await visible(detail.getByText('kon-live plan fixture log revision 0', { exact: true }));
        const record = fixture.records.get('primary/kon-live');
        record.history.push({ stage: 'code', run: 0, started_at: stamp, completed_at: stamp, exit_code: 0 });
        record.stage = 'test';
        record.started_at = '2026-10-01T10:01:00Z';
        fixture.behavior.outputRevision = 1;
        await page.evaluate(record => Alpine.$data(document.body).applyTicketUpdate(record), record);
        await detail.getByRole('button', { name: 'activity', exact: true }).click();
        await visible(detail.locator('#activity-scroll').getByText('Fixture activity revision 1', { exact: true }));
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).activityStage), 'test');
        const popupPromise = page.waitForEvent('popup');
        await detail.getByRole('button', { name: '⧉ raw', exact: true }).click();
        const popup = await popupPromise;
        await popup.waitForLoadState('domcontentloaded');
        assert.equal(await popup.locator('body').innerText(), 'kon-live test fixture log revision 1\n');
        await popup.close();
      });
    });
    for (const width of [320, 390]) {
      test(`phone cached activity follows on reveal ${width}px / ${theme}`, async t => {
        await withPage(t, width, theme, async (page, fixture) => {
          fixture.behavior.activityEvents = Array.from({ length: 80 }, (_, i) => ({ kind: 'text', text: `Cached activity event ${i}` }));
          await openTicket(page, 'kon-done');
          await page.waitForFunction(() => Alpine.$data(document.body).activity?.tape?.events.length === 80);
          await page.evaluate(() => Alpine.nextTick());
          await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).detailTab), 'ticket');
          const reads = fixture.requests.filter(request => request === 'GET /api/tickets/kon-done/activity').length;
          const detail = page.locator('.mobile-detail');
          await detail.getByRole('button', { name: 'activity', exact: true }).click();
          const scroller = detail.locator('#activity-scroll');
          await visible(scroller.getByText('Cached activity event 79', { exact: true }));
          await page.evaluate(() => Alpine.nextTick());
          await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
          const geometry = await scroller.evaluate(el => ({ height: el.scrollHeight, client: el.clientHeight, top: el.scrollTop }));
          assert.ok(geometry.height > geometry.client, 'Cached transcript overflows');
          assert.ok(geometry.height - geometry.client - geometry.top <= 1, `Follow opens at the newest event: ${JSON.stringify(geometry)}`);
          assert.equal(fixture.requests.filter(request => request === 'GET /api/tickets/kon-done/activity').length, reads, 'Reveal reuses the completed transcript');
          await detail.getByRole('button', { name: /follow/i }).click();
          await scroller.evaluate(el => { el.scrollTop = 200; });
          const top = await scroller.evaluate(el => el.scrollTop);
          await detail.getByRole('button', { name: '▤ ticket', exact: true }).click();
          await detail.getByRole('button', { name: 'activity', exact: true }).click();
          await visible(scroller);
          await page.evaluate(() => Alpine.nextTick());
          assert.equal(await scroller.evaluate(el => el.scrollTop), top, 'Follow off preserves the reading position');
          await detail.getByRole('button', { name: /follow/i }).click();
          await expect.poll(() => scroller.evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThanOrEqual(1);
        });
      });
    }
    test(`activity breakpoint roundtrip / ${theme}`, async t => {
      await withPage(t, 1280, theme, async page => {
        await openTicket(page, 'kon-live');
        await page.getByRole('button', { name: 'activity', exact: true }).filter({ visible: true }).click();
        await visible(page.locator('#activity-scroll').getByText('Fixture activity revision 0', { exact: true }));
        await page.setViewportSize({ width: 390, height: 900 });
        await page.waitForFunction(() => Alpine.$data(document.body).isMobile && Alpine.$data(document.body).detailTab === 'activity');
        await visible(page.locator('.mobile-detail #activity-scroll'));
        assert.equal(await page.locator('#activity-scroll').count(), 1);
        await page.setViewportSize({ width: 1280, height: 900 });
        await page.waitForFunction(() => !Alpine.$data(document.body).isMobile && Alpine.$data(document.body).activeTab === 'activity');
        await visible(page.locator('#activity-scroll').getByText('Fixture activity revision 0', { exact: true }));
        assert.equal(await page.locator('#activity-scroll').count(), 1);
      });
    });
  }
  test('pane snapshots batch writes and preserve received output across socket close', async t => {
    await withPage(t, 1280, 'dark', async (page, fixture) => {
      await openTerminalSubject(page, 'kon-live');
      await terminalGridFits(page, fixture);
      await page.evaluate(() => {
        const term = window.termState.term;
        const stats = window.__paneStats = { received: 0, submitted: 0, parsed: 0, pending: 0, peakPending: 0 };
        const write = term.write.bind(term);
        term.write = (data, callback) => {
          stats.submitted++;
          stats.peakPending = Math.max(stats.peakPending, ++stats.pending);
          write(data, () => { stats.parsed++; stats.pending--; callback?.(); });
        };
        window.termState.ws.addEventListener('message', event => {
          if (typeof event.data !== 'string') stats.received++;
        });
      });
      const ws = fixture.socketRoutes.at(-1);
      const { cols, rows } = await page.evaluate(() => ({ cols: window.termState.term.cols, rows: window.termState.term.rows }));
      ws.send(JSON.stringify({ type: 'reset', cols: 201, rows: 50 }));
      const snapshot = Buffer.from(Array.from({ length: 5000 }, (_, i) =>
        `\x1b[0m${String(i).padStart(4, '0')} ${'x'.repeat(160)}\x1b[0m`).join('\r\n'));
      let sent = 0;
      for (let offset = 0; offset < snapshot.length; offset += 32768) {
        ws.send(snapshot.subarray(offset, offset + 32768));
        sent++;
      }
      ws.send(JSON.stringify({ type: 'reset', cols, rows }));
      ws.send(Buffer.from('final snapshot\r\nfinal output'));
      sent++;
      ws.close({ code: 1000, reason: 'Fixture complete' });
      await page.waitForFunction(() => {
        const term = window.termState.term;
        return window.termState.ws === null && term.buffer.active.baseY === 0
          && term.buffer.active.getLine(0)?.translateToString(true) === 'final snapshot'
          && term.buffer.active.getLine(1)?.translateToString(true) === 'final output';
      }).catch(async error => {
        t.diagnostic(JSON.stringify(await page.evaluate(() => window.__paneStats)));
        throw error;
      });
      const stats = await page.evaluate(() => window.__paneStats);
      t.diagnostic(JSON.stringify({ snapshotBytes: snapshot.length, sent, ...stats }));
      assert.equal(stats.received, sent);
      assert.equal(stats.submitted, sent);
      assert.equal(stats.parsed, sent);
      assert.ok(stats.peakPending > 1, 'consecutive frames reach xterm before earlier write callbacks finish');
    });
  });

  test('read-only pane history scrolls locally without socket input', async t => {
    await withPage(t, 1280, 'dark', async (page, fixture) => {
      await openTerminalSubject(page, 'kon-live');
      await terminalGridFits(page, fixture);
      const ws = fixture.socketRoutes.at(-1);
      ws.send(Buffer.from(Array.from({ length: 400 }, (_, i) => `history-${i}\r\n`).join('') + 'history-end'));
      await page.waitForFunction(() => {
        const term = window.termState.term;
        return term.buffer.active.baseY > 300
          && term.buffer.active.getLine(term.buffer.active.baseY + term.rows - 1)?.translateToString(true) === 'history-end';
      });
      const before = fixture.messages.length;
      await page.locator('.xterm:visible').hover();
      await page.mouse.wheel(0, -1200);
      await page.waitForFunction(() => {
        const buffer = window.termState.term.buffer.active;
        return buffer.viewportY < buffer.baseY;
      });
      assert.equal(fixture.messages.length, before, 'scrolling is local, with no input or resize message');
      assert.equal(await page.evaluate(() => Alpine.$data(document.body).terminalRW), false);
    });
  });
  for (const width of [390, 1280]) {
    test(`pane resets, reply suppression and focus reports / ${width}px`, async t => {
      await withPage(t, width, 'dark', async (page, fixture) => {
        await openTerminalSubject(page, 'kon-live');
        await terminalGridFits(page, fixture);
        await page.getByRole('button', { name: width < 768 ? 'Enable input' : 'read-only', exact: true })
          .filter({ visible: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalRW && Alpine.$data(document.body).terminalConnected);
        await terminalGridFits(page, fixture);
        const ws = fixture.socketRoutes.at(-1);
        const { cols, rows } = await page.evaluate(() => ({ cols: window.termState.term.cols, rows: window.termState.term.rows }));
        const inputCount = () => fixture.messages.filter(message => message.type === 'input').length;
        const before = inputCount();
        const marker = 'queries-processed';
        ws.send(Buffer.from('\x1b[c\x1b[>c\x1b[5n\x1b[6n\x1b[?6n\x1b[4$p\x1b[?25$p'
          + '\x1bP$qm\x1b\\\x1b]4;0;?;1;?\x07\x1b]10;?\x07\x1b]11;?\x07\x1b]12;?\x07'
          + `\x1b[${rows};1H${marker}`));
        await page.waitForFunction(marker => {
          const term = window.termState.term;
          return term.buffer.active.getLine(term.buffer.active.baseY + term.rows - 1)?.translateToString(true) === marker;
        }, marker);
        assert.equal(inputCount(), before, 'terminal queries must not produce input replies');

        ws.send(Buffer.from('old output\r\n'.repeat(5000)));
        ws.send(JSON.stringify({ type: 'reset', cols, rows }));
        ws.send(Buffer.from('fresh snapshot'));
        await page.waitForFunction(() => {
          const term = window.termState.term;
          return term.buffer.active.baseY === 0 && term.buffer.active.getLine(0)?.translateToString(true) === 'fresh snapshot';
        });
        ws.send(Buffer.from('\x1b[?1004h'));
        await expect.poll(() => fixture.messages.filter(message => message.type === 'input' && /^\x1b\[[IO]$/.test(message.data)).length).toBeGreaterThan(0);
        assert.equal(fixture.messages.filter(message => message.type === 'input').every(message => message.url.searchParams.get('rw') === '1'), true);
      });
    });
  }
  for (const width of [320, 390]) for (const theme of themes) {
    test(`terminal visible geometry ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await openTicket(page, 'kon-live');
        await visible(page.locator('#terminal-container-mobile .xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#terminal-container-mobile').evaluate(el => el.clientWidth), width - 14);
        assert.equal(await page.evaluate(() => termState.term.options.scrollback), 0);
        for (const height of [360, 420, 900]) {
          await visualViewport(page, height, height === 900 ? 0 : 12);
          assert.equal(await page.evaluate(() => innerHeight), 900, 'Only the visual viewport shrinks');
          await terminalRows(page, height, height === 900 ? 0 : 12);
          const status = page.locator('.mobile-terminal-toolbar [role="status"]');
          assert.equal(await page.locator('.mobile-detail-actions').isVisible(), height >= 500);
          await page.evaluate(() => { window.__fixtureHoldOpen = true; });
          await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).click();
          await page.waitForFunction(() => window.__fixturePendingOpens.length > 0);
          assert.equal(await status.textContent(), 'Connecting…');
          assert.equal(await page.locator('.mobile-detail-actions').isVisible(), false);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
          const inputs = fixture.messages.filter(message => message.type === 'input').length;
          const keys = page.locator('.mobile-detail .mobile-terminal-keys');
          for (const button of await keys.getByRole('button').all()) assert.equal(await button.isDisabled(), true);
          await keys.getByRole('button', { name: 'Escape', exact: true }).dispatchEvent('click');
          assert.equal(fixture.messages.filter(message => message.type === 'input').length, inputs);
          await page.locator('#terminal-container-mobile .xterm-screen').click();
          await page.keyboard.type('must-not-send');
          assert.equal(fixture.messages.filter(message => message.type === 'input').length, inputs);
          // A connecting socket cannot request the grid for the smaller input toolbar.
          await terminalRows(page, height, height === 900 ? 0 : 12, false);
          await page.evaluate(() => {
            window.__fixtureHoldOpen = false;
            window.__fixturePendingOpens.splice(0).forEach(open => open());
          });
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), false);
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el === document.activeElement), false,
            'A tap during connection must not open the keyboard when readiness changes');
          await terminalRows(page, height, height === 900 ? 0 : 12);
          for (const [name, data] of [['Escape', '\x1b'], ['Tab', '\t'], ['Shift+Tab', '\x1b[Z'],
            ['Up', '\x1b[A'], ['Down', '\x1b[B'], ['Right', '\x1b[C'], ['Left', '\x1b[D'], ['Ctrl+C', '\x03'], ['Enter', '\r']]) {
            await keys.getByRole('button', { name, exact: true }).click();
            await expect.poll(() => fixture.messages.filter(message => message.type === 'input').at(-1)?.data).toBe(data);
            assert.equal(fixture.messages.filter(message => message.type === 'input').at(-1).url.searchParams.get('rw'), '1');
            assert.equal(await page.evaluate(() => Alpine.$data(document.body).terminalRW), true);
            assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el === document.activeElement), false);
          }
          await page.locator('#terminal-container-mobile .xterm-screen').click();
          await page.keyboard.type('fixture-input');
          await page.waitForFunction(() => document.activeElement?.classList.contains('xterm-helper-textarea'));
          const done = page.getByRole('button', { name: 'Done typing', exact: true }).filter({ visible: true });
          if (height === 420) await page.keyboard.press('Escape');
          else await done.click();
          await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).waitFor();
          assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el === document.activeElement), false);
          assert.equal(new URL(page.url()).hash, '#/t/kon-live');
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          await fixture.socketRoutes.at(-1).close({ code: 1000, reason: 'Fixture disconnect' });
          await visible(page.locator('.mobile-terminal-pane [role="alert"]'));
          assert.equal(await status.textContent(), 'Disconnected');
          // The error banner changes the container, but only a server reset can resize the grid.
          await terminalRows(page, height, height === 900 ? 0 : 12, false);
          await page.getByRole('button', { name: 'retry connection', exact: true }).filter({ visible: true }).click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        }
        assert.ok(fixture.messages.some(message => message.type === 'input' && message.url.searchParams.get('rw') === '1'));
        assert.ok(fixture.messages.some(message => message.type === 'resize' && message.rows >= 8));
        assert.equal(await page.evaluate(() => window.__fixtureSockets.filter(ws => ws.readyState < 2).length), 1);
        for (const height of [360, 420, 900]) {
          await page.setViewportSize({ width, height });
          await visualViewport(page, height);
          await terminalRows(page, height);
        }
        const detail = page.locator('.mobile-detail');
        assert.equal(await detail.locator('.mobile-detail-title').textContent(), liveTitle);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        assert.equal(await detail.getByRole('button', { name: '≡ logs', exact: true }).getAttribute('aria-pressed'), 'true');
        const stages = detail.getByLabel('Stages and cost estimates');
        await visible(stages);
        assert.equal(await stages.getByRole('button').count(), 3, 'Each stage has one control, including unpriced stages');
        assert.match(await stages.textContent(), /1\/2/);
        await stages.getByRole('button', { name: /^test:/ }).click();
        assert.equal(await detail.getByRole('button', { name: '≡ logs', exact: true }).getAttribute('aria-pressed'), 'true');
        await stages.getByRole('button', { name: /^plan:/ }).click();
        await visible(detail.getByText('kon-live plan fixture log revision 0', { exact: true }));
        await stages.getByRole('button', { name: /^code:/ }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).detailTab === 'terminal' && Alpine.$data(document.body).terminalConnected);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        assert.equal(await detail.locator('.mobile-detail-title').textContent(), liveTitle);
        await detail.getByRole('button', { name: '▤ ticket', exact: true }).click();
        assert.equal(await detail.locator('h1[x-text="selectedTicket?.title"]').textContent(), liveTitle);
        await visible(detail.getByText(liveAgent, { exact: true }));
        await visible(detail.getByText(liveBranch, { exact: true }));
        const copy = detail.getByRole('button', { name: 'Copy attach command', exact: true });
        await copy.focus();
        await page.keyboard.press('Enter');
        assert.equal(await page.evaluate(() => window.__fixtureCopied), 'kontora attach kon-live');
        await detail.getByRole('button', { name: '›_ terminal', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
        await page.getByRole('button', { name: 'Enable input', exact: true }).filter({ visible: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await detail.getByRole('button', { name: '≡ logs', exact: true }).click();
        await detail.getByRole('button', { name: '›_ terminal', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#terminal-container-mobile textarea').evaluate(el => el.readOnly), true);
      });
    });
  }
  for (const width of widths) for (const theme of themes) {
    test(`${width}px / ${theme}`, { timeout: 110000 }, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        // The fixture store deliberately survives to verify persistence.
        t.beforeEach(async () => {
          await page.setViewportSize({ width, height: 900 });
          await page.goto(base, { waitUntil: 'domcontentloaded' });
          await ready(page);
        });
        await t.test('header actions, theme and local assets', async () => {
          const toggle = page.getByRole('button', { name: 'Toggle theme', exact: true }).filter({ visible: true });
          for (const name of ['Toggle theme', 'Assistant', 'New ticket']) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          await inViewport(page.getByRole('textbox', { name: 'Filter tickets', exact: true }).filter({ visible: true }));
          assert.equal(await page.locator('html').getAttribute('data-theme'), theme);
          await toggle.click();
          assert.equal(await page.locator('html').getAttribute('data-theme'), theme === 'dark' ? 'light' : 'dark');
          await toggle.click();
          await noPageOverflow(page);
          for (const path of ['/api/config', '/api/tickets', '/api/ticket-sources', `/api/sources/${source}/tickets`, '/api/events']) {
            assert.ok(fixture.requests.includes(`GET ${path}`), `Expected intercepted ${path}`);
          }
        });
        await t.test('long identifiers keep ticket actions reachable', async () => {
          await openTicket(page, longID);
          await noPageOverflow(page);
          for (const name of ['queue', /^(?:✕ )?cancel$/]) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          const more = width < 768 ? page.getByRole('button', { name: 'Actions', exact: true })
            : page.getByRole('banner').getByRole('button', { name: 'More actions', exact: true });
          await inViewport(more);
          await more.click();
          if (width < 768) {
            await visible(page.getByRole('dialog', { name: 'Ticket actions' }));
            await page.keyboard.press('Escape');
          } else {
            await visible(page.getByRole('button', { name: 'delete file', exact: true }));
            await more.click();
          }
          await page.setViewportSize({ width, height: 420 });
          await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '420px');
          for (const name of ['queue', /^(?:✕ )?cancel$/]) {
            await inViewport(page.getByRole('button', { name, exact: true }).filter({ visible: true }));
          }
          await inViewport(more);
          await noPageOverflow(page);
        });
        for (const [route, selector] of [
          ['#/new', '[x-model="createForm.title"]'],
          ['#/stats', '.stats-title'],
          ['#/automations', '.automations-view .stats-title'],
          ['#/archive', '[aria-label="Filter archive"]'],
          ['#/settings', width <= 1100 ? '#settings-mobile-section' : '[aria-label="Search settings"]'],
        ]) {
          await t.test(`${route} renders and reloads`, async () => {
            await routeTo(page, route);
            await visible(page.locator(`${selector}:visible`).first());
            await noPageOverflow(page);
            await page.reload({ waitUntil: 'domcontentloaded' });
            await ready(page);
            await visible(page.locator(`${selector}:visible`).first());
            assert.equal(new URL(page.url()).hash, route);
            await noPageOverflow(page);
            await page.goBack();
            await page.waitForFunction(() => Alpine.$data(document.body).currentView === 'board');
            await ready(page);
            assert.equal(new URL(page.url()).hash, '#/');
            assert.equal(await page.evaluate(() => Alpine.$data(document.body)._statsTimer), null);
            await page.goForward();
            await visible(page.locator(`${selector}:visible`).first());
            await ready(page);
            assert.equal(new URL(page.url()).hash, route);
            await noPageOverflow(page);
          });
        }
        await t.test('Archive detail keeps its route before opening a running Board ticket', async () => {
          await routeTo(page, '#/archive');
          await page.locator('#archive-view .archive-table-scroll').getByTitle('Archived fixture', { exact: true }).click();
          await visible(page.locator('.archive-detail:visible'));
          await page.waitForFunction(() => !Alpine.$data(document.body).archiveDetailLoading);
          assert.equal(new URL(page.url()).hash, '#/archive/t/kon-archived');
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).archiveDetailOpen()), true);
          const header = page.locator('.archive-detail-header');
          for (const name of ['restore to board', 'copy id', 'Close']) {
            const control = header.getByRole('button', { name, exact: true });
            await control.scrollIntoViewIfNeeded();
            await inViewport(control);
          }
          assert.ok(await header.evaluate(el => el.scrollWidth <= el.clientWidth + 1), 'Archive controls must wrap instead of scrolling sideways');
          await noPageOverflow(page);
          await openTicket(page, 'kon-live');
          await visible(page.locator('#terminal-session:visible .xterm, #terminal-container-mobile:visible .xterm'));
          assert.equal(await page.evaluate(() => Alpine.$data(document.body)._archiveDetailId), null);
          assert.equal(new URL(page.url()).hash, '#/t/kon-live');
          assert.equal(await page.locator('.xterm:visible').count(), 1);
        });
        await t.test('creation persists the submitted fields', async () => {
          await backToBoard(page);
          await page.getByRole('button', { name: 'New ticket', exact: true }).filter({ visible: true }).click();
          await model(page, 'createForm.title').fill('Created in browser');
          await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
          await model(page, 'createForm.body').fill('Browser acceptance criteria.');
          const submit = page.getByRole('button', { name: 'create ticket', exact: true });
          await submit.scrollIntoViewIfNeeded();
          await inViewport(submit);
          await Promise.all([
            page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST'),
            submit.click(),
          ]);
          const mutation = fixture.mutations.find(m => m.method === 'POST' && m.path === '/api/tickets');
          assert.ok(mutation);
          assert.equal(mutation.body.title, 'Created in browser');
          assert.equal(mutation.body.body, 'Browser acceptance criteria.');
          assert.equal(mutation.body.path, '/fixtures/repository-with-a-long-name');
          await page.waitForURL('**/#/');
          await ready(page);
          await page.reload({ waitUntil: 'domcontentloaded' });
          await openTicket(page, mutation.ticket.id);
          await visible(page.getByRole('heading', { name: /Created in browser$/ }));
        });
        await t.test('scheduled creation validates the input and persists an open ticket', async () => {
          await routeTo(page, '#/new');
          await model(page, 'createForm.title').fill('Scheduled browser fixture');
          await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
          await page.getByRole('button', { name: 'later', exact: true }).click();
          await model(page, 'createForm.scheduled_at').fill('not a time');
          const submit = page.getByRole('button', { name: 'schedule ticket', exact: true });
          assert.ok(await submit.isDisabled(), 'Invalid schedules cannot be submitted');
          await model(page, 'createForm.scheduled_at').fill('2099-09-01 09:00');
          await submit.scrollIntoViewIfNeeded();
          await Promise.all([
            page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST'),
            submit.click(),
          ]);
          const mutation = fixture.mutations.find(m => m.body.title === 'Scheduled browser fixture');
          assert.equal(mutation?.body.status, 'open');
          assert.equal(mutation.body.scheduled_at, '2099-09-01T09:00:00Z');
        });
        await t.test('combined board card selection keeps namesakes distinct', async () => {
          await backToBoard(page);
          assert.equal(await page.getByRole('combobox', { name: 'Filter by ticket folder', exact: true }).count(), 0);
          const namesake = width < 768 ? page.getByRole('button').filter({ hasText: 'Secondary namesake', visible: true })
            : page.locator('[data-ticket-id="secondary/kon-draft"]:visible');
          await visible(namesake);
          const primary = width < 768 ? page.getByRole('button').filter({ hasText: 'Primary draft', visible: true })
            : page.locator('[data-ticket-id="kon-draft"]:visible');
          await visible(primary);
          await namesake.click();
          await page.waitForURL('**/#/s/secondary/t/kon-draft');
          await ready(page);
          assert.equal(new URL(page.url()).hash, '#/s/secondary/t/kon-draft');
          await backToBoard(page);
          await primary.click();
          await page.waitForURL('**/#/t/kon-draft');
          await ready(page);
          assert.equal(new URL(page.url()).hash, '#/t/kon-draft');
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).selectedTicket?.title), 'Primary draft');
          await backToBoard(page);
        });
        await t.test('primary description editing saves without replacing notes', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          const preview = width < 768 ? page.getByRole('button', { name: /^Edit (ticket )?description$/i })
            : page.locator('[x-ref="bodyPreview"]:visible');
          await preview.click();
          const editor = model(page, 'editForm.body');
          await editor.fill('Edited primary description.\n\n' + 'long-token-'.repeat(20));
          await editor.blur();
          await page.waitForFunction(() => Alpine.$data(document.body).selectedTicket?.description?.startsWith('Edited primary description.'));
          assert.equal(fixture.records.get('primary/kon-draft').description_tail, tail);
          assert.ok(fixture.records.get('primary/kon-draft').body.endsWith(tail));
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          await preview.click();
          assert.ok((await editor.inputValue()).startsWith('Edited primary description.'));
          await noPageOverflow(page);
        });
        await t.test('source-qualified description saves preserve the tail across reopen', async () => {
          await backToBoard(page);
          const primaryBefore = structuredClone(fixture.records.get('primary/kon-draft'));
          await openTicket(page, 'kon-draft', source);
          const preview = page.locator('[x-ref="bodyPreview"]:visible');
          const editor = model(page, 'editForm.body');
          await visible(editor.or(preview));
          if (await preview.isVisible()) await preview.click();
          await editor.fill('Edited secondary description.\n\n' + 'long-token-'.repeat(20));
          await editor.blur();
          await page.waitForFunction(() => {
            const app = Alpine.$data(document.body);
            return app.selectedTicket?.description?.startsWith('Edited secondary description.') && !app.editSubmitting;
          });
          assert.deepEqual(fixture.records.get('primary/kon-draft'), primaryBefore);
          const saved = fixture.records.get(`${source}/kon-draft`);
          assert.equal(saved.description_tail, tail);
          assert.ok(saved.body.endsWith(tail));
          assert.ok(fixture.mutations.some(m => m.method === 'PUT' && m.path === `/api/sources/${source}/tickets/kon-draft`));
          await backToBoard(page);
          await openTicket(page, 'kon-draft', source);
          await visible(editor.or(preview));
          if (await preview.isVisible()) await preview.click();
          assert.ok((await editor.inputValue()).startsWith('Edited secondary description.'));
          await noPageOverflow(page);
        });
        await t.test('init dialog owns focus and Escape closes it', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-draft');
          const queue = page.getByRole('button', { name: /^(queue|initialize|start)$/i }).filter({ visible: true }).first();
          await queue.click();
          const dialog = page.getByRole('dialog', { name: /Primary draft$/ });
          await visible(dialog);
          await inViewport(dialog);
          await page.waitForFunction(() => document.activeElement?.closest('[aria-labelledby="init-modal-title"]'));
          await page.keyboard.press('Escape');
          await dialog.waitFor({ state: 'hidden' });
        });
        await t.test('navigation dialog traps focus and restores its trigger', async () => {
          await backToBoard(page);
          const trigger = width < 768 ? page.getByRole('button', { name: 'Open navigation', exact: true })
            : page.getByRole('button', { name: 'Open command palette', exact: true }).filter({ visible: true });
          await trigger.click();
          const dialog = page.getByRole('dialog', { name: width < 768 ? 'Navigation' : 'Command palette', exact: true });
          await inViewport(dialog);
          await page.waitForFunction(() => document.activeElement?.closest('[role="dialog"]'));
          for (const key of ['Tab', 'Shift+Tab']) for (let i = 0; i < 12; i++) {
            await page.keyboard.press(key);
            assert.ok(await dialog.evaluate(el => el.contains(document.activeElement)), 'Tab must stay in modal');
          }
          // Tab drills into actions/schedule in the desktop palette. Escape
          // pops those scopes before closing; the phone sheet closes in one.
          for (let i = 0; i < (width < 768 ? 1 : 3) && await dialog.isVisible(); i++) {
            await page.keyboard.press('Escape');
          }
          await dialog.waitFor({ state: 'hidden' });
          await page.waitForFunction(label => document.activeElement?.getAttribute('aria-label') === label,
            width < 768 ? 'Open navigation' : 'Open command palette');
          assert.ok(await trigger.evaluate(el => el === document.activeElement), 'Closing restores trigger focus');
        });
        await t.test('repeated ticket and epic panels keep offline terminal grids contained', async () => {
          const socketsBefore = fixture.sockets.length;
          for (const id of terminalSubjects) for (let repeat = 0; repeat < 3; repeat++) {
            await backToBoard(page);
            await openTerminalSubject(page, id);
            const terminal = page.locator(terminalSelector);
            await terminalGridFits(page, fixture);
            await inViewport(terminal);
            if (width >= 768 && repeat === 0) {
              await page.getByRole('button', { name: 'read-only', exact: true }).filter({ visible: true }).click();
              await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected &&
                new URL(window.termState.ws.url).searchParams.get('rw') === '1');
              await terminalGridFits(page, fixture);
              const decoration = await terminal.evaluate(host => {
                const frame = host.parentElement, css = getComputedStyle(host), outer = getComputedStyle(frame);
                return { hostPadding: [css.paddingTop, css.paddingRight, css.paddingBottom, css.paddingLeft],
                  hostBorder: [css.borderTopWidth, css.borderRightWidth, css.borderBottomWidth, css.borderLeftWidth],
                  frameRing: frame.classList.contains('ring-1'), frameShadow: outer.boxShadow,
                  framePadding: [outer.paddingTop, outer.paddingRight], frameBorder: outer.borderTopWidth };
              });
              assert.deepEqual(decoration.hostPadding, ['0px', '0px', '0px', '0px']);
              assert.deepEqual(decoration.hostBorder, ['0px', '0px', '0px', '0px']);
              assert.equal(decoration.frameRing, true);
              assert.notEqual(decoration.frameShadow, 'none');
              assert.deepEqual(decoration.framePadding, ['14px', '18px']);
              assert.equal(decoration.frameBorder, '1px');
              await page.getByRole('button', { name: 'activity', exact: true }).filter({ visible: true }).click();
              await page.getByRole('button', { name: 'session', exact: true }).filter({ visible: true }).click();
              await terminalGridFits(page, fixture);
            }
            await backToBoard(page);
            await page.locator('.xterm:visible').waitFor({ state: 'hidden' });
            await openTicket(page, 'kon-draft', source);
            await visible(model(page, 'editForm.body').or(page.locator('[x-ref="bodyPreview"]:visible')));
          }
          assert.ok(fixture.sockets.length >= socketsBefore + 6, 'Each panel reopened an intercepted terminal');
          await backToBoard(page);
          await noPageOverflow(page);
        });
        await t.test('completed runs and passive epic routes', async () => {
          await backToBoard(page);
          await openTicket(page, 'kon-done');
          if (width < 768) {
            await page.getByRole('button', { name: '▤ ticket', exact: true }).click();
            await visible(page.getByText('Second code run complete.', { exact: true }).filter({ visible: true }));
          } else {
            const cards = page.locator('.stage-card-head:visible');
            await visible(cards.first());
            assert.equal(await cards.count(), 2, 'Repeated code runs have independent cards');
            const earlierBody = page.locator('.stage-card-body').last();
            await cards.last().click();
            await earlierBody.waitFor({ state: 'hidden' });
            assert.equal(await page.locator('.stage-card-body:visible').count(), 1);
            await cards.last().click();
            await visible(earlierBody);
            assert.equal(await page.locator('.stage-card-body:visible').count(), 2);
          }
          const socketsBefore = fixture.sockets.length;
          await backToBoard(page);
          await routeTo(page, '#/e/kon-epic');
          await visible(page.getByRole('heading', { name: /Passive fixture epic$/ }).first());
          assert.equal(fixture.sockets.length, socketsBefore, 'Passive epic must never attach a terminal');
          await noPageOverflow(page);
        });
      });
    });
  }
  for (const width of [390, 1024, 1280]) for (const theme of themes) {
    test(`Archive Back resumes retained outputs / ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        const phone = width < 768;
        const id = phone ? 'kon-done' : 'kon-live';
        await openTicket(page, id);
        await ready(page);
        const tab = phone ? page.locator('[x-text="mobileFirstTabLabel()"]:visible')
          : page.getByRole('button', { name: 'activity', exact: true }).filter({ visible: true });
        await tab.click();
        if (phone) await page.getByLabel('Stages and cost estimates').getByRole('button', { name: /^plan:/ }).click();
        const output = revision => page.getByText(phone
          ? `${id} plan fixture log revision ${revision}` : `Fixture activity revision ${revision}`, { exact: true }).filter({ visible: true });
        await visible(output(0));
        const endpoint = `GET /api/tickets/${id}/${phone ? 'logs' : 'activity'}`;
        for (const revision of [1, 2]) {
          await routeTo(page, '#/archive');
          await visible(page.getByRole('textbox', { name: 'Filter archive', exact: true }));
          const before = fixture.requests.filter(request => request === endpoint).length;
          fixture.behavior.outputRevision = revision;
          await page.goBack();
          await visible(output(revision));
          assert.equal(new URL(page.url()).hash, `#/t/${id}`);
          assert.ok(fixture.requests.filter(request => request === endpoint).length > before, 'Return reloads the retained output');
          if (!phone) {
            await page.waitForFunction(() => Alpine.$data(document.body)._activityPoll !== null);
            await page.waitForRequest(request => new URL(request.url()).pathname === `/api/tickets/${id}/activity`, { timeout: 6000 });
          }
        }
        await backToBoard(page);
        await openTicket(page, 'kon-live');
        const terminal = page.locator('#terminal-session:visible, #terminal-container-mobile:visible');
        await visible(terminal.locator('.xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        for (let repeat = 0; repeat < 2; repeat++) {
          await routeTo(page, '#/archive');
          await visible(page.getByRole('textbox', { name: 'Filter archive', exact: true }));
          const sockets = fixture.sockets.length;
          await page.goBack();
          await visible(terminal.locator('.xterm'));
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          assert.ok(fixture.sockets.length > sockets, 'Return reconnects the retained terminal');
          assert.equal(await page.locator('.xterm:visible').count(), 1);
          await inViewport(terminal);
        }
        await noPageOverflow(page);
      });
    });

    test(`filtered compact Settings exposes its query and clear / ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async page => {
        await page.setViewportSize({ width: 1280, height: 900 });
        await routeTo(page, '#/settings');
        await page.getByRole('navigation', { name: 'Settings sections', exact: true }).getByText('general', { exact: true }).click();
        const desktopSearch = page.getByRole('textbox', { name: 'Search settings', exact: true });
        await desktopSearch.fill('branch_prefix');
        await visible(page.locator('#gen-branch_prefix:visible'));
        await page.locator('#gen-tickets_dir').waitFor({ state: 'hidden' });
        for (const compactWidth of [...new Set([width <= 1100 ? width : 1100, 390])]) {
          await page.setViewportSize({ width: compactWidth, height: 900 });
          const search = page.getByRole('searchbox', { name: 'Search settings', exact: true });
          await inViewport(search);
          assert.equal(await search.inputValue(), 'branch_prefix');
          const clear = page.getByRole('button', { name: 'Clear settings search', exact: true });
          await inViewport(clear);
          await clear.click();
          assert.equal(await search.inputValue(), '');
          await visible(page.locator('#gen-tickets_dir:visible'));
          await search.fill('branch_prefix');
          await page.locator('#gen-tickets_dir').waitFor({ state: 'hidden' });
          await noPageOverflow(page);
        }
        await page.setViewportSize({ width: 1280, height: 900 });
        assert.equal(await desktopSearch.inputValue(), 'branch_prefix');
      });
    });
  }
  test('the harness rejects unmodelled API, asset, external HTTP and WebSocket requests', async () => {
    const context = await browser.newContext({ serviceWorkers: 'block' });
    const fixture = fixtures();
    await fixture.install(context);
    const page = await context.newPage();
    try {
      // about:blank has no document CSP, so these exercise the interceptor,
      // not just the application's self-only CSP. Nothing reaches a server.
      for (const url of [base + '/api/not-a-fixture', base + '/not-an-asset', 'https://external.invalid/not-allowed']) {
        await page.evaluate(url => fetch(url).catch(() => {}), url);
      }
      await page.evaluate(() => new Promise(resolve => {
        const ws = new WebSocket('ws://external.invalid/not-allowed');
        ws.onclose = ws.onerror = () => resolve();
      }));
      assert.equal(fixture.violations.length, 4);
      assert.ok(fixture.violations.some(message => message.includes('Unmodelled API')));
      assert.ok(fixture.violations.some(message => message.includes('Unknown asset')));
      assert.ok(fixture.violations.some(message => message.includes('External request')));
      assert.equal(fixture.sockets.length, 0);
    } finally {
      await context.close();
    }
  });
  for (const theme of themes) {
    test(`native phone forms, picker focus and short viewport / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        const opener = page.getByRole('button', { name: 'New ticket', exact: true }).filter({ visible: true });
        await opener.click();
        const dialog = page.getByRole('dialog', { name: 'New ticket', exact: true });
        const submit = dialog.getByRole('button', { name: 'create ticket', exact: true });
        assert.ok(await submit.isDisabled());
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        await dialog.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.equal(await page.locator(':focus').textContent(), 'cancel');
        await page.setViewportSize({ width: 390, height: 900 });
        assert.ok(await model(page, 'createForm.title').getAttribute('required') !== null);
        await model(page, 'createForm.title').fill('Native scheduled fixture');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        const date = page.getByLabel('Start at', { exact: true });
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await model(page, 'createForm.body').fill('Keep this draft while the viewport shrinks.');
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).isMobile), true);
        await dialog.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.ok(await submit.evaluate(el => el === document.activeElement));
        await submit.scrollIntoViewIfNeeded();
        await inViewport(submit);
        assert.equal(await model(page, 'createForm.body').inputValue(), 'Keep this draft while the viewport shrinks.');
        const picker = page.locator('#mobile-new-agent');
        await picker.scrollIntoViewIfNeeded();
        await picker.click();
        const panel = page.locator('#agent-picker-mobile-create');
        await visible(panel);
        await inViewport(panel);
        await page.waitForFunction(() => !!document.activeElement?.closest('[data-agent-picker-panel]'));
        await page.keyboard.press('Escape');
        await page.waitForFunction(() => !document.querySelector('#agent-picker-mobile-create')?.matches(':popover-open'));
        assert.ok(await dialog.isVisible(), 'Escape closes the top-layer picker first');
        assert.ok(await picker.evaluate(el => el === document.activeElement));
        fixture.behavior.createFailure = '<img src=x onerror=alert(1)> Request failed';
        await submit.scrollIntoViewIfNeeded();
        await submit.click();
        const error = dialog.locator('[x-text="createError"]');
        await visible(error);
        await error.scrollIntoViewIfNeeded();
        await inViewport(error);
        assert.equal(await error.textContent(), fixture.behavior.createFailure);
        assert.equal(await error.locator('img').count(), 0, 'Server text must not become markup');
        assert.equal(fixture.mutations.length, 0);
        fixture.behavior.createFailure = null;
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const scheduled = fixture.mutations.find(m => m.body.title === 'Native scheduled fixture');
        assert.equal(scheduled.body.scheduled_at, '2099-09-01T09:00:00Z');
        assert.equal(scheduled.body.status, 'open');
        await page.setViewportSize({ width: 390, height: 900 });
        await opener.click();
        await model(page, 'createForm.title').fill('Cleared phone schedule');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await date.fill('');
        await date.blur();
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const cleared = fixture.mutations.find(m => m.body.title === 'Cleared phone schedule');
        assert.equal(cleared.body.status, 'todo');
        assert.equal('scheduled_at' in cleared.body, false);
        await opener.click();
        await model(page, 'createForm.title').fill('Epic ignores old schedule');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        await date.fill('2099-09-01T09:00');
        await date.blur();
        await model(page, 'createForm.kind').selectOption('epic');
        await submit.click();
        await dialog.waitFor({ state: 'hidden' });
        const epic = fixture.mutations.find(m => m.body.title === 'Epic ignores old schedule');
        assert.equal(epic.body.status, 'open');
        assert.equal('scheduled_at' in epic.body, false);
        assert.equal('pipeline' in epic.body, false);
        await opener.click();
        await model(page, 'createForm.title').fill('Pending old form');
        await model(page, 'createForm.path').fill('/fixtures/repository-with-a-long-name');
        let release;
        fixture.behavior.createWait = new Promise(resolve => { release = resolve; });
        const request = page.waitForRequest(r => r.url() === base + '/api/tickets' && r.method() === 'POST');
        await submit.click();
        await request;
        await dialog.getByRole('button', { name: 'Close', exact: true }).click();
        await opener.click();
        await model(page, 'createForm.title').fill('Replacement draft');
        const response = page.waitForResponse(r => r.url() === base + '/api/tickets' && r.request().method() === 'POST');
        release();
        await response;
        await page.evaluate(() => new Promise(resolve => Alpine.nextTick(resolve)));
        assert.equal(await model(page, 'createForm.title').inputValue(), 'Replacement draft');
        assert.ok(await dialog.isVisible());
        assert.equal(await error.isVisible(), false);
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        assert.ok(await opener.evaluate(el => el === document.activeElement));
        await page.setViewportSize({ width: 390, height: 900 });
        await openTicket(page, 'kon-draft');
        await page.getByRole('button', { name: 'Actions', exact: true }).click();
        await page.getByRole('dialog', { name: 'Ticket actions' }).getByRole('button', { name: 'Schedule…', exact: true }).click();
        const schedule = page.getByRole('dialog', { name: 'Start later', exact: true });
        const field = schedule.getByRole('textbox', { name: 'Start date or duration', exact: true });
        await field.fill('2h');
        await page.setViewportSize({ width: 390, height: 340 });
        await page.waitForFunction(() => document.documentElement.style.getPropertyValue('--mobile-vv-height') === '340px');
        await field.scrollIntoViewIfNeeded();
        await inViewport(field);
        const saveSchedule = schedule.getByRole('button', { name: 'schedule', exact: true });
        await saveSchedule.scrollIntoViewIfNeeded();
        await inViewport(saveSchedule);
        assert.equal(await saveSchedule.isDisabled(), false);
        await schedule.getByRole('button', { name: 'Close', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        await inViewport(page.locator(':focus'));
        assert.ok(await schedule.getByRole('button', { name: 'run now instead', exact: true }).evaluate(el => el === document.activeElement));
        await page.keyboard.press('Escape');
        await schedule.waitFor({ state: 'hidden' });
        await noPageOverflow(page);
      });
    });

    test(`dirty phone Settings resumes exact routes and handles failed Save / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        let attempt = 0;
        for (const destination of ['#/t/kon-live', '#/s/secondary/t/kon-draft', '#/e/kon-epic']) {
          const prefix = 'unsaved-prefix-' + attempt++;
          await routeTo(page, '#/settings');
          await page.locator('#settings-mobile-section').selectOption('general');
          await page.locator('#gen-branch_prefix').fill(prefix);
          await page.evaluate(route => { location.hash = route; }, destination);
          const guard = page.getByRole('dialog', { name: 'Leave settings?' });
          await visible(guard);
          assert.equal(await page.evaluate(() => Alpine.$data(document.body).currentView), 'settings');
          await guard.getByRole('button', { name: 'keep editing', exact: true }).click();
          await guard.waitFor({ state: 'hidden' });
          assert.equal(new URL(page.url()).hash, '#/settings');
          assert.equal(await page.locator('#gen-branch_prefix').inputValue(), prefix);
          await page.evaluate(route => { location.hash = route; }, destination);
          await visible(guard);
          fixture.behavior.saveFailure = 'Fixture save refused';
          await guard.getByRole('button', { name: 'save & leave', exact: true }).click();
          await visible(guard.getByText('Fixture save refused', { exact: true }));
          assert.equal(new URL(page.url()).hash, '#/settings');
          fixture.behavior.saveFailure = null;
          await guard.getByRole('button', { name: 'save & leave', exact: true }).click();
          await guard.waitFor({ state: 'hidden' });
          await page.waitForURL('**/' + destination);
          assert.equal(new URL(page.url()).hash, destination);
          if (destination.includes('kon-live')) {
            await visible(page.locator('#terminal-container-mobile .xterm'));
            assert.equal(await page.locator('.xterm:visible').count(), 1);
          }
          await backToBoard(page);
        }
      });
    });

    test(`assistant views retain flex space in a short phone viewport / ${theme}`, async t => {
      await withPage(t, 390, theme, async page => {
        await page.getByRole('button', { name: 'Assistant', exact: true }).filter({ visible: true }).click();
        const dialog = page.getByRole('dialog', { name: 'Assistant', exact: true });
        await visible(dialog);
        await page.setViewportSize({ width: 390, height: 420 });
        for (let i = 0; i < 3; i++) {
          await dialog.getByRole('button', { name: 'Chat', exact: true }).click();
          const thread = page.locator('#assistant-sheet-thread');
          await visible(thread);
          assert.equal(await thread.evaluate(el => getComputedStyle(el).display), 'flex');
          assert.ok((await thread.boundingBox()).height > 0);
          const composer = page.getByRole('textbox', { name: 'Message the assistant', exact: true });
          await composer.fill('A recoverable unsent draft');
          await composer.scrollIntoViewIfNeeded();
          await inViewport(composer);
          const send = dialog.getByRole('button', { name: 'send', exact: true });
          await send.scrollIntoViewIfNeeded();
          await inViewport(send);
          await dialog.getByRole('button', { name: 'History', exact: true }).click();
          await visible(dialog.getByText('No chats yet.', { exact: true }));
          await dialog.getByRole('button', { name: 'Inbox', exact: true }).click();
        }
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        await noPageOverflow(page);
      });
    });
  }
  for (const width of [320, 390]) for (const theme of themes) {
    test(`supervisor terminal input lifecycle ${width}px / ${theme}`, async t => {
      await withPage(t, width, theme, async (page, fixture) => {
        await routeTo(page, '#/e/' + supervisorID);
        const epic = page.locator('.mobile-epic');
        await epic.getByRole('button', { name: 'session', exact: true }).click();
        await visible(page.locator('#epic-terminal-mobile .xterm'));
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el.readOnly), false);
        await epic.getByRole('button', { name: 'Escape', exact: true }).click();
        await expect.poll(() => fixture.messages.filter(message => message.type === 'input').at(-1)?.data).toBe('\x1b');
        assert.equal(fixture.messages.filter(message => message.type === 'input').at(-1).url.searchParams.get('rw'), '1');
        assert.equal(await page.evaluate(() => Alpine.$data(document.body).terminalRW), true);
        assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el === document.activeElement), false);
        await page.locator('#epic-terminal-mobile .xterm-screen').click();
        await page.keyboard.type('supervisor-input');
        for (const height of [420, 360, 900]) {
          const top = height === 900 ? 0 : 12;
          await visualViewport(page, height, top);
          await page.waitForFunction(({ height, top }) => {
            const box = document.querySelector('.mobile-epic').getBoundingClientRect();
            return Math.abs(box.height - height) < 1 && Math.abs(box.top - top) < 1;
          }, { height, top });
          assert.equal(await page.evaluate(() => innerHeight), 900, 'Only the visual viewport shrinks');
          await terminalGridFits(page, fixture, 8);
          const done = epic.getByRole('button', { name: 'Done typing', exact: true });
          const box = await done.boundingBox();
          assert.ok(box && box.width >= 44 && box.height >= 44 && box.x >= 0 && box.x + box.width <= width &&
            box.y >= top && box.y + box.height <= top + height,
          `Input exit outside visible overlay: ${JSON.stringify(box)} at ${width}x${height}`);
          assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el === document.activeElement), true);
          assert.equal(await done.evaluate(el => {
            const box = el.getBoundingClientRect();
            return el.contains(document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2));
          }), true, 'Input exit must receive touch input');
          await done.click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected && !Alpine.$data(document.body).terminalRW);
          assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el === document.activeElement), false);
          await visualViewport(page, 900);
          await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
          await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
          await page.locator('#epic-terminal-mobile .xterm-screen').click();
        }
        await page.keyboard.press('Escape');
        assert.equal(new URL(page.url()).hash, '#/e/' + supervisorID);
        await visible(epic.getByRole('button', { name: 'Enable input', exact: true }));
        await epic.getByRole('button', { name: 'Enable input', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        await epic.getByRole('button', { name: 'activity', exact: true }).click();
        await epic.getByRole('button', { name: 'session', exact: true }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).terminalConnected);
        assert.equal(await page.locator('#epic-terminal-mobile textarea').evaluate(el => el.readOnly), true);
        assert.ok(fixture.sockets.every(url => url.pathname === '/ws/terminal/' + supervisorID));
        assert.ok(fixture.messages.some(message => message.type === 'input'));
        assert.equal(await page.evaluate(() => window.__fixtureSockets.filter(ws => ws.readyState < 2).length), 1);
      });
    });
  }
  for (const theme of themes) {
    test(`live terminal survives breakpoint roundtrip / ${theme}`, async t => {
      await withPage(t, 390, theme, async (page, fixture) => {
        for (const id of terminalSubjects) {
          await page.setViewportSize({ width: 390, height: 900 });
          await backToBoard(page);
          await openTerminalSubject(page, id);
          const socketsBefore = fixture.sockets.length;
          for (const width of [...widths, 390]) for (const height of width === 1280 ? [900, 600, 900] : [900]) {
            await page.setViewportSize({ width, height });
            await page.waitForFunction(({ width }) => {
              const app = Alpine.$data(document.body);
              return app.isMobile === (width < 768) && (width >= 768 ||
                Math.abs(parseFloat(document.documentElement.style.getPropertyValue('--mobile-vv-width')) - width) < 1);
            }, { width });
            await terminalGridFits(page, fixture);
            assert.equal(await page.evaluate(() => termState.term.options.scrollback), width < 768 ? 0 : 5000);
            assert.equal(new URL(page.url()).hash, `#/${id === supervisorID ? 'e' : 't'}/${id}`);
            await inViewport(page.locator(terminalSelector));
            await noPageOverflow(page);
          }
          assert.ok(fixture.sockets.length >= socketsBefore + 2, 'Breakpoint crossings reconnect each subject');
        }
      });
    });
  }
});

describe('start-dialog notification availability', () => {
  async function openStartDialog(page) {
    await openTicket(page, 'kon-draft');
    await page.getByRole('button', { name: /^(queue|initialize|start)$/i }).filter({ visible: true }).first().click();
    const dialog = page.getByRole('dialog', { name: /Primary draft$/ });
    await visible(dialog);
    return dialog;
  }

  async function choices(page) {
    return page.evaluate(() => {
      const form = Alpine.$data(document.body).initForm;
      return { notify: [...form.notify], notify_channels: [...form.notifyChannels] };
    });
  }

  async function unavailableControls(dialog, loading = false) {
    const group = dialog.getByRole('group', { name: 'notify me', exact: true });
    assert.equal(await group.getAttribute('aria-describedby'), loading ? 'init-notify-loading' : 'init-notify-unavailable');
    await expect(group).toHaveAccessibleDescription(loading
      ? 'Waiting for notification configuration. If loading does not finish, close and reopen this dialog to retry.'
      : /No notification channels are configured\..*To start without notifications, select off first, even if it is already selected\. Configure notifications/);
    assert.equal(await group.getByRole('radiogroup', { name: 'notify me', exact: true }).count(), 1);
    assert.equal(await group.getByRole('radio').count(), 3);
    for (const radio of await group.getByRole('radio').all()) {
      const off = (await radio.textContent()).trim().toLowerCase() === 'off';
      assert.equal(await radio.isDisabled(), loading || !off);
    }
    assert.equal(await dialog.getByRole('button', { name: 'custom', exact: true }).isDisabled(), true);
    const chips = dialog.locator('button.notify-chip');
    assert.ok(await chips.count(), 'Custom status controls exist even when collapsed');
    for (const chip of await chips.all()) assert.equal(await chip.isDisabled(), true);
  }

  async function noChannelGuidance(dialog) {
    const help = dialog.locator('#init-notify-help');
    await visible(help);
    assert.match((await help.innerText()).replace(/\s+/g, ' '), /No notification channels are configured\..*notifications\.channels.*restart the daemon\. To start without notifications, select off first, even if it is already selected\./);
    const link = help.getByRole('link');
    assert.equal(await link.getAttribute('href'), 'https://worksonmyai.github.io/kontora/configuration/#notifications');
    assert.equal(await link.getAttribute('target'), '_blank');
    assert.deepEqual((await link.getAttribute('rel')).split(/\s+/).sort(), ['noopener', 'noreferrer']);
    await link.scrollIntoViewIfNeeded();
    await inViewport(link);
  }

  async function start(page, fixture, dialog) {
    const [response] = await Promise.all([
      page.waitForResponse(r => r.url() === base + '/api/tickets/kon-draft/init' && r.request().method() === 'POST'),
      dialog.getByRole('button', { name: 'start ticket', exact: true }).click(),
    ]);
    assert.equal(response.status(), 200);
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(fixture.mutations.length, 1, 'Notification choices must not autosave before init');
    const mutation = fixture.mutations[0];
    assert.equal(mutation.method, 'POST');
    assert.equal(mutation.path, '/api/tickets/kon-draft/init');
    return { notify: mutation.body.notify, notify_channels: mutation.body.notify_channels };
  }

  for (const width of [1280, 390]) {
    test(`no configured channels, already off / ${width}px`, async t => {
      await withPage(t, width, 'dark', async (page, fixture) => {
        const dialog = await openStartDialog(page);
        await unavailableControls(dialog);
        await noChannelGuidance(dialog);
        const off = dialog.getByRole('radio', { name: 'off', exact: true });
        assert.equal(await off.getAttribute('aria-checked'), 'true');
        const submit = dialog.getByRole('button', { name: 'start ticket', exact: true });
        assert.equal(await submit.isDisabled(), false);
        await inViewport(submit);
        await noPageOverflow(page);
        assert.deepEqual(await start(page, fixture, dialog), { notify: [], notify_channels: [] });
      }, { config: { channels: [], default_channels: [] }, ticket: { notify: [], notify_channels: [] } });
    });
  }

  for (const notify of [['paused', 'done'], []]) {
    test(`removed channel keeps initial choices until explicit Off recovery / ${notify.length ? 'enabled' : 'already off'}`, async t => {
      const initial = { notify, notify_channels: ['removed-channel'] };
      await withPage(t, 1280, 'dark', async (page, fixture) => {
        const dialog = await openStartDialog(page);
        await unavailableControls(dialog);
        await noChannelGuidance(dialog);
        assert.deepEqual(await choices(page), initial);
        assert.equal(await dialog.getByRole('radio', { name: 'off', exact: true }).getAttribute('aria-checked'), String(!notify.length));
        for (const status of ['paused', 'done']) {
          assert.equal(await dialog.locator(`.notify-chip[data-status="${status}"]`).getAttribute('aria-pressed'), String(notify.includes(status)));
        }
        assert.deepEqual(fixture.mutations, []);
        await dialog.getByRole('radio', { name: 'off', exact: true }).click();
        assert.deepEqual(await choices(page), { notify: [], notify_channels: [] });
        assert.equal(await dialog.getByRole('radio', { name: 'off', exact: true }).getAttribute('aria-checked'), 'true');
        assert.deepEqual(await start(page, fixture, dialog), { notify: [], notify_channels: [] });
      }, { config: { channels: [], default_channels: [] }, ticket: initial });
    });
  }

  test('one configured channel without an inherited route enables choices and requires a channel', async t => {
    await withPage(t, 390, 'dark', async (page, fixture) => {
      const dialog = await openStartDialog(page);
      const group = dialog.getByRole('radiogroup', { name: 'notify me', exact: true });
      for (const radio of await group.getByRole('radio').all()) assert.equal(await radio.isDisabled(), false);
      assert.equal(await dialog.getByRole('button', { name: 'custom', exact: true }).isDisabled(), false);
      assert.equal(await dialog.locator('#init-notify-help').isVisible(), false);
      const notificationGroup = dialog.getByRole('group', { name: 'notify me', exact: true });
      assert.equal(await notificationGroup.getAttribute('aria-describedby'), null);
      await expect(notificationGroup).toHaveAccessibleDescription('');
      const needs = group.getByRole('radio', { name: 'when it needs me', exact: true });
      await needs.click();
      assert.deepEqual(await choices(page), { notify: ['paused', 'human_review', 'waiting'], notify_channels: [] });
      const submit = dialog.getByRole('button', { name: 'start ticket', exact: true });
      assert.equal(await submit.isDisabled(), false);
      await submit.click();
      const error = dialog.getByText('Choose a notification channel, configure an inherited route, or turn notifications off.', { exact: true });
      await visible(error);
      assert.deepEqual(fixture.mutations, []);
      assert.equal(fixture.requests.includes('POST /api/tickets/kon-draft/init'), false);
      const channel = dialog.getByRole('button', { name: 'ops', exact: true });
      assert.equal(await channel.isDisabled(), false);
      await channel.click();
      await error.waitFor({ state: 'hidden' });
      await group.getByRole('radio', { name: 'off', exact: true }).click();
      assert.deepEqual(await choices(page), { notify: [], notify_channels: ['ops'] });
      await needs.click();
      assert.equal(await channel.getAttribute('aria-pressed'), 'true');
      assert.deepEqual(await start(page, fixture, dialog), {
        notify: ['paused', 'human_review', 'waiting'], notify_channels: ['ops'],
      });
    }, { config: { channels: ['ops'], default_channels: [] }, ticket: { notify: [], notify_channels: [] } });
  });

  for (const pending of [true, false]) {
    test(`${pending ? 'pending' : 'failed'} config disables even Off without changing initial choices`, async t => {
      const initial = { notify: ['paused'], notify_channels: ['ops'] };
      await withPage(t, 1280, 'dark', async (page, fixture) => {
        const failure = pending ? null : page.waitForResponse(r => r.url() === base + '/api/config' && r.status() === 503);
        let dialog = await openStartDialog(page);
        if (failure) await failure;
        await unavailableControls(dialog, true);
        const help = dialog.locator('#init-notify-help');
        await visible(help);
        assert.match(await help.innerText(), /loading/i);
        assert.equal(await help.getByRole('link').isVisible(), false);
        assert.deepEqual(await choices(page), initial);
        assert.deepEqual(fixture.mutations, []);
        if (pending) {
          fixture.releaseConfig();
        } else {
          fixture.behavior.configFailure = false;
          await dialog.getByRole('button', { name: 'cancel', exact: true }).click();
          dialog = await openStartDialog(page);
        }
        await page.waitForFunction(() => {
          const radios = document.querySelectorAll('[aria-labelledby="init-modal-title"] [role="radio"]');
          return radios.length === 3 && [...radios].every(radio => !radio.disabled);
        });
        await help.waitFor({ state: 'hidden' });
        const group = dialog.getByRole('group', { name: 'notify me', exact: true });
        assert.equal(await group.getAttribute('aria-describedby'), null);
        await expect(group).toHaveAccessibleDescription('');
        assert.equal(await dialog.getByRole('button', { name: 'custom', exact: true }).isDisabled(), false);
        assert.equal(await dialog.getByRole('button', { name: 'ops', exact: true }).isDisabled(), false);
        assert.deepEqual(await choices(page), initial);
        assert.deepEqual(await start(page, fixture, dialog), initial);
      }, { configRetryPending: pending, configFailure: !pending,
        config: { channels: ['ops'], default_channels: ['ops'] }, ticket: initial });
    });
  }
});

for (const width of [390, 1280]) {
  test(`Archive settles and reloads after navigation through shipped Alpine / ${width}px`, async t => {
    await withPage(t, width, 'dark', async (page, fixture) => {
      for (let visit = 1; visit <= 3; visit++) {
        await routeTo(page, '#/archive');
        await visible(page.getByText('Archived fixture', { exact: true }));
        await page.waitForFunction(() => !Alpine.$data(document.body).archiveLoading);
        assert.equal(fixture.requests.filter(r => r === 'GET /api/tickets/archived').length, visit);
        assert.equal(await page.evaluate(() => Alpine.$data(document.body)._archiveFlight), null);
        await routeTo(page, '#/');
        await page.locator('#archive-view').waitFor({ state: 'detached' });
      }
      assert.deepEqual(fixture.mutations, []);
    });
  });

  test(`large-board metadata through shipped Alpine / ${width}px`, async t => {
    await withPage(t, width, 'dark', async page => {
      const result = await page.evaluate(() => {
        const state = Alpine.reactive(kontora());
        let scans = 0;
        state.tickets = Array.from({ length: 5 }, (_, i) => ({ id: 'primary-' + i, title: 'Primary', status: 'open' }));
        state.secondaryTickets = Array.from({ length: 2161 }, (_, i) => ({
          id: 'secondary-' + i, source: 'secondary', title: 'Secondary ' + i,
          get status() { scans++; return i < 1742 ? 'archived' : 'open'; },
        }));
        state.recomputeBoard();
        const columns = state.columns;
        state.setActiveColumn(columns.findIndex(c => c.key === 'archived'));
        const initial = state.mobileTickets().length;
        state.mobileLoadMore();
        const expanded = state.mobileTickets().length;
        state.secondaryTickets[0].title = 'Renamed';
        state.recomputeBoard();
        const stable = columns === state.columns;
        scans = 0;
        for (let i = 0; i < 1000; i++) {
          state.columns;
          state.mobileColumn();
          state.mobileTickets();
        }
        const repeatedReads = scans;
        const counts = [state._boardTotal, state.boardTickets('archived').length, state.secondaryTickets.length];
        state.searchQuery = 'secondary-1741';
        state.recomputeBoard();
        return { initial, expanded, stable, repeatedReads, counts,
          filtered: state._boardTotal, visible: state.mobileTickets().length, retained: state.secondaryTickets.length };
      });
      assert.deepEqual(result, { initial: 100, expanded: 200, stable: true, repeatedReads: 0,
        counts: [2166, 1742, 2161], filtered: 1, visible: 1, retained: 2161 });
    });
  });
}
