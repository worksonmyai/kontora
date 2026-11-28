import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraTickets } from '../ui/tickets.js';
import { kontoraDetail } from '../ui/detail.js';
import { statsDerive } from '../ui/stats.js';
import { settingsFixture, settingsState } from './settings_fixtures.mjs';
import { parse } from '../static/vendor/yaml@2.8.1/yaml.mjs';

function state() {
  return Object.assign({}, kontoraTickets(), kontoraDetail(), { statusLabel: s => s, costLabel: () => '', ticketRunCost: () => null });
}
const waiting = () => ({ id: 'ticket', status: 'paused', graph_execution: {
  execution_id: 'exec', revision: 7, wake_count: 2,
  wait: { id: 'wait', kind: 'human', phase: 'pending', poll_count: 3, deadline: '2026-01-01T00:00:00Z', next: 'Review sign-off', output: '<script>not HTML</script>' },
  wait_history: [{ id: 'past', kind: 'timer', phase: 'released', receipt: { id: 'receipt', request_id: 'request', reason: 'released', accepted_at: '2025-01-01T00:00:00Z' } }],
} });

test('paused plus durable wait projects Waiting without inventing a status', () => {
  const s = state(), t = waiting();
  assert.equal(s.workflowStatusLabel(t), 'Waiting');
  assert.equal(t.status, 'paused');
  assert.equal(s.workflowStatusLabel({ status: 'paused' }), 'paused');
  assert.equal(s.workflowStatusLabel({ ...t, status: 'done' }), 'done');
  const text = s.workflowWaitText(t.graph_execution.wait) + '\n' + s.workflowTraceText(t.graph_execution);
  for (const fragment of ['Waiting: human', 'polls: 3', 'deadline: 2026-', 'Review sign-off', '<script>not HTML</script>', 'Historical wait: timer', 'receipt: receipt']) assert.ok(text.includes(fragment), fragment);
  assert.match(s.workflowReleaseInstructions(t), /kontora release ticket --execution-id exec --wait-id wait --revision 7 --request-id/);
  t.graph_execution.wait.disarmed = true;
  assert.match(s.workflowReleaseInstructions(t), /retry does not release/);
});

test('historical waiting and continuation retain provenance', () => {
  const s = state();
  assert.equal(s.runOutcomeLabel({ run_outcome: 'waiting' }), 'waiting');
  assert.match(s.historyLabel({ stage: 'work', node_visit_id: 'visit', visit_attempt: 1, continuation: true, run_outcome: 'waiting' }), /attempt 1 · continuation · Waiting/);
  const meta = s.stageCardMeta({ outcome: 'waiting', continuation: true, nextStep: 'Resume review', verdict: 'accepted', wake: { reason: 'released', wait_id: 'wait', count: 2 } });
  assert.match(meta, /continuation \(not a retry\)/);
  assert.match(meta, /next: Resume review/);
  assert.match(meta, /verdict: accepted/);
  assert.match(meta, /wake: released · wait wait · count 2/);
});

test('active waits explain release rather than dispatching ordinary retry', async () => {
  const s = state();
  Object.assign(s, { selectedTicket: waiting(), ticketCanControl: () => true, actionLoading: null });
  await s.action('retry');
  assert.match(s.error, /kontora release/);
  assert.equal(s.actionLoading, null);
});

test('waiting stats count spend and runs without unverified outcomes', () => {
  const derived = statsDerive({ totals: { runs: 1, first_pass_visits: 0 }, agents: [{ name: 'agent', runs: 1, first_pass_visits: 0 }], stages: [{ name: 'work', runs: 1, waiting: 1, tokens: 100, token_runs: 1 }] });
  assert.match(derived.stages[0].time.meta, /1 waiting/);
  assert.match(derived.stages[0].tokens.meta, /1 waiting/);
  assert.doesNotMatch(derived.stages[0].time.meta, /unverified|complete/);
  const firstPass = derived.kpis.find(k => k.label === 'first-pass');
  assert.equal(firstPass.value, '—');
  assert.equal(firstPass.delta, 'no judged visits');
  assert.equal(derived.agents[0].pct, '—');
  assert.equal(derived.agents[0].tone, 'neutral');
});

const graphYAML = `pipelines:
  repair:
    version: 2 # preserve graph
    entry: work
    nodes:
      - id: work
        type: agent
        stage: implement
        agent: claude
        wake: {max: 3, check_timeout: 60s} # preserve capability
      - {id: human, type: wait, mode: human, timeout: 1h}
      - {id: deploy, type: wait, mode: deployment, environment: production}
      - {id: stop, type: end, status: done}
    edges:
      - {from: {node: work, port: done}, to: human}
      - {from: {node: human, port: released}, to: deploy}
      - {from: {node: human, port: timeout}, to: stop}
      - {from: {node: deploy, port: released}, to: stop}
`;

test('unrelated settings save preserves all wait graph YAML and comments', async () => {
  const raw = settingsFixture + graphYAML;
  let saved;
  const graph = parse(raw).pipelines.repair;
  graph.nodes[0].wake.check_timeout = '1m0s';
  const s = await settingsState(raw, {
    state: { configCache: { pipelines: [{ name: 'repair', graph_definition: graph }] } },
    fetch: async (_url, options) => {
      if (!options) return { ok: true, json: async () => ({ content: raw }) };
      saved = JSON.parse(options.body).content;
      return { status: 204 };
    },
  });
  assert.match(s.settingsPipelineGraphText('repair'), /wake max 3 · check timeout 1m0s/);
  assert.match(s.settingsPipelineGraphText('repair'), /mode deployment · environment production/);
  s.settingsConfig.general.branch_prefix = 'changed';
  assert.equal(await s.saveSettings(), true);
  assert.deepEqual(parse(saved).pipelines, parse(raw).pipelines);
  assert.match(saved, /# preserve graph/);
  assert.match(saved, /# preserve capability/);
});
