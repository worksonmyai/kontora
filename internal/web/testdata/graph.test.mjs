import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraActivity } from '../ui/activity.js';
import { kontoraBoard } from '../ui/board.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraMobile } from '../ui/mobile.js';
import { settingsFixture, settingsState } from './settings_fixtures.mjs';
import { parse } from '../static/vendor/yaml@2.8.1/yaml.mjs';

const definition = {
  version: 2, entry: 'work',
  nodes: [{ id: 'unused', type: 'agent', template: 'code' }, { id: 'work', type: 'agent', template: 'code' }, { id: 'again', type: 'loop', max: 2 }],
  edges: [{ from: { node: 'again', port: 'repeat' }, to: 'work' }],
};
const history = [
  { stage: 'work', run: 0, node_visit_id: 'exec.1', workflow_execution_id: 'exec', template: 'code', visit_attempt: 1 },
  { stage: 'work', run: 1, node_visit_id: 'exec.1', workflow_execution_id: 'exec', template: 'code', visit_attempt: 2 },
  { stage: 'work', run: 2, node_visit_id: 'exec.2', workflow_execution_id: 'exec', template: 'code', visit_attempt: 1 },
];
function graphTicket() {
  return { id: 'graph', kontora: true, status: 'paused', stage: 'work', attempt: 0,
    graph_definition: definition, graph_execution: { execution_id: 'exec', revision: 4, current_node: 'work', current_template: 'code', visit: 2, visit_id: 'exec.2', attempt: 0,
      selected_edges: [{ from: 'again', port: 'repeat', to: 'work' }], loops: [{ node_id: 'again', used: 1, max: 2, exhausted: false }],
      transitions: [{ from: 'again', port: 'repeat', to: 'work', revision: 3 }] }, history };
}
function state(ticket = graphTicket()) {
  const merged = {};
  for (const mixin of [kontoraTickets(), kontoraActivity(), kontoraDetail(), kontoraMobile()]) Object.defineProperties(merged, Object.getOwnPropertyDescriptors(mixin));
  return Object.assign(merged, {
    selectedTicket: ticket, now: Date.now(), outputSubject() { return this.selectedTicket; }, currentTicketCost: () => null,
    ticketStageCost: () => null, ticketCanControl: () => true, plannotatorInFlight: {}, configCache: {},
    isSecondary: t => !!t?.source && t.source !== 'primary',
  });
}

test('graph topology and selected transitions never imply YAML-order progress', () => {
  const s = state();
  assert.equal(kontoraBoard().cardStageProgress({ ...s.selectedTicket, stages: ['unused', 'work'], execution_active: true }), null);
  assert.equal(s.canSetStage(), false);
  assert.equal(s.canSkipStage(), false);
  assert.equal(s.stageMaxAttempts(), 0);
  assert.deepEqual(s.stageRibbon().map(r => r.name), ['work']);
  const execution = s.selectedTicket.graph_execution;
  assert.doesNotMatch(s.workflowCursorText(s.selectedTicket), /Stage \d|\d of \d/);
  assert.match(s.workflowCursorText(s.selectedTicket), /Current node: work · template code/);
  assert.match(s.workflowCursorText(s.selectedTicket), /Loop again: 1 \/ 2/);
  assert.match(s.workflowTraceText(execution), /r3: again.repeat -> work/);
  assert.match(s.workflowConfigText(definition), /again.repeat -> work/);
  assert.doesNotMatch(s.workflowConfigText(definition), /\[selected\]|r3:/);
  const reconfigured = { ...definition, edges: [{ from: { node: 'again', port: 'repeat' }, to: 'unused' }] };
  assert.match(s.workflowConfigText(reconfigured), /again.repeat -> unused/);
  assert.match(s.workflowTraceText(execution), /r3: again.repeat -> work/);
  assert.equal(s.mobileStageState({ name: 'unused', state: 'queued', runs: 0 }), 'No recorded visit');
  delete execution.loops[0].max;
  assert.match(s.workflowCursorText(s.selectedTicket), /Loop again: 1 \/ unknown/);
  assert.doesNotMatch(s.workflowCursorText(s.selectedTicket), /undefined|1 \/ 0/);
});

