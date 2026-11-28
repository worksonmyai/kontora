import assert from 'node:assert/strict';
import test from 'node:test';
import { layoutWorkflowGraph } from '../ui/workflow_layout.js';

const nodesOf = rows => rows.map(([id, type, extra]) => ({ id, type, ...extra }));
const edgesOf = rows => rows.map(([from, port, to, state = 'idle', count = 0]) => ({ from, to, port, state, count, label: port }));
const byId = graph => Object.fromEntries(graph.nodes.map(node => [node.id, node]));

const starterNodes = nodesOf([
  ['repair', 'agent'], ['implement', 'agent'], ['tests', 'check'], ['review', 'agent'],
  ['approved', 'condition'], ['release', 'wait'], ['handoff', 'end', { status: 'human_review' }],
  ['repairable', 'condition'], ['repair-budget', 'loop'],
]);
const starterEdges = edgesOf([
  ['implement', 'complete', 'tests', 'taken'], ['tests', 'pass', 'review', 'taken'],
  ['tests', 'fail', 'repairable', 'fail'], ['repairable', 'true', 'repair-budget', 'taken'],
  ['repair-budget', 'repeat', 'repair', 'taken'], ['repair', 'complete', 'tests', 'taken'],
  ['review', 'complete', 'approved', 'taken'], ['approved', 'true', 'release', 'taken'],
  ['release', 'released', 'handoff', 'next'],
]);

function assertGeometry(graph) {
  const { width, height, nodes, edges } = graph;
  assert.ok(width > 0 && height > 0);
  function inBounds(x, y) {
    assert.ok(x >= 0 && x <= width && y >= 0 && y <= height, `${x},${y} not in ${width}×${height}`);
  }
  for (const [i, a] of nodes.entries()) {
    inBounds(a.x - a.w / 2, a.y - a.h / 2);
    inBounds(a.x + a.w / 2, a.y + a.h / 2);
    for (const b of nodes.slice(i + 1)) {
      assert.ok(Math.abs(a.x - b.x) >= (a.w + b.w) / 2 || Math.abs(a.y - b.y) >= (a.h + b.h) / 2,
        `${a.id} overlaps ${b.id}`);
    }
  }
  for (const { d, arrow, labelX, labelY, labelSide } of edges) {
    assert.match(d, /^M[-\d.]+ [-\d.]+(?: C[-\d.]+ [-\d.]+ [-\d.]+ [-\d.]+ [-\d.]+ [-\d.]+)+$/);
    assert.match(arrow, /^[-\d.]+,[-\d.]+(?: [-\d.]+,[-\d.]+){2}$/);
    for (const coords of [d.replace(/[MC]/g, '').split(' '), arrow.split(/[ ,]/)]) {
      for (let i = 0; i < coords.length; i += 2) inBounds(Number(coords[i]), Number(coords[i + 1]));
    }
    inBounds(labelX, labelY);
    assert.ok(['right', 'left', 'above', 'below', 'center'].includes(labelSide));
  }
}

test('matches the G2A starter projection independent of input order and edge state', () => {
  const layout = layoutWorkflowGraph('implement', starterNodes, starterEdges);
  assert.deepEqual([layout.width, layout.height], [540, 590]);
  assert.deepEqual(Object.fromEntries(layout.nodes.map(({ id, x, y, w, h, labelSide }) => [id, [x, y, w, h, labelSide]])), {
    implement: [370, 32, 150, 50, undefined],
    tests: [370, 128, 150, 50, undefined],
    review: [370, 312, 150, 50, undefined],
    approved: [370, 398, 26, 26, 'right'],
    release: [370, 480, 150, 50, undefined],
    handoff: [370, 562, 170, 26, undefined],
    repairable: [120, 128, 26, 26, 'above'],
    'repair-budget': [120, 214, 36, 36, 'left'],
    repair: [120, 312, 150, 50, undefined],
  });
  const fail = layout.edges.find(edge => edge.port === 'fail');
  assert.equal(fail.d, 'M295 118 C228.1 118 203 128 143.1 128');
  assert.equal(fail.arrow, '136.1,128 143.1,124.2 143.1,131.8');
  assert.deepEqual([fail.labelX, fail.labelY, fail.labelSide], [215.6, 123, 'above']);
  const repeat = layout.edges.find(edge => edge.port === 'repeat');
  assert.deepEqual([repeat.labelX, repeat.labelY, repeat.labelSide], [110, 259.5, 'left']);
  const back = layout.edges.find(edge => edge.from === 'repair');
  assert.equal(back.d, 'M160 287 C160 203.2 211.2 140 288 140');
  assert.equal(back.arrow, '295,140 288,143.8 288,136.2');
  assert.deepEqual([back.labelX, back.labelY, back.labelSide], [196.1, 182.1, 'center']);
  const idle = layoutWorkflowGraph('implement', starterNodes.toReversed(), starterEdges.toReversed().map(edge => ({ ...edge, state: 'idle' })));
  assert.deepEqual(idle.nodes.toReversed(), layout.nodes);
  assert.deepEqual(idle.edges.toReversed().map(edge => edge.d), layout.edges.map(edge => edge.d));
  assertGeometry(layout);
});

test('requires the actual starter topology, not just its node IDs', () => {
  for (const edges of [
    starterEdges.map(edge => edge.port === 'pass' ? { ...edge, to: 'release' } : edge),
    starterEdges.map(edge => edge.port === 'pass' ? starterEdges[0] : edge),
  ]) {
    const layout = layoutWorkflowGraph('implement', starterNodes, edges);
    assert.notDeepEqual([layout.width, layout.height], [540, 590]);
    assert.equal(byId(layout).handoff.w, 132);
    assertGeometry(layout);
  }
});

