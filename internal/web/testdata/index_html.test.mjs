import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const dirname = path.dirname(fileURLToPath(import.meta.url));
const htmlPath = path.join(dirname, "../static/index.html");
const appPath = path.join(dirname, "../static/app.js");

// recomputeBoard builds its column buckets with array literals created inside
// the VM realm, so their prototype differs from this file's Array and
// deepStrictEqual's prototype check would fail. Spread into the test realm
// before asserting.
const bt = (state, key) => [...state.boardTickets(key)];

// Minimal DOM double for the board: enough for the keyed reconcile in
// renderColumn / _patchColumn, and it records every structural operation so a
// test can assert that an update touched only the cards that changed.
// Card nodes carry a uid so a test can tell "moved" from "rebuilt".
function fakeBoard(colKeys) {
  const ops = [];
  const els = {};
  let seq = 0;

  function detach(n) {
    if (!n.parent) return;
    const i = n.parent.children.indexOf(n);
    if (i >= 0) n.parent.children.splice(i, 1);
    n.parent = null;
  }

  function node(id) {
    return {
      dataset: { ticketId: id },
      uid: ++seq,
      parent: null,
      remove() {
        ops.push(`remove:${id}`);
        detach(this);
      },
    };
  }

  function element(key) {
    const el = {
      children: [],
      _html: "",
      get firstChild() {
        return this.children[0] || null;
      },
      get innerHTML() {
        return this._html;
      },
      // The real innerHTML parses the card markup into nodes; the stub picks the
      // card roots out by their data-ticket-id so both render paths agree.
      set innerHTML(v) {
        ops.push(`innerHTML:${key}`);
        this._html = v;
        this.children.forEach((c) => { c.parent = null; });
        this.children = [...v.matchAll(/data-ticket-id="([^"]+)"/g)].map((m) => {
          const n = node(m[1]);
          n.parent = el;
          return n;
        });
      },
      insertBefore(n, ref) {
        ops.push(`insert:${n.dataset.ticketId}`);
        detach(n);
        const at = ref ? this.children.indexOf(ref) : -1;
        this.children.splice(at < 0 ? this.children.length : at, 0, n);
        n.parent = el;
        return n;
      },
      replaceChild(fresh, old) {
        ops.push(`replace:${fresh.dataset.ticketId}`);
        detach(fresh);
        const at = this.children.indexOf(old);
        this.children[at] = fresh;
        fresh.parent = el;
        old.parent = null;
        return old;
      },
    };
    return el;
  }

  function boardCols() {
    return { listeners: [], addEventListener(type) { this.listeners.push(type); } };
  }

  function build() {
    colKeys.forEach((k) => { els[`col-${k}`] = element(k); });
    els["board-cols"] = boardCols();
  }
  build();

  const documentEvents = [];

  // _toggleCardMenu appends the menu inside its card, so the menu is in the
  // document exactly as long as that card node is still in a column.
  let openMenu = null;
  function menuIsLive() {
    return !!openMenu && colKeys.some((k) => els[`col-${k}`].children.includes(openMenu.host));
  }

  return {
    ops,
    els,
    node,
    documentEvents,
    // Alpine rebuilds the whole layer when the breakpoint flips: fresh, empty
    // column elements and a fresh delegation root.
    remount() { build(); },
    // Attach a menu to the card that renders ticket `id` in column `key`.
    openMenuOn(key, id) {
      openMenu = { host: els[`col-${key}`].children.find((c) => c.dataset.ticketId === id) };
      assert.ok(openMenu.host, `no card for ${id} in ${key}`);
    },
    menuIsLive,
    document: {
      getElementById(id) { return els[id] || null; },
      querySelector(sel) { return sel.includes(".card-menu") && menuIsLive() ? openMenu : null; },
      querySelectorAll() { return []; },
      addEventListener(type) { documentEvents.push(type); },
      documentElement: { style: {} },
    },
    // Card ids currently in a column, in DOM order.
    ids(key) { return els[`col-${key}`].children.map((c) => c.dataset.ticketId); },
    uids(key) { return els[`col-${key}`].children.map((c) => c.uid); },
  };
}

// Board wired to the fake DOM, rendered once. Returns the harness plus the list
// of ticket ids each later render rebuilt through _cardNode.
function renderedBoard(tickets, overrides = {}) {
  const board = fakeBoard(["open", "in_progress", "human_review", "done", "cancelled"]);
  const state = loadKontoraState({ document: board.document, ...overrides });
  const built = [];
  state.tickets = tickets;
  state._boardInit = true;
  state.recomputeBoard();
  state._cardNode = (t) => {
    built.push(t.id);
    return board.node(t.id);
  };
  board.ops.length = 0;
  return { board, state, built };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function loadKontoraState(overrides = {}) {
  return loadKontoraContext(overrides).state;
}

// The terminal handles live on a module-scope `termState` in app.js rather than
// on the component, so a lifecycle test needs the VM context to reach them. A
// top-level `var` lands on the context object, so `ctx.termState` is the holder.
function loadKontoraContext(overrides = {}) {
  const context = kontoraContext(overrides);
  vm.createContext(context);
  vm.runInContext(`${fs.readFileSync(appPath, "utf8")}\nthis.kontora = kontora;`, context);
  return { ctx: context, state: context.kontora() };
}

const flushMicrotasks = () => new Promise((resolve) => setTimeout(resolve, 0));

// Doubles for the vendored xterm modules, installed on termState so the
// lifecycle runs without the dynamic import(). Each records what the terminal
// lifecycle did to it, so a test can tell reuse from a rebuild.
function stubXterm(termState) {
  const created = [];

  termState.Terminal = class {
    constructor(options) {
      this.options = options;
      this.cols = 80;
      this.rows = 24;
      this.unicode = {};
      this.element = { clientWidth: 800, clientHeight: 400, parentNode: null };
      this.resets = 0;
      this.disposed = false;
      created.push(this);
    }
    loadAddon() {}
    open(container) {
      this.element.parentNode = container;
    }
    reset() {
      this.resets += 1;
    }
    write() {}
    onData() {
      return { dispose() {} };
    }
    scrollToBottom() {}
    dispose() {
      this.disposed = true;
    }
  };
  termState.Terminal.created = created;

  termState.FitAddon = class {
    constructor() {
      this.fits = 0;
    }
    fit() {
      this.fits += 1;
    }
  };
  termState.Unicode11Addon = class {};
  termState.WebglAddon = class {
    constructor() {
      this.disposed = false;
    }
    onContextLoss(handler) {
      this._handler = handler;
    }
    // Stands in for the browser dropping the GPU context.
    loseContext() {
      this._handler();
    }
    dispose() {
      this.disposed = true;
    }
  };
}

// A component with a terminal open and streaming, on the real openTerminal path.
async function liveTerminal() {
  const { ctx, state } = await openedTerminal();
  assert.ok(ctx.termState.ws, "stream did not connect");
  return { ctx, state };
}

// liveTerminal without the streaming half, so a test can hold back the frame
// _connectTerminal defers the connect to.
async function openedTerminal(overrides = {}) {
  const container = { textContent: "" };
  const { ctx, state } = loadKontoraContext({
    document: {
      getElementById(id) {
        return id === "terminal-container" ? container : null;
      },
      querySelector() {
        return null;
      },
      hasFocus() {
        return true;
      },
      documentElement: { style: {} },
    },
    WebSocket: class {
      static OPEN = 1;
      constructor(url) {
        this.url = url;
        this.readyState = 1;
        this.closed = false;
      }
      send() {}
      close() {
        this.closed = true;
        this.readyState = 3;
      }
    },
    ...overrides,
  });

  stubXterm(ctx.termState);
  state.selectedTicket = { id: "tst-001", status: "in_progress" };
  state.activeTab = "terminal";
  state.$nextTick = (callback) => {
    if (callback) callback();
    return Promise.resolve();
  };
  state.startEditing = () => {};
  state.flushEditSave = () => {};
  state.recomputeBoard = () => {};

  await state.openTerminal();
  assert.ok(ctx.termState.term, "terminal did not open");
  return { ctx, state };
}

function kontoraContext(overrides = {}) {
  const context = {
    console,
    setTimeout,
    clearTimeout,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    localStorage: {
      getItem() {
        return null;
      },
      setItem() {},
    },
    // _cssVar and _getTerminalTheme call the bare global, not window's.
    getComputedStyle() {
      return { getPropertyValue: () => "" };
    },
    getStoredTheme() {
      return null;
    },
    setStoredTheme() {},
    applyTheme() {},
    window: {
      innerWidth: 1200,
      addEventListener() {},
      getComputedStyle() {
        return {};
      },
    },
    document: {
      getElementById() {
        return null;
      },
      querySelector() {
        return null;
      },
      hasFocus() {
        return true;
      },
      documentElement: {
        style: {},
      },
    },
    navigator: {
      clipboard: {
        writeText() {},
      },
    },
    fetch: async () => ({ ok: false, json: async () => ({}) }),
    EventSource: class {},
    DOMPurify: {
      sanitize(value) {
        return value;
      },
    },
    marked: {
      parse(value) {
        return value;
      },
    },
    WebSocket: class {},
    location: {
      protocol: "http:",
      host: "localhost:8080",
    },
  };

  Object.assign(context, overrides);
  return context;
}

// The vendored marked bundle, loaded through its CommonJS branch. The built-in
// harness stub only has parse, and bodyBlockOffsets needs the real lexer.
function realMarked() {
  const src = fs.readFileSync(path.join(dirname, "../static/vendor/marked@15.0.7/marked.min.js"), "utf8");
  const ctx = { module: { exports: {} } };
  ctx.exports = ctx.module.exports;
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  return ctx.module.exports;
}

// Deterministic geometry for the anchoring tests. One scroller holds either the
// rendered prose (fixed-height blocks) or the textarea, both at a known offset
// inside the scrolled content. Line wrapping is off: a textarea line is a "\n".
const LINE = 20;
const PAD = 8;
const BORDER = 1;

function fakeBodyDom({ body, blockHeights = [], proseTop = 500, editorTop = 500, scrollTop = 0, scrollHeight = 2000, clientHeight = 400 }) {
  const scroller = {
    clientTop: 2,
    clientHeight,
    scrollHeight,
    scrollTop,
    parentElement: null,
    getBoundingClientRect: () => ({ top: 0 }),
  };
  // Viewport top of an element sitting docTop into the scrolled content.
  const rectAt = (docTop) => () => ({ top: scroller.clientTop - scroller.scrollTop + docTop });
  const wrapper = { parentElement: scroller };

  let top = proseTop;
  const children = blockHeights.map((h) => {
    const child = { parentElement: null, getBoundingClientRect: rectAt(top) };
    top += h;
    return child;
  });
  const prose = { parentElement: wrapper, children, getBoundingClientRect: rectAt(proseTop) };
  children.forEach((c) => { c.parentElement = prose; });

  const textarea = {
    parentElement: wrapper,
    value: body,
    style: { height: "" },
    selectionStart: 0,
    selectionEnd: 0,
    focusOptions: null,
    getBoundingClientRect: rectAt(editorTop),
    get clientHeight() {
      const h = parseFloat(this.style.height);
      return Number.isNaN(h) ? 0 : Math.max(0, h);
    },
    get scrollHeight() {
      return Math.max(this.clientHeight, this.value.split("\n").length * LINE + 2 * PAD);
    },
    focus(options) { this.focusOptions = options; },
    setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; },
  };

  const window = {
    innerWidth: 1200,
    addEventListener() {},
    getComputedStyle(el) {
      if (el === scroller) return { overflowY: "auto" };
      if (el === textarea) return { paddingTop: `${PAD}px`, paddingBottom: `${PAD}px`, borderTopWidth: `${BORDER}px` };
      return { overflowY: "visible" };
    },
  };

  return { scroller, prose, textarea, window };
}