for (const scenario of [
  { status: 'paused', history, names: ['work'] },
  { status: 'done', history, names: ['work'] },
  { status: 'paused', history: [], names: [] },
  { status: 'done', history: [], names: [] },
]) {
  test(`graph ribbon excludes reached end nodes: ${scenario.status}, ${scenario.history.length} runs`, () => {
    const ticket = graphTicket();
    ticket.stage = 'stop';
    ticket.status = scenario.status;
    ticket.history = scenario.history;
    ticket.graph_definition = { ...definition, nodes: [...definition.nodes, { id: 'stop', type: 'end', status: scenario.status }] };
    ticket.graph_execution = { ...ticket.graph_execution, current_node: 'stop' };
    const ribbon = state(ticket).stageRibbon();
    assert.deepEqual(ribbon.map(segment => segment.name), scenario.names);
    assert.ok(ribbon.every(segment => segment.state === 'done'));
  });
}

for (const [when, label] of [
  [{ field: 'exit_code', equals: 0 }, 'exit_code = 0'],
  [{ field: 'exit_code', not_equals: 0 }, 'exit_code != 0'],
  [{ field: 'exit_code', in: [1, 2] }, 'exit_code in [1, 2]'],
  [{ field: 'verdict', equals: 'needs changes' }, 'verdict = "needs changes"'],
  [{ field: 'verdict', not_equals: 'say "yes"' }, 'verdict != "say \\"yes\\""'],
  [{ field: 'verdict', in: ['red, blue'] }, 'verdict in ["red, blue"]'],
  [{ field: 'verdict', in: ['red', 'blue'] }, 'verdict in ["red", "blue"]'],
]) {
  test(`workflow graph and definition show ${label}`, () => {
    const t = graphTicket();
    t.graph_definition = { entry: 'choose', nodes: [{ id: 'choose', type: 'condition', when }], edges: [] };
    t.graph_execution = { current_node: 'choose' };
    const s = state(t);
    const [node] = s.workflowGraph(t).nodes;
    assert.equal(node.sub, label);
    assert.deepEqual(node.config, [['when', label]]);
    assert.equal(s.workflowConfigText(t.graph_definition), `Entry: choose\nchoose [condition] · ${label}`);
  });
}

test('a command condition shows its command and evaluation without a live attempt', () => {
  const t = graphTicket();
  const command = 'test -f ready';
  t.graph_definition = { ...definition, nodes: [...definition.nodes,
    { id: 'choose', type: 'condition', command, timeout: '3s' },
    { id: 'stop', type: 'end', status: 'done' },
  ], edges: [...definition.edges,
    { from: { node: 'work', port: 'complete' }, to: 'choose' },
    { from: { node: 'choose', port: 'true' }, to: 'again' },
    { from: { node: 'choose', port: 'false' }, to: 'stop' },
    { from: { node: 'again', port: 'exhausted' }, to: 'stop' },
  ] };
  t.stage = 'choose';
  t.status = 'in_progress';
  t.execution_active = true;
  t.graph_execution = { ...t.graph_execution, current_node: 'choose', current_template: '', transitions: [
    { from: 'work', port: 'complete', to: 'choose', revision: 1 },
    { from: 'choose', port: 'true', to: 'again', revision: 2, evaluation_id: '00000000000000000001-previous' },
    { from: 'again', port: 'repeat', to: 'work', revision: 2 },
    { from: 'work', port: 'complete', to: 'choose', revision: 3 },
  ] };
  delete t.graph_execution.attempt;
  const s = state(t);
  const choose = t => s.workflowGraph(t).nodes.find(n => n.id === 'choose');
  const node = choose(t);
  assert.equal(node.sub, command);
  assert.deepEqual(node.config.slice(0, 2), [['command', command], ['timeout', '3s']]);
  assert.match(node.description, /Exit code 0 selects true/);
  assert.match(node.description, /Other exits, timeouts, and runner errors pause without taking an edge\./);
  assert.equal(node.state, 'current');
  assert.equal(node.statusLabel, 'evaluating');
  assert.deepEqual(node.visits, []);
  assert.match(s.workflowCursorText(t), /Current node: choose · evaluating/);
  assert.doesNotMatch(s.workflowCursorText(t), /attempt/);
  assert.match(s.workflowTraceText(t.graph_execution), /evaluation 00000000000000000001-previous/);
  assert.equal(s.runningRun(), null);
  assert.equal(node.decisions[0].result, 'evaluation 00000000000000000001-previous');
  const queued = { ...t, status: 'todo', execution_active: false };
  assert.equal(choose(queued).statusLabel, 'at cursor');
  const paused = choose({ ...queued, status: 'paused' });
  assert.equal(paused.statusLabel, 'paused · no edge taken');
  assert.equal(paused.state, 'reached');
  assert.deepEqual(paused.decisions, node.decisions);
  const later = { ...queued, stage: 'work', graph_execution: { ...t.graph_execution, current_node: 'work' } };
  assert.equal(choose(later).statusLabel, 'took true');
});

