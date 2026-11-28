const ROW = 96;
const COLUMN_GAP = 92;
const NORMAL = { left: [-1, 0], right: [1, 0], top: [0, -1], bottom: [0, 1] };
const FAILURE_PORTS = new Set(['fail', 'failed', 'exhausted', 'timeout']);

const STARTER_NODES = [
  ['implement', 'agent', 370, 32],
  ['tests', 'check', 370, 128],
  ['review', 'agent', 370, 312],
  ['approved', 'condition', 370, 398, 'right'],
  ['release', 'wait', 370, 480],
  ['handoff', 'end', 370, 562],
  ['repairable', 'condition', 120, 128, 'above'],
  ['repair-budget', 'loop', 120, 214, 'left'],
  ['repair', 'agent', 120, 312],
];
const STARTER_EDGES = [
  ['implement', 'complete', 'tests'],
  ['tests', 'fail', 'repairable', { fromSide: 'left', toSide: 'right', fromOff: -10 }],
  ['repairable', 'true', 'repair-budget'],
  ['repair-budget', 'repeat', 'repair', { toOff: -20, labelSide: 'left' }],
  ['repair', 'complete', 'tests', { fromSide: 'top', toSide: 'left', fromOff: 40, toOff: 12, labelSide: 'center' }],
  ['tests', 'pass', 'review'],
  ['review', 'complete', 'approved'],
  ['approved', 'true', 'release'],
  ['release', 'released', 'handoff'],
];

function isPausedEnd(node) {
  return node.type === 'end' && node.status === 'paused';
}

function sizeNode(node) {
  const side = node.type === 'condition' ? 26 : node.type === 'loop' ? 36 : 0;
  if (side) return { ...node, w: side, h: side };
  if (node.type === 'end') {
    const w = Number.isFinite(node.w) && node.w > 0 ? node.w : 132;
    return { ...node, w, h: 26 };
  }
  return { ...node, w: 150, h: 50 };
}

const edgeKey = (from, port, to) => JSON.stringify([from, port, to]);

function placeStarter(entry, byId, links) {
  // Keep recipe coordinates only while its visible topology matches the reference graph.
  if (entry !== 'implement' || byId.size !== STARTER_NODES.length || links.length !== STARTER_EDGES.length) return false;
  if (!STARTER_NODES.every(([id, type]) => byId.get(id)?.box.type === type) || isPausedEnd(byId.get('handoff').box)) return false;
  const expected = new Map(STARTER_EDGES.map(([from, port, to, route]) => [edgeKey(from, port, to), route ?? {}]));
  for (const link of links) {
    const key = edgeKey(link.edge.from, link.edge.port, link.edge.to);
    if (!expected.has(key)) return false;
    link.starterRoute = expected.get(key);
    expected.delete(key);
  }
  for (const [id, , x, y, labelSide] of STARTER_NODES) {
    const { box } = byId.get(id);
    Object.assign(box, { x, y });
    if (labelSide) box.labelSide = labelSide;
    if (id === 'handoff') box.w = 170;
  }
  return true;
}

function compareLinks(a, b) {
  return Number(a.failure) - Number(b.failure) || b.count - a.count || a.index - b.index;
}

// Follow recorded, non-failure ports first. Backtrack past a loop-only detour
// when a non-failure exit exists. For a closed cycle, use its first simple walk.
function markSpine(root) {
  const seen = new Set([root]);
  const stack = [{ node: root, index: 0 }];
  let fallback;
  while (stack.length) {
    const frame = stack.at(-1);
    const link = frame.node.out[frame.index++];
    if (!link || link.failure) {
      if (frame.index === 1) break;
      fallback ??= stack.slice();
      stack.pop();
    } else if (!link.to.component && !seen.has(link.to)) {
      seen.add(link.to);
      stack.push({ node: link.to, index: 0, via: link });
    }
  }
  for (const { node, via } of stack.length ? stack : fallback) {
    node.spine = true;
    if (via) {
      const { out } = via.from;
      out.splice(out.indexOf(via), 1);
      out.unshift(via);
    }
  }
}