// State wired to that geometry, with the real lexer and the refs the transition
// methods read.
function bodyEditState(dom, body) {
  const state = loadKontoraState({ window: dom.window, marked: realMarked() });
  state.editing = true;
  state.selectedTicket = { id: "kon-1", status: "todo", body };
  state.editForm = { body, pipeline: "", path: "", agent: "", branch: "" };
  state.$refs = { bodyPreview: dom.prose, bodyEditor: null };
  state.saveEdit = () => {};
  return state;
}

test("kontora inline app initializes in a minimal VM context", () => {
  const state = loadKontoraState();

  assert.equal(typeof state.openTerminal, "function");
  assert.equal(typeof state.reconnectTerminal, "function");
  assert.equal(state.panelWidth, 430);
});

test("index.html loads the external app script", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /<script src="\/app\.js"><\/script>/);
});

test("openTerminal cancels stale startup after teardown", async () => {
  const { ctx, state } = loadKontoraContext();
  const nextTick = deferred();
  const connectCalls = [];

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.$nextTick = () => nextTick.promise;
  ctx.termState.Terminal = class {};
  ctx.termState.FitAddon = class {};
  state._connectTerminal = (seq) => {
    connectCalls.push(seq);
  };

  const openPromise = state.openTerminal();
  assert.equal(state.terminalOpen, true);
  assert.equal(state._terminalOpening, true);

  state.closeTerminal();

  nextTick.resolve();
  await openPromise;

  assert.deepEqual(connectCalls, []);
});

test("openTerminal clears terminalOpen if the tab changes before startup completes", async () => {
  const { ctx, state } = loadKontoraContext();
  const nextTick = deferred();
  const connectCalls = [];
  let closeCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.$nextTick = () => nextTick.promise;
  ctx.termState.Terminal = class {};
  ctx.termState.FitAddon = class {};
  state._connectTerminal = (seq) => {
    connectCalls.push(seq);
  };
  state.closeTerminal = () => {
    closeCalls += 1;
    state.terminalOpen = false;
  };

  const openPromise = state.openTerminal();
  state.activeTab = "ticket";

  nextTick.resolve();
  await openPromise;

  assert.equal(state.terminalOpen, false);
  assert.equal(state._terminalOpening, false);
  assert.equal(closeCalls, 1);
  assert.deepEqual(connectCalls, []);
});

test("reconnectTerminal does nothing while the terminal is already opening", () => {
  const state = loadKontoraState();
  let closeCalls = 0;
  let openCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.terminalOpen = true;
  state._terminalOpening = true;
  state.closeTerminal = () => {
    closeCalls += 1;
  };
  state.openTerminal = () => {
    openCalls += 1;
  };

  state.reconnectTerminal();

  assert.equal(closeCalls, 0);
  assert.equal(openCalls, 0);
});

test("reconnectTerminal tears down and reopens once when transport is ready", () => {
  const state = loadKontoraState();
  let closeCalls = 0;
  let openCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.terminalOpen = true;
  state._terminalOpening = false;
  state.closeTerminal = () => {
    closeCalls += 1;
  };
  state.openTerminal = () => {
    openCalls += 1;
  };

  state.reconnectTerminal();

  assert.equal(closeCalls, 1);
  assert.equal(openCalls, 1);
});

test("the component holds no xterm handles", () => {
  const state = loadKontoraState();

  // Anything reachable through the component is wrapped in Alpine's reactive
  // Proxy, which taxes every property read inside xterm.
  for (const key of [
    "_term",
    "_termWs",
    "_fitAddon",
    "_webglAddon",
    "_termInputDisposable",
    "_resizeObserver",
    "_resizeTimer",
    "_TerminalClass",
    "_FitAddonClass",
    "_Unicode11AddonClass",
    "_WebglAddonClass",
  ]) {
    assert.equal(key in state, false, `${key} must stay off the Alpine component`);
  }
});

test("switching away from the terminal tab and back keeps one Terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const created = ctx.termState.Terminal.created;

  for (let i = 0; i < 10; i++) {
    state.switchTab("ticket");
    state.switchTab("terminal");
  }

  assert.equal(ctx.termState.Terminal.created.length, created.length, "no extra Terminal built");
  assert.equal(ctx.termState.term, term);
  assert.equal(term.disposed, false);
  assert.equal(ctx.termState.webgl.disposed, false, "the WebGL context survives");
});

test("selecting another ticket reuses the terminal after reset", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const firstWs = ctx.termState.ws;

  await state.selectTicket({ id: "tst-002", status: "in_progress" });
  await flushMicrotasks();

  assert.equal(ctx.termState.term, term, "same Terminal reused");
  assert.equal(term.disposed, false);
  assert.equal(term.resets, 1, "previous ticket's output cleared");
  assert.equal(firstWs.closed, true, "previous socket closed");
  assert.notEqual(ctx.termState.ws, firstWs);
  assert.match(ctx.termState.ws.url, /\/ws\/terminal\/tst-002\?/);
});

test("closing the detail panel drops the stream and keeps the terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const ws = ctx.termState.ws;

  state.closeDetail();

  assert.equal(ws.closed, true, "no viewer session left running for a hidden ticket");
  assert.equal(ctx.termState.ws, null);
  assert.equal(ctx.termState.term, term);
  assert.equal(term.disposed, false);
});

test("toggling read-write reconnects the stream but keeps the terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const webgl = ctx.termState.webgl;
  const firstWs = ctx.termState.ws;

  state.toggleTerminalRW();

  assert.equal(ctx.termState.term, term);
  assert.equal(ctx.termState.webgl, webgl, "same WebGL context");
  assert.equal(term.disposed, false);
  assert.equal(firstWs.closed, true);
  assert.match(ctx.termState.ws.url, /rw=1/);
  assert.equal(term.options.cursorBlink, true, "cursor follows the mode");
});

test("crossing the breakpoint rebuilds the terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  state._mountBoard = () => {};

  state._onBreakpointChange();

  assert.equal(term.disposed, true, "the container's layout layer is gone");
  assert.equal(ctx.termState.term, null);
  assert.equal(ctx.termState.webgl, null);
});

test("a lost webgl context is retried once before falling back", async () => {
  const { ctx } = await liveTerminal();
  const first = ctx.termState.webgl;

  first.loseContext();

  const second = ctx.termState.webgl;
  assert.equal(first.disposed, true);
  assert.notEqual(second, first);
  assert.ok(second, "one fresh WebglAddon is attempted");

  second.loseContext();

  assert.equal(second.disposed, true);
  assert.equal(ctx.termState.webgl, null, "second loss falls back to the DOM renderer");
});

test("the cursor stops blinking when the next stream is read-only", async () => {
  const { ctx, state } = await liveTerminal();
  state.toggleTerminalRW();
  assert.equal(ctx.termState.term.options.cursorBlink, true);

  // selectTicket resets terminalRW, and the reused terminal keeps the cursor it
  // was built with unless the reconnect resyncs it.
  await state.selectTicket({ id: "tst-002", status: "in_progress" });
  await flushMicrotasks();

  assert.equal(state.terminalRW, false);
  assert.equal(ctx.termState.term.options.cursorBlink, false, "cursor follows the mode");
});

test("a read-write toggle racing the first connect leaves one socket", async () => {
  // _connectTerminal builds the terminal now and connects a frame later, so the
  // read-write button is live for a frame before the first socket exists.
  const frames = [];
  const { ctx, state } = await openedTerminal({
    requestAnimationFrame(callback) {
      frames.push(callback);
      return frames.length;
    },
  });
  assert.equal(ctx.termState.ws, null, "the connect is still queued");

  state.toggleTerminalRW();
  const raced = ctx.termState.ws;
  assert.ok(raced, "the toggle opened a stream of its own");

  frames.forEach((frame) => frame());

  assert.equal(raced.closed, true, "no orphaned kontora-view session");
  assert.notEqual(ctx.termState.ws, raced);
  assert.equal(ctx.termState.ws.closed, false);
});

test("a terminal with no container closes instead of stranding the tab", async () => {
  const { ctx, state } = await liveTerminal();
  state.closeTerminal();
  ctx.document.getElementById = () => null;

  await state.openTerminal();

  assert.equal(state.terminalOpen, false, "switchTab can retry the open");
  assert.equal(ctx.termState.ws, null);
});

test("refitTerminal skips a container it cannot measure", async () => {
  const { ctx, state } = await liveTerminal();
  const fitsWhileVisible = ctx.termState.fit.fits;

  ctx.termState.term.element.clientWidth = 0;
  state.refitTerminal();

  assert.equal(ctx.termState.fit.fits, fitsWhileVisible, "no reflow against a hidden container");
});

test("board columns include non-Kontora tickets in their status columns", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "ext-001", title: "External", status: "todo", kontora: false },
    { id: "kon-001", title: "Kontora", status: "todo", kontora: true },
  ];

  const ids = state.ticketsByStatuses("todo").map(t => t.id);

  assert.deepEqual(ids.sort(), ["ext-001", "kon-001"]);
});

