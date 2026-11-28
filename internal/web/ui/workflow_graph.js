import { layoutWorkflowGraph } from './workflow_layout.js';

const failurePorts = new Set(['fail', 'failed', 'false', 'exhausted', 'timeout']);
const edgeKey = ({ from, port, to }) => JSON.stringify([from, port, to]);
const types = { agent: 'agent', check: 'check', condition: 'if', loop: 'loop', wait: 'wait', end: 'end' };
const descriptions = {
  agent: 'Runs the configured agent in the ticket worktree.',
  check: 'Runs the configured command. Exit code 0 passes; any other exit code fails.',
  condition: 'Compares one field from the last executable result.',
  loop: 'Allows a set number of repeats per execution. Restarting at entry resets the count.',
};

function time(value) {
  return value && !value.startsWith('0001-') ? Date.parse(value) : NaN;
}

export function workflowDuration(seconds, compact = false) {
  const s = Math.max(0, Math.floor(seconds));
  if (s >= 3600) return Math.floor(s / 3600) + 'h' + (!compact && s % 3600 >= 60 ? ' ' + Math.floor(s % 3600 / 60) + 'm' : '');
  if (s >= 60) return Math.floor(s / 60) + 'm' + (!compact && s % 60 ? ' ' + s % 60 + 's' : '');
  return s + 's';
}

export function workflowDeadline(wait, now, compact = false) {
  const deadline = time(wait?.deadline);
  if (!Number.isFinite(deadline)) return '';
  return deadline > now ? workflowDuration((deadline - now) / 1000, compact) + ' left' : 'deadline passed';
}

export function workflowWaitFacts(wait) {
  if (!wait) return [];
  return [
    ['wait', [wait.kind, wait.id].filter(Boolean).join(' · ')], ['phase', [wait.phase, wait.disarmed && 'disarmed'].filter(Boolean).join(' · ')],
    ['visit', wait.visit_id], ['invocation', wait.invocation_id], ['polls', wait.poll_count || 0],
    ...['requested_at', 'until', 'deadline', 'next_poll'].filter(k => Number.isFinite(time(wait[k]))).map(k => [k.replaceAll('_', ' '), wait[k]]),
    ['poll interval', wait.poll && workflowDuration(wait.poll / 1e9)],
    ...['command', 'next', 'output', 'log_path', 'repository', 'branch', 'environment', 'required_commit'].map(k => [k.replaceAll('_', ' '), wait[k]]),
    ['output truncated', wait.output_truncated && 'yes'], ['receipt', wait.receipt?.id], ['request', wait.receipt?.request_id],
    ['reason', wait.receipt?.reason], ['commit', wait.receipt?.commit], ['accepted', wait.receipt?.accepted_at],
  ].filter(([, value]) => value || value === 0);
}

function attemptState(outcome) {
  if (outcome === 'complete' || outcome === 'released') return 'done';
  if (outcome === 'disarmed') return 'disarmed';
  if (outcome === 'waiting' || outcome === 'running') return 'current';
  if (['failed', 'blocked', 'protocol_error', 'timeout'].includes(outcome)) return 'failed';
  return 'idle';
}

function visitDuration(attempts) {
  return attempts.reduce((sum, a) => sum + (a.seconds || 0), 0);
}

function measuredCost(items) {
  return items.some(a => a.usd != null) ? items.reduce((sum, a) => sum + (a.usd ?? 0), 0) : null;
}

function metrics(seconds, usd) {
  return [seconds > 0 && workflowDuration(seconds), usd != null && '$' + usd.toFixed(2)].filter(Boolean).join(' · ');
}

function resultLabel(result, attempt) {
  if (result.verdict) return 'verdict ' + result.verdict;
  if (Number.isInteger(result.exit_code)) return 'exit ' + result.exit_code;
  return attempt > 1 ? 'attempt ' + attempt : '';
}

function addVisit(groups, node, key, id) {
  if (!groups.has(node)) groups.set(node, new Map());
  const visits = groups.get(node);
  if (!visits.has(key)) visits.set(key, { key, id, attempts: [] });
  return visits.get(key);
}

function declaredWaitPorts(execution, routing) {
  const waits = new Map();
  for (const wait of execution.wait_history || []) {
    if (wait.invocation_id || !['human', 'deployment'].includes(wait.kind)) continue;
    if (!waits.has(wait.node_id)) waits.set(wait.node_id, []);
    waits.get(wait.node_id).push(wait);
  }
  const ports = new Map();
  for (const [node, history] of waits) {
    // Declared waits reuse the preceding visit ID. Match departures only when the trace accounts for every wait.
    const departures = routing.get(node)?.filter(e => ['released', 'timeout'].includes(e.port));
    if (departures?.length === history.length) history.forEach((wait, i) => ports.set(wait.id, departures[i].port));
  }
  return ports;
}