function rankNodes(entry, records) {
  const rows = [];
  let component = 0;
  for (const root of [entry, ...records]) {
    if (!root || root.component) continue;
    markSpine(root);
    // DFS with the spine visited first. Removing edges to active ancestors
    // leaves a DAG; reverse finishing order ranks even joins by longest path.
    root.component = ++component;
    root.rank = rows.length ? rows.length + 1 : 0;
    root.active = true;
    const stack = [{ node: root, index: 0 }];
    const order = [];
    while (stack.length) {
      const frame = stack.at(-1);
      const link = frame.node.out[frame.index++];
      if (!link) {
        frame.node.active = false;
        order.push(frame.node);
        stack.pop();
      } else if (!link.to.active && (!link.to.component || link.to.component === component)) {
        link.forward = true;
        if (!link.to.parent || compareLinks(link, link.to.parent) < 0) link.to.parent = link;
        if (!link.to.component) {
          link.to.component = component;
          link.to.active = true;
          stack.push({ node: link.to, index: 0 });
        }
      }
    }
    for (const node of order.reverse()) {
      (rows[node.rank] ??= []).push(node);
      for (const link of node.out) {
        if (link.forward) link.to.rank = Math.max(link.to.rank, node.rank + 1);
      }
    }
  }
  return rows;
}

const exitText = exit => `⊘ ${exit.port}`;
const textWidth = (text, glyph) => String(text ?? '').length * glyph;

function placeLayers(entry, records) {
  const columns = new Map();
  rankNodes(entry, records).forEach((row, rank) => {
    const used = new Set([0]);
    // Establish continuations before allocating new branches in this row.
    const parentLane = node => node.parent?.from.lane ?? 0;
    row.sort((a, b) => Number(!parentLane(a)) - Number(!parentLane(b)) || a.index - b.index);
    for (const node of row) {
      const lane = node.spine ? 0 : freeLane(parentLane(node), used);
      used.add(lane);
      node.lane = lane;
      const { box } = node;
      box.y = rank * ROW;
      const half = box.type === 'condition' ? box.w * 0.62 : box.w / 2;
      let left = half, right = half;
      const exits = (box.exits ?? []).map(exitText);
      if (box.type === 'condition' || box.type === 'loop') {
        box.labelSide = lane < 0 ? 'left' : 'right';
        const labels = [box.id, box.sub, ...exits];
        const label = Math.max(...labels.map(line => textWidth(line, 6.7))) + 8;
        if (lane < 0) left += label;
        else right += label;
      } else if (exits.length) {
        right += Math.max(...exits.map(exit => textWidth(exit, 6.4))) + 7;
      }
      const column = columns.get(lane) ?? { left: 0, right: 0, nodes: [] };
      column.left = Math.max(column.left, left);
      column.right = Math.max(column.right, right);
      column.nodes.push(node);
      columns.set(lane, column);
    }
  });

  let x = 0;
  for (const lane of [...columns.keys()].sort((a, b) => a - b)) {
    const { left, right, nodes } = columns.get(lane);
    const center = x + left;
    for (const node of nodes) {
      node.box.x = center;
      node.extent = [center - left, center + right];
      node.gutter = [center - left - COLUMN_GAP / 2, center + right + COLUMN_GAP / 2];
    }
    x = center + right + COLUMN_GAP;
  }
}

function freeLane(origin, used) {
  if (!used.has(origin)) return origin;
  for (let step = 1; ; step++) {
    const first = origin < 0 ? origin + step : origin - step;
    const second = origin * 2 - first;
    if (!used.has(first)) return first;
    if (!used.has(second)) return second;
  }
}

function anchor(box, side, offset = 0) {
  const [nx, ny] = NORMAL[side];
  let reach = nx ? box.w / 2 : box.h / 2;
  if (box.type === 'condition') reach = box.w * 0.62 - Math.abs(offset);
  if (box.type === 'loop') reach = Math.sqrt(reach ** 2 - offset ** 2);
  return [box.x + (nx ? nx * reach : offset), box.y + (ny ? ny * reach : offset)];
}

function parallelOffset(box, side, fraction) {
  const span = (NORMAL[side][0] ? box.h : box.w) / 2;
  return fraction * Math.min(24, span * 0.65);
}

const offsetPoint = ([x, y], [nx, ny], distance) => [x + nx * distance, y + ny * distance];