test("non-Kontora start and resume actions open initialization instead of posting", async () => {
  const state = loadKontoraState();
  const opened = [];
  state.tickets = [
    { id: "ext-run", title: "Run", status: "open", kontora: false },
    { id: "ext-retry", title: "Retry", status: "paused", kontora: false },
  ];
  state.openInitModal = (ticket) => { opened.push(ticket.id); };

  await state.moveTicketVia("ext-run", "run", null);
  await state.moveTicketVia("ext-retry", "retry", null);
  await state.moveTask("ext-retry", "todo");

  assert.deepEqual(opened, ["ext-run", "ext-retry", "ext-retry"]);
});

test("human_review column sorts by the last stage's finish, newest first", () => {
  const state = loadKontoraState();
  state.tickets = [
    {
      id: "rev-old",
      title: "Old",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T07:00:00Z",
      // Edited after review started: a later mtime must not win.
      updated_at: "2026-05-19T20:00:00Z",
      history: [
        { stage: "plan", completed_at: "2026-05-19T08:00:00Z" },
        { stage: "code", completed_at: "2026-05-19T09:00:00Z" },
      ],
    },
    {
      id: "rev-new",
      title: "New",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T06:00:00Z",
      updated_at: "2026-05-19T12:00:00Z",
      history: [{ stage: "code", completed_at: "2026-05-19T11:00:00Z" }],
    },
  ];

  const ids = state.ticketsByStatuses("human_review").map(t => t.id);

  assert.deepEqual(ids, ["rev-new", "rev-old"]);
});

test("human_review column falls back to updated_at, then created_at, without a finished stage", () => {
  const state = loadKontoraState();
  state.tickets = [
    {
      id: "rev-moved",
      title: "Moved by hand",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T08:00:00Z",
      updated_at: "2026-05-19T11:00:00Z",
    },
    {
      id: "rev-no-update",
      title: "NoUpdate",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T12:00:00Z",
    },
    {
      id: "rev-running-stage",
      title: "Stage still open",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T09:00:00Z",
      updated_at: "2026-05-19T09:30:00Z",
      history: [{ stage: "code" }],
    },
  ];

  const ids = state.ticketsByStatuses("human_review").map(t => t.id);

  assert.deepEqual(ids, ["rev-no-update", "rev-moved", "rev-running-stage"]);
});

test("reviewFinishedAt only reports a finish time for tickets waiting on review", () => {
  const state = loadKontoraState();
  const history = [{ stage: "code", completed_at: "2026-05-19T09:00:00Z" }];

  assert.equal(state.reviewFinishedAt({ status: "human_review", history }), "2026-05-19T09:00:00Z");
  assert.equal(state.reviewFinishedAt({ status: "done", history }), "");
  assert.equal(state.reviewFinishedAt({ status: "todo", created_at: "2026-05-19T08:00:00Z" }), "");
});

test("non-review columns ignore updated_at and keep existing sort", () => {
  const state = loadKontoraState();
  state.tickets = [
    {
      id: "todo-recent-update",
      title: "Recent",
      status: "todo",
      kontora: true,
      created_at: "2026-05-19T08:00:00Z",
      updated_at: "2026-05-19T20:00:00Z",
    },
    {
      id: "todo-newer-created",
      title: "Newer",
      status: "todo",
      kontora: true,
      created_at: "2026-05-19T10:00:00Z",
      updated_at: "2026-05-19T11:00:00Z",
    },
  ];

  const ids = state.ticketsByStatuses("todo").map(t => t.id);

  assert.deepEqual(ids, ["todo-newer-created", "todo-recent-update"]);
});

test("applyTicketUpdate removes a ticket that becomes archived", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [
    { id: "kon-001", title: "Done", status: "done", kontora: true },
    { id: "kon-002", title: "Todo", status: "todo", kontora: true },
  ];

  state.applyTicketUpdate({ id: "kon-001", title: "Done", status: "archived", kontora: true });

  assert.deepEqual(state.tickets.map(t => t.id), ["kon-002"]);
});

test("applyTicketUpdate closes the detail panel when the selected ticket is archived", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [{ id: "kon-001", title: "Done", status: "done", kontora: true }];
  state.selectedTicket = { id: "kon-001", title: "Done", status: "done" };

  state.applyTicketUpdate({ id: "kon-001", title: "Done", status: "archived", kontora: true });

  assert.equal(state.selectedTicket, null);
  assert.deepEqual(state.tickets, []);
});

test("applyTicketUpdate keeps non-archived updates on the board", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [{ id: "kon-001", title: "Todo", status: "todo", kontora: true }];

  state.applyTicketUpdate({ id: "kon-001", title: "Todo", status: "paused", kontora: true });

  assert.equal(state.tickets.length, 1);
  assert.equal(state.tickets[0].status, "paused");
});

test("agent running counts come from the recompute pass and skip non-running tickets", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "kon-001", agent: "a1", status: "in_progress", kontora: true },
    { id: "kon-002", agent: "a1", status: "in_progress", kontora: true },
    { id: "ext-001", agent: "a1", status: "in_progress", kontora: false },
    { id: "kon-003", agent: "a1", status: "done", kontora: true },
    { id: "kon-004", status: "in_progress", kontora: true },
    { id: "kon-005", agent: "a2", status: "in_progress", kontora: true },
  ];

  state.recomputeBoard();

  assert.equal(state.agentRunningCount("a1"), 2);
  assert.equal(state.agentRunningCount("a2"), 1);
  assert.equal(state.agentRunningCount("missing"), 0);
  assert.equal(state.agentRunningCount(""), 0);
  assert.equal(state.agentRunningCount(undefined), 0);
});

test("columns and agents named after Object.prototype members stay data", () => {
  // Column keys come from config custom_statuses and agent names from ticket
  // data, so both key maps have to carry no prototype.
  assert.equal(loadKontoraState().agentRunningCount("constructor"), 0);

  const board = fakeBoard(["open", "in_progress", "constructor", "human_review", "done", "cancelled"]);
  const state = loadKontoraState({ document: board.document });
  state.configCache = { custom_statuses: ["constructor"] };
  state.tickets = [
    { id: "kon-a", title: "A", status: "constructor", kontora: true, agent: "constructor", created_at: "2026-05-19T08:00:00Z" },
    { id: "kon-b", title: "B", status: "in_progress", kontora: true, agent: "constructor", created_at: "2026-05-19T09:00:00Z" },
  ];
  state._boardInit = true;
  state.recomputeBoard();

  assert.deepEqual(board.ids("constructor"), ["kon-a"]);
  assert.equal(state.agentRunningCount("constructor"), 1);

  // The cached render state has to be readable back, not a function.
  board.ops.length = 0;
  state.recomputeBoard();
  assert.deepEqual(board.ops, []);
});

test("selectTicket refreshes the agent counts after replacing a ticket", async () => {
  const full = { id: "kon-001", title: "One", status: "in_progress", agent: "a2", kontora: true };
  const state = loadKontoraState({
    fetch: async () => ({ ok: true, json: async () => full }),
  });
  state.openTerminal = () => {};
  state.tickets = [{ id: "kon-001", title: "One", status: "in_progress", agent: "a1", kontora: true }];
  state.recomputeBoard();
  assert.equal(state.agentRunningCount("a1"), 1);

  await state.selectTicket(state.tickets[0]);

  assert.equal(state.agentRunningCount("a1"), 0);
  assert.equal(state.agentRunningCount("a2"), 1);
});

test("recomputeBoard caches sorted+filtered lists keyed by column", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
    { id: "kon-b", title: "B", status: "todo", kontora: true, created_at: "2026-05-19T10:00:00Z" },
    { id: "kon-c", title: "C", status: "human_review", kontora: true, updated_at: "2026-05-19T09:00:00Z" },
  ];

  state.recomputeBoard();

  // In Progress column groups todo/in_progress/paused and sorts newest first.
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-b", "kon-a"]);
  assert.deepEqual(bt(state, "human_review").map(t => t.id), ["kon-c"]);
  assert.equal(bt(state, "open").length, 0);
  assert.equal(state.filteredTicketCount(), 3);
});

test("recomputeBoard applies the search query to the cached board", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "kon-alpha", title: "Alpha", status: "todo", kontora: true },
    { id: "kon-beta", title: "Beta", status: "todo", kontora: true },
  ];
  state.searchQuery = "alpha";

  state.recomputeBoard();

  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-alpha"]);
  assert.equal(state.filteredTicketCount(), 1);
});

test("queueTicketUpdate coalesces a burst into a single recompute", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  let recomputes = 0;
  const realRecompute = state.recomputeBoard.bind(state);
  state.recomputeBoard = () => { recomputes += 1; realRecompute(); };
  state.tickets = [];

  // Pretend a frame is already scheduled so queued updates only buffer.
  state._boardRaf = 1;
  state.queueTicketUpdate({ id: "kon-1", title: "One", status: "todo", kontora: true });
  state.queueTicketUpdate({ id: "kon-2", title: "Two", status: "todo", kontora: true });

  assert.equal(state._pendingTicketUpdates.length, 2);
  assert.equal(recomputes, 0);

  state.flushTicketUpdates();

  assert.equal(recomputes, 1);
  assert.equal(state._pendingTicketUpdates.length, 0);
  assert.equal(state._boardRaf, null);
  assert.deepEqual(bt(state, "in_progress").map(t => t.id).sort(), ["kon-1", "kon-2"]);
});

test("queueTicketUpdate flushes the buffer and refreshes the cached board", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [];

  // The harness runs requestAnimationFrame synchronously, so this flushes now.
  state.queueTicketUpdate({ id: "kon-1", title: "One", status: "human_review", kontora: true, updated_at: "2026-05-19T09:00:00Z" });

  assert.deepEqual(bt(state, "human_review").map(t => t.id), ["kon-1"]);
  assert.equal(state._pendingTicketUpdates.length, 0);
});

test("moveTask re-buckets the cached board optimistically and reverts on failure", async () => {
  // Default harness fetch returns ok:false, so the move request fails.
  const state = loadKontoraState();
  state.tickets = [{ id: "kon-1", title: "One", status: "todo", kontora: true }];
  state.recomputeBoard();
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-1"]);

  // Optimistic move reflects in the cache before the request resolves.
  const pending = state.moveTask("kon-1", "human_review");
  assert.equal(bt(state, "in_progress").length, 0);
  assert.deepEqual(bt(state, "human_review").map(t => t.id), ["kon-1"]);

  // The failed request reverts the optimistic change in the cache too.
  await pending;
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-1"]);
  assert.equal(bt(state, "human_review").length, 0);
});