for (const type of ['agent', 'check', 'condition', 'loop', 'wait', 'end']) {
  test(`a running graph adds a live attempt only for executable nodes: ${type}`, () => {
    const t = graphTicket();
    t.history = [];
    t.execution_active = true;
    t.graph_definition = { entry: 'work', nodes: [{ id: 'work', type, max: 2, status: 'done' }], edges: [] };
    const s = state(t);
    const [node] = s.workflowGraph(t).nodes;
    const live = ['agent', 'check'].includes(type);
    assert.deepEqual(node.visits.flatMap(v => v.attempts.map(a => a.key)), live ? ['live'] : []);
    assert.notEqual(node.statusLabel, 'evaluating');
  });
}

test('graph output navigation separates visits from retries and preserves artifact run indexes', () => {
  const s = state();
  const rows = s.outputRuns();
  assert.deepEqual(rows.map(r => r.run), [2, 1, 0]);
  assert.match(rows[0].label, /visit exec.2 · attempt 1/);
  assert.match(rows[1].label, /visit exec.1 · attempt 2/);
  assert.equal(s.historyLabel({ stage: 'code' }), 'code');
  s.selectedTicket = { ...s.selectedTicket, status: 'in_progress', execution_active: true, attempt: 0, graph_execution: { ...s.selectedTicket.graph_execution, attempt: 1 } };
  assert.match(s.outputRuns()[0].label, /visit 2 · attempt 2 · live/);
  assert.equal(s.outputRuns()[0].run, 3);
  s.activityStage = 'work';
  s.activityRun = 2;
  assert.match(s.activityRunLabel(), /visit exec.2 · attempt 1/);
  s.activityRun = 3;
  assert.match(s.activityRunLabel(), /visit exec.2 · attempt 2/);
});

test('graph runs link completed local rows by occurrence, including imported rows in the count', () => {
  const t = graphTicket();
  t.history = history.map((h, i) => ({ ...h, imported: i === 0, completed_at: i === 2 ? '' : '2026-01-01T00:00:00Z' }));
  const s = state(t), opens = [];
  s.openActivity = (...args) => opens.push(args);
  const rows = s.workflowRunRows(t, t.graph_execution);
  assert.deepEqual(rows.map(r => r.run), [0, 1, 2]);
  for (const row of rows) s.openWorkflowRun(row);
  assert.deepEqual(opens, [['work', 1]]);
  assert.deepEqual(s.workflowRunRows(t, { execution_id: 'earlier' }), []);
  s.selectedTicket = { ...t, history: [] };
  s.openWorkflowRun(rows[1]);
  assert.equal(opens.length, 1);
});

