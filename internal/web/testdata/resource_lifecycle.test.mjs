import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

// Dependency-free DOM double: the lifecycle itself runs the SHIPPED Alpine
// x-data directive and Sortable, not a hand-called imitation of x-init cleanup.
const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = (file) => fs.readFileSync(path.join(web, file), 'utf8');

function harness() {
  const frames = new Map(), ticks = [], observers = [], errors = [];
  let frameID = 0;
  class Events {
    listeners = new Map();
    addEventListener(type, cb) {
      if (!this.listeners.has(type)) this.listeners.set(type, new Set());
      this.listeners.get(type).add(cb);
    }
    removeEventListener(type, cb) { this.listeners.get(type)?.delete(cb); }
    dispatchEvent(evt) {
      evt.target ||= this;
      for (const cb of [...(this.listeners.get(evt.type) || [])]) {
        if (typeof cb === 'function') cb(evt); else cb.handleEvent(evt);
      }
      return true;
    }
    count() { return [...this.listeners.values()].reduce((n, set) => n + set.size, 0); }
  }
  const document = new Events();
  class Element extends Events {
    nodeType = 1;
    style = {};
    children = [];
    parentNode = null;
    dataset = {};
    _attrs = new Map();
    scrollWidth = 100; clientWidth = 80; scrollLeft = 0;
    scrollHeight = 100; clientHeight = 80; scrollTop = 0;
    constructor(tag = 'div', attrs = {}) {
      super(); this.tagName = this.nodeName = tag.toUpperCase(); this.ownerDocument = document;
      for (const [k, v] of Object.entries(attrs)) this.setAttribute(k, v);
      const classes = new Set((attrs.class || '').split(/\s+/).filter(Boolean));
      this.classList = { add: (x) => classes.add(x), remove: (x) => classes.delete(x), contains: (x) => classes.has(x) };
    }
    get attributes() { return [...this._attrs].map(([name, value]) => ({ name, value })); }
    get parentElement() { return this.parentNode; }
    get firstElementChild() { return this.children[0] || null; }
    get firstChild() { return this.firstElementChild; }
    set innerHTML(value) { this.children.slice().forEach((c) => c.remove()); this._html = value; }
    get innerHTML() { return this._html || ''; }
    get nextElementSibling() { return this.parentNode?.children[this.parentNode.children.indexOf(this) + 1] || null; }
    get nextSibling() { return this.nextElementSibling; }
    get previousElementSibling() { return this.parentNode?.children[this.parentNode.children.indexOf(this) - 1] || null; }
    get isConnected() { return this === document.documentElement || !!this.parentNode?.isConnected; }
    setAttribute(k, v) {
      this._attrs.set(k, String(v)); if (k === 'id') this.id = v;
      if (k.startsWith('data-')) this.dataset[k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = String(v);
    }
    getAttribute(k) { return this._attrs.get(k) ?? null; }
    hasAttribute(k) { return this._attrs.has(k); }
    removeAttribute(k) { this._attrs.delete(k); }
    appendChild(n) { return this.insertBefore(n, null); }
    insertBefore(n, ref) {
      n.remove(); const i = ref ? this.children.indexOf(ref) : this.children.length;
      this.children.splice(i, 0, n); n.parentNode = this; return n;
    }
    removeChild(n) { n.remove(); return n; }
    after(n) { this.parentNode.insertBefore(n, this.nextSibling); }
    before(n) { this.parentNode.insertBefore(n, this); }
    remove() {
      if (!this.parentNode) return;
      const siblings = this.parentNode.children;
      siblings.splice(siblings.indexOf(this), 1); this.parentNode = null;
    }
    contains(n) { return this === n || this.children.some((c) => c.contains(n)); }
    matches(sel) {
      if (sel === '.epic-row:not(.epic-row-add)') return this.classList.contains('epic-row') && !this.classList.contains('epic-row-add');
      if (sel.includes(' ')) {
        const [ancestor, target] = sel.split(/\s+/);
        return this.matches(target) && !!this.parentNode?.closest(ancestor);
      }
      if (sel.startsWith('#')) return this.id === sel.slice(1);
      if (sel.startsWith('.')) return sel.slice(1).split('.').every((c) => this.classList.contains(c));
      if (sel.startsWith('[')) {
        const attr = sel.slice(1, -1);
        const equal = /^([^=]+)="([^"]*)"$/.exec(attr);
        if (equal) return this.getAttribute(equal[1]) === equal[2];
        if (attr.startsWith('data-')) return attr.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()) in this.dataset;
        return this.hasAttribute(attr);
      }
      return this.tagName.toLowerCase() === sel.toLowerCase();
    }
    closest(sel) { return this.matches(sel) ? this : this.parentNode?.closest(sel) || null; }
    querySelectorAll(sel) {
      const out = [];
      for (const c of this.children) { if (c.matches(sel)) out.push(c); out.push(...c.querySelectorAll(sel)); }
      return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    getElementsByTagName(tag) { return this.querySelectorAll(tag); }
    getBoundingClientRect() { return { top: 0, left: 0, right: 80, bottom: 80, width: 80, height: 80 }; }
    cloneNode(deep) {
      const n = new Element(this.tagName, Object.fromEntries(this._attrs));
      n.dataset = { ...this.dataset };
      if (deep) this.children.forEach((c) => n.appendChild(c.cloneNode(true)));
      if (this.content) n.content = this.content.cloneNode(deep);
      return n;
    }
  }
  document.documentElement = new Element('html');
  document.body = new Element('body'); document.documentElement.appendChild(document.body);
  document.createElement = (tag) => new Element(tag);
  document.importNode = (node, deep) => node.cloneNode(deep);
  document.querySelectorAll = (sel) => document.documentElement.querySelectorAll(sel);
  document.querySelector = (sel) => document.querySelectorAll(sel)[0] || null;
  document.getElementById = (id) => document.querySelector('#' + id);
  document.defaultView = { getComputedStyle: (el) => ({ ...el.style, display: 'block', position: 'static', transform: 'none', marginLeft: '0', marginRight: '0' }) };
  class Observer {
    constructor(cb) { this.cb = cb; this.connected = false; observers.push(this); }
    observe() { this.connected = true; }
    disconnect() { this.connected = false; }
    takeRecords() { return []; }
  }
  const window = new Events();
  Object.assign(window, { document, Element, navigator: { userAgent: '' }, getSelection: () => ({ removeAllRanges() {} }), innerWidth: 1200, innerHeight: 800 });
  const ctx = vm.createContext({
    window, document, Element, HTMLElement: Element, ShadowRoot: class {}, MutationObserver: Observer, ResizeObserver: Observer,
    navigator: window.navigator, console: { ...console, warn: (...args) => errors.push(args) },
    setTimeout, clearTimeout, setInterval, clearInterval,
    queueMicrotask: (cb) => ticks.push(cb),
    requestAnimationFrame: (cb) => { frames.set(++frameID, cb); return frameID; },
    cancelAnimationFrame: (id) => frames.delete(id),
    CustomEvent: class { constructor(type, options) { this.type = type; Object.assign(this, options); } },
  });
  window.CustomEvent = ctx.CustomEvent;
  vm.runInContext(source('static/vendor/sortablejs@1.15.6/Sortable.min.js'), ctx);
  vm.runInContext(source('static/vendor/alpinejs@3.14.8/cdn.min.js'), ctx);
  // Do not auto-start the document; initTree/destroyTree below exercise the
  // actual directives without depending on the double's mutation delivery.
  ticks.length = 0;
  ctx.Alpine = window.Alpine;
  ctx.Alpine.magic('nextTick', () => (cb) => ticks.push(cb));
  vm.runInContext(source('ui/board.js').replaceAll('export function ', 'function ') + '\n' + source('ui/epics.js').replace(/^import .*;\n/m, '').replaceAll('export function ', 'function '), ctx);
  const state = ctx.kontoraBoard();
  Object.assign(state, ctx.kontoraEpics(), {
    isMobile: false, _rendered: {}, _dirtyCols: {}, _boardInit: true,
    columns: [{ key: 'open', statuses: ['open'] }, { key: 'done', statuses: ['done'] }], paints: [], moves: [], orders: [], flushed: 0,
    renderColumn(key) { this.paints.push(key); },
    renderBoard() { this.columns.forEach((c) => this.renderColumn(c.key)); },
    flushTicketUpdates() { this.flushed++; this._boardDirty = false; },
    moveTask(...args) { this.moves.push(args); }, epicSaveOrder(ids) { this.orders.push(ids); },
  });
  const root = new Element('div', { 'x-data': 'state' });
  document.body.appendChild(root); ctx.state = state;
  ctx.Alpine.initTree(root);
  function mount(expr, dataset = {}, parent = root) {
    const el = new Element('div', { 'x-data': expr }); Object.assign(el.dataset, dataset);
    parent.appendChild(el); ctx.Alpine.initTree(el); return el;
  }
  function destroy(el) { ctx.Alpine.destroyTree(el); el.remove(); }
  function flush() { while (ticks.length) ticks.splice(0).forEach((cb) => cb()); }
  return { ctx, state, root, Element, document, window, mount, destroy, flush, frames, observers, errors,
    runFrames() { const batch = [...frames.values()]; frames.clear(); batch.forEach((cb) => cb()); },
  };
}