function makeRoute(start, curves, tip, toSide, label, labelSide) {
  const normal = NORMAL[toSide];
  const base = offsetPoint(tip, normal, 7);
  const perpendicular = [normal[1], -normal[0]];
  curves.at(-1)[2] = base;
  return { start, curves, arrow: [tip, offsetPoint(base, perpendicular, 3.8), offsetPoint(base, perpendicular, -3.8)], label, labelSide };
}

function cubicRoute(a, b, hints = {}, fraction = 0, stagger = false) {
  const dx = b.x - a.x, dy = b.y - a.y;
  const [fs, ts] = Math.abs(dx) > Math.abs(dy)
    ? (dx > 0 ? ['right', 'left'] : ['left', 'right'])
    : (dy > 0 ? ['bottom', 'top'] : ['top', 'bottom']);
  const fromSide = hints.fromSide ?? fs, toSide = hints.toSide ?? ts;
  const start = anchor(a, fromSide, hints.fromOff ?? parallelOffset(a, fromSide, fraction));
  const tip = anchor(b, toSide, hints.toOff ?? parallelOffset(b, toSide, fraction));
  const x = tip[0] - start[0], y = tip[1] - start[1];
  const k = hints.k ?? Math.max(14, Math.min(90, Math.hypot(x, y) * 0.42));
  const c1 = offsetPoint(start, NORMAL[fromSide], k), c2 = offsetPoint(tip, NORMAL[toSide], k);
  const vertical = Math.abs(x) < Math.abs(y);
  const labelSide = hints.labelSide ?? (vertical ? (fraction < 0 ? 'left' : 'right') : (fraction > 0 ? 'below' : 'above'));
  const t = stagger ? 0.5 + fraction * 0.24 : 0.5, u = 1 - t;
  const label = start.map((v, i) => u ** 3 * v + 3 * u * u * t * c1[i] + 3 * u * t * t * c2[i] + t ** 3 * tip[i]);
  return makeRoute(start, [[c1, c2, tip]], tip, toSide, label, labelSide);
}

function layeredRoute({ from, to }, fraction, stagger) {
  const a = from.box, b = to.box;
  const dy = b.y - a.y;
  if (from === to) {
    const side = from.lane < 0 ? 'left' : 'right';
    const spread = Math.min(9, a.h * 0.28);
    const shift = fraction * Math.min(5, a.h * 0.12);
    return cubicRoute(a, a, {
      fromSide: side, toSide: side, fromOff: shift - spread, toOff: shift + spread, k: 54 + fraction * 20, labelSide: side,
    });
  }
  if (dy === ROW) {
    // Cross columns within the gap between the two rows, not through the nodes
    // beside either endpoint. Aligned nodes can use the usual control distance.
    const k = a.x === b.x ? undefined : (dy - (a.h + b.h) / 2) * 0.55;
    return cubicRoute(a, b, { fromSide: 'bottom', toSide: 'top', k }, fraction, stagger);
  }
  // Back edges and edges spanning multiple layers travel through a column
  // gutter. The horizontal parts stay in empty gaps between rows. Use
  // different tracks for ascending and descending edges between the same nodes.
  const direction = Math.sign(dy);
  const fs = direction > 0 ? 'bottom' : 'top', ts = direction > 0 ? 'top' : 'bottom';
  const right = a.x === b.x ? (direction > 0) === (from.lane >= 0) : b.x > a.x;
  const gutter = from.gutter[Number(right)] + direction * 8 + fraction * 12;
  const anchorFraction = direction > 0 ? fraction : (fraction + (right ? 2 : -2)) / 3;
  const start = anchor(a, fs, parallelOffset(a, fs, anchorFraction));
  const tip = anchor(b, ts, parallelOffset(b, ts, anchorFraction));
  const leave = [gutter, a.y + direction * ROW / 2];
  const enter = [gutter, b.y - direction * ROW / 2];
  const curves = [[[start[0], leave[1]], [gutter, leave[1] - direction * 12], leave]];
  if (leave[1] !== enter[1]) curves.push([leave, enter, enter]);
  curves.push([[gutter, enter[1] + direction * 12], [tip[0], enter[1]], tip]);
  const labelSide = gutter < a.x ? 'left' : 'right';
  return makeRoute(start, curves, tip, ts, [gutter, (leave[1] + enter[1]) / 2 + fraction * 10], labelSide);
}