test('follows recorded non-failure ports and ranks a merge after its longest dependency', () => {
  const nodes = nodesOf([
    ['join', 'end'], ['prepare', 'check'], ['entry', 'agent'], ['gate', 'condition'],
    ['long-branch', 'agent'], ['main', 'agent'], ['fix', 'agent'],
  ]);
  const edges = edgesOf([
    ['entry', 'failed', 'fix', 'fail', 5], ['fix', 'complete', 'entry', 'taken'],
    ['entry', 'complete', 'gate', 'taken'],
    ['gate', 'true', 'long-branch', 'taken', 1], ['gate', 'false', 'main', 'taken', 2],
    ['long-branch', 'complete', 'prepare'], ['prepare', 'pass', 'join'], ['main', 'complete', 'join', 'taken'],
  ]);
  const layout = layoutWorkflowGraph('entry', nodes, edges);
  const n = byId(layout);
  for (const id of ['gate', 'main', 'join']) assert.equal(n[id].x, n.entry.x);
  assert.notEqual(n.fix.x, n.entry.x);
  assert.notEqual(n['long-branch'].x, n.entry.x);
  assert.equal(n['long-branch'].x, n.prepare.x);
  assert.ok(n.entry.y < n.fix.y && n.main.y < n.join.y && n.prepare.y < n.join.y);
  assertGeometry(layout);
  const idle = byId(layoutWorkflowGraph('entry', nodes, edges.map(edge => ({ ...edge, state: 'idle', count: 0 }))));
  assert.equal(idle.entry.x, idle['long-branch'].x, 'on an unrecorded graph, use the first declared non-failure port');
});

test('keeps a loop-only detour off the spine when a non-failure exit exists', () => {
  const graph = layoutWorkflowGraph('gate', nodesOf([['gate', 'condition'], ['retry', 'agent'], ['end', 'end']]), edgesOf([
    ['gate', 'true', 'retry', 'taken', 3], ['retry', 'complete', 'gate', 'taken', 3], ['gate', 'false', 'end'],
  ]));
  const { gate, retry, end } = byId(graph);
  assert.equal(gate.x, end.x);
  assert.notEqual(gate.x, retry.x);
  assert.ok(gate.y < retry.y);
  assertGeometry(graph);
});

test('routes parallel, reciprocal and self edges without mutating or losing input data', () => {
  const nodes = Object.freeze(nodesOf([
    ['work', 'agent', { sub: 'custom data' }], ['again', 'loop', { counter: '2/3' }], ['done', 'end', { w: 220 }],
  ]).map(Object.freeze));
  const edges = Object.freeze(edgesOf([
    ['work', 'a', 'again', 'taken', 3], ['work', 'b', 'again', 'next'], ['work', 'c', 'again'],
    ['again', 'repeat', 'work', 'taken'], ['again', 'continue', 'again', 'taken'], ['again', 'retry', 'again'],
    ['again', 'finish', 'done', 'next'],
  ]).map(Object.freeze));
  const layout = layoutWorkflowGraph('work', nodes, edges);
  assert.deepEqual(layoutWorkflowGraph('work', nodes, edges), layout);
  assert.equal(byId(layout).work.sub, 'custom data');
  assert.equal(byId(layout).again.counter, '2/3');
  assert.ok(byId(layout).again.labelSide);
  assert.equal(byId(layout).done.w, 220);
  assert.equal(layout.edges.length, edges.length);
  assert.equal(new Set(layout.edges.map(edge => edge.d)).size, edges.length);
  assert.equal(new Set(layout.edges.slice(0, 3).map(edge => edge.arrow)).size, 3);
  assert.equal(new Set(layout.edges.filter(edge => edge.from === edge.to).map(edge => edge.arrow)).size, 2);
  assert.deepEqual(layout.edges.map(({ d, arrow, labelX, labelY, labelSide, ...edge }) => edge), edges);
  assertGeometry(layout);
});

test('draws a taken paused end as a side sink, identified by status rather than name', () => {
  const layout = layoutWorkflowGraph('gate', nodesOf([
    ['gate', 'condition'], ['stop-to-investigate', 'end', { status: 'paused' }], ['inspection', 'end', { status: 'done' }],
  ]), edgesOf([['gate', 'true', 'stop-to-investigate', 'taken', 4], ['gate', 'false', 'inspection']]));
  const n = byId(layout);
  assert.equal(n.gate.x, n.inspection.x);
  assert.notEqual(n.gate.x, n['stop-to-investigate'].x);
  assert.deepEqual([n['stop-to-investigate'].type, n['stop-to-investigate'].status, n['stop-to-investigate'].h], ['end', 'paused', 26]);
  assert.equal(layout.edges.find(edge => edge.to === 'stop-to-investigate').state, 'taken');
  assertGeometry(layout);
});

test('lays out disconnected components and a closed cycle, and handles empty inputs', () => {
  const layout = layoutWorkflowGraph('start', nodesOf([
    ['start', 'check'], ['retry', 'loop'], ['separate', 'wait'], ['end', 'end'],
  ]), edgesOf([['start', 'pass', 'retry'], ['retry', 'repeat', 'start'], ['separate', 'released', 'end']]));
  const n = byId(layout);
  assert.ok(n.start.y < n.retry.y && n.separate.y < n.end.y);
  assertGeometry(layout);
  assert.deepEqual(layoutWorkflowGraph(undefined, [], []), { width: 0, height: 0, nodes: [], edges: [] });
});