function start(h, list, card) {
  const s = h.ctx.Sortable.get(list);
  s._prepareDragStart({ target: card, clientX: 0, clientY: 0, type: 'mousedown', button: 0 }, null, card);
  s._onDragStart({ type: 'mousedown', clientX: 0, clientY: 0 }, true);
  s._dragStarted(true, { type: 'dragstart' });
  return s;
}

test('shipped Alpine x-data owns each board Sortable through repeated replacement', () => {
  const h = harness();
  const baseline = h.document.count();
  for (let i = 0; i < 40; i++) {
    const old = h.mount("boardSortableOwner('open')", { dropStatus: 'open' }); old.id = 'col-open';
    const raw = h.ctx.Sortable.get(old);
    assert.ok(raw);
    const replacement = h.mount("boardSortableOwner('open')", { dropStatus: 'open' }); replacement.id = 'col-open';
    const marker = h.state._rendered.open = { replacement: true };
    const paints = h.state.paints.length;
    h.destroy(old); h.ctx.Alpine.destroyTree(old); // idempotent
    assert.equal(h.state._rendered.open, marker, 'old teardown must not clear replacement cache');
    assert.equal(raw.el, null);
    assert.ok(h.ctx.Sortable.get(replacement), 'old owner must not look up replacement by id');
    h.flush();
    assert.equal(h.state.paints.length, paints + 1, 'only the live replacement paints on nextTick');
    h.destroy(replacement);
    assert.equal(h.document.count(), baseline, 'Sortable global plugin listeners are released');
  }
  assert.deepEqual(h.errors, []);
});