function routeLinks(links, starter) {
  const bySource = new Map();
  for (const link of links) {
    if (!bySource.has(link.from)) bySource.set(link.from, new Map());
    const targets = bySource.get(link.from);
    if (!targets.has(link.to)) targets.set(link.to, []);
    targets.get(link.to).push(link);
  }
  for (const targets of bySource.values()) {
    for (const group of targets.values()) {
      group.forEach((link, i) => {
        const fraction = group.length === 1 ? 0 : i * 2 / (group.length - 1) - 1;
        link.route = starter ? cubicRoute(link.from.box, link.to.box, link.starterRoute) : layeredRoute(link, fraction, group.length > 2);
      });
    }
  }
}

function canvasBounds(records, links) {
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  const include = ([x, y]) => {
    minX = Math.min(minX, x); maxX = Math.max(maxX, x);
    minY = Math.min(minY, y); maxY = Math.max(maxY, y);
  };
  for (const { box, extent: [left, right] } of records) {
    const half = box.type === 'condition' ? box.h * 0.62 : box.h / 2;
    include([left, box.y - half]);
    include([right, box.y + half]);
  }
  for (const { edge, route: { start, curves, arrow, label: [x, y], labelSide } } of links) {
    include(start);
    curves.forEach(curve => curve.forEach(include));
    arrow.forEach(include);
    const title = edge.label ?? `${edge.port ?? ''}${edge.count > 1 ? ` ×${edge.count}` : ''}`;
    const w = String(title).length * 6.1 + 12, h = 16;
    const left = labelSide === 'left' ? x - w - 7 : labelSide === 'right' ? x + 7 : x - w / 2;
    const top = labelSide === 'above' ? y - h - 3 : labelSide === 'below' ? y + 4 : y - h / 2;
    include([left, top]);
    include([left + w, top + h]);
  }
  const pad = 16;
  return { dx: pad - Math.floor(minX), dy: pad - Math.floor(minY), width: Math.ceil(maxX) - Math.floor(minX) + pad * 2, height: Math.ceil(maxY) - Math.floor(minY) + pad * 2 };
}

const round = value => Math.round(value * 10) / 10;

function outputEdge({ edge, route }, dx, dy) {
  const translate = ([x, y]) => [round(x + dx), round(y + dy)];
  const point = p => translate(p).join(' ');
  const d = `M${point(route.start)} ${route.curves.map(curve => `C${curve.map(point).join(' ')}`).join(' ')}`;
  const arrow = route.arrow.map(p => translate(p).join(',')).join(' ');
  const [labelX, labelY] = translate(route.label);
  return { ...edge, d, arrow, labelX, labelY, labelSide: route.labelSide };
}

export function layoutWorkflowGraph(entry, nodes, edges) {
  if (!nodes.length) return { width: 0, height: 0, nodes: [], edges: [] };
  const records = nodes.map((node, index) => ({ box: sizeNode(node), index, rank: 0, out: [] }));
  const byId = new Map(records.map(record => [record.box.id, record]));
  const links = [];
  edges.forEach((edge, index) => {
    const from = byId.get(edge.from), to = byId.get(edge.to);
    if (!from || !to) return;
    const failure = edge.state === 'fail' || FAILURE_PORTS.has(edge.port) || isPausedEnd(to.box);
    const count = edge.count > 0 ? edge.count : Number(edge.state === 'taken');
    const link = { edge, from, to, failure, count, index };
    from.out.push(link);
    links.push(link);
  });

  const starter = placeStarter(entry, byId, links);
  if (!starter) {
    for (const { out } of records) out.sort(compareLinks);
    placeLayers(byId.get(entry), records);
  }
  routeLinks(links, starter);
  const { width, height, dx, dy } = starter ? { width: 540, height: 590, dx: 0, dy: 0 } : canvasBounds(records, links);
  return { width, height, nodes: records.map(({ box }) => ({ ...box, x: box.x + dx, y: box.y + dy })), edges: links.map(link => outputEdge(link, dx, dy)) };
}