test('workflow graph separates recorded attempts, control decisions, and configured paths', () => {
  const t = graphTicket();
  const port = (node, port, to) => ({ from: { node, port }, to });
  t.graph_definition = { entry: 'work', nodes: [
    { id: 'work', type: 'agent', template: 'code', handoff: 'previous' },
    { id: 'again', type: 'loop', max: 2 }, { id: 'release', type: 'wait', mode: 'human' },
    { id: 'handoff', type: 'end', status: 'human_review' }, { id: 'inspection', type: 'end', status: 'paused' },
  ], edges: [port('work', 'complete', 'again'), port('work', 'failed', 'inspection'),
    port('again', 'repeat', 'work'), port('again', 'exhausted', 'release'),
    port('release', 'released', 'handoff'), port('release', 'timeout', 'inspection')] };
  t.history = [2, 1, 0].map(i => ({ ...history[i], run_outcome: i === 0 ? 'failed' : 'complete',
    started_at: '2026-10-01T00:00:00Z', completed_at: '2026-10-01T00:01:00Z' }));
  t.history.push({ ...t.history[0], workflow_execution_id: 'old' });
  const x = t.graph_execution;
  Object.assign(x, { current_node: 'release', wait: { id: 'wait-1', node_id: 'release', visit_id: 'exec.2',
    kind: 'human', phase: 'pending', deadline: '2026-10-01T21:14:00Z' } });
  x.transitions = [
    { from: 'work', port: 'complete', to: 'again', revision: 2 },
    { from: 'again', port: 'repeat', to: 'work', revision: 2, loop_count: 1 },
    { from: 'work', port: 'complete', to: 'again', revision: 6 },
    { from: 'again', port: 'exhausted', to: 'release', revision: 6, loop_count: 1 },
  ];
  const s = state(t);
  s.now = Date.parse('2026-10-01T00:00:00Z');
  s.ticketKey = ticket => ticket.id;
  s.ticketRunCost = () => ({ cost_usd: '0.120000' });
  const graph = s.workflowGraph(t), node = id => graph.nodes.find(n => n.id === id);
  assert.deepEqual(node('work').visits.map(v => [v.id, v.attempts.map(a => a.number).join(',')]).sort(), [['exec.1', '1,2'], ['exec.2', '1']]);
  assert.match(node('work').sub, /2 visits · 3m · \$0\.36/);
  assert.ok(node('work').config.some(([k, v]) => k === 'evidence' && v === 'previous'));
  assert.deepEqual(node('again').decisions.map(d => [d.revision, d.port]), [[2, 'repeat'], [6, 'exhausted']]);
  assert.equal(graph.edges.find(e => e.from === 'work').label, 'complete ×2');
  assert.equal(graph.edges.find(e => e.port === 'released').state, 'next');
  assert.equal(graph.edges.find(e => e.port === 'exhausted').state, 'fail');
  assert.equal(node('inspection'), undefined);
  assert.deepEqual(node('release').exits, [{ port: 'timeout', to: 'inspection', state: 'next' }]);
  assert.equal(node('release').sub, 'human · 21h left');
  assert.equal(s.workflowInspectedNode(graph).id, 'release');
  s.workflowSelectNode('work');
  assert.equal(s.workflowInspectedNode(graph).id, 'work');
  s.selectedTicket = { ...t, id: 'another' };
  assert.equal(s.workflowInspectedNode(graph).id, 'release');

  const timeout = { from: 'release', port: 'timeout', to: 'inspection', revision: 8 };
  const timedOut = { ...t, graph_execution: { ...x, wait: null, wait_history: [x.wait], current_node: 'inspection', transitions: [...x.transitions, timeout] } };
  const result = s.workflowGraph(timedOut);
  assert.equal(result.nodes.find(n => n.id === 'inspection').state, 'failed');
  assert.equal(result.edges.find(e => e.port === 'timeout').state, 'fail');
  assert.equal(result.nodes.find(n => n.id === 'release').visits[0].attempts[0].outcome, 'timeout');
  assert.equal(result.unmatched, false);
  const removedEdge = { ...timedOut, graph_definition: { ...t.graph_definition, edges: t.graph_definition.edges.filter(e => e.from.port !== 'timeout') } };
  assert.equal(s.workflowGraph(removedEdge).unmatched, true);
});

test('thin list keeps a trace only for the same ticket, execution and revision', () => {
  for (const patch of [{}, { execution_id: 'new' }, { revision: 5 }, { definition_fingerprint: 'changed' }]) {
    const s = state(), t = s.selectedTicket;
    s.sameTicket = (a, b) => a.id === b.id;
    s.ticketKey = t => t.id;
    s.queueTicketUpdate = projection => { s.selectedTicket = projection; };
    const { transitions, ...thin } = t.graph_execution;
    s._reconcileSnapshotSelection([{ id: t.id, status: 'paused', graph_definition: definition,
      graph_execution: { ...thin, ...patch }, _graphDetailPending: true }], false, new Set());
    if (Object.keys(patch).length === 0) {
      assert.deepEqual(s.selectedTicket.graph_execution.transitions, transitions);
      assert.equal(s.selectedTicket.history, t.history);
      assert.equal(s.selectedTicket._graphDetailPending, undefined);
    } else {
      assert.equal(s.selectedTicket.graph_execution.transitions, undefined);
      assert.equal(s.selectedTicket.history, t.history);
      assert.equal(s.selectedTicket._graphDetailPending, true);
      assert.deepEqual(s.workflowRunRows(s.selectedTicket, s.selectedTicket.graph_execution), []);
      const graph = s.workflowGraph(s.selectedTicket);
      assert.ok(graph.nodes.every(n => n.state === 'idle' && !n.current && !n.visits.length));
      assert.ok(graph.edges.length > 0);
      assert.ok(graph.edges.every(e => e.state === 'idle'));
      s.selectedTicket.execution_active = true;
      assert.equal(s.runningRun(), null, 'a thin snapshot cannot supply the live occurrence');
      assert.deepEqual(s.outputRuns().map(r => r.run), [2, 1, 0]);
    }
  }
});