test("deleteSelectedTicket drops the card from the cached board on success", async () => {
  const state = loadKontoraState({
    fetch: async () => ({ ok: true, json: async () => ({}) }),
  });
  state.updateFavicon = () => {};
  state.tickets = [
    { id: "kon-1", title: "One", status: "todo", kontora: true },
    { id: "kon-2", title: "Two", status: "todo", kontora: true },
  ];
  state.selectedTicket = { id: "kon-1" };
  state.recomputeBoard();
  assert.deepEqual(bt(state, "in_progress").map(t => t.id).sort(), ["kon-1", "kon-2"]);

  await state.deleteSelectedTicket();

  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-2"]);
  assert.equal(state.filteredTicketCount(), 1);
});

test("moveTicketVia re-buckets the cached board after a status change", async () => {
  const state = loadKontoraState({
    fetch: async () => ({
      ok: true,
      json: async () => ({ id: "kon-1", title: "One", status: "human_review", kontora: true, updated_at: "2026-05-19T10:00:00Z" }),
    }),
  });
  state.tickets = [{ id: "kon-1", title: "One", status: "in_progress", kontora: true }];
  state.recomputeBoard();
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-1"]);

  await state.moveTicketVia("kon-1", "move", { status: "human_review" });

  assert.equal(bt(state, "in_progress").length, 0);
  assert.deepEqual(bt(state, "human_review").map(t => t.id), ["kon-1"]);
});

test("detailMoves drops the bespoke Pause/Resume buttons but keeps the rest", () => {
  const state = loadKontoraState();

  // in_progress: the panel renders a tooltip-bearing Pause button itself, so the
  // validMoves "pause" entry must not be duplicated.
  const inProgress = state.detailMoves({ status: "in_progress" });
  assert.equal(inProgress.some(mv => mv.endpoint === "pause"), false);
  assert.deepEqual([...inProgress.map(mv => mv.label)], ["Send to review", "Mark done", "Cancel"]);

  // paused: same for the bespoke Resume (retry) button.
  const paused = state.detailMoves({ status: "paused" });
  assert.equal(paused.some(mv => mv.endpoint === "retry"), false);
  assert.deepEqual([...paused.map(mv => mv.label)], ["Mark done", "Cancel"]);
});

test("detailMoves exposes the per-status actions that only lived in the card menu", () => {
  const state = loadKontoraState();

  // open gets Queue (run); human_review gets Approve (move->done) and Send back.
  assert.deepEqual(
    [...state.detailMoves({ status: "open" }).map(mv => `${mv.label}:${mv.endpoint}`)],
    ["Queue:run", "Cancel:move"],
  );

  const review = state.detailMoves({ status: "human_review" });
  const approve = review.find(mv => mv.label === "Approve");
  assert.equal(approve.endpoint, "move");
  assert.equal(approve.status, "done");
  assert.equal(review.some(mv => mv.label === "Send back" && mv.endpoint === "retry"), true);

  assert.deepEqual([...state.detailMoves(null)], []);
});

test("formatDuration and timeAgo advance with the reactive clock", () => {
  const state = loadKontoraState();
  const started = "2026-05-19T10:00:00Z";
  const base = new Date(started).getTime();

  state.now = base + 5 * 60000;
  assert.equal(state.formatDuration({ started_at: started }), "5m");
  assert.equal(state.timeAgo(started), "5m");

  // Advancing the clock alone re-renders the duration, no SSE event needed.
  state.now = base + 70 * 60000;
  assert.equal(state.formatDuration({ started_at: started }), "1h 10m");
  assert.equal(state.timeAgo(started), "1h");
});

test("_escapeHtml escapes the five HTML-significant characters", () => {
  const state = loadKontoraState();

  assert.equal(state._escapeHtml('<b>'), "&lt;b&gt;");
  assert.equal(state._escapeHtml(`a & "b" 'c'`), "a &amp; &quot;b&quot; &#39;c&#39;");
  assert.equal(state._escapeHtml(null), "");
  assert.equal(state._escapeHtml(undefined), "");
});

test("_cardHTML neutralizes HTML in interpolated text (no XSS)", () => {
  const state = loadKontoraState();
  const html = state._cardHTML(
    { id: "sta-1", title: "<img src=x onerror=alert(1)>", status: "todo", created_at: "2026-05-19T08:00:00Z", kontora: true, pipeline: "kontora" },
    { key: "open" },
  );

  assert.equal(html.includes("<img src=x"), false);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
});

test("_cardHTML renders an in-progress card with selection, glyph, bars, and a live duration span", () => {
  const state = loadKontoraState();
  state.selectedTicket = { id: "sta-2" };
  const html = state._cardHTML(
    {
      id: "sta-2", title: "Run it", status: "in_progress", started_at: "2026-05-19T10:00:00Z",
      stage: "code", agent: "claude", kontora: true, pipeline: "kontora", stages: ["plan", "code"], attempt: 2,
    },
    { key: "in_progress" },
  );

  assert.match(html, /class="[^"]*\bis-selected\b/);
  assert.match(html, /class="[^"]*\bcard-state-running\b/);
  assert.match(html, /card-glyph-running/);
  // Single-track progress bar: stage 2 of 2 renders as (1 + 0.5) / 2 = 75%.
  assert.match(html, /class="card-progress"/);
  assert.match(html, /width:75%/);
  assert.match(html, /data-since="2026-05-19T10:00:00Z"/);
  assert.match(html, /retry 2/);
  assert.match(html, /data-ticket-id="sta-2"/);
  assert.match(html, /data-pipe-color="[a-z]+"/);
});

test("_cardHTML shows the finish time on review cards", () => {
  const state = loadKontoraState();
  state.now = new Date("2026-05-19T11:00:00Z").getTime();
  const html = state._cardHTML(
    {
      id: "sta-rev", title: "Check it", status: "human_review", kontora: true, pipeline: "kontora",
      created_at: "2026-05-19T06:00:00Z", updated_at: "2026-05-19T10:30:00Z",
      history: [{ stage: "code", completed_at: "2026-05-19T09:00:00Z" }],
    },
    { key: "human_review" },
  );

  assert.match(html, /data-finished="2026-05-19T09:00:00Z"/);
  assert.match(html, /finished 2h ago/);
  assert.equal(/data-ago=/.test(html), false);
});

test("finishedAgo drops the trailing ago for a fresh finish", () => {
  const state = loadKontoraState();
  state.now = new Date("2026-05-19T09:00:20Z").getTime();

  assert.equal(state.finishedAgo("2026-05-19T09:00:00Z"), "finished just now");
  assert.equal(state.finishedAgo(""), "");
});

test("_cardHTML uses data-ago for non-running cards and omits is-selected when unselected", () => {
  const state = loadKontoraState();
  state.selectedTicket = null;
  const html = state._cardHTML(
    { id: "sta-3", title: "Plain", status: "todo", created_at: "2026-05-19T08:00:00Z", kontora: true, pipeline: "kontora" },
    { key: "in_progress" },
  );

  assert.equal(/\bis-selected\b/.test(html), false);
  assert.match(html, /data-ago="2026-05-19T08:00:00Z"/);
  assert.match(html, /◌/); // todo glyph in the in_progress column
});

test("_cardHTML ships the menu button but no menu", () => {
  const state = loadKontoraState();
  const html = state._cardHTML(
    { id: "sta-4", title: "T", status: "todo", created_at: "2026-05-19T08:00:00Z", kontora: true },
    { key: "in_progress" },
  );

  assert.match(html, /class="card-menu-btn/);
  assert.match(html, /aria-label="More actions"/);
  assert.match(html, /aria-expanded="false"/);
  // The menu itself is built on demand by _toggleCardMenu.
  assert.equal(html.includes("card-menu-item"), false);
  assert.equal(/class="card-menu /.test(html), false);
  // Text glyph instead of a three-circle SVG (4 fewer nodes per card).
  assert.equal(html.includes("<svg"), false);
});

test("_cardMenuHTML encodes menu actions and the Initialize entry", () => {
  const state = loadKontoraState();

  // Kontora todo: move actions carry endpoint + target status.
  const todo = state._cardMenuHTML({ id: "sta-4", title: "T", status: "todo", kontora: true });
  assert.match(todo, /class="card-menu /);
  assert.match(todo, /data-act="move" data-status="open"/);
  assert.match(todo, /data-act="move" data-status="cancelled"/);

  // Open ticket: Queue maps to the run endpoint with no status.
  assert.match(state._cardMenuHTML({ id: "sta-5", title: "O", status: "open", kontora: true }), /data-act="run"/);

  // Non-kontora ticket: Initialize action is present.
  assert.match(state._cardMenuHTML({ id: "sta-6", title: "Imported", status: "open", kontora: false, path: "/x/proj" }), /data-act="init"/);
  // Kontora ticket: it is not.
  assert.equal(state._cardMenuHTML({ id: "sta-5", title: "O", status: "open", kontora: true }).includes('data-act="init"'), false);

  // Unknown (custom) status with no valid moves: fallback label.
  assert.match(state._cardMenuHTML({ id: "sta-8", title: "C", status: "review", kontora: true }), /No actions available/);
});

test("_cardHTML keeps the not-a-kontora-ticket badge off open tickets", () => {
  const state = loadKontoraState();

  const ext = state._cardHTML(
    { id: "sta-6", title: "Imported", status: "open", created_at: "2026-05-19T08:00:00Z", kontora: false, path: "/x/proj" },
    { key: "open" },
  );
  assert.equal(ext.includes("not a kontora ticket"), false);

  const extTodo = state._cardHTML(
    { id: "sta-7", title: "Imported2", status: "todo", created_at: "2026-05-19T08:00:00Z", kontora: false, path: "/x/proj" },
    { key: "in_progress" },
  );
  assert.match(extTodo, /not a kontora ticket/);
});

test("card menus are built on open and removed on close", () => {
  const menus = [];
  const cards = {};
  function fakeCard(id) {
    const btnWrap = { children: [], appendChild(n) { this.children.push(n); n.parent = this; } };
    const btn = { parentElement: btnWrap, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } };
    return {
      dataset: { ticketId: id },
      classes: new Set(),
      classList: {
        add(c) { cards[id].classes.add(c); },
        remove(c) { cards[id].classes.delete(c); },
      },
      querySelector(sel) { return sel === ".card-menu-btn" ? btn : null; },
      btn,
      btnWrap,
    };
  }
  cards["kon-1"] = fakeCard("kon-1");
  cards["kon-2"] = fakeCard("kon-2");

  const state = loadKontoraState({
    document: {
      getElementById() { return null; },
      querySelector() { return null; },
      querySelectorAll(sel) {
        if (sel.includes(".kt-card.menu-open")) return Object.values(cards).filter((c) => c.classes.has("menu-open"));
        if (sel.includes(".card-menu")) return menus.filter((m) => m.attached);
        return [];
      },
      documentElement: { style: {} },
    },
  });
  state._nodeFromHTML = (html) => {
    const menu = { html, attached: true, parent: null, remove() { this.attached = false; } };
    menus.push(menu);
    return menu;
  };
  state.tickets = [
    { id: "kon-1", title: "One", status: "todo", kontora: true },
    { id: "kon-2", title: "Two", status: "open", kontora: false },
  ];

  state._toggleCardMenu(cards["kon-1"]);
  assert.equal(state._openMenuId, "kon-1");
  assert.equal(menus.filter((m) => m.attached).length, 1);
  assert.deepEqual(cards["kon-1"].btnWrap.children, [menus[0]]);
  assert.match(menus[0].html, /data-act="move" data-status="open"/);
  assert.equal(menus[0].html.includes('data-act="init"'), false);
  assert.equal(cards["kon-1"].btn.attrs["aria-expanded"], "true");

  // Opening another card's menu removes the first one.
  state._toggleCardMenu(cards["kon-2"]);
  assert.equal(state._openMenuId, "kon-2");
  assert.equal(menus[0].attached, false);
  assert.equal(menus.filter((m) => m.attached).length, 1);
  assert.match(menus[1].html, /data-act="init"/);   // non-kontora gets Initialize
  assert.equal(cards["kon-1"].btn.attrs["aria-expanded"], "false");
  assert.equal(cards["kon-1"].classes.has("menu-open"), false);

  // Toggling the open card closes it and leaves no menu behind.
  state._toggleCardMenu(cards["kon-2"]);
  assert.equal(state._openMenuId, null);
  assert.equal(menus.filter((m) => m.attached).length, 0);
  assert.equal(cards["kon-2"].classes.has("menu-open"), false);
  assert.equal(cards["kon-2"].btn.attrs["aria-expanded"], "false");
});

test("_emptyStateHTML keeps the .empty-state class for Sortable's filter", () => {
  const state = loadKontoraState();
  const html = state._emptyStateHTML({ key: "open", emptyText: "Nothing Here" });

  assert.match(html, /class="empty-state/);
  assert.match(html, /∅ nothing here/);
});

test("index.html error banner uses the --err token, not raw red palette classes", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.equal(/red-400|red-500/.test(html), false);
  assert.match(html, /border-err/);
  assert.match(html, /text-err\/80/);
});

test("index.html drops the stats bar but keeps the matched counter", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.equal(/uppercase tracking-widest[^"]*">\s*stats/i.test(html), false);
  assert.match(html, /filteredTicketCount\(\)[^<]*<\/span>\s*matched/);
});

test("index.html detail header carries the status chip and pipeline tag", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /class="status-chip[^"]*"[^>]*:data-status="selectedTicket\?\.status"/);
  assert.match(html, /class="pipe-tag shrink-0[^"]*"/);
});