function recordedVisits(execution, runs, running, runCost, now, routing) {
  const groups = new Map();
  if (!execution?.execution_id) return groups;
  for (const row of runs) {
    const h = row.history;
    const a = h.visit_attempt || 1;
    const seconds = (time(h.completed_at) - time(h.started_at)) / 1000;
    const usd = !row.imported ? runCost(row.historyIndex)?.cost_usd : null;
    addVisit(groups, h.stage, h.node_visit_id, h.node_visit_id).attempts.push({
      key: row.key, row, number: a, continuation: h.continuation, outcome: h.run_outcome || 'unverified',
      state: attemptState(h.run_outcome), result: resultLabel(h, a), verdict: h.verdict, reason: h.outcome_reason || h.summary,
      seconds: seconds > 0 ? seconds : 0, usd: usd != null && Number.isFinite(Number(usd)) ? Number(usd) : null,
    });
  }
  if (running && execution.visit_id) {
    const a = (execution.attempt || 0) + 1;
    addVisit(groups, execution.current_node, execution.visit_id, execution.visit_id).attempts.push({
      key: 'live', number: a, continuation: execution.continuation, outcome: 'running', state: 'current', result: 'attempt ' + a,
    });
  }
  const waitPorts = declaredWaitPorts(execution, routing);
  for (const wait of [...(execution.wait_history || []), execution.wait].filter(Boolean)) {
    if (!wait.node_id) continue;
    // A declared wait uses the preceding executable's visit ID. Its own ID distinguishes repeat waits.
    const visit = wait.invocation_id && groups.get(wait.node_id)?.get(wait.visit_id);
    if (visit) {
      const attempt = visit.attempts.find(a => a.row?.history.invocation_id === wait.invocation_id);
      if (attempt) { attempt.wait = wait; continue; }
    }
    const current = wait === execution.wait;
    const outcome = current ? (wait.disarmed ? 'disarmed' : wait.phase === 'pending' ? 'waiting' : wait.phase || 'waiting')
      : wait.receipt ? 'released' : waitPorts.get(wait.id) || 'retained';
    addVisit(groups, wait.node_id, wait.id, wait.id).attempts.push({
      key: wait.id, number: 1, wait, outcome, state: current ? (wait.disarmed ? 'disarmed' : 'current') : attemptState(outcome),
      result: current ? workflowDeadline(wait, now) : wait.receipt?.id,
    });
  }
  return groups;
}

function configRows(node, ports, wait) {
  const config = [
    ['stage', node.template], ['agent', node.agent], ['verdicts', node.verdicts?.join(', ')],
    ['command', node.command], ['timeout', node.timeout], ['when', node.when && conditionLabel(node)],
    ['max', node.type === 'loop' ? node.max : null], ['mode', node.mode], ['environment', node.environment],
    ['deadline', Number.isFinite(time(wait?.deadline)) ? wait.deadline : ''], ['evidence', node.handoff],
    ['wake', node.wake && `max ${node.wake.max}, check timeout ${node.wake.check_timeout}`], ['status', node.status],
  ].filter(([, value]) => value || value === 0);
  if (ports.length) config.push(['ports', ports.map(edge => `${edge.port} → ${edge.to}`).join('\n')]);
  return config;
}

export function conditionLabel(node) {
  const when = node.when;
  if (!when) return '';
  const format = value => typeof value === 'string' ? JSON.stringify(value) : String(value);
  if (Array.isArray(when.in)) return `${when.field} in [${when.in.map(format).join(', ')}]`;
  if (when.not_equals != null) return `${when.field} != ${format(when.not_equals)}`;
  if (when.equals != null) return `${when.field} = ${format(when.equals)}`;
  return '';
}

function description(node, released) {
  if (node.type === 'end') return 'Ends the workflow with ticket status ' + node.status + '.';
  if (node.type === 'wait') {
    if (node.mode === 'human') return 'Waits for a person to release it.' + (released ? ' Release from the band above the tabs.' : '');
    return 'Waits for a deployment receipt' + (node.environment ? ' for ' + node.environment : '') + '.';
  }
  return descriptions[node.type] || '';
}

