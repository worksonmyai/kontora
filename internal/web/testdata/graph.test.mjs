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
  });
}

test('graph topology and selected transitions never imply YAML-order progress', () => {
  const s = state();
  assert.equal(kontoraBoard().cardStageProgress({ ...s.selectedTicket, stages: ['unused', 'work'], execution_active: true }), null);
  assert.equal(s.canSetStage(), false);
  assert.equal(s.canSkipStage(), false);
  assert.equal(s.stageMaxAttempts(), 0);
  assert.deepEqual(s.stageRibbon().map(r => r.name), ['work']);
  assert.doesNotMatch(s.graphText(s.selectedTicket), /Stage \d|\d of \d/);
  assert.match(s.graphText(s.selectedTicket), /current: work · template code/);
  assert.match(s.graphText(s.selectedTicket), /\[selected\] again.repeat -> work/);
  assert.match(s.graphText(s.selectedTicket), /r3: again.repeat -> work/);
  assert.match(s.graphText(s.selectedTicket), /loop again: 1 \/ 2/);
  assert.equal(s.mobileStageState({ name: 'unused', state: 'queued', runs: 0 }), 'No recorded visit');
  delete s.selectedTicket.graph_execution.loops[0].max;
  assert.match(s.graphText(s.selectedTicket), /loop again: 1 \/ unknown/);
  assert.doesNotMatch(s.graphText(s.selectedTicket), /undefined|1 \/ 0/);
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

test('graph SSE updates runtime without replacing unsaved text or editor state', () => {
  const s = state();
  Object.assign(s, { editing: true, selectedEpic: null, tickets: [s.selectedTicket], currentView: 'board', activeTab: 'ticket',
    createForm: {}, editForm: { body: 'unsaved form text' }, _selectedTicketUpdateSeq: 0,
    isSecondary: () => false, _touchTicket() {}, refreshInitContext() {}, sameTicket: (a, b) => a?.id === b?.id,
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
    edges:
      - {from: {node: again, port: repeat}, to: work}
`;

test('settings graph is read-only topology and unrelated saves preserve its YAML', async () => {
  const raw = settingsFixture + graphYAML;
  let saved;
  const s = await settingsState(raw, { fetch: async (_url, options) => {
    if (!options) return { ok: true, json: async () => ({ content: raw }) };
    saved = JSON.parse(options.body).content;
    return { status: 204 };
  } });
  assert.equal(s.settingsPipelineSteps('repair').length, 0);
  assert.equal(s.settingsPipelineGraph('repair').entry, 'work');
  assert.match(s.settingsPipelineGraphText('repair'), /again.repeat -> work/);
  assert.match(s.settingsPipelineGraphText('repair'), /exit_code = 0/);
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