test("parseTitleTag extracts a [tag] title prefix and falls back to the project basename", () => {
  const state = loadKontoraState();

  assert.deepEqual({ ...state.parseTitleTag({ title: "[api] Add retry backoff", path: "/x/proj" }) },
    { tag: "api", rest: "Add retry backoff" });
  assert.deepEqual({ ...state.parseTitleTag({ title: "No prefix here", path: "/x/proj" }) },
    { tag: "proj", rest: "No prefix here" });
  assert.deepEqual({ ...state.parseTitleTag({ title: "No prefix" }) },
    { tag: null, rest: "No prefix" });
});

test("cancelled column starts collapsed and toggling persists the set", () => {
  const stored = {};
  const state = loadKontoraState({
    localStorage: {
      getItem(k) { return Object.prototype.hasOwnProperty.call(stored, k) ? stored[k] : null; },
      setItem(k, v) { stored[k] = String(v); },
    },
  });
  state.$nextTick = (fn) => { if (fn) fn(); };

  assert.equal(state.isCollapsed("cancelled"), true);
  assert.equal(state.isCollapsed("open"), false);

  state.toggleColumnCollapsed("cancelled");
  assert.equal(state.isCollapsed("cancelled"), false);
  assert.equal(stored["kontora-collapsed-cols"], "[]");

  state.toggleColumnCollapsed("done");
  assert.equal(stored["kontora-collapsed-cols"], JSON.stringify(["done"]));
});

test("renderColumn fills expanded columns but clears collapsed rails", () => {
  const board = fakeBoard(["open", "cancelled"]);
  const state = loadKontoraState({ document: board.document });
  state.tickets = [
    { id: "kon-open", title: "O", status: "open", kontora: true },
    { id: "kon-cxl", title: "C", status: "cancelled", kontora: true },
  ];
  state.recomputeBoard();
  // A card Sortable dropped onto the collapsed rail.
  board.els["col-cancelled"].innerHTML = '<div data-ticket-id="kon-cxl"></div>';

  state.renderColumn("open");
  assert.match(board.els["col-open"].innerHTML, /data-ticket-id="kon-open"/);
  assert.deepEqual(board.ids("open"), ["kon-open"]);
  const open = state._rendered["open"];
  assert.equal(open.empty, false);
  assert.deepEqual([...open.ids], ["kon-open"]);
  assert.deepEqual({ ...open.sigs }, { "kon-open": state._cardSig(state.tickets[0], { key: "open" }) });

  // Collapsed rail: any node Sortable dropped in is removed, no cards rendered.
  state.renderColumn("cancelled");
  assert.equal(board.els["col-cancelled"].innerHTML, "");
  assert.deepEqual(board.ids("cancelled"), []);
  const cancelled = state._rendered["cancelled"];
  assert.equal(cancelled.empty, true);
  assert.deepEqual([...cancelled.ids], []);
  assert.deepEqual({ ...cancelled.sigs }, {});
});

test("_cardSig changes for every field _cardHTML renders", () => {
  const state = loadKontoraState();
  const base = {
    id: "sig-1", title: "Base", status: "in_progress", stage: "code", pipeline: "kontora",
    path: "/x/proj", agent: "claude", attempt: 0, kontora: true,
    started_at: "2026-05-19T10:00:00Z", created_at: "2026-05-19T08:00:00Z", stages: ["plan", "code"],
  };
  const col = { key: "in_progress" };
  const sig = state._cardSig(base, col);

  const cases = [
    { name: "col.key", col: { key: "done" } },
    { name: "id", ticket: { id: "sig-2" } },
    { name: "title", ticket: { title: "Changed" } },
    { name: "status", ticket: { status: "paused" } },
    { name: "stage", ticket: { stage: "review" } },
    { name: "pipeline", ticket: { pipeline: "other" } },
    { name: "path", ticket: { path: "/x/other" } },
    { name: "agent", ticket: { agent: "codex" } },
    { name: "attempt", ticket: { attempt: 2 } },
    { name: "kontora", ticket: { kontora: false } },
    { name: "stages", ticket: { stages: ["plan", "code", "commit"] } },
    { name: "started_at", ticket: { started_at: "2026-05-19T11:00:00Z" } },
    { name: "created_at", ticket: { created_at: "2026-05-19T09:00:00Z" } },

    { name: "showPipelineBadges", toggle: "showPipelineBadges" },
    { name: "showAgentMeta", toggle: "showAgentMeta" },
  ];

  for (const c of cases) {
    if (c.toggle) state[c.toggle] = !state[c.toggle];
    const got = state._cardSig({ ...base, ...(c.ticket || {}) }, c.col || col);
    if (c.toggle) state[c.toggle] = !state[c.toggle];
    assert.notEqual(got, sig, `${c.name} must change the card signature`);
  }
});

test("_cardSig follows the review finish time", () => {
  const state = loadKontoraState();
  const base = {
    id: "sig-rev", title: "Review", status: "human_review", kontora: true,
    created_at: "2026-05-19T08:00:00Z", updated_at: "2026-05-19T10:00:00Z",
    history: [{ stage: "code", completed_at: "2026-05-19T09:00:00Z" }],
  };
  const col = { key: "human_review" };
  const sig = state._cardSig(base, col);

  const relanded = { ...base, history: [...base.history, { stage: "code", completed_at: "2026-05-19T12:00:00Z" }] };
  assert.notEqual(state._cardSig(relanded, col), sig);

  // A plain edit to the markdown bumps the mtime but no card text, so the
  // signature must not change and the card must not be rebuilt.
  assert.equal(state._cardSig({ ...base, updated_at: "2026-05-19T20:00:00Z" }, col), sig);
});

test("_cardSig ignores the reactive clock and the selection highlight", () => {
  const state = loadKontoraState();
  const ticket = { id: "sig-3", title: "T", status: "in_progress", started_at: "2026-05-19T10:00:00Z", kontora: true };
  const col = { key: "in_progress" };

  state.now = new Date("2026-05-19T10:05:00Z").getTime();
  const early = state._cardSig(ticket, col);
  state.now = new Date("2026-05-19T12:00:00Z").getTime();
  assert.equal(state._cardSig(ticket, col), early);

  // Selection is a class toggle (_markSelectedCard), not a rebuild.
  state.selectedTicket = { id: "sig-3" };
  assert.equal(state._cardSig(ticket, col), early);
});

// Pull one method body out of app.js. Methods sit at four-space indent and close
// with "    }," at the same indent, so the slice needs no brace counting.
function methodBody(source, name) {
  const start = source.indexOf(`\n    ${name}(`);
  assert.notEqual(start, -1, `${name} is not a method in app.js`);
  const end = source.indexOf("\n    },", start);
  assert.notEqual(end, -1, `${name} has no closing brace at method indent`);
  return source.slice(start, end);
}