test('chosen-but-not-started teardown is idempotent and clears pending source handlers', () => {
  const h = harness();
  const baseline = h.document.count();
  const list = h.mount("boardSortableOwner('open')", { dropStatus: 'open' });
  const owner = h.ctx.Alpine.$data(list);
  const card = new h.Element(); card.dataset.ticketId = 'ticket'; list.appendChild(card);
  const raw = h.ctx.Sortable.get(list);
  raw._prepareDragStart({ target: card, clientX: 0, clientY: 0, type: 'mousedown', button: 0 }, null, card);
  assert.equal(card.classList.contains('sortable-chosen'), true);
  assert.equal(h.ctx.Sortable.active, undefined);
  h.destroy(list); owner.destroy(); owner.destroy();
  assert.equal(raw.el, null);
  assert.equal(card.classList.contains('sortable-chosen'), false);
  assert.equal(h.document.count(), baseline);
  assert.deepEqual(h.state.moves, []);
  assert.deepEqual(h.errors, []);
});

test('destroying a drag destination cancels without posting and releases source/global state', () => {
  const h = harness();
  const from = h.mount("boardSortableOwner('open')", { dropStatus: 'open' }); from.id = 'col-open';
  const to = h.mount("boardSortableOwner('done')", { dropStatus: 'done' }); to.id = 'col-done';
  const card = new h.Element(); card.dataset.ticketId = 'ticket'; from.appendChild(card);
  const s = start(h, from, card);
  assert.equal(h.state._dragging, true);
  h.state._renderHeld = true; h.state._boardDirty = true;
  to.appendChild(card);
  h.destroy(to);
  assert.equal(h.ctx.Sortable.active, null);
  assert.equal(h.ctx.Sortable.dragged, null);
  assert.equal(h.state._dragging, false);
  assert.equal(h.state._renderHeld, false);
  assert.equal(h.state.flushed, 1);
  assert.equal(card.classList.contains('sortable-chosen'), false);
  assert.equal(card.classList.contains('sortable-ghost'), false);
  assert.deepEqual(h.state.moves, []);
  assert.equal(h.state._dirtyCols.open, true);
  // A queued/stale end callback must remain harmless after cancellation.
  s.options.onEnd({ item: card, from, to });
  assert.deepEqual(h.state.moves, []);
  assert.ok(h.state.paints.includes('open'));
  h.destroy(from);
  assert.deepEqual(h.errors, []);
});