export function buildWorkflowGraph(definition, execution, runs, { now, running, runCost, releaseAvailable }) {
  const configured = definition?.nodes || [];
  const byID = new Map(configured.map(node => [node.id, node]));
  const transitions = execution?.transitions || [];
  const wait = execution?.wait;
  const reached = new Set(execution?.current_node ? [execution.current_node] : []);
  const counts = new Map(), routing = new Map(), ports = new Map();
  for (const e of transitions) {
    reached.add(e.from);
    reached.add(e.to);
    const key = edgeKey(e);
    counts.set(key, (counts.get(key) || 0) + 1);
    if (!routing.has(e.from)) routing.set(e.from, []);
    routing.get(e.from).push(e);
  }
  const visits = recordedVisits(execution, runs, running, runCost, now, routing);
  const collapsed = new Map();
  const edges = [], visibleEdges = new Set();
  const left = workflowDeadline(wait, now, true);
  for (const e of definition?.edges || []) {
    const edge = { from: e.from.node, port: e.from.port, to: e.to };
    const count = counts.get(edgeKey(edge)) || 0;
    const next = edge.from === execution?.current_node && wait?.node_id === edge.from && !wait.disarmed && wait.phase === 'pending';
    const state = count ? failurePorts.has(edge.port) ? 'fail' : 'taken' : next ? 'next' : 'idle';
    const source = byID.get(edge.from), target = byID.get(edge.to);
    if (!ports.has(edge.from)) ports.set(edge.from, []);
    ports.get(edge.from).push(edge);
    if (!source || !target) continue;
    if (target.type === 'end' && target.status === 'paused' && !count) {
      if (!collapsed.has(edge.from)) collapsed.set(edge.from, []);
      collapsed.get(edge.from).push({ port: edge.port, to: edge.to, state });
    } else {
      const key = edgeKey(edge);
      visibleEdges.add(key);
      edges.push({ ...edge, key, count, state,
        label: edge.port + (count > 1 ? ' ×' + count : '') + (state === 'next' && edge.port === 'timeout' && left.endsWith(' left') ? ' · ' + left.slice(0, -5) : ''),
      });
    }
  }
  const loops = new Map((execution?.loops || []).map(loop => [loop.node_id, loop]));
  const nodes = configured.map(node => {
    const current = node.id === execution?.current_node;
    const waiting = current && wait?.node_id === node.id;
    const loop = loops.get(node.id);
    const decisions = routing.get(node.id) || [];
    const lastEdge = decisions.at(-1);
    const used = loop?.used ?? 0, max = loop ? loop.max || '?' : node.max;
    const usedLabel = `${used} of ${max} used`;
    const nodeVisits = [...(visits.get(node.id)?.values() || [])];
    for (const visit of nodeVisits) visit.attempts.sort((a, b) => a.number - b.number);
    const attempts = nodeVisits.flatMap(v => v.attempts);
    const last = attempts.at(-1);
    const routeState = lastEdge ? (failurePorts.has(lastEdge.port) ? 'failed' : 'done') : null;
    const sink = node.type === 'end' && node.status === 'paused';
    const visited = reached.has(node.id) || !!nodeVisits.length || !!loop?.used;
    const state = waiting ? (wait.disarmed ? 'disarmed' : 'current') : loop?.exhausted ? 'failed' :
      node.type === 'end' && visited ? (sink ? 'failed' : 'done') :
      (last?.state !== 'idle' && last?.state) || routeState || (visited ? 'reached' : 'idle');
    const status = waiting ? (wait.disarmed ? 'disarmed' : wait.phase === 'pending' ? 'waiting' : wait.phase || 'waiting') :
      node.type === 'loop' && visited ? (loop?.exhausted ? 'exhausted' : usedLabel) :
      node.type === 'condition' && lastEdge ? 'took ' + lastEdge.port :
      nodeVisits.length > 1 ? nodeVisits.length + ' visits' : last?.verdict || last?.outcome.replaceAll('_', ' ') || (visited ? current && node.type !== 'end' ? 'at cursor' : 'reached' : 'not reached');
    const details = waiting ? [wait.kind, workflowDeadline(wait, now, true)].filter(Boolean).join(' · ') :
      node.type === 'loop' ? usedLabel : node.type === 'condition' ? conditionLabel(node) : node.type === 'end' ? (!sink ? node.status : '') :
      [nodeVisits.length > 1 ? nodeVisits.length + ' visits' : last?.verdict,
        metrics(last?.verdict && nodeVisits.length === 1 ? 0 : visitDuration(attempts), measuredCost(attempts))].filter(Boolean).join(' · ');
    return { ...node, typeLabel: types[node.type] || node.type, state, statusLabel: status,
      sub: details, counter: `${used}/${max}`, current, sink, exits: collapsed.get(node.id) || [],
      config: configRows(node, ports.get(node.id) || [], waiting ? wait : last?.wait),
      description: description(node, waiting && releaseAvailable),
      visits: nodeVisits.map(visit => ({ ...visit,
        meta: metrics(visitDuration(visit.attempts), measuredCost(visit.attempts)),
        attempts: visit.attempts.map(a => ({ ...a, meta: metrics(a.seconds, a.usd), evidence: workflowWaitFacts(a.wait) })),
      })),
      decisions: ['condition', 'loop'].includes(node.type) ? decisions.map((e, i) => ({
        key: i, revision: e.revision, port: e.port, to: e.to, tone: failurePorts.has(e.port) ? 'failed' : 'done',
        result: node.type === 'loop' && Number.isInteger(e.loop_count) ? `${e.loop_count} used` : '',
      })) : [],
    };
  });
  const visible = new Set(edges.flatMap(e => [e.from, e.to]));
  const canvas = layoutWorkflowGraph(definition?.entry, nodes.filter(n => !n.sink || reached.has(n.id) || visible.has(n.id) || n.id === definition?.entry), edges);
  return { ...canvas, current: execution?.current_node, entry: definition?.entry,
    inspection: [...new Set([...collapsed.values()].flat().map(e => e.to))].join(', '),
    unmatched: transitions.some(e => !visibleEdges.has(edgeKey(e))),
  };
}