test('graph SSE updates runtime without replacing unsaved text or editor state', () => {
  const s = state();
  Object.assign(s, { editing: true, selectedEpic: null, tickets: [s.selectedTicket], currentView: 'board', activeTab: 'ticket',
    createForm: {}, editForm: { body: 'unsaved form text' }, _selectedTicketUpdateSeq: 0,
    isSecondary: () => false, _touchTicket() {}, refreshInitContext() {}, sameTicket: (a, b) => a?.id === b?.id,
    ticketKey: ticket => ticket?.id || '',
    editorCanEdit: () => true, logTabActive: () => false, reconcileOutput() {},
  });
  s.selectedTicket.body = 'original body';
  s.selectedTicket.description = 'original description';
  const next = { ...graphTicket(), stage: 'again', body: 'server body', description: 'server description',
    graph_execution: { ...graphTicket().graph_execution, current_node: 'again', revision: 8, loops: [{ node_id: 'again', used: 2, max: 2, exhausted: true }] } };
  s.applyTicketUpdate(next);
  assert.equal(s.selectedTicket.graph_execution.revision, 8);
  assert.equal(s.selectedTicket.graph_execution.loops[0].exhausted, true);
  assert.equal(s.selectedTicket.stage, 'again');
  assert.equal(s.selectedTicket.body, 'original body');
  assert.equal(s.selectedTicket.description, 'original description');
  assert.equal(s.editForm.body, 'unsaved form text');
  assert.equal(s.editing, true);
});

const graphYAML = `pipelines:
  repair:
    version: 2 # keep graph comment
    entry: work
    nodes:
      - {id: stop, type: end, status: paused}
      - {id: work, type: agent, stage: implement, agent: claude}
      - {id: again, type: loop, max: 2}
      - {id: passed, type: condition, when: {field: exit_code, equals: 0}}
      - {id: failed, type: condition, when: {field: exit_code, not_equals: 0}}
      - {id: repairable, type: condition, when: {field: exit_code, in: [010, 10]}}
    edges:
      - {from: {node: again, port: repeat}, to: work}
`;

const settingsGraphDefinition = {
  entry: 'work',
  nodes: [
    { id: 'passed', type: 'condition', when: { field: 'exit_code', equals: 0 } },
    { id: 'failed', type: 'condition', when: { field: 'exit_code', not_equals: 0 } },
    { id: 'repairable', type: 'condition', when: { field: 'exit_code', in: [8, 10] } },
  ],
  edges: [{ from: { node: 'again', port: 'repeat' }, to: 'work' }],
};
const settingsGraphConfig = { pipelines: [{ name: 'repair', graph_definition: settingsGraphDefinition }] };

for (const operand of ['in: [010, 10]', 'equals: 0, in: [1]', 'in: [{toString: 0}]']) {
  test(`settings shows the active graph when disk has ${operand}`, async () => {
    const raw = settingsFixture + graphYAML.replace('in: [010, 10]', operand);
    const s = await settingsState(raw, { state: { configCache: settingsGraphConfig } });
    assert.match(s.settingsPipelineGraphText('repair'), /^Active graph\n/);
    assert.match(s.settingsPipelineGraphText('repair'), /exit_code in \[8, 10\]/);
    s.configCache = { pipelines: [] };
    assert.match(s.settingsPipelineGraphText('repair'), /No active graph/);
  });
}

test('settings graph is read-only topology and unrelated saves preserve its YAML', async () => {
  const raw = settingsFixture + graphYAML.replace('010', '8');
  let saved;
  const s = await settingsState(raw, {
    state: { configCache: settingsGraphConfig },
    fetch: async (_url, options) => {
      if (!options) return { ok: true, json: async () => ({ content: raw }) };
      saved = JSON.parse(options.body).content;
      return { status: 204 };
    },
  });
  assert.equal(s.settingsPipelineSteps('repair').length, 0);
  assert.equal(s.settingsPipelineGraph('repair').entry, 'work');
  assert.match(s.settingsPipelineGraphText('repair'), /again.repeat -> work/);
  assert.match(s.settingsPipelineGraphText('repair'), /exit_code = 0/);
  assert.match(s.settingsPipelineGraphText('repair'), /exit_code != 0/);
  assert.match(s.settingsPipelineGraphText('repair'), /exit_code in \[8, 10\]/);
  assert.deepEqual(Array.from(s.settingsStagePipelines('implement')), ['repair']);
  assert.deepEqual(Array.from(s.settingsAgentStages('claude')), ['implement']);
  assert.doesNotThrow(() => s.settingsSectionSummary('pipelines'));
  assert.doesNotThrow(() => s.settingsFieldCatalogue());
  s.settingsConfig.general.branch_prefix = 'changed';
  assert.equal(await s.saveSettings(), true);
  assert.deepEqual(parse(saved).pipelines, parse(raw).pipelines);
  assert.match(saved, /version: 2 # keep graph comment/);
  assert.match(saved, /branch_prefix: changed/);
});