test('overflow owners cancel frames, observers and global listeners for both axes', () => {
  for (const axis of ['x', 'y']) {
    const h = harness();
    const baseline = h.window.count();
    for (let i = 0; i < 40; i++) {
      const list = new h.Element('div', { 'x-ref': 'colList', 'x-data': "boardSortableOwner('open')" });
      list.dataset.dropStatus = 'open';
      const owner = new h.Element('div', { 'x-data': `boardOverflowOwner('${axis}')` });
      owner.appendChild(list);
      h.root.appendChild(owner); h.ctx.Alpine.initTree(owner);
      assert.equal(h.frames.size, 1);
      h.runFrames();
      const data = h.ctx.Alpine.$data(owner);
      assert.equal(axis === 'x' ? data.hasOverflowRight : data.overflowBottom, true);
      h.window.dispatchEvent({ type: 'resize' });
      h.window.dispatchEvent({ type: 'resize' });
      assert.equal(h.frames.size, 1, 'bursts coalesce into one measure');
      const resourceObservers = h.observers.filter((o) => o.connected);
      const pending = [...h.frames.values()];
      h.destroy(owner); h.ctx.Alpine.destroyTree(owner);
      assert.equal(h.frames.size, 0);
      assert.equal(h.window.count(), baseline);
      assert.ok(resourceObservers.every((o) => !o.connected));
      // Even a callback already delivered by the browser cannot reschedule or
      // read geometry on the detached list after destroy.
      const measured = axis === 'x' ? owner : list;
      Object.defineProperty(measured, axis === 'x' ? 'scrollWidth' : 'scrollHeight', { get() { throw new Error('detached measure'); } });
      pending.forEach((cb) => cb()); resourceObservers.forEach((o) => o.cb());
      assert.equal(h.frames.size, 0);
    }
    assert.deepEqual(h.errors, []);
  }
});

test('epic cancellation restores surviving x-for rows from current keyed state, never saves order', () => {
  const h = harness();
  const from = h.mount('epicSortableOwner()', { epicGroup: 'open' });
  const to = h.mount('epicSortableOwner()', { epicGroup: 'done' });
  const template = new h.Element('template'); from.appendChild(template);
  const otherTemplate = new h.Element('template'); to.appendChild(otherTemplate);
  const card = new h.Element(); card.classList.add('epic-row'); card.dataset.childId = 'child'; from.appendChild(card);
  template._x_prevKeys = ['child']; template._x_lookup = { child: card };
  otherTemplate._x_prevKeys = []; otherTemplate._x_lookup = {};
  const s = start(h, from, card);
  assert.equal(h.state.epicDragging, true);
  to.appendChild(card);
  h.destroy(to);
  assert.equal(h.state.epicDragging, false);
  assert.equal(card.parentNode, from);
  assert.equal(template.nextElementSibling, card);
  s.options.onEnd({ item: card, from, to });
  assert.deepEqual(h.state.moves, []);
  assert.deepEqual(h.state.orders, []);
  h.destroy(from);
  assert.deepEqual(h.errors, []);
});

