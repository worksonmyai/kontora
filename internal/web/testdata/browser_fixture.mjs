import assert from 'node:assert/strict';
import * as yaml from '../static/vendor/yaml@2.8.1/yaml.mjs';

export const source = 'secondary';
export const sourcePath = '/fixtures/team-tickets';
export const secondSource = 'secondary-two';
export const secondSourcePath = '/fixtures/other-team-tickets';
export const unavailablePath = '/fixtures/' + 'unavailable-folder-'.repeat(12);
export const longID = 'kon-' + 'long-identifier-'.repeat(12);
export const tail = '\n## Notes\n\nProtected note identity: browser-note-1\n\n## Audit\nDo not edit this tail.\n';
export const stamp = '2026-10-01T10:00:00Z';
export const liveTitle = 'Running fixture with a long title explaining terminal keyboard layout and lifecycle behavior';
export const liveBranch = 'fixture/mobile-terminal-keyboard-layout-and-input-lifecycle-with-long-metadata';
export const liveAgent = 'fixture-agent-with-a-long-descriptive-name';
export const supervisorID = 'abcdef-abcd';
const themeModes = new Set(['system', 'light', 'dark']);

export function ticket(id, title, extra = {}) {
  const description = `# ${title}\n\nFixture description.\n\n`;
  return { id, title, status: 'open', kontora: true, source: 'primary',
    path: '/fixtures/repository-with-a-long-name', pipeline: 'standard', agent: 'fixture-agent',
    stages: ['plan', 'code'], stage: 'plan', created_at: stamp, updated_at: stamp,
    history: [], dependencies: [], children: [], links: [],
    description, description_tail: tail, body: description + tail,
    can_edit: true, can_control: true, ...extra };
}

// Settings use a separate raw document from /api/config: draft definitions must
// not silently come from the operational cache. No fixture URL is contacted.
export const settingsRaw = `# Browser settings fixture: preserve comments and aliases.
tickets_dir: /fixtures/primary
worktrees_dir: /fixtures/worktrees
logs_dir: /fixtures/logs
branch_prefix: fixture
auto_pick_up: false
max_concurrent_agents: 3
max_concurrent_epic_supervisors: 9007199254740993
default_agent: fixture-agent
environment: &shared_environment
  FIXTURE_MODE: never-search-environment-value
agents:
  fixture-agent:
    binary: pi
    args: [--model, fixture-model]
    environment: *shared_environment
  draft-only:
    binary: claude
  unsupported:
    binary: custom-runner
  a/b:
    binary: pi
  a_2Fb:
    binary: claude
  ' spaced agent ':
    binary: pi
stages:
  plan:
    prompt: never-search-stage-prompt
    timeout: 30m
  code:
    prompt: Implement the fixture.
    timeout: 1h30m
    model:
      pi: fixture-model
pipelines:
  standard:
    - stage: plan
      agent: fixture-agent
      on_success: next
      on_failure: paused
    - stage: code
      agent: draft-only
      on_success: done
      on_failure: paused
projects:
  Fixture repository:
    path: /fixtures/repository-with-a-long-name
    pipeline: standard
    agent: fixture-agent
    branch_prefix: fixture
    notify_channels: [hook]
web:
  host: 127.0.0.1
  port: 8080
  token: never-search-web-token
assistant:
  agent: fixture-agent
  model: fixture-chat-model
  timeout: 10m
  autonomy: ask
  prompt: never-search-assistant-prompt
  companion:
    enabled: true
    name: Fixture companion
plannotator:
  binary: plannotator
  timeout: 30m
  reviews_dir: /fixtures/reviews
notifications:
  enabled: true
  timeout: 10s
  attempts: 3
  backoff: 5s
  default: [hook]
  channels:
    hook:
      type: webhook
      url: https://fixture.invalid/hook
      method: POST
      headers:
        X-Fixture: never-search-header-value
    telegram:
      type: telegram
      chat_id: '-1001234567890'
      secret_env: FIXTURE_BOT_TOKEN
statuses: [needs_review]
`;

export function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

export function detailPath(folder, id) {
  return (folder === 'primary' ? '/api' : `/api/sources/${encodeURIComponent(folder)}`) + `/tickets/${encodeURIComponent(id)}`;
}

function listProjection(record) {
  const result = structuredClone(record);
  for (const field of ['body', 'description', 'description_tail', 'history', 'notes', 'last_error', 'last_log', 'summary', 'final_summary']) delete result[field];
  if (record.source !== 'primary') {
    for (const field of ['blocks', 'children']) delete result[field];
  }
  return result;
}

// Deterministic populated windows; range names and fields match stats.Result.
// Opt in to avoid multiplying the existing empty-chart browser matrix.
export function populatedStats(range = '90d') {
  const count = { '1d': 1, '1w': 7, '30d': 35, '90d': 98, all: 182 }[range];
  assert.ok(count, `Unknown Stats fixture range: ${range}`);
  const end = Date.UTC(2026, 10, 18), dayMS = 86400000;
  const date = time => new Date(time).toISOString().slice(0, 10);
  const start = end - (count - 1) * dayMS;
  const days = Array.from({ length: count }, (_, i) => ({ date: date(start + i * dayMS), runs: count === 1 ? 120 : (i % 5 + 2) * 4 }));
  const unit = count === 1 ? 'hour' : count <= 7 ? 'day' : 'week';
  const bucketCount = unit === 'hour' ? 24 : unit === 'day' ? count : Math.ceil((count + (new Date(start).getUTCDay() + 6) % 7) / 7);
  const bucketStart = unit === 'week' ? start - ((new Date(start).getUTCDay() + 6) % 7) * dayMS : start;
  const buckets = Array.from({ length: bucketCount }, (_, i) => ({
    start: unit === 'hour' ? `${date(end)}T${String(i).padStart(2, '0')}:00` : date(bucketStart + i * dayMS * (unit === 'week' ? 7 : 1)),
    done: i % 4 + 1, cancelled: i % 3 === 0 ? 1 : 0,
    tokens_in: (i + 1) * 1000, tokens_out: (i + 1) * 100,
    tokens_cache_create: (i + 1) * 100, tokens_cache_read: (i + 1) * 200,
  }));
  const sum = key => buckets.reduce((n, b) => n + b[key], 0);
  const runs = days.reduce((n, d) => n + d.runs, 0), tokens = sum('tokens_in') + sum('tokens_out');
  const busiest = days.reduce((best, day) => day.runs >= best.runs ? day : best);
  return { window: { days: count, unit, buckets: buckets.length, from: date(start), to: date(end) }, days, buckets,
    totals: { shipped: sum('done'), shipped_this_week: 4, runs, median_cycle_ms: 3600000, median_cycle_delta_ms: -60000,
      first_pass_pct: 75, first_pass_visits: 3, tokens_in: sum('tokens_in'), tokens_out: sum('tokens_out'),
      tokens_cache_create: sum('tokens_cache_create'), tokens_cache_read: sum('tokens_cache_read'), tokens_delta_pct: 12,
      busiest_day: busiest.date, busiest_day_runs: busiest.runs },
    live: { running: 1, slots: 3, queued: 2, oldest_wait_ms: 60000, in_review: 1, busy: ['fixture-agent'] },
    stages: [{ name: 'plan', p50_ms: 60000, p90_ms: 120000, share: 25, runs: 1, complete: 1, failed: 0, unknown: 0,
      waiting: 0, blocked: 0, protocol_error: 0, runner_failed: 0, retry_pct: 0, tokens: tokens / 4, tokens_p90: tokens / 4, token_share: 25, token_runs: 1 },
    { name: 'code', p50_ms: 180000, p90_ms: 240000, share: 75, runs: runs - 1, complete: runs - 2, failed: 1, unknown: 0,
      waiting: 0, blocked: 0, protocol_error: 0, runner_failed: 1, retry_pct: 25, tokens: tokens * 3 / 4, tokens_p90: tokens / 2, token_share: 75, token_runs: runs - 1 }],
    agents: [{ name: 'fixture-agent', model: 'fixture-model', runs, first_pass_pct: 75, first_pass_visits: 3,
      median_ms: 120000, tokens_per_run: Math.round(tokens / runs), retries_per_ticket: 0.25 }],
    projects: [{ name: 'Fixture repository', done: sum('done'), median_cycle_ms: 3600000 }] };
}