// Everything _cardHTML reads, following the helpers that take the whole ticket.
// Returns ticket fields and plain state reads (this.foo, not this.foo(...) —
// those are followed instead).
function cardRenderReads() {
  const source = fs.readFileSync(appPath, "utf8");
  const fields = new Set();
  const stateReads = new Set();
  const seen = new Set();
  const queue = ["_cardHTML"];
  while (queue.length) {
    const name = queue.shift();
    if (seen.has(name)) continue;
    seen.add(name);
    const body = methodBody(source, name);
    for (const m of body.matchAll(/(?<![\w$])ticket\.([A-Za-z_$][\w$]*)/g)) fields.add(m[1]);
    for (const m of body.matchAll(/this\.([A-Za-z_$][\w$]*)\(ticket\)/g)) queue.push(m[1]);
    for (const m of body.matchAll(/this\.([A-Za-z_$][\w$]*)\b(?!\s*\()/g)) stateReads.add(m[1]);
  }
  return { fields, stateReads, source };
}

// The forward test above proves each signature field matters. This is the other
// direction, and it is the one that goes stale: a field added to _cardHTML but
// not to _cardSig leaves cards showing old data until something else in the
// column changes. Deriving the read set from the source keeps the check honest
// when the markup grows.
test("_cardSig covers every ticket field _cardHTML reads", () => {
  const { fields, source } = cardRenderReads();
  const sig = methodBody(source, "_cardSig");

  // Sanity: the scan found the helpers, not just _cardHTML's own reads.
  assert.ok(fields.has("pipeline"), "expected ticketTagLabel/ticketPipeColor to be followed");
  assert.ok(fields.has("path"), "expected parseTitleTag to be followed");
  assert.ok(fields.size >= 10, `expected a full field set, got ${[...fields].join(", ")}`);

  const missing = [...fields].filter((f) => !sig.includes(`ticket.${f}`));
  assert.deepEqual(missing, [], `_cardSig must include: ${missing.join(", ")}`);
});

test("_cardHTML reads no board state beyond the two the signature tracks", () => {
  const { stateReads, source } = cardRenderReads();
  const sig = methodBody(source, "_cardSig");

  // now: the 30s clock, patched in place by _updateCardTimers.
  // selectedTicket: a class toggle applied by _markSelectedCard.
  const patchedInPlace = new Set(["now", "selectedTicket"]);
  const untracked = [...stateReads].filter((s) => !patchedInPlace.has(s) && !sig.includes(`this.${s}`));
  assert.deepEqual(untracked, [], `_cardSig must track, or _updateCardTimers must patch: ${untracked.join(", ")}`);

  // Sanity: the scan sees the display toggles, so an empty result means covered.
  assert.ok(stateReads.has("showAgentMeta") && stateReads.has("showPipelineBadges"));
});

test("one changed ticket patches one card and leaves the rest of the board alone", () => {
  const { board, state, built } = renderedBoard([
    { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
    { id: "kon-b", title: "B", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
    { id: "kon-c", title: "C", status: "todo", kontora: true, created_at: "2026-05-19T10:00:00Z" },
  ]);
  const before = board.uids("in_progress");
  assert.deepEqual(board.ids("in_progress"), ["kon-c", "kon-b", "kon-a"]);

  state.tickets[1].title = "B renamed";
  state.recomputeBoard();

  assert.deepEqual(built, ["kon-b"]);
  assert.deepEqual(board.ops, ["replace:kon-b"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-c", "kon-b", "kon-a"]);
  // The two untouched cards keep their nodes; only the changed one is new.
  const after = board.uids("in_progress");
  assert.equal(after[0], before[0]);
  assert.equal(after[2], before[2]);
  assert.notEqual(after[1], before[1]);
});

test("a clock tick alone patches nothing", () => {
  const { board, state, built } = renderedBoard([
    { id: "kon-a", title: "A", status: "in_progress", kontora: true, started_at: "2026-05-19T08:00:00Z" },
    { id: "kon-b", title: "B", status: "human_review", kontora: true, updated_at: "2026-05-19T09:00:00Z" },
  ]);

  state.now += 30000;
  state.recomputeBoard();

  assert.deepEqual(built, []);
  assert.deepEqual(board.ops, []);
});

test("a reordered card is moved, not rebuilt", () => {
  const rev = (id, finish) => ({
    id, title: id.toUpperCase(), status: "human_review", kontora: true,
    created_at: "2026-05-19T06:00:00Z", history: [{ stage: "code", completed_at: finish }],
  });
  const { board, state, built } = renderedBoard([
    rev("rev-a", "2026-05-19T09:00:00Z"),
    rev("rev-b", "2026-05-19T08:00:00Z"),
    rev("rev-c", "2026-05-19T07:00:00Z"),
  ]);
  const before = board.uids("human_review");
  assert.deepEqual(board.ids("human_review"), ["rev-a", "rev-b", "rev-c"]);

  // A corrected finish on rev-a drops it to the bottom. Only rev-a's own card
  // text changed, so rev-b and rev-c are moved with their nodes intact.
  state.tickets[0].history = [{ stage: "code", completed_at: "2026-05-19T06:30:00Z" }];
  state.recomputeBoard();

  assert.deepEqual(built, ["rev-a"]);
  assert.deepEqual(board.ops, ["insert:rev-b", "insert:rev-c", "replace:rev-a"]);
  assert.deepEqual(board.ids("human_review"), ["rev-b", "rev-c", "rev-a"]);
  assert.deepEqual(board.uids("human_review").slice(0, 2), [before[1], before[2]]);
});

test("a ticket that changes column moves node-for-node and spares the others", () => {
  const { board, state, built } = renderedBoard([
    { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
    { id: "kon-b", title: "B", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
    { id: "rev-old", title: "Old", status: "human_review", kontora: true, updated_at: "2026-05-19T07:00:00Z" },
  ]);
  const keptInProgress = board.uids("in_progress")[1];   // kon-a, below kon-b
  const keptReview = board.uids("human_review")[0];

  state.tickets[1].status = "human_review";
  state.tickets[1].updated_at = "2026-05-19T12:00:00Z";
  state.recomputeBoard();

  assert.deepEqual(built, ["kon-b"]);
  assert.deepEqual(board.ops, ["remove:kon-b", "insert:kon-b"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-a"]);
  assert.deepEqual(board.ids("human_review"), ["kon-b", "rev-old"]);
  assert.equal(board.uids("in_progress")[0], keptInProgress);
  assert.equal(board.uids("human_review")[1], keptReview);
});

test("a search query removes only the cards that stopped matching", () => {
  const { board, state, built } = renderedBoard([
    { id: "kon-alpha", title: "Alpha", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
    { id: "kon-beta", title: "Beta", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
  ]);
  const kept = board.uids("in_progress")[1];   // kon-alpha, below kon-beta

  state.searchQuery = "alpha";
  state.recomputeBoard();

  assert.deepEqual(built, []);
  assert.deepEqual(board.ops, ["remove:kon-beta"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-alpha"]);
  assert.equal(board.uids("in_progress")[0], kept);
});

test("an open card menu survives a render that leaves its card alone", () => {
  const cases = [
    { name: "a clock tick", change: (s) => { s.now += 30000; }, live: true },
    { name: "another card rebuilt", change: (s) => { s.tickets[1].title = "Beta renamed"; }, live: true },
    { name: "another card removed", change: (s) => { s.searchQuery = "alpha"; }, live: true },
    { name: "its own card rebuilt", change: (s) => { s.tickets[0].title = "Alpha renamed"; }, live: false },
    { name: "its own card filtered out", change: (s) => { s.searchQuery = "beta"; }, live: false },
    {
      name: "its own card moved to another column",
      change: (s) => { s.tickets[0].status = "human_review"; s.tickets[0].updated_at = "2026-05-19T12:00:00Z"; },
      live: false,
    },
    { name: "the column emptied", change: (s) => { s.tickets = []; }, live: false },
    {
      name: "the board remounted at the breakpoint",
      change: () => {},
      render: (s, b) => { b.remount(); s._mountBoard(); },
      live: false,
    },
  ];

  for (const c of cases) {
    const { board, state } = renderedBoard([
      { id: "menu-a", title: "Alpha", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
      { id: "menu-b", title: "Beta", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
    ]);
    board.openMenuOn("in_progress", "menu-a");
    state._openMenuId = "menu-a";

    c.change(state);
    if (c.render) c.render(state, board);
    else state.recomputeBoard();

    assert.equal(board.menuIsLive(), c.live, `${c.name}: menu node`);
    assert.equal(state._openMenuId, c.live ? "menu-a" : null, `${c.name}: open-menu state`);
  }
});

test("a column emptied of cards renders the empty state once", () => {
  const { board, state, built } = renderedBoard([
    { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
  ]);

  state.tickets = [];
  state.recomputeBoard();

  assert.deepEqual(built, []);
  assert.deepEqual(board.ops, ["innerHTML:in_progress"]);
  assert.match(board.els["col-in_progress"].innerHTML, /empty-state/);
  assert.equal(state._rendered["in_progress"].empty, true);

  // A second recompute with the column still empty writes nothing.
  board.ops.length = 0;
  state.recomputeBoard();
  assert.deepEqual(board.ops, []);

  // Refilling the column builds it in one innerHTML write, not node by node.
  state.tickets = [{ id: "kon-b", title: "B", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" }];
  state.recomputeBoard();
  assert.deepEqual(built, []);
  assert.deepEqual(board.ops, ["innerHTML:in_progress"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-b"]);
});

test("renderColumn clears a card dropped onto a collapsed rail", () => {
  const { board, state } = renderedBoard([
    { id: "kon-cxl", title: "C", status: "cancelled", kontora: true },
  ]);
  // cancelled starts collapsed: the rail is a drop target only.
  assert.equal(state.isCollapsed("cancelled"), true);
  board.els["col-cancelled"].innerHTML = '<div data-ticket-id="kon-cxl"></div>';
  board.ops.length = 0;

  state.renderColumn("cancelled");

  assert.equal(board.els["col-cancelled"].innerHTML, "");
  assert.deepEqual(board.ids("cancelled"), []);
});

test("formatElapsed renders history durations", () => {
  const state = loadKontoraState();

  assert.equal(state.formatElapsed("2026-05-19T10:00:00Z", "2026-05-19T10:00:45Z"), "45s");
  assert.equal(state.formatElapsed("2026-05-19T10:00:00Z", "2026-05-19T10:38:00Z"), "38m");
  assert.equal(state.formatElapsed("2026-05-19T10:00:00Z", "2026-05-19T11:04:00Z"), "1h 4m");
  assert.equal(state.formatElapsed(null, "2026-05-19T10:00:00Z"), "");
});

test("display toggles invalidate the rendered-card cache and drop card sections", () => {
  const state = loadKontoraState();
  state._rendered = { open: { empty: false, ids: ["kon-1"], sigs: { "kon-1": "x" } } };

  state.toggleShowBadges();
  assert.equal(state.showPipelineBadges, false);
  assert.deepEqual({ ...state._rendered }, {});
  const noBadge = state._cardHTML(
    { id: "sta-9", title: "T", status: "open", created_at: "2026-05-19T08:00:00Z", kontora: true, pipeline: "kontora" },
    { key: "open" },
  );
  assert.equal(noBadge.includes("pipe-tag"), false);

  state.toggleShowAgentMeta();
  assert.equal(state.showAgentMeta, false);
  const noAgent = state._cardHTML(
    { id: "sta-10", title: "T", status: "open", created_at: "2026-05-19T08:00:00Z", kontora: true, agent: "claude-agent" },
    { key: "open" },
  );
  assert.equal(noAgent.includes("claude-agent"), false);
});

test("index.html builds one layout layer with complementary x-if templates", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // Desktop and mobile layers are built exclusively, so the inactive one costs
  // no DOM nodes. x-show would keep both in the document.
  assert.match(html, /<template x-if="!isMobile">\s*<main /);
  assert.match(html, /<template x-if="isMobile">\s*<div class="h-full flex flex-col"/);
  assert.equal(/<main x-show=/.test(html), false);
  assert.equal(/<div x-show="isMobile" x-cloak class="h-full flex flex-col"/.test(html), false);
});

test("_mountBoard rebuilds the board and binds the delegated handler per mount", () => {
  const board = fakeBoard(["open", "in_progress", "human_review", "done", "cancelled"]);
  const state = loadKontoraState({ document: board.document });
  state.tickets = [{ id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" }];
  state.recomputeBoard();
  // Nothing renders before the first mount.
  assert.deepEqual(board.ops, []);

  state._bindGlobalEvents();
  state._mountBoard();

  assert.equal(state._boardInit, true);
  assert.deepEqual(board.ids("in_progress"), ["kon-a"]);
  assert.deepEqual(board.els["board-cols"].listeners, ["click", "keydown"]);
  assert.deepEqual(board.documentEvents, ["click", "keydown"]);

  // Crossing the breakpoint: Alpine replaces the layer, so the cache must reset
  // and the fresh root must get its own listeners without duplicating the
  // document-level ones.
  board.remount();
  board.ops.length = 0;
  state._mountBoard();

  // Every expanded column is filled from scratch; the collapsed rail is already
  // empty, so it takes no write.
  assert.deepEqual(board.ops,
    ["innerHTML:open", "innerHTML:in_progress", "innerHTML:human_review", "innerHTML:done"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-a"]);
  assert.deepEqual(board.els["board-cols"].listeners, ["click", "keydown"]);
  assert.deepEqual(board.documentEvents, ["click", "keydown"]);
});

test("crossing the breakpoint closes the terminal before the replacement board mounts", () => {
  const board = fakeBoard(["open", "in_progress", "human_review", "done", "cancelled"]);
  const state = loadKontoraState({ document: board.document });
  const order = [];
  state.tickets = [];
  state._mountBoard();

  state.closeTerminal = () => { order.push("closeTerminal"); };
  state._mountBoard = () => { order.push("mountBoard"); };
  state.$nextTick = (fn) => { order.push("nextTick"); fn(); };

  state._onBreakpointChange();

  // The terminal container belongs to the layer Alpine is about to destroy.
  assert.deepEqual(order, ["closeTerminal", "nextTick", "mountBoard"]);
});

test("index.html board renders cards imperatively, not via an Alpine x-for", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The columns wrapper carries the event-delegation anchor.
  assert.match(html, /id="board-cols"/);
  // The per-ticket card x-for template was removed (cards come from renderColumn).
  assert.equal(/x-for="ticket in boardTickets\(col\.key\)"/.test(html), false);
  // The imperative card-list container is still wired for Sortable.
  assert.match(html, /x-ref="colList"[\s\S]*?x-init="initSortable\(\$el\)/);
});

test("index.html column x-init drops the render state Alpine just invalidated", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const app = fs.readFileSync(appPath, "utf8");

  // Alpine builds the column element empty, so the x-init must drop that
  // column's cached signatures or renderColumn would skip filling it. The name
  // has to be the field app.js actually keeps, hence the pair of assertions.
  assert.match(html, /x-init="initSortable\(\$el\)[^"]*delete _rendered\[col\.key\]/);
  assert.match(app, /^ {4}_rendered: .+,$/m);
  // _renderedHTML was replaced by the per-card cache; a leftover reference in an
  // Alpine expression throws at evaluation time and skips the renderColumn call.
  assert.equal(html.includes("_renderedHTML"), false);
  assert.equal(app.includes("_renderedHTML"), false);
});

test("setProse renders once and skips an identical second write", () => {
  const state = loadKontoraState();
  let renders = 0;
  const el = { _writes: 0, set innerHTML(v) { this._writes += 1; this._html = v; }, get innerHTML() { return this._html; } };
  state.renderMarkdown = (md) => { renders += 1; return `<p>${md}</p>`; };

  state.setProse(el, "hello");
  assert.equal(el.innerHTML, "<p>hello</p>");
  assert.equal(el._writes, 1);

  state.setProse(el, "hello");
  assert.equal(el._writes, 1);
  assert.equal(renders, 1);

  state.setProse(el, "changed");
  assert.equal(el._writes, 2);
  assert.equal(el.innerHTML, "<p>changed</p>");
});

test("bodyBlockOffsets maps every rendered block back to its source offset", () => {
  const state = loadKontoraState({ marked: realMarked() });
  const cases = [
    {
      name: "headings and paragraphs",
      md: "# Title\n\npara one\n\npara two\n",
      want: [0, 9, 19],
    },
    {
      name: "list, fence, and table",
      md: "- a\n- b\n\n```go\nx := 1\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n",
      want: [0, 9, 27],
    },
    {
      name: "leading blank line",
      md: "\n\n# H\n\ntext\n",
      want: [2, 7],
    },
    {
      // marked consumes the definition without pushing a token, so the cursor
      // is short by its length until indexOf re-finds the next block.
      name: "link-reference definition",
      md: "para one\n\n[ref]: https://example.com\n\npara two\n",
      want: [0, 38],
    },
    {
      name: "CRLF body",
      md: "# H\r\n\r\npara\r\n",
      want: [0, 5],
    },
  ];

  for (const c of cases) {
    state._blockOffsetsSrc = null;
    assert.deepEqual([...state.bodyBlockOffsets(c.md)], c.want, c.name);
    // Offsets are computed against the CR-normalized source, which is what the
    // textarea's value is too.
    const src = c.md.replace(/\r\n|\r/g, "\n");
    for (const off of c.want) assert.notEqual(src[off], "\n", `${c.name}: offset ${off} is a line start`);
  }
});

test("bodyBlockOffsets gives up when a token's raw is not in the source", () => {
  // marked appends a synthetic newline to raw in some paragraph/code
  // sequences, after which no offset in the document can be trusted.
  const state = loadKontoraState({
    marked: { lexer: () => [{ type: "paragraph", raw: "para\n" }, { type: "paragraph", raw: "not in the source" }] },
  });

  assert.equal(state.bodyBlockOffsets("para\n\nafter\n"), null);
});

test("bodyBlockOffsets memoizes on the body and yields null without a lexer", () => {
  let calls = 0;
  const marked = realMarked();
  const state = loadKontoraState({
    marked: { lexer: (md) => { calls += 1; return marked.lexer(md); } },
  });

  const first = state.bodyBlockOffsets("# H\n\ntext\n");
  assert.deepEqual([...first], [0, 5]);
  assert.equal(state.bodyBlockOffsets("# H\n\ntext\n"), first);
  assert.equal(calls, 1);

  // The harness default stub has parse but no lexer: no offsets, no throw.
  assert.equal(loadKontoraState().bodyBlockOffsets("# H\n"), null);
});

test("closestScroller takes the innermost scrolling ancestor, overflowing or not", () => {
  // A panel narrower than 720px stacks, which turns an outer container into a
  // second scroller. The inner one is the one that moves the ticket body.
  const outer = { parentElement: null, scrollHeight: 5000, clientHeight: 400 };
  const inner = { parentElement: outer, scrollHeight: 300, clientHeight: 400 };
  const plain = { parentElement: inner };
  const leaf = { parentElement: plain };
  const overflow = new Map([[outer, "scroll"], [inner, "auto"], [plain, "visible"]]);
  const state = loadKontoraState({
    window: { innerWidth: 1200, addEventListener() {}, getComputedStyle: (el) => ({ overflowY: overflow.get(el) }) },
  });

  // inner does not currently overflow, which is exactly the state the swap
  // passes through; an overflow test here would pick outer.
  assert.equal(state.closestScroller(leaf), inner);
  assert.equal(state.closestScroller(inner), outer);
  assert.equal(state.closestScroller(outer), null);
});

test("caretLineTop returns the line's top and leaves the textarea untouched", () => {
  const body = "# Title\n\npara one\n\npara two\n";
  const dom = fakeBodyDom({ body });
  const state = bodyEditState(dom, body);
  dom.textarea.style.height = "136px";

  // Offset 19 starts "para two" on the fifth line: border + padding + 4 lines.
  assert.equal(state.caretLineTop(dom.textarea, 19), BORDER + PAD + 4 * LINE);
  assert.equal(state.caretLineTop(dom.textarea, 0), BORDER + PAD);
  assert.equal(dom.textarea.value, body);
  assert.equal(dom.textarea.style.height, "136px");
});

test("entering edit mode holds the clicked block on screen", () => {
  const doc = "# Title\n\npara one\n\npara two\n";
  // A markdown comment is one token and no element, so the block map cannot be
  // trusted: three offsets against two rendered children.
  const commented = "para one\n\n<!-- note -->\n\npara two\n";
  const cases = [
    {
      name: "a block halfway down a long ticket",
      body: doc, blockHeights: [40, 40, 40], block: 2, editorScrollHeight: 1500,
      // The textarea starts 500 into the content, "para two" 89 into the
      // textarea, and the block has to land back at screen Y 180.
      wantTop: 500 + 89 - 180, wantCaret: 19,
    },
    {
      name: "an exact target past the end of the scroll range",
      body: doc, blockHeights: [40, 40, 40], block: 2, editorScrollHeight: 600,
      wantTop: 600 - 400, wantCaret: 19,
    },
    {
      name: "blocks that do not line up with the rendered elements",
      body: commented, blockHeights: [40, 40], block: 1, editorScrollHeight: 1500,
      wantTop: 400 * 1500 / 2000, wantCaret: 0,
    },
    {
      name: "a click on the wrapper rather than a block",
      body: doc, blockHeights: [40, 40, 40], block: null, editorScrollHeight: 1500,
      wantTop: 400 * 1500 / 2000, wantCaret: 0,
    },
  ];

  for (const c of cases) {
    const dom = fakeBodyDom({ body: c.body, blockHeights: c.blockHeights, scrollTop: 400 });
    const state = bodyEditState(dom, c.body);
    state.$nextTick = (fn) => {
      // The swap: the tall preview is replaced by the shorter textarea.
      state.$refs.bodyEditor = dom.textarea;
      dom.scroller.scrollHeight = c.editorScrollHeight;
      fn();
    };

    state.beginBodyEdit({ target: c.block === null ? { parentElement: null } : dom.prose.children[c.block] });

    assert.equal(state.editingBody, true, c.name);
    assert.equal(dom.scroller.scrollTop, c.wantTop, c.name);
    assert.equal(dom.textarea.selectionStart, c.wantCaret, c.name);
    assert.equal(dom.textarea.selectionEnd, c.wantCaret, c.name);
    assert.deepEqual({ ...dom.textarea.focusOptions }, { preventScroll: true }, c.name);
    // Auto-grow, not the removed fixed height.
    assert.equal(dom.textarea.style.height, `${c.body.split("\n").length * LINE + 2 * PAD}px`, c.name);
  }
});

test("leaving edit mode puts the caret's block back at the same screen position", () => {
  const doc = "# Title\n\npara one\n\npara two\n";
  // The comment is a block to the lexer and no element after sanitizing, so
  // three offsets meet two rendered children and every index after the comment
  // names the block before the one it should.
  const commented = "<!-- note -->\n\npara one\n\npara two\n";
  const ratio = 300 * 2000 / 1500;
  const cases = [
    {
      name: "the block containing the caret",
      body: doc, blockHeights: [40, 40, 40], caret: 22,
      // "para two" starts at 19, so a caret at 22 belongs to the third block.
      // Its line sat at screen Y 500 + 89 - 300 = 289 and the rendered block is
      // 580 into the content.
      wantTop: 580 - 289,
    },
    {
      // The preview has fewer children than the body has blocks, so the block
      // the caret belongs to has no element to put back.
      name: "a preview that lost a block to sanitizing",
      body: doc, blockHeights: [40, 40], caret: 22,
      wantTop: ratio,
    },
    {
      // Same shortfall, but the caret's index still hits an element: only the
      // block count catches that it is the wrong one.
      name: "a dropped block that leaves the caret's index in range",
      body: commented, blockHeights: [40, 40], caret: 17,
      wantTop: ratio,
    },
  ];

  for (const c of cases) {
    const dom = fakeBodyDom({ body: c.body, blockHeights: c.blockHeights, scrollTop: 300, scrollHeight: 1500 });
    const state = bodyEditState(dom, c.body);
    state.editingBody = true;
    state.$refs.bodyEditor = dom.textarea;
    dom.textarea.selectionStart = c.caret;
    dom.textarea.style.height = "136px";
    state.$nextTick = (fn) => {
      dom.scroller.scrollHeight = 2000;
      fn();
    };

    state.exitBodyEdit();

    assert.equal(state.editingBody, false, c.name);
    assert.equal(dom.scroller.scrollTop, c.wantTop, c.name);
  }
});

test("a save that lands after the panel closed does not reopen it", async () => {
  const updated = { id: "kon-1", title: "One", status: "todo", kontora: true, body: "typed" };
  const state = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => updated }) });
  state.tickets = [{ id: "kon-1", title: "One", status: "todo", kontora: true, body: "old" }];
  state.selectedTicket = state.tickets[0];
  state.editing = true;
  state.editForm = { body: "typed", pipeline: "", path: "", agent: "", branch: "" };

  // closeDetail flushes the pending body, then clears the selection while the
  // request is still in flight.
  const pending = state.saveEdit();
  state.selectedTicket = null;
  await pending;

  assert.equal(state.selectedTicket, null);
  assert.equal(state.editSubmitting, false);
  assert.equal(state.tickets[0].body, "typed");
});

test("flushEditSave cancels the pending debounce and saves once", () => {
  const state = loadKontoraState();
  let saves = 0;
  state.editing = true;
  state.selectedTicket = { id: "kon-1", status: "todo" };
  state.saveEdit = () => { saves += 1; };

  state.debounceSaveEdit();
  assert.notEqual(state._editDebounce, null);

  state.flushEditSave();

  assert.equal(saves, 1);
  assert.equal(state._editDebounce, null);
});

test("closeDetail, the SSE editability guard, and switchTab flush before clearing the editor", () => {
  const cases = [
    {
      name: "closeDetail",
      act: (s) => s.closeDetail(),
      check: (s) => { assert.equal(s.selectedTicket, null); assert.equal(s.editingBody, false); },
    },
    {
      name: "a run starts under the editor",
      act: (s) => s.applyTicketUpdate({ id: "kon-1", status: "in_progress", kontora: true }),
      check: (s) => { assert.equal(s.editing, false); assert.equal(s.editingBody, false); },
    },
    {
      name: "switchTab away from the ticket tab",
      act: (s) => s.switchTab("terminal"),
      check: (s) => { assert.equal(s.editingBody, false); assert.equal(s.editing, true); },
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    const saved = [];
    state.updateFavicon = () => {};
    state.closeTerminal = () => {};
    state.openTerminal = () => {};
    state.tickets = [{ id: "kon-1", title: "One", status: "todo", kontora: true }];
    state.selectedTicket = { id: "kon-1", title: "One", status: "todo", body: "old" };
    state.editing = true;
    state.editingBody = true;
    state.editForm = { body: "typed", pipeline: "", path: "", agent: "", branch: "" };
    state.saveEdit = () => { saved.push(state.editForm.body); };

    state.debounceSaveEdit();
    c.act(state);

    assert.deepEqual(saved, ["typed"], `${c.name}: saved the pending body`);
    assert.equal(state._editDebounce, null, `${c.name}: debounce cancelled`);
    c.check(state);
  }
});

test("opening another ticket never writes the previous body onto it", async () => {
  const cases = [
    // Long enough for an uncancelled debounce to fire against the new ticket.
    { name: "a debounce still pending at the click", act: () => {}, wait: 900 },
    { name: "an editor left open after the swap", act: (s) => s.closeDetail(), wait: 0 },
  ];

  for (const c of cases) {
    const puts = [];
    const other = { id: "kon-2", title: "Two", status: "in_progress", kontora: true, body: "two" };
    const state = loadKontoraState({
      fetch: async (url, options) => {
        if (options?.method === "PUT") {
          puts.push({ url, body: JSON.parse(options.body) });
          return { ok: true, json: async () => ({ id: url.split("/").pop() }) };
        }
        return { ok: true, json: async () => other };
      },
    });
    state.closeTerminal = () => {};
    state.openTerminal = () => {};
    state.tickets = [{ id: "kon-1", title: "One", status: "todo", kontora: true, body: "old" }, other];
    state.selectedTicket = state.tickets[0];
    state.editing = true;
    state.editingBody = true;
    state.editForm = { body: "typed", pipeline: "", path: "", agent: "", branch: "" };

    state.debounceSaveEdit();
    await state.selectTicket(other);
    c.act(state);
    await new Promise((resolve) => setTimeout(resolve, c.wait));

    assert.deepEqual(puts, [{ url: "/api/tickets/kon-1", body: { body: "typed" } }], c.name);
    assert.equal(state._editDebounce, null, c.name);
    assert.equal(state.editing, false, c.name);
    assert.equal(state.editingBody, false, c.name);
  }
});

test("blurBodyEdit saves but keeps the editor open when the window lost focus", () => {
  const dom = fakeBodyDom({ body: "text\n" });
  const cases = [
    { name: "click elsewhere in the page", hasFocus: true, wantEditing: false },
    { name: "switched to another application", hasFocus: false, wantEditing: true },
  ];

  for (const c of cases) {
    const state = loadKontoraState({
      window: dom.window,
      document: { getElementById: () => null, querySelector: () => null, hasFocus: () => c.hasFocus, documentElement: { style: {} } },
    });
    const saved = [];
    state.editing = true;
    state.editingBody = true;
    state.selectedTicket = { id: "kon-1", status: "todo" };
    state.editForm = { body: "typed", pipeline: "", path: "", agent: "", branch: "" };
    state.$refs = { bodyEditor: dom.textarea, bodyPreview: dom.prose };
    state.$nextTick = (fn) => fn();
    state.saveEdit = () => { saved.push(state.editForm.body); };

    state.blurBodyEdit();

    assert.deepEqual(saved, ["typed"], c.name);
    assert.equal(state.editingBody, c.wantEditing, c.name);
  }
});

test("index.html body editor grows with its content and owns its exit gestures", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const textarea = html.match(/<textarea x-model="editForm\.body"[\s\S]*?<\/textarea>/)[0];

  // The fixed height was what collapsed scrollHeight and threw the reader to
  // the top; overflow stays hidden so caretLineTop measures a stable wrap width.
  assert.equal(/h-\[calc\(/.test(textarea), false);
  assert.match(textarea, /overflow-hidden/);
  assert.match(textarea, /min-h-\[100px\]/);
  assert.match(textarea, /@input="autoGrowBody\(\$el\); debounceSaveEdit\(\)"/);
  assert.match(textarea, /x-ref="bodyEditor"/);
  assert.equal(/x-init=/.test(textarea), false);

  // Escape must not reach the @keydown.escape.window cascade, which closes the
  // whole detail panel.
  assert.match(textarea, /@keydown\.escape="\$event\.stopPropagation\(\); exitBodyEdit\(\)"/);
  assert.match(textarea, /@keydown\.enter="if \(\$event\.metaKey \|\| \$event\.ctrlKey\)/);
  assert.match(textarea, /@blur="blurBodyEdit\(\)"/);

  assert.match(html, /@click="beginBodyEdit\(\$event\)"/);
  assert.match(html, /x-ref="bodyPreview"/);
});

test("index.html renders every prose block through the idempotent write", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // x-html reassigns innerHTML on every effect run, so an SSE refresh rebuilds
  // the subtree and the scroller clamps to the top while it is empty.
  assert.equal(html.includes('x-html="renderMarkdown'), false);
  assert.equal(html.match(/x-effect="setProse\(\$el, /g).length, 6);
});