test('normal epic reorder reads the drop order then restores Alpine-owned DOM', () => {
  const h = harness();
  const list = h.mount('epicSortableOwner()', { epicGroup: 'open' });
  const template = new h.Element('template'); list.appendChild(template);
  const a = new h.Element(), b = new h.Element();
  for (const [row, id] of [[a, 'a'], [b, 'b']]) {
    row.classList.add('epic-row'); row.dataset.childId = id; list.appendChild(row);
  }
  template._x_prevKeys = ['a', 'b']; template._x_lookup = { a, b };
  const s = start(h, list, a);
  list.insertBefore(b, a);
  s._onDrop({ type: 'mouseup', cancelable: false, preventDefault() {}, stopPropagation() {} });
  assert.deepEqual(h.state.orders.map((ids) => [...ids]), [['b', 'a']]);
  assert.equal(template.nextElementSibling, a);
  assert.equal(a.nextElementSibling, b);
  assert.equal(h.state.epicDragging, false);
  assert.deepEqual(h.state.moves, []);
  h.destroy(list);
  assert.deepEqual(h.errors, []);
});

test('epic cancellation reconciles actual Alpine x-for rows including updates during drag', () => {
  const h = harness();
  const data = h.ctx.Alpine.$data(h.root);
  data.rows = [{ id: 'child' }];
  const from = new h.Element('div', { 'x-data': 'epicSortableOwner()', 'data-epic-group': 'open' });
  const template = new h.Element('template', { 'x-for': 'r in rows', ':key': 'r.id' });
  template.content = new h.Element('fragment');
  template.content.appendChild(new h.Element('div', { class: 'epic-row', ':data-child-id': 'r.id' }));
  from.appendChild(template); h.root.appendChild(from); h.ctx.Alpine.initTree(from);
  const to = h.mount('epicSortableOwner()', { epicGroup: 'done' });
  const otherTemplate = new h.Element('template'); to.appendChild(otherTemplate);
  otherTemplate._x_prevKeys = []; otherTemplate._x_lookup = {};
  const row = template._x_lookup.child;
  start(h, from, row); to.appendChild(row);
  data.rows = [{ id: 'child' }, { id: 'arrived-during-drag' }]; h.flush();
  h.destroy(to);
  assert.equal(template._x_lookup.child, row, 'preserves Alpine row identity');
  assert.deepEqual(from.children.filter((c) => c.dataset.childId).map((c) => c.dataset.childId), ['child', 'arrived-during-drag']);
  assert.equal(h.state.epicDragging, false);
  assert.deepEqual(h.state.moves, []); assert.deepEqual(h.state.orders, []);
  h.destroy(from);
  assert.deepEqual(h.errors, []);
});

for (const expression of ["boardSortableOwner('open')", 'epicSortableOwner()']) {
test(`shipped Alpine x-if remounts ${expression} without retaining resources`, () => {
  const h = harness();
  const data = h.ctx.Alpine.$data(h.root);
  data.shown = true;
  const gate = new h.Element('template', { 'x-if': 'shown' });
  gate.content = new h.Element('fragment');
  gate.content.appendChild(new h.Element('div', { 'x-data': expression }));
  h.root.appendChild(gate);
  const baseline = h.document.count();
  h.ctx.Alpine.initTree(gate);
  for (let i = 0; i < 40; i++) {
    const list = gate._x_currentIfEl;
    const raw = h.ctx.Sortable.get(list);
    assert.ok(raw);
    data.shown = false; h.flush();
    assert.equal(raw.el, null);
    assert.equal(list.isConnected, false);
    assert.equal(h.document.count(), baseline);
    data.shown = true; h.flush();
    assert.notEqual(gate._x_currentIfEl, list);
  }
  h.destroy(gate);
  assert.equal(h.document.count(), baseline);
  assert.deepEqual(h.errors, []);
});
}