// Keys include source and id so a primary PUT cannot save a secondary namesake.
export function fixtures({ base, assets, engine }, options = {}) {
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
  if (options.secondSecondary) {
    put(ticket('kon-draft', 'Second secondary namesake', { source: secondSource, kontora: false,
      can_control: false, can_claim: false, description: 'Other secondary-only description.\n', body: 'Other secondary-only description.\n' + tail }));
    for (const folder of [source, secondSource]) {
      Object.assign(records.get(key(folder, 'kon-draft')), { summary: 'Shared summary references kon-reference.',
        history: [{ stage: 'code', run: 0, completed_at: stamp, exit_code: 0, summary: 'Shared summary references kon-reference.' }] });
      put(ticket('kon-reference', `${folder} reference target`, { source: folder, kontora: false, can_control: false }));
    }
  }
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
  if (options.onlyRecords) records.clear();
  for (const record of options.records || []) put(record);
  if (options.empty) records.clear();
  const config = { config_path: '/fixtures/config.yaml', theme: options.themePreference ?? 'system', revision: 1, pipelines: ['standard'], default_pipeline: 'standard',
    pipeline_infos: [{ name: 'standard', stages: ['plan', 'code'] }],
    agents: ['fixture-agent'], agent_infos: [{ name: 'fixture-agent', binary: 'pi' }],
    projects: [{ name: 'Fixture repository', path: '/fixtures/repository-with-a-long-name', agent: 'fixture-agent', pipeline: 'standard' }],
    custom_statuses: [], stages: ['plan', 'code'], channels: [], default_channels: [], ...options.config };
  const requests = [], mutations = [], sockets = [], socketRoutes = [], messages = [], terminalInputs = [], violations = [], expectedHTTPFailures = [];
  const descriptionWrites = [], pendingResizes = [], pickupRequests = [], enqueued = [], artifactReads = [];
  const uploads = [], stylesheetRequests = [], authNavigations = [];
  const rawOutputs = new Map(Object.entries(options.rawOutputs || {
    'kon-done/code/0': 'Recorded occurrence zero: failed code visit.\n',
    'kon-done/code/1': 'Recorded occurrence one: completed code visit.\n',
    'kon-live/code/0': 'Live code registration: unrecorded running output.\n',
  }));
  let mintedNotes = 0, createdThreads = 0;
  const terminalMarkers = new Map();
  const behavior = { createFailure: null, createWait: null, saveFailure: null, outputRevision: 0, activityEvents: null, bracketedPaste: false,
    descriptionWait: null, descriptionFailure: null, editWait: null, holdResizes: false,
    epicWait: null, epicFailure: null, assistantMessageFailure: null, assistantGateWait: null,
    changesWait: null, changesFailure: null, lifecycleWait: null, lifecycleFailure: null,
    // Keys are exact request paths. Refusals persist; post-write failures are consumed once.
    lifecycleRefusals: new Map(), lifecyclePostWriteFailures: new Map(), lifecycleAckWait: null,
    noteWait: null, noteDispatchWait: null, noteFailure: null, noteLoseResponse: false,
    assistantCreateWait: null, assistantCreateFailure: null, assistantMissingThreads: new Set(options.assistantMissingThreads || []),
    uploadWait: null, uploadResponse: options.uploadResponse || null, stylesheetFailures: options.stylesheetFailures || 0, rawFailures: new Map(),
    authRequired: options.authRequired || false, authToken: 'fixture-token',
    automationFailure: null, automationReadFailure: null, automationWait: null, automationsAvailable: true, loseCreate: false, loseRun: false,
    unavailableFolder: false, sourcesFailure: false, configFailure: options.configFailure || false,
    rawConfigUnavailable: options.rawConfigUnavailable || false, rawReadWait: null, configRefreshWait: null,
    saveWait: null, saveResponse: null, conflictOnSave: false, themeWait: null, themeFailure: null,
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
  const configWrites = [];
  let rawConfig = options.rawConfig ?? 'tickets_dir: /fixtures/primary\nauto_pick_up: false\nmax_concurrent_agents: 3\nagents:\n  fixture-agent:\n    binary: pi\n';
  const list = folder => [...records.values()].filter(t => t.source === folder && !t.archived_at)
    .map(t => options.thinLists ? listProjection(t) : t);
  const active = record => record.execution_active ?? (record.status === 'in_progress');
  const json = (route, value, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
  const heldReads = [], readQueues = new Map();
  function holdRead(path, { delayBody = false } = {}) {
    const started = deferred(), response = deferred(), done = deferred(), body = deferred(), decoding = deferred();
    const held = { path, started: started.promise, done: done.promise, decoding: decoding.promise, claimed: false,
      decodeID: delayBody ? String(heldReads.length) : null, body,
      releaseBody: () => body.resolve(), signalDecoding: decoding.resolve,
      release: result => response.resolve(structuredClone(result || {})), response, signalStarted: started.resolve, signalDone: done.resolve };
    if (!delayBody) held.releaseBody();
    heldReads.push(held);
    if (!readQueues.has(path)) readQueues.set(path, []);
    readQueues.get(path).push(held);
    return held;
  }
  const holdSnapshot = (folder = 'primary') => holdRead(folder === 'primary' ? '/api/tickets' : `/api/sources/${folder}/tickets`);
  const holdDetail = (folder, id = 'kon-draft') => holdRead(detailPath(folder, id));
  const pendingSnapshot = options.pendingSnapshot ? holdSnapshot(options.pendingSnapshot) : null;
  async function readResponse(route, path, value) {
    const failureURL = route.request().url?.() || base + path;
    const held = readQueues.get(path)?.shift();
    if (!held) return json(route, value);
    const snapshot = structuredClone(value);
    held.claimed = true;
    held.signalStarted();
    try {
      const result = await held.response.promise;
      if (result.failure === 'network') {
        expectedHTTPFailures.push(failureURL);
        return await route.abort('failed');
      }
      if (result.failure === 'http') {
        expectedHTTPFailures.push(failureURL);
        return await json(route, { error: result.error ?? 'Fixture detail read failed' }, 503);
      }
      const headers = { ...result.headers, ...(held.decodeID === null ? {} : { 'X-Fixture-Decode': held.decodeID }) };
      if (result.failure === 'json') return await route.fulfill({ contentType: 'application/json', body: '{invalid json', headers });
      const status = result.status || 200;
      if (status >= 400) expectedHTTPFailures.push(failureURL);
      // Playwright's WebKit backend rejects every 3xx status, including 304.
      if (status === 304) headers['X-Fixture-Status'] = '304';
      return await route.fulfill({ status: status === 304 ? 200 : status, contentType: 'application/json', headers,
        body: status === 304 || status === 204 ? '' : JSON.stringify(result.value ?? snapshot) });
    } finally { held.signalDone(); }
  }
  async function releaseReads() {
    for (const held of heldReads) { held.release(); held.releaseBody(); }
    await Promise.all(heldReads.filter(held => held.claimed).map(held => held.done));
  }

  async function api(route, url) {
    const request = route.request(), method = request.method(), path = url.pathname;
    requests.push(`${method} ${path}`);
    const allowedQuery = /\/reactions(?:\/[^/]+)?$/.test(path) ? ['actor']
      : path === '/api/stats' ? ['range', 'project', 'pipeline']
      : /\/runs$/.test(path) ? ['offset', 'limit']
      : /\/runs\/\d+\/log$/.test(path) ? ['raw']
      : /^\/api\/assistant\/threads\/[^/]+\/activity$/.test(path) ? ['after']
      : /\/logs$/.test(path) ? ['stage', 'run', 'stage_only']
      : /\/activity$/.test(path) ? ['stage', 'run', 'offset', 'limit', 'after', 'log_tail_bytes'] : [];
    assert.ok([...url.searchParams.keys()].every(k => allowedQuery.includes(k)), `Unexpected query: ${url}`);
    if (behavior.authRequired && method === 'GET' && ['/api/config', '/api/tickets', '/api/assistant'].includes(path)) {
      expectedHTTPFailures.push(base + path);
      return json(route, { error: 'Fixture authentication required' }, 401);
    }
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
      // Explicit authoritative parser cases, not a permissive catch-all. Unknown
      // inputs still fail strict interception rather than silently validating.
      const rejected = new Map([
        ['0 9 * *', 'cron takes 5 fields'],
        ['61 9 * * *', 'minute out of range'],
        ['0 9 31 2 *', 'schedule has no occurrences'],
        ['not a schedule', 'invalid schedule'],
      ]);
      if (rejected.has(schedule.schedule)) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: rejected.get(schedule.schedule) }, 400);
      }
      assert.ok(new Set(['0 9 * * 1-5', '0 * * * *', '0 */6 * * *', '0 9 * * *',
        '0 3 * * *', '0 10 * * 1', '30m', '1.5h', '0.5h']).has(schedule.schedule),
      `Unmodelled authoritative preview schedule: ${schedule.schedule}`);
      return json(route, { occurrences: fires, human: schedule.schedule === '1.5h' ? 'Every 1h30m' : 'Every weekday at 09:00' });
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
      if (method === 'GET' && !action) {
        if (behavior.automationReadFailure) {
          expectedHTTPFailures.push(base + path);
          return json(route, { error: behavior.automationReadFailure }, 500);
        }
        return json(route, definition);
      }
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
          if (behavior.configRefreshWait) await behavior.configRefreshWait;
          return readResponse(route, path, config);
        case '/api/ticket-sources':
          if (behavior.sourcesFailure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: 'Sources offline' }, 503);
          }
          return json(route, [
            { id: 'primary', path: '/fixtures/primary', available: true },
            { id: source, path: sourcePath, available: true },
            ...(options.secondSecondary ? [{ id: secondSource, path: secondSourcePath, available: true }] : []),
            ...(behavior.unavailableFolder ? [{ id: 'unavailable', path: unavailablePath, available: false, error: 'Fixture folder unavailable' }] : []),
          ]);
        case '/api/tickets': return readResponse(route, path, { tickets: list('primary'), running_agents: list('primary').filter(active).length });
        case `/api/sources/${source}/tickets`: return readResponse(route, path, list(source));
        case `/api/sources/${secondSource}/tickets`:
          assert.ok(options.secondSecondary, 'Second secondary source must be explicitly enabled');
          return readResponse(route, path, list(secondSource));
        case '/api/assistant': return readResponse(route, path, behavior.assistantConfig);
        case '/api/assistant/threads': return json(route, { threads: [...assistantThreads.values()].map(({ messages, ...thread }) => thread) });
        case '/api/assistant/companion':
          if (behavior.companionWait) await behavior.companionWait;
          if (behavior.companionFailure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: behavior.companionFailure }, 503);
          }
          return json(route, behavior.companionSnapshot);
        case '/api/events': return route.fulfill({ contentType: 'text/event-stream', body: 'retry: 3600000\n\n: offline fixture\n\n' });
        case '/api/config/raw':
          if (behavior.rawReadWait) await behavior.rawReadWait;
          if (behavior.rawConfigUnavailable) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: 'No config file' }, 501);
          }
          return json(route, { content: rawConfig });
        case '/api/stats': return readResponse(route, path, options.populatedStats ? populatedStats(url.searchParams.get('range') || '90d')
          : { totals: { done: 1, runs: 2, first_pass_pct: 50 }, live: {}, days: [], buckets: [], stages: [], agents: [], projects: [] });
        case '/api/tickets/archived': return json(route, { tickets: [records.get('primary/kon-archived')] });
      }
    }
    if (method === 'PUT' && path === '/api/config/theme') {
      assert.match(request.headers()['content-type'] || '', /^application\/json(?:;|$)/i);
      const body = request.postDataJSON();
      assert.deepEqual(Object.keys(body), ['theme']);
      assert.ok(themeModes.has(body.theme), `Unexpected theme preference: ${body.theme}`);
      mutations.push({ method, path, body });
      if (behavior.themeWait) await behavior.themeWait;
      const failure = behavior.themeFailure;
      if ((!failure || failure.written) && !behavior.rawConfigUnavailable) {
        const doc = yaml.parseDocument(rawConfig);
        if (!doc.get('web')) doc.set('web', doc.createNode({}));
        doc.setIn(['web', 'theme'], body.theme);
        rawConfig = String(doc);
      }
      if (failure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: failure.error, code: failure.code }, failure.status);
      }
      config.theme = body.theme;
      config.revision++;
      return route.fulfill({ status: 204, headers: {
        'X-Kontora-Theme': config.theme, 'X-Kontora-Config-Revision': String(config.revision),
      } });
    }
    if (method === 'PUT' && path === '/api/config/raw') {
      const body = request.postDataJSON();
      assert.deepEqual(Object.keys(body).sort(), ['content', 'expected_content']);
      assert.equal(typeof body.content, 'string');
      assert.equal(typeof body.expected_content, 'string');
      mutations.push({ method, path, body });
      if (behavior.saveWait) await behavior.saveWait;
      if (behavior.conflictOnSave || body.expected_content !== rawConfig) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: 'Configuration changed before conditional write' }, 409);
      }
      if (behavior.saveFailure) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: behavior.saveFailure }, 503);
      }
      const response = behavior.saveResponse;
      if (!response || response.status === 204 || response.body?.code === 'config_saved_reload_failed') {
        rawConfig = body.content;
        configWrites.push(rawConfig);
        if (options.settingsNotificationReload) config.default_notify = yaml.parse(rawConfig).notifications?.default_statuses;
      }
      if (response && response.status !== 204) {
        expectedHTTPFailures.push(base + path);
        return json(route, response.body, response.status);
      }
      return route.fulfill({ status: 204 });
    }
    if (method === 'POST' && path === '/api/tickets/upload') {
      assert.equal(request.headers()['x-kontora-confirm'], 'upload-tickets');
      const contentType = request.headers()['content-type'];
      assert.match(contentType, /^multipart\/form-data; boundary=/);
      const form = await new Response(request.postDataBuffer(), { headers: { 'Content-Type': contentType } }).formData();
      const files = [];
      for (const [field, file] of form) {
        assert.equal(field, 'files'); assert.equal(typeof file, 'object'); assert.match(file.name, /\.md$/i);
        files.push({ name: file.name, type: file.type, content: await file.text() });
      }
      assert.ok(files.length, 'Upload must contain Markdown files');
      uploads.push({ path, files });
      const response = structuredClone(behavior.uploadResponse);
      if (behavior.uploadWait) await behavior.uploadWait;
      if (response?.failure === 'network') { expectedHTTPFailures.push(base + path); return route.abort('failed'); }
      if (response) {
        if (response.status >= 400) expectedHTTPFailures.push(base + path);
        return route.fulfill(response);
      }
      // Deliberately no invented ticket import: callers provide the success
      // receipt (and records) they need. Failed requests never mutate records.
      expectedHTTPFailures.push(base + path);
      return json(route, { tickets: [], errors: files.map(file => ({ file: file.name, error: 'Fixture import not configured' })) }, 400);
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
      if (body.parent) {
        assert.equal(records.get(key('primary', body.parent))?.kind, 'epic');
        created.parent = { id: body.parent };
      }
      put(created);
      if (created.status === 'todo') enqueued.push({ id: created.id, action: 'create' });
      mutations.push({ method, path, body, ticket: structuredClone(created) });
      return json(route, created, 201);
    }
    if (method === 'POST' && path === '/api/assistant/threads') {
      const body = request.postDataJSON();
      assert.deepEqual(Object.keys(body), ['autonomy']); assert.ok(['read', 'ask', 'auto'].includes(body.autonomy));
      const captured = structuredClone(body), failure = structuredClone(behavior.assistantCreateFailure);
      if (behavior.assistantCreateWait) await behavior.assistantCreateWait;
      if (failure) { expectedHTTPFailures.push(base + path); return json(route, { error: failure.error }, failure.status); }
      const id = 'c' + String(++createdThreads).padStart(15, '0');
      assert.ok(!assistantThreads.has(id), 'Fixture thread identities must be unique');
      const thread = { id, title: 'New fixture chat', autonomy: captured.autonomy, turns: 0, writes: 0,
        running: false, messages: [], events: [], created_at: stamp, updated_at: stamp };
      assistantThreads.set(id, thread); mutations.push({ method, path, body: captured });
      return json(route, thread, 201);
    }
    const gateMatch = path.match(/^\/api\/assistant\/gate\/([^/]+)$/);
    if (gateMatch && method === 'POST') {
      const thread = [...assistantThreads.values()].find(t => t.gate?.id === decodeURIComponent(gateMatch[1]));
      assert.ok(thread, 'Approval must name a recorded pending gate');
      const body = request.postDataJSON();
      assert.deepEqual(Object.keys(body), ['decision']);
      assert.ok(['approve', 'skip'].includes(body.decision));
      if (behavior.assistantGateWait) await behavior.assistantGateWait;
      delete thread.gate;
      mutations.push({ method, path, body });
      return route.fulfill({ status: 204 });
    }
    const assistantMatch = path.match(/^\/api\/assistant\/threads\/([^/]+)(?:\/(activity|stream|stop|messages))?$/);
    if (assistantMatch) {
      const id = decodeURIComponent(assistantMatch[1]), action = assistantMatch[2];
      const thread = assistantThreads.get(id);
      if (behavior.assistantMissingThreads.has(id)) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: 'Assistant thread not found' }, 404);
      }
      assert.ok(thread, `Unknown assistant thread: ${id}`);
      if (method === 'GET' && !action) return readResponse(route, path, thread);
      if (method === 'DELETE' && !action) {
        assert.equal(request.postData(), null);
        assistantThreads.delete(id);
        mutations.push({ method, path });
        return route.fulfill({ status: 204 });
      }
      if (method === 'POST' && action === 'messages') {
        const body = request.postDataJSON();
        assert.ok(body.text?.trim());
        assert.ok(['read', 'ask', 'auto'].includes(body.autonomy));
        assert.equal(typeof body.context, 'string');
        if (behavior.assistantMessageFailure) {
          expectedHTTPFailures.push(base + path);
          return json(route, { error: behavior.assistantMessageFailure }, 503);
        }
        thread.messages.push({ n: thread.messages.length + 1, text: body.text, at: stamp });
        thread.turns++;
        mutations.push({ method, path, body });
        return route.fulfill({ status: 204 });
      }
      if (method === 'GET' && action === 'activity') {
        const after = url.searchParams.get('after');
        assert.ok(after !== null && /^\d+$/.test(after), 'Assistant activity requires a numeric cursor');
        return readResponse(route, path, { running: thread.running, autonomy: thread.autonomy, messages: thread.messages,
          evidence: thread.evidence, gate: thread.gate, offset: 0,
          tape: { events: thread.events || [{ kind: 'text', text: `Recorded reply for ${thread.title}.` }] },
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
    // Membership is stored on the child; ordering writes only the epic. Like
    // SetChildOrder, retain existing nonmembers, reject unresolved references,
    // and never infer membership from child_order. Held replies capture owners.
    const epicMatch = path.match(/^\/api\/tickets\/([^/]+)\/(parent|unparent|children)$/);
    if (epicMatch) {
      const record = records.get(key('primary', decodeURIComponent(epicMatch[1])));
      assert.ok(record, 'Epic operation requires an existing primary ticket');
      const action = epicMatch[2], body = request.postData() ? request.postDataJSON() : undefined;
      assert.equal(method, action === 'children' ? 'PUT' : 'POST');
      const failure = behavior.epicFailure;
      if (behavior.epicWait) await behavior.epicWait;
      const refuse = error => {
        expectedHTTPFailures.push(base + path);
        return json(route, { error }, 400);
      };
      if (failure) return refuse(failure);
      const before = structuredClone(record);
      if (action === 'children') {
        assert.deepEqual(Object.keys(body), ['children']);
        assert.equal(record.kind, 'epic');
        assert.ok(Array.isArray(body.children));
        if (body.children.some(id => id === record.id || !records.has(key('primary', id)))) return refuse('Unknown or invalid retained order reference');
        record.child_order = [...new Set(body.children)];
      } else if (action === 'parent') {
        assert.deepEqual(Object.keys(body), ['parent']);
        const parent = records.get(key('primary', body.parent));
        if (record.kind === 'epic' || parent?.kind !== 'epic') return refuse('Epics cannot nest; parent must be an epic');
        record.parent = { id: parent.id, title: parent.title, status: parent.status, kind: 'epic' };
      } else {
        assert.equal(body, undefined, 'Unparent takes no body');
        delete record.parent;
      }
      mutations.push({ method, path, body, before, ticket: structuredClone(record) });
      // Relation endpoints acknowledge identity, not a replacement ticket.
      return json(route, { id: record.id });
    }
    const noteMatch = path.match(/^\/api(?:\/sources\/([^/]+))?\/tickets\/([^/]+)\/(note|notes\/([^/]+)(?:\/reactions(?:\/([^/]+))?)?)$/);
    if (noteMatch) {
      const folder = noteMatch[1] ? decodeURIComponent(noteMatch[1]) : 'primary';
      const record = records.get(key(folder, decodeURIComponent(noteMatch[2])));
      assert.ok(record, 'Notes require an existing source-qualified ticket');
      const body = request.postData() ? request.postDataJSON() : {};
      if (behavior.noteDispatchWait) await behavior.noteDispatchWait;
      const failure = behavior.noteFailure && (!behavior.noteFailure.path || behavior.noteFailure.path === path) ? behavior.noteFailure : null;
      if (failure || record.can_edit === false) {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: failure?.error || 'Fixture ticket is read-only' }, failure?.status || 403);
      }
      const notes = record.notes ||= [];
      const resolveNote = ref => notes.find(n => n.id === ref);
      const mint = note => {
        if (note.id.startsWith('#')) {
          const oldID = note.id;
          do { note.id = `minted${++mintedNotes}fx`; } while (notes.some(n => n !== note && n.id === note.id));
          for (const reply of notes) if (reply.parent_id === oldID) reply.parent_id = note.id;
        }
        return note.id;
      };
      const note = resolveNote(decodeURIComponent(noteMatch[4] || ''));
      if (noteMatch[3] === 'note' && method === 'POST') {
        assert.ok(body.text?.trim());
        const parent = body.parent ? resolveNote(body.parent) : null;
        if (body.parent) { assert.ok(parent); mint(parent); }
        let id; do { id = `n${++mintedNotes}fx`; } while (notes.some(n => n.id === id));
        notes.push({ id, author: config.author || 'Browser author', author_kind: 'human',
          at: stamp, text: body.text, ...(parent ? { parent_id: parent.id } : {}), reactions: [] });
      } else {
        assert.ok(note, 'Conversation operation requires an existing note');
        if (noteMatch[3].includes('/reactions') || method === 'PATCH') mint(note);
        if (noteMatch[3].includes('/reactions')) {
          const emoji = noteMatch[5] ? decodeURIComponent(noteMatch[5]) : body.emoji;
          const actor = body.actor || url.searchParams.get('actor') || config.author || 'Browser author';
          assert.ok(emoji);
          const reactions = note.reactions ||= [];
          const previous = reactions.find(r => r.emoji === emoji);
          if (method === 'POST') {
            if (!previous) reactions.push({ emoji, actors: [actor] });
            else if (!previous.actors.includes(actor)) previous.actors.push(actor);
          } else {
            assert.equal(method, 'DELETE');
            if (previous) previous.actors = previous.actors.filter(a => a !== actor);
            note.reactions = reactions.filter(r => r.actors.length);
          }
        } else if (method === 'PATCH') {
          assert.equal(note.author, config.author || 'Browser author');
          assert.ok(body.text?.trim()); note.text = body.text; note.edited = true;
        } else {
          assert.equal(method, 'DELETE');
          assert.equal(note.author, config.author || 'Browser author');
          record.notes = notes.filter(n => n !== note && n.parent_id !== note.id);
        }
      }
      // ParseNotes exposes legacy references by their position in the whole
      // accepted note list. Deletion can therefore change #N without a mint.
      record.notes.forEach((note, index) => { if (note.id.startsWith('#')) note.id = `#${index}`; });
      // Notes update the protected tail, never the description. Retain its audit suffix.
      const audit = record.description_tail.slice(record.description_tail.indexOf('## Audit'));
      record.description_tail = '\n## Notes\n\n' + record.notes.map(n => `**${n.at} · ${n.author} · ${n.id}**\n\n${n.text}`).join('\n\n') + '\n\n' + audit;
      record.body = record.description + record.description_tail;
      mutations.push({ method, path, body, ticket: structuredClone(record) });
      const snapshot = structuredClone(record);
      if (behavior.noteWait) await behavior.noteWait;
      if (behavior.noteLoseResponse && (!behavior.noteLoseResponse.path || behavior.noteLoseResponse.path === path)) {
        expectedHTTPFailures.push(base + path); return route.abort('failed');
      }
      return json(route, snapshot);
    }
    const match = path.match(/^\/api(?:\/sources\/([^/]+))?\/tickets\/([^/]+)(?:\/(cost|chain|changes|logs|activity|move|pause|retry|init|run|schedule|set-stage))?$/);
    if (match) {
      const folder = match[1] ? decodeURIComponent(match[1]) : 'primary';
      const id = decodeURIComponent(match[2]), action = match[3];
      const record = records.get(key(folder, id));
      if (folder === 'primary' && id === 'kon-deleted' && !action && method === 'GET') {
        expectedHTTPFailures.push(base + path);
        return json(route, { error: 'Ticket not found' }, 404);
      }
      assert.ok(record, `Unknown source-qualified ticket: ${folder}/${id}`);
      if (method === 'GET' && !action) {
        const detail = record.archived_at ? { ...record, status: 'archived', archived_from: record.status } : { ...record };
        if (record.source === 'primary' && record.status === 'open' && record.kontora && !record.scheduled_at) {
          detail.run_preview ??= { stages: (record.stages || []).slice(Math.max(0, record.stages?.indexOf(record.stage) ?? 0)),
            agent: record.agent, branch: record.branch || record.auto_branch, repo: record.path };
        }
        return readResponse(route, path, detail);
      }
      if (method === 'PUT' && !action) {
        const body = request.postDataJSON();
        assert.ok(!('body' in body), 'Description edits must not replace the protected tail');
        if (behavior.editWait) await behavior.editWait;
        if (record.can_edit === false) {
          expectedHTTPFailures.push(base + path);
          return json(route, { error: 'Fixture ticket is read-only' }, 403);
        }
        if ('description' in body) {
          descriptionWrites.push({ method, path, body });
          const failure = behavior.descriptionFailure;
          if (behavior.descriptionWait) await behavior.descriptionWait;
          if (failure) {
            expectedHTTPFailures.push(base + path);
            return json(route, { error: failure }, 503);
          }
        }
        Object.assign(record, body);
        record.body = record.description + record.description_tail;
        mutations.push({ method, path, body, ticket: structuredClone(record) });
        return json(route, record);
      }
      assert.equal(folder, 'primary', 'Secondary tickets cannot use control/output endpoints');
      if (method === 'GET') {
        switch (action) {
          case 'cost': if (options.costs?.[id]) return readResponse(route, path, options.costs[id]);
            if (id === 'kon-live') return json(route, { id, cost_usd: '0.42', tracked_runs: 3, priced_runs: 2,
            stages: [{ name: 'plan', cost_usd: '0.30', tracked_runs: 1, priced_runs: 1 },
              { name: 'code', cost_usd: '0.12', tracked_runs: 2, priced_runs: 1 }], runs: [] });
            return json(route, { id, cost_usd: record.history.length || active(record) ? '0.12' : null,
            tracked_runs: record.history.length || (active(record) ? 1 : 0),
            priced_runs: record.history.length || (active(record) ? 1 : 0), stages: [], runs: [] });
          case 'chain': return json(route, options.chains?.[id] || { id, nodes: [], verdict: 'ready', total: 0, done: 0, position: 0, path_length: 0, goal: '', cycle: [] });
          case 'changes': {
            const failure = behavior.changesFailure;
            if (behavior.changesWait) await behavior.changesWait;
            if (failure) {
              expectedHTTPFailures.push(base + path);
              return json(route, { error: failure }, 503);
            }
            return readResponse(route, path, options.changes?.[id] || { base: 'main', branch: record.branch || '', files: [], commits: [] });
          }
          case 'logs': {
            if (!options.distinctRaw) return readResponse(route, path, { content: `${id} ${url.searchParams.get('stage') || 'code'} fixture log revision ${behavior.outputRevision}\n` });
            const stage = url.searchParams.get('stage');
            const runText = url.searchParams.get('run');
            const stageOnly = url.searchParams.get('stage_only') === 'true';
            assert.ok(stage, 'Distinct raw fixtures require a stage');
            assert.ok(runText === null || /^(0|[1-9]\d*)$/.test(runText), 'Raw run must be a nonnegative occurrence');
            const occurrences = (record.history || []).filter(h => h.stage === stage);
            const latest = active(record) && record.stage === stage ? occurrences.length : occurrences.length - 1;
            const run = runText === null ? latest : Number(runText), outputKey = `${id}/${stage}/${run}`;
            const failure = behavior.rawFailures.get(outputKey);
            artifactReads.push({ id, stage, run, raw: true, stageOnly });
            if (failure || occurrences[run]?.imported || !rawOutputs.has(outputKey)) {
              expectedHTTPFailures.push(url.href);
              return json(route, { error: failure?.error || 'Exact raw run output unavailable' }, failure?.status || 404);
            }
            return readResponse(route, path, { content: rawOutputs.get(outputKey) });
          }
          case 'activity': {
            const stage = url.searchParams.get('stage'), run = Number(url.searchParams.get('run') || 0);
            artifactReads.push({ id, stage, run });
            const occurrences = (record.history || []).filter(h => h.stage === stage);
            if (occurrences[run]?.imported) throw new Error(`Imported history has no local artifact: ${id}/${stage}/${run}`);
            return readResponse(route, path, { live: active(record), source: 'events',
              tape: { events: options.artifacts?.[`${id}/${stage}/${run}`] || behavior.activityEvents || [{ kind: 'text', text: `Fixture activity revision ${behavior.outputRevision}` }] } });
          }
        }
      }
      if (method === 'POST' && ['move', 'pause', 'retry', 'init', 'run', 'schedule', 'set-stage'].includes(action)) {
        const body = request.postData() ? request.postDataJSON() : {};
        if (behavior.lifecycleWait) await behavior.lifecycleWait;
        const refuse = (message, status = 400) => {
          expectedHTTPFailures.push(base + path);
          return json(route, { error: message }, status);
        };
        if (behavior.lifecycleRefusals.has(path)) return refuse(behavior.lifecycleRefusals.get(path), 409);
        if (behavior.lifecycleFailure) return refuse(behavior.lifecycleFailure);
        if (record.archived_at || record.status === 'archived') return refuse('Ticket is archived');
        if (record.kind === 'epic' && record.epic_mode === 'passive') return refuse('Passive epic status is derived');
        if (!record.can_control) return refuse('Ticket is not controllable');
        const before = structuredClone(record);
        if (action === 'schedule') {
          if (active(record)) return refuse('Cannot schedule a running ticket', 409);
          if (body.clear === true) {
            assert.deepEqual(Object.keys(body), ['clear']);
            delete record.scheduled_at;
          } else {
            assert.deepEqual(Object.keys(body), ['scheduled_at']);
            const at = Date.parse(body.scheduled_at);
            if (typeof body.scheduled_at !== 'string' ||
                !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(body.scheduled_at) || !Number.isFinite(at)) {
              return refuse('scheduled_at must be an RFC 3339 timestamp');
            }
            if (at < (options.scheduleNow ?? Date.now()) - 60_000) return refuse('scheduled_at is in the past');
            if (!record.kontora || record.kind === 'epic' || !['open', 'todo'].includes(record.status)) {
              return refuse('Schedule requires an initialized open or todo ticket', 409);
            }
            record.scheduled_at = new Date(at).toISOString().replace(/\.\d{3}Z$/, 'Z');
            record.status = 'open';
          }
        } else if (action === 'set-stage') {
          assert.deepEqual(Object.keys(body), ['stage']);
          if (active(record) || !record.stages?.includes(body.stage)) return refuse('Invalid stage change');
          record.stage = body.stage;
          record.attempt = 0;
        } else if (action === 'move') {
          assert.deepEqual(Object.keys(body), ['status'], 'A move is not an execution settings update');
          if (!['open', 'todo', 'paused', 'human_review', 'done', 'cancelled', ...config.custom_statuses].includes(body.status)) return refuse('Invalid status');
          if (record.status === body.status) return refuse('Ticket already has that status');
          if (body.status === 'paused' && ['done', 'cancelled'].includes(record.status)) return refuse('Use retry to reopen');
          record.status = body.status;
          record.kontora = true;
          record.last_error = '';
          delete record.annotation_return_status;
          if (body.status !== 'open') delete record.scheduled_at;
          if (body.status === 'done') record.completed_at = stamp;
          // SetStatus cancels/reconciles, but never enqueues or resets stage/attempt.
        } else if (action === 'pause') {
          if (!active(record) && record.status !== 'todo') return refuse('Ticket is not running');
          record.status = 'paused'; delete record.scheduled_at;
        } else {
          if (active(record)) return refuse('Ticket is already running');
          if (action === 'retry' && (!record.kontora || record.status === 'todo')) return refuse('Cannot retry this status');
          if (action === 'run' && (!record.kontora || !['open', 'todo'].includes(record.status))) return refuse('Run requires an initialized open or todo ticket');
          if (action === 'run') {
            assert.deepEqual(body, {}, 'Run uses saved ticket settings');
            Object.assign(record, { status: 'todo', last_error: '' });
          } else {
            Object.assign(record, body, { status: body.status || 'todo', attempt: 0, last_error: '' });
          }
          delete record.scheduled_at;
          pickupRequests.push({ id, action });
          if (record.status === 'todo' && !behavior.pickupBlocked?.has(id)) enqueued.push({ id, action });
        }
        record.execution_active = false; // Mutations park/queue; only a later daemon claim makes execution active.
        const snapshot = structuredClone(record);
        mutations.push({ method, path, body, before, ticket: snapshot });
        const failedAck = behavior.lifecyclePostWriteFailures.get(path);
        behavior.lifecyclePostWriteFailures.delete(path);
        if (behavior.lifecycleAckWait) await behavior.lifecycleAckWait;
        if (failedAck) {
          if (failedAck.failure !== 'network') return refuse(failedAck.error ?? 'Lifecycle response failed after write', 503);
          expectedHTTPFailures.push(base + path);
          return route.abort('failed');
        }
        return json(route, snapshot);
      }
    }
    throw new Error(`Unmodelled API request: ${method} ${url}`);
  }

  async function themedDocument(route, url) {
    assert.ok(themeModes.has(config.theme), `Unexpected fixture theme: ${config.theme}`);
    const response = await route.fetch({ url, maxRedirects: 0 });
    assert.equal(response.status(), 200, `Could not read embedded document: ${url}`);
    const body = await response.text();
    const marker = /(<html\b[^>]*\bdata-theme-preference=")(system|light|dark)(")/;
    assert.match(body, marker, 'Embedded document must declare a theme preference on <html>');
    const headers = response.headers();
    for (const header of ['content-encoding', 'content-length', 'etag']) delete headers[header];
    return route.fulfill({ response, headers, body: body.replace(marker, (_, prefix, _mode, suffix) => prefix + config.theme + suffix) });
  }

  async function install(context) {
    // Delay JSON decoding, not arbitrary app state. Headers and wire bytes have
    // already arrived; tests release this second boundary independently.
    await context.exposeBinding('__fixtureDecode', async (_source, id) => {
      const held = heldReads.find(read => read.decodeID === id);
      assert.ok(held, `Unknown decode gate: ${id}`);
      held.signalDecoding(); await held.body.promise;
    });
    await context.addInitScript(materializeMultipart => {
      const nativeFetch = window.fetch;
      window.fetch = async (...args) => {
        const init = args[1];
        if (materializeMultipart && init?.body instanceof FormData) {
          // WebKit omits file parts from intercepted FormData bodies.
          const body = new Response(init.body);
          const headers = new Headers(init.headers);
          if (!headers.has('Content-Type')) headers.set('Content-Type', body.headers.get('Content-Type'));
          args[1] = { ...init, headers, body: await body.arrayBuffer() };
        }
        let response = await nativeFetch(...args);
        if (response.headers.get('X-Fixture-Status') === '304') {
          const headers = new Headers(response.headers);
          headers.delete('X-Fixture-Status');
          response = new Response(null, { status: 304, headers });
        }
        const id = response.headers.get('X-Fixture-Decode');
        if (id !== null) {
          const json = response.json.bind(response);
          response.json = async () => { await window.__fixtureDecode(id); return json(); };
        }
        return response;
      };
    }, engine === 'webkit');
    // Install BOTH interception layers before creating/navigating the page.
    // No catch-all API success, external fallthrough, service worker or live WS.
    await context.route('**/*', async route => {
      const request = route.request(), url = new URL(request.url());
      try {
        assert.equal(url.origin, base, `External request: ${url}`);
        if (url.protocol === 'blob:') {
          // WebKit intercepts Raw log popups even though Blob URLs never reach the server.
          assert.equal(request.method(), 'GET');
          assert.ok(request.isNavigationRequest(), `Unexpected Blob request: ${url}`);
          return await route.continue();
        }
        if (url.pathname.startsWith('/api/')) return await api(route, url);
        if (url.pathname === '/' && url.searchParams.has('token')) {
          assert.equal(request.method(), 'GET'); assert.ok(request.isNavigationRequest());
          assert.deepEqual([...url.searchParams.keys()], ['token']);
          const token = url.searchParams.get('token'), accepted = token === behavior.authToken;
          authNavigations.push({ token, accepted });
          if (accepted) {
            behavior.authRequired = false;
            // WebKit cannot fulfill redirects. The test server sends the 303.
            return await route.continue();
          }
          // The real server still serves index.html for a rejected token. Fetch
          // only that fixed embedded asset, never the token-bearing URL.
          return await themedDocument(route, base + '/');
        }
        assert.equal(request.method(), 'GET');
        // index.html names its scripts and stylesheets with a ?v= content hash.
        assert.ok(assets.has(url.pathname) && (!url.search || /^\?v=[0-9a-f]+$/.test(url.search)), `Unknown asset: ${url}`);
        if (url.pathname === '/' || url.pathname === '/index.html') return await themedDocument(route, url.href);
        if (url.pathname === '/vendor/xterm@5.5.0/xterm.css') {
          stylesheetRequests.push(url.href);
          if (behavior.stylesheetFailures > 0) {
            behavior.stylesheetFailures--; expectedHTTPFailures.push(url.href);
            return await route.fulfill({ status: 503, contentType: 'text/css', body: '' });
          }
        }
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
        assert.ok([...records.values()].some(record => record.source === 'primary' && active(record) &&
          url.pathname === '/ws/terminal/' + encodeURIComponent(record.id))
          || /^\/ws\/automations\/[^/]+\/\d+$/.test(url.pathname), `Unknown socket: ${url}`);
        const paneStream = !url.pathname.startsWith('/ws/automations/');
        if (!paneStream) {
          const [, , , id, n] = url.pathname.split('/');
          const row = definitions.get(id); assert.ok(row?.running); assert.equal(Number(n), row.current_run.n);
        }
        assert.ok([...url.searchParams.keys()].every(k => ['cols', 'rows', 'rw', 'stream'].includes(k)));
        assert.equal(url.searchParams.get('stream'), paneStream ? 'pane' : null);
        assert.ok(Number(url.searchParams.get('cols')) > 0 && Number(url.searchParams.get('rows')) > 0);
        const socketIndex = sockets.length;
        sockets.push(url);
        socketRoutes.push(ws);
        // Do not call connectToServer(): there is no terminal server at all.
        const ticketID = paneStream ? decodeURIComponent(url.pathname.split('/').at(-1)) : null;
        const snapshots = options.terminalSnapshots;
        const transcript = ticketID !== null && snapshots && Object.hasOwn(snapshots, ticketID) ? snapshots[ticketID] : undefined;
        const snapshot = (cols, rows) => {
          if (paneStream) ws.send(JSON.stringify({ type: 'reset', cols, rows }));
          const marker = terminalMarkers.get(ws);
          const content = transcript ?? ('Offline fixture terminal\r\n$ ' + (behavior.bracketedPaste ? '\x1b[?2004h' : ''));
          ws.send(Buffer.from(content + (marker ? `\x1b[${rows};1H${marker}` : '')));
        };
        ws.onMessage(message => {
          try {
            const data = JSON.parse(String(message));
            assert.ok(['resize', 'input'].includes(data.type));
            messages.push({ url, ...data });
            if (data.type === 'resize') {
              assert.ok(Number.isInteger(data.cols) && data.cols > 0 && Number.isInteger(data.rows) && data.rows > 0);
              if (behavior.holdResizes) pendingResizes.push({ url, ...data, reset: () => snapshot(data.cols, data.rows) });
              else snapshot(data.cols, data.rows);
            } else {
              assert.equal(typeof data.data, 'string');
              assert.equal(url.searchParams.get('rw'), '1', 'Input requires a writable fixture stream');
              // Reconnects can reuse the URL; index identifies the actual route that received input.
              terminalInputs.push({ socketIndex, url, data: data.data });
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
  return { install, api, config, configWrites, records, definitions, runs, receipts, runReceipts, assistantThreads, requests, mutations, sockets, socketRoutes, messages, violations, behavior, expectedHTTPFailures, releaseConfig, descriptionWrites, pendingResizes, terminalMarkers,
    holdSnapshot, holdDetail, holdRead, pendingSnapshot, releaseReads, pickupRequests, enqueued, artifactReads, terminalInputs,
    uploads, stylesheetRequests, authNavigations, rawOutputs,
    get rawConfig() { return rawConfig; }, set rawConfig(content) { rawConfig = content; } };
}

export async function unloadFixtureDocuments(context) {
  // Unload pages and popups while routing still intercepts their final polls.
  const results = await Promise.allSettled(context.pages().filter(page => !page.isClosed()).map(page => page.goto('about:blank')));
  const errors = results.filter(result => result.status === 'rejected').map(result => result.reason);
  if (errors.length) throw new AggregateError(errors, 'Could not unload fixture documents');
}

export function fixtureSession(context, environment, options = {}) {
  const fixture = fixtures(environment, options), errors = [];
  const { base } = environment;

  async function preparePage() {
    await fixture.install(context);
    if (options.noNavigation) await context.addInitScript(() => {
      Object.defineProperty(window, 'navigation', { configurable: true, value: undefined });
    });
    if (options.nativeAgentPicker) await context.addInitScript(() => {
      Object.defineProperty(HTMLElement.prototype, 'showPopover', { configurable: true, value: undefined });
    });
    await context.addInitScript(({ sidebar, savedThread, assistantOpen, base }) => {
      // Teardown navigates to an opaque about:blank document with no storage.
      if (location.origin !== base) return;
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
    }, { sidebar: options.sidebar, savedThread: options.savedThread, assistantOpen: options.assistantOpen, base });
    const page = await context.newPage();
    page.setDefaultTimeout(4000);
    page.setDefaultNavigationTimeout(15000);
    page.on('pageerror', error => errors.push(error.stack || error.message));
    page.on('console', msg => {
      if (msg.type() !== 'error') return;
      if (msg.text().startsWith('Failed to load resource:') && fixture.expectedHTTPFailures.includes(msg.location().url)) return;
      errors.push(msg.text());
    });
    return page;
  }

  async function navigate(page) {
    await options.beforeNavigation?.(page, fixture);
    await page.goto(base + (options.hash || ''), { waitUntil: 'domcontentloaded' });
    await options.beforeReady?.(page, fixture);
    await ready(page);
  }

  function check() {
    assert.deepEqual(fixture.violations, [], 'Unexpected requests (all must be explicitly modelled)');
    assert.deepEqual(errors, [], 'Browser/Alpine/console errors');
  }

  async function unload() {
    fixture.releaseConfig();
    try {
      await fixture.releaseReads();
    } finally {
      await unloadFixtureDocuments(context);
    }
    check();
  }

  return { fixture, preparePage, navigate, check, unload };
}

export async function ready(page) {
  await page.waitForFunction(() => {
    if (!window.Alpine) return false;
    const app = Alpine.$data(document.body);
    return !app.loading && app._applyingRoute === null && !app.detailLoading && !app.settingsLoading && !app.automationsLoading && !app.archiveDetailLoading;
  });
  await page.evaluate(() => document.fonts.ready);
}
export async function routeTo(page, route) {
  await ready(page);
  await page.evaluate(route => { location.hash = route; }, route);
  if (route === '#/settings') route = '#/settings/general';
  await page.waitForFunction(route => {
    const app = Alpine.$data(document.body);
    return location.hash === route && app.routeHash() === route && app._applyingRoute === null;
  }, route);
  await ready(page);
}
export async function openTicket(page, id, folder = 'primary') {
  await routeTo(page, folder === 'primary' ? `#/t/${id}` : `#/s/${folder}/t/${id}`);
  await page.waitForFunction(({ id, folder }) => {
    const app = Alpine.$data(document.body);
    return app.selectedTicket?.id === id && (app.selectedTicket.source || 'primary') === folder && !app.detailLoading;
  }, { id, folder });
}