test('cancel source or destination reconciles surviving board DOM from canonical buckets', () => {
  for (const removed of ['source', 'destination']) {
    const h = harness();
    const from = h.mount("boardSortableOwner('open')", { dropStatus: 'open' }); from.id = 'col-open';
    const to = h.mount("boardSortableOwner('done')", { dropStatus: 'done' }); to.id = 'col-done';
    function card(id) { const n = new h.Element(); n.dataset.ticketId = id; return n; }
    const moving = card('moving'), stay = card('stay');
    from.appendChild(moving); to.appendChild(stay);
    const original = h.ctx.kontoraBoard();
    Object.assign(h.state, {
      renderColumn: original.renderColumn,
      _board: { open: [{ id: 'moving' }], done: [{ id: 'stay' }] },
      _rendered: {
        open: { empty: false, ids: ['moving'], sigs: { moving: 'sig' } },
        done: { empty: false, ids: ['stay'], sigs: { stay: 'sig' } },
      },
      isCollapsed: () => false, ticketKey: (t) => t.id,
      _cardSig: () => 'sig', _cardNode: (t) => card(t.id),
    });
    const s = start(h, from, moving);
    to.appendChild(moving);
    // Simulate a detached old layer: cancellation must repair only the live
    // destination/source and not rely on the old node still being in document.
    const gone = removed === 'source' ? from : to;
    gone.remove(); h.ctx.Alpine.destroyTree(gone);
    const surviving = removed === 'source' ? to : from;
    assert.deepEqual(surviving.children.map((c) => c.dataset.ticketId), removed === 'source' ? ['stay'] : ['moving']);
    assert.equal(h.state._dragging, false);
    assert.equal(h.ctx.Sortable.ghost, null);
    assert.equal(h.ctx.Sortable.clone, null);
    assert.equal(h.document.body.querySelector('.sortable-fallback'), null);
    s.options.onEnd({ item: moving, from, to });
    assert.deepEqual(h.state.moves, []);
    h.destroy(surviving);
    assert.deepEqual(h.errors, []);
  }
});

test('normal board drop still posts exactly once after a cancelled drag', () => {
  const h = harness();
  const from = h.mount("boardSortableOwner('open')", { dropStatus: 'open' });
  const to = h.mount("boardSortableOwner('done')", { dropStatus: 'done' });
  const card = new h.Element(); card.dataset.ticketId = 'ticket'; from.appendChild(card);
  start(h, from, card);
  h.destroy(to);
  const replacement = h.mount("boardSortableOwner('done')", { dropStatus: 'done' });
  const s = start(h, from, card);
  replacement.appendChild(card);
  s._onDrop({ type: 'mouseup', cancelable: false, preventDefault() {}, stopPropagation() {} });
  assert.deepEqual(h.state.moves, [['ticket', 'done']]);
  assert.equal(h.state._dragging, false);
  h.destroy(from); h.destroy(replacement);
  assert.deepEqual(h.errors, []);
});

test('markup uses x-data lifecycle owners, never disposer-returning x-init', () => {
  const html = source('static/index.html');
  assert.equal((html.match(/x-data="boardSortableOwner\(col.key\)"/g) || []).length, 2);
  assert.match(html, /x-data="epicSortableOwner\(\)"/);
  assert.match(html, /x-data="boardOverflowOwner\('x'\)"/);
  assert.match(html, /x-data="boardOverflowOwner\('y'\)"/);
  assert.doesNotMatch(html, /x-init="(?:epicInitSortable|initSortable)\(/);
});
