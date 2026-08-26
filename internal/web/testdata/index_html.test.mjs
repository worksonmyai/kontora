import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const dirname = path.dirname(fileURLToPath(import.meta.url));
const htmlPath = path.join(dirname, "../static/index.html");
const tipsPath = path.join(dirname, "../static/tips.js");
const uiDir = path.join(dirname, "../ui");
const statsPath = path.join(uiDir, "stats.js");
const settingsPath = path.join(uiDir, "settings.js");
// The UI modules as one string. The assertions that read source rather than
// drive the component are about what the code says, not which module says it.
const uiSource = fs
  .readdirSync(uiDir, { recursive: true })
  .filter((f) => f.endsWith(".js"))
  .sort()
  .map((f) => fs.readFileSync(path.join(uiDir, f), "utf8"))
  .join("\n");
// The compiled bundle, written by staticjs_test.go. Testing what the daemon
// serves means the source split cannot drift from what the browser runs.
const bundlePath = process.env.KONTORA_BUNDLE;
if (!bundlePath) throw new Error("KONTORA_BUNDLE is unset; run this through go test");

// recomputeBoard builds its column buckets with array literals created inside
// the VM realm, so their prototype differs from this file's Array and
// deepStrictEqual's prototype check would fail. Spread into the test realm
// before asserting.
const bt = (state, key) => [...state.boardTickets(key)];

// Same realm problem, one level deeper: settings values are nested arrays and
// objects built inside the VM, so a spread is not enough. Rebuild in this
// realm before asserting on structure.
const vmValue = (v) => JSON.parse(JSON.stringify(v));

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

// The bundle is an IIFE, so the only things it leaves behind are the three it
// assigns to globalThis: `kontora`, and the module-scope `termState` and
// `statsDerive` the tests below drive directly. Running it in a VM context
// makes globalThis that context, so `ctx.termState` is the terminal handles.
function loadKontoraContext(overrides = {}) {
  const context = kontoraContext(overrides);
  vm.createContext(context);
  vm.runInContext(fs.readFileSync(bundlePath, "utf8"), context);
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
    attachCustomKeyEventHandler(handler) {
      this.keyHandler = handler;
    }
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
        return id === "terminal-session" ? container : null;
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
  state.activeTab = "session";
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

// makeEventSourceFake returns an EventSource class that records every instance
// on `opened`, so a test can fire a frame at one, assert what the pane did with
// it, and tell an open connection from a closed one.
function makeEventSourceFake(opened = []) {
  return class FakeEventSource {
    constructor(url) {
      this.url = url;
      this.closed = false;
      this.listeners = {};
      this.onerror = null;
      opened.push(this);
    }
    addEventListener(type, handler) {
      (this.listeners[type] || (this.listeners[type] = [])).push(handler);
    }
    close() {
      this.closed = true;
    }
    // emit drives one server frame. data is the JSON the event carries.
    emit(type, data) {
      for (const h of this.listeners[type] || []) h({ data: JSON.stringify(data === undefined ? {} : data) });
    }
    fail() {
      if (this.onerror) this.onerror({});
    }
  };
}

function kontoraContext(overrides = {}) {
  const context = {
    console,
    setTimeout,
    clearTimeout,
    structuredClone,
    TextEncoder,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    cancelAnimationFrame() {},
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
    setInterval() {
      return 1;
    },
    clearInterval() {},
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
      // Prose writes mark ticket ids, which walks the text nodes. The stub
      // elements here are not a tree, so the walk finds nothing; fakeProseDom
      // is the harness that exercises the marking itself.
      createTreeWalker() {
        return { nextNode: () => null };
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
    // A recording fake, not a bare class: the assistant pane registers
    // listeners on the handle it opens, and reads back what it closed.
    EventSource: makeEventSourceFake(),
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
// The shipped min-h-[100px]: the box never gets shorter than this, whatever
// the inline height says, so a measurement that only zeroes the height reads
// the floor rather than the text.
const MIN_HEIGHT = 100;

function fakeBodyDom({ body, blockHeights = [], proseTop = 500, editorTop = 500, scrollTop = 0, scrollHeight = 2000, clientHeight = 400, scrollFollowsEditor = false }) {
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
    style: { height: "", minHeight: `${MIN_HEIGHT}px` },
    selectionStart: 0,
    selectionEnd: 0,
    focusOptions: null,
    getBoundingClientRect: rectAt(editorTop),
    get clientHeight() {
      const h = parseFloat(this.style.height);
      const min = parseFloat(this.style.minHeight) || 0;
      return Math.max(Number.isNaN(h) ? 0 : h, min);
    },
    get scrollHeight() {
      return Math.max(this.clientHeight, this.value.split("\n").length * LINE + 2 * PAD);
    },
    focus(options) { this.focusOptions = options; },
    setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; },
  };

  // While the editor is open the textarea is what makes the panel scrollable,
  // so its height is part of the panel's content: shrinking it shrinks the
  // content and the browser clamps scrollTop to fit. Growing it back does not
  // restore the clamped position. Opt-in, because the other cases stage the
  // preview/editor swap by setting scroller.scrollHeight directly.
  if (scrollFollowsEditor) {
    let height = "";
    textarea.style = {
      minHeight: `${MIN_HEIGHT}px`,
      get height() { return height; },
      set height(value) {
        scroller.scrollHeight += (parseFloat(value) || 0) - (parseFloat(height) || 0);
        height = value;
        scroller.scrollTop = Math.min(scroller.scrollTop, Math.max(0, scroller.scrollHeight - scroller.clientHeight));
      },
    };
  }

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
});

test("index.html loads the external app script", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /<script src="\/app\.js"><\/script>/);
});

// The head scripts are deferred so they do not block the parser, which makes
// their execution order the document order. Alpine's CDN build starts itself as
// it loads and immediately walks x-init, where initSortable calls Sortable, so
// a Sortable tag placed after Alpine's leaves the board with no drop targets.
test("head scripts are deferred, with Alpine after the libraries it uses", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const head = html.slice(0, html.indexOf("</head>"));

  const tags = [...head.matchAll(/<script([^>]*)src="([^"]+)"/g)];
  const vendor = tags.filter((m) => m[2].startsWith("/vendor/"));
  assert.ok(vendor.length >= 4, "expected the vendored libraries in <head>");
  for (const m of vendor) {
    assert.ok(m[1].includes("defer"), `${m[2]} must be deferred`);
  }

  const order = vendor.map((m) => m[2]);
  const alpine = order.findIndex((s) => s.includes("alpinejs"));
  const sortable = order.findIndex((s) => s.includes("sortablejs"));
  assert.ok(sortable >= 0 && alpine >= 0, "expected both Alpine and Sortable");
  assert.ok(sortable < alpine, "Sortable must load before Alpine starts");

  // theme.js is the exception: deferring it would let the page paint in the
  // wrong palette before data-theme is set.
  const theme = tags.find((m) => m[2] === "/theme.js");
  assert.ok(theme, "expected /theme.js in <head>");
  assert.ok(!theme[1].includes("defer"), "/theme.js must run before first paint");
});

// xterm is the one vendored library the board never needs. Its stylesheet is
// render-blocking, so it loads from openTerminal instead of <head>.
test("xterm is kept out of the initial page", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.ok(!html.includes("xterm"), "index.html must not reference xterm");
  assert.match(uiSource, /_loadTerminalCSS/);
});

test("openTerminal cancels stale startup after teardown", async () => {
  const { ctx, state } = loadKontoraContext();
  const nextTick = deferred();
  const connectCalls = [];

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "session";
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
  state.activeTab = "session";
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
  state.activeTab = "session";
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
  state.activeTab = "session";
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

test("switching away from the session tab and back keeps one Terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const created = ctx.termState.Terminal.created;

  for (let i = 0; i < 10; i++) {
    state.switchTab("ticket");
    state.switchTab("session");
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

test("Escape in the terminal drops read-write instead of reaching the agent", async () => {
  const cases = [
    {
      name: "Escape while read-write",
      rw: true,
      event: { type: "keydown", key: "Escape" },
      wantToAgent: false,
      wantRW: false,
      wantStopped: true,
    },
    {
      name: "Escape while read-only",
      rw: false,
      event: { type: "keydown", key: "Escape" },
      wantToAgent: true,
      wantRW: false,
      wantStopped: false,
    },
    {
      name: "another key while read-write",
      rw: true,
      event: { type: "keydown", key: "a" },
      wantToAgent: true,
      wantRW: true,
      wantStopped: false,
    },
    {
      name: "the keyup half of Escape",
      rw: true,
      event: { type: "keyup", key: "Escape" },
      wantToAgent: true,
      wantRW: true,
      wantStopped: false,
    },
  ];

  for (const tc of cases) {
    const { ctx, state } = await liveTerminal();
    if (tc.rw) state.toggleTerminalRW();
    assert.equal(state.terminalRW, tc.rw, `${tc.name}: mode setup`);

    let stopped = false;
    const event = {
      ...tc.event,
      preventDefault() {},
      stopPropagation() {
        stopped = true;
      },
    };

    assert.equal(ctx.termState.term.keyHandler(event), tc.wantToAgent, `${tc.name}: key to agent`);
    assert.equal(state.terminalRW, tc.wantRW, `${tc.name}: read-write mode`);
    // A stopped event never reaches the body Escape chain, which would close
    // the ticket on the same keypress.
    assert.equal(stopped, tc.wantStopped, `${tc.name}: event stopped`);
  }
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

test("start actions open initialization instead of posting", async () => {
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url) => { posted.push(url); return { ok: true, json: async () => ({}) }; },
  });
  const opened = [];
  state.tickets = [
    { id: "ext-run", title: "Run", status: "open", kontora: false },
    { id: "ext-retry", title: "Retry", status: "paused", kontora: false },
    // Managed, so it could run as it stands; queueing it still confirms the
    // fields first.
    { id: "kon-open", title: "Queue", status: "open", kontora: true },
    // Managed and past open: resuming it posts.
    { id: "kon-paused", title: "Resume", status: "paused", kontora: true },
  ];
  state.openInitModal = (ticket) => { opened.push(ticket.id); };

  await state.moveTicketVia("ext-run", "run", null);
  await state.moveTicketVia("ext-retry", "retry", null);
  await state.moveTask("ext-retry", "todo");
  await state.moveTicketVia("kon-open", "run", null);
  await state.moveTask("kon-open", "todo");
  await state.moveTicketVia("kon-paused", "retry", null);

  assert.deepEqual(opened, ["ext-run", "ext-retry", "ext-retry", "kon-open", "kon-open"]);
  assert.deepEqual(posted, ["/api/tickets/kon-paused/retry"]);
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
      finished_at: "2026-05-19T09:00:00Z",
      // Edited long after the run: a later mtime must not win.
      updated_at: "2026-05-19T20:00:00Z",
    },
    {
      id: "rev-new",
      title: "New",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T06:00:00Z",
      finished_at: "2026-05-19T11:00:00Z",
      updated_at: "2026-05-19T12:00:00Z",
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
      id: "rev-never-ran",
      title: "Never ran",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T09:00:00Z",
      updated_at: "2026-05-19T09:30:00Z",
    },
  ];

  const ids = state.ticketsByStatuses("human_review").map(t => t.id);

  assert.deepEqual(ids, ["rev-no-update", "rev-moved", "rev-never-ran"]);
});

test("reviewFinishedAt reads finished_at and falls back to the mtime, then creation", () => {
  const state = loadKontoraState();
  const review = (t) => state.reviewFinishedAt({ status: "human_review", ...t });

  assert.equal(review({ finished_at: "2026-05-19T09:00:00Z" }), "2026-05-19T09:00:00Z");
  assert.equal(review({ updated_at: "2026-05-19T10:00:00Z" }), "2026-05-19T10:00:00Z");
  assert.equal(review({ created_at: "2026-05-19T08:00:00Z" }), "2026-05-19T08:00:00Z");
  assert.equal(review({}), "");

  // The card whose file was touched after its last run: the run still wins.
  assert.equal(
    review({
      finished_at: "2026-04-20T09:00:00Z",
      updated_at: "2026-08-07T18:00:00Z",
      created_at: "2026-04-01T08:00:00Z",
    }),
    "2026-04-20T09:00:00Z",
  );

  // Any other column has no finish time to show.
  assert.equal(state.reviewFinishedAt({ status: "done", finished_at: "2026-05-19T09:00:00Z" }), "");
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

test("applyTicketUpdate keeps the body off the board entry and on the open ticket", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [{ id: "kon-001", title: "Todo", status: "todo", kontora: true }];
  state.selectedTicket = { id: "kon-001", title: "Todo", status: "todo", body: "fetched body" };

  state.applyTicketUpdate({
    id: "kon-001",
    title: "Todo",
    status: "in_progress",
    kontora: true,
    body: "event body",
    notes: [{ at: "2026-05-19T09:00:00Z", text: "a note" }],
    history: [{ stage: "code", completed_at: "2026-05-19T09:00:00Z" }],
    summary: "did the thing",
  });

  assert.equal(state.tickets[0].status, "in_progress");
  assert.equal(state.tickets[0].history.length, 1);
  assert.equal(Object.hasOwn(state.tickets[0], "body"), false);
  assert.equal(Object.hasOwn(state.tickets[0], "notes"), false);
  // The panel keeps the body it fetched and takes the event's live fields.
  assert.equal(state.selectedTicket.body, "fetched body");
  assert.equal(state.selectedTicket.summary, "did the thing");
  assert.equal(state.selectedTicket.notes.length, 1);
});

test("applyTicketUpdate appends a new ticket without its body", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [];

  state.applyTicketUpdate({ id: "kon-002", title: "New", status: "todo", kontora: true, body: "event body" });

  assert.equal(state.tickets.length, 1);
  assert.equal(state.tickets[0].title, "New");
  assert.equal(Object.hasOwn(state.tickets[0], "body"), false);
});

test("a board taking updates for every ticket does not grow with their bodies", () => {
  // The board held 59 KB per opened ticket before entries dropped the body:
  // 359 tickets converged on ~21 MB of retained JSON.
  const body = "x".repeat(55 * 1024);
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = Array.from({ length: 80 }, (_, i) => ({
    id: "kon-" + i, title: "T" + i, status: "todo", kontora: true,
  }));
  const before = JSON.stringify(state.tickets).length;

  for (const t of state.tickets.slice()) {
    state.applyTicketUpdate({ ...t, status: "in_progress", body, notes: [{ at: "", text: body }] });
  }

  const after = JSON.stringify(state.tickets).length;
  assert.equal(state.tickets.length, 80);
  assert.ok(after < before * 2, `board grew from ${before} to ${after} bytes`);
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

test("canAnnotateTicket takes the daemon's answer, not its own status list", () => {
  const state = loadKontoraState();

  assert.equal(state.canAnnotateTicket({ status: "open", can_annotate: true }), true);
  // An open ticket parked for an annotation run is refused, and only the daemon
  // knows that, so the status alone must not enable the button.
  assert.equal(state.canAnnotateTicket({ status: "open" }), false);
  assert.equal(state.canAnnotateTicket(null), false);
});

test("historyLabel marks an annotation run and how it got its session", () => {
  const state = loadKontoraState();

  assert.equal(state.historyLabel({ stage: "code" }), "code");
  assert.equal(
    state.historyLabel({ stage: "code", kind: "annotation", session_reused: true }),
    "code \u00b7 annotation (resumed)",
  );
  assert.equal(
    state.historyLabel({ stage: "code", kind: "annotation", session_reused: false }),
    "code \u00b7 annotation (fresh)",
  );
  // An entry written before session_reused existed reads as a fresh run.
  assert.equal(
    state.historyLabel({ stage: "code", kind: "annotation" }),
    "code \u00b7 annotation (fresh)",
  );
});

test("a history row chips what Kontora passed the agent", () => {
  const state = loadKontoraState();

  assert.equal(state.historySettings({ stage: "code", model: "haiku", effort: "low" }), "haiku · low");
  // Either half alone still says something; neither means Kontora passed no
  // flag at all, so the row shows no chip.
  assert.equal(state.historySettings({ stage: "code", model: "haiku" }), "haiku");
  assert.equal(state.historySettings({ stage: "code", effort: "low" }), "low");
  assert.equal(state.historySettings({ stage: "code" }), "");
  assert.equal(state.historySettings(null), "");
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

test("selectTicket refreshes the board entry from the fetched ticket but leaves its body behind", async () => {
  const full = {
    id: "kon-001",
    title: "One",
    status: "in_progress",
    agent: "a2",
    kontora: true,
    body: "# One\n\nlong body",
    notes: [{ at: "2026-05-19T09:00:00Z", text: "a note" }],
  };
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
  assert.equal(state.tickets[0].agent, "a2");
  assert.equal(Object.hasOwn(state.tickets[0], "body"), false);
  assert.equal(Object.hasOwn(state.tickets[0], "notes"), false);
  assert.equal(state.selectedTicket.body, "# One\n\nlong body");
});

// GET /api/tickets is projected server-side today, so the client projection is
// what keeps the invariant if that endpoint ever starts answering with a body.
test("fetchTasks keeps the body off the board even when the list carries one", async () => {
  const state = loadKontoraState({
    fetch: async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        tickets: [{ id: "kon-001", title: "One", status: "todo", kontora: true, body: "# One", notes: [{ at: "", text: "a note" }] }],
        running_agents: 0,
      }),
    }),
  });
  state.updateFavicon = () => {};

  await state.fetchTasks();

  assert.equal(state.tickets.length, 1);
  assert.equal(state.tickets[0].title, "One");
  assert.equal(Object.hasOwn(state.tickets[0], "body"), false);
  assert.equal(Object.hasOwn(state.tickets[0], "notes"), false);
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

// A typed value narrows by substring; the `=` form a sidebar click writes
// matches the whole field.
const tok = (value, exact = false) => ({ value, exact });

test("parseFilterQuery splits the box into tokens and free text", () => {
  const state = loadKontoraState();
  const cases = [
    { name: "a project token", q: "project:kontora", want: { text: "", project: [tok("kontora")], agent: [] } },
    { name: "an agent token", q: "agent:Claude", want: { text: "", project: [], agent: [tok("claude")] } },
    { name: "an exact token", q: "project:=kontora", want: { text: "", project: [tok("kontora", true)], agent: [] } },
    // A name with a space in it only survives the term split quoted, which is
    // how the sidebar writes it.
    { name: "a quoted value", q: 'project:="my notes"', want: { text: "", project: [tok("my notes", true)], agent: [] } },
    { name: "quoted free text", q: '"two words"', want: { text: "two words", project: [], agent: [] } },
    { name: "a token plus free text", q: "project:Kontora Fonts", want: { text: "fonts", project: [tok("kontora")], agent: [] } },
    { name: "two tokens", q: "project:kontora agent:claude", want: { text: "", project: [tok("kontora")], agent: [tok("claude")] } },
    { name: "a repeated key", q: "project:kontora project:widget-api", want: { text: "", project: [tok("kontora"), tok("widget-api")], agent: [] } },
    // Between the colon and the first typed character the key constrains
    // nothing, rather than emptying the board.
    { name: "a bare prefix", q: "project:", want: { text: "", project: [], agent: [] } },
    { name: "a bare exact prefix", q: "project:=", want: { text: "", project: [], agent: [] } },
    { name: "a bare prefix and free text", q: "project: fonts", want: { text: "fonts", project: [], agent: [] } },
    { name: "an unknown prefix", q: "foo:bar", want: { text: "foo:bar", project: [], agent: [] } },
    { name: "an empty box", q: "  ", want: { text: "", project: [], agent: [] } },
  ];

  for (const c of cases) {
    assert.deepEqual(vmValue(state.parseFilterQuery(c.q)), c.want, c.name);
  }

  // An unknown prefix is matched as the literal it looks like.
  assert.equal(state.ticketMatchesQuery({ title: "see foo:bar" }, "foo:bar"), true);
  assert.equal(state.ticketMatchesQuery({ title: "see bar" }, "foo:bar"), false);
});

// Configured projects whose names overlap, one whose name has a space in it,
// and one repository that is not configured at all, so a project name has to
// come from the config in one case and from the path in the other.
const FILTER_PROJECTS = [
  { name: "kontora", path: "~/projects/kontora", resolved_path: "/home/u/projects/kontora" },
  { name: "kontora-web", path: "~/projects/kontora-web", resolved_path: "/home/u/projects/kontora-web" },
  { name: "agento11y", path: "~/projects/widget-api", resolved_path: "/home/u/projects/widget-api" },
  { name: "my notes", path: "~/projects/notes", resolved_path: "/home/u/projects/notes" },
];

test("ticketProjectName names the configured project, or the repository", () => {
  const state = loadKontoraState();
  state.configCache = { projects: FILTER_PROJECTS, pipelines: [], agents: [] };
  const cases = [
    { name: "the configured path", path: "~/projects/kontora", want: "kontora" },
    { name: "the resolved path", path: "/home/u/projects/widget-api", want: "agento11y" },
    { name: "a trailing slash", path: "~/projects/kontora/", want: "kontora" },
    { name: "no configured project", path: "/home/u/projects/scratch", want: "scratch" },
    { name: "no path at all", path: "", want: "" },
  ];

  for (const c of cases) {
    assert.equal(state.ticketProjectName({ path: c.path }), c.want, c.name);
  }
});

// One ticket per project, each with its own agent, so a token that names one of
// them has the others to exclude. The kontora and kontora-web pair, and the
// claude and claude-opus pair, are what a substring token over-matches.
const FILTER_TICKETS = [
  { id: "kon-1", title: "Vendor the fonts", status: "todo", kontora: true, agent: "claude", path: "~/projects/kontora" },
  { id: "kon-2", title: "Fonts on the stats page", status: "todo", kontora: true, agent: "codex", path: "/home/u/projects/widget-api" },
  { id: "kon-3", title: "Scratch work", status: "todo", kontora: true, agent: "pi-opus", path: "/home/u/projects/scratch" },
  { id: "kon-4", title: "Dashboard sidebar", status: "todo", kontora: true, agent: "claude-opus", path: "~/projects/kontora-web" },
  { id: "kon-5", title: "Sort the inbox", status: "todo", kontora: true, agent: "codex", path: "/home/u/projects/notes" },
];
const FILTER_ALL = ["kon-1", "kon-2", "kon-3", "kon-4", "kon-5"];

test("recomputeBoard narrows the board by the project and agent tokens", () => {
  const cases = [
    { name: "a configured project by its path", q: "project:=kontora", want: ["kon-1"] },
    { name: "a configured project by its resolved path", q: "project:=agento11y", want: ["kon-2"] },
    { name: "a repository with no configured project", q: "project:=scratch", want: ["kon-3"] },
    { name: "a project name with a space", q: 'project:="my notes"', want: ["kon-5"] },
    // A typed value narrows by substring, so a name that contains another name
    // keeps both. Only the `=` form the sidebar writes selects one project.
    { name: "a partly typed value", q: "project:kon", want: ["kon-1", "kon-4"] },
    { name: "a name that contains another name", q: "project:kontora", want: ["kon-1", "kon-4"] },
    { name: "an agent, exactly", q: "agent:=claude", want: ["kon-1"] },
    { name: "an agent, as a substring", q: "agent:claude", want: ["kon-1", "kon-4"] },
    { name: "both keys at once", q: "project:kontora agent:codex", want: [] },
    { name: "a token and free text", q: "project:kontora fonts", want: ["kon-1"] },
    { name: "free text alone", q: "fonts", want: ["kon-1", "kon-2"] },
    // A bare word must not reach the agent field: that split is what keeps the
    // command palette a wider search than the board filter.
    { name: "an agent name as free text", q: "pi-opus", want: [] },
    { name: "a bare prefix", q: "project:", want: FILTER_ALL },
    { name: "an unknown prefix", q: "foo:bar", want: [] },
    { name: "an empty box", q: "", want: FILTER_ALL },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = { projects: FILTER_PROJECTS, pipelines: [], agents: [] };
    state.tickets = FILTER_TICKETS;
    state.searchQuery = c.q;

    state.recomputeBoard();

    assert.deepEqual(bt(state, "in_progress").map(t => t.id).sort(), c.want, c.name);
    assert.equal(state.filteredTicketCount(), c.want.length, c.name);
  }
});

test("a sidebar row writes its token into the filter box and takes it back out", () => {
  const state = loadKontoraState();
  state.configCache = { projects: FILTER_PROJECTS, pipelines: [], agents: [] };
  state.tickets = FILTER_TICKETS;

  state.toggleFilterToken("project", "agento11y");
  assert.equal(state.searchQuery, "project:=agento11y");
  assert.equal(state.filterTokenActive("project", "agento11y"), true);
  assert.equal(state.filterTokenActive("project", "kontora"), false);
  assert.equal(state.filterTokenActive("agent", "agento11y"), false);

  // Clicking the active row again clears the box, so the board comes back whole.
  state.toggleFilterToken("project", "agento11y");
  assert.equal(state.searchQuery, "");
  assert.equal(state.filterTokenActive("project", "agento11y"), false);
  state.recomputeBoard();
  assert.equal(state.filteredTicketCount(), FILTER_ALL.length);

  // Clicking another row replaces the query rather than adding to it.
  state.toggleFilterToken("project", "kontora");
  state.toggleFilterToken("agent", "claude");
  assert.equal(state.searchQuery, "agent:=claude");

  // The row selects the name it shows, not every name that contains it.
  state.recomputeBoard();
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-1"]);

  // A name with a space in it is quoted, so the row it wrote is still the whole
  // query and a second click empties the box.
  state.toggleFilterToken("project", "my notes");
  assert.equal(state.searchQuery, 'project:="my notes"');
  assert.equal(state.filterTokenActive("project", "my notes"), true);
  state.recomputeBoard();
  assert.deepEqual(bt(state, "in_progress").map(t => t.id), ["kon-5"]);
  state.toggleFilterToken("project", "my notes");
  assert.equal(state.searchQuery, "");
});

test("no sidebar row is active once the query says more than the row does", () => {
  const state = loadKontoraState();
  const cases = [
    { name: "the token alone", q: "project:=agento11y", want: true },
    { name: "the token with free text", q: "project:=agento11y fonts", want: false },
    { name: "the token with another key", q: "project:=agento11y agent:=claude", want: false },
    { name: "the token repeated", q: "project:=agento11y project:=kontora", want: false },
    { name: "a different value", q: "project:=kontora", want: false },
    // A typed value narrows by substring, so it is not what the row writes.
    { name: "the value typed without the = form", q: "project:agento11y", want: false },
    { name: "an empty box", q: "", want: false },
  ];

  for (const c of cases) {
    state.searchQuery = c.q;
    assert.equal(state.filterTokenActive("project", "agento11y"), c.want, c.name);
  }
});

test("the sidebar project and agent rows are buttons wired to the filter tokens", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // A row is a button, so it takes focus and a keypress, not only a click. The
  // same row drives the archive's own filters while that view is open, so each
  // binding is a conditional rather than a bare call.
  assert.match(html, /<button[^>]*@click="currentView === 'archive' \? archiveToggleFacet\('project', p\.name\) : toggleFilterToken\('project', p\.name\)"/, "the project row is a button");
  assert.match(html, /<button[^>]*@click="currentView === 'archive' \? archiveToggleFacet\('agent', a\) : toggleFilterToken\('agent', a\)"/, "the agent row is a button");

  assert.ok(html.includes(`x-show="(configCache?.projects || []).length > 0"`), "no projects, no section");
  for (const wired of [
    `archiveFacetActive('project', p.name) : filterTokenActive('project', p.name)) ? 'bg-surface-850`,
    `:aria-pressed="currentView === 'archive' ? archiveFacetActive('project', p.name) : filterTokenActive('project', p.name)"`,
    `archiveFacetActive('agent', a) : filterTokenActive('agent', a)) ? 'bg-surface-850`,
    `:aria-pressed="currentView === 'archive' ? archiveFacetActive('agent', a) : filterTokenActive('agent', a)"`,
    `x-text="currentView === 'archive' ? archiveFacetCount('agent', a) : agentRunningCount(a)"`,
    `x-text="archiveFacetCount('project', p.name)"`,
  ]) {
    assert.ok(html.includes(wired), wired);
  }
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

test("a card's id carries a hover card only when the frontmatter names a relation", () => {
  const state = loadKontoraState();
  const card = (extra) => state._cardHTML(
    { id: "sta-rel", title: "Run it", status: "todo", kontora: true, pipeline: "kontora", ...extra },
    { key: "todo" },
  );

  // The title is already on the card and the status is the column, so a card
  // that only repeats them is noise.
  assert.equal(/data-tip-e/.test(card({})), false);
  assert.equal(/data-tip-e/.test(card({ deps: [], links: [] })), false);

  const html = card({
    parent: { id: "sta-epic" },
    deps: [{ id: "sta-blk" }, { id: "sta-blk2" }],
    links: [{ id: "sta-rel2" }],
  });
  assert.match(html, /data-tip-e="sta-rel"/);
  assert.match(html, /data-tip-e-body="under sta-epic · waits on sta-blk, sta-blk2 · related to sta-rel2"/);
});

test("_cardHTML shows the finish time on review cards", () => {
  const state = loadKontoraState();
  state.now = new Date("2026-05-19T11:00:00Z").getTime();
  const html = state._cardHTML(
    {
      id: "sta-rev", title: "Check it", status: "human_review", kontora: true, pipeline: "kontora",
      created_at: "2026-05-19T06:00:00Z", updated_at: "2026-05-19T10:30:00Z",
      finished_at: "2026-05-19T09:00:00Z",
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

test("the ticket page's identity block carries the status chip and pipeline tag", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // shrink-0 pins these to the desktop page's identity block; the mobile
  // overlay's own chips carry neither class, so a match here cannot come from
  // the wrong layer.
  assert.match(html, /class="status-chip shrink-0"[^>]*:data-status="selectedTicket\?\.status"/);
  assert.match(html, /class="pipe-tag shrink-0"\s*\n\s*:data-pipe-color="selectedTicket \? ticketPipeColor\(selectedTicket\) : 'none'"/);
});

test("parseTitleTag extracts a [tag] title prefix and falls back to the project basename", () => {
  const state = loadKontoraState();

  assert.deepEqual({ ...state.parseTitleTag({ title: "[api] Add retry backoff", path: "/x/proj" }) },
    { tag: "api", rest: "Add retry backoff" });
  assert.deepEqual({ ...state.parseTitleTag({ title: "No prefix here", path: "/x/proj" }) },
    { tag: "proj", rest: "No prefix here" });
  assert.deepEqual({ ...state.parseTitleTag({ title: "No prefix" }) },
    { tag: null, rest: "No prefix" });

  // The half without the fallback, which the hover card calls: a ref has no
  // path, so a basename would make one id read two ways.
  assert.deepEqual({ ...state.splitTitleTag("[api] Add retry backoff") },
    { tag: "api", rest: "Add retry backoff" });
  assert.deepEqual({ ...state.splitTitleTag("No prefix here") },
    { tag: null, rest: "No prefix here" });
  assert.deepEqual({ ...state.splitTitleTag(undefined) }, { tag: null, rest: "" });
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
    finished_at: "2026-05-19T09:00:00Z",
  };
  const col = { key: "human_review" };
  const sig = state._cardSig(base, col);

  const relanded = { ...base, finished_at: "2026-05-19T12:00:00Z" };
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
  const source = uiSource;
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

// The card fields are read off list entries, which boardEntry has already
// stripped. A field added to boardEntry that a card renders would blank that
// part of every card at runtime, and _cardSig's static scan would not see it.
test("boardEntry drops no field the cards render", () => {
  const { fields, source } = cardRenderReads();
  const dropped = methodBody(source, "boardEntry").match(/var \{([^}]*?),\s*\.\.\.rest \}/);
  assert.ok(dropped, "boardEntry must destructure the dropped fields into rest");

  const names = dropped[1].split(",").map((s) => s.trim());
  // Sanity: the parse found the names, so an empty result below means covered.
  assert.ok(names.includes("body"), `expected body among ${names.join(", ")}`);
  assert.deepEqual(names.filter((n) => fields.has(n)), [], `boardEntry drops rendered fields: ${names.join(", ")}`);
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
    created_at: "2026-05-19T06:00:00Z", finished_at: finish,
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
  state.tickets[0].finished_at = "2026-05-19T06:30:00Z";
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

test("a drag that only opens the init modal puts the card back", async () => {
  const cases = [
    {
      name: "target column has cards",
      others: [{ id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" }],
      target: ["kon-a"],
    },
    { name: "target column is empty", others: [], target: [] },
  ];

  for (const c of cases) {
    let opts = null;
    const { board, state } = renderedBoard(
      [{ id: "ext-1", title: "External", status: "open", kontora: false }, ...c.others],
      { Sortable: class { constructor(el, o) { opts = o; } option() {} } },
    );
    state.$nextTick = () => Promise.resolve();
    state.initSortable(board.els["col-open"]);
    assert.ok(opts && opts.onEnd, `${c.name}: Sortable took no handlers`);

    // Sortable has already moved the card node into the target column when
    // onEnd runs. ext-1 is not a kontora ticket, so moveTask opens the init
    // modal and leaves the status alone instead of posting the move: nothing
    // in the board data changed, and only the dropped render cache makes the
    // card go back where it came from.
    const card = board.els["col-open"].children[0];
    board.els["col-in_progress"].insertBefore(card, null);
    opts.onEnd({
      item: card,
      from: { dataset: { dropStatus: "open" } },
      to: { dataset: { dropStatus: "todo" } },
    });
    await flushMicrotasks();

    assert.equal(state.initModal, true, `${c.name}: init modal`);
    assert.equal(state.tickets[0].status, "open", `${c.name}: ticket status`);
    assert.deepEqual(board.ids("open"), ["ext-1"], `${c.name}: source column`);
    assert.deepEqual(board.ids("in_progress"), c.target, `${c.name}: target column`);
  }
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
  const app = uiSource;

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

// These tests need containment, not "both strings are somewhere in the file",
// and there is no DOM parser here. Strip comments first so a <div> mentioned in
// prose cannot shift the depth count, then walk <div>/</div> depth.
function htmlNoComments() {
  return fs.readFileSync(htmlPath, "utf8").replace(/<!--[\s\S]*?-->/g, "");
}

// elementAt returns the whole element that starts at or before idx, from its
// opening <div to its matching </div>.
function elementAt(html, idx) {
  const start = html.lastIndexOf("<div", idx);
  assert.ok(start >= 0, "no enclosing <div");
  const re = /<div\b|<\/div>/g;
  re.lastIndex = start;
  let depth = 0;
  let m;
  while ((m = re.exec(html)) !== null) {
    depth += m[0] === "</div>" ? -1 : 1;
    if (depth === 0) return html.slice(start, re.lastIndex);
  }
  throw new Error("unbalanced <div> from index " + start);
}

function elementWithId(html, id) {
  const idx = html.indexOf(`id="${id}"`);
  assert.ok(idx > 0, `${id} not found`);
  return elementAt(html, idx);
}

// The empty-board panels key off the raw list. _boardTotal and the column lists
// both go to zero when a search matches nothing, which would replace the
// filtered-empty message for someone who already has tickets.
test("index.html gates the empty-board panel on the raw ticket list", () => {
  const html = htmlNoComments();

  const desktop = elementWithId(html, "board-empty-desktop");
  assert.match(desktop, /x-show="tickets\.length === 0"/);
  // The panel covers the board instead of replacing it: it is a sibling of
  // #board-cols inside the same board area, so Alpine keeps the column DOM and
  // its Sortable bindings mounted while the panel shows.
  assert.equal(desktop.includes('id="board-cols"'), false);
  const panelStart = html.lastIndexOf("<div", html.indexOf('id="board-empty-desktop"'));
  const boardArea = elementAt(html, panelStart - 1);
  assert.ok(boardArea.includes('id="board-cols"'), "panel and columns share the board area");

  // Desktop inherits !loading from the board view; mobile has no such wrapper,
  // so its own gate carries it. Without that, every phone load paints the
  // panel while the first fetch is still in flight.
  const boardView = elementAt(html, html.indexOf(`x-show="!loading && currentView === 'board'"`));
  assert.ok(boardView.includes('id="board-empty-desktop"'), "desktop panel sits under the !loading board view");
  const mobileGate = '<template x-if="!loading && tickets.length === 0">';
  const mobileIdx = html.indexOf(mobileGate);
  assert.ok(mobileIdx > 0, "mobile panel needs its own !loading gate");
  assert.match(html.slice(mobileIdx + mobileGate.length, mobileIdx + mobileGate.length + 40), /^\s*<div id="board-empty-mobile"/);

  // Mobile's per-column empty state stands down while the panel shows, so the
  // two never stack.
  assert.match(html, /x-if="tickets\.length > 0 && boardTickets\(mobileColumn\(\)\.key\)\.length === 0"/);
});

// The copy path needs a secure context, which plain HTTP over a tailnet is not,
// so the request has to be readable on screen and not only on the clipboard.
test("index.html shows the coding-agent request in both empty-board panels", () => {
  const html = htmlNoComments();

  for (const id of ["board-empty-desktop", "board-empty-mobile"]) {
    const panel = elementWithId(html, id);
    assert.equal([...panel.matchAll(/x-text="agentSetupRequest"/g)].length, 1, `${id} must display the request`);
    assert.equal([...panel.matchAll(/@click="copyCmd\(agentSetupRequest\)"/g)].length, 1, `${id} must offer one copy action`);
  }

  assert.equal([...elementWithId(html, "board-empty-desktop").matchAll(/@click="gotoView\('new'\)"/g)].length, 1);
  assert.equal([...elementWithId(html, "board-empty-mobile").matchAll(/@click="openNewSheet\(\)"/g)].length, 1);
});

test("the coding-agent setup request names the command that prints the brief", () => {
  const state = loadKontoraState();

  assert.equal(
    state.agentSetupRequest,
    "Run `kontora setup --agent` on the Kontora daemon host and follow its instructions.",
  );
});

test("the empty-board copy action goes through copyCmd", () => {
  let copied = null;
  const state = loadKontoraState({
    navigator: { clipboard: { writeText(v) { copied = v; } } },
  });

  state.copyCmd(state.agentSetupRequest);

  assert.equal(copied, state.agentSetupRequest);
  assert.equal(state.copiedCmd, state.agentSetupRequest);
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
  // 600 of content besides the editor, so the 700 the editor adds is the only
  // reason a scrollTop of 900 is reachable.
  const dom = fakeBodyDom({ body, scrollFollowsEditor: true, scrollHeight: 600, clientHeight: 400, scrollTop: 900 });
  const state = bodyEditState(dom, body);
  dom.textarea.style.height = "700px";

  // The measurement collapses the editor, which is what makes the panel clamp.
  // Zeroing the height alone leaves the box at its min-height floor, so the
  // measurement has to clear that too or every short prefix reads 100.
  dom.textarea.style.height = "0px";
  assert.equal(dom.scroller.scrollHeight, 600);
  assert.equal(dom.scroller.scrollTop, 200);
  assert.equal(dom.textarea.clientHeight, MIN_HEIGHT);
  dom.textarea.style.height = "700px";
  dom.scroller.scrollTop = 900;

  // Offset 19 starts "para two" on the fifth line: border + padding + 4 lines.
  assert.equal(state.caretLineTop(dom.textarea, 19), BORDER + PAD + 4 * LINE);
  assert.equal(state.caretLineTop(dom.textarea, 0), BORDER + PAD);
  assert.equal(dom.textarea.value, body);
  assert.equal(dom.textarea.style.height, "700px");
  assert.equal(dom.textarea.style.minHeight, `${MIN_HEIGHT}px`);
  assert.equal(dom.scroller.scrollTop, 900);

  // A textarea with no scroller above it still measures.
  dom.textarea.parentElement = null;
  assert.equal(state.caretLineTop(dom.textarea, 19), BORDER + PAD + 4 * LINE);
  assert.equal(dom.textarea.value, body);
  assert.equal(dom.textarea.style.height, "700px");

  // A caller that already resolved the panel passes it in, and the scroll
  // position comes back although there is no ancestor to walk to.
  dom.scroller.scrollTop = 900;
  assert.equal(state.caretLineTop(dom.textarea, 19, dom.scroller), BORDER + PAD + 4 * LINE);
  assert.equal(dom.scroller.scrollTop, 900);
});

test("autoGrowBody sizes the editor without moving the panel", () => {
  const body = "# Title\n\n" + "one\n".repeat(33);
  // Only the editor makes this panel scrollable, so the 'auto' step of the
  // grow shrinks the content to 600 and clamps the panel from 900 to 200.
  const dom = fakeBodyDom({ body, scrollFollowsEditor: true, scrollHeight: 600, clientHeight: 400, scrollTop: 900 });
  const state = bodyEditState(dom, body);
  dom.textarea.style.height = "700px";

  state.autoGrowBody(dom.textarea);

  assert.equal(dom.textarea.style.height, `${body.split("\n").length * LINE + 2 * PAD}px`);
  assert.equal(dom.scroller.scrollTop, 900);
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
  // A heading and two 33-line paragraphs: 70 lines, so the editor is 1416 tall
  // and by itself makes a 400-tall panel scrollable.
  const long = "# Title\n\n" + "one\n".repeat(33) + "\n" + "two\n".repeat(33);
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
    {
      // Only the editor makes this panel scrollable, so measuring the caret
      // line collapses the content from 2016 to 600 and the panel clamps from
      // 900 to 200. An anchor taken across that clamp is 700 short, which puts
      // the reader back at the top.
      name: "a caret halfway down a ticket the editor makes scrollable",
      body: long, blockHeights: [40, 40, 40], caret: 150,
      scrollFollowsEditor: true, scrollTop: 900, scrollHeight: 600, editorHeight: "1416px",
      // The third block starts at 142 on the 37th line, so a caret at 150
      // belongs to it and its line sat at screen Y 500 + 729 - 900. The
      // rendered block is 580 into the content.
      wantTop: 580 - (500 + BORDER + PAD + 36 * LINE - 900),
    },
  ];

  for (const c of cases) {
    const dom = fakeBodyDom({
      body: c.body,
      blockHeights: c.blockHeights,
      scrollTop: c.scrollTop ?? 300,
      scrollHeight: c.scrollHeight ?? 1500,
      scrollFollowsEditor: !!c.scrollFollowsEditor,
    });
    const state = bodyEditState(dom, c.body);
    state.editingBody = true;
    state.$refs.bodyEditor = dom.textarea;
    dom.textarea.selectionStart = c.caret;
    dom.textarea.style.height = c.editorHeight ?? "136px";
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
  const updated = { id: "kon-1", title: "One", status: "paused", kontora: true, body: "typed" };
  let sent = null;
  const state = loadKontoraState({
    fetch: async (url, options) => {
      sent = JSON.parse(options.body);
      return { ok: true, json: async () => updated };
    },
  });
  state.tickets = [{ id: "kon-1", title: "One", status: "todo", kontora: true }];
  state.selectedTicket = { id: "kon-1", title: "One", status: "todo", kontora: true, body: "old" };
  state.editing = true;
  state.editForm = { body: "typed", pipeline: "", path: "", agent: "", branch: "" };

  // closeDetail flushes the pending body, then clears the selection while the
  // request is still in flight.
  const pending = state.saveEdit();
  state.selectedTicket = null;
  await pending;

  assert.equal(state.selectedTicket, null);
  assert.equal(state.editSubmitting, false);
  assert.equal(sent.body, "typed");
  assert.equal(state.tickets[0].status, "paused");
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

// A ticket file separates its frontmatter from its body with a blank line, so
// the body the API returns begins with that newline. The source editor must not
// open on an empty first line, and a save must put the newline back or the file
// loses the blank line. A body that arrived without one stays without one.
test("the editor hides the newline the frontmatter left on the body", async () => {
  const cases = [
    { name: "separator newline", body: "\n# Title\n", shown: "# Title\n" },
    { name: "no separator newline", body: "# Title\n", shown: "# Title\n" },
    { name: "a second newline is a blank line the user wrote", body: "\n\n# Title\n", shown: "\n# Title\n" },
    { name: "empty body", body: "", shown: "" },
  ];
  for (const c of cases) {
    let sent = null;
    const state = loadKontoraState({
      fetch: async (url, options) => {
        sent = JSON.parse(options.body);
        return { ok: true, json: async () => ({ id: "kon-1", status: "open", body: c.body + "typed" }) };
      },
    });
    state.configCache = { projects: [], pipelines: [], agents: [] };
    state.$nextTick = async () => {};
    state.selectedTicket = { id: "kon-1", status: "open", body: c.body };
    state.tickets = [{ id: "kon-1", status: "open" }];

    await state.startEditing();
    assert.equal(state.editForm.body, c.shown, c.name);

    // Nothing typed: the body is unchanged, so the request carries no body at
    // all and the file keeps its bytes.
    await state.saveEdit();
    assert.equal(sent, null, c.name);

    state.editForm.body += "typed";
    await state.saveEdit();
    assert.equal(sent.body, c.body + "typed", c.name);
  }
});

// Two projects with different defaults, so a retarget has something to move
// between.
const EDIT_PROJECTS = [
  { name: "kontora", path: "~/projects/kontora", resolved_path: "/home/u/projects/kontora", pipeline: "implement", agent: "claude" },
  { name: "widget-api", path: "~/projects/widget-api", resolved_path: "/home/u/projects/widget-api", pipeline: "commit-no-push", agent: "pi-opus" },
];

// The edit form as startEditing leaves it for a ticket at fromPath, with the
// project defaults for that path recorded as what the fields inherited.
function editPathState(fromPath, fields) {
  const state = loadKontoraState();
  state.configCache = { projects: EDIT_PROJECTS, pipelines: [], agents: [] };
  state.selectedTicket = { id: "kon-1", status: "open", path: fromPath, ...fields };
  state.editing = true;
  state.editForm = { body: "", path: fromPath, pipeline: "", agent: "", branch: "", ...fields };
  state._editInherited = state.projectDefaultsFor(fromPath);
  state.saveEdit = () => { state.savedPath = state.editForm.path; };
  return state;
}

test("the edit form applies project defaults without changing the branch", () => {
  const cases = [
    {
      name: "blank fields inherit from the project",
      from: "", fields: {}, to: "~/projects/kontora",
      want: { pipeline: "implement", agent: "claude", branch: "" },
    },
    {
      name: "an absolute path matches the same project",
      from: "", fields: {}, to: "/home/u/projects/kontora",
      want: { pipeline: "implement", agent: "claude", branch: "" },
    },
    {
      name: "inherited values follow the retarget",
      from: "~/projects/kontora",
      fields: { pipeline: "implement", agent: "claude", branch: "" },
      to: "~/projects/widget-api",
      want: { pipeline: "commit-no-push", agent: "pi-opus", branch: "" },
    },
    {
      name: "values the user chose are kept",
      from: "~/projects/kontora",
      fields: { pipeline: "review-only", agent: "codex", branch: "wip/experiment" },
      to: "~/projects/widget-api",
      want: { pipeline: "review-only", agent: "codex", branch: "wip/experiment" },
    },
    {
      name: "a path outside every project clears inherited defaults",
      from: "~/projects/kontora",
      fields: { pipeline: "implement", agent: "claude", branch: "old/branch" },
      to: "~/projects/other",
      want: { pipeline: "", agent: "", branch: "old/branch" },
    },
  ];

  for (const c of cases) {
    const state = editPathState(c.from, c.fields);
    state.editForm.path = c.to;

    state.onEditPathChange();

    assert.equal(state.editForm.pipeline, c.want.pipeline, c.name);
    assert.equal(state.editForm.agent, c.want.agent, c.name);
    assert.equal(state.editForm.branch, c.want.branch, c.name);
    assert.equal(state.savedPath, c.to, `${c.name}: saved`);
  }
});

test("the init form never fills or replaces the branch", async () => {
  for (const branch of ["", "my/custom-branch"]) {
    const state = loadKontoraState();
    state.configCache = { projects: EDIT_PROJECTS, pipelines: [], agents: [] };
    state.$nextTick = async () => {};

    await state.openInitModal({ id: "kon-1", path: "~/projects/kontora", branch });
    assert.equal(state.initForm.branch, branch);
    assert.equal(state.initForm.pipeline, "implement");

    state.initForm.path = "~/projects/widget-api";
    state.onInitPathChange();
    assert.equal(state.initForm.branch, branch);
    assert.equal(state.initForm.pipeline, "commit-no-push");
  }
});

test("create and init requests omit empty branches and send custom branches", async () => {
  for (const branch of ["", "my/custom-branch"]) {
    const sent = [];
    const state = loadKontoraState({
      fetch: async (_url, options) => {
        sent.push(JSON.parse(options.body));
        return { ok: true, json: async () => ({}) };
      },
    });
    state.closeCreateModal = () => {};
    state.closeInitModal = () => {};
    state.createForm = {
      title: "Fix retry",
      path: "/repos/api",
      pipeline: "",
      agent: "",
      status: "todo",
      body: "",
      branch,
      base_branch: "",
    };
    state.initForm = {
      ticketId: "kon-1",
      path: "/repos/api",
      pipeline: "",
      agent: "",
      branch,
    };

    await state.submitCreateTicket();
    await state.submitInitTicket();

    assert.equal(sent.length, 2);
    for (const body of sent) {
      if (branch) {
        assert.equal(body.branch, branch);
      } else {
        assert.equal(Object.hasOwn(body, "branch"), false);
      }
    }
  }
});

test("edit requests omit an unchanged empty branch and send a custom branch", async () => {
  const cases = [
    { name: "empty branch", selected: "", edited: "", wantBranch: undefined },
    { name: "custom branch", selected: "", edited: "my/custom-branch", wantBranch: "my/custom-branch" },
  ];

  for (const c of cases) {
    let sent = null;
    const state = loadKontoraState({
      fetch: async (_url, options) => {
        sent = JSON.parse(options.body);
        return { ok: true, json: async () => ({ id: "kon-1", path: "/repos/new", branch: c.edited }) };
      },
    });
    state.selectedTicket = { id: "kon-1", path: "/repos/old", branch: c.selected };
    state.editing = true;
    state.editForm = {
      body: "",
      pipeline: "",
      path: "/repos/new",
      agent: "",
      branch: c.edited,
      base_branch: "",
    };
    state.tickets = [];

    await state.saveEdit();

    assert.ok(sent, c.name);
    if (c.wantBranch) {
      assert.equal(sent.branch, c.wantBranch, c.name);
    } else {
      assert.equal(Object.hasOwn(sent, "branch"), false, c.name);
    }
  }
});

test("index.html applies project defaults and leaves branch naming to the daemon", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /<input type="text" x-model="editForm\.path" @change="onEditPathChange\(\)"/);
  // The create form has no ticket ID yet, so it is the only branch field that
  // states the rule instead of naming the branch.
  assert.equal((html.match(/placeholder="daemon assigns branch when run starts"/g) || []).length, 1);
  // The init modal states the rule in its placeholder and shows the resolved
  // name in a preview row of its own instead.
  assert.match(html, /placeholder="override the automatic name"/);
  assert.match(html, /x-text="initBranchPlaceholder\(\)"/);
  assert.match(html, /:placeholder="branchPlaceholder\(selectedTicket\?\.auto_branch\)"/);
  assert.doesNotMatch(html, /auto-generate from ticket ID/);
});

test("the init modal's header band names the transition the ticket is about to make", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // A ticket already in todo would read "todo → todo · queued", and a ticket
  // with no status would draw an empty pill, so the source chip and the arrow
  // between them drop out together in both cases.
  assert.match(html, /<template x-if="initForm\.status && initForm\.status !== 'todo'">/);
  assert.match(html, /x-text="statusLabel\(initForm\.status\)"/);
  assert.match(html, /data-status="in_progress">todo · queued</);
  // The pulsing dot belongs to a run in flight, not to a queued ticket, and
  // .status-chip-sm is the pair that cancels it.
  assert.equal((html.match(/class="status-chip status-chip-sm" (:data-status="initForm\.status"|data-status="in_progress")/g) || []).length, 2);
  assert.match(html, /<h2 id="init-modal-title"/);
  // The modal's tag differs from a card's in size alone, so it adds a class to
  // .title-tag instead of restating the family, weight and colour.
  assert.match(html, /class="title-tag init-title-tag" :data-pipe-color="pipelineColorByName\(initForm\.tag\)"/);
  assert.match(html, /\.title-tag\.init-title-tag \{ font-size: 13px; \}/);
});

test("the init modal states what starting the ticket does", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // Both the strip's trailing note and the footer's run line have to name the
  // "none" pipeline as a single pass rather than as zero stages.
  assert.ok(html.includes(`x-text="initStages().length ? 'starts at ' + initStages()[0] : 'single run, no stages'"`));
  assert.ok(html.includes(`initStages().length + (initStages().length === 1 ? ' stage' : ' stages') : 'a single pass'`));
  assert.ok(html.includes(`x-text="initForm.agent || 'the agent each stage picks'"`));
  // The stages moved out of the select's option labels and into the strip.
  const select = html.slice(html.indexOf('id="init-pipeline"'));
  const options = select.slice(0, select.indexOf("</select>"));
  assert.ok(options.includes(`<option value="">none (single run, no stages)</option>`));
  assert.ok(options.includes(`<option :value="p" x-text="p">`));
  assert.equal(options.includes("pipelineLabel"), false);
  // Agent shares a row with Path, and the create form's wording does not fit
  // the half-width select.
  const agent = html.slice(html.indexOf('id="init-agent"'));
  assert.ok(agent.slice(0, agent.indexOf("</select>")).includes(`<option value="">default (per stage)</option>`));
});

test("the init modal's error line is its own, not the app-wide one", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The toast at the bottom right renders `error` and clears it after 10s. The
  // modal outlives a failed start, so it binds a field of its own.
  assert.match(html, /x-show="initError" class="text-\[11px\] text-err" x-text="initError"/);
  const modal = html.slice(html.indexOf('x-show="initModal"'));
  assert.equal(modal.slice(0, modal.indexOf("</form>")).includes(`x-text="error"`), false);
});

test("the init modal's error line reports the start it is showing and nothing else", async () => {
  const ticket = { id: "kon-1", status: "open", path: "~/projects/kontora" };
  const cases = [
    {
      name: "an unrelated failure still inside the toast's window",
      error: "Delete failed",
      wantInitError: null,
      wantError: "Delete failed",
      wantModal: true,
    },
    {
      name: "the server's reason, with the modal left open to fix it",
      submit: true,
      fetch: async () => ({ ok: false, json: async () => ({ error: "path is not a git repository" }) }),
      wantInitError: "path is not a git repository",
      wantModal: true,
    },
    {
      name: "a rejection that names no reason",
      submit: true,
      fetch: async () => ({ ok: false, json: async () => ({}) }),
      wantInitError: "Failed to start ticket",
      wantModal: true,
    },
    {
      name: "a request that never reached the daemon",
      submit: true,
      fetch: async () => { throw new Error("offline"); },
      wantInitError: "Failed to start ticket: offline",
      wantModal: true,
    },
    {
      name: "a start that works closes the modal",
      submit: true,
      fetch: async () => ({ ok: true, json: async () => ({}) }),
      wantInitError: null,
      wantModal: false,
    },
    {
      name: "reopening drops the previous failure",
      submit: true,
      fetch: async () => ({ ok: false, json: async () => ({ error: "path is not a git repository" }) }),
      reopen: true,
      wantInitError: null,
      wantModal: true,
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState(c.fetch ? { fetch: c.fetch } : {});
    state.configCache = { projects: EDIT_PROJECTS, pipelines: [], agents: [] };
    state.$nextTick = async () => {};
    if (c.error) state.error = c.error;

    await state.openInitModal(ticket);
    if (c.submit) await state.submitInitTicket();
    if (c.reopen) await state.openInitModal(ticket);

    assert.equal(state.initError, c.wantInitError, c.name);
    assert.equal(state.error, c.error || null, c.name);
    assert.equal(state.initModal, c.wantModal, c.name);
    assert.equal(state.initSubmitting, false, c.name);
  }
});

test("the pipeline badge names a project only when the pipeline came from it", async () => {
  const cases = [
    {
      name: "a value inherited from the path's project",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      want: "kontora",
    },
    {
      name: "a value the ticket carries itself",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", pipeline: "commit-no-push" },
      want: null,
    },
    {
      name: "a value the user picked in the select",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      pipeline: "commit-no-push",
      want: null,
    },
    {
      name: "the none pipeline",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      pipeline: "",
      want: null,
    },
    {
      name: "a path no project owns",
      ticket: { id: "kon-1", status: "open", path: "~/scratch", pipeline: "implement" },
      want: null,
    },
    {
      name: "a retargeted path re-inherits, and the badge follows",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      retarget: "~/projects/widget-api",
      want: "widget-api",
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = { projects: EDIT_PROJECTS, pipelines: ["implement", "commit-no-push"], agents: [] };
    state.$nextTick = async () => {};

    await state.openInitModal(c.ticket);
    if (c.retarget) {
      state.initForm.path = c.retarget;
      state.onInitPathChange();
    }
    if (c.pipeline !== undefined) state.initForm.pipeline = c.pipeline;

    assert.equal(state.initPipelineProject()?.name ?? null, c.want, c.name);
  }
});

test("the init form's derived values follow the pipeline, the path and the branch field", async () => {
  const infos = [
    { name: "implement", stages: ["implement", "review", "fix-review", "commit"] },
    { name: "commit-no-push", stages: ["implement", "commit"] },
  ];
  const cases = [
    {
      name: "a pipeline with stages",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      wantStages: ["implement", "review", "fix-review", "commit"],
      wantBranchResolved: true,
    },
    {
      name: "no pipeline selected",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      pipeline: "",
      wantStages: [],
      wantBranchResolved: true,
    },
    {
      name: "a pipeline the config does not know",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      pipeline: "gone",
      wantStages: [],
      wantBranchResolved: true,
    },
    {
      name: "a branch typed by hand overrides the automatic name",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      branch: "  my-branch  ",
      wantStages: ["implement", "review", "fix-review", "commit"],
      wantBranchResolved: false,
    },
    {
      name: "a retargeted path re-resolves the pipeline and drops the preview",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      retarget: "~/projects/widget-api",
      wantStages: ["implement", "commit"],
      wantBranchResolved: false,
    },
    {
      name: "the server named no branch",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      wantStages: ["implement", "review", "fix-review", "commit"],
      wantBranchResolved: false,
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = { projects: EDIT_PROJECTS, pipelines: ["implement", "commit-no-push"], agents: [], pipeline_infos: infos };
    state.$nextTick = async () => {};

    await state.openInitModal(c.ticket);
    if (c.retarget) {
      state.initForm.path = c.retarget;
      state.onInitPathChange();
    }
    if (c.pipeline !== undefined) state.initForm.pipeline = c.pipeline;
    if (c.branch !== undefined) state.initForm.branch = c.branch;

    assert.deepEqual(vmValue(state.initStages()), c.wantStages, c.name);
    assert.equal(state.initBranchResolved(), c.wantBranchResolved, c.name);
  }
});

test("openInitModal carries the ticket's status and parsed title into the header band", async () => {
  const cases = [
    {
      name: "a literal [tag] prefix",
      ticket: { id: "kon-1", status: "open", title: "[widget-api] Bound the eval worker", path: "~/projects/kontora" },
      wantStatus: "open",
      wantTag: "widget-api",
      wantRest: "Bound the eval worker",
    },
    {
      name: "no prefix falls back to the path basename",
      ticket: { id: "kon-1", status: "todo", title: "Bound the eval worker", path: "~/projects/kontora" },
      wantStatus: "todo",
      wantTag: "kontora",
      wantRest: "Bound the eval worker",
    },
    {
      name: "no title and no path",
      ticket: { id: "kon-1" },
      wantStatus: "",
      wantTag: "",
      wantRest: "",
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = { projects: EDIT_PROJECTS, pipelines: [], agents: [] };
    state.$nextTick = async () => {};

    await state.openInitModal(c.ticket);

    assert.equal(state.initForm.status, c.wantStatus, c.name);
    assert.equal(state.initForm.tag, c.wantTag, c.name);
    assert.equal(state.initForm.titleRest, c.wantRest, c.name);
  }
});

test("an empty branch field shows the name the daemon would assign", async () => {
  const generic = "daemon assigns branch when run starts";
  const cases = [
    {
      name: "the ticket's own path",
      ticket: { id: "kon-1", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      want: "kontora/fix-retry-kon-1",
    },
    {
      name: "a retargeted path drops the name",
      ticket: { id: "kon-1", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      retarget: "~/projects/widget-api",
      want: generic,
    },
    {
      name: "a trailing slash is the same path",
      ticket: { id: "kon-1", path: "~/projects/kontora", auto_branch: "kontora/fix-retry-kon-1" },
      retarget: "~/projects/kontora/",
      want: "kontora/fix-retry-kon-1",
    },
    {
      name: "no name from the server",
      ticket: { id: "kon-1", path: "~/projects/kontora" },
      want: generic,
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = { projects: EDIT_PROJECTS, pipelines: [], agents: [] };
    state.$nextTick = async () => {};

    await state.openInitModal(c.ticket);
    if (c.retarget) {
      state.initForm.path = c.retarget;
      state.onInitPathChange();
    }

    assert.equal(state.initBranchPlaceholder(), c.want, c.name);
    assert.equal(state.branchPlaceholder(c.ticket.auto_branch), c.ticket.auto_branch || generic, `${c.name}: edit form`);
  }
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
      act: (s) => s.switchTab("session"),
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
  // 148px is the content box of a 150px .md-editor, which is the height the
  // rendered body it replaces holds when the ticket has no description yet.
  assert.match(textarea, /min-h-\[148px\]/);
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
  // Two writers share the pattern, counted apart: entity chips belong to the
  // stage summary, and a ticket body swapped onto setSummaryProse would keep
  // any combined total the same. Ten: the assistant's own prose runs through
  // the same write in both layers, and a streaming pane is exactly the case a
  // rebuild-per-effect would ruin; the archive's read-only ticket tab renders
  // its body through it too; and the notes tab's own note and reply bodies
  // are markdown like every other authored block.
  assert.equal(html.match(/x-effect="setProse\(\$el, /g).length, 10);
  assert.equal(html.match(/x-effect="setSummaryProse\(\$el, /g).length, 2);
  // Nothing in the markup writes a note as plain text any more: the rail block
  // that did is gone and the notes tab renders markdown.
  assert.equal(html.match(/x-effect="setNoteText\(\$el, /g), null);
  assert.equal(html.includes('x-text="n.text"'), false);
});


// ---------------------------------------------------------------------------
// Archive view

// One archived row, with the fields the list renders. Overrides win.
function archiveRow(over = {}) {
  return Object.assign({
    id: "kon-001",
    title: "Old work",
    project: "web",
    pipeline: "default",
    agent: "claude",
    branch: "kontora/kon-001",
    status: "done",
    wall_seconds: 600,
    archived_at: "2026-08-20T09:00:00Z",
    archived_by: "web",
  }, over);
}

const ARCHIVE_DEFAULTS = {
  query: "", status: "all", project: "all", pipeline: "all", agent: "all",
  range: "all", sortKey: "archived", sortDir: "desc",
};

test("archiveDerive filters on the search box, the pills and the date range", () => {
  const { ctx } = loadKontoraContext();
  const rows = [
    archiveRow({ id: "kon-001", project: "web", agent: "claude", status: "done", archived_at: "2026-08-20T09:00:00Z" }),
    archiveRow({ id: "kon-002", project: "web-sdk", agent: "codex", status: "cancelled", archived_at: "2026-08-19T09:00:00Z", title: "Drop the shim" }),
    archiveRow({ id: "kon-003", project: "api", agent: "claude", status: "done", archived_at: "2026-01-01T09:00:00Z", pipeline: "review", branch: "kontora/kon-003" }),
  ];
  // Two days after the newest row, so the 7-day window holds the first two.
  const now = Date.parse("2026-08-22T09:00:00Z");

  const cases = [
    { name: "an empty box lists everything", state: {}, want: ["kon-001", "kon-002", "kon-003"] },
    { name: "free text matches the id", state: { query: "kon-002" }, want: ["kon-002"] },
    { name: "free text matches the title, case-insensitively", state: { query: "DROP THE" }, want: ["kon-002"] },
    { name: "free text matches the branch", state: { query: "kontora/kon-003" }, want: ["kon-003"] },
    { name: "a substring token widens to both projects", state: { query: "project:web" }, want: ["kon-001", "kon-002"] },
    { name: "the = form takes the whole field", state: { query: "project:=web" }, want: ["kon-001"] },
    { name: "tokens of different keys are ANDed", state: { query: "project:web agent:claude" }, want: ["kon-001"] },
    { name: "repeats of one key widen it", state: { query: "agent:claude agent:codex" }, want: ["kon-001", "kon-002", "kon-003"] },
    { name: "a status token reads archived_from", state: { query: "status:cancelled" }, want: ["kon-002"] },
    { name: "a pipeline token narrows", state: { query: "pipeline:review" }, want: ["kon-003"] },
    { name: "an unknown key falls through to free text", state: { query: "worktree:web" }, want: [] },
    { name: "a key with nothing after the colon constrains nothing", state: { query: "project:" }, want: ["kon-001", "kon-002", "kon-003"] },
    { name: "a pill is exact where a token is a substring", state: { project: "web" }, want: ["kon-001"] },
    { name: "the status pill narrows", state: { status: "cancelled" }, want: ["kon-002"] },
    { name: "the agent pill narrows", state: { agent: "codex" }, want: ["kon-002"] },
    { name: "the date range drops what is older", state: { range: "7d" }, want: ["kon-001", "kon-002"] },
    { name: "a pill and the box are ANDed", state: { agent: "claude", query: "project:web" }, want: ["kon-001"] },
  ];

  for (const c of cases) {
    const out = ctx.archiveDerive(rows, Object.assign({}, ARCHIVE_DEFAULTS, c.state), now);
    assert.deepEqual(out.rows.map(r => r.id).sort(), c.want.slice().sort(), c.name);
    assert.equal(out.total, 3, c.name + " — the total counts every archived ticket");
    assert.equal(out.shown, c.want.length, c.name + " — shown counts what is listed");
  }
});

test("archiveDerive sorts by any of the six columns, newest archived first by default", () => {
  const { ctx } = loadKontoraContext();
  const rows = [
    archiveRow({ id: "kon-002", title: "b", project: "web", status: "done", wall_seconds: 30, archived_at: "2026-08-19T09:00:00Z" }),
    archiveRow({ id: "kon-001", title: "c", project: "api", status: "cancelled", wall_seconds: 900, archived_at: "2026-08-20T09:00:00Z" }),
    archiveRow({ id: "kon-003", title: "a", project: "sdk", status: "done", wall_seconds: 60, archived_at: "2026-08-18T09:00:00Z" }),
  ];

  const cases = [
    { sortKey: "archived", sortDir: "desc", want: ["kon-001", "kon-002", "kon-003"] },
    { sortKey: "archived", sortDir: "asc", want: ["kon-003", "kon-002", "kon-001"] },
    { sortKey: "id", sortDir: "asc", want: ["kon-001", "kon-002", "kon-003"] },
    { sortKey: "title", sortDir: "asc", want: ["kon-003", "kon-002", "kon-001"] },
    { sortKey: "project", sortDir: "asc", want: ["kon-001", "kon-003", "kon-002"] },
    { sortKey: "status", sortDir: "asc", want: ["kon-001", "kon-002", "kon-003"] },
    { sortKey: "wall", sortDir: "desc", want: ["kon-001", "kon-003", "kon-002"] },
  ];

  for (const c of cases) {
    const out = ctx.archiveDerive(rows, Object.assign({}, ARCHIVE_DEFAULTS, c), 0);
    assert.deepEqual(out.rows.map(r => r.id), c.want, c.sortKey + " " + c.sortDir);
  }
});

test("archiveDerive gives a row with no archived_from a status to filter and sort by", () => {
  const { ctx } = loadKontoraContext();
  const rows = [archiveRow({ id: "kon-001", status: "" })];

  const out = ctx.archiveDerive(rows, Object.assign({}, ARCHIVE_DEFAULTS, { status: "unknown" }), 0);
  assert.deepEqual(out.rows.map(r => r.id), ["kon-001"]);
});

test("the archive's parsed tokens carry the term they were typed as, so a chip can cut it back out", () => {
  const state = loadKontoraState();
  state.archiveRows = [archiveRow()];
  state.archiveQuery = 'fonts project:="web sdk" agent:codex';

  const tokens = state.archiveView().tokens;
  // Spread into a host array: the bundle runs in its own VM context, so an
  // array it built has a different Array prototype and deepEqual rejects it.
  assert.deepEqual([...tokens].map(t => [t.key, t.value, t.exact]), [["project", "web sdk", true], ["agent", "codex", false]]);

  state.archiveRemoveToken(tokens[0].term);
  assert.equal(state.archiveQuery, "fonts agent:codex");
});

test("the count line reports the shown total beside the archived total", () => {
  const state = loadKontoraState();
  state.archiveRows = [
    archiveRow({ id: "kon-001", project: "web" }),
    archiveRow({ id: "kon-002", project: "web-sdk" }),
    archiveRow({ id: "kon-003", project: "api" }),
  ];
  assert.equal(state.archiveCountLine(), "3 archived · 3 shown");

  state.archiveQuery = "project:=web";
  assert.equal(state.archiveCountLine(), "3 archived · 1 shown");
  assert.deepEqual(state.archiveView().rows.map(r => r.id), ["kon-001"]);
});

test("archiveSort flips the direction on the active key and picks a sensible one on a new key", () => {
  const state = loadKontoraState();
  assert.equal(state.archiveSortKey, "archived");
  assert.equal(state.archiveSortDir, "desc");
  assert.equal(state.archiveSortMark("archived"), "▼");
  assert.equal(state.archiveSortMark("id"), "");

  state.archiveSort("archived");
  assert.equal(state.archiveSortDir, "asc");

  // A string column reads best the other way round, so a new key does not
  // inherit the direction the previous one was left in.
  state.archiveSort("title");
  assert.deepEqual([state.archiveSortKey, state.archiveSortDir], ["title", "asc"]);
  state.archiveSort("wall");
  assert.deepEqual([state.archiveSortKey, state.archiveSortDir], ["wall", "desc"]);
});

test("the archive's pill options are the values the rows actually carry", () => {
  const state = loadKontoraState();
  state.archiveRows = [
    archiveRow({ id: "kon-001", project: "web", status: "done", agent: "claude" }),
    archiveRow({ id: "kon-002", project: "api", status: "superseded", agent: "claude" }),
    archiveRow({ id: "kon-003", project: "web", status: "", agent: "codex" }),
  ];

  assert.deepEqual([...state.archiveOptions("project")], ["all", "api", "web"]);
  assert.deepEqual([...state.archiveOptions("agent")], ["all", "claude", "codex"]);
  assert.deepEqual([...state.archiveOptions("status")], ["all", "done", "superseded", "unknown"]);
});

test("the sidebar facets toggle the archive's own filters and count per name", () => {
  const state = loadKontoraState();
  state.archiveRows = [
    archiveRow({ id: "kon-001", project: "web", agent: "claude" }),
    archiveRow({ id: "kon-002", project: "web", agent: "codex" }),
    archiveRow({ id: "kon-003", project: "api", agent: "claude" }),
  ];

  assert.equal(state.archiveFacetCount("project", "web"), 2);
  assert.equal(state.archiveFacetCount("agent", "codex"), 1);

  state.archiveToggleFacet("project", "web");
  assert.equal(state.archiveProject, "web");
  assert.ok(state.archiveFacetActive("project", "web"));
  assert.deepEqual(state.archiveView().rows.map(r => r.id), ["kon-001", "kon-002"]);

  // Clicking the active row clears it rather than reselecting it.
  state.archiveToggleFacet("project", "web");
  assert.equal(state.archiveProject, "all");
  assert.equal(state.archiveView().shown, 3);
});

test("clear all empties the box and every pill", () => {
  const state = loadKontoraState();
  state.archiveRows = [archiveRow()];
  assert.equal(state.archiveFiltersActive(), false);

  state.archiveQuery = "shim";
  state.archiveStatus = "done";
  state.archiveRange = "30d";
  assert.ok(state.archiveFiltersActive());

  state.archiveClearFilters();
  assert.equal(state.archiveFiltersActive(), false);
  assert.equal(state.archiveView().shown, 1);
});

test("the archive overlay opens only for the ticket the archive itself opened", () => {
  const state = loadKontoraState();
  state.currentView = "archive";
  assert.equal(state.archiveDetailOpen(), false, "nothing open");

  // Clicking Archive in the sidebar while a board ticket is on screen leaves
  // that ticket in selectedTicket. Rendering it here would stamp a running
  // ticket as archived.
  state.selectedTicket = { id: "kon-009", title: "Running", status: "in_progress" };
  assert.equal(state.archiveDetailOpen(), false, "a board ticket is not the archive's");

  state._archiveDetailId = "kon-009";
  assert.equal(state.archiveDetailOpen(), true);

  state.currentView = "board";
  assert.equal(state.archiveDetailOpen(), false, "the overlay belongs to the archive view");
});

test("applyTicketUpdate keeps the archive's open ticket instead of closing it", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.currentView = "archive";
  state.tickets = [{ id: "kon-001", title: "Done", status: "done", kontora: true }];
  state.archiveRows = [archiveRow({ id: "kon-001", title: "Old work", status: "", archived_at: null })];
  state.selectedTicket = { id: "kon-001", title: "Old work", status: "archived" };
  state._archiveDetailId = "kon-001";

  state.selectedTicket.body = "# Old work\n\nthe fetched body";

  // broadcastTicketUpdate strips the body from the event, so this is the shape
  // the overlay actually receives.
  const event = {
    id: "kon-001", title: "Renamed", status: "archived", kontora: true,
    archived_from: "done", archived_at: "2026-08-26T09:12:00Z", archived_by: "web",
  };
  state.applyTicketUpdate(event);

  // The board still drops it; the archive's overlay and its row take the update.
  assert.deepEqual(state.tickets, []);
  assert.equal(state.selectedTicket.id, "kon-001");
  assert.equal(state.selectedTicket.archived_from, "done");
  assert.equal(state.selectedTicket.body, "# Old work\n\nthe fetched body");
  assert.deepEqual(state.archiveRows.map(r => [r.title, r.status, r.archived_at, r.archived_by]),
    [["Renamed", "done", "2026-08-26T09:12:00Z", "web"]]);
});

test("a board ticket archived elsewhere closes its page even while the archive is open", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.writeHash = () => {};
  // Opening a board ticket and then the Archive view leaves that ticket in
  // selectedTicket without the archive owning it, so an archive event for it
  // must close the board's page rather than restamp it as archived.
  state.currentView = "archive";
  state.tickets = [{ id: "kon-009", title: "Live", status: "done", kontora: true }];
  state.archiveRows = [archiveRow({ id: "kon-001" })];
  state.selectedTicket = { id: "kon-009", title: "Live", status: "done" };
  state._archiveDetailId = null;

  state.applyTicketUpdate({ id: "kon-009", title: "Live", status: "archived", kontora: true, archived_from: "done" });

  assert.deepEqual(state.tickets, []);
  assert.equal(state.selectedTicket, null, "the board's page cannot outlive the board row");
});

test("applyTicketUpdate drops a restored ticket from the archive list and closes its overlay", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.currentView = "archive";
  state.tickets = [];
  state.archiveRows = [archiveRow({ id: "kon-001" }), archiveRow({ id: "kon-002" })];
  state.selectedTicket = { id: "kon-001", title: "Old work", status: "archived" };
  state._archiveDetailId = "kon-001";

  // The restore could come from another browser, so the list has to follow the
  // event rather than only the optimistic removal this session made.
  state.applyTicketUpdate({ id: "kon-001", title: "Old work", status: "done", kontora: true });

  assert.deepEqual([...state.archiveRows].map(r => r.id), ["kon-002"]);
  assert.deepEqual(state.tickets.map(t => t.id), ["kon-001"]);
  assert.equal(state.archiveDetailOpen(), false, "the read-only page cannot outlive the archive row");
});

test("the archive detail chip reads archived_from, not the archived status beside it", () => {
  const state = loadKontoraState();

  // A list row carries archived_from in its own status field; the detail
  // payload answers status: archived with archived_from beside it.
  assert.equal(state.archiveRowStatus(archiveRow({ status: "cancelled" })), "cancelled");
  assert.equal(state.archiveRowStatus(archiveRow({ status: "" })), "unknown");

  state.selectedTicket = { id: "kon-001", status: "archived", archived_from: "cancelled" };
  assert.equal(state.archiveDetailStatus(), "cancelled");

  state.selectedTicket = { id: "kon-001", status: "archived" };
  assert.equal(state.archiveDetailStatus(), "unknown");
});

test("applyTicketUpdate still closes the board's detail panel from the archive view's row", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  // The board is the open view, so an archived update closes its panel as ever.
  state.currentView = "board";
  state.tickets = [{ id: "kon-001", title: "Done", status: "done", kontora: true }];
  state.selectedTicket = { id: "kon-001", title: "Done", status: "done" };

  state.applyTicketUpdate({ id: "kon-001", title: "Done", status: "archived", kontora: true });

  assert.equal(state.selectedTicket, null);
});

// ---------------------------------------------------------------------------
// Settings view
// ---------------------------------------------------------------------------

const yamlPath = path.join(dirname, "../static/vendor/yaml@2.8.1/yaml.mjs");

// A commented config exercising everything the form must preserve but does not
// model: an editor key, a per-agent environment map, a | block prompt with a
// trailing comment, and an agent that disables failure detection with [].
const SETTINGS_FIXTURE = `# kontora configuration
tickets_dir: ~/org/tickets
editor: nvim # not modelled by the form

agents:
  claude:
    binary: claude
    args:
      - --dangerously-skip-permissions
    environment:
      ANTHROPIC_LOG: debug
  pi:
    binary: pi
    args: []
    failure_patterns: []
    effort: high

stages:
  # the planning pass
  plan:
    prompt: |
      Read the ticket.
      Draft a plan.
      Write it to PLAN.md.
    timeout: 45m
    effort: low
  implement:
    prompt: Implement PLAN.md
    timeout: 2h
    model:
      claude: opus
      pi: anthropic/claude-opus-5

pipelines:
  full:
    - stage: plan
      agent: claude
      on_success: next
      on_failure: retry
      max_retries: 2
    - stage: implement
      agent: claude
      on_success: human_review
      on_failure: pause

projects:
  kontora:
    path: ~/projects/kontora
    pipeline: full

environment:
  KONTORA_MODE: dev

web:
  host: 127.0.0.1
  port: 8080

summary_effort: low
`;

// Every collection key present with an empty value, which is how a key parses
// when all of its entries are commented out. Nothing can be written under one
// of these without replacing the null scalar the parser produced.
const SETTINGS_BARE_FIXTURE = `# nothing configured yet
statuses:
environment:
stages:
agents:
  claude:
    binary: claude
    args:
`;

// A Settings view with the fixture parsed, using the real vendored yaml module
// in place of the dynamic import() the browser resolves.
async function settingsState(text = SETTINGS_FIXTURE, overrides = {}) {
  const state = loadKontoraState(overrides);
  state._settingsYAML = await import(yamlPath);
  await state._settingsParse(text);
  return state;
}

// Alpine wraps the x-data object in a deep reactive Proxy, so in the browser
// every read of settingsConfig hands back a Proxy rather than the stored
// object. loadKontoraState returns the bare object, which hides anything that
// breaks on a Proxy (structuredClone throws DataCloneError on one). Wrap the
// state the way @vue/reactivity does: plain objects, arrays, maps and sets are
// proxied on read, everything else (RegExp, module namespaces) is handed back
// as it is.
const REACTIVE_TAGS = new Set(["[object Object]", "[object Array]", "[object Map]", "[object Set]"]);

function settingsReactive(target, cache = new WeakMap()) {
  if (cache.has(target)) return cache.get(target);
  const proxy = new Proxy(target, {
    get(obj, key, receiver) {
      const value = Reflect.get(obj, key, receiver);
      const proxyable = value !== null && typeof value === "object" && REACTIVE_TAGS.has(Object.prototype.toString.call(value));
      return proxyable ? settingsReactive(value, cache) : value;
    },
  });
  cache.set(target, proxy);
  return proxy;
}

// A daemon double for the save path. One save is three requests: GET the file
// to check it did not change under the tab, PUT the new content, then GET
// /api/config so the board picks up the reloaded values.
function settingsFetch({ raw = SETTINGS_FIXTURE, put = { status: 204, ok: true }, config } = {}) {
  const sent = [];
  const fetch = async (url, init = {}) => {
    sent.push({ url, init });
    const method = init.method || "GET";
    if (url === "/api/config/raw" && method === "GET") {
      return { status: 200, ok: true, json: async () => ({ content: typeof raw === "function" ? raw() : raw }) };
    }
    if (url === "/api/config/raw") return typeof put === "function" ? put() : put;
    if (url === "/api/config") return { status: 200, ok: true, json: async () => config || {} };
    return { status: 404, ok: false, json: async () => ({}) };
  };
  const of = (method) => sent.filter((s) => (s.init.method || "GET") === method);
  return { fetch, sent, puts: () => of("PUT"), gets: () => of("GET") };
}

test("settings loads the fixture clean and defaults to the stages section", async () => {
  const state = await settingsState();

  assert.equal(state.settingsState, "ok");
  assert.equal(state.settingsSection, "stages");
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(state.settingsDirty(), false);
});

test("the whole edit cycle runs on Alpine's reactive proxy", async () => {
  const daemon = settingsFetch();
  const state = settingsReactive(loadKontoraState({ fetch: daemon.fetch }));
  state._settingsYAML = await import(yamlPath);

  await state._settingsParse(SETTINGS_FIXTURE);
  assert.equal(state.settingsState, "ok");
  assert.equal(state.settingsLoadError, "");

  state.settingsConfig.stages.plan.timeout = "1h30m";
  assert.deepEqual([...state.settingsChangedPaths()], ["stages.plan.timeout"]);

  state.revertSettingsStage("plan");
  assert.equal(state.settingsDirty(), false);

  state.settingsConfig.general.branch_prefix = "agent";
  state.discardSettings();
  assert.equal(state.settingsDirty(), false);

  state.settingsConfig.stages.plan.timeout = "1h30m";
  assert.equal(await state.saveSettings(), true);
  assert.match(JSON.parse(daemon.puts()[0].init.body).content, /timeout: 1h30m/);
  assert.equal(state.settingsDirty(), false);
});

test("settingsChangedPaths reports exactly the edited field", async () => {
  const state = await settingsState();

  state.settingsConfig.stages.plan.prompt = "Read the ticket.\nDraft a plan.\nWrite it to PLAN.md.\n";
  assert.deepEqual([...state.settingsChangedPaths()], []);

  state.settingsConfig.stages.plan.prompt = "Read the ticket.\nDraft a better plan.\nWrite it to PLAN.md.\n";
  assert.deepEqual([...state.settingsChangedPaths()], ["stages.plan.prompt"]);
  assert.equal(state.settingsDirty(), true);
  assert.equal(state.settingsPathDirty("stages.plan"), true);
  assert.equal(state.settingsPathDirty("stages.implement"), false);
});

test("a multiline change diffs the middle with one context line each side", async () => {
  const state = await settingsState();

  state.settingsConfig.stages.plan.prompt = "Read the ticket.\nDraft a better plan.\nWrite it to PLAN.md.\n";
  const hunks = state.settingsDiffHunks();

  assert.equal(hunks.length, 1);
  assert.equal(hunks[0].path, "stages.plan.prompt");
  assert.deepEqual(vmValue(hunks[0].lines).map((l) => [l.kind, l.text]), [
    ["context", "Read the ticket."],
    ["del", "Draft a plan."],
    ["add", "Draft a better plan."],
    ["context", "Write it to PLAN.md."],
  ]);
});

test("inserting a token replaces the selection and reports the caret after it", async () => {
  let restored = null;
  const el = {
    selectionStart: 5,
    selectionEnd: 12,
    focus() {},
    setSelectionRange(from, to) { restored = [from, to]; },
  };
  const state = await settingsState(SETTINGS_FIXTURE, {
    document: { getElementById: () => el, querySelector: () => null, documentElement: { style: {} } },
  });
  state.settingsConfig.stages.implement.prompt = "Impl PLAN.md now";

  const caret = state.insertTemplateToken("prompt", "implement", "{{ .Ticket.ID }}");

  assert.equal(state.settingsConfig.stages.implement.prompt, "Impl {{ .Ticket.ID }} now");
  assert.equal(caret, 5 + "{{ .Ticket.ID }}".length);
  assert.deepEqual(restored, [caret, caret]);
});

test("client validation uses the daemon's exact wording", async () => {
  const state = await settingsState();

  state.settingsConfig.agents.claude.binary = "";
  state.settingsConfig.general.default_agent = "codex";
  state.settingsConfig.projects.kontora.path = "";
  state.settingsConfig.statuses = ["Needs-QA"];

  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'agent "claude": binary is required',
    'default_agent "codex": not found in agents',
    'project "kontora": path is required',
    'custom status "Needs-QA": must match [a-z][a-z0-9_]*',
  ]);
});

// The assistant block with a comment inside it and a value on every field, so
// a write can be checked for what it left alone as well as what it changed.
const SETTINGS_ASSISTANT_FIXTURE = `tickets_dir: ~/org/tickets
editor: nvim # not modelled by the form

agents:
  claude:
    binary: claude
  codex:
    binary: codex

assistant:
  # the chat beside the board
  agent: claude
  model: opus
  workdir: ~/org
  timeout: 10m
  autonomy: ask
  prompt: |
    Answer from the board.
`;

test("the assistant block writes only the field that changed", async () => {
  const state = await settingsState(SETTINGS_ASSISTANT_FIXTURE);

  assert.deepEqual(vmValue(state.settingsConfig.assistant), {
    agent: "claude", model: "opus", effort: "", workdir: "~/org",
    timeout: "10m", autonomy: "ask", prompt: "Answer from the board.\n",
  });
  assert.deepEqual([...state.settingsChangedPaths()], []);

  state.settingsConfig.assistant.autonomy = "auto";
  assert.deepEqual([...state.settingsChangedPaths()], ["assistant.autonomy"]);

  const out = await state._settingsWrite();
  assert.match(out, /autonomy: auto/);
  assert.match(out, /# the chat beside the board/);
  assert.match(out, /editor: nvim # not modelled by the form/);
  assert.match(out, /agent: claude/);
  assert.match(out, /model: opus/);
});

test("clearing an assistant field deletes its key rather than writing an empty string", async () => {
  const state = await settingsState(SETTINGS_ASSISTANT_FIXTURE);

  state.settingsConfig.assistant.workdir = "";
  const out = await state._settingsWrite();

  assert.equal(/workdir:/.test(out), false);
  assert.match(out, /agent: claude/);
});

test("an assistant prompt keeps its whitespace, and the scalars are trimmed", async () => {
  const state = await settingsState(SETTINGS_ASSISTANT_FIXTURE);

  state.settingsConfig.assistant.prompt = "Answer from the board.\n  Indented line.\n";
  state.settingsConfig.assistant.model = "  sonnet  ";
  const out = await state._settingsWrite();

  assert.match(out, /\n {6}Indented line\./);
  assert.match(out, /model: sonnet\n/);
});

test("assistant validation mirrors the daemon and stands down when no agent is named", async () => {
  const state = await settingsState(SETTINGS_ASSISTANT_FIXTURE);

  state.settingsConfig.assistant.agent = "gemini";
  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'assistant.agent "gemini": not found in agents',
  ]);

  // codex takes no session id, which is what one chat resumes on.
  state.settingsConfig.assistant.agent = "codex";
  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'assistant.agent "codex": runs codex, and the assistant only supports claude and pi ' +
      "(they are the CLIs that take a session id, which one chat needs)",
  ]);

  state.settingsConfig.assistant.agent = "claude";
  state.settingsConfig.assistant.autonomy = "yolo";
  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'assistant.autonomy "yolo": must be one of read, ask, auto',
  ]);

  // An empty agent disables the pane, so nothing else in the block is checked.
  state.settingsConfig.assistant.agent = "";
  state.settingsConfig.assistant.effort = "nonsense";
  assert.deepEqual(vmValue(state.settingsClientErrors()), []);

  // Both are filled by applyDefaults when absent, so blank is not an error.
  state.settingsConfig.assistant.agent = "claude";
  state.settingsConfig.assistant.autonomy = "";
  state.settingsConfig.assistant.timeout = "";
  assert.deepEqual(vmValue(state.settingsClientErrors()), []);

  // The agents map is a plain object, so a name it shares with an
  // Object.prototype member has to read as absent, not as a configured agent.
  state.settingsConfig.assistant.agent = "constructor";
  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'assistant.agent "constructor": not found in agents',
  ]);
});

test("durations the daemon accepts are not marked invalid", async () => {
  const state = await settingsState();

  for (const ok of ["30m", "1h30m", "1.5h", "500ms", ""]) {
    assert.equal(state.settingsDurationValid(ok), true, ok);
  }
  assert.equal(state.settingsDurationValid("not-a-duration"), false);

  assert.equal(state.settingsIntValid("4"), true);
  assert.equal(state.settingsIntValid("many"), false);

  // Neither check blocks the save: the daemon owns both messages.
  state.settingsConfig.stages.plan.timeout = "not-a-duration";
  state.settingsConfig.general.max_concurrent_agents = "many";
  assert.deepEqual(vmValue(state.settingsClientErrors()), []);
});

test("the built-in rework stage is listed, marked, and creates a real key when edited", async () => {
  const state = await settingsState();

  assert.equal(state.settingsConfig.stages.rework.builtin, true);
  assert.match(state.settingsConfig.stages.rework.prompt, /plannotatorReview/);
  assert.deepEqual(vmValue(state.settingsStageNames()), ["implement", "plan", "rework"]);
  assert.deepEqual([...state.settingsChangedPaths()], []);

  state.settingsConfig.stages.rework.prompt = "Redo it.";
  await state._settingsWrite();

  // The timeout is written with the prompt even though it did not change. A
  // materialised rework without one runs unbounded: process.Run starts the
  // timer only above zero.
  assert.match(String(state._settingsDoc), /rework:\n\s+prompt: Redo it\.\n\s+timeout: 30m/);
});

test("a stage added to a file with none is written whole", async () => {
  const state = await settingsState(SETTINGS_BARE_FIXTURE);

  state.settingsNewStage = "qa";
  state.settingsAddStage();
  state.settingsConfig.stages.qa.prompt = "Review {{ .Ticket.ID }}";
  state.settingsConfig.stages.qa.timeout = "30m";
  await state._settingsWrite();

  assert.match(String(state._settingsDoc), /stages:\n\s+qa:\n\s+prompt: Review \{\{ \.Ticket\.ID \}\}\n\s+timeout: 30m/);
});

test("keys the file leaves empty accept a value instead of throwing", async () => {
  // A list cannot be assigned into the null scalar an empty key parses as, and
  // setIn cannot descend through one. Both used to throw and write nothing.
  const statuses = await settingsState(SETTINGS_BARE_FIXTURE);
  statuses.settingsNewStatus = "needs_qa";
  statuses.settingsAddStatus();
  assert.match(await statuses._settingsWrite(), /statuses:\n\s+- needs_qa/);

  const args = await settingsState(SETTINGS_BARE_FIXTURE);
  args.settingsConfig.agents.claude.args = "--dangerously-skip-permissions";
  assert.match(await args._settingsWrite(), /args:\n\s+- --dangerously-skip-permissions/);

  const env = await settingsState(SETTINGS_BARE_FIXTURE);
  env.settingsNewEnvKey = "KONTORA_TEST";
  env.settingsAddEnv();
  env.settingsConfig.environment.KONTORA_TEST = "enabled";
  assert.match(await env._settingsWrite(), /environment:\n\s+KONTORA_TEST: enabled/);

  // The keys keep their position and the file keeps its comment.
  assert.match(await env._settingsWrite(), /^# nothing configured yet\nstatuses:/);
});

test("a field left blank on a new entry is skipped, not deleted through a null key", async () => {
  // A new stage writes prompt and timeout together, so one of them is blank
  // whenever the user fills in only the other. Deleting the blank one used to
  // throw on the null scalar `stages:` parses as, and the save wrote nothing.
  const state = await settingsState(SETTINGS_BARE_FIXTURE);
  state.settingsNewStage = "qa";
  state.settingsAddStage();
  state.settingsConfig.stages.qa.timeout = "10m";

  const out = await state._settingsWrite();
  assert.match(out, /stages:\n\s+qa:\n\s+timeout: 10m/);
  assert.equal(/prompt:/.test(out), false);
});

test("paths and durations are trimmed, prompts and environment values are not", async () => {
  const state = await settingsState();

  state.settingsConfig.general.tickets_dir = "~/org/other ";
  state.settingsConfig.stages.plan.timeout = " 45m ";
  state.settingsConfig.environment.KONTORA_MODE = "dev ";
  state.settingsConfig.stages.implement.prompt = "Implement PLAN.md ";
  const out = await state._settingsWrite();

  assert.match(out, /^tickets_dir: ~\/org\/other$/m);
  assert.match(out, /timeout: 45m$/m);
  // yaml quotes a value whose whitespace matters, which is the tell that these
  // two reached the file untouched.
  assert.match(out, /KONTORA_MODE: "dev "/);
  assert.match(out, /prompt: "Implement PLAN\.md "/);
});

test("failure_patterns keeps its three states apart", async () => {
  const state = await settingsState();

  const claude = state.settingsFailurePatterns("claude");
  assert.equal(claude.mode, "default");
  assert.equal(claude.patterns.length, 8);

  assert.deepEqual(vmValue(state.settingsFailurePatterns("pi")), { mode: "disabled", patterns: [] });

  state.settingsConfig.agents.pi.failure_patterns = ["quota exceeded"];
  assert.deepEqual(vmValue(state.settingsFailurePatterns("pi")), { mode: "override", patterns: ["quota exceeded"] });
});

test("a model and an effort round-trip in both of their shapes", async () => {
  const state = await settingsState();

  // A scalar in the file is one value for every agent; a map is per agent.
  assert.deepEqual(vmValue(state.settingsConfig.stages.plan.effort), { mode: "any", any: "low", by: {} });
  assert.deepEqual(vmValue(state.settingsConfig.stages.implement.model),
    { mode: "per_agent", any: "", by: { claude: "opus", pi: "anthropic/claude-opus-5" } });
  assert.equal(state.settingsConfig.agents.pi.effort, "high");
  assert.deepEqual(vmValue(state.settingsConfig.summary_effort), { mode: "any", any: "low", by: {} });
  // A field the file leaves out is unset, not per-agent.
  assert.deepEqual(vmValue(state.settingsConfig.stages.plan.model), { mode: "any", any: "", by: {} });

  // A collapsed stage card labels both fields, so a stage carrying only a model
  // is not indistinguishable from one with no overrides at all.
  assert.equal(state.settingsPerAgentLabel("stages.plan.effort"), "low");
  assert.equal(state.settingsPerAgentLabel("stages.plan.model"), "");
  assert.equal(state.settingsPerAgentLabel("stages.implement.model"), "claude: opus, pi: anthropic/claude-opus-5");
  const card = fs.readFileSync(htmlPath, "utf8").split("settingsToggleStage(name)")[1].split("</button>")[0];
  assert.match(card, /x-for="field in \['model', 'effort'\]"/);

  // Nothing was edited, so nothing is written.
  assert.deepEqual(vmValue(state.settingsChangedPaths()), []);
  const out = await state._settingsWrite();
  assert.equal(out, SETTINGS_FIXTURE);
});

test("switching a field to per-agent replaces the bare value with a map", async () => {
  const state = await settingsState();

  state.settingsSetPerAgentMode("stages.plan.effort", "per_agent");
  state.settingsNewPerAgentKey["stages.plan.effort"] = "claude";
  state.settingsAddPerAgent("stages.plan.effort");
  state.settingsPerAgent("stages.plan.effort").by.claude = "xhigh";

  const out = await state._settingsWrite();
  assert.match(out, /effort:\n {6}claude: xhigh/);
  assert.equal(/effort: low\n/.test(out.split("implement:")[0]), false, "the bare value is gone");

  // And back the other way: the value typed before the flip is still there.
  state.settingsSetPerAgentMode("stages.plan.effort", "any");
  assert.match(await state._settingsWrite(), /^ {4}effort: low$/m);
});

test("clearing a per-agent field removes the key rather than writing an empty value", async () => {
  const state = await settingsState();

  state.settingsConfig.stages.plan.effort.any = "";
  state.settingsConfig.agents.pi.effort = "";
  state.settingsConfig.summary_effort.any = "  ";
  const out = await state._settingsWrite();

  assert.equal(/effort:/.test(out), false, "no effort key survives anywhere");
  assert.match(out, /timeout: 45m/, "the neighbouring key is untouched");
});

test("a per-agent row can be added and removed", async () => {
  const state = await settingsState();
  const path = "stages.implement.model";

  state.settingsNewPerAgentOpen[path] = true;
  state.settingsNewPerAgentKey[path] = "pi-fast";
  state.settingsAddPerAgent(path);
  assert.deepEqual(vmValue(state.settingsPerAgentKeys(path)), ["claude", "pi", "pi-fast"]);
  assert.equal(state.settingsNewPerAgentOpen[path], false, "adding closes the draft row");

  // An empty value writes no entry, so a half-filled row is not an edit yet.
  assert.match(await state._settingsWrite(), /model:\n {6}claude: opus\n {6}pi: anthropic\/claude-opus-5/);

  state.settingsPerAgent(path).by["pi-fast"] = "anthropic/claude-haiku-4-5";
  assert.match(await state._settingsWrite(), /pi-fast: anthropic\/claude-haiku-4-5/);

  state.settingsRemovePerAgent(path, "pi-fast");
  state.settingsRemovePerAgent(path, "pi");
  const out = await state._settingsWrite();
  assert.equal(/pi-fast/.test(out), false);
  assert.match(out, /model:\n {6}claude: opus/);

  // Removing the last one deletes the key.
  state.settingsRemovePerAgent(path, "claude");
  assert.equal(/model:/.test(await state._settingsWrite()), false);
});

test("an effort on a stage the file does not declare writes the stage whole", async () => {
  const state = await settingsState();

  // rework is injected, not declared. Writing only its effort would leave a
  // declared stage with no prompt and no timeout, which runs unbounded.
  state.settingsConfig.stages.rework.effort.any = "xhigh";
  const out = await state._settingsWrite();

  assert.match(out, /^ {2}rework:$/m);
  assert.match(out, /^ {4}effort: xhigh$/m);
  assert.match(out, /^ {4}timeout: 30m$/m);
  assert.match(out, /The reviewer requested changes\./);
});

test("a per-agent key is marked when it names neither an agent nor a kind", async () => {
  const state = await settingsState();

  assert.equal(state.settingsAgentKeyValid("claude"), true, "an agent kind");
  assert.equal(state.settingsAgentKeyValid("pi"), true);
  assert.equal(state.settingsAgentKeyValid("nope"), false);
  // An empty draft is not wrong yet, only incomplete.
  assert.equal(state.settingsAgentKeyValid(""), true);
  // An agent named after an Object.prototype member is not configured.
  assert.equal(state.settingsAgentKeyValid("constructor"), false);

  state.settingsConfig.agents["pi-fast"] = { binary: "pi", effort: "", args: "", failure_patterns: null };
  assert.equal(state.settingsAgentKeyValid("pi-fast"), true, "a configured agent name");
});

test("a per-agent map is written entry by entry", async () => {
  // Replacing the mapping node would drop the comment inside it, sort its keys,
  // and — where another key aliases its anchor — strand the alias, so stringify
  // throws and no save can succeed again.
  const fixture = `stages:
  plan:
    model: &models
      pi: anthropic/claude-opus-5   # the expensive one
      claude: opus
    timeout: 45m
  implement:
    model: *models
`;
  const state = await settingsState(fixture);

  state.settingsPerAgent("stages.plan.model").by.claude = "sonnet";
  const out = await state._settingsWrite();

  assert.match(out, /pi: anthropic\/claude-opus-5 # the expensive one/, "the comment survives");
  assert.match(out, /model: &models\n {6}pi: /, "the anchor and the key order survive");
  assert.match(out, /claude: sonnet/);
  assert.match(out, /implement:\n {4}model: \*models/, "the alias still resolves");
  assert.match(out, /timeout: 45m/);

  // A key removed in the form is removed from the map, and the rest stay put.
  state.settingsRemovePerAgent("stages.plan.model", "pi");
  const pruned = await state._settingsWrite();
  assert.equal(/pi: anthropic/.test(pruned), false);
  assert.match(pruned, /claude: sonnet/);
});

test("switching to per agent without filling a row keeps the value", async () => {
  const state = await settingsState();

  state.settingsSetPerAgentMode("stages.plan.effort", "per_agent");

  // Nothing is filled in yet, so nothing changed and the bare value still
  // applies. Writing the field would delete it and the stage would silently
  // fall back to the agent's own effort.
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(await state._settingsWrite(), SETTINGS_FIXTURE);

  // And with the rows still empty, the single value is still what is written,
  // rather than the key being deleted as an empty map would delete it.
  state.settingsPerAgent("stages.plan.effort").any = "high";
  const plan = (await state._settingsWrite()).split("  plan:")[1].split("  implement:")[0];
  assert.match(plan, /^ {4}effort: high$/m);
});

test("a one-key map is not confused with a bare value that reads like one", async () => {
  // Change detection compares the flattened strings, and a one-key map renders
  // as `claude: low` — the same text the user can type into the single box. A
  // flat string with nothing to tell the two shapes apart reports no change, so
  // the save button never lights up and the edit is lost at the next reload.
  const state = await settingsState("summary_effort:\n  claude: low\n");

  state.settingsSetPerAgentMode("summary_effort", "any");
  state.settingsPerAgent("summary_effort").any = "claude: low";

  assert.deepEqual([...state.settingsChangedPaths()], ["summary_effort"]);
  assert.match(await state._settingsWrite(), /^summary_effort: ["']claude: low["']$/m);
});

test("a per-agent key naming nothing blocks the save with the daemon's wording", async () => {
  // Kept apart from the wording test above, whose config has no binary on
  // claude: config.Validate stops at the first error, so the checks that read
  // an agent's binary are skipped while one is missing.
  const state = await settingsState();

  state.settingsPerAgent("stages.implement.model").by.codex = "gpt-5";
  state.settingsPerAgent("summary_effort").mode = "per_agent";
  state.settingsPerAgent("summary_effort").by.codex = "high";
  state.settingsConfig.agents.claude.binary = "codex";
  state.settingsConfig.agents.claude.effort = "high";

  assert.deepEqual(vmValue(state.settingsClientErrors()), [
    'agent "claude": sets effort "high", which codex takes no flag for',
    'stage "implement": model "codex": neither a configured agent nor an agent kind (claude, pi)',
    'summary_effort: effort "codex": neither a configured agent nor an agent kind (claude, pi)',
    'pipeline "full" stage 0: stage "plan" sets effort "low", which agent "claude" (codex) takes no flag for',
    'pipeline "full" stage 1: stage "implement" sets model "opus", which agent "claude" (codex) takes no flag for',
  ]);

  // A row left blank writes no key, so it is not an error yet.
  state.settingsPerAgent("stages.implement.model").by.codex = "";
  assert.equal(vmValue(state.settingsClientErrors()).some(e => e.startsWith('stage "implement"')), false);

  // The kind behind the agent's own effort field, marked in the form the same
  // way. A wrapper runs the binary after its separator.
  assert.equal(state.settingsAgentKindOf("claude"), "", "binary codex takes no flag");
  assert.equal(state.settingsAgentKindOf("pi"), "pi");
  state.settingsConfig.agents.claude.binary = "/opt/homebrew/bin/nono";
  state.settingsConfig.agents.claude.args = "run\n--profile\nagent\n--\nclaude\n--verbose";
  assert.equal(state.settingsAgentKindOf("claude"), "claude");
});

test("an agent named after an Object.prototype member takes a per-agent value", async () => {
  const state = await settingsState();
  const path = "stages.implement.model";

  state.settingsConfig.agents.constructor = { binary: "claude", effort: "", args: "", failure_patterns: null };
  state.settingsNewPerAgentKey[path] = "constructor";
  state.settingsAddPerAgent(path);

  assert.equal(state.settingsPerAgentHasKey(path, "constructor"), true);
  assert.deepEqual(vmValue(state.settingsPerAgentKeys(path)), ["claude", "constructor", "pi"]);

  // A key that reaches Object.prototype's setter on a plain object stays data.
  state.settingsPerAgent(path).by.__proto__ = "haiku";
  state.settingsPerAgent(path).by.constructor = "haiku";
  assert.match(await state._settingsWrite(), /__proto__: haiku/);
  assert.match(String(state._settingsDoc), /constructor: haiku/);
});

test("reverting a stage drops the per-agent row left half-typed on it", async () => {
  const state = await settingsState();

  state.settingsSetPerAgentMode("stages.plan.effort", "per_agent");
  state.settingsNewPerAgentOpen["stages.plan.effort"] = true;
  state.settingsNewPerAgentKey["stages.plan.effort"] = "claude";
  state.settingsNewPerAgentOpen.summary_effort = true;

  state.revertSettingsStage("plan");
  assert.equal(state.settingsNewPerAgentOpen["stages.plan.effort"], undefined);
  assert.equal(state.settingsNewPerAgentKey["stages.plan.effort"], undefined);
  assert.equal(state.settingsNewPerAgentOpen.summary_effort, true, "another field's draft is left alone");

  state.discardSettings();
  assert.deepEqual(vmValue(state.settingsNewPerAgentOpen), {});
});

test("after a save the form shows what was written", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: settingsFetch().fetch });

  // An empty value writes no entry, so this row does not reach the file.
  state.settingsPerAgent("stages.implement.model").by.pi = "";
  assert.equal(await state.saveSettings(), true);

  assert.deepEqual(vmValue(state.settingsPerAgentKeys("stages.implement.model")), ["claude"]);
  assert.equal(state.settingsDirty(), false);
});

test("a sequence where a per-agent field is expected reads as unset", async () => {
  // PerAgent takes a scalar or a mapping, so the daemon can never have loaded
  // such a file. Rendering it as rows keyed 0 and 1 would let a save rewrite
  // the sequence as a mapping the daemon then rejects.
  const state = await settingsState("stages:\n  plan:\n    model:\n      - opus\n      - haiku\n");

  assert.deepEqual(vmValue(state.settingsConfig.stages.plan.model), { mode: "any", any: "", by: {} });
  assert.deepEqual([...state.settingsChangedPaths()], []);
});

test("a stage added here starts with a timeout", async () => {
  const state = await settingsState(SETTINGS_BARE_FIXTURE);

  state.settingsNewStage = "audit";
  state.settingsAddStage();
  // Editing only the model writes the stage whole. Without a timeout of its
  // own the stage would run until the agent exits: internal/process starts the
  // timer only above zero.
  state.settingsConfig.stages.audit.model.any = "haiku";

  const out = await state._settingsWrite();
  assert.match(out, /audit:\n {4}model: haiku\n {4}timeout: 30m/);
});

test("the per-agent control is one block, rendered wherever it is needed", async () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const state = await settingsState();

  // Two call sites, no second copy of the markup: the copies had already
  // drifted, and every fix to the control had to be made twice.
  assert.equal(html.match(/x-html="settingsPerAgentControl"/g).length, 2);
  assert.equal(html.includes("settingsAddPerAgent"), false);

  // Every method the control calls is on the component, so a rename cannot
  // leave the markup calling something that no longer exists.
  for (const name of new Set(state.settingsPerAgentControl.match(/settings[A-Za-z]+(?=\()/g))) {
    assert.equal(typeof state[name], "function", name);
  }
  // And every scope name it reads is supplied at both call sites.
  for (const scope of ["p", "label", "labelClass", "inputId", "hint"]) {
    assert.equal(html.match(new RegExp(`${scope}:`, "g")).length >= 2, true, scope);
  }
});

test("saving one field leaves comments, unmodelled keys, and block style intact", async () => {
  const cases = [
    {
      name: "a prompt",
      edit: cfg => { cfg.stages.plan.prompt = "Read the ticket.\nDraft a better plan.\nWrite it to PLAN.md.\n"; },
      changed: ["      Draft a better plan."],
    },
    {
      name: "an effort",
      edit: cfg => { cfg.stages.plan.effort.any = "high"; },
      changed: ["    effort: high"],
    },
  ];

  for (const tc of cases) {
    const state = await settingsState();
    tc.edit(state.settingsConfig);
    await state._settingsWrite();
    const out = String(state._settingsDoc);

    assert.match(out, /^# kontora configuration$/m, tc.name);
    assert.match(out, /editor: nvim # not modelled by the form/, tc.name);
    assert.match(out, /ANTHROPIC_LOG: debug/, tc.name);
    assert.match(out, /# the planning pass\n {2}plan:/, tc.name);
    assert.match(out, /prompt: \|/, tc.name);

    // Everything except the edited line is byte-for-byte what came in.
    const changed = out.split("\n").filter((l, i) => l !== SETTINGS_FIXTURE.split("\n")[i]);
    assert.deepEqual(vmValue(changed), tc.changed, tc.name);
  }
});

test("checkpoint_compaction_tokens is preserved when another agent field is edited", async () => {
  const fixture = `agents:
  pi:
    binary: pi
    args:
      - --dangerously-skip-permissions
    effort: high
    checkpoint_compaction_tokens: 800000

stages:
  plan:
    prompt: Draft a plan.
    timeout: 45m
`;
  const state = await settingsState(fixture);

  // The form models effort but not checkpoint_compaction_tokens. Editing the
  // modelled field must not drop the unmodelled one.
  state.settingsConfig.agents.pi.effort = "low";
  const out = await state._settingsWrite();

  assert.match(out, /checkpoint_compaction_tokens: 800000/);
  assert.match(out, /effort: low/);
});

test("a general key absent from the file is appended without reformatting", async () => {
  const state = await settingsState();

  state.settingsConfig.general.instance_name = "laptop";
  await state._settingsWrite();
  const out = String(state._settingsDoc);

  assert.match(out, /^instance_name: laptop$/m);
  assert.equal(out.startsWith(SETTINGS_FIXTURE.trimEnd()), true);
});

test("501 and 401 produce explicit states instead of an empty form", async () => {
  const unavailable = loadKontoraState({ fetch: async () => ({ status: 501, ok: false }) });
  await unavailable.openSettings();
  assert.equal(unavailable.settingsState, "unavailable");
  assert.equal(unavailable.settingsConfig, null);

  const unauthorized = loadKontoraState({ fetch: async () => ({ status: 401, ok: false }) });
  await unauthorized.openSettings();
  assert.equal(unauthorized.needsAuth, true);
  assert.equal(unauthorized.settingsConfig, null);
});

test("a successful save PUTs the serialized document and adopts it as the baseline", async () => {
  const daemon = settingsFetch({ config: { default_agent: "claude", custom_statuses: ["needs_qa"] } });
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: daemon.fetch });

  state.settingsConfig.stages.plan.timeout = "1h30m";
  assert.equal(await state.saveSettings(), true);

  const puts = daemon.puts();
  assert.equal(puts.length, 1);
  assert.equal(puts[0].url, "/api/config/raw");
  assert.equal(JSON.parse(puts[0].init.body).content, String(state._settingsDoc));
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.match(state.settingsSavedAt, /^\d{2}:\d{2}$/);
  assert.equal(state.settingsSavedRestart, false);

  // The daemon reloaded, so the config the board reads is refetched. Nothing
  // broadcasts a config reload over SSE.
  assert.deepEqual(vmValue(state.configCache), { default_agent: "claude", custom_statuses: ["needs_qa"] });
});

test("a daemon rejection renders unchanged and keeps the edits", async () => {
  const message = 'parsing config: invalid duration "not-a-duration": time: invalid duration "not-a-duration"';
  const daemon = settingsFetch({ put: { status: 400, ok: false, json: async () => ({ error: message }) } });
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: daemon.fetch });

  state.settingsConfig.stages.plan.timeout = "not-a-duration";
  assert.equal(await state.saveSettings(), false);

  assert.deepEqual(vmValue(state.settingsErrors), [message]);
  assert.equal(state.settingsConfig.stages.plan.timeout, "not-a-duration");
  assert.deepEqual([...state.settingsChangedPaths()], ["stages.plan.timeout"]);
});

test("a rejected value does not reappear in the next save", async () => {
  let reject = true;
  const daemon = settingsFetch({
    put: () => (reject
      ? { status: 400, ok: false, json: async () => ({ error: "parsing config: boom" }) }
      : { status: 204, ok: true }),
  });
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: daemon.fetch });

  // A rejected save used to leave its value in the parsed document. Discard
  // drops it from the form, so no later save would ever overwrite it.
  state.settingsConfig.web.token = "LEAKED";
  assert.equal(await state.saveSettings(), false);
  state.discardSettings();

  reject = false;
  state.settingsConfig.web.host = "0.0.0.0";
  assert.equal(await state.saveSettings(), true);

  const body = JSON.parse(daemon.puts().at(-1).init.body).content;
  assert.equal(body.includes("LEAKED"), false);
  assert.match(body, /host: 0\.0\.0\.0/);
});

test("a save refuses when config.yaml changed under the tab", async () => {
  const daemon = settingsFetch({ raw: SETTINGS_FIXTURE + "instance_name: edited-elsewhere\n" });
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: daemon.fetch });

  state.settingsConfig.stages.plan.timeout = "1h";
  assert.equal(await state.saveSettings(), false);

  assert.equal(daemon.puts().length, 0);
  assert.match(vmValue(state.settingsErrors)[0], /changed on disk/);
  assert.equal(state.settingsDiffOpen, true);
  assert.deepEqual([...state.settingsChangedPaths()], ["stages.plan.timeout"]);
});

test("a save that throws opens the diff panel so the error is visible", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: async () => { throw new Error("Failed to fetch"); },
  });

  state.settingsConfig.stages.plan.timeout = "1h";
  assert.equal(await state.saveSettings(), false);

  // settingsErrors only renders inside the diff panel, collapsed by default.
  assert.equal(state.settingsDiffOpen, true);
  assert.deepEqual(vmValue(state.settingsErrors), ["Failed to fetch"]);
  assert.equal(state.settingsSaving, false);
});

test("client validation blocks the request before it is sent", async () => {
  let calls = 0;
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: async () => { calls++; return { status: 204, ok: true }; },
  });

  state.settingsConfig.agents.claude.binary = "";
  assert.equal(await state.saveSettings(), false);

  assert.equal(calls, 0);
  assert.deepEqual(vmValue(state.settingsErrors), ['agent "claude": binary is required']);
  assert.equal(state.settingsConfig.agents.claude.binary, "");
});

test("saving a restart-only field says so", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: settingsFetch().fetch });

  state.settingsConfig.web.port = "9090";
  assert.equal(await state.saveSettings(), true);
  assert.equal(state.settingsSavedRestart, true);
});

test("discard restores the baseline without writing", async () => {
  let calls = 0;
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: async () => { calls++; return { status: 204, ok: true }; },
  });

  state.settingsConfig.stages.plan.prompt = "changed";
  state.settingsConfig.web.host = "0.0.0.0";
  state.discardSettings();

  assert.equal(state.settingsConfig.stages.plan.prompt, state.settingsBaseline.stages.plan.prompt);
  assert.equal(state.settingsConfig.web.host, "127.0.0.1");
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(calls, 0);
});

test("revert restores a stage, and drops one the baseline never had", async () => {
  const state = await settingsState();

  // The built-in rework is in the baseline, so reverting puts its default
  // prompt back rather than deleting a stage the daemon still runs.
  state.settingsConfig.stages.rework.prompt = "Redo it.";
  state.revertSettingsStage("rework");
  assert.match(state.settingsConfig.stages.rework.prompt, /plannotatorReview/);
  assert.deepEqual([...state.settingsChangedPaths()], []);

  state.settingsNewStage = "qa";
  state.settingsAddStage();
  state.settingsConfig.stages.qa.prompt = "Review {{ .Ticket.ID }}";
  assert.equal(state.settingsOpenStage, "qa");

  state.revertSettingsStage("qa");
  assert.equal(state.settingsConfig.stages.qa, undefined);
  assert.equal(state.settingsOpenStage, null);
  assert.deepEqual([...state.settingsChangedPaths()], []);
});

test("add flows create modelled entries and removal undoes them", async () => {
  const state = await settingsState();

  state.settingsNewStage = "qa";
  state.settingsAddStage();
  state.settingsConfig.stages.qa.prompt = "Review {{ .Ticket.ID }}";
  state.settingsConfig.stages.qa.timeout = "30m";
  assert.equal(state.settingsOpenStage, "qa");
  assert.deepEqual([...state.settingsChangedPaths()], ["stages.qa.prompt", "stages.qa.timeout"]);

  state.settingsNewEnvKey = "KONTORA_TEST";
  state.settingsAddEnv();
  state.settingsConfig.environment.KONTORA_TEST = "enabled";
  assert.equal(state.settingsChangedPaths().includes("environment.KONTORA_TEST"), true);

  state.settingsRemoveEnv("KONTORA_TEST");
  assert.equal(state.settingsChangedPaths().includes("environment.KONTORA_TEST"), false);
  assert.equal(state.settingsConfig.environment.KONTORA_MODE, "dev");

  state.settingsNewStatus = "needs_qa";
  state.settingsAddStatus();
  assert.equal(state.settingsChangedPaths().includes("statuses"), true);
  assert.deepEqual(vmValue(state.settingsClientErrors()), []);

  await state._settingsWrite();
  const out = String(state._settingsDoc);
  assert.match(out, /qa:\n\s+prompt: Review \{\{ \.Ticket\.ID \}\}\n\s+timeout: 30m/);
  assert.match(out, /statuses:\n\s+- needs_qa/);
});

test("an invalid custom status blocks the save with the daemon's wording", async () => {
  let calls = 0;
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: async () => { calls++; return { status: 204, ok: true }; },
  });

  state.settingsNewStatus = "Needs-QA";
  state.settingsAddStatus();

  assert.deepEqual(vmValue(state.settingsClientErrors()), ['custom status "Needs-QA": must match [a-z][a-z0-9_]*']);
  assert.equal(await state.saveSettings(), false);
  assert.equal(calls, 0);

  state.settingsRemoveStatus("Needs-QA");
  assert.deepEqual(vmValue(state.settingsClientErrors()), []);
});

test("display preferences stay in localStorage and never enter the diff", async () => {
  const stored = {};
  const state = await settingsState(SETTINGS_FIXTURE, {
    localStorage: { getItem: (k) => stored[k] ?? null, setItem: (k, v) => { stored[k] = v; } },
  });

  state.renderBoard = () => {};
  state.toggleShowBadges();
  state.toggleShowAgentMeta();
  state.toggleTheme();

  assert.equal(stored["kontora-show-badges"], "0");
  assert.equal(stored["kontora-show-agent-meta"], "0");
  assert.equal(state.lightTheme, true);
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(state.settingsDirty(), false);
});

test("switching sections keeps edits made in another one", async () => {
  const state = await settingsState();

  state.settingsConfig.stages.plan.timeout = "1h";
  state.settingsSection = "web";
  state.settingsConfig.web.host = "0.0.0.0";
  state.settingsSection = "agents";

  assert.deepEqual([...state.settingsChangedPaths()], ["stages.plan.timeout", "web.host"]);
});

test("leaving a dirty settings view records the target instead of navigating", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: settingsFetch().fetch });
  state.currentView = "settings";
  state.settingsConfig.stages.plan.timeout = "1h";

  await state.gotoView("board");
  assert.equal(state.settingsGuard, true);
  assert.equal(state.settingsGuardTarget, "board");
  assert.equal(state.currentView, "settings");

  await state.settingsGuardSave();
  assert.equal(state.settingsGuard, false);
  assert.equal(state.currentView, "board");
  assert.deepEqual([...state.settingsChangedPaths()], []);
});

test("save & leave that the daemon rejects stays on settings with the edits", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: settingsFetch({ put: { status: 400, ok: false, json: async () => ({ error: "parsing config: boom" }) } }).fetch,
  });
  state.currentView = "settings";
  state.settingsConfig.stages.plan.timeout = "nope";

  await state.gotoView("board");
  await state.settingsGuardSave();

  assert.equal(state.currentView, "settings");
  assert.equal(state.settingsGuard, true);
  assert.deepEqual(vmValue(state.settingsErrors), ["parsing config: boom"]);
  assert.equal(state.settingsConfig.stages.plan.timeout, "nope");
});

test("discarding from the guard leaves without writing", async () => {
  let calls = 0;
  const state = await settingsState(SETTINGS_FIXTURE, {
    fetch: async () => { calls++; return { status: 204, ok: true }; },
  });
  state.currentView = "settings";
  state.settingsConfig.stages.plan.timeout = "1h";

  await state.gotoView("board");
  await state.settingsGuardDiscard();

  assert.equal(calls, 0);
  assert.equal(state.currentView, "board");
  assert.equal(state.settingsGuard, false);
  assert.equal(state.settingsConfig.stages.plan.timeout, "45m");
});

test("a clean settings view navigates away immediately", async () => {
  const state = await settingsState();
  state.currentView = "settings";

  await state.gotoView("board");
  assert.equal(state.currentView, "board");
  assert.equal(state.settingsGuard, false);
});

test("index.html wires the settings shell to the guard and the section rail", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // Every nav button goes through the guard; none assigns currentView inline.
  assert.match(html, /@click="gotoView\('board'\)"/);
  assert.match(html, /@click="gotoView\('new'\)"/);
  assert.match(html, /@click="gotoView\('settings'\)"/);
  assert.equal(html.includes(`@click="currentView = 'board'"`), false);

  // The header is visible in the Settings view, so its New-ticket button needs
  // the guard too. The one direct openCreateModal() left is the board column
  // header, which only exists while the board is the current view.
  const direct = [...html.matchAll(/@click(?:\.stop)?="openCreateModal\(\)"/g)];
  assert.equal(direct.length, 1);
  assert.ok(direct[0].index > html.indexOf(`x-show="col.key === 'open' && colHover"`));

  // The guard closes before the create form and every other overlay below it.
  assert.match(html, /else if \(settingsGuard\) settingsGuard = false; else if \(currentView === 'new'\)/);

  assert.match(html, /x-show="!loading && currentView === 'settings'" x-cloak/);
  assert.match(html, /w-\[168px\][^"]*bg-surface-frame border-r border-surface-700\/50/);

  // The board's search box shrinks so the three-column settings layout fits.
  assert.match(html, /flex-1 min-w-\[120px\] max-w-\[300px\] relative/);
  assert.equal(html.includes('class="w-[300px] relative"'), false);

  // The 501 state offers no way to edit or save: it renders before the
  // settingsState === 'ok' template, and every editable control is inside it.
  const empty = html.slice(
    html.indexOf(`settingsState === 'unavailable'`),
    html.indexOf(`settingsState === 'parse-error'`),
  );
  assert.equal(/x-model|saveSettings/.test(empty), false);
  const editable = html.indexOf(`<template x-if="settingsState === 'ok'">`);
  assert.ok(editable > 0);
  for (const m of html.matchAll(/x-model="settingsConfig/g)) assert.ok(m.index > editable);
});

test("index.html renders every settings section the rail lists", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const app = fs.readFileSync(settingsPath, "utf8");

  const rail = [...app.matchAll(/\{ key: '([a-z]+)', label:/g)].map((m) => m[1]);
  assert.deepEqual(rail, [
    "general", "environment", "agents", "stages", "pipelines",
    "projects", "web", "assistant", "plannotator", "statuses", "display",
  ]);
  for (const key of rail) {
    assert.match(html, new RegExp(`x-show="settingsSection === '${key}'"`), key);
  }

  // Read-only sections carry no editable control.
  for (const key of ["pipelines", "projects"]) {
    const section = html.match(new RegExp(`x-show="settingsSection === '${key}'"[\\s\\S]*?\\n                </div>`))[0];
    assert.equal(/x-model/.test(section), false, key);
  }

  // A stage has no default timeout, so a placeholder naming one reads as a
  // fallback that does not exist. Blank means the stage runs unbounded.
  assert.equal(html.includes(`x-model="settingsConfig.stages[name].timeout"`), true);
  assert.equal(/settingsConfig\.stages\[name\]\.timeout"[\s\S]{0,300}?placeholder="30m"/.test(html), false);

  // The daemon token is the credential for every /api and /ws call, so it is
  // masked until asked for.
  assert.match(html, /id="web-token" :type="settingsShowToken \? 'text' : 'password'"/);

  // The assistant section mounts with the model already loaded, so x-model
  // applies its value before the x-for below has added an option and the select
  // falls back to the first one. Both selects re-apply on the next tick.
  for (const f of ["agent", "autonomy"]) {
    assert.match(html, new RegExp(
      `<select id="as-${f}" x-model="settingsConfig\\.assistant\\.${f}"\\s+` +
      `x-init="\\$nextTick\\(\\(\\) => \\$el\\.value = settingsConfig\\.assistant\\.${f}\\)"`), f);
  }
});

// ── Command palette ──────────────────────────────────────────────────────

// Three tickets across the statuses the palette rows differ on.
const PALETTE_TICKETS = [
  { id: "kon-1", title: "[kontora] Add a settings view", path: "/w/kontora", pipeline: "default", status: "in_progress", stage: "code", stages: ["plan", "code", "review"], agent: "claude", kontora: true },
  { id: "kon-2", title: "[kontora] Vendor the fonts", path: "/w/kontora", pipeline: "default", status: "paused", stage: "plan", stages: ["plan", "code", "review"], agent: "codex", kontora: true },
  { id: "gol-3", title: "Toroidal grid wrapping", path: "/w/game-of-life", pipeline: "simple", status: "open", stage: "", stages: [], agent: "claude", kontora: false },
];

// A component with the palette open. $refs.paletteInput records the focus and
// blur calls and ignores focus while it is hidden, as a browser does.
//
// Alpine reveals an x-show panel from its own animation frame, queued behind
// the one the open scheduled, so the stub panel is hidden (offsetParent null)
// for the first frame and on screen from the second. The harness runs each
// frame straight away, so the focus retry resolves in line.
//
// `recents` seeds the persisted recent-ticket ids read on construction.
function paletteState(tickets = PALETTE_TICKETS, { recents = [], ...overrides } = {}) {
  const input = {
    focused: 0,
    blurred: 0,
    offsetParent: null,
    focus() { if (this.offsetParent) this.focused += 1; },
    blur() { this.blurred += 1; },
  };
  let frame = 0;
  const state = loadKontoraState({
    localStorage: {
      getItem: (k) => (k === "kontora-recent-tickets" ? JSON.stringify(recents) : null),
      setItem() {},
    },
    requestAnimationFrame(callback) {
      frame += 1;
      if (frame > 1) input.offsetParent = {};
      callback();
      return frame;
    },
    ...overrides,
  });
  state.$nextTick = (callback) => { if (callback) callback(); return Promise.resolve(); };
  state.$refs = { paletteInput: input };
  state.updateFavicon = () => {};
  state.tickets = tickets.map((t) => ({ ...t }));
  state.recomputeBoard();
  state.openPalette();
  return state;
}

// Group labels and row ids, rebuilt in this realm (see vmValue).
const groupLabels = (state) => [...state.paletteGroups.map((g) => g.label)];
const groupIn = (state, label) => state.paletteGroups.find((g) => g.label === label);
const rowIds = (items) => [...items.map((r) => r.id)];
const rowTitles = (items) => [...items.map((r) => r.title)];

test("typing in the board filter still narrows the columns", async () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "kon-alpha", title: "Alpha", status: "todo", kontora: true },
    { id: "kon-beta", title: "Beta", status: "todo", kontora: true },
  ];
  state.recomputeBoard();
  assert.equal(bt(state, "in_progress").length, 2);

  // What the $watch on searchQuery calls; the autocomplete no longer sits in
  // front of it.
  state.searchQuery = "alpha";
  state.debounceRecomputeBoard();
  await new Promise((resolve) => setTimeout(resolve, 200));

  assert.deepEqual(bt(state, "in_progress").map((t) => t.id), ["kon-alpha"]);
});

test("an empty palette query lists the recent tickets then the navigation commands", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2", "gol-3"] });

  assert.deepEqual(groupLabels(state), ["Recent", "Go to"]);
  assert.deepEqual(rowIds(groupIn(state, "Recent").items), ["ticket:kon-2", "ticket:gol-3"]);
  assert.deepEqual(rowTitles(groupIn(state, "Go to").items),
    ["Go to board", "Stats", "Archive", "New ticket", "Settings", "Toggle sidebar", "Toggle theme"]);
});

test("the open ticket leads the root list with its actions and its live logs", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2"] });
  state.selectedTicket = { id: "kon-1" };
  state.recomputePalette();

  assert.deepEqual(groupLabels(state), ["This ticket · kon-1", "Recent", "Go to"]);
  const lead = groupIn(state, "This ticket · kon-1");
  assert.equal(lead.hint, "Add a settings view");
  assert.deepEqual(rowIds(lead.items), ["drill:kon-1", "tab:kon-1:session"]);
  assert.equal(lead.items[0].meta, "4 →");
  assert.equal(lead.items[1].meta, "live");
});

test("a query replaces the recent group with the matching tickets", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1"] });
  state.paletteQuery = "fonts";
  state.recomputePalette();

  assert.deepEqual(groupLabels(state), ["Tickets"]);
  const found = groupIn(state, "Tickets");
  assert.equal(found.hint, "1 of 3");
  assert.deepEqual(rowIds(found.items), ["ticket:kon-2"]);
});

test("a query narrows the navigation commands by label", () => {
  const state = paletteState();
  state.paletteQuery = "theme";
  state.recomputePalette();

  assert.deepEqual(groupLabels(state), ["Go to"]);
  assert.deepEqual(rowTitles(groupIn(state, "Go to").items), ["Toggle theme"]);
});

test("a query that matches nothing leaves no rows to render", () => {
  const state = paletteState();
  state.paletteQuery = "nothing matches this";
  state.recomputePalette();

  assert.equal(state.paletteGroups.length, 0);
  assert.equal(state._paletteRows.length, 0);
  assert.equal(state._paletteSelId, null);
});

test("the palette matches on stage and agent, which the board filter ignores", () => {
  const state = paletteState();

  for (const q of ["codex", "plan"]) {
    assert.equal(state.ticketMatchesQuery(state.tickets[1], q), false, q);
    state.paletteQuery = q;
    state.recomputePalette();
    assert.deepEqual(rowIds(groupIn(state, "Tickets").items), ["ticket:kon-2"], q);
  }
});

test("the palette reads the same project and agent tokens as the board", () => {
  const cases = [
    // The two kontora tickets, by the repository their path names.
    { name: "a project token", q: "project:=kontora", want: ["ticket:kon-1", "ticket:kon-2"] },
    { name: "an agent token", q: "agent:=codex", want: ["ticket:kon-2"] },
    // The token narrows, and the free text next to it still reaches the two
    // fields only the palette searches.
    { name: "a token and a stage as free text", q: "project:=kontora plan", want: ["ticket:kon-2"] },
    { name: "a token no ticket has", q: "project:=nothing", want: [] },
  ];

  for (const c of cases) {
    const state = paletteState();
    state.paletteQuery = c.q;
    state.recomputePalette();

    const tickets = groupIn(state, "Tickets");
    assert.deepEqual(tickets ? rowIds(tickets.items).sort() : [], c.want, c.name);
  }
});

test("a ticket row carries its status pill, project tag, and stage position", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1"] });

  const row = groupIn(state, "Recent").items[0];
  assert.equal(row.glyph, "●");
  assert.equal(row.glyphClass, "text-st-progress");
  assert.equal(row.tag, "[kontora]");
  assert.equal(row.status, "in_progress");
  assert.equal(row.statusText, "running");
  assert.equal(row.sub, "kon-1  ·  code 2/3  ·  claude");
  assert.equal(row.meta, "→");
});

test("an unstarted ticket reports its stage count and whether kontora manages it", () => {
  const state = paletteState([
    { id: "gol-4", title: "Add a CLI", path: "/w/game-of-life", status: "open", stages: ["plan", "code"], agent: "claude", kontora: false },
  ], { recents: ["gol-4"] });

  const row = groupIn(state, "Recent").items[0];
  assert.equal(row.sub, "gol-4  ·  2 stages  ·  claude  ·  unmanaged");
  assert.equal(row.statusText, "open");
});

test("drilling into a paused ticket lists its legal moves then the three tab targets", () => {
  const state = paletteState();
  state.palettePush("kon-2");

  assert.deepEqual(groupLabels(state), ["Actions", "Open"]);
  const actions = groupIn(state, "Actions");
  assert.equal(actions.hint, "paused · 3 legal");
  assert.deepEqual(rowTitles(actions.items), ["Resume", "Mark done", "Cancel"]);
  assert.deepEqual(rowTitles(groupIn(state, "Open").items),
    ["Open logs", "Open summary", "Open ticket body"]);
});

test("an unmanaged ticket gets Initialize before its other moves", () => {
  const state = paletteState();
  state.palettePush("gol-3");

  assert.deepEqual(rowTitles(groupIn(state, "Actions").items), ["Initialize", "Queue", "Cancel"]);
});

test("the palette offers no Delete anywhere", () => {
  const state = paletteState();
  state.selectedTicket = { id: "kon-1" };
  state.recomputePalette();
  assert.equal(rowTitles(state._paletteRows).includes("Delete"), false);

  state.palettePush("kon-1");
  assert.equal(rowTitles(state._paletteRows).includes("Delete"), false);
});

test("the highlight wraps across group boundaries", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1"] });
  const ids = rowIds(state._paletteRows);
  assert.equal(ids.length, 8);
  assert.equal(state._paletteSelId, ids[0]);

  state.paletteMove(-1);
  assert.equal(state._paletteSelId, ids[ids.length - 1]);

  state.paletteMove(1);
  assert.equal(state._paletteSelId, ids[0]);
});

test("a live status change keeps the highlight on the same row", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2", "kon-1"] });
  state.paletteMove(1);
  assert.equal(state._paletteSelId, "ticket:kon-1");

  state.queueTicketUpdate({ ...PALETTE_TICKETS[0], status: "human_review" });

  assert.equal(state._paletteSelId, "ticket:kon-1");
  const row = state._paletteRows[state.paletteSel];
  assert.equal(row.id, "ticket:kon-1");
  assert.equal(row.statusText, "review");
});

test("a scope whose ticket disappears falls back to the root list", () => {
  const state = paletteState();
  state.palettePush("kon-2");
  assert.deepEqual(groupLabels(state), ["Actions", "Open"]);

  state.tickets = state.tickets.filter((t) => t.id !== "kon-2");
  state.recomputePalette();

  assert.equal(state.paletteScope, null);
  assert.deepEqual(groupLabels(state), ["Go to"]);
});

test("opening a ticket row closes the palette and opens the detail panel", async () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1"] });
  const opened = [];
  state.selectTicket = async (t) => { opened.push(t.id); state.selectedTicket = t; };

  await state.paletteRun(state._paletteRows[0], false);

  assert.deepEqual(opened, ["kon-1"]);
  assert.equal(state.paletteOpen, false);
  assert.equal(state.toast, "opened kon-1 · live logs");
});

test("opening a ticket row from another view returns to the board first", async () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2"] });
  const calls = [];
  state.currentView = "settings";
  state.gotoView = async (v) => { calls.push("goto:" + v); state.currentView = "board"; };
  state.selectTicket = async (t) => { calls.push("select:" + t.id); };

  await state.paletteRun(state._paletteRows[0], false);

  assert.deepEqual(calls, ["goto:board", "select:kon-2"]);
  assert.equal(state.toast, "opened kon-2");
});

test("a Settings form that refuses to navigate leaves the ticket unselected", async () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2"] });
  const calls = [];
  state.currentView = "settings";
  // What gotoView does with a dirty form: it opens its guard and stays put.
  // Selecting behind it would attach a live terminal to a hidden container.
  state.gotoView = async (v) => { calls.push("goto:" + v); state.settingsGuard = true; };
  state.selectTicket = async (t) => { calls.push("select:" + t.id); };

  await state.paletteRun(state._paletteRows[0], false);

  assert.deepEqual(calls, ["goto:board"]);
  assert.equal(state.settingsGuard, true);
  assert.equal(state.toast, null);
});

test("a ticket already open is not handed to selectTicket, which would close it", async () => {
  const state = paletteState();
  let selects = 0;
  state.selectTicket = async () => { selects += 1; };
  state.selectedTicket = { ...PALETTE_TICKETS[0] };
  state.recomputePalette();

  await state.paletteRun(state._paletteRows.find((r) => r.id === "tab:kon-1:session"), false);

  assert.equal(selects, 0);
  assert.equal(state.activeTab, "session");
});

test("a tab row applies its tab only after selectTicket resolves", async () => {
  const state = paletteState();
  const gate = deferred();
  const calls = [];
  state.selectTicket = async (t) => { calls.push("select"); state.selectedTicket = t; await gate.promise; };
  state.switchTab = (tab) => { calls.push("tab:" + tab); state.activeTab = tab; };
  state.palettePush("kon-1");

  const running = state.paletteRun(state._paletteRows.find((r) => r.id === "tab:kon-1:session"), false);
  assert.deepEqual(calls, ["select"]);

  gate.resolve();
  await running;

  assert.deepEqual(calls, ["select", "tab:session"]);
  assert.equal(state.toast, "kon-1 · open logs");
});

test("a tab the ticket cannot show falls back to the ticket body", async () => {
  const state = paletteState();
  state.selectTicket = async (t) => { state.selectedTicket = t; };
  state.startEditing = () => {};
  state.palettePush("kon-2");

  // kon-2 recorded no summary, so the summary tab would render nothing.
  await state.paletteRun(state._paletteRows.find((r) => r.id === "tab:kon-2:summary"), false);

  assert.equal(state.activeTab, "ticket");
  assert.equal(state.toast, "kon-2 · open ticket body");
});

test("navigation rows dispatch through the existing view and toggle methods", async () => {
  const cases = [
    { id: "nav-board", expect: "goto:board" },
    { id: "nav-stats", expect: "goto:stats" },
    { id: "nav-new", expect: "goto:new" },
    { id: "nav-settings", expect: "goto:settings" },
    { id: "nav-sidebar", expect: "sidebar" },
    { id: "nav-theme", expect: "theme" },
  ];
  for (const c of cases) {
    const state = paletteState();
    const calls = [];
    // Recorded before dispatch: the palette must be closed by then.
    const note = (what) => calls.push(state.paletteOpen ? what + ":open" : what);
    state.gotoView = async (v) => note("goto:" + v);
    state.toggleSidebar = () => note("sidebar");
    state.toggleTheme = () => note("theme");
    state.recomputePalette();

    await state.paletteRun(state._paletteRows.find((r) => r.id === c.id), false);

    assert.deepEqual(calls, [c.expect], c.id);
    assert.equal(state.paletteOpen, false, c.id);
  }
});

test("⌘Enter on a ticket drills into its actions instead of opening it", async () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2"] });
  state.selectTicket = async () => { throw new Error("should not open the ticket"); };

  await state.paletteRun(state._paletteRows[0], true);

  assert.equal(state.paletteScope, "kon-2");
  assert.equal(state.paletteOpen, true);
});

test("an action that is no longer legal sends nothing and says so", async () => {
  let posts = 0;
  const state = paletteState(PALETTE_TICKETS, {
    fetch: async () => { posts += 1; return { ok: true, json: async () => ({}) }; },
  });
  state.palettePush("kon-1");
  const pause = state._paletteRows.find((r) => r.id === "action:kon-1:pause");

  // The agent finished between building the row and pressing Enter.
  state.tickets[0].status = "human_review";

  await state.paletteRun(pause, false);

  assert.equal(posts, 0);
  assert.equal(state.toast, "kon-1 is no longer running");
  assert.equal(state.paletteOpen, false);
});

test("a legal action posts through moveTicketVia and confirms in past tense", async () => {
  const state = paletteState();
  const calls = [];
  state.moveTicketVia = async (id, endpoint, body) => { calls.push([id, endpoint, vmValue(body)]); };
  state.palettePush("kon-1");

  await state.paletteRun(state._paletteRows.find((r) => r.id === "action:kon-1:pause"), false);

  assert.deepEqual(vmValue(calls), [["kon-1", "pause", null]]);
  assert.equal(state.paletteOpen, false);
  assert.equal(state.toast, "kon-1 paused");
});

test("an action carrying a target status sends it as the request body", async () => {
  const state = paletteState();
  const calls = [];
  state.moveTicketVia = async (id, endpoint, body) => { calls.push([id, endpoint, vmValue(body)]); };
  state.palettePush("kon-1");

  await state.paletteRun(state._paletteRows.find((r) => r.id === "action:kon-1:send-to-review"), false);

  assert.deepEqual(vmValue(calls), [["kon-1", "move", { status: "human_review" }]]);
  assert.equal(state.toast, "kon-1 sent to review");
});

test("a failed action is not confirmed as done", async () => {
  // The default harness fetch answers ok:false, so moveTicketVia sets error.
  const state = paletteState();
  state.palettePush("kon-1");

  await state.paletteRun(state._paletteRows.find((r) => r.id === "action:kon-1:pause"), false);

  assert.equal(state.toast, null);
  assert.match(state.error, /pause failed/);
});

test("starting an unmanaged ticket opens initialization and confirms nothing", async () => {
  const state = paletteState();
  let posts = 0;
  state.fetch = async () => { posts += 1; return { ok: true, json: async () => ({}) }; };
  state.palettePush("gol-3");

  await state.paletteRun(state._paletteRows.find((r) => r.id === "action:gol-3:initialize"), false);

  assert.equal(state.initModal, true);
  assert.equal(posts, 0);
  assert.equal(state.toast, null);
});

test("Escape pops the ticket scope before it closes the palette", () => {
  const state = paletteState();
  state.palettePush("kon-1");

  state.palettePop();
  assert.equal(state.paletteScope, null);
  assert.equal(state.paletteOpen, true);

  state.palettePop();
  assert.equal(state.paletteOpen, false);
});

test("opening the palette focuses its input once the panel is on screen", () => {
  // The panel's display flips a frame after paletteOpen, so a focus call in the
  // same tick lands on a hidden input and does nothing: the stub input counts
  // only the calls it took while visible.
  const state = paletteState();

  assert.equal(state.$refs.paletteInput.focused, 1);
});

test("closing the palette returns focus to whatever had it", () => {
  let focused = 0;
  const terminalInput = { focus() { focused += 1; } };
  const state = paletteState(PALETTE_TICKETS, {
    document: { getElementById: () => null, querySelector: () => null, documentElement: { style: {} }, activeElement: terminalInput },
  });

  state.closePalette();

  assert.equal(focused, 1);
  // A hidden input that keeps focus is still a typing target, so every bare
  // shortcut would go dead after one ⌘K.
  assert.equal(state.$refs.paletteInput.blurred, 1);
  assert.equal(state.paletteGroups.length, 0);
});

test("the palette chord is ⌘K on macOS and Ctrl+K elsewhere, from a capture-phase window listener", () => {
  // On macOS Ctrl+K is readline's kill-to-end-of-line, so taking it would break
  // editing the agent's command line in the terminal tab.
  const cases = [
    { platform: "MacIntel", ours: { metaKey: true }, theirs: { ctrlKey: true } },
    { platform: "Win32", ours: { ctrlKey: true }, theirs: { metaKey: true } },
  ];
  for (const c of cases) {
    const listeners = [];
    const state = loadKontoraState({
      navigator: { platform: c.platform, clipboard: { writeText() {} } },
      window: { innerWidth: 1200, addEventListener(type, handler, capture) { listeners.push({ type, handler, capture }); } },
      document: { getElementById: () => null, querySelector: () => null, documentElement: { style: {} }, addEventListener() {} },
    });
    state.$nextTick = (callback) => { if (callback) callback(); return Promise.resolve(); };
    state.$refs = {};
    state._bindGlobalEvents();

    // Capture phase, because xterm reads the keystroke from a hidden textarea
    // and a bubble listener never sees it.
    const captured = listeners.filter((l) => l.type === "keydown" && l.capture === true);
    assert.equal(captured.length, 1, c.platform);

    let prevented = 0;
    let stopped = 0;
    const press = (init) => captured[0].handler({
      preventDefault() { prevented += 1; },
      stopPropagation() { stopped += 1; },
      ...init,
    });

    press({ key: "k", ...c.ours });
    assert.equal(state.paletteOpen, true, c.platform);
    // The "k" must not reach the agent's stdin.
    assert.equal(prevented, 1, c.platform);
    assert.equal(stopped, 1, c.platform);

    press({ key: "K", ...c.ours });
    assert.equal(state.paletteOpen, false, c.platform);

    // The other platform's chord, and a bare "k", pass through untouched.
    press({ key: "k", ...c.theirs });
    press({ key: "k" });
    assert.equal(state.paletteOpen, false, c.platform);
    assert.equal(prevented, 2, c.platform);
  }
});

test("the phone layout leaves the palette closed", () => {
  const state = loadKontoraState({ window: { innerWidth: 500, addEventListener() {} } });
  assert.equal(state.isMobile, true);

  state.openPalette();

  assert.equal(state.paletteOpen, false);
  assert.equal(state.paletteGroups.length, 0);
});

test("→ drills only with the caret at the end of the query", () => {
  const state = paletteState();
  state.paletteQuery = "kon";
  state.recomputePalette();
  const event = (caret) => ({ key: "ArrowRight", target: { selectionStart: caret }, preventDefault() { this.prevented = true; } });

  const mid = event(1);
  state.paletteDrillFromKey(mid);
  assert.equal(state.paletteScope, null);
  assert.equal(mid.prevented, undefined);

  const end = event(3);
  state.paletteDrillFromKey(end);
  assert.equal(state.paletteScope, "kon-1");
  assert.equal(end.prevented, true);
});

test("→ on a command row does nothing", () => {
  const state = paletteState();
  state.paletteQuery = "theme";
  state.recomputePalette();

  state.paletteDrillFromKey({ key: "ArrowRight", target: { selectionStart: 5 }, preventDefault() { throw new Error("should not drill"); } });

  assert.equal(state.paletteScope, null);
});

test("keyboard movement scrolls the highlighted row into view", () => {
  // The list scrolls inside a panel capped at the viewport height, so a
  // highlight arrowed past the fold would leave the screen.
  const scrolled = [];
  const state = paletteState(PALETTE_TICKETS, {
    recents: ["kon-1", "kon-2"],
    document: {
      getElementById: (id) => (id.startsWith("palette-row-")
        ? { scrollIntoView(options) { scrolled.push([id, vmValue(options)]); } }
        : null),
      querySelector: () => null,
      documentElement: { style: {} },
    },
  });

  state.paletteMove(1);

  assert.deepEqual(vmValue(scrolled), [["palette-row-ticket:kon-2", { block: "nearest" }]]);
});

test("editing the query moves the highlight back to the top of the new list", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2"] });
  state.paletteMove(1);
  state.paletteMove(1);
  assert.equal(state._paletteSelId, "nav-stats");

  // recomputePalette follows the highlighted row across a rebuild, which is
  // right for an SSE re-rank and wrong here: "New ticket" survives the query
  // and Enter would run it over the ticket matches now above it.
  state.paletteQuery = "n";
  state.onPaletteQueryChanged();

  assert.equal(state.paletteSel, 0);
  assert.equal(state._paletteSelId, state._paletteRows[0].id);
  assert.equal(state._paletteRows[0].kind, "ticket");
});

test("query matches are ordered like the board, and the hint counts the rows left out", () => {
  const many = Array.from({ length: 8 }, (_, i) => ({
    id: "kon-" + (i + 1), title: "[kontora] Task " + (i + 1), path: "/w/kontora",
    status: i === 7 ? "in_progress" : "todo", stages: [], agent: "claude", kontora: true,
  }));
  const state = paletteState(many);

  state.paletteQuery = "task";
  state.onPaletteQueryChanged();

  const found = groupIn(state, "Tickets");
  assert.equal(found.hint, "8 of 8 · 6 shown");
  // Raw server order leaves the running ticket last; the board ranks it first.
  assert.deepEqual(rowIds(found.items),
    ["ticket:kon-8", "ticket:kon-1", "ticket:kon-2", "ticket:kon-3", "ticket:kon-4", "ticket:kon-5"]);
});

test("a scoped query that matches nothing renders no group headers", () => {
  const state = paletteState();
  state.palettePush("kon-2");

  state.paletteQuery = "zzzz";
  state.onPaletteQueryChanged();

  assert.deepEqual(groupLabels(state), []);
  assert.equal(state._paletteRows.length, 0);
});

test("the legal-move count in the scope header does not move with the query", () => {
  const state = paletteState();
  state.palettePush("kon-2");
  assert.equal(groupIn(state, "Actions").hint, "paused · 3 legal");

  state.paletteQuery = "cancel";
  state.onPaletteQueryChanged();

  assert.equal(groupIn(state, "Actions").hint, "paused · 3 legal");
  assert.deepEqual(rowTitles(groupIn(state, "Actions").items), ["Cancel"]);
});

test("hovering a row moves the highlight to it", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1", "kon-2"] });

  state.paletteHover("ticket:kon-2");

  assert.equal(state._paletteSelId, "ticket:kon-2");
  assert.equal(state.paletteSel, 1);
});

test("opened tickets are remembered most recent first, deduplicated and capped at three", async () => {
  const stored = {};
  const state = loadKontoraState({
    localStorage: {
      getItem(k) { return Object.prototype.hasOwnProperty.call(stored, k) ? stored[k] : null; },
      setItem(k, v) { stored[k] = String(v); },
    },
    fetch: async () => ({ ok: false, json: async () => ({}) }),
  });
  state.tickets = ["kon-1", "kon-2", "kon-3", "kon-4"].map((id) => ({ id, title: id, status: "todo", kontora: true }));
  state.startEditing = () => {};

  for (const id of ["kon-1", "kon-2", "kon-1", "kon-3", "kon-4"]) {
    state.selectedTicket = null;
    await state.selectTicket(state.tickets.find((t) => t.id === id));
  }

  assert.deepEqual([...state.recentTickets], ["kon-4", "kon-3", "kon-1"]);
  assert.deepEqual(JSON.parse(stored["kontora-recent-tickets"]), ["kon-4", "kon-3", "kon-1"]);
});

test("recent tickets survive a reload and drop ids that no longer exist", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-2", "gone-9", "kon-1"] });

  assert.deepEqual(rowIds(groupIn(state, "Recent").items), ["ticket:kon-2", "ticket:kon-1"]);
});

test("the palette input and rows carry the listbox semantics", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /role="dialog" aria-modal="true" aria-label="Command palette"/);
  assert.match(html, /id="palette-list" role="listbox"/);
  assert.match(html, /role="combobox"[\s\S]{0,200}?:aria-activedescendant="_paletteSelId \? 'palette-row-' \+ _paletteSelId : null"/);
  assert.match(html, /:id="'palette-row-' \+ row\.id" role="option" :aria-selected="_paletteSelId === row\.id"/);
  // The group wrapper, its header, and the no-match line stand between the
  // listbox and its options; ARIA only keeps the options owned when they do
  // not carry a role of their own.
  assert.match(html, /<div role="presentation" class="px-1\.5">/);
  assert.match(html, /<div role="presentation" class="flex items-baseline gap-2 px-2\.5/);
  assert.match(html, /x-show="paletteQuery && !_paletteRows\.length" role="presentation"/);
});

test("a row id can be referenced by aria-activedescendant, which takes no whitespace", () => {
  const state = paletteState(PALETTE_TICKETS, { recents: ["kon-1"] });
  state.selectedTicket = { id: "kon-1" };
  state.recomputePalette();
  const ids = rowIds(state._paletteRows);
  state.palettePush("kon-1");
  ids.push(...rowIds(state._paletteRows));

  assert.deepEqual(ids.filter((id) => /\s/.test(id)), []);
  assert.equal(ids.includes("action:kon-1:send-to-review"), true);
});

test("the palette dismisses on a press that starts on the scrim, not on a drag that ends there", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // A drag from the input that ends outside the panel dispatches its click on
  // the scrim, so dismissing on click would discard the query being selected.
  assert.match(html, /x-show="paletteOpen" x-cloak @mousedown\.self="closePalette\(\)"/);
  assert.equal(/@click="closePalette\(\)"/.test(html), false);
});

test("⌫ and ← pop a ticket scope and leave the root palette open", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // Both keys reach palettePop, which closes the palette when there is no
  // scope. At the root ← only moves the caret, and closing on it is a surprise.
  assert.match(html, /@keydown\.backspace="if \(!paletteQuery && paletteScope\) \{ \$event\.preventDefault\(\); palettePop\(\); \}"/);
  assert.match(html, /@keydown\.arrow-left="if \(!paletteQuery && paletteScope\) \{ \$event\.preventDefault\(\); palettePop\(\); \}"/);
});

test("init wires the palette query watcher to the query handler", () => {
  // Every palette test below drives recomputePalette by hand, because the VM
  // harness has no Alpine $watch. Without this assertion, dropping the watcher
  // leaves the suite green and the palette deaf to typing.
  assert.match(uiSource, /this\.\$watch\('paletteQuery', \(\) => this\.onPaletteQueryChanged\(\)\)/);
});

test("the palette leads the Escape stack and the highlight follows the mouse, not the pointer entering", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /@keydown\.escape\.window="if \(paletteOpen\) palettePop\(\); else if \(deleteModal\)/);
  // mouseenter would move the highlight when the list re-renders under a
  // stationary cursor.
  assert.match(html, /@mousemove="paletteHover\(row\.id\)"/);
  assert.equal(/@mouseenter="paletteHover/.test(html), false);
});

test("the filter input keeps a plain filter and a ⌘K keycap", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The autocomplete dropdown and its handlers are gone.
  assert.equal(/searchOpen|suggestions|selectedIndex|updateSuggestions|applySuggestion|acceptSelection|moveSelection|flatIndex/.test(html), false);
  assert.match(html, /x-show="!searchQuery" @click="openPalette\(\)"/);
  assert.match(html, /x-text="isMac \? '⌘K' : 'Ctrl K'"/);
  // Room for the keycap next to the clear button.
  assert.match(html, /placeholder="Filter tickets…"[\s\S]{0,200}?pr-11/);
});

test("the palette animations are dropped under reduced motion", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /\.pal-panel \{ animation: pal-in/);
  assert.match(html, /\.pal-scrim \{ animation: scrim-in/);
  const reduced = html.match(/@media \(prefers-reduced-motion: reduce\) \{(?:[^{}]|\{[^{}]*\})*?\.pal-panel, \.pal-scrim \{ animation: none; \}/);
  assert.notEqual(reduced, null);
});

test("the UI sources keep no autocomplete leftovers", () => {
  assert.equal(/searchOpen|suggestions|selectedIndex/.test(uiSource), false);
});

test("every color utility the page writes exists in the built app.css", () => {
  // Tailwind's opacity scale runs in steps of 5, so bg-ok/14 silently emits
  // nothing and the element renders with no fill. Arbitrary values (/[0.14])
  // always emit. Nothing else catches this: the class is valid-looking text.
  const css = fs.readFileSync(path.join(dirname, "../static/app.css"), "utf8");
  const sources = fs.readFileSync(htmlPath, "utf8") + "\n" + uiSource;

  // Match the class anywhere, not just after a dot: hover:bg-accent/30 is
  // emitted as .hover\:bg-accent\/30:hover.
  const escape = (s) => s.replace(/[\\^$.*+?()[\]{}|/]/g, "\\$&");
  const missing = new Set();
  for (const m of sources.matchAll(/\b((?:bg|text|border|ring|from|via|to)-[a-z]+(?:-[a-z0-9]+)?\/\d+)\b/g)) {
    if (!new RegExp(escape(m[1].replace("/", "\\/")) + "(?!\\d)").test(css)) missing.add(m[1]);
  }
  assert.deepEqual([...missing], []);
});

// The fake location a router test drives. Assigning hash records the write so
// a test can assert the inverse mapping without a real browser.
function routerState(hash = "") {
  const location = { protocol: "http:", host: "localhost:8080", hash };
  const state = loadKontoraState({ location });
  state.$nextTick = (cb) => { if (cb) cb(); return Promise.resolve(); };
  state.closeTerminal = () => {};
  state.startEditing = () => {};
  state.flushEditSave = () => {};
  state.recomputeBoard = () => {};
  state.fetchChanges = () => {};
  state.fetchStageLogs = () => {};
  state.openTerminal = () => {};
  state.openSettings = async () => { state.currentView = "settings"; };
  return { state, location };
}

test("the hash router maps each route to a view", async () => {
  const cases = [
    { hash: "#/", view: "board", ticket: null },
    { hash: "", view: "board", ticket: null },
    { hash: "#/new", view: "new", ticket: null },
    { hash: "#/stats", view: "stats", ticket: null },
    { hash: "#/settings", view: "settings", ticket: null },
    { hash: "#/t/kon-vd0j", view: "board", ticket: "kon-vd0j" },
    { hash: "#/nonsense", view: "board", ticket: null },
  ];

  for (const tc of cases) {
    const { state, location } = routerState(tc.hash);
    state.tickets = [{ id: "kon-vd0j", status: "todo" }];
    await state.applyRoute();
    assert.equal(state.currentView, tc.view, tc.hash);
    assert.equal(state.selectedTicket ? state.selectedTicket.id : null, tc.ticket, tc.hash);
    // Serializing the resulting view restores the same hash.
    assert.equal(state.routeHash(), tc.hash === "" || tc.hash === "#/nonsense" ? "#/" : tc.hash, tc.hash);
    assert.equal(location.hash, state.routeHash(), tc.hash);
  }
});

test("selecting and closing a ticket writes and clears the hash", async () => {
  const { state, location } = routerState("#/");
  state.tickets = [{ id: "kon-1", status: "todo" }];

  await state.selectTicket(state.tickets[0]);
  assert.equal(location.hash, "#/t/kon-1");

  state.closeDetail();
  assert.equal(location.hash, "#/");
  assert.equal(state.selectedTicket, null);
});

test("a hash naming an unknown ticket falls back to the board", async () => {
  const { state, location } = routerState("#/t/ghost");
  state.tickets = [{ id: "kon-1", status: "todo" }];

  await state.applyRoute();

  assert.equal(state.selectedTicket, null);
  assert.equal(state.currentView, "board");
  assert.equal(location.hash, "#/");
});

test("a malformed escape in the hash does not throw", async () => {
  const { state, location } = routerState("#/t/%");
  state.tickets = [{ id: "kon-1", status: "todo" }];

  // applyRoute is awaited in init() before the board is rendered, so a throw
  // here would leave the page blank rather than just mis-routing.
  await state.applyRoute();

  assert.equal(state.parseHash("#/t/%").ticketId, "%");
  assert.equal(state.selectedTicket, null);
  assert.equal(location.hash, "#/");
});

test("mobileSwitchTab drives the terminal without touching the desktop tab", () => {
  const cases = [
    {
      name: "opening the mobile terminal",
      tab: "terminal",
      status: "in_progress",
      check: (s, calls) => {
        assert.equal(s.detailTab, "terminal");
        assert.deepEqual(calls, ["open"]);
        assert.equal(s.terminalWanted(), true);
      },
    },
    {
      name: "leaving it for the logs tab",
      tab: "logs",
      status: "in_progress",
      check: (s, calls) => {
        assert.equal(s.detailTab, "logs");
        assert.deepEqual(calls, ["close"]);
        assert.equal(s.terminalWanted(), false);
      },
    },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    const calls = [];
    state.isMobile = true;
    state.activeTab = "ticket";
    state.terminalOpen = true;
    state.selectedTicket = { id: "kon-1", status: c.status, stage: "plan" };
    state.openTerminal = () => calls.push("open");
    state.closeTerminal = () => calls.push("close");
    state.fetchStageLogs = () => {};

    state.mobileSwitchTab(c.tab);

    assert.equal(state.activeTab, "ticket", `${c.name}: desktop tab must not move`);
    c.check(state, calls);
  }
});

// ---------------------------------------------------------------------------
// Ticket page: ribbon, transcript, rail
// ---------------------------------------------------------------------------

// A state wired for the page's derived views, with no network of its own.
function pageState(ticket, overrides = {}) {
  const state = loadKontoraState();
  state.selectedTicket = ticket;
  state.now = Date.parse("2026-08-08T12:00:00Z");
  Object.assign(state, overrides);
  return state;
}

const TAPE_TICKET = {
  id: "kon-1",
  status: "human_review",
  pipeline: "review-pipe",
  stage: "commit",
  stages: ["plan", "review", "commit"],
  history: [
    { stage: "plan", exit_code: 0, run: 0, started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:40Z" },
    { stage: "review", exit_code: 1, run: 0, started_at: "2026-08-08T11:01:00Z", completed_at: "2026-08-08T11:01:20Z" },
    { stage: "review", exit_code: 0, run: 1, started_at: "2026-08-08T11:06:00Z", completed_at: "2026-08-08T11:06:30Z" },
  ],
};

test("the ribbon sizes each stage by its real duration", () => {
  const state = pageState(TAPE_TICKET);
  const segs = state.stageRibbon();

  assert.deepEqual(segs.map((s) => s.name), ["plan", "review", "commit"]);
  assert.equal(segs[0].seconds, 40);
  // Two runs of 20s and 30s, five minutes apart: the queue gap does not count.
  assert.equal(segs[1].seconds, 50);
  assert.equal(segs[1].runs, 2);
  assert.equal(segs[2].seconds, 0, "a stage with no run has no duration");
  assert.equal(segs[2].state, "queued");
  assert.equal(segs[2].meta, "not started");
});

test("a long stage grows more than a short one", () => {
  const state = pageState({
    id: "kon-2", status: "done", stage: "build", stages: ["lint", "build"],
    history: [
      { stage: "lint", exit_code: 0, run: 0, started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:40Z" },
      { stage: "build", exit_code: 0, run: 0, started_at: "2026-08-08T11:01:00Z", completed_at: "2026-08-08T11:13:00Z" },
    ],
  });
  const segs = state.stageRibbon();

  assert.equal(segs[0].seconds, 40);
  assert.equal(segs[1].seconds, 720);
  assert.equal(segs.every((s) => s.state === "done"), true);
});

test("the running stage animates and returns to the live session on click", () => {
  const state = pageState({
    id: "kon-3", status: "in_progress", stage: "build", stages: ["lint", "build"],
    started_at: "2026-08-08T11:59:00Z",
    history: [{ stage: "lint", exit_code: 0, run: 0, started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:40Z" }],
  });
  state.openTerminal = () => {};
  state.closeTerminal = () => {};

  const segs = state.stageRibbon();
  assert.equal(segs[1].state, "running");
  assert.equal(segs[1].seconds, 60, "a running stage counts the time since pickup");

  state.clickRibbon(segs[1]);
  assert.equal(state.activeTab, "session");
});

test("clicking a finished segment loads that run's transcript", () => {
  const asked = [];
  const state = pageState(TAPE_TICKET, { fetchActivity: (stage, run) => asked.push([stage, run]) });

  const review = state.stageRibbon()[1];
  state.clickRibbon(review);

  assert.equal(state.activeTab, "activity");
  // The segment addresses the newest run of the stage it stands for.
  assert.deepEqual(asked, [["review", 1]]);
});

test("meters follow the ticket's status", () => {
  const configCache = {
    pipeline_infos: [{ name: "review-pipe", stages: ["plan", "review", "commit"], max_retries: [0, 1, 0] }],
  };
  const running = pageState({
    ...TAPE_TICKET, status: "in_progress", stage: "review", attempt: 1,
    started_at: "2026-08-08T11:59:00Z",
  }, { configCache });
  running.activity = { tape: { totals: { input: 100, output: 200, cache_create: 300, cache_read: 400 } } };

  // ribbonMeters builds its array inside the VM realm, so copy it out before
  // a deep-equal against a literal.
  const keys = Array.from(running.ribbonMeters(), (m) => m.k);
  assert.deepEqual(keys, ["elapsed", "tokens", "attempt"]);
  assert.equal(running.ribbonMeters().find((m) => m.k === "attempt").v, "2 / 2");
  assert.equal(running.ribbonMeters().find((m) => m.k === "tokens").v, "1k");

  // started_at is rewritten at every stage spawn, so here it is the last
  // stage's pickup. Wall spans the whole ticket instead: the first history
  // entry's start (11:00:00) to the last exit (11:06:30).
  const review = pageState({ ...TAPE_TICKET, started_at: "2026-08-08T11:06:00Z" }, { configCache });
  assert.deepEqual(Array.from(review.ribbonMeters(), (m) => m.k), ["wall"], "no verified usage means no tokens meter");
  assert.equal(review.ribbonMeters()[0].v, "6m");
});

test("a tape that declares usage partial shows no token count", () => {
  const state = pageState(TAPE_TICKET);
  state.activity = { tape: { partial: ["time", "usage", "is_error"], totals: { input: 5 } } };

  assert.equal(state.tapeTokens(state.activity.tape), "");
  assert.equal(state.tapePartial("time"), true);
  assert.equal(state.eventTime({ time: "2026-08-08T11:00:00Z" }), "", "a partial tape has no timestamp gutter");
});

test("tool rows expand by id, and a failure starts expanded", () => {
  const state = pageState(TAPE_TICKET);
  state.activity = {
    source: "events",
    tape: {
      events: [
        { kind: "model", model: "claude-opus-4-6" },
        { kind: "text", text: "Running the tests." },
        { kind: "tool", id: "t1", tool: "Bash", arg: " go test ./...", summary: "ok", result: "ok" },
        { kind: "tool", id: "t2", tool: "Read", arg: " /x.go", summary: "err", result: "File does not exist.", is_error: true },
      ],
    },
  };
  const events = state.tapeEvents();
  assert.equal(events.length, 4);

  assert.equal(state.toolExpanded(events[2], 2), false);
  state.toggleTool(events[2], 2);
  assert.equal(state.toolExpanded(events[2], 2), true);

  assert.equal(state.toolFailed(events[3]), true);
  assert.equal(state.toolExpanded(events[3], 3), true, "a failure is never collapsed by default");
});

// A state holding a synthetic tape of n events. Even indices are tool rows with
// no id, the shape a Pi session produces, so the expand map has to fall back to
// the event's place in the tape.
function tapeState(n, overrides = {}) {
  const events = [];
  for (let i = 0; i < n; i++) {
    events.push(i % 2 === 0
      ? { kind: "tool", tool: "Bash", arg: `step ${i}`, result: `out ${i}` }
      : { kind: "text", text: `line ${i}` });
  }
  const state = pageState(TAPE_TICKET, overrides);
  state.activity = { source: "events", tape: { events } };
  state.$nextTick = (callback) => { callback(); };
  return state;
}

// A state whose activity-scroll element reports a height that follows the
// rendered row count, at 10px a row.
function scrolledTape(n, scrollTop) {
  let state;
  const scroller = {
    scrollTop,
    clientHeight: 400,
    get scrollHeight() { return Math.min(state.tapeWindow, n) * 10; },
  };
  state = loadKontoraState({
    document: {
      getElementById: (id) => (id === "activity-scroll" ? scroller : null),
      querySelector: () => null,
      documentElement: { style: {} },
    },
  });
  state.selectedTicket = TAPE_TICKET;
  state.activity = { tape: { events: Array.from({ length: n }, (_, i) => ({ kind: "text", text: `line ${i}` })) } };
  state.$nextTick = (callback) => { callback(); };
  return { state, scroller };
}

test("a tape at the event cap mounts only its newest window", () => {
  const state = tapeState(5000);
  const size = state.tapeWindow;

  assert.equal(size, 200, "the measured step: 5000 events cost 700ms to mount, this many cost 30ms");
  const shown = Array.from(state.visibleTapeEvents(), (r) => r.idx);
  assert.deepEqual(shown, Array.from({ length: size }, (_, i) => 5000 - size + i));
  assert.equal(state.hiddenTapeEventCount(), 5000 - size);
  assert.equal(state.visibleTapeEvents()[0].ev, state.tapeEvents()[5000 - size], "a row carries the event at its own index");
});

test("a tape shorter than the window renders whole", () => {
  const state = tapeState(12);

  assert.deepEqual(Array.from(state.visibleTapeEvents(), (r) => r.idx), [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]);
  assert.equal(state.hiddenTapeEventCount(), 0, "no control appears when the whole tape is on screen");
});

test("loading earlier events grows the range one step at a time", () => {
  const state = tapeState(1000);
  const step = state.tapeWindow;
  const shown = () => Array.from(state.visibleTapeEvents(), (r) => r.idx);

  assert.deepEqual([shown()[0], shown().at(-1), state.hiddenTapeEventCount()], [1000 - step, 999, 1000 - step]);

  state.loadEarlierTapeEvents();
  assert.deepEqual([shown()[0], shown().at(-1), state.hiddenTapeEventCount()], [1000 - 2 * step, 999, 1000 - 2 * step]);

  // The last step is clamped to the tape, and the control goes away with it.
  while (state.hiddenTapeEventCount() > 0) state.loadEarlierTapeEvents();
  assert.equal(state.tapeWindow, 1000);
  assert.deepEqual([shown()[0], shown().length], [0, 1000]);
});

test("loading earlier events holds the reader's place", () => {
  const { state, scroller } = scrolledTape(1000, 300);
  const step = state.tapeWindow;
  const before = scroller.scrollHeight;

  state.loadEarlierTapeEvents();

  assert.equal(scroller.scrollHeight, before + step * 10, "one step of rows was prepended");
  assert.equal(scroller.scrollTop, 300 + step * 10, "the reader is still on the row they were reading");
});

test("follow still lands the transcript on the newest event", () => {
  const { state, scroller } = scrolledTape(5000, 0);

  assert.equal(state.logFollow, true);
  state.scrollLogToEnd();

  // The browser clamps this write to scrollHeight - clientHeight; the stub
  // records it raw.
  assert.equal(scroller.scrollTop, scroller.scrollHeight);
});

test("an expanded row keeps its identity when earlier events load", () => {
  const state = tapeState(1000);
  const rowAt = (i) => state.visibleTapeEvents().find((r) => r.idx === i);

  const row = rowAt(900);
  assert.equal(state.toolKey(row.ev, row.idx), "i900", "an id-less row is keyed by its place in the full tape");
  state.toggleTool(row.ev, row.idx);

  state.loadEarlierTapeEvents();

  const grown = rowAt(900);
  assert.equal(state.toolExpanded(grown.ev, grown.idx), true);
  assert.deepEqual(vmValue(state.expandedTools), { i900: true });
  const earlier = rowAt(700);
  assert.equal(state.toolExpanded(earlier.ev, earlier.idx), false, "the row 200 places back did not inherit the expansion");
});

test("every activity load starts back at the newest window", async () => {
  const state = tapeState(1000);
  const size = state.tapeWindow;
  state.loadEarlierTapeEvents();
  assert.equal(state.tapeWindow, size * 2);

  state._resetActivity();
  assert.equal(state.tapeWindow, size, "closing the detail view drops the grown window");

  state.tapeWindow = size * 3;
  await state.fetchActivity("review", 1);
  assert.equal(state.tapeWindow, size, "another run opens at its own tail");
});

test("the plaintext fallback renders one block and no window control", () => {
  const state = pageState(TAPE_TICKET);
  state.activity = { content: "alpha\nbeta" };

  assert.equal(state.renderLogHTML(state.activity.content), "alpha\nbeta");
  assert.equal(state.hiddenTapeEventCount(), 0);
  assert.deepEqual(Array.from(state.visibleTapeEvents()), []);
});

test("index.html binds the transcript to the windowed tail", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /x-for="\{ ev, idx \} in visibleTapeEvents\(\)" :key="idx"/);
  // Tool state is read by the full-array index, never by the window offset.
  assert.match(html, /@click="toggleTool\(ev, idx\)"/);
  assert.equal((html.match(/toolExpanded\(ev, idx\)/g) || []).length, 2);
  assert.equal(/\b(toggleTool|toolExpanded)\(ev, i\)/.test(html), false);
  // One control above the first rendered event, carrying the hidden count.
  assert.match(html, /<template x-if="hiddenTapeEventCount\(\) > 0">/);
  assert.match(html, /<button type="button" @click="loadEarlierTapeEvents\(\)"/);
  assert.match(html, /'load earlier events \(' \+ hiddenTapeEventCount\(\)/);
  // The fallback keeps its single block, and follow still fires on the payload
  // rather than on the window.
  assert.equal((html.match(/id="stage-log-pre"/g) || []).length, 1);
  assert.match(html, /x-effect="activity; if \(logFollow\)/);
});

test("index.html marks a live run and holds back the finished-run states", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The pill sits beside the attempt chip and takes the running ribbon's colour.
  assert.match(html, /x-show="activity\?\.live"[^>]*text-st-progress">live<\/span>/);
  // The stale banner names a finished attempt, which a run in flight is not.
  assert.match(html, /x-show="activity\?\.stale && !activity\?\.live"/);
  // "No completed stage yet." is false while a stage is running.
  assert.match(html, /x-if="!activityLoading && !activityError && !activity && !runningRun\(\)"/);
  // A run in flight with nothing recorded yet is waiting, not empty, and says
  // so once for both the structured and the plaintext source.
  assert.match(html, /x-if="!activityLoading && activity\?\.live && !tapeEvents\(\)\.length && !activity\.content"/);
  assert.equal((html.match(/Waiting for the agent's first step/g) || []).length, 1);
  assert.match(html, /x-if="!tapeEvents\(\)\.length && !activity\?\.live"/);
});

test("error styling is suppressed when the tape cannot verify it", () => {
  const state = pageState(TAPE_TICKET);
  state.activity = { tape: { partial: ["time", "usage", "is_error"], events: [{ kind: "tool", tool: "read", is_error: true }] } };

  assert.equal(state.toolFailed(state.tapeEvents()[0]), false);
  assert.equal(state.toolExpanded(state.tapeEvents()[0], 0), false);
});

test("the plaintext fallback is escaped before it reaches the DOM", () => {
  const state = pageState(TAPE_TICKET);
  const html = state.renderLogHTML('> Bash <img src=x onerror=alert(1)>\nplain <b>text</b>');

  assert.equal(html.includes("<img"), false);
  assert.equal(html.includes("<b>text</b>"), false);
  assert.match(html, /&lt;img/);
});

test("the activity tab opens the run in flight", async () => {
  const asked = [];
  const state = pageState({ ...TAPE_TICKET, status: "in_progress", stage: "commit" }, {
    fetchActivity: (stage, run) => asked.push([stage, run]),
  });
  state.flushEditSave = () => {};
  state.openTerminal = () => {};

  state.switchTab("activity");

  assert.equal(state.activeTab, "activity");
  assert.deepEqual(asked, [["commit", 0]], "commit has no history rows, so it is running its first run");
});

test("the activity header reports the effort of the run it shows", () => {
  const state = pageState({
    ...TAPE_TICKET,
    history: TAPE_TICKET.history.map((h) =>
      h.stage === "review" && h.run === 1 ? { ...h, model: "opus", effort: "high" } : h),
  });
  state.activityStage = "review";
  state.activityRun = 1;
  // The tape reports the model the agent itself ran as, which need not be the
  // alias Kontora passed. Only the effort is chipped, so the two never disagree
  // on screen.
  state.activity = { tape: { model: "claude-opus-4-6" } };

  assert.equal(state.activityEffort(), "high");
  assert.equal(vmValue(state.activityHistoryEntry()).model, "opus");

  // One model chip, and it is the tape's. A config-derived second one would
  // disagree with it whenever the alias and the reported id differ.
  const header = fs.readFileSync(htmlPath, "utf8").split("<!-- Activity:")[1].split("</div>")[0];
  assert.equal(header.match(/x-text="activity\?\.tape\?\.model"/g).length, 1);
  assert.equal(/activityHistoryEntry\(\)/.test(header), false);
  assert.match(header, /x-text="activityEffort\(\)"/);

  // A stale payload is the newest run of the stage, not the one that was asked
  // for, so the effort of the asked-for run would label another run's output.
  state.activity = { stale: true };
  assert.equal(state.activityEffort(), "");

  // The other run of the same stage recorded nothing, so no chip.
  state.activity = { tape: { model: "claude-opus-4-6" } };
  state.activityRun = 0;
  assert.equal(state.activityEffort(), "");
  assert.equal(vmValue(state.activityHistoryEntry()).effort, undefined);

  // A run in flight has no history entry yet.
  state.activityStage = "commit";
  assert.equal(state.activityHistoryEntry(), null);
  assert.equal(state.activityEffort(), "");
});

test("the run in flight is numbered by the stage's finished runs", () => {
  // review already ran twice, so the run under way is run 2.
  const state = pageState({ ...TAPE_TICKET, status: "in_progress", stage: "review" });

  assert.deepEqual(vmValue(state.runningRun()), { stage: "review", run: 2 });
  assert.deepEqual(vmValue(state.activityTarget()), { stage: "review", run: 2 });

  const finished = pageState(TAPE_TICKET);
  assert.equal(finished.runningRun(), null);
  assert.deepEqual(vmValue(finished.activityTarget()), { stage: "review", run: 1 });
});

test("the activity tab of a finished ticket still opens its last run", async () => {
  const asked = [];
  const state = pageState(TAPE_TICKET, { fetchActivity: (stage, run) => asked.push([stage, run]) });
  state.flushEditSave = () => {};

  state.switchTab("activity");

  assert.deepEqual(asked, [["review", 1]]);
});

// --- the live activity poll ---

const RUNNING_TICKET = { ...TAPE_TICKET, status: "in_progress", stage: "commit" };

const tapeEvent = (n) => ({ kind: "text", text: `line ${n}` });

// livePayload builds one /activity response body for the running commit run.
const livePayload = (events, { offset = 0, live = true } = {}) => ({
  source: "events", stage: "commit", run: 0, live, offset,
  tape: { version: 1, agent: "claude", events },
});

// activityHarness drives the poll with a queue of responses and a timer double,
// so a test advances time itself rather than waiting two seconds per tick.
// Each queue entry is [status, body, etag, hold]; a held entry answers only when
// the test calls release(), which is how a request is made to land out of order.
function activityHarness(responses, ticket = RUNNING_TICKET) {
  const calls = [];
  const timers = new Map();
  const held = [];
  let nextTimer = 1;
  const state = loadKontoraState({
    setTimeout: (fn, ms) => {
      timers.set(nextTimer, { fn, ms });
      return nextTimer++;
    },
    clearTimeout: (id) => timers.delete(id),
    fetch: async (url, init = {}) => {
      calls.push({ url, ifNoneMatch: ((init && init.headers) || {})["If-None-Match"] || null });
      const [status, body, etag, hold] = responses.shift() || [500, {}, null];
      const response = {
        ok: status >= 200 && status < 300,
        status,
        headers: { get: (name) => (name.toLowerCase() === "etag" ? etag || null : null) },
        json: async () => body,
      };
      if (hold) return new Promise((resolve) => held.push(() => resolve(response)));
      return response;
    },
  });
  state.selectedTicket = ticket;
  state.activeTab = "activity";
  state.flushEditSave = () => {};
  state.closeTerminal = () => {};
  state.startEditing = () => {};
  state.recomputeBoard = () => {};
  state.writeHash = () => {};
  state.$nextTick = (callback) => { if (callback) callback(); return Promise.resolve(); };

  return {
    state,
    calls,
    // Whether a poll is armed, and how far out.
    pending: () => (state._activityPoll === null ? 0 : 1),
    delay: () => timers.get(state._activityPoll).ms,
    // Fire the armed poll.
    tick: async () => {
      const armed = timers.get(state._activityPoll);
      assert.ok(armed, "no poll was armed");
      timers.delete(state._activityPoll);
      armed.fn();
      await flushMicrotasks();
    },
    // Answer the oldest held request.
    release: async () => {
      const answer = held.shift();
      assert.ok(answer, "no request was held");
      answer();
      await flushMicrotasks();
    },
  };
}

test("a running stage keeps polling its transcript every two seconds", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, livePayload([tapeEvent(1)], { offset: 1 }), '"b"'],
    [200, { ...livePayload([tapeEvent(0), tapeEvent(1), tapeEvent(2)]), live: false }, '"c"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  assert.equal(h.state.activity.live, true);
  assert.equal(h.pending(), 1, "a live payload arms the next poll");
  assert.equal(h.delay(), 2000);

  await h.tick();
  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0", "line 1"]);
  assert.match(h.calls[1].url, /after=1/, "the poll sends the count of events it already holds");
  assert.equal(h.calls[1].ifNoneMatch, '"a"', "and the validator of the payload it holds");
  assert.equal(h.pending(), 1);

  // The run has ended: the payload is the completed transcript and nothing is
  // scheduled after it.
  await h.tick();
  assert.equal(h.state.activity.live, false);
  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0", "line 1", "line 2"]);
  assert.equal(h.pending(), 0, "a finished run schedules no further request");
});

test("a poll leaves the reader's expansions, window and loading state alone", async () => {
  const h = activityHarness([
    [200, livePayload([{ kind: "tool", tool: "Bash", id: "t1", arg: "go test" }]), '"a"'],
    [200, livePayload([{ kind: "tool", tool: "Bash", id: "t1", arg: "go test", summary: "FAIL", result: "boom", is_error: true }, tapeEvent(1)]), '"b"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  h.state.toggleTool(h.state.tapeEvents()[0], 0);
  assert.deepEqual(vmValue(h.state.expandedTools), { t1: true });

  await h.tick();

  assert.deepEqual(vmValue(h.state.expandedTools), { t1: true }, "the expanded row survives the tick");
  assert.equal(h.state.activityLoading, false, "a refresh shows no spinner");
  assert.equal(h.state.activityError, null);
  // The server walked its cursor back over the tool row, so the result lands on
  // the row already on screen instead of being lost.
  assert.equal(h.state.tapeEvents()[0].result, "boom");
  assert.equal(h.state.toolFailed(h.state.tapeEvents()[0]), true);
});

test("a live run without a structured record keeps tailing its plaintext log", async () => {
  const logPayload = (content) => ({ source: "log", stage: "commit", run: 0, live: true, stale: true, content });
  const h = activityHarness([
    [200, logPayload("first line\n"), null],
    [200, logPayload("first line\nsecond line\n"), null],
  ]);

  await h.state.fetchActivity("commit", 0);
  await h.tick();

  assert.equal(h.state.activity.content, "first line\nsecond line\n");
  assert.equal(h.state.activity.tape, undefined, "a plaintext payload must not grow an empty tape");
});

test("a poll with follow off keeps every visible row in the window", async () => {
  const first = [];
  for (let i = 0; i < 250; i++) first.push(tapeEvent(i));
  const h = activityHarness([
    [200, livePayload(first), '"a"'],
    [200, livePayload([tapeEvent(250), tapeEvent(251)], { offset: 250 }), '"b"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  h.state.logFollow = false;
  const oldest = h.state.visibleTapeEvents()[0].idx;

  await h.tick();

  assert.equal(h.state.tapeWindow, 202, "the window grew by the two events that arrived");
  assert.equal(h.state.visibleTapeEvents()[0].idx, oldest, "no row fell off the top");
});

test("a poll with follow on leaves the window at its own size", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, livePayload([tapeEvent(1)], { offset: 1 }), '"b"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  assert.equal(h.state.logFollow, true);

  await h.tick();

  assert.equal(h.state.tapeWindow, 200);
});

test("an unchanged transcript costs a 304 and changes nothing", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [304, {}, '"a"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  const before = vmValue(h.state.activity);

  await h.tick();

  assert.deepEqual(vmValue(h.state.activity), before);
  assert.equal(h.pending(), 1, "the poll keeps running while the run does");
});

test("a failed poll keeps the transcript and only the third one is reported", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [500, {}, null],
    [500, {}, null],
    [500, { error: "daemon unreachable" }, null],
  ]);

  await h.state.fetchActivity("commit", 0);

  await h.tick();
  assert.equal(h.state.activityError, null, "one dropped request is not worth an error");
  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0"]);

  await h.tick();
  assert.equal(h.state.activityError, null);

  await h.tick();
  assert.equal(h.state.activityError, "daemon unreachable");
  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0"], "the transcript is still on screen");
});

test("a payload for a run the reader has left is discarded", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, livePayload([tapeEvent(1)], { offset: 1 }), '"b"'],
  ]);

  await h.state.fetchActivity("commit", 0);

  // The reader clicks another stage in the ribbon while the poll is in flight.
  const inFlight = h.state._loadActivity("commit", 0, { merge: true });
  h.state.activityStage = "review";
  h.state.activityRun = 1;
  await inFlight;

  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0"], "the answer for the old run was dropped");
});

test("a poll answered after the finished transcript arrived is discarded", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    // The poll in flight when the run ended. Its answer is held back until
    // after the finished transcript has arrived.
    [200, livePayload([tapeEvent(1)], { offset: 1 }), '"b"', true],
    [200, { ...livePayload([tapeEvent(0), tapeEvent(1), tapeEvent(2)]), live: false }, '"done"'],
  ]);
  h.state.tickets = [RUNNING_TICKET];

  await h.state.fetchActivity("commit", 0);

  const inFlight = h.state._loadActivity("commit", 0, { merge: true });
  h.state.applyTicketUpdate({ ...RUNNING_TICKET, status: "human_review" });
  await flushMicrotasks();
  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0", "line 1", "line 2"]);

  await h.release();
  await inFlight;

  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0", "line 1", "line 2"],
    "the late poll must not cut the transcript back to its partial tape");
  assert.equal(h.state.activity.live, false);
  assert.equal(h.pending(), 0);
});

test("leaving the activity tab and closing the panel both stop the poll", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, livePayload([tapeEvent(0)]), '"a"'],
  ]);

  await h.state.fetchActivity("commit", 0);
  assert.equal(h.pending(), 1);

  h.state.switchTab("ticket");
  assert.equal(h.pending(), 0, "a hidden pane is not worth a request every two seconds");

  h.state.activity = null;
  h.state.switchTab("activity");
  await flushMicrotasks();
  assert.equal(h.pending(), 1);

  h.state.closeDetail();
  assert.equal(h.pending(), 0);
  assert.equal(h.state.activity, null);
});

test("a ticket that leaves in_progress reads the finished transcript once", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, { ...livePayload([tapeEvent(0), tapeEvent(1)]), live: false }, '"done"'],
  ]);
  h.state.tickets = [RUNNING_TICKET];

  await h.state.fetchActivity("commit", 0);
  assert.equal(h.pending(), 1);

  h.state.applyTicketUpdate({ ...RUNNING_TICKET, status: "human_review" });
  await flushMicrotasks();

  assert.deepEqual(h.state.tapeEvents().map((e) => e.text), ["line 0", "line 1"]);
  assert.equal(h.state.activity.live, false);
  assert.equal(h.pending(), 0, "the run has ended, so nothing is scheduled");
});

test("switchTab('session') falls back to activity on a finished ticket", () => {
  const asked = [];
  const state = pageState(TAPE_TICKET, { fetchActivity: (stage, run) => asked.push([stage, run]) });
  state.flushEditSave = () => {};

  state.switchTab("session");

  assert.equal(state.activeTab, "activity");
  assert.deepEqual(asked, [["review", 1]]);
});

test("a stage finishing under the live session loads its transcript", () => {
  const asked = [];
  const state = pageState({ ...TAPE_TICKET, status: "in_progress", stage: "review" }, {
    fetchActivity: (stage, run) => asked.push([stage, run]),
  });
  state.activeTab = "session";
  state.tickets = [state.selectedTicket];
  state.recomputeBoard = () => {};
  state.closeTerminal = () => {};

  // The SSE update that ends the run switches the column to the transcript.
  // Nothing else fetches it, so the pane would otherwise read "no completed
  // stage yet" for a run that just produced one.
  state.applyTicketUpdate({ ...TAPE_TICKET, status: "human_review" });

  assert.equal(state.activeTab, "activity");
  assert.deepEqual(asked, [["review", 1]]);
});

test("the churn block bounds a large change set", () => {
  const files = [];
  for (let i = 0; i < 70; i++) files.push({ path: `pkg/f${i}.go`, added: i, deleted: 0 });
  const state = pageState(TAPE_TICKET, { ticketChanges: { base: "main", files, commits: [] } });

  const c = state.churn();
  assert.equal(c.count, 70);
  assert.equal(c.added, files.reduce((n, f) => n + f.added, 0));
  assert.deepEqual(c.top.map((f) => f.path), ["pkg/f69.go", "pkg/f68.go", "pkg/f67.go"]);
  assert.equal(c.more, 67);
});

test("files with equal churn are ordered by path", () => {
  const state = pageState(TAPE_TICKET, {
    ticketChanges: {
      base: "main", commits: [],
      files: [
        { path: "z.go", added: 5, deleted: 5 },
        { path: "a.go", added: 5, deleted: 5 },
        { path: "m.go", added: 5, deleted: 5 },
        { path: "b.go", added: 1, deleted: 0 },
      ],
    },
  });

  const first = state.churn().top.map((f) => f.path);
  assert.deepEqual(first, ["a.go", "m.go", "z.go"]);
  assert.deepEqual(state.churn().top.map((f) => f.path), first, "repeated renders agree");
});

// The relation refs the daemon resolves for the rail: an epic above, two
// blockers of which one is gone from the tickets dir, one dependent, and more
// links than the rail shows at once.
const RELATION_TICKET = {
  id: "kon-r1",
  status: "todo",
  parent: { id: "kon-epic", title: "The epic", status: "open" },
  deps: [
    { id: "kon-blk1", title: "Archived blocker", status: "archived" },
    { id: "kon-gone" },
  ],
  blocks: [{ id: "kon-wait", title: "Waiting on this", status: "open" }],
  links: Array.from({ length: 10 }, (_, i) => ({ id: `kon-l${i}`, title: `Related ${i}`, status: "done" })),
};

test("the rail lists the frontmatter relations and drops the rows with nothing in them", () => {
  const cases = [
    {
      name: "every relation set",
      ticket: RELATION_TICKET,
      want: ["parent", "deps", "blocks", "links"],
    },
    {
      name: "one row only",
      ticket: { id: "kon-r2", deps: [], links: [{ id: "kon-l0", status: "done" }] },
      want: ["links"],
    },
    { name: "no relations", ticket: { id: "kon-r3" }, want: [] },
    { name: "no ticket open", ticket: null, want: [] },
  ];

  for (const c of cases) {
    const state = pageState(c.ticket);
    // Spread, because the component builds the row list inside the VM realm and
    // the strict assert compares prototypes.
    assert.deepEqual([...state.relationRows().map((r) => r.key)], c.want, c.name);
  }
});

test("a relation row shows the first eight refs until it is expanded", () => {
  const state = pageState(RELATION_TICKET);
  const links = state.relationRows().find((r) => r.key === "links");

  assert.equal(state.relationRefs(links).length, 8);
  assert.equal(state.relationHidden(links), 2);

  state.relExpanded[links.key] = true;
  assert.equal(state.relationRefs(links).length, 10);
  assert.equal(state.relationHidden(links), 0);

  // A row inside the cap never offers the reveal.
  const deps = state.relationRows().find((r) => r.key === "deps");
  assert.equal(state.relationHidden(deps), 0);
});

test("a relation chip wears the status of the ticket it names, and says when there is none", () => {
  const state = pageState(RELATION_TICKET);
  const [blocker, gone] = state.relationRows().find((r) => r.key === "deps").refs;

  assert.equal(state.relationChipClass(blocker), "ent ent-ticket text-surface-600");
  assert.deepEqual({ ...state.ticketTip(blocker) }, {
    tag: "",
    tagColor: "none",
    title: "Archived blocker",
    body: "archived",
    hint: "click to open",
  });

  // The frontmatter still points at kon-gone, so it stays on screen. The daemon
  // found no ticket for it, so it is not a link.
  assert.equal(state.relationChipClass(gone), "ent ent-ticket text-surface-600 ent-ticket-gone");
  assert.deepEqual({ ...state.ticketTip(gone) }, {
    tag: "",
    tagColor: "none",
    title: "kon-gone",
    body: "not in the tickets dir",
    hint: "",
  });

  // A running dependent carries the colour the board column uses.
  const running = { id: "kon-run", title: "Running", status: "in_progress" };
  assert.equal(state.relationChipClass(running), "ent ent-ticket text-st-progress");
  assert.equal(state.ticketTip(running).body, "running");
});

test("the hover card peels the [tag] off the title and hands over its hue", () => {
  const state = pageState(RELATION_TICKET);

  // The hue is hashed off the bare tag, the string the board card and the
  // palette row hash, so one ticket wears one colour everywhere.
  const tagged = state.ticketTip({ id: "kon-t1", title: "[acta] Swift port", status: "open" });
  assert.equal(tagged.tag, "[acta]");
  assert.equal(tagged.title, "Swift port");
  assert.equal(tagged.tagColor, state.pipelineColorByName("acta"));
  assert.notEqual(tagged.tagColor, state.pipelineColorByName("[acta]"));

  // No prefix, no tag. A ref carries no path, so there is no basename to stand
  // in the way parseTitleTag lets one stand in for a card.
  const bare = state.ticketTip({ id: "kon-t2", title: "Swift port", status: "open" });
  assert.equal(bare.tag, "");
  assert.equal(bare.title, "Swift port");
  assert.equal(bare.tagColor, "none");

  // A title that is a tag and nothing else leaves no title, and setupTip drops
  // the whole card on an empty one, status word and click hint included.
  const only = state.ticketTip({ id: "kon-t3", title: "[acta]", status: "open" });
  assert.equal(only.tag, "[acta]");
  assert.equal(only.title, "kon-t3");
});


// The design's graph, as the endpoint hands it over: roots to goal, depth
// never going backwards, one node holding the chain.
const CHAIN_PAYLOAD = {
  id: "pca-d3q9",
  verdict: "blocked",
  position: 3,
  path_length: 4,
  goal: "pca-uvby",
  total: 6,
  done: 2,
  cycle: [],
  nodes: [
    { id: "pca-itf5", title: "Runtime", status: "done", depth: 0, direction: "upstream", on_critical_path: false, resolved: true, holds_chain: false, waits_on: { open: 0, total: 0 }, missing: false },
    { id: "pca-csyi", title: "Schema", status: "done", depth: 0, direction: "upstream", on_critical_path: false, resolved: true, holds_chain: false, waits_on: { open: 0, total: 0 }, missing: false },
    { id: "pca-qv0y", title: "[pca] Permissions", status: "paused", depth: 0, direction: "upstream", on_critical_path: true, resolved: false, holds_chain: true, waits_on: { open: 0, total: 0 }, missing: false },
    { id: "pca-u8ax", title: "Protocol", status: "open", depth: 1, direction: "upstream", on_critical_path: true, resolved: false, holds_chain: false, waits_on: { open: 1, total: 3 }, missing: false },
    { id: "pca-d3q9", title: "The open ticket", status: "todo", depth: 2, direction: "self", on_critical_path: true, resolved: false, holds_chain: false, waits_on: { open: 1, total: 1 }, missing: false },
    { id: "pca-uvby", title: "The goal", status: "open", depth: 3, direction: "downstream", on_critical_path: true, resolved: false, holds_chain: false, waits_on: { open: 1, total: 1 }, missing: false },
  ],
};

const chainState = (chain, ticket = { id: "pca-d3q9", status: "todo" }) => pageState(ticket, { chain });

test("the ladder rows step by depth and close each run at the elbow", () => {
  const rows = vmValue(chainState(CHAIN_PAYLOAD).chainRows());

  assert.deepEqual(rows.map((r) => r.id), ["pca-itf5", "pca-csyi", "pca-qv0y", "pca-u8ax", "pca-d3q9", "pca-uvby"]);
  // One 16px step per depth layer, which is what draws the staircase.
  assert.deepEqual(rows.map((r) => r.indent), [0, 0, 0, 16, 32, 48]);
  // The run is the block sharing a depth: only its final row stops the stem,
  // because the next depth's gutter is a step to the right.
  assert.deepEqual(rows.map((r) => r.last), [false, false, true, true, true, true]);
  assert.deepEqual(rows.map((r) => r.is_self), [false, false, false, false, true, false]);
  // The meta cell names what the row itself is waiting for, counting deps the
  // chain does not carry. A node with no deps says nothing at all.
  assert.deepEqual(rows.map((r) => r.meta), ["", "", "", "waits on 1 of 3", "waits on 1 of 1", "waits on 1 of 1"]);
});

test("a ladder row for a ticket that is gone is not painted as a link", () => {
  const state = chainState(CHAIN_PAYLOAD);
  const cases = [
    { name: "the open ticket", row: { is_self: true }, want: "bg-accent/[.09]" },
    { name: "a ticket the click opens", row: { missing: false }, want: "cursor-pointer hover:bg-surface-850/40" },
    { name: "an id with no ticket file", row: { missing: true }, want: "ent-ticket-gone" },
  ];
  for (const c of cases) assert.equal(state.chainRowClass(c.row), c.want, c.name);
});

test("the ladder meta cell reads the node's own deps", () => {
  const cases = [
    { name: "no deps at all", node: { waits_on: { open: 0, total: 0 } }, want: "" },
    { name: "some deps open", node: { waits_on: { open: 2, total: 5 } }, want: "waits on 2 of 5" },
    { name: "every dep closed", node: { waits_on: { open: 0, total: 3 } }, want: "deps clear" },
    { name: "no ticket file", node: { missing: true, waits_on: { open: 0, total: 0 } }, want: "not in the tickets dir" },
  ];
  const state = chainState(CHAIN_PAYLOAD);
  for (const c of cases) assert.equal(state._chainMeta(c.node), c.want, c.name);
});

test("the chain summary says the size, the place and the state in one voice", () => {
  const cases = [
    {
      name: "blocked mid-chain",
      chain: CHAIN_PAYLOAD,
      ticket: { id: "pca-d3q9", status: "todo" },
      want: { text: "6 tickets · 2 done", place: "3rd of 4", state: "waiting on pca-qv0y", releases: "finishing this unblocks pca-uvby" },
    },
    {
      name: "nothing blocking, and something downstream to release",
      chain: {
        ...CHAIN_PAYLOAD, verdict: "ready", position: 1, path_length: 2, goal: "pca-uvby", total: 2, done: 0,
        nodes: [
          { id: "pca-d3q9", title: "Self", status: "todo", depth: 0, direction: "self", on_critical_path: true, resolved: false, holds_chain: true, waits_on: { open: 0, total: 0 } },
          { id: "pca-uvby", title: "Goal", status: "open", depth: 1, direction: "downstream", on_critical_path: true, resolved: false, holds_chain: false, waits_on: { open: 1, total: 1 } },
        ],
      },
      ticket: { id: "pca-d3q9", status: "todo" },
      want: { text: "2 tickets · 0 done", place: "1st of 2", state: "nothing blocking", releases: "finishing this unblocks pca-uvby" },
    },
    {
      name: "the ticket is done, so the copy is retrospective",
      chain: {
        ...CHAIN_PAYLOAD, verdict: "ready", position: 1, path_length: 2, goal: "pca-uvby", total: 2, done: 2,
        nodes: [
          { id: "pca-d3q9", title: "Self", status: "done", depth: 0, direction: "self", on_critical_path: true, resolved: true, holds_chain: false, waits_on: { open: 0, total: 0 } },
          { id: "pca-uvby", title: "Goal", status: "cancelled", depth: 1, direction: "downstream", on_critical_path: true, resolved: true, holds_chain: false, waits_on: { open: 0, total: 1 } },
        ],
      },
      ticket: { id: "pca-d3q9", status: "done" },
      want: { text: "2 tickets · 2 done", place: "1st of 2", state: "chain cleared", releases: "finishing this unblocked pca-uvby" },
    },
    {
      name: "a downstream holder is not blocking the ticket, it is waiting on it",
      chain: {
        ...CHAIN_PAYLOAD, verdict: "ready", position: 1, path_length: 2, goal: "pca-uvby", total: 2, done: 1,
        nodes: [
          { id: "pca-d3q9", title: "Self", status: "done", depth: 0, direction: "self", on_critical_path: true, resolved: true, holds_chain: false, waits_on: { open: 0, total: 0 } },
          { id: "pca-uvby", title: "Goal", status: "open", depth: 1, direction: "downstream", on_critical_path: true, resolved: false, holds_chain: true, waits_on: { open: 0, total: 1 } },
        ],
      },
      ticket: { id: "pca-d3q9", status: "done" },
      want: { text: "2 tickets · 1 done", place: "1st of 2", state: "nothing blocking", releases: "finishing this unblocked pca-uvby" },
    },
    {
      name: "a cycle has no place and no ladder",
      chain: { id: "pca-d3q9", verdict: "cycle", position: 0, path_length: 0, goal: "", total: 0, done: 0, cycle: ["pca-d3q9", "pca-u8ax", "pca-d3q9"], nodes: [] },
      ticket: { id: "pca-d3q9", status: "todo" },
      want: { text: "0 tickets · 0 done", place: "", state: "dependency cycle", releases: "" },
    },
    {
      name: "a lone ticket counts in the singular",
      chain: { id: "solo", verdict: "ready", position: 1, path_length: 1, goal: "solo", total: 1, done: 0, cycle: [], nodes: [{ id: "solo", title: "Solo", status: "todo", depth: 0, direction: "self", on_critical_path: true, resolved: false, holds_chain: true, waits_on: { open: 0, total: 0 } }] },
      ticket: { id: "solo", status: "todo" },
      want: { text: "1 ticket · 0 done", place: "1st of 1", state: "nothing blocking", releases: "" },
    },
  ];

  for (const c of cases) {
    const got = chainState(c.chain, c.ticket).chainSummary();
    assert.deepEqual({ text: got.text, place: got.place, state: got.state, releases: got.releases }, c.want, c.name);
  }
});

test("the chain lookups find the holder, the goal and the direct dependents", () => {
  const state = chainState(CHAIN_PAYLOAD, { id: "pca-d3q9", status: "todo", blocks: [{ id: "pca-uvby", status: "open" }] });

  assert.equal(state.chainVerdict(), "blocked");
  assert.equal(state.chainHolder().id, "pca-qv0y");
  assert.equal(state.chainGoal().id, "pca-uvby");
  // unblocks is the frontmatter edge, read off the chain for its status.
  assert.deepEqual(vmValue(state.chainUnblocks()).map((n) => n.id), ["pca-uvby"]);
  // One segment per node, in the ladder's own order and hue.
  assert.deepEqual(vmValue(state.chainMeter()).map((s) => s.cls), [
    "text-st-done bg-current", "text-st-done bg-current", "text-st-paused bg-current",
    "text-st-open bg-current", "text-surface-600 bg-current", "text-st-open bg-current",
  ]);
});

test("the resume pill only offers a move the daemon would take", () => {
  const cases = [
    { name: "paused holder resumes", status: "paused", want: { id: "pca-qv0y", endpoint: "retry", label: "resume" } },
    { name: "open holder queues", status: "open", want: { id: "pca-qv0y", endpoint: "run", label: "queue" } },
    { name: "a running holder needs no push", status: "in_progress", want: null },
    { name: "a queued holder needs no push", status: "todo", want: null },
    { name: "a holder in review is not the pill's business", status: "human_review", want: null },
  ];

  for (const c of cases) {
    const chain = { ...CHAIN_PAYLOAD, nodes: CHAIN_PAYLOAD.nodes.map((n) => (n.holds_chain ? { ...n, status: c.status } : n)) };
    assert.deepEqual(vmValue(chainState(chain).chainResumeMove()), c.want, c.name);
  }

  // No holder, and a holder with no ticket file, both offer nothing.
  assert.equal(chainState({ ...CHAIN_PAYLOAD, nodes: CHAIN_PAYLOAD.nodes.map((n) => ({ ...n, holds_chain: false })) }).chainResumeMove(), null);
  const gone = { ...CHAIN_PAYLOAD, nodes: CHAIN_PAYLOAD.nodes.map((n) => (n.holds_chain ? { ...n, status: "", missing: true } : n)) };
  assert.equal(chainState(gone).chainResumeMove(), null);
});

test("an SSE batch refetches the chain when it names a node or waits on one", () => {
  const cases = [
    { name: "a node moved", pending: [{ id: "pca-qv0y", status: "done" }], want: true },
    { name: "the open ticket itself moved", pending: [{ id: "pca-d3q9", status: "in_progress" }], want: true },
    { name: "a new dependent named a node", pending: [{ id: "pca-new", status: "open", deps: [{ id: "pca-d3q9", status: "todo" }] }], want: true },
    { name: "an unrelated ticket moved", pending: [{ id: "kon-other", status: "done", deps: [{ id: "kon-x" }] }], want: false },
  ];

  for (const c of cases) {
    assert.equal(chainState(CHAIN_PAYLOAD)._chainTouchedBy(c.pending), c.want, c.name);
  }
  // With no chain loaded there is nothing to re-derive.
  assert.equal(pageState({ id: "pca-d3q9" })._chainTouchedBy([{ id: "pca-d3q9" }]), false);
});

test("deleting a node of the open chain refetches the ladder", async () => {
  const listeners = {};
  let fetched = 0;
  const state = loadKontoraState({
    EventSource: class {
      addEventListener(name, fn) { listeners[name] = fn; }
      close() {}
    },
    fetch: async () => { fetched++; return { ok: true, json: async () => CHAIN_PAYLOAD }; },
  });
  state.selectedTicket = { id: "pca-d3q9", status: "todo" };
  state.chain = CHAIN_PAYLOAD;
  state.connectSSE();

  listeners.ticket_deleted({ data: JSON.stringify({ id: "kon-other" }) });
  await flushMicrotasks();
  assert.equal(fetched, 0, "a ticket outside the chain leaves the ladder alone");

  // The ladder would otherwise keep drawing the gone node, holder and resume
  // pill included.
  listeners.ticket_deleted({ data: JSON.stringify({ id: "pca-uvby" }) });
  await flushMicrotasks();
  assert.equal(fetched, 1);
});

test("the chain fetch seeds the ladder collapsed only when it has one line to say", async () => {
  const cases = [
    { name: "a blocked six-node chain opens on the ladder", chain: CHAIN_PAYLOAD, want: false },
    { name: "a ready chain opens shut", chain: { ...CHAIN_PAYLOAD, verdict: "ready" }, want: true },
    { name: "a two-node chain opens shut", chain: { ...CHAIN_PAYLOAD, total: 2 }, want: true },
  ];

  for (const c of cases) {
    const state = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => c.chain }) });
    state.selectedTicket = { id: "pca-d3q9", status: "todo" };
    await state.fetchChain("pca-d3q9", true);
    assert.equal(state.chainCollapsed, c.want, c.name);
    assert.equal(state.chainError, null, c.name);
  }

  // Only the first fetch for a ticket seeds it: an SSE refetch must not undo
  // the user's own expand or collapse.
  const kept = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => ({ ...CHAIN_PAYLOAD, verdict: "ready" }) }) });
  kept.selectedTicket = { id: "pca-d3q9", status: "todo" };
  await kept.fetchChain("pca-d3q9", true);
  assert.equal(kept.chainCollapsed, true);
  kept.chainCollapsed = false;
  await kept.fetchChain("pca-d3q9");
  assert.equal(kept.chainCollapsed, false, "a refetch leaves the ladder as the user left it");

  // A response for a ticket the user has already navigated away from is dropped.
  const stale = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => CHAIN_PAYLOAD }) });
  stale.selectedTicket = { id: "pca-other", status: "todo" };
  await stale.fetchChain("pca-d3q9");
  assert.equal(stale.chain, null);

  // Two fetches for the same ticket in flight: the older answer is dropped
  // whatever order they resolve in, so the ladder ends on the newer graph.
  const raced = loadKontoraState({
    fetch: async (url) => {
      const nth = raced._nth = (raced._nth || 0) + 1;
      if (nth === 1) await new Promise((r) => setTimeout(r, 10));
      return { ok: true, json: async () => ({ ...CHAIN_PAYLOAD, total: nth }) };
    },
  });
  raced.selectedTicket = { id: "pca-d3q9", status: "todo" };
  const first = raced.fetchChain("pca-d3q9");
  const second = raced.fetchChain("pca-d3q9");
  await Promise.all([first, second]);
  assert.equal(raced.chain.total, 2, "the newer fetch wins");

  const failed = loadKontoraState({ fetch: async () => ({ ok: false, json: async () => ({}) }) });
  failed.selectedTicket = { id: "pca-d3q9", status: "todo" };
  await failed.fetchChain("pca-d3q9");
  assert.equal(failed.chain, null);
  assert.equal(failed.chainError, "Failed to load the chain");
});

test("a relation dot fills with the status hue the chip's text uses", () => {
  const state = pageState(RELATION_TICKET);

  assert.equal(state.relationDotClass({ id: "kon-run", status: "in_progress" }), "text-st-progress bg-current");
  assert.equal(state.relationDotClass({ id: "kon-fin", status: "done" }), "text-st-done bg-current");
  // A status the palette does not mark, and a ref with none at all, both fall
  // back to the dim mono colour the gone chip already wears.
  assert.equal(state.relationDotClass({ id: "kon-odd", status: "needs_qa" }), "text-surface-600 bg-current");
  assert.equal(state.relationDotClass(null), "text-surface-600 bg-current");
});

// An epic with more sub-tickets than the tree draws at once: three finished,
// one running mid-pipeline, one never picked up.
const CHILDREN_TICKET = {
  id: "kon-epic",
  status: "open",
  children: [
    { id: "kon-child-1", title: "[kontora] First", status: "done", stage: "commit", stage_index: 4, stage_count: 4,
      started_at: "2026-08-12T09:00:00Z", completed_at: "2026-08-12T09:41:00Z" },
    { id: "kon-child-2", title: "Second", status: "done", stage: "commit",
      started_at: "2026-08-12T09:00:00Z", completed_at: "2026-08-12T10:12:00Z" },
    { id: "kon-child-3", title: "Third", status: "done", stage: "commit" },
    { id: "kon-child-4", title: "Fourth", status: "in_progress", stage: "implement", stage_index: 2, stage_count: 4,
      started_at: "2026-08-12T09:30:00Z" },
    { id: "kon-child-5", title: "Fifth", status: "todo" },
  ],
};

test("the sub-ticket tree caps its rows and marks the last one it draws", () => {
  const many = Array.from({ length: 15 }, (_, i) => ({ id: `kon-c${i}`, title: `Child ${i}`, status: "todo" }));
  const state = pageState({ id: "kon-epic", children: many });

  var rows = vmValue(state.childRows());
  assert.equal(rows.length, 12);
  assert.equal(state.childrenHidden(), 3);
  // `last` is the last row drawn, not the last child, so a hidden tail does not
  // leave the connector stem running into nothing.
  assert.equal(rows[11].id, "kon-c11");
  assert.deepEqual(rows.filter((c) => c.last).map((c) => c.id), ["kon-c11"]);

  state.childrenExpanded = true;
  rows = vmValue(state.childRows());
  assert.equal(rows.length, 15);
  assert.equal(state.childrenHidden(), 0);
  assert.deepEqual(rows.filter((c) => c.last).map((c) => c.id), ["kon-c14"]);

  // No children, no rows, and nothing to reveal.
  const empty = pageState({ id: "kon-r3" });
  assert.deepEqual(vmValue(empty.childRows()), []);
  assert.equal(empty.childrenHidden(), 0);
});

test("the rollup counts every child, not only the ones on screen", () => {
  const state = pageState(CHILDREN_TICKET);
  assert.deepEqual(vmValue(state.childRollup()), { done: 3, total: 5, pct: 60 });

  // The cap hides rows, never children: the count is the same either way.
  const many = Array.from({ length: 15 }, (_, i) => ({ id: `kon-c${i}`, status: i < 5 ? "done" : "todo" }));
  assert.deepEqual(vmValue(pageState({ id: "kon-e2", children: many }).childRollup()), { done: 5, total: 15, pct: 33 });

  assert.deepEqual(vmValue(pageState({ id: "kon-e3" }).childRollup()), { done: 0, total: 0, pct: 0 });
});

test("a child reports its stage position while it runs, and the stage word otherwise", () => {
  const state = pageState(CHILDREN_TICKET);
  const [first, , , running, queued] = CHILDREN_TICKET.children;

  assert.equal(state.childStageLine(running), "implement 2/4");
  // Finished: the position would read as progress it no longer makes.
  assert.equal(state.childStageLine(first), "commit");
  // Nothing has run it yet, so there is no stage to name.
  assert.equal(state.childStageLine(queued), "—");
  // A single-stage pipeline has no position worth printing.
  assert.equal(state.childStageLine({ id: "kon-c", status: "in_progress", stage: "implement", stage_index: 1, stage_count: 1 }), "implement");
});

test("a finished child prints the wall the daemon bounded, a running one is clocked live", () => {
  const state = pageState(CHILDREN_TICKET);
  const [first, second, noStamps, running, queued] = CHILDREN_TICKET.children;

  assert.equal(state.childElapsed(first), "41m");
  assert.equal(state.childElapsed(second), "1h 12m");
  // Still running: the column reads off the reactive `now`, which the 30s tick
  // advances, so a running child's elapsed climbs without a refetch.
  state.now = new Date("2026-08-12T10:11:00Z");
  assert.equal(state.childElapsed(running), "41m");
  state.now = new Date("2026-08-12T10:41:00Z");
  assert.equal(state.childElapsed(running), "1h 11m");

  assert.equal(state.childElapsed(noStamps), "—");
  assert.equal(state.childElapsed(queued), "—");

  // A child that is not running gets no live clock, whatever it is missing: it
  // has no run left to make, so a climbing number would be a lie.
  assert.equal(state.childElapsed({ id: "kon-c", status: "done", started_at: "2026-08-12T09:00:00Z" }), "—");
});

test("a child finishing patches its row in the open epic without a refetch", () => {
  const state = pageState(CHILDREN_TICKET);
  state.tickets = [];
  let refetched = 0;
  state.selectTicket = () => { refetched++; };

  // The event the daemon broadcasts for the child itself: its pipeline, its
  // history, and the parent that names the open epic.
  state.applyTicketUpdate({
    id: "kon-child-4", title: "Fourth", status: "done", stage: "commit",
    stages: ["plan", "implement", "review", "commit"],
    started_at: "2026-08-12T10:00:00Z",
    history: [
      { stage: "plan", started_at: "2026-08-12T09:30:00Z", completed_at: "2026-08-12T09:50:00Z" },
      { stage: "commit", started_at: "2026-08-12T10:00:00Z", completed_at: "2026-08-12T10:05:00Z" },
    ],
    parent: { id: "kon-epic", title: "The epic", status: "open" },
  });

  const patched = vmValue(state.selectedTicket.children).find((c) => c.id === "kon-child-4");
  assert.equal(patched.status, "done");
  assert.equal(patched.stage, "commit");
  // The position moves with the stage, so the row cannot read "commit 2/4".
  assert.equal(state.childStageLine(patched), "commit");
  assert.equal(patched.stage_index, 4);
  assert.equal(patched.stage_count, 4);
  // The wall is rebounded off the history: first pickup to last exit, not the
  // frontmatter's started_at, and no longer a live clock that climbs forever.
  assert.equal(patched.started_at, "2026-08-12T09:30:00Z");
  assert.equal(patched.completed_at, "2026-08-12T10:05:00Z");
  assert.equal(state.childElapsed(patched), "35m");
  assert.deepEqual(vmValue(state.childRollup()), { done: 4, total: 5, pct: 80 });
  assert.equal(refetched, 0);

  // A child picked up rather than finished: it keeps its live clock, and the
  // position follows the stage it is on now.
  state.applyTicketUpdate({
    id: "kon-child-5", title: "Fifth", status: "in_progress", stage: "implement",
    stages: ["plan", "implement", "review", "commit"],
    started_at: "2026-08-12T10:00:00Z",
    parent: { id: "kon-epic", status: "open" },
  });
  const running = vmValue(state.selectedTicket.children).find((c) => c.id === "kon-child-5");
  assert.equal(state.childStageLine(running), "implement 2/4");
  assert.equal(running.completed_at, null);
  state.now = new Date("2026-08-12T10:41:00Z");
  assert.equal(state.childElapsed(running), "41m");

  // Marked done by hand, so there is no history row to end on: the event has no
  // completed_at of its own and the file's mtime stands in.
  state.applyTicketUpdate({
    id: "kon-child-5", title: "Fifth", status: "done", stage: "implement",
    stages: ["plan", "implement", "review", "commit"],
    started_at: "2026-08-12T10:00:00Z", updated_at: "2026-08-12T10:20:00Z",
    parent: { id: "kon-epic", status: "open" },
  });
  const byHand = vmValue(state.selectedTicket.children).find((c) => c.id === "kon-child-5");
  assert.equal(state.childElapsed(byHand), "20m");

  // An update for a ticket under some other epic leaves this tree alone.
  state.applyTicketUpdate({
    id: "kon-child-3", status: "cancelled", stage: "commit",
    parent: { id: "kon-other", status: "open" },
  });
  assert.deepEqual(vmValue(state.childRollup()), { done: 5, total: 5, pct: 100 });
});

test("a relation opens through the board entry when the board has one", async () => {
  const state = pageState(RELATION_TICKET);
  const boardEntry = { id: "kon-wait", title: "Waiting on this", status: "open", pipeline: "default" };
  state.tickets = [boardEntry];
  let opened;
  state._paletteOpenTicket = (t) => { opened = t; };

  // The board entry is the object the card list holds, so the panel and the
  // selected card agree.
  await state.openTicketRef({ id: "kon-wait", title: "Waiting on this", status: "open" });
  assert.equal(opened, boardEntry);

  // A ticket the board hides is opened from the ref, and the detail fetch fills
  // the rest in.
  await state.openTicketRef({ id: "kon-blk1", title: "Archived blocker", status: "archived" });
  assert.deepEqual({ ...opened }, { id: "kon-blk1", title: "Archived blocker", status: "archived" });

  // Nothing to open behind an unresolved ref.
  opened = null;
  await state.openTicketRef({ id: "kon-gone" });
  assert.equal(opened, null);
});

test("index.html renders the relation rows inside the frontmatter grid", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // One row per relation, spanning both grid columns so the label stays on the
  // rail's 58px column.
  assert.match(html, /x-for="row in relationRows\(\)" :key="row\.key"/);
  assert.match(html, /class="col-span-2 grid grid-cols-\[58px_1fr\]/);
  // The chip carries the same hover card the summary chips use, and a click
  // opens the ticket it names.
  assert.match(html, /:class="relationChipClass\(ref\)"/);
  assert.match(html, /:data-tip-e="ticketTip\(ref\)\.title"/);
  assert.match(html, /:data-tip-e-tag="ticketTip\(ref\)\.tag"/);
  assert.match(html, /:data-tip-e-tag-color="ticketTip\(ref\)\.tagColor"/);
  assert.match(html, /@click="openTicketRef\(ref\)"/);
  assert.match(html, /@click="relExpanded\[row\.key\] = true"/);
  // Notes moved to their own tab: the rail carries relations and details only.
  assert.equal(html.includes("Notes: read back from the body"), false);
  // A gone relation is dashed and not clickable.
  assert.match(html, /\.ent-ticket-gone \{ border-style: dashed;/);
});

test("index.html renders the parent crumb, the relations strip and the sub-ticket tree", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The crumb sits between board and the open id, and only when there is a
  // parent to sit there.
  assert.match(html, /<template x-if="selectedTicket\?\.parent">/);
  assert.match(html, /@click="openTicketRef\(selectedTicket\.parent\)"/);
  assert.match(html, /:class="relationDotClass\(selectedTicket\.parent\)"/);
  // All five, because setupTip mirrors them on every hover and an omitted one
  // leaks the previous chip's tag or hue.
  for (const attr of ["", "-tag", "-tag-color", "-body", "-hint"]) {
    assert.match(html, new RegExp(`:data-tip-e${attr}="ticketTip\\(selectedTicket\\.parent\\)\\.`));
    assert.match(html, new RegExp(`:data-tip-e${attr}="ticketTip\\(c\\)\\.`));
  }


  // The ladder replaces the strip: it is drawn for a chain with something to
  // say and for a cycle, and not at all for a lone ticket.
  assert.match(html, /<template x-if="chain && \(chainVerdict\(\) === 'cycle' \|\| chainSummary\(\)\.total > 1\)">/);
  assert.match(html, /x-for="row in chainRows\(\)" :key="row\.id"/);
  assert.match(html, /@click="openTicketRef\(row\)"/);
  assert.match(html, /@click="chainCollapsed = !chainCollapsed"/);
  // The staircase, the borrowed 26px gutter, and the stem stopping at the
  // elbow on the last row of a run.
  assert.match(html, /:style="'width:' \+ row\.indent \+ 'px'"/);
  assert.match(html, /<div class="w-\[26px\] shrink-0 relative">/);
  assert.match(html, /:class="row\.last \? 'h-\[20px\]' : 'bottom-0'"/);
  // The meta cell is fixed at 178px and must not shrink: it holds the longest
  // meta plus the resume pill.
  assert.match(html, /grid-cols-\[1fr_178px_94px_96px\]/);
  // The pill is re-derived at click time rather than read off the rendered row.
  assert.match(html, /x-show="row\.holds_chain && chainResumeMove\(\)"/);
  assert.match(html, /@click\.stop="resumeChainHolder\(\)"/);
  // The row class is derived, so a row for a gone ticket is not painted as a
  // link it cannot be.
  assert.match(html, /:class="chainRowClass\(row\)"/);
  // A chain that failed to load says so, in the header when the old one is
  // still on screen and on its own when there is nothing to draw.
  assert.match(html, /<template x-if="!chain && chainError">/);
  assert.match(html, /x-show="chainError" class="font-mono text-\[10\.5px\] text-warn shrink-0"/);
  // A cycle draws the banner instead of the ladder.
  assert.match(html, /<template x-if="chainVerdict\(\) === 'cycle'">/);
  // Every row carries all five, or the hover card leaks the previous one's hue.
  for (const attr of ["", "-tag", "-tag-color", "-body", "-hint"]) {
    assert.match(html, new RegExp(`:data-tip-e${attr}="ticketTip\\(row\\)\\.`));
  }
  // The rail's three derived rows sit under id, and its relation rows are left
  // as they were.
  assert.match(html, /<template x-if="chainSummary\(\)\.total > 1 && chainSummary\(\)\.place">/);
  assert.match(html, /<span class="text-surface-600">chain<\/span>/);
  // Only an upstream holder is blocking: a downstream one is waiting on this
  // ticket, and naming it "blocked by" would point the row the wrong way.
  assert.match(html, /<template x-if="chainHolder\(\) && chainHolder\(\)\.direction === 'upstream'">/);
  assert.match(html, /<span class="text-surface-600">blocked by<\/span>/);
  assert.match(html, /<span class="text-surface-600">unblocks<\/span>/);
  assert.match(html, /x-for="row in relationRows\(\)" :key="row\.key"/);

  // The tree: no children, no block, so the body does not shift.
  assert.match(html, /<template x-if="\(selectedTicket\?\.children \|\| \[\]\)\.length > 0">/);
  assert.match(html, /x-for="c in childRows\(\)" :key="c\.id"/);
  assert.match(html, /@click="openTicketRef\(c\)"/);
  assert.match(html, /x-text="childRollup\(\)\.done \+ ' of ' \+ childRollup\(\)\.total"/);
  assert.match(html, /@click="childrenCollapsed = !childrenCollapsed"/);
  assert.match(html, /@click="childrenExpanded = true"/);
  // The stem stops at the elbow on the last drawn row.
  assert.match(html, /:class="c\.last \? 'h-\[20px\]' : 'bottom-0'"/);
});

test("index.html renders the ribbon, transcript and rail the page needs", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // flex-grow carries the duration; the floor and basis stay in Tailwind.
  assert.match(html, /:style="'flex-grow:' \+ seg\.seconds"/);
  assert.match(html, /min-w-\[136px\] basis-0/);
  // A 308px rail, and a churn block whose height does not follow the file count.
  assert.match(html, /w-\[308px\] border-l border-surface-700\/50/);
  assert.match(html, /class="flex flex-col gap-\[9px\] h-\[152px\] shrink-0"/);
  // One resolvable desktop terminal target.
  assert.equal((html.match(/id="terminal-session"/g) || []).length, 1);
  assert.equal(html.includes('id="terminal-container"'), false);
  // The sweep is declared next to the other loops and stops under reduced motion.
  assert.match(html, /@keyframes sweep \{/);
  assert.match(html, /@media \(prefers-reduced-motion: reduce\) \{[\s\S]{0,200}?\.sweep \{ animation: none; \}/);
});

// ---------------------------------------------------------------------------
// Ticket page: notes tab
// ---------------------------------------------------------------------------

const NOTES_TICKET = {
  id: "kon-1",
  status: "human_review",
  finished_at: "2026-08-08T11:30:00Z",
  history: [
    { stage: "plan", exit_code: 0, run: 0, started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:40Z" },
    { stage: "review", exit_code: 1, run: 1, started_at: "2026-08-08T11:01:00Z", completed_at: "2026-08-08T11:01:20Z" },
  ],
  notes: [
    { id: "q88f", at: "2026-08-08T11:02:00Z", author: "claude", author_kind: "agent", text: "done" },
    { id: "a1b2", at: "2026-08-08T11:03:00Z", author: "alexander", author_kind: "human", parent_id: "q88f", text: "ack" },
    { id: "n3x0", at: "2026-08-08T11:40:00Z", author: "kontora", author_kind: "system", text: "paused: hook failed", reactions: [{ emoji: "👍", actors: ["alexander"] }] },
  ],
};

test("the notes feed interleaves lifecycle rows, sorts ascending and nests replies", () => {
  const state = pageState(NOTES_TICKET, { configCache: { author: "alexander" } });
  const feed = state.notesFeed();

  assert.deepEqual(vmValue(feed.map((i) => i.key)), [
    "e:0:start",
    "e:0:end",
    "e:1:start",
    "e:1:end",
    "n:q88f",
    "e:review",
    "n:n3x0",
  ]);
  // A reply is drawn under its parent, not as a row of its own.
  assert.deepEqual(vmValue(feed.find((i) => i.key === "n:q88f").replies.map((r) => r.id)), ["a1b2"]);
  assert.equal(state.noteCount(), 2, "replies are excluded from the header count");

  const rows = feed.filter((i) => i.kind === "event");
  assert.deepEqual(vmValue(rows.map((r) => [r.label, r.color])), [
    ["picked up · stage plan", "indigo"],
    ["plan done", "green"],
    ["retried · stage review", "amber"],
    ["review failed", "rose"],
    ["moved to human review", "mauve"],
  ]);
  assert.equal(rows[1].meta, "exit 0 · 40s");
});

test("a ticket with no notes draws no lifecycle rows", () => {
  const state = pageState({ ...NOTES_TICKET, notes: [] });
  assert.deepEqual(vmValue(state.notesFeed()), []);
  assert.equal(state.noteLastAgo(), "nothing yet");
});

test("author kind picks the avatar hue, and only your own notes are yours", () => {
  const state = pageState(NOTES_TICKET, { configCache: { author: "alexander" } });
  const [agent, human, system] = NOTES_TICKET.notes;

  assert.equal(state.noteAuthorHue(agent), state.pipelineColorByName("claude"));
  assert.equal(state.noteAuthorHue(human), "mauve");
  assert.equal(state.noteAuthorHue(system), "none");
  // A note written before notes carried an author reads neutral and unnamed.
  assert.equal(state.noteAuthorHue({ id: "#0", text: "legacy" }), "none");
  assert.equal(state.noteRole(system), "system");
  assert.equal(state.noteRole(human), "", "a person carries no role label");

  assert.equal(state.noteIsMine(human), true);
  assert.equal(state.noteIsMine(agent), false);
  // Your reactions read violet, everyone else's grey.
  assert.equal(state.noteReactionHue({ emoji: "👍", actors: ["alexander"] }), "mauve");
  assert.equal(state.noteReactionHue({ emoji: "👍", actors: ["claude"] }), "none");
});

test("opening a reply box closes the edit box, and both clear with the ticket", () => {
  const state = pageState(NOTES_TICKET, { configCache: { author: "alexander" } });
  const note = NOTES_TICKET.notes[0];

  state.noteOpenEdit(note);
  assert.equal(state.noteEditOn, "q88f");
  assert.equal(state.noteEditDraft, "done");

  state.noteOpenReply(note);
  assert.equal(state.noteEditOn, null, "one box at a time");
  assert.equal(state.noteReplyOn, "q88f");

  state.noteReplyDraft = "half typed";
  state.noteResetDrafts();
  assert.deepEqual([state.noteReplyOn, state.noteReplyDraft, state.noteEditOn], [null, "", null]);
});

test("a note mutation replaces the ticket, and patches only the notes while editing", () => {
  const state = pageState(NOTES_TICKET, { tickets: [{ id: "kon-1" }], recomputeBoard: () => {} });
  const full = { ...NOTES_TICKET, title: "renamed", notes: [{ id: "z9z9", text: "new" }] };

  state._noteApplyTicket("kon-1", full);
  assert.equal(state.selectedTicket.title, "renamed");

  state.editing = true;
  state.selectedTicket = { ...NOTES_TICKET, title: "being typed" };
  state._noteApplyTicket("kon-1", full);
  assert.equal(state.selectedTicket.title, "being typed", "an edited field is not overwritten");
  assert.deepEqual(vmValue(state.selectedTicket.notes.map((n) => n.id)), ["z9z9"]);
});

test("the notes tab is always present and its composer is never gated on status", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tabBar = html.slice(html.indexOf("<!-- Tab bar -->"), html.indexOf("<!-- Content column -->"));

  // No x-show: an open ticket with no runs can still carry a note.
  assert.match(tabBar, /<button @click="switchTab\('notes'\)"[\s\S]{0,240}>notes<\/button>/);

  const pane = html.slice(html.indexOf("<!-- Notes: the ticket's conversation"), html.indexOf("<!-- Right rail."));
  // Header, scrolling feed, pinned composer.
  assert.match(pane, /class="h-full flex flex-col bg-surface-frame"/);
  assert.equal((pane.match(/max-w-\[820px\] mx-auto/g) || []).length, 3);
  assert.match(pane, /class="flex-1 min-h-0 overflow-y-auto px-6/);
  // The feed pins to the newest comment the way the activity scroller does.
  assert.match(pane, /\$el\.scrollTop = \$el\.scrollHeight/);
  // Every textarea swallows Escape, or the global chain closes the ticket page.
  assert.equal((pane.match(/<textarea/g) || []).length, 3);
  assert.equal((pane.match(/@keydown\.escape\.stop/g) || []).length, 3);
  // ⌘↵ submits; the composer never carries a status condition.
  assert.equal((pane.match(/\$event\.metaKey \|\| \$event\.ctrlKey/g) || []).length, 3);
  assert.equal(pane.includes("in_progress"), false);
});

test("deleting a note is confirmed in a modal that leads the ticket page on Escape", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /<div x-show="noteDeleteTarget"[\s\S]{0,400}@click\.self="noteCloseDelete\(\)"/);
  assert.match(html, /aria-labelledby="delete-note-title"/);
  assert.match(html, /:disabled="noteDeleteSubmitting"/);
  // Ahead of closeDetail in the one global Escape chain and behind the
  // file-delete modal, so Escape never closes the page out from under a dialog.
  const chain = html.slice(html.indexOf("@keydown.escape.window="), html.indexOf("@dragenter"));
  assert.ok(chain.indexOf("noteDeleteTarget") < chain.indexOf("closeDetail()"));
  assert.ok(chain.indexOf("deleteModal") < chain.indexOf("noteDeleteTarget"));
});

// ---------------------------------------------------------------------------
// Ticket page: summary tab
// ---------------------------------------------------------------------------

// A finished four-stage run, one summarised attempt per stage. The top-level
// summary repeats the last stage's text, which is what the daemon writes.
const SUMMARY_TICKET = {
  id: "kon-s1",
  status: "human_review",
  stage: "commit",
  branch: "kontora/kon-s1",
  summary: "Committed the redesign.",
  stages: ["implement", "review", "fix-review", "commit"],
  history: [
    { stage: "implement", agent: "claude", exit_code: 0, run: 0, summary: "Wrote the cards.", started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:10:41Z" },
    { stage: "review", agent: "codex", exit_code: 0, run: 0, summary: "Reviewed them.", started_at: "2026-08-08T11:11:00Z", completed_at: "2026-08-08T11:31:34Z" },
    { stage: "fix-review", agent: "claude", exit_code: 0, run: 0, summary: "Fixed the findings.", started_at: "2026-08-08T11:32:00Z", completed_at: "2026-08-08T12:22:04Z" },
    { stage: "commit", agent: "claude", exit_code: 0, run: 0, summary: "Committed the redesign.", started_at: "2026-08-08T12:23:00Z", completed_at: "2026-08-08T12:23:32Z" },
  ],
};

// The same review stage twice: 30m that failed, then 20m that passed.
const RETRY_TICKET = {
  id: "kon-s2",
  status: "human_review",
  stage: "review",
  summary: "",
  stages: ["implement", "review"],
  history: [
    { stage: "implement", agent: "claude", exit_code: 0, run: 0, summary: "Implemented.", started_at: "2026-08-08T10:00:00Z", completed_at: "2026-08-08T10:05:00Z" },
    { stage: "review", agent: "codex", exit_code: 1, run: 0, summary: "First pass found a bug.", started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:30:00Z" },
    { stage: "review", agent: "codex", exit_code: 0, run: 1, summary: "Second pass is clean.", started_at: "2026-08-08T11:40:00Z", completed_at: "2026-08-08T12:00:00Z" },
  ],
};

// The same two review runs, recorded before the run field existed. Every row
// on disk older than a209f5b looks like this.
const LEGACY_TICKET = {
  ...RETRY_TICKET,
  id: "kon-s4",
  history: RETRY_TICKET.history.map(({ run, ...rest }) => rest),
};

// The pipeline was repointed after the run, so review is no longer one of its
// stages and the ribbon has no segment for it.
const DROPPED_STAGE_TICKET = { ...RETRY_TICKET, id: "kon-s5", stages: ["implement"] };

test("summaryCards builds one card per summarised run", () => {
  const cases = [
    {
      name: "finished four-stage pipeline",
      ticket: SUMMARY_TICKET,
      keys: ["commit#0", "fix-review#0", "review#0", "implement#0"],
      hues: ["--st-done", "--st-paused", "--st-review", "--st-open"],
    },
    {
      name: "stage that ran twice",
      ticket: RETRY_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
      hues: ["--st-done", "--st-error", "--st-open"],
      meta: ["50m 00s · ×2 · codex", "50m 00s · ×2 · codex · failed", "5m 00s · claude"],
    },
    {
      // Alpine throws on a duplicate x-for key, so the whole tab would render
      // empty if the two review runs both keyed review#0.
      name: "history recorded before the run field existed",
      ticket: LEGACY_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
    },
    {
      // No ribbon segment for review, so each card reads its own clock and the
      // stage's own attempt count.
      name: "stage dropped from the pipeline",
      ticket: DROPPED_STAGE_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
      meta: ["20m 00s · ×2 · codex", "30m 00s · ×2 · codex · failed", "5m 00s · claude"],
    },
    {
      name: "running ticket with an empty top-level summary",
      ticket: { ...RETRY_TICKET, status: "in_progress", summary: "" },
      keys: ["review#1", "review#0", "implement#0"],
    },
    {
      name: "summary set by hand, differing from every run",
      ticket: { ...SUMMARY_TICKET, summary: "Closed by hand." },
      keys: ["summary#top", "commit#0", "fix-review#0", "review#0", "implement#0"],
    },
    {
      name: "no pipeline history at all",
      ticket: { id: "kon-s3", status: "done", summary: "Done outside a pipeline.", stages: [], history: [] },
      keys: ["summary#top"],
    },
    {
      name: "runs without a summary are skipped",
      ticket: { ...SUMMARY_TICKET, summary: "", history: SUMMARY_TICKET.history.map((h, i) => (i === 1 ? { ...h, summary: "" } : h)) },
      keys: ["commit#0", "fix-review#0", "implement#0"],
    },
  ];

  for (const c of cases) {
    const state = pageState(c.ticket);
    const cards = vmValue(state.summaryCards());
    assert.deepEqual(cards.map((k) => k.key), c.keys, c.name);
    if (c.hues) assert.deepEqual(cards.map((card, i) => state.stageHue(card, i)), c.hues, c.name);
    if (c.meta) assert.deepEqual(cards.map((card) => state.stageCardMeta(card)), c.meta, c.name);
  }
});

test("a running ticket keeps the last completed summary on screen", () => {
  // The top-level field is cleared while a stage runs, and the old markup read
  // only that field, so the tab went blank mid-run.
  const state = pageState({ ...RETRY_TICKET, status: "in_progress", summary: "" });

  const cards = state.summaryCards();
  assert.equal(cards[0].summary, "Second pass is clean.");
  assert.equal(cards.some((c) => c.key === "summary#top"), false);
});

test("the synthetic card names the stage that ended the run", () => {
  const state = pageState({ ...SUMMARY_TICKET, summary: "Closed by hand." });

  const top = state.summaryCards()[0];
  assert.equal(top.key, "summary#top");
  assert.equal(top.stage, "commit");
  assert.equal(top.summary, "Closed by hand.");
});

test("card meta reports the aggregate the ribbon reports", () => {
  const state = pageState(RETRY_TICKET);
  const cards = state.summaryCards();
  const ribbon = state.stageRibbon().find((s) => s.name === "review");

  // Both review runs read the stage total, so a card and the ribbon segment
  // above it can never disagree.
  assert.equal(ribbon.seconds, 3000);
  assert.equal(cards[0].seconds, 3000);
  assert.equal(cards[1].seconds, 3000);
});

test("the outcome headline counts stages, churn and commits", () => {
  const cases = [
    { name: "no changes fetched", changes: null, want: "4 stages" },
    // The ribbon above the headline draws one segment per stage, so a retried
    // stage and a hand-set summary must not push the count past it.
    { name: "a stage that ran twice", ticket: RETRY_TICKET, changes: null, want: "2 stages" },
    { name: "a summary set by hand", ticket: { ...SUMMARY_TICKET, summary: "Closed by hand." }, changes: null, want: "4 stages" },
    {
      name: "churn and one commit",
      changes: { base: "main", commits: [{ sha: "abc1234", subject: "Fix summary rail" }], files: [{ path: "a.go", added: 3, deleted: 1 }] },
      want: "4 stages · 1 file · +3 −1 · 1 commit",
    },
    {
      name: "several files and commits",
      changes: {
        base: "main",
        commits: [{ sha: "abc1234", subject: "One" }, { sha: "def5678", subject: "Two" }],
        files: [{ path: "a.go", added: 3, deleted: 1 }, { path: "b.go", added: 0, deleted: 4 }],
      },
      want: "4 stages · 2 files · +3 −5 · 2 commits",
    },
  ];

  for (const c of cases) {
    const state = pageState(c.ticket || SUMMARY_TICKET, { ticketChanges: c.changes });
    assert.equal(state.summaryHeadline(), c.want, c.name);
  }
});

test("collapse-all leaves the newest card open and flips its label", () => {
  const state = pageState(SUMMARY_TICKET);
  const cards = state.summaryCards();

  assert.equal(state.earlierAllCollapsed(), false);
  state.toggleAllStages();
  assert.deepEqual(vmValue(state.collapsedStages), {
    "fix-review#0": true, "review#0": true, "implement#0": true,
  });
  assert.equal(state.collapsedStages[cards[0].key], undefined, "the outcome stays open");
  assert.equal(state.earlierAllCollapsed(), true);

  state.toggleAllStages();
  assert.equal(state.earlierAllCollapsed(), false);
  assert.equal(Object.values(vmValue(state.collapsedStages)).some(Boolean), false);
});

test("a card collapses on its own, including the newest", () => {
  const state = pageState(SUMMARY_TICKET);
  const cards = state.summaryCards();

  state.toggleStageCard(cards[0]);
  assert.equal(state.collapsedStages["commit#0"], true);
  assert.equal(state.earlierAllCollapsed(), false, "the outcome does not count");

  state.toggleStageCard(cards[0]);
  assert.equal(state.collapsedStages["commit#0"], false);
});

test("two runs of one stage collapse independently", () => {
  const state = pageState(RETRY_TICKET);
  const cards = state.summaryCards();

  state.toggleStageCard(cards[1]);
  assert.equal(state.collapsedStages["review#0"], true);
  assert.equal(state.collapsedStages["review#1"], undefined);
});

test("collapse state survives a refresh and resets between tickets", async () => {
  const { state } = routerState("#/");
  state.tickets = [RETRY_TICKET, { id: "kon-other", status: "todo" }];
  state.selectedTicket = RETRY_TICKET;
  state.collapsedStages = { "review#1": true };

  // An SSE refresh replaces selectedTicket wholesale; a collapse keyed on the
  // component rather than on the DOM is unaffected.
  state.applyTicketUpdate({ ...RETRY_TICKET, stage: "review" });
  assert.equal(state.collapsedStages["review#1"], true);

  await state.selectTicket(state.tickets[1]);
  assert.deepEqual(vmValue(state.collapsedStages), {}, "a different ticket starts clean");

  state.collapsedStages = { "review#1": true };
  state.closeDetail();
  assert.deepEqual(vmValue(state.collapsedStages), {}, "closing detail starts clean");
});

test("the summary tab renders one card per run and a commit rail", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tab = html.slice(html.indexOf(`x-show="activeTab === 'summary'"`), html.indexOf("<!-- Diff:"));

  // One card per summarised run, keyed stage#run so a retried stage keeps two.
  assert.match(tab, /x-for="\(card, ci\) in summaryCards\(\)" :key="card\.key"/);
  // The hue rides a custom property, the same shape the board columns use for
  // --col-tint, so no per-hue Tailwind opacity utility is needed.
  assert.match(tab, /:style="'--stage-h: var\(' \+ stageHue\(card, ci\) \+ '\)'"/);
  // Collapse is Alpine state, not a style.display mutation, so it survives the
  // re-render an SSE update triggers.
  assert.match(tab, /@click="toggleStageCard\(card\)"/);
  assert.match(tab, /x-show="!collapsedStages\[card\.key\]"/);
  assert.match(tab, /:aria-expanded="!collapsedStages\[card\.key\]"/);
  assert.match(tab, /x-text="earlierAllCollapsed\(\) \? 'expand all' : 'collapse all'"/);
  assert.match(tab, /x-text="summaryHeadline\(\)"/);
  assert.match(tab, /x-text="stageCardMeta\(card\)"/);

  // The error banner keeps its shape and sits above the outcome rule.
  assert.ok(tab.indexOf('x-text="selectedTicket.last_error"') < tab.indexOf(">outcome<"));
  assert.match(tab, /border-l-\[3px\] border-err pl-3 text-xs/);

  // The rail is dropped, not hidden, when the branch has no commit, and every
  // fact on it comes out of the one changes payload.
  assert.match(tab, /<template x-if="ticketChanges\?\.commits\?\.length">/);
  assert.match(tab, /x-text="ticketChanges\?\.branch"/);
  assert.match(tab, /x-text="ticketChanges\?\.base"/);
  // copyBranch stores the copied string, so each sha chip ticks on its own;
  // copiedId is one boolean and would tick every chip at once.
  assert.match(tab, /@click="copyBranch\(c\.sha\)"/);
  assert.match(tab, /copiedBranch === c\.sha/);
  // The old centered card and its details disclosure are gone.
  assert.equal(/earlierSummaries|max-w-3xl mx-auto px-6 py-6[\s\S]{0,80}last_error/.test(tab), false);
});

// The same finished run, with the ticket-level summary the daemon synthesizes
// once a terminal stage succeeds.
const FINAL_SUMMARY_TICKET = { ...SUMMARY_TICKET, id: "kon-s6", final_summary: "Redesigned the summary tab end to end." };

test("the final summary is a ticket-level field, not a stage card", () => {
  const cases = [
    {
      name: "final summary next to the stage cards",
      ticket: FINAL_SUMMARY_TICKET,
      final: "Redesigned the summary tab end to end.",
      keys: ["commit#0", "fix-review#0", "review#0", "implement#0"],
      headline: "4 stages",
      tab: true,
    },
    {
      name: "latest run only, as before",
      ticket: SUMMARY_TICKET,
      final: "",
      keys: ["commit#0", "fix-review#0", "review#0", "implement#0"],
      headline: "4 stages",
      tab: true,
    },
    {
      name: "history only, with the field cleared for a running stage",
      ticket: { ...RETRY_TICKET, status: "in_progress", summary: "" },
      final: "",
      keys: ["review#1", "review#0", "implement#0"],
      headline: "2 stages",
      tab: true,
    },
    {
      name: "a legacy ticket carrying no summary at all",
      ticket: { id: "kon-s7", status: "done", summary: "", stages: [], history: [] },
      final: "",
      keys: [],
      headline: "0 stages",
      tab: false,
    },
    {
      name: "a final summary with no run summary behind it",
      ticket: { id: "kon-s8", status: "done", summary: "", stages: [], history: [], final_summary: "Everything is done." },
      final: "Everything is done.",
      keys: [],
      headline: "0 stages",
      tab: true,
    },
  ];

  for (const c of cases) {
    const state = pageState(c.ticket);
    assert.equal(state.finalSummary(), c.final, c.name);
    // The ticket-level text is no card, so it changes neither the stage cards,
    // the stage count, nor which card is treated as the latest.
    assert.deepEqual(vmValue(state.summaryCards()).map((k) => k.key), c.keys, c.name);
    assert.equal(state.summaryHeadline(), c.headline, c.name);
    assert.equal(state.showSummaryTab(), c.tab, c.name);
  }
});

test("the final summary stays out of the collapse controls", () => {
  const state = pageState(FINAL_SUMMARY_TICKET);

  state.toggleAllStages();
  assert.deepEqual(vmValue(state.collapsedStages), {
    "fix-review#0": true, "review#0": true, "implement#0": true,
  }, "collapse-all still skips only the newest stage card");
  assert.equal(state.earlierAllCollapsed(), true);
});

test("a final summary arriving over SSE reaches the open summary tab", () => {
  const { state } = routerState("#/");
  state.tickets = [SUMMARY_TICKET];
  state.selectedTicket = SUMMARY_TICKET;
  state.activeTab = "summary";

  assert.equal(state.finalSummary(), "");
  // The daemon writes the ticket-level summary after the terminal update, so
  // the tab sees it arrive on its own event.
  state.applyTicketUpdate({ ...SUMMARY_TICKET, final_summary: FINAL_SUMMARY_TICKET.final_summary });

  assert.equal(state.finalSummary(), "Redesigned the summary tab end to end.");
  assert.equal(state.activeTab, "summary", "the open tab stays open");
});

test("both summary views put the ticket-level outcome above the stage summaries", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tab = html.slice(html.indexOf(`x-show="activeTab === 'summary'"`), html.indexOf("<!-- Diff:"));

  // Desktop: its own block above the cards, with no collapse control of its own.
  assert.ok(tab.indexOf('x-if="finalSummary()"') < tab.indexOf('x-for="(card, ci) in summaryCards()"'));
  assert.equal(/final-card[\s\S]{0,400}toggleStageCard/.test(tab), false);

  // Mobile: the outcome above the last run's summary, which keeps its stage.
  const mobile = html.slice(html.indexOf(`x-show="detailTab==='ticket'"`));
  assert.ok(mobile.indexOf('x-if="finalSummary()"') < mobile.indexOf('x-if="selectedTicket?.summary"'));
  assert.match(mobile, /x-text="'\u00b7 ' \+ summaryStage\(\)"/);
});

test("the summary rail wraps on a container query, not a viewport one", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The ticket page keeps a fixed 308px rail, so the pane is ~300px narrower
  // than the window and a viewport breakpoint would fire far too late.
  assert.match(html, /\.summary-scroll \{ container-type: inline-size; \}/);
  assert.match(html, /@container \(max-width: 1000px\) \{\s*\.summary-layout:has\(\.summary-rail\) \{ grid-template-columns: minmax\(0, 1fr\); \}\s*\.summary-rail \{ grid-column: 1; grid-row: auto; position: static; \}\s*\}/);
  assert.match(html, /\.summary-rail \{[^}]*position: sticky/);
  assert.match(html, /\.summary-layout:has\(\.summary-rail\) \{ grid-template-columns: minmax\(0, 1fr\) 264px; \}/);
});

test("every tab pane starts its first row on the rail's first label", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The rail's p-4 is the reference inset; the ticket, diff and summary panes
  // match it, so the pane's first row and the rail's first label share a line.
  assert.match(html, /overflow-y-auto p-4 pb-\[22px\]/);
  assert.match(html, /max-w-\[960px\] mx-auto px-6 pt-4 pb-6/);
  assert.match(html, /max-w-3xl mx-auto px-6 pt-4 pb-6/);
  assert.match(html, /\.summary-layout \{[^}]*padding: 1rem 1\.5rem 3rem/);
  // text-xs on the banner wrapper: the inherited strut is taller than the 12px
  // line it holds and would push the first line below the rail's label.
  assert.match(html, /border-l-\[3px\] border-warn pl-3 text-xs/);
});

test("one rule closes the tab row across the pane and the rail", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // The rule sits on the row, not inside either cell, so it runs edge to edge
  // instead of breaking into a px-6 segment and a px-4 one.
  assert.match(html, /<div class="shrink-0 flex items-stretch border-b border-surface-700\/70">/);
  assert.match(html, /<div class="flex-1 min-w-0 px-6 pt-\[11px\]">\s*<div class="flex items-center gap-5">/);
  // The rail's header cell is a bare spacer: painting it would start the frame
  // colour a tab row above the pane's.
  assert.match(html, /<div class="shrink-0 w-\[308px\] border-l border-surface-700\/50"><\/div>/);
});

test("the commit rail starts level with the top of the card column", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tab = html.slice(html.indexOf(`x-show="activeTab === 'summary'"`), html.indexOf("<!-- Diff:"));

  // The outcome header is a sibling of the card column, not its first child, so
  // grid row 2 starts at the top of the column in both columns: the final
  // outcome block when the ticket has one, the first stage card otherwise.
  assert.ok(tab.indexOf('class="summary-head') < tab.indexOf('class="summary-col"'));
  assert.match(tab, /<div class="summary-col">\s*<template x-if="finalSummary\(\)">/);
  assert.match(tab, /<\/template>\s*<template x-for="\(card, ci\) in summaryCards\(\)"/);
  assert.match(html, /\.summary-rail \{[^}]*grid-row: 2/);
  // The rail card's own top padding lines its label up with the stage name,
  // which the stage head centres in 36px.
  assert.match(html, /\.rail-card \{[^}]*padding: 11px 15px 14px/);
});

test("the UI sources no longer define earlierSummaries", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // earlierSummaries fed the <details> disclosure the cards replaced.
  assert.equal(/earlierSummaries/.test(uiSource + html), false);
});

// A DOM double deep enough for the entity pass: an element/text tree, the
// TreeWalker it walks, and the fragment splice it ends on. innerHTML does not
// parse — the harness stubs marked and DOMPurify with identity, so the "HTML"
// a prose writer assigns is the source text, and one text node is all the
// entity pass needs to chew on.
function fakeProseDom() {
  const doc = {
    getElementById: () => null,
    querySelector: () => null,
    hasFocus: () => true,
    documentElement: { style: {} },
    createTextNode(nodeValue) {
      return { nodeType: 3, nodeValue, parentNode: null, get parentElement() { return this.parentNode; } };
    },
    createDocumentFragment() {
      return { nodeType: 11, childNodes: [], appendChild(n) { this.childNodes.push(n); return n; } };
    },
    createElement(tag) {
      const el = {
        nodeType: 1,
        tagName: tag.toUpperCase(),
        className: "",
        style: {},
        childNodes: [],
        parentNode: null,
        attrs: {},
        events: {},
        _html: "",
        get parentElement() { return this.parentNode; },
        get textContent() {
          return this.childNodes.map((n) => (n.nodeType === 3 ? n.nodeValue : n.textContent)).join("");
        },
        set textContent(v) { this.childNodes = [doc.createTextNode(v)]; this.childNodes[0].parentNode = this; },
        get innerHTML() { return this._html; },
        set innerHTML(v) { this._html = v; this.textContent = v; },
        classList: {
          add: (c) => { if (!el.className.split(" ").includes(c)) el.className = `${el.className} ${c}`.trim(); },
          remove: (c) => { el.className = el.className.split(" ").filter((x) => x && x !== c).join(" "); },
        },
        setAttribute(k, v) { this.attrs[k] = String(v); },
        getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
        addEventListener(type, fn) { (this.events[type] = this.events[type] || []).push(fn); },
        appendChild(n) { n.parentNode = this; this.childNodes.push(n); return n; },
        replaceChild(fresh, old) {
          const at = this.childNodes.indexOf(old);
          const kids = fresh.nodeType === 11 ? fresh.childNodes : [fresh];
          kids.forEach((k) => { k.parentNode = this; });
          this.childNodes.splice(at, 1, ...kids);
          return old;
        },
        closest(selector) {
          const tags = selector.split(",").map((s) => s.trim().toUpperCase());
          for (let n = this; n; n = n.parentNode) if (tags.includes(n.tagName)) return n;
          return null;
        },
      };
      return el;
    },
    // Snapshot rather than live: the caller mutates the tree as it goes, which
    // is the same reason the implementation collects before it wraps.
    createTreeWalker(root, whatToShow, filter) {
      const out = [];
      (function walk(n) {
        (n.childNodes || []).forEach((c) => {
          if (c.nodeType === 3) {
            if (!filter || filter.acceptNode(c) === 1) out.push(c);
          } else {
            walk(c);
          }
        });
      })(root);
      let i = 0;
      return { nextNode: () => (i < out.length ? out[i++] : null) };
    },
  };

  // ["tag", child, ...] builds an element; a string builds a text node.
  const build = (spec) => {
    if (typeof spec === "string") return doc.createTextNode(spec);
    const el = doc.createElement(spec[0]);
    spec.slice(1).forEach((c) => el.appendChild(build(c)));
    return el;
  };

  const chips = (root) => {
    const out = [];
    (function walk(n) {
      (n.childNodes || []).forEach((c) => {
        if (c.nodeType !== 1) return;
        if (/^ent[\s-]/.test(c.className)) out.push(c);
        walk(c);
      });
    })(root);
    return out;
  };

  return { doc, build, chips };
}

// The commit the summary talks about, plus a hex word that is not a commit.
const ENTITY_CHANGES = {
  base: "main",
  commits: [{ sha: "abc1234", subject: "Fix summary rail" }],
  files: [{ path: "internal/web/ui/app.js", added: 2, deleted: 1 }],
  remote: "https://github.com/owner/repo",
};

// Tickets a summary may name, one status each: a ticket chip wears the status
// colour of the ticket it points at.
const ENTITY_TICKETS = [
  { id: "kon-4h2p", title: "Rebuild the summary tab", status: "done" },
  { id: "kon-9xz1", title: "Drop the stale rail", status: "cancelled" },
];

// The entity pass reaches the document global, so the double has to be the
// context's document rather than a field on the component.
function entityState(dom, overrides = {}) {
  const state = loadKontoraState({ document: dom.doc });
  state.selectedTicket = { ...SUMMARY_TICKET, branch: "kontora/kon-s1" };
  state.now = Date.parse("2026-08-08T12:00:00Z");
  state.ticketChanges = ENTITY_CHANGES;
  state.tickets = ENTITY_TICKETS;
  Object.assign(state, overrides);
  return state;
}

test("entity chips skip code and links and stop at the cap", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const many = Array.from({ length: 50 }, (_, i) => `ENV_VAR${i}`).join(" ");
  const root = dom.build(["div",
    `Committed abc1234 on kontora/kon-s1. ${many}`,
    ["code", "abc1234"],
    ["pre", ["code", "abc1234 in a fence"]],
    ["a", "abc1234"],
  ]);

  state._markEntities(root);
  const marked = dom.chips(root);

  // The hover card carries the commit subject the rail would otherwise hide.
  const sha = marked.find((c) => c.textContent === "abc1234");
  assert.equal(sha.getAttribute("data-tip-e"), "abc1234");
  assert.equal(sha.getAttribute("data-tip-e-body"), "Fix summary rail");
  // A long summary is not confetti.
  assert.equal(marked.length, 40);
  // Code and links own their text, so their copies are untouched.
  const owned = root.childNodes.filter((n) => n.nodeType === 1 && !["SPAN"].includes(n.tagName));
  assert.deepEqual(owned.map((n) => n.tagName), ["CODE", "PRE", "A"]);
  owned.forEach((n) => {
    assert.deepEqual(dom.chips(n), [], n.tagName);
    assert.match(n.textContent, /abc1234/);
  });
});

test("only shas the branch produced become chips", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const root = dom.build(["div", "abc1234 is real, deadbeef and feedface are not."]);

  state._markEntities(root);

  // \b[0-9a-f]{7,40}\b alone chips every hex-looking word in the prose.
  assert.deepEqual(dom.chips(root).map((c) => c.textContent), ["abc1234"]);
});

test("each pattern claims only the text it names", () => {
  const cases = [
    {
      // \b sits inside domain and maintenance, because a branch name may hold
      // a slash or a dash and those are not word characters.
      name: "a short branch name does not chip the middle of a word",
      branch: "main",
      text: "The remaining domain maintenance work landed on main.",
      want: [["main", "ent ent-branch"]],
    },
    {
      name: "a file whose name starts with the branch name",
      branch: "main",
      text: "Rewrote maintenance.go.",
      want: [["maintenance.go", "ent ent-file"]],
    },
    {
      name: "a dotted file name is a file, not an attribute path",
      branch: "kontora/kon-s1",
      text: "Added cases to contract.test.ts.",
      want: [["contract.test.ts", "ent ent-file"]],
    },
    {
      name: "a dotted attribute path is still an attribute",
      branch: "kontora/kon-s1",
      text: "Raised api.client.timeout.",
      want: [["api.client.timeout", "ent ent-attr"]],
    },
    {
      // The extension list decides this one: an extension missing from it
      // leaves the run to the attribute pattern, which claims it on the dots.
      name: "a test file with two dots is a file, not an attribute path",
      branch: "kontora/kon-s1",
      text: "Extended index_html.test.mjs.",
      want: [["index_html.test.mjs", "ent ent-file"]],
    },
    {
      name: "a file chips with the directories it was written with",
      branch: "kontora/kon-s1",
      text: "Rewrote /Users/a/org/x.md and internal/web/static/app.css.",
      want: [
        ["/Users/a/org/x.md", "ent ent-file"],
        ["internal/web/static/app.css", "ent ent-file"],
      ],
    },
    {
      name: "a library named after an extension stays prose",
      branch: "kontora/kon-s1",
      text: "Alpine.js reads app.js.",
      want: [["app.js", "ent ent-file"]],
    },
    {
      name: "an uppercase word is an env var only with an underscore",
      branch: "kontora/kon-s1",
      text: "README and CHANGELOG describe ASTRA_BENCH_GATE.",
      want: [["ASTRA_BENCH_GATE", "ent ent-env"]],
    },
    {
      // chips() walks into the chip, so the halves follow their parent.
      name: "a diff stat splits into a green half and a red one",
      branch: "kontora/kon-s1",
      text: "The branch is +750/-350 so far.",
      want: [
        ["+750/-350", "ent ent-diff"],
        ["+750", "ent-add"],
        ["-350", "ent-del"],
      ],
    },
    {
      name: "the deleted half is red whichever side it was written on",
      branch: "kontora/kon-s1",
      text: "Reverted, -50/+350 against main.",
      want: [
        ["-50/+350", "ent ent-diff"],
        ["-50", "ent-del"],
        ["+350", "ent-add"],
      ],
    },
    {
      name: "the exit code is coloured by zero against everything else",
      branch: "kontora/kon-s1",
      text: "The stage exits 0; the retry exited 5.",
      want: [["exits 0", "ent ent-ok"], ["exited 5", "ent ent-bad"]],
    },
    {
      // test-lisp has the shape of an id and names no ticket.
      name: "a ticket id chips only when the board holds that ticket",
      branch: "kontora/kon-s1",
      text: "Follows kon-4h2p, and test-lisp still runs.",
      want: [["kon-4h2p", "ent ent-ticket text-st-done"]],
    },
    {
      name: "a counted noun takes one word in front of it",
      branch: "kontora/kon-s1",
      text: "239 node tests and 1 skipped.",
      want: [["239 node tests", "ent-count"], ["1 skipped", "ent-count"]],
    },
  ];

  for (const c of cases) {
    const dom = fakeProseDom();
    const state = entityState(dom, { selectedTicket: { ...SUMMARY_TICKET, branch: c.branch } });
    const root = dom.build(["div", c.text]);

    state._markEntities(root);

    assert.deepEqual(dom.chips(root).map((n) => [n.textContent, n.className]), c.want, c.name);
  }
});

test("the branch chip carries the base it forked from", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const root = dom.build(["div", "Pushed kontora/kon-s1."]);

  state._markEntities(root);
  const chip = dom.chips(root)[0];

  assert.equal(chip.textContent, "kontora/kon-s1");
  assert.match(chip.getAttribute("data-tip-e-body"), /main/);
});

test("a pull request number links under the project's own origin", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const root = dom.build(["div", "Merged PR #511."]);

  state._markEntities(root);
  const chip = dom.chips(root)[0];

  assert.equal(chip.tagName, "A");
  assert.equal(chip.getAttribute("href"), "https://github.com/owner/repo/pull/511");
  // A new tab, and no window handle back to this page.
  assert.equal(chip.getAttribute("target"), "_blank");
  assert.equal(chip.getAttribute("rel"), "noopener noreferrer");
  assert.equal(chip.getAttribute("data-tip-e-body"), "owner/repo");
});

test("a number stays prose without a github origin behind it", () => {
  // The path a number sits under is the host's convention, so a remote that is
  // not GitHub, and a project with no origin at all, both decline the chip.
  for (const remote of ["https://gitlab.com/owner/repo", "", undefined]) {
    const dom = fakeProseDom();
    const state = entityState(dom, { ticketChanges: { ...ENTITY_CHANGES, remote } });
    const root = dom.build(["div", "Merged PR #511."]);

    state._markEntities(root);

    assert.deepEqual(dom.chips(root), [], String(remote));
    assert.equal(root.textContent, "Merged PR #511.");
  }
});

test("a ticket chip opens the ticket it names and keeps the prose around it", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  let opened = null;
  state._paletteOpenTicket = (t) => { opened = t; };
  const text = "no-push, then kon-9xz1, then test-lisp.";
  const root = dom.build(["div", text]);

  state._markEntities(root);
  const chip = dom.chips(root)[0];

  // The id says nothing about the ticket, so the card leads with the title.
  assert.equal(chip.textContent, "kon-9xz1");
  assert.equal(chip.className, "ent ent-ticket text-st-cancel");
  assert.equal(chip.getAttribute("data-tip-e"), "Drop the stale rail");
  assert.equal(chip.getAttribute("data-tip-e-body"), "cancelled");
  assert.equal(chip.getAttribute("data-tip-e-hint"), "click to open");
  // No [tag] in that title, so the card paints none and asks for the neutral
  // hue rather than leaving the last chip's colour on the shared node.
  assert.equal(chip.getAttribute("data-tip-e-tag"), "");
  assert.equal(chip.getAttribute("data-tip-e-tag-color"), "none");

  chip.events.click[0]();
  assert.equal(opened.id, "kon-9xz1");
  // The two declined runs sit either side of the chip, so a splice that drops
  // them would show up here.
  assert.equal(root.textContent, text);
});

test("a file the branch changed carries its diff stat", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const root = dom.build(["div", "Rewrote app.js, left docs/plan.md alone."]);

  state._markEntities(root);
  const [changed, untouched] = dom.chips(root);

  // The summary names the file by its basename; the changed-file list holds
  // the path it was committed under.
  assert.equal(changed.textContent, "app.js");
  assert.equal(changed.getAttribute("data-tip-e-body"), "+2/-1 on this branch");
  assert.equal(changed.getAttribute("data-tip-e-hint"), "click to copy");
  // A file outside the diff has no record behind it, so it stays a plain chip.
  assert.equal(untouched.textContent, "docs/plan.md");
  assert.equal(untouched.getAttribute("data-tip-e"), null);
});

test("authored prose gets ticket chips only, summary prose gets the rest too", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  const body = dom.doc.createElement("div");
  const note = dom.doc.createElement("div");
  const summary = dom.doc.createElement("div");
  const text = "Reverted kon-9xz1 in abc1234.";

  // A body and a note are authored text: a chip over the sha would rewrite what
  // the reporter typed, while an id names a record the reader can open.
  state.setProse(body, text);
  assert.deepEqual(dom.chips(body).map((c) => c.textContent), ["kon-9xz1"]);
  assert.equal(body.textContent, text);

  state.setNoteText(note, text);
  assert.deepEqual(dom.chips(note).map((c) => c.textContent), ["kon-9xz1"]);
  assert.equal(note.textContent, text);

  state.setSummaryProse(summary, text);
  assert.deepEqual(dom.chips(summary).map((c) => c.textContent), ["kon-9xz1", "abc1234"]);
});

test("an authored-prose write re-runs when the board lands and skips a repeat", () => {
  const dom = fakeProseDom();
  const state = entityState(dom, { tickets: [] });
  const body = dom.doc.createElement("div");
  const note = dom.doc.createElement("div");

  // fetchTasks resolves after the first paint, so a memo keyed on the text
  // alone would leave the id as prose for good.
  state.setProse(body, "Reverted kon-9xz1.");
  state.setNoteText(note, "Reverted kon-9xz1.");
  assert.deepEqual(dom.chips(body), []);
  assert.deepEqual(dom.chips(note), []);

  state.tickets = ENTITY_TICKETS;
  state.setProse(body, "Reverted kon-9xz1.");
  state.setNoteText(note, "Reverted kon-9xz1.");
  assert.deepEqual(dom.chips(body).map((c) => c.textContent), ["kon-9xz1"]);
  assert.deepEqual(dom.chips(note).map((c) => c.textContent), ["kon-9xz1"]);

  // The second write with everything unchanged marks nothing again.
  const marked = dom.chips(note)[0];
  state.setNoteText(note, "Reverted kon-9xz1.");
  assert.equal(dom.chips(note)[0], marked);
});

test("a prose id resolves against the open ticket's relations, not the board alone", () => {
  const dom = fakeProseDom();
  const state = entityState(dom);
  // An archived dep is not on the board, and the daemon resolved it for the
  // rail, so the body can point at it as well.
  state.selectedTicket = {
    ...state.selectedTicket,
    deps: [{ id: "kon-arc1", title: "Archived blocker", status: "archived" }],
  };
  const body = dom.doc.createElement("div");

  state.setProse(body, "Waiting on kon-arc1, and on no-push.");
  const chip = dom.chips(body)[0];

  assert.equal(chip.textContent, "kon-arc1");
  assert.equal(chip.getAttribute("data-tip-e"), "Archived blocker");
  // A word of the same shape that names no ticket stays prose.
  assert.deepEqual(dom.chips(body).length, 1);
});

test("the summary prose write re-runs when the commit list lands", () => {
  const dom = fakeProseDom();
  const state = entityState(dom, { ticketChanges: null });
  const el = dom.doc.createElement("div");

  // fetchChanges resolves after the first paint, so a memo keyed on the
  // markdown alone would leave the summary with no sha chip for good.
  state.setSummaryProse(el, "Reverted in abc1234.");
  assert.deepEqual(dom.chips(el), []);

  state.ticketChanges = ENTITY_CHANGES;
  state.setSummaryProse(el, "Reverted in abc1234.");
  assert.deepEqual(dom.chips(el).map((c) => c.textContent), ["abc1234"]);
});

test("the entity hover card is a third tip instance that stops under reduced motion", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tips = fs.readFileSync(tipsPath, "utf8");

  assert.match(html, /<div id="global-tip-e">/);
  assert.match(tips, /setupTip\('global-tip-e', '\[data-tip-e\]', 'data-tip-e'/);
  // Left-aligned to the entity, 12px off either window edge, flipped below
  // when it does not fit above.
  assert.match(tips, /if \(left < 12\) left = 12;/);
  assert.match(tips, /if \(left \+ tw > window\.innerWidth - 12\)/);
  assert.match(tips, /if \(top < 8\) top = r\.bottom \+ 10;/);
  // After the rule it overrides, not in the reduced-motion block near the top:
  // one id selector loses to a later id selector whatever the media query says.
  const reduced = html.indexOf("#global-tip-e { transition: none; transform: none; }");
  assert.ok(reduced > html.indexOf("transition: opacity .12s ease"));
  assert.match(html.slice(0, reduced), /@media \(prefers-reduced-motion: reduce\) \{\s*$/);
  // em, so a chip tracks the prose it interrupts.
  assert.match(html, /\.ent \{[^}]*font-size: \.86em/);
});

// tips.js hangs its three cards off document-level listeners, so the harness
// only has to answer getElementById and hand the handlers an event.
function fakeTips() {
  const handlers = {};
  const span = () => ({ textContent: "", style: {} });
  const card = () => {
    const title = { textContent: "", style: {}, children: [span(), span()] };
    return {
      textContent: "",
      style: {},
      attrs: {},
      offsetWidth: 200,
      offsetHeight: 60,
      children: [title, span(), span()],
      setAttribute(k, v) { this.attrs[k] = v; },
    };
  };
  const cards = { "global-tip": card(), "global-tip-t": card(), "global-tip-e": card() };
  const ctx = {
    document: {
      getElementById: (id) => cards[id],
      addEventListener(type, fn) { (handlers[type] = handlers[type] || []).push(fn); },
    },
    window: { innerWidth: 1400 },
    setTimeout,
    clearTimeout,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(tipsPath, "utf8"), ctx);

  const chip = (attrs) => {
    const el = {
      attrs,
      getAttribute: (k) => (k in attrs ? attrs[k] : null),
      getBoundingClientRect: () => ({ left: 300, right: 400, top: 200, bottom: 220, width: 100, height: 20 }),
      closest: (sel) => (sel.slice(1, -1) in attrs ? el : null),
    };
    return el;
  };
  const fire = (type, target) => (handlers[type] || []).forEach((fn) => fn({ target }));
  return { cards, chip, fire };
}

test("a hover card drops on the click that opens the ticket, and returns on the next move", () => {
  const tips = fakeTips();
  const card = tips.cards["global-tip-e"];

  tips.fire("mouseenter", tips.chip({ "data-tip-e": "gra-1", "data-tip-e-body": "done" }));
  assert.equal(card.style.opacity, "1");

  // Opening the ticket takes the chip off the page, and a node the DOM no
  // longer holds fires no mouseleave: without the click the card would stay.
  tips.fire("click");
  assert.equal(card.style.opacity, "0");

  // A move with nothing under it leaves the card down, and a move over the row
  // the click drew there brings it back with that row's text.
  tips.fire("mousemove", tips.chip({}));
  assert.equal(card.style.opacity, "0");
  tips.fire("mousemove", tips.chip({ "data-tip-e": "gra-2", "data-tip-e-body": "todo" }));
  assert.equal(card.style.opacity, "0");

  tips.fire("mouseenter", tips.chip({ "data-tip-e": "gra-2", "data-tip-e-body": "todo" }));
  tips.fire("click");
  tips.fire("mousemove", tips.chip({ "data-tip-e": "gra-3", "data-tip-e-body": "todo" }));
  assert.equal(card.style.opacity, "1");
  assert.equal(card.children[0].children[1].textContent, "gra-3");
});

test("the hover card's title splits into a coloured tag and the name", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tips = fs.readFileSync(tipsPath, "utf8");

  // A real space between the two spans, the only place the title can wrap
  // between them: the card has a max-width and no overflow-wrap.
  assert.match(html, /<span class="tip-e-title"><span class="tip-e-tag"><\/span> <span class="tip-e-name"><\/span><\/span>/);
  // The tag goes in the first child of the title span, the name in the second,
  // so children[1] and children[2] still address the body and the hint.
  assert.match(tips, /title\.children\[0\]\.textContent = tag;/);
  assert.match(tips, /title\.children\[0\]\.style\.display = tag \? '' : 'none';/);
  assert.match(tips, /title\.children\[1\]\.textContent = trig\.getAttribute\('data-tip-e'\);/);
  // The card is a body-level node with no tagged ancestor, so the hue is
  // mirrored onto it, unconditionally, or it leaks into the next hover.
  assert.match(tips, /el\.setAttribute\('data-pipe-color', trig\.getAttribute\('data-tip-e-tag-color'\) \|\| 'none'\);/);
  assert.match(html, /#global-tip-e \.tip-e-tag \{ color: hsl\(var\(--pipe-h, 240 10% 55%\)\); white-space: nowrap; \}/);
});

// --- Stats view ---

function statsPayload(over = {}) {
  return {
    days: [
      { date: "2026-08-10", runs: 4 },
      { date: "2026-08-11", runs: 0 },
      { date: "2026-08-12", runs: 9 },
    ],
    buckets: [{ start: "2026-08-09", done: 3, cancelled: 1, tokens_in: 1300, tokens_out: 200, tokens_cache_create: 50, tokens_cache_read: 250 }],
    // Two stages that rank one way by time and the other way by tokens, so the
    // stage panel's two modes cannot pass by agreeing with each other.
    stages: [
      { name: "plan", p50_ms: 120000, p90_ms: 300000, share: 60, runs: 2, failed: 0, retry_pct: 0, tokens: 300, tokens_p90: 150, token_share: 37.5, token_runs: 2 },
      { name: "implement", p50_ms: 132000, p90_ms: 600000, share: 40, runs: 4, failed: 1, retry_pct: 25, tokens: 500, tokens_p90: 500, token_share: 62.5, token_runs: 1 },
    ],
    agents: [{ name: "claude", model: "sonnet-4.6", runs: 4, first_pass_pct: 82, median_ms: 132000, tokens_per_run: 14200, retries_per_ticket: 0.4 }],
    projects: [{ name: "kontora", done: 3, median_cycle_ms: 13200000 }],
    live: { running: 1, slots: 3, queued: 2, oldest_wait_ms: 360000, in_review: 5, busy: ["claude"] },
    totals: {
      shipped: 3, shipped_this_week: 3, runs: 13, median_cycle_ms: 15120000,
      median_cycle_delta_ms: -2280000, first_pass_pct: 78, tokens_in: 1300, tokens_out: 200,
      tokens_cache_create: 50, tokens_cache_read: 250,
      tokens_delta_pct: 12, busiest_day: "2026-08-12", busiest_day_runs: 9,
    },
    window: { days: 98, unit: "week", buckets: 14, from: "2026-05-07", to: "2026-08-12" },
    ...over,
  };
}

// statsState wires the fake clock, storage and fetch the Stats poll needs, and
// records what each of them saw.
function statsState(payload = statsPayload()) {
  const timers = { made: 0, cleared: [], fn: null };
  const fetched = [];
  const store = {};
  const location = { protocol: "http:", host: "localhost:8080", hash: "" };
  const { ctx, state } = loadKontoraContext({
    location,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = v; },
    },
    setInterval(fn) { timers.made++; timers.fn = fn; return timers.made; },
    clearInterval(id) { timers.cleared.push(id); },
    fetch: async (url) => {
      fetched.push(url);
      if (url.startsWith("/api/tickets/")) {
        const id = url.slice("/api/tickets/".length);
        return { ok: true, json: async () => ({ id: id, status: "todo" }) };
      }
      return { ok: true, json: async () => payload };
    },
  });
  state.$nextTick = (cb) => { if (cb) cb(); return Promise.resolve(); };
  state.recomputeBoard = () => {};
  state.closeTerminal = () => {};
  state.startEditing = () => {};
  state.flushEditSave = () => {};
  state.openSettings = async () => { state.currentView = "settings"; };
  state.openCreateModal = async () => { state.currentView = "new"; };
  return { ctx, state, timers, fetched, store, location };
}

// statsModeState builds the component over a storage the caller controls, so a
// test can seed the stage mode read at construction, corrupt it, or make the
// read throw the way a browser in private mode does.
function statsModeState(storage) {
  const { state } = loadKontoraContext({
    location: { protocol: "http:", host: "localhost:8080", hash: "" },
    localStorage: storage,
    setInterval() { return 1; },
    clearInterval() {},
    fetch: async () => ({ ok: true, json: async () => statsPayload() }),
  });
  return state;
}

test("statsCompact shortens a count the way a 26px KPI needs", () => {
  const { state } = statsState();
  const cases = [
    [0, "0"], [7, "7"], [999, "999"], [1000, "1k"], [2345, "2.3k"],
    [14200, "14.2k"], [1000000, "1M"], [24000000, "24M"], [3200000000, "3.2B"],
    // Rounding must not carry a value past the unit it was scaled to: '1000k'
    // is one character wider than the KPI value slot holds.
    [999999, "1M"], [999950, "1M"], [999999999, "1B"],
  ];
  for (const [input, want] of cases) assert.equal(state.statsCompact(input), want, String(input));
});

test("statsDuration writes a span the way the panels read it", () => {
  const { state } = statsState();
  const cases = [
    // An em dash means "not measured", so a real span too short to round to a
    // minute must not borrow it.
    [0, "—"], [null, "—"], [20000, "<1m"], [1000, "<1m"], [60000, "1m"], [1320000, "22m"],
    [3900000, "1h 05m"], [15120000, "4h 12m"], [180000000, "2d 02h"],
  ];
  for (const [input, want] of cases) assert.equal(state.statsDuration(input), want, String(input));
});

test("statsFirstPassColor turns on the design's 78 and 70 boundaries", () => {
  const { state } = statsState();
  const cases = [[100, "ok"], [78, "ok"], [77.9, "warn"], [70, "warn"], [69.9, "err"], [0, "err"]];
  for (const [input, want] of cases) assert.equal(state.statsFirstPassColor(input), want, String(input));
});

test("the heat map pads a window that does not open on a Sunday", () => {
  const { state } = statsState();
  // 2026-08-05 is a Wednesday, so the first column needs three blanks.
  const days = [];
  for (let i = 0; i < 10; i++) days.push({ date: "2026-08-" + String(5 + i).padStart(2, "0"), runs: i });

  const heat = state.statsHeatWeeks(days);

  assert.equal(heat.weeks.length, 2);
  assert.deepEqual(vmValue(heat.weeks[0].days.slice(0, 3)), [null, null, null]);
  assert.equal(heat.weeks[0].days[3].date, "2026-08-05");
  assert.equal(heat.weeks[1].days[0].date, "2026-08-09");
  // Every column holds seven rows, so the weekday gutter lines up.
  for (const w of heat.weeks) assert.equal(w.days.length, 7);
  assert.equal(heat.weeks[1].days[6], null);
  assert.equal(heat.max, 9);
  assert.equal(heat.weeks[0].days[3].level, 0, "a day with no runs is level 0");
  assert.equal(heat.weeks[1].days[5].level, 4, "the busiest day is level 4");
  assert.deepEqual(vmValue(heat.months), [{ label: "Aug", left: 0 }]);
});

test("a month tick sits on the column its month starts in", () => {
  const { state } = statsState();
  const days = [];
  const start = Date.UTC(2026, 6, 27); // Monday 2026-07-27
  for (let i = 0; i < 20; i++) {
    days.push({ date: new Date(start + i * 86400000).toISOString().slice(0, 10), runs: 1 });
  }

  const heat = state.statsHeatWeeks(days);

  // 16px is the cell pitch: a 13px cell plus the 3px gap.
  assert.deepEqual(vmValue(heat.months), [{ label: "Jul", left: 0 }, { label: "Aug", left: 16 }]);
});

test("the derived view model survives an empty window", () => {
  const { ctx, state } = statsState();
  const derived = ctx.statsDerive({
    days: [], buckets: [], stages: [], agents: [], projects: [],
    live: { slots: 3 }, totals: {}, window: { days: 98, unit: "week", buckets: 14 },
  });

  assert.deepEqual(vmValue(derived.heat.weeks), []);
  assert.deepEqual(vmValue(derived.throughput), []);
  assert.deepEqual(vmValue(derived.stages), []);
  assert.equal(derived.slots.length, 3, "the slot strip is drawn from the daemon's capacity, not its history");
  assert.equal(derived.live.oldest, "—");
  assert.equal(derived.kpis.length, 6);
  assert.equal(derived.medianCycle, "—");

  const bad = [];
  (function scan(v, path) {
    if (typeof v === "number") { if (!Number.isFinite(v)) bad.push(path); return; }
    if (v && typeof v === "object") Object.keys(v).forEach((k) => scan(v[k], path + "." + k));
  })(vmValue(derived), "derived");
  assert.deepEqual(bad, [], "no chart may divide by a zero maximum");
  assert.equal(state.statsCards().length, 6, "the KPI strip keeps its six cards before the first payload");
});

test("the throughput charts follow the bucket the server cut", () => {
  const { ctx, state } = statsState();
  const cases = [
    {
      unit: "hour",
      buckets: [{ start: "2026-08-12T09:00", done: 1, cancelled: 1, tokens_in: 300, tokens_out: 40 }],
      shipped: "1 shipped · 09:00 · 1 cancelled",
      tokens: "340 tokens · 09:00 · ",
    },
    {
      unit: "day",
      buckets: [{ start: "2026-08-12", done: 2, cancelled: 0, tokens_in: 300, tokens_out: 40 }],
      shipped: "2 shipped · Aug 12",
      tokens: "340 tokens · Aug 12 · ",
    },
    {
      unit: "week",
      buckets: [{ start: "2026-08-09", done: 3, cancelled: 0, tokens_in: 300, tokens_out: 40 }],
      shipped: "3 shipped · week of Aug 9",
      tokens: "340 tokens · week of Aug 9 · ",
    },
  ];

  for (const tc of cases) {
    const payload = statsPayload({ buckets: tc.buckets, window: { days: 1, unit: tc.unit, buckets: 1 } });
    const derived = ctx.statsDerive(payload);
    assert.equal(derived.throughput[0].tip, tc.shipped, tc.unit);
    assert.ok(derived.tokens[0].tip.startsWith(tc.tokens), derived.tokens[0].tip);

    state.statsDerived = derived;
    assert.equal(state.statsBucketUnit(), tc.unit);
  }

  // Before the first payload both titles still name a unit rather than reading
  // "tickets shipped per undefined".
  state.statsDerived = null;
  assert.equal(state.statsBucketUnit(), "week");

  // And the markup is what titles them: neither heading writes "week" itself.
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /x-text="'tickets shipped per ' \+ statsBucketUnit\(\)"/);
  assert.match(html, /x-text="'tokens per ' \+ statsBucketUnit\(\)"/);
});

test("both token tooltips break the same window into four categories", () => {
  const { ctx, state } = statsState();
  const payload = statsPayload();

  const derived = ctx.statsDerive(payload);
  state.statsDerived = derived;
  const card = state.statsCards().find((c) => c.label === "tokens");

  // tokens_in already holds both cache figures, so fresh input is 1000 and the
  // total stays in + out.
  const want = "1k fresh in \u00b7 50 cache write \u00b7 250 cache read \u00b7 200 out";
  assert.ok(derived.tokens[0].tip.includes(want), derived.tokens[0].tip);
  assert.ok(derived.tokens[0].tip.startsWith("1.5k tokens"), derived.tokens[0].tip);
  assert.ok(card.tip.includes(want), card.tip);
  assert.ok(card.tip.startsWith("1.5k tokens"), card.tip);
  assert.equal(card.value, "1.5k", "the card face still reports in + out");
  // The bar keeps its two segments: cache read would render as three slivers.
  assert.ok(derived.tokens[0].inH > 0 && derived.tokens[0].outH > 0);
  assert.equal(Object.keys(vmValue(derived.tokens[0])).sort().join(","), "inH,latest,outH,start,tip");
  // Only the tokens card has a tip, so the other five bind no attribute at all.
  assert.equal(state.statsCards().filter((c) => c.tip).length, 1);
  // And the markup is what renders it: nothing else in the KPI strip reads k.tip.
  assert.match(fs.readFileSync(htmlPath, "utf8"),
    /<div class="stats-card stats-kpi flex-col gap-1\.5" :data-tip-t="k\.tip">/);
});

test("a bucket with no tokens breaks down without a NaN", () => {
  const { ctx } = statsState();
  const payload = statsPayload();
  payload.buckets = [{ start: "2026-08-09", done: 0, cancelled: 0, tokens_in: 0, tokens_out: 0, tokens_cache_create: 0, tokens_cache_read: 0 }];

  const derived = ctx.statsDerive(payload);

  assert.ok(!derived.tokens[0].tip.includes("NaN"), derived.tokens[0].tip);
  assert.equal(derived.tokens[0].inH, 0);
  assert.equal(derived.tokens[0].outH, 0);
});

test("an agent with no usable token counts shows an em dash, not a zero", () => {
  const { ctx } = statsState();
  const payload = statsPayload();
  payload.agents = [
    { name: "pi", runs: 3, first_pass_pct: 66, median_ms: 60000, tokens_per_run: null, retries_per_ticket: 1.2 },
    { name: "claude", runs: 3, first_pass_pct: 90, median_ms: 60000, tokens_per_run: 0, retries_per_ticket: 0 },
  ];

  const derived = ctx.statsDerive(payload);

  assert.equal(derived.agents[0].perRun, "—");
  assert.equal(derived.agents[1].perRun, "0", "a measured zero is not the same as no measurement");
});

test("every stage row carries both a time and a token figure", () => {
  const { ctx } = statsState();

  const derived = ctx.statsDerive(statsPayload());

  assert.deepEqual(vmValue(derived.stages[0].time), {
    share: 60,
    value: "2m",
    sub: "p90 5m",
    meta: "2 runs · 0 failed",
    tip: "plan · 60% of measured time · p50 2m · p90 5m",
  });
  assert.deepEqual(vmValue(derived.stages[0].tokens), {
    share: 37.5,
    value: "300",
    // Labelled per run: the bold figure beside it is the stage total.
    sub: "p90/run 150",
    meta: "2 runs · 0 failed",
    tip: "plan · 38% of stage tokens · 300 over 2 measured runs",
  });
  assert.equal(derived.stages[1].tokens.value, "500");
  assert.equal(derived.stages[1].tokens.sub, "p90/run 500");
  // "in stages" against the KPI card's total, which also counts annotation runs.
  assert.equal(derived.stageTokens, "800 tokens in stages");
});

test("a stage only some of whose runs recorded counts says how many", () => {
  const { ctx } = statsState();

  const derived = ctx.statsDerive(statsPayload());

  // implement ran 4 times and one run wrote counts, so its 500 is a total over
  // that one run, not over the four the time mode counts.
  assert.equal(derived.stages[1].time.meta, "4 runs · 1 failed");
  assert.equal(derived.stages[1].tokens.meta, "1 of 4 runs measured · 1 failed");
});

test("the token order reranks the stage rows without recolouring them", () => {
  const { ctx } = statsState();

  const derived = ctx.statsDerive(statsPayload());

  assert.deepEqual(derived.stages.map((s) => s.name), ["plan", "implement"]);
  assert.deepEqual(derived.stagesByTokens.map((s) => s.name), ["implement", "plan"]);
  // The colour comes from the server's time order, so a row keeps it wherever
  // the token order puts it.
  const color = {};
  derived.stages.forEach((s) => { color[s.name] = s.color; });
  derived.stagesByTokens.forEach((s) => { assert.equal(s.color, color[s.name], s.name); });
  assert.notEqual(color.plan, color.implement);
});

test("a stage that recorded no counts reads as unmeasured, not as free", () => {
  const { ctx } = statsState();
  const payload = statsPayload();
  payload.stages[0] = { ...payload.stages[0], name: "pi", tokens: 0, tokens_p90: 0, token_share: 0, token_runs: 0 };

  const derived = ctx.statsDerive(payload);

  assert.equal(derived.stages[0].tokens.value, "—");
  assert.equal(derived.stages[0].tokens.sub, "no counts");
  assert.equal(derived.stages[0].tokens.share, 0, "it takes no width in the tokens bar");
  assert.equal(derived.stages[0].tokens.tip, "pi · no token counts recorded");
  assert.equal(derived.stages[0].time.value, "2m", "its time figures are untouched");
  // The measured stage still leads the token order.
  assert.equal(derived.stagesByTokens[0].name, "implement");
});

test("a window where nothing recorded counts empties the card instead of the bar", async () => {
  const payload = statsPayload();
  payload.stages = payload.stages.map((s) => ({ ...s, tokens: 0, tokens_p90: 0, token_share: 0, token_runs: 0 }));
  const { state } = statsState(payload);
  await state.gotoView("stats");
  await flushMicrotasks();

  assert.equal(state.statsStageRows().length, 2);
  assert.equal(state.statsStageCaption(), "median cycle 4h 12m");

  state.setStatsStageMode("tokens");

  // Every row would take a zero-width segment, so the card says so rather than
  // drawing an empty strip under "0 tokens in stages".
  assert.deepEqual(vmValue(state.statsStageRows()), []);
  assert.equal(state.statsStageEmpty(), "no token counts in this window");
  assert.equal(state.statsStageCaption(), "");
});

test("a window with no stages at all keeps the median cycle under the empty card", async () => {
  const { state } = statsState(statsPayload({ stages: [] }));
  await state.gotoView("stats");
  await flushMicrotasks();

  assert.equal(state.statsStageEmpty(), "no data in this window");
  assert.equal(state.statsStageCaption(), "median cycle 4h 12m", "a ticket figure outlives the stage rows");

  state.setStatsStageMode("tokens");

  assert.equal(state.statsStageEmpty(), "no data in this window");
  assert.equal(state.statsStageCaption(), "");
});

test("the stage mode persists and switches without another request", async () => {
  const { state, fetched, store } = statsState();
  await state.gotoView("stats");
  await flushMicrotasks();

  assert.equal(state.statsStageMode, "time");
  assert.deepEqual(state.statsStageRows().map((s) => s.name), ["plan", "implement"]);

  state.setStatsStageMode("tokens");
  await flushMicrotasks();

  assert.equal(store["kontora-stats-mode"], "tokens");
  assert.equal(fetched.length, 1, "both orders come out of the payload already held");
  assert.deepEqual(state.statsStageRows().map((s) => s.name), ["implement", "plan"]);

  state.setStatsStageMode("time");
  assert.deepEqual(state.statsStageRows().map((s) => s.name), ["plan", "implement"]);
  assert.equal(fetched.length, 1);

  assert.deepEqual(statsModeState({ getItem: () => "tokens", setItem() {} }).statsStageMode, "tokens",
    "reopening Stats comes back in the mode that was left");
});

test("a stage mode the storage cannot supply falls back to time", () => {
  const cases = [
    { name: "no key yet", storage: { getItem: () => null, setItem() {} } },
    { name: "a value the panel has no mode for", storage: { getItem: () => "minutes", setItem() {} } },
    { name: "storage that throws on read", storage: { getItem() { throw new Error("denied"); }, setItem() {} } },
  ];

  for (const c of cases) {
    assert.equal(statsModeState(c.storage).statsStageMode, "time", c.name);
  }
});

test("the stage panel's header carries the mode toggle and the rows follow it", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /x-text="statsStageMode === 'tokens' \? 'where the tokens go' : 'where the time goes'"/);
  assert.match(html, /<template x-for="m in statsStageModes" :key="m">/);
  assert.match(html, /@click="setStatsStageMode\(m\)"/);
  // The caption sits under the stacked bar: the title and the pill fill the header.
  assert.match(html, /<span class="stats-cap num" x-show="statsStageCaption\(\)" x-text="statsStageCaption\(\)">/);
  // It outlives the bar, which the empty window drops along with its segments.
  assert.match(html, /<span class="stats-cap" x-show="statsDerived && !statsStageRows\(\)\.length" x-text="statsStageEmpty\(\)">/);
  assert.match(html, /<div class="flex h-\[9px\] gap-0\.5" x-show="statsStageRows\(\)\.length">/);
  // Bar and rows read the same mode object, so neither can drift from the other.
  assert.match(html, /:style="'flex:' \+ s\[statsStageMode\]\.share \+ ';background:' \+ s\.color"/);
  assert.match(html, /x-text="s\[statsStageMode\]\.value"/);
  assert.match(html, /x-text="s\[statsStageMode\]\.sub"/);
  assert.match(html, /x-text="s\[statsStageMode\]\.meta"/);
  assert.equal(html.match(/x-for="s in statsStageRows\(\)"/g).length, 2);
});

test("the Stats poll runs only while Stats is the current view", async () => {
  const { state, timers, fetched } = statsState();

  await state.gotoView("stats");
  await flushMicrotasks();
  assert.equal(state.currentView, "stats");
  assert.equal(fetched.length, 1);
  assert.ok(fetched[0].startsWith("/api/stats?range="), fetched[0]);
  assert.equal(timers.made, 1);
  assert.equal(state.statsDerived.live.queued, 2);

  await state.gotoView("board");
  assert.deepEqual(timers.cleared, [1]);

  // Even if the cleared interval still fired, the fetch is guarded by the view.
  timers.fn();
  await flushMicrotasks();
  assert.equal(fetched.length, 1);
});

test("a dirty Settings form blocks the Stats sidebar item", async () => {
  const { state, fetched } = statsState();
  state.currentView = "settings";
  state.settingsDirty = () => true;

  await state.gotoView("stats");

  assert.equal(state.currentView, "settings");
  assert.equal(state.settingsGuard, true);
  assert.equal(state.settingsGuardTarget, "stats");
  assert.equal(fetched.length, 0);
});

test("the range persists and every filter change refetches", async () => {
  const { state, fetched, store } = statsState();
  await state.gotoView("stats");
  await flushMicrotasks();
  assert.ok(fetched[0].includes("range=90d"), fetched[0]);
  assert.ok(!fetched[0].includes("project="), "an unfiltered request omits the parameter");

  state.setStatsRange("30d");
  await flushMicrotasks();
  assert.equal(store["kontora-stats-range"], "30d");
  assert.ok(fetched[1].includes("range=30d"), fetched[1]);

  state.setStatsProject("kontora");
  state.setStatsPipeline("default");
  await flushMicrotasks();
  assert.ok(fetched[3].includes("project=kontora"), fetched[3]);
  assert.ok(fetched[3].includes("pipeline=default"), fetched[3]);

  // Setting the same value again is not a reason to hit the daemon.
  state.setStatsRange("30d");
  await flushMicrotasks();
  assert.equal(fetched.length, 4);
});

test("a KPI without the data behind it says so rather than reading zero", () => {
  const { ctx } = statsState();

  const empty = ctx.statsDerive({
    days: [], buckets: [], stages: [], agents: [], projects: [],
    live: {}, totals: {}, window: { days: 98, unit: "week", buckets: 14 },
  });
  const firstPass = empty.kpis.find((k) => k.label === "first-pass");
  assert.equal(firstPass.value, "—", "no stage run means the rate is unmeasured, not catastrophic");
  assert.equal(firstPass.delta, "no stage runs");
  assert.equal(firstPass.tone, "neutral");

  // Zero is a value the server emits whenever both windows shipped something.
  const flat = statsPayload();
  flat.totals.median_cycle_delta_ms = 0;
  const cycle = ctx.statsDerive(flat).kpis.find((k) => k.label === "median cycle");
  assert.equal(cycle.delta, "no change vs prev");

  // The server clamps the shipped count to the window, so on the one-day window
  // it is today's, not the week's.
  const day = statsPayload({ window: { days: 1, unit: "hour", buckets: 19, from: "2026-08-12", to: "2026-08-12" } });
  const shipped = ctx.statsDerive(day).kpis.find((k) => k.label === "shipped");
  assert.equal(shipped.delta, "+3 today");
});

test("the range chips are labelled with the window the server cuts", async () => {
  const { state } = statsState();

  // The two short windows are cut to the day, so they keep their own names.
  assert.deepEqual([...state.statsRanges].map((r) => state.statsRangeLabel(r)), ["1d", "1w", "5w", "14w", "26w"]);

  await state.gotoView("stats");
  await flushMicrotasks();
  assert.equal(state.statsWindowLabel().startsWith("last 98 days"), true, state.statsWindowLabel());

  // A window that opens mid-week spans one more Sunday bucket than its length,
  // so the caption may not be written in weeks.
  state.stats.window = { days: 182, unit: "week", buckets: 27 };
  assert.equal(state.statsWindowLabel().startsWith("last 182 days"), true, state.statsWindowLabel());

  state.stats.window = { days: 1, unit: "hour", buckets: 19 };
  assert.equal(state.statsWindowLabel().startsWith("last 1 day ·"), true, state.statsWindowLabel());
});

test("a 401 while Stats is open asks for the token instead of polling on", async () => {
  const timers = { made: 0, cleared: [], fn: null };
  const store = {};
  const { state } = loadKontoraContext({
    location: { protocol: "http:", host: "localhost:8080", hash: "" },
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = v; },
    },
    setInterval(fn) { timers.made++; timers.fn = fn; return timers.made; },
    clearInterval(id) { timers.cleared.push(id); },
    fetch: async () => ({ ok: false, status: 401, json: async () => ({}) }),
  });
  state.$nextTick = (cb) => { if (cb) cb(); return Promise.resolve(); };
  state.recomputeBoard = () => {};

  await state.gotoView("stats");
  await flushMicrotasks();

  assert.equal(state.needsAuth, true, "the login modal opens, the way every other fetch treats a 401");
  assert.equal(state.statsError, null, "not a red error string in the filter bar");
  assert.deepEqual(timers.cleared, [1], "the poll stops rather than hitting a dead session every 30s");
});

test("opening a ticket from Stats leaves the view and stops the poll", async () => {
  const { state, timers, fetched, location } = statsState();
  const statsCalls = () => fetched.filter((u) => u.startsWith("/api/stats")).length;
  state.tickets = [{ id: "kon-1", status: "todo" }];
  state.fetchChanges = () => {};
  state.fetchStageLogs = () => {};
  state.openTerminal = () => {};

  await state.gotoView("stats");
  await flushMicrotasks();
  assert.equal(statsCalls(), 1);

  // Browser Back to a ticket hash, or a pasted link. selectTicket does not
  // touch currentView, so applyRoute has to.
  location.hash = "#/t/kon-1";
  await state.applyRoute();

  assert.equal(state.selectedTicket.id, "kon-1");
  assert.equal(state.currentView, "board", "the rail covers Stats; closing it must reveal the board");
  assert.deepEqual(timers.cleared, [1]);
  timers.fn();
  await flushMicrotasks();
  assert.equal(statsCalls(), 1, "no further request after leaving Stats");
});

test("index.html adds the Stats nav item and loads the bundle as a classic script", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.ok(html.includes("gotoView('stats')"), "the sidebar navigates through the guarded gotoView");

  // Alpine is deferred, so it starts after this tag runs and kontora() is
  // there when it walks x-init. defer, async or type="module" on the tag all
  // reverse that and leave x-data unresolved; async only sometimes, which is
  // worse.
  const tags = [...html.matchAll(/<script([^>]*)src="\/app\.js"([^>]*)>/g)].map((m) => m[1] + m[2]);
  assert.equal(tags.length, 1, "one tag loads the bundle");
  assert.equal(/defer|async|type=/.test(tags[0]), false, tags[0]);
});

test("the Stats view paints from theme tokens, never a literal hex", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const view = html.slice(html.indexOf("<!-- Stats -->"), html.indexOf("<!-- Settings -->"));
  assert.ok(view.length > 1000, "the Stats view markup was found");

  const hex = /#[0-9a-fA-F]{6}\b/g;
  assert.deepEqual(view.match(hex) || [], [], "a literal hex in the markup is a light-theme bug");
  assert.deepEqual(fs.readFileSync(statsPath, "utf8").match(hex) || [], []);
});

test("a slow earlier request cannot overwrite the newest answer", async () => {
  const pending = [];
  const store = {};
  const { state } = loadKontoraContext({
    location: { protocol: "http:", host: "localhost:8080", hash: "" },
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = v; },
    },
    setInterval: () => 1,
    clearInterval() {},
    fetch: (url) => new Promise((resolve) => pending.push({ url, resolve })),
  });
  state.currentView = "stats";

  const stale = statsPayload();
  stale.totals.shipped = 111;
  const fresh = statsPayload();
  fresh.totals.shipped = 222;

  state.fetchStats();
  state.fetchStats();
  await flushMicrotasks();
  assert.equal(pending.length, 2);

  // The newer request answers first; the older one arrives after it.
  pending[1].resolve({ ok: true, json: async () => fresh });
  await flushMicrotasks();
  pending[0].resolve({ ok: true, json: async () => stale });
  await flushMicrotasks();

  assert.equal(state.stats.totals.shipped, 222);
  assert.equal(state.statsLoading, false);
  assert.equal(state.statsError, null);
});

test("the stage picker opens on every status but a running one", () => {
  const cases = [
    { status: "open", want: true },
    { status: "todo", want: true },
    { status: "paused", want: true },
    { status: "human_review", want: true },
    { status: "done", want: true },
    { status: "cancelled", want: true },
    { status: "in_progress", want: false },
    { status: "archived", want: false },
    { status: "closed", want: false },
    { status: "waiting", custom: ["waiting"], want: true },
    { status: "waiting", want: false },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = c.custom ? { custom_statuses: c.custom } : null;
    state.selectedTicket = { id: "kon-a", status: c.status, stage: "plan", stages: ["plan", "code"] };
    assert.equal(state.canSetStage(), c.want, `${c.status} custom=${c.custom || "none"}`);
  }

  // Nothing selected, nothing to move.
  assert.equal(loadKontoraState().canSetStage(), false);
});

test("the stage row is gated on the predicate and on the pipeline, not on the stage", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.equal(html.includes("['paused', 'open'].includes"), false, "the hardcoded status list is gone");
  // The row now hangs off the pipeline alone, so a ticket that gained one
  // through the detail form still gets somewhere to set its stage.
  assert.ok(html.includes(`<template x-if="selectedTicket?.stages?.length > 1"><span class="text-surface-600">stage</span></template>`));
  assert.ok(html.includes(`<template x-if="canSetStage() && setStageOpen">`));
  assert.ok(html.includes(`@click="if (canSetStage()) setStageOpen = true"`));
  assert.ok(html.includes(`x-text="displayStage()"`));
  // The pre-selected option is the stage on the file, never the display fallback:
  // a pick that matches the selection fires no change event.
  assert.ok(html.includes(`:selected="s === selectedTicket?.stage"`));
  assert.equal(html.includes(`:selected="s === displayStage()"`), false);
  assert.ok(html.includes(`<template x-if="!selectedTicket?.stage">`));
  assert.ok(html.includes(`<option value="" selected>set stage…</option>`));
});

test("an empty stage shows the pipeline's first stage without writing it", async () => {
  const calls = [];
  const { state } = loadKontoraContext({
    fetch: (url) => {
      calls.push(url);
      return Promise.resolve({ ok: true, json: async () => ({ id: "kon-a", status: "done", stage: "code" }) });
    },
  });
  state.selectedTicket = { id: "kon-a", status: "done", stage: "", stages: ["plan", "code"] };

  assert.equal(state.displayStage(), "plan");
  assert.deepEqual(calls, [], "reading the row must not write the ticket");

  // The picker's placeholder carries the selection, so its empty value must not
  // reach the endpoint.
  await state.setStage("");
  assert.deepEqual(calls, [], "the placeholder is not a stage");

  // Only the user's pick reaches the file.
  await state.setStage("code");
  assert.deepEqual(calls, ["/api/tickets/kon-a/set-stage"]);
  assert.equal(state.displayStage(), "code");
});

test("the detail panel's re-run button says restart on a finished ticket", () => {
  const state = loadKontoraState();
  const cases = [
    { status: "paused", idle: "retry", busy: "retrying..." },
    { status: "done", idle: "restart", busy: "restarting..." },
    { status: "cancelled", idle: "restart", busy: "restarting..." },
  ];

  for (const c of cases) {
    state.selectedTicket = { id: "kon-a", status: c.status };
    state.actionLoading = null;
    assert.equal(state.retryLabel(), c.idle, c.status);
    state.actionLoading = "retry";
    assert.equal(state.retryLabel(), c.busy, c.status);
  }

  const html = fs.readFileSync(htmlPath, "utf8");
  // One button, one endpoint, three statuses, and only on a ticket the daemon owns:
  // Service.Retry rejects a foreign one before it looks at the status.
  assert.ok(html.includes(`<template x-if="['paused', 'done', 'cancelled'].includes(selectedTicket?.status) && selectedTicket?.kontora">`));
  assert.ok(html.includes(`<button @click="action('retry')" :disabled="!!actionLoading"`));
  assert.ok(html.includes(`<span x-text="retryLabel()"></span>`));
});

test("the detail header renders one retry button, not the bespoke one plus Reopen", () => {
  const state = loadKontoraState();

  // The bespoke restart button covers the endpoint for every parked status, so
  // the validMoves list must not put a second /retry button next to it.
  for (const status of ["paused", "done", "cancelled"]) {
    const moves = state.detailMoves({ status: status, kontora: true });
    assert.equal(moves.some(mv => mv.endpoint === "retry"), false, status);
  }

  // Everything else on those statuses stays.
  assert.deepEqual([...state.detailMoves({ status: "done", kontora: true }).map(mv => mv.label)], ["Send to review"]);
  assert.deepEqual([...state.detailMoves({ status: "cancelled", kontora: true }).map(mv => mv.label)], []);
});

// ─── Waiting for input ──────────────────────────────────────────────────────

const waitingTicket = (extra) => ({
  id: "kon-wait", title: "Blocked", status: "in_progress", stage: "implement",
  pipeline: "kontora", path: "/x/proj", agent: "pi", attempt: 0, kontora: true,
  started_at: "2026-05-19T10:00:00Z",
  waiting_for_input: true,
  waiting_since: "2026-05-19T10:40:00Z",
  waiting_tool: "ask_user_question",
  waiting_question: "Which database should I use?",
  ...extra,
});

test("_cardHTML badges a waiting ticket with the tool, the question and the elapsed time", () => {
  const state = loadKontoraState();
  state.now = new Date("2026-05-19T11:05:00Z").getTime();

  const html = state._cardHTML(waitingTicket(), { key: "in_progress" });

  assert.match(html, /class="waiting-chip"/);
  assert.match(html, /needs input/);
  // The wrapping, width-capped tip, not the nowrap one built for timestamps: a
  // 200-byte question on one line runs off the side of the viewport.
  assert.match(html, /class="waiting-chip" data-tip="/);
  // The clock text is patched by _updateCardTimers through data-since, so the
  // attribute has to be the marker's own start, not the run's.
  assert.match(html, /data-since="2026-05-19T10:40:00Z"/);
  assert.match(html, />25m</);
  assert.match(html, /ask_user_question/);
  assert.match(html, /Which database should I use\?/);
});

test("_cardHTML leaves a card with no pending question unbadged", () => {
  const state = loadKontoraState();
  const html = state._cardHTML(waitingTicket({
    waiting_for_input: false, waiting_since: undefined,
    waiting_tool: undefined, waiting_question: undefined,
  }), { key: "in_progress" });

  assert.equal(/waiting-chip/.test(html), false);
  assert.equal(/needs input/.test(html), false);
});

test("_cardSig changes for every waiting field", () => {
  const state = loadKontoraState();
  const col = { key: "in_progress" };
  const sig = state._cardSig(waitingTicket(), col);

  const cases = [
    { name: "waiting_for_input", ticket: { waiting_for_input: false } },
    { name: "waiting_since", ticket: { waiting_since: "2026-05-19T10:50:00Z" } },
    { name: "waiting_tool", ticket: { waiting_tool: "question" } },
    { name: "waiting_question", ticket: { waiting_question: "Something else?" } },
  ];
  for (const c of cases) {
    assert.notEqual(state._cardSig(waitingTicket(c.ticket), col), sig,
      `${c.name} must change the card signature`);
  }
});

test("the waiting badge's elapsed text advances on the clock tick alone", () => {
  const { board, state, built } = renderedBoard([waitingTicket()]);
  state.now = new Date("2026-05-19T11:05:00Z").getTime();

  // A tick rebuilds no card: the duration is patched in place.
  state.recomputeBoard();
  assert.deepEqual(built, []);
  assert.deepEqual(board.ops, []);

  assert.equal(state.waitingFor(waitingTicket()), "25m");
  state.now = new Date("2026-05-19T12:40:00Z").getTime();
  assert.equal(state.waitingFor(waitingTicket()), "2h 0m");
});

test("waitingLabel names the tool, and falls back to it when there is no question", () => {
  const state = loadKontoraState();

  assert.equal(state.waitingLabel(waitingTicket()),
    "ask_user_question: Which database should I use?");
  assert.equal(state.waitingLabel(waitingTicket({ waiting_question: "" })), "ask_user_question");
  assert.equal(state.waitingLabel(waitingTicket({ waiting_tool: "" })), "a question: Which database should I use?");
  assert.equal(state.waitingLabel({ id: "x" }), "");
  assert.equal(state.waitingFor({ id: "x" }), "");
});

test("recomputeBoard tallies waiting tickets and the favicon ranks them over running ones", () => {
  const { ctx, state } = loadKontoraContext();
  state.tickets = [
    waitingTicket(),
    { id: "kon-run", title: "Running", status: "in_progress", kontora: true },
    // Not a kontora ticket: outside the tally, like every other status count.
    waitingTicket({ id: "kon-foreign", kontora: false }),
  ];
  state.recomputeBoard();

  assert.equal(state._statusCounts.needsInput, 1);
  assert.equal(state._statusCounts.in_progress, 2);

  state.updateFavicon();
  assert.equal(ctx.document.title, "(1 needs input) kontora");

  // "waiting" is a legal custom status. It must not be read as a pending
  // question, which is why the tally key is camelCase.
  state.tickets = [{ id: "kon-st", title: "Custom status", status: "waiting", kontora: true }];
  state.recomputeBoard();
  assert.equal(state._statusCounts.needsInput, 0);
  state.updateFavicon();
  assert.equal(ctx.document.title, "kontora");

  // With nothing waiting, the running count takes the title back.
  state.tickets = [{ id: "kon-run", title: "Running", status: "in_progress", kontora: true }];
  state.recomputeBoard();
  state.updateFavicon();
  assert.equal(ctx.document.title, "(1 running) kontora");
});

test("index.html badges a waiting ticket in the detail header and both mobile layers", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // Desktop detail header: beside the status chip in the identity block.
  assert.match(html, /x-show="selectedTicket\?\.waiting_for_input" class="waiting-chip"/);
  // Mobile board card and mobile detail header.
  assert.match(html, /x-if="ticket\.waiting_for_input"/);
  assert.match(html, /x-if="selectedTicket\?\.waiting_for_input"/);
  // Three badges, and each shows its own elapsed time.
  assert.equal(html.match(/class="waiting-chip"/g).length, 3);
  assert.equal(html.match(/waitingFor\((selectedTicket|ticket)\)/g).length, 3);
  // The chip reuses the review hue and the shared pulse animation.
  assert.match(html, /\.waiting-chip \{[^}]*--st-review/s);
  assert.match(html, /class="waiting-dot pulse-dot"/);
});

test("the board badge's question uses the wrapping tip, not the nowrap one", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const tips = fs.readFileSync(tipsPath, "utf8");

  // Any [data-tip], not only the round .tip button, so the badge can use it.
  assert.match(tips, /setupTip\('global-tip', '\[data-tip\]', 'data-tip'/);
  assert.match(html, /#global-tip \{[^}]*white-space: normal;[^}]*max-width: 260px/);
  // The timestamp variant is one unbounded line, which a 200-byte question
  // would run off the side of the viewport.
  assert.match(html, /#global-tip-t \{[^}]*white-space: nowrap/);
});

// ---------------------------------------------------------------------------
// Assistant pane
// ---------------------------------------------------------------------------

// assistantState builds the component over a storage and a fetch the caller
// controls, so a test can seed the persisted width, drive the poll, and read
// back what the pane sent.
function assistantState(opts = {}) {
  const store = Object.assign({}, opts.store);
  const requests = [];
  const listeners = [];
  // Every EventSource the pane opened, newest last.
  const streams = [];
  // The default frame runs its callback straight away, which leaves the tests
  // that predate the stream alone. opts.manualFrames queues them instead, so a
  // test can prove that three deltas paint once.
  const frames = [];
  // opts.manualTimers captures the reconnect backoff, so a test can drive it.
  const timers = [];
  // The default document holds no elements, so a test that cares about the
  // autoscroll hands one in and reads its scrollTop back.
  const scroller = opts.scroller;
  const routes = Object.assign(
    {
      "/api/assistant": { enabled: true, agent: "cl", kind: "claude", autonomy: "ask" },
      "/api/assistant/threads": { threads: [] },
    },
    opts.routes,
  );
  const { state } = loadKontoraContext({
    location: { protocol: "http:", host: "localhost:8080", hash: "" },
    EventSource: makeEventSourceFake(streams),
    ...(opts.manualTimers
      ? {
          setTimeout(fn) { timers.push(fn); return timers.length; },
          clearTimeout(id) { if (id) timers[id - 1] = null; },
        }
      : {}),
    ...(opts.manualFrames
      ? {
          requestAnimationFrame(cb) { frames.push(cb); return frames.length; },
          cancelAnimationFrame(id) { if (id) frames[id - 1] = null; },
        }
      : {}),
    ...(scroller
      ? { document: { getElementById: (id) => (id.startsWith("assistant") ? scroller : null), querySelector: () => null, hasFocus: () => true, documentElement: { style: {} } } }
      : {}),
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = v; },
    },
    window: {
      innerWidth: 1200,
      addEventListener(type, handler, capture) { listeners.push({ type, handler, capture }); },
      removeEventListener(type, handler) {
        const i = listeners.findIndex((l) => l.type === type && l.handler === handler);
        if (i >= 0) listeners.splice(i, 1);
      },
    },
    fetch: async (url, init) => {
      requests.push({ url, init });
      const key = Object.keys(routes).find((k) => url === k || url.startsWith(k + "?"));
      const body = key ? routes[key] : (opts.fallback || {});
      if (body && body.__status) return { ok: false, status: body.__status, json: async () => body };
      return { ok: true, status: 200, headers: { get: () => (opts.etag || "") }, json: async () => body };
    },
  });
  state.$nextTick = (cb) => { if (cb) cb(); return Promise.resolve(); };
  const runFrames = () => {
    for (const cb of frames.splice(0, frames.length)) if (cb) cb();
  };
  const runTimers = () => {
    for (const fn of timers.splice(0, timers.length)) if (fn) fn();
  };
  return { state, store, requests, listeners, scroller, streams, runFrames, runTimers };
}

test("the pane opens and closes, and remembers that it was open", async () => {
  const { state, store } = assistantState();
  await state.initAssistant();

  assert.equal(state.assistantOpen, false);
  state.toggleAssistant();
  assert.equal(state.assistantOpen, true);
  assert.equal(store["kontora-assistant-open"], "1");

  state.toggleAssistant();
  assert.equal(state.assistantOpen, false);
  assert.equal(store["kontora-assistant-open"], "0");

  // A second component over the same storage comes back open.
  const reopened = assistantState({ store: { "kontora-assistant-open": "1" } });
  assert.equal(reopened.state.assistantOpen, true);
});

test("the rail keeps the pane mounted at icon width", () => {
  const { state, store } = assistantState();
  state.toggleAssistant();
  state.toggleAssistantRail();

  assert.equal(state.assistantRail, true);
  assert.equal(state.assistantOpen, true, "the rail is collapsed, not closed");
  assert.equal(store["kontora-assistant-rail"], "1");

  // Toggling from the rail expands rather than closing, so ⌘J never hides a
  // running turn behind one keystroke the user did not mean.
  state.toggleAssistant();
  assert.equal(state.assistantRail, false);
  assert.equal(state.assistantOpen, true);
});

test("the pane width is clamped into 340-640 and persisted", () => {
  const { state, store } = assistantState();

  assert.equal(state.setAssistantWidth(200), 340);
  assert.equal(state.setAssistantWidth(9000), 640);
  assert.equal(state.setAssistantWidth(500), 500);
  assert.equal(store["kontora-assistant-width"], "500");
  // Junk in storage falls back rather than laying the pane out at NaN pixels.
  assert.equal(state.setAssistantWidth("nonsense"), 420);

  const restored = assistantState({ store: { "kontora-assistant-width": "9000" } });
  assert.equal(restored.state.assistantWidth, 640, "a stored width past the bound is clamped on read");
});

test("dragging the left edge widens the pane and persists once", () => {
  const { state, store, listeners } = assistantState();
  state.assistantWidth = 400;

  state.startAssistantResize({ clientX: 800 });
  const move = listeners.find((l) => l.type === "mousemove");
  const up = listeners.find((l) => l.type === "mouseup");

  // The pane is on the right, so dragging left widens it.
  move.handler({ clientX: 700 });
  assert.equal(state.assistantWidth, 500);
  assert.equal(store["kontora-assistant-width"], undefined, "a drag does not hammer storage");

  move.handler({ clientX: 100 });
  assert.equal(state.assistantWidth, 640, "the drag is clamped too");

  up.handler();
  assert.equal(store["kontora-assistant-width"], "640");
  assert.equal(listeners.filter((l) => l.type === "mousemove").length, 0, "the drag listeners are removed");
});

test("⌘J toggles the pane from the same capture-phase listener as ⌘K", () => {
  const listeners = [];
  const state = loadKontoraState({
    navigator: { platform: "MacIntel", clipboard: { writeText() {} } },
    window: { innerWidth: 1200, addEventListener(type, handler, capture) { listeners.push({ type, handler, capture }); } },
    document: { getElementById: () => null, querySelector: () => null, documentElement: { style: {} }, addEventListener() {} },
  });
  state.$nextTick = (cb) => { if (cb) cb(); return Promise.resolve(); };
  state.$refs = {};
  state._bindGlobalEvents();

  const captured = listeners.filter((l) => l.type === "keydown" && l.capture === true);
  assert.equal(captured.length, 1, "one listener carries both chords");

  let prevented = 0;
  let stopped = 0;
  const press = (init) => captured[0].handler({
    preventDefault() { prevented += 1; },
    stopPropagation() { stopped += 1; },
    ...init,
  });

  press({ key: "j", metaKey: true });
  assert.equal(state.assistantOpen, true);
  // The "j" must not reach the agent's stdin.
  assert.equal(prevented, 1);
  assert.equal(stopped, 1);

  press({ key: "j", metaKey: true });
  assert.equal(state.assistantOpen, false);

  // Ctrl+J on macOS is not ours, and neither chord fires on the phone layer.
  press({ key: "j", ctrlKey: true });
  assert.equal(state.assistantOpen, false);
  state.isMobile = true;
  press({ key: "j", metaKey: true });
  assert.equal(state.assistantOpen, false);
});

test("escape closes the pane only once nothing inside it is open", () => {
  const { state } = assistantState();
  state.toggleAssistant();

  state.assistantMenu = { kind: "slash", rows: [{ id: "a", insert: "x" }], index: 0, from: 0 };
  assert.equal(state.assistantEscape(), true);
  assert.equal(state.assistantMenu, null);
  assert.equal(state.assistantOpen, true, "the menu closed, not the pane");

  state.assistantAutonomyOpen = true;
  assert.equal(state.assistantEscape(), true);
  assert.equal(state.assistantAutonomyOpen, false);
  assert.equal(state.assistantOpen, true);

  state.assistantView = "history";
  assert.equal(state.assistantEscape(), true);
  assert.equal(state.assistantView, "thread");
  assert.equal(state.assistantOpen, true);

  assert.equal(state.assistantEscape(), true);
  assert.equal(state.assistantOpen, false);
  // A closed pane consumes nothing, so the body chain falls through to the
  // ticket detail behind it.
  assert.equal(state.assistantEscape(), false);
});

test("the body escape chain reaches the pane after the sheets and before the terminal", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const chain = html.match(/@keydown\.escape\.window="([^"]+)"/)[1];

  assert.ok(chain.indexOf("closeSheet()") < chain.indexOf("assistantEscape()"));
  assert.ok(chain.indexOf("assistantEscape()") < chain.indexOf("terminalRW"));
  // The palette is first, so ⎋ with both open closes the palette and leaves the
  // pane where it was.
  assert.ok(chain.indexOf("palettePop()") < chain.indexOf("assistantEscape()"));
});

test("the docked pane is desktop only and the phone gets the sheet instead", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.match(html, /x-if="!isMobile && assistantOpen && !assistantRail"/);
  assert.match(html, /x-if="!isMobile && assistantOpen && assistantRail"/);
  assert.match(html, /x-if="sheet\.type==='assistant'"/);
  // A flex sibling of the views rather than an overlay, so the board narrows
  // and stays visible.
  assert.match(html, /id="assistant-pane"[\s\S]{0,200}?shrink-0/);
  assert.equal(/id="assistant-pane"[\s\S]{0,300}?(fixed|absolute) inset-0/.test(html), false);
});

test("a tool row expands by the agent's own call id", () => {
  const { state } = assistantState();
  const rows = [
    { kind: "tool", id: "call_1", tool: "Bash", result: "one" },
    { kind: "tool", id: "call_2", tool: "Read", result: "two" },
  ];

  assert.equal(state.assistantToolExpanded(rows[0], 0), false);
  state.toggleAssistantTool(rows[0], 0);
  assert.equal(state.assistantToolExpanded(rows[0], 0), true);
  assert.equal(state.assistantToolExpanded(rows[1], 1), false, "one row opening does not open the next");

  // A row that moved index stays open: the key is the call id, not the slot.
  assert.equal(state.assistantToolExpanded(rows[0], 5), true);

  state.toggleAssistantTool(rows[0], 0);
  assert.equal(state.assistantToolExpanded(rows[0], 0), false);

  // A tape carrying no ids falls back to the index rather than collapsing every
  // row onto one key.
  const bare = { kind: "tool", tool: "Bash" };
  state.toggleAssistantTool(bare, 3);
  assert.equal(state.assistantToolExpanded(bare, 3), true);
  assert.equal(state.assistantToolExpanded(bare, 4), false);
});

test("a failed tool row reads as failed", () => {
  const { state } = assistantState();
  assert.equal(state.assistantToolFailed({ is_error: true }), true);
  assert.equal(state.assistantToolFailed({ is_error: false }), false);
  assert.equal(state.assistantToolFailed(null), false);
});

test("approving a parked write answers the gate and clears the card", async () => {
  const { state, requests } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.assistantGate = { id: "g1", tool: "Bash", arg: "kontora run kon-7d21", kind: "write" };

  await state.resolveAssistantGate(true);

  const sent = requests.find((r) => r.url === "/api/assistant/gate/g1");
  assert.equal(sent.init.method, "POST");
  assert.deepEqual(JSON.parse(sent.init.body), { decision: "approve" });
  // Cleared before the request lands, so a second click cannot answer twice.
  assert.equal(state.assistantGate, null);
});

test("skipping a parked write sends the refusal", async () => {
  const { state, requests } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.assistantGate = { id: "g9", tool: "Bash", arg: "kontora delete kon-7d21", kind: "delete" };

  await state.resolveAssistantGate(false);

  const sent = requests.find((r) => r.url === "/api/assistant/gate/g9");
  assert.deepEqual(JSON.parse(sent.init.body), { decision: "skip" });
  assert.equal(state.assistantGate, null);
});

test("a config reload re-reads the assistant payload without touching the open chat", async () => {
  let payload = { enabled: true, agent: "cl", kind: "claude", autonomy: "ask" };
  const { state, requests } = assistantState();
  // assistantState freezes its routes at build time, so the second payload —
  // the one the reload must pick up — is served from here instead.
  const seen = [];
  state._assistantFetch = async (url) => {
    seen.push(url);
    return { ok: true, status: 200, json: async () => payload };
  };
  await state.initAssistant();
  assert.equal(state.assistantEnabled(), true);

  state.assistantThread = { id: "t1" };
  state._assistantCursor = 12;
  state.assistantEvents = [{ kind: "text" }];

  payload = { enabled: false, hint: "Set assistant.agent in config.yaml." };
  await state._assistantRefreshConfig();

  assert.equal(state.assistantEnabled(), false);
  assert.equal(state.assistantAgent.hint, "Set assistant.agent in config.yaml.");
  assert.deepEqual(vmValue(state.assistantThread), { id: "t1" }, "the open chat survives a reload");
  assert.equal(state._assistantCursor, 12);
  assert.equal(state.assistantEvents.length, 1);
  assert.deepEqual(seen, ["/api/assistant", "/api/assistant"]);
  assert.equal(requests.length, 0);
});

test("a changed assistant.autonomy beats the stored picker, an unchanged one does not", async () => {
  // First run: nothing seeded, so the config value is adopted and recorded.
  const first = assistantState({ store: { "kontora-assistant-autonomy": "read" } });
  await first.state.initAssistant();
  assert.equal(first.state.assistantAutonomy, "ask");
  assert.equal(first.store["kontora-assistant-autonomy-seed"], "ask");

  // The user picks a mode by hand. The seed does not move with it, so the same
  // config value on the next refresh leaves the pick alone.
  first.state.setAssistantAutonomy("read");
  assert.equal(first.store["kontora-assistant-autonomy-seed"], "ask");
  await first.state._assistantRefreshConfig();
  assert.equal(first.state.assistantAutonomy, "read");

  // The file changes: the config wins over the manual pick.
  const changed = assistantState({
    store: { "kontora-assistant-autonomy": "read", "kontora-assistant-autonomy-seed": "ask" },
    routes: { "/api/assistant": { enabled: true, agent: "cl", kind: "claude", autonomy: "auto" } },
  });
  await changed.state.initAssistant();
  assert.equal(changed.state.assistantAutonomy, "auto");
  assert.equal(changed.store["kontora-assistant-autonomy"], "auto");
  assert.equal(changed.store["kontora-assistant-autonomy-seed"], "auto");

  // A thread carries its own mode, which the seed has no say over.
  changed.state._assistantAdopt({ id: "t1", autonomy: "ask" }, []);
  assert.equal(changed.state.assistantAutonomy, "ask");

  // Nor when the file changes under an open chat: the switch is that chat's,
  // and the next message would otherwise go out at a mode nobody picked. The
  // seed moves anyway, so the next new chat starts on the file's value.
  const busy = assistantState({
    store: { "kontora-assistant-autonomy": "ask", "kontora-assistant-autonomy-seed": "ask" },
    routes: { "/api/assistant": { enabled: true, agent: "cl", kind: "claude", autonomy: "auto" } },
  });
  busy.state.assistantThread = { id: "t1", autonomy: "ask" };
  await busy.state._assistantRefreshConfig();
  assert.equal(busy.state.assistantAutonomy, "ask");
  assert.equal(busy.store["kontora-assistant-autonomy-seed"], "auto");
});

test("a refresh that cannot reach the daemon leaves a working pane alone", async () => {
  const { state } = assistantState();
  await state.initAssistant();
  assert.equal(state.assistantEnabled(), true);

  state._assistantFetch = async () => { throw new Error("network"); };
  await state._assistantRefreshConfig();
  assert.equal(state.assistantEnabled(), true, "a blip must not swap the composer for the hint");

  // The first fetch has nothing to keep, so that one does become the hint.
  const cold = assistantState();
  cold.state._assistantFetch = async () => { throw new Error("network"); };
  await cold.state.initAssistant();
  assert.equal(cold.state.assistantEnabled(), false);
  assert.equal(cold.state.assistantAgent.hint, "The daemon did not answer.");
});

test("config_reloaded refreshes the assistant payload and the board's config cache", async () => {
  const listeners = {};
  const urls = [];
  const state = loadKontoraState({
    EventSource: class {
      addEventListener(name, fn) { listeners[name] = fn; }
      close() {}
    },
    fetch: async (url) => {
      urls.push(url);
      return { ok: true, status: 200, json: async () => ({ enabled: true, agent: "cl", autonomy: "auto" }) };
    },
  });
  state.connectSSE();

  listeners.config_reloaded({ data: "{}" });
  await flushMicrotasks();

  // The real handlers, not stubs: the two mixins that own them are merged into
  // the same object connectSSE runs on, and a rename would break this.
  assert.deepEqual(urls, ["/api/assistant", "/api/config"]);
  assert.equal(state.assistantAgent.agent, "cl");
  assert.equal(state.assistantAutonomy, "auto");
});

test("the autonomy switch persists and names itself", () => {
  const { state, store } = assistantState();

  assert.deepEqual(vmValue(state.assistantAutonomies().map((a) => a.key)), ["read", "ask", "auto"]);

  state.setAssistantAutonomy("read");
  assert.equal(state.assistantAutonomy, "read");
  assert.equal(state.assistantAutonomyLabel(), "read only");
  assert.equal(store["kontora-assistant-autonomy"], "read");
  assert.equal(state.assistantAutonomyOpen, false, "choosing a mode closes the popover");

  const restored = assistantState({ store: { "kontora-assistant-autonomy": "auto" } });
  assert.equal(restored.state.assistantAutonomy, "auto");
});

test("the page the message was sent from goes out with it", async () => {
  const { state, requests } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.selectedTicket = { id: "kon-7d21", status: "in_progress", stage: "review", title: "Fix the index", branch: "kontora/kon-7d21" };
  state.searchQuery = "project:=kontora fonts";
  state.assistantDraft = "what am I looking at";

  await state.sendAssistantMessage();

  const sent = requests.find((r) => r.url === "/api/assistant/threads/t1/messages");
  assert.deepEqual(JSON.parse(sent.init.body).context.split("\n"), [
    "Open ticket: kon-7d21 (in_progress, stage review)",
    "Title: Fix the index",
    "Branch: kontora/kon-7d21",
    "View: board",
    "Board filter: text \"fonts\", project kontora",
  ]);
});

test("a view the user is not on contributes nothing", async () => {
  const { state } = assistantState();
  await state.initAssistant();

  assert.equal(state.statsPageContext(), null, "the board is not the stats view");
  assert.equal(state.settingsPageContext(), null);
  assert.equal(state.detailPageContext(), null, "no ticket is open");
  assert.equal(state._assistantPageContext(), "View: board");
});

test("a ticket left open behind another view is not described as on screen", async () => {
  const { state } = assistantState();
  await state.initAssistant();
  state.selectedTicket = { id: "kon-7d21", status: "in_progress", stage: "review" };
  state.currentView = "stats";

  assert.equal(state.detailPageContext(), null);
  assert.equal(state._assistantPageContext().includes("kon-7d21"), false);
});

test("the context is trimmed to the byte cap the API enforces", async () => {
  const { state } = assistantState();
  await state.initAssistant();
  // Three bytes per character, so a filter that fits in 2048 UTF-16 units is
  // still well past the byte cap the handler rejects on.
  state.searchQuery = "мышь".repeat(400);

  const context = state._assistantPageContext();
  assert.ok(new TextEncoder().encode(context).length <= 2048);
});

test("the mode goes out with the message, so the switch takes effect next turn", async () => {
  const { state, requests } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.setAssistantAutonomy("auto");
  state.assistantDraft = "move kon-7d21 to done";

  await state.sendAssistantMessage();

  const sent = requests.find((r) => r.url === "/api/assistant/threads/t1/messages");
  assert.deepEqual(JSON.parse(sent.init.body), { text: "move kon-7d21 to done", autonomy: "auto", context: "View: board" });
  assert.equal(state.assistantDraft, "", "the composer is cleared");
  // The message is on screen before the daemon answers, so it does not vanish
  // while the turn is starting.
  assert.equal(state.assistantMessages.at(-1).text, "move kon-7d21 to done");
  assert.equal(state.assistantStreaming, true);
});

test("an empty draft sends nothing", async () => {
  const { state, requests } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.assistantDraft = "   ";

  await state.sendAssistantMessage();
  assert.equal(requests.some((r) => r.url.includes("/messages")), false);
});

test("the poll splices the new suffix onto the events already held", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };
  state.assistantEvents = [{ kind: "text", text: "a" }, { kind: "tool", tool: "Bash" }];

  // The daemon walked its cursor back over the tool row whose result arrived
  // late, so that row is rewritten rather than duplicated.
  state._mergeAssistantActivity({
    running: true,
    offset: 1,
    tape: { events: [{ kind: "tool", tool: "Bash", result: "done" }, { kind: "text", text: "b" }] },
  });

  assert.deepEqual(vmValue(state.assistantEvents.map((e) => e.kind)), ["text", "tool", "text"]);
  assert.equal(state.assistantEvents[1].result, "done");
  assert.equal(state._assistantCursor, 3);
  assert.equal(state.assistantStreaming, true);
});

// Alpine's string form of :style replaces the whole style attribute, so a
// static style beside it survives only while the binding evaluates to "". The
// sheet's send button spends most of its life in the enabled state, which is
// the branch that would come out unstyled.
test("the assistant send buttons do not mix a static style with a bound one", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  const tags = [...html.matchAll(/<button[^>]*sendAssistantMessage\(\)[^>]*>/g)].map((m) => m[0]);
  assert.equal(tags.length, 2, "expected the docked pane's send button and the sheet's");
  for (const tag of tags) {
    if (!tag.includes(":style=")) continue;
    assert.ok(!/(^|\s)style="/.test(tag), "a bound :style would blank this static style");
    assert.match(tag, /background:rgba\(var\(--accent\),1\)/, "the enabled state keeps the accent fill");
  }
});

test("the message just sent stays on screen until the daemon records it", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };
  state.assistantMessages = [{ n: 1, text: "first" }, { n: 2, text: "second" }];

  // A turn is only written to turns.jsonl once the agent exits, so mid-turn the
  // daemon's list is one message short. Dropping the message the user just
  // typed for the length of the turn is the bug this guards.
  state._mergeAssistantActivity({ running: true, tape: { events: [] }, messages: [{ n: 1, text: "first" }] });
  assert.deepEqual(vmValue(state.assistantMessages.map((m) => m.text)), ["first", "second"]);

  // The recorded list catches up and replaces it.
  state._mergeAssistantActivity({
    running: false,
    tape: { events: [] },
    messages: [{ n: 1, text: "first" }, { n: 2, text: "second", error: "boom" }],
  });
  assert.equal(state.assistantMessages.length, 2);
  assert.equal(state.assistantMessages[1].error, "boom");
});

// Two ordered streams, one thread. Before this the template rendered every
// message and then every event, so turn 2's question sat above turn 1's answer.
test("a message sits above the events its own turn produced", () => {
  const { state } = assistantState();
  state.assistantMessages = [
    { n: 1, text: "hey", at: "2026-08-23T08:25:50Z" },
    { n: 2, text: "what is in progress?", at: "2026-08-23T08:26:10Z" },
  ];
  state.assistantEvents = [
    { kind: "model", model: "claude-sonnet-5", time: "2026-08-23T08:25:52Z" },
    { kind: "text", text: "Hey.", time: "2026-08-23T08:25:53Z" },
    // No time of its own: grouped with the event before it.
    { kind: "tool", tool: "Bash", arg: "kontora ls" },
    { kind: "text", text: "Four tickets.", time: "2026-08-23T08:26:14Z" },
  ];

  assert.deepEqual(
    vmValue(state.assistantThreadRows()).map((r) => (r.msg ? "msg:" + r.msg.n : r.event.kind)),
    ["msg:1", "model", "text", "tool", "msg:2", "text"],
  );
  // The tool row keeps its index into assistantEvents, which is what keys its
  // expansion.
  assert.equal(state.assistantThreadRows()[3].index, 2);
});

test("the message just sent rides at the end until its turn writes anything", () => {
  const { state } = assistantState();
  state.assistantEvents = [{ kind: "text", text: "Hey.", time: "2026-08-23T08:25:53Z" }];
  state.assistantMessages = [
    { n: 1, text: "hey", at: "2026-08-23T08:25:50Z" },
    { n: 2, text: "thanks", at: "2026-08-23T08:26:40Z" },
  ];

  assert.deepEqual(
    vmValue(state.assistantThreadRows()).map((r) => (r.msg ? "msg:" + r.msg.n : r.event.kind)),
    ["msg:1", "text", "msg:2"],
  );
});

// A pi session whose clock this build cannot read leaves every event untimed.
// Nothing orders the two streams then, so the old layout stands rather than
// dropping the first message under the answer to it.
test("a tape with no timestamps keeps the messages ahead of the events", () => {
  const { state } = assistantState();
  state.assistantMessages = [{ n: 1, text: "hey", at: "2026-08-23T08:25:50Z" }];
  state.assistantEvents = [{ kind: "text", text: "Hey." }, { kind: "tool", tool: "Bash" }];

  assert.deepEqual(
    vmValue(state.assistantThreadRows()).map((r) => (r.msg ? "msg:" + r.msg.n : r.event.kind)),
    ["msg:1", "text", "tool"],
  );
});

test("every row carries its own key, so the x-for diff never collides", () => {
  const { state } = assistantState();
  state.assistantMessages = [{ n: 1, text: "hey", at: "2026-08-23T08:25:50Z" }];
  state.assistantEvents = [{ kind: "text", text: "Hey.", time: "2026-08-23T08:25:53Z" }];

  const keys = state.assistantThreadRows().map((r) => r.key);
  assert.equal(new Set(keys).size, keys.length);

  // The text being typed is deliberately not a row. Alpine re-runs an x-for
  // expression whenever a reactive dependency changes, so folding it in would
  // re-run the O(n) interleave and re-diff n keyed rows on every token.
  const before = state.assistantThreadRows().map((r) => r.key);
  state.assistantPartial = "half a sen";
  state.assistantPartialGen = 1;
  assert.deepEqual(state.assistantThreadRows().map((r) => r.key), before);
});

test("the typed text goes and the settled row arrives in one update", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };

  state._mergeAssistantActivity({ running: true, partial: "Two tickets", partial_gen: 1, tape: { events: [] } });
  assert.equal(state.assistantPartial, "Two tickets");
  assert.equal(state.assistantPartialTyping, true);

  // One synchronous call: no frame renders the words twice, and none renders
  // neither of them.
  state._mergeAssistantActivity({
    running: true,
    tape: { events: [{ kind: "text", text: "Two tickets are running." }] },
  });
  assert.equal(state.assistantPartial, "");
  assert.equal(state.assistantEvents.length, 1);
});

test("a poll snapshot behind the stream does not rewind the text", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };

  state._assistantAdoptPartial({ partial: "the board has two runs", partial_gen: 3 });
  // The poll read the daemon before the last deltas landed. Adopting it would
  // rub out words the reader has already seen.
  state._assistantAdoptPartial({ partial: "the board", partial_gen: 3 });
  assert.equal(state.assistantPartial, "the board has two runs");

  // A new block replaces wholesale, however short it is.
  state._assistantAdoptPartial({ partial: "Now", partial_gen: 4 });
  assert.equal(state.assistantPartial, "Now");
  assert.equal(state.assistantPartialGen, 4);
});

test("a finished turn always ends with nothing typed", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };
  state._assistantAdoptPartial({ partial: "half a sen", partial_gen: 1 });

  // The backstop: a payload that omits the field must not leave the caret
  // blinking under a thread that has stopped.
  state._mergeAssistantActivity({ running: false, tape: { events: [] } });
  assert.equal(state.assistantPartial, "");
  assert.equal(state.assistantPartialGen, 0);
  assert.equal(state.assistantPartialTyping, false);
});

test("a turn that dies mid-reply keeps the words on screen", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };
  state._mergeAssistantActivity({ running: true, partial: "half a sen", partial_gen: 1, tape: { events: [] } });

  // Stopped, or killed: the agent never wrote the message to its session file,
  // so what the daemon buffered is the only record of it.
  state._mergeAssistantActivity({ running: false, partial: "half a sen", partial_gen: 1, tape: { events: [] } });
  assert.equal(state.assistantPartial, "half a sen");
  assert.equal(state.assistantPartialTyping, false, "the caret stops");
  assert.equal(state.assistantStreaming, false);

  // The next turn opens a buffer of its own, and the daemon reports none.
  state._mergeAssistantActivity({ running: true, tape: { events: [] } });
  assert.equal(state.assistantPartial, "");
});

test("the poll alone types, with no stream anywhere", async () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };

  // Successive polls of a growing message. The transcript, the messages and
  // the gate are untouched by any of it.
  state._mergeAssistantActivity({ running: true, partial: "the ", partial_gen: 1, tape: { events: [] } });
  state._mergeAssistantActivity({ running: true, partial: "the board ", partial_gen: 1, tape: { events: [] } });
  state._mergeAssistantActivity({ running: true, partial: "the board has two runs", partial_gen: 1, tape: { events: [] } });
  assert.equal(state.assistantPartial, "the board has two runs");
  assert.equal(state.assistantEvents.length, 0);
  assert.equal(state.assistantGate, null);
});

test("the stream opens with the turn and closes with it", () => {
  const { state, streams } = assistantState();
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };

  state._assistantSetStreaming(true);
  assert.equal(streams.length, 1);
  assert.match(streams[0].url, /\/api\/assistant\/threads\/t1\/stream$/);

  state._assistantSetStreaming(false);
  assert.equal(streams[0].closed, true);
  assert.equal(state.assistantPartial, "");
});

test("a hidden pane holds no stream", () => {
  const { state, streams } = assistantState();
  state.assistantThread = { id: "t1" };

  // Closed, so nobody is reading the tokens and the connection is pure cost.
  state._assistantSetStreaming(true);
  assert.equal(streams.length, 0);

  state.assistantOpen = true;
  state.closeAssistant();
  state._assistantSetStreaming(true);
  assert.equal(streams.length, 0);
});

test("frames from a stream apply to the pane", () => {
  const { state, streams } = assistantState();
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 2, text: "the ", tool: "" });
  assert.equal(state.assistantPartial, "the ");
  assert.equal(state.assistantPartialGen, 2);
  assert.equal(state.assistantPartialTyping, true);

  es.emit("delta", { gen: 2, text: "board" });
  assert.equal(state.assistantPartial, "the board");

  // A delta for a block the client does not hold is not this message's.
  es.emit("delta", { gen: 9, text: " nonsense" });
  assert.equal(state.assistantPartial, "the board");

  es.emit("tool", { gen: 2, name: "Bash" });
  assert.equal(state.assistantPartialTool, "Bash");

  es.emit("end");
  assert.equal(state.assistantPartialTyping, false, "the caret stops and the text stays");
  assert.equal(state.assistantPartial, "the board");
});

test("three deltas paint once", () => {
  const { state, streams, runFrames } = assistantState({ manualFrames: true });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 1, text: "" });
  es.emit("delta", { gen: 1, text: "a" });
  es.emit("delta", { gen: 1, text: "b" });
  es.emit("delta", { gen: 1, text: "c" });
  // Nothing has reached the reactive property yet: the deltas are in a plain
  // buffer, so a burst of tokens is one paint rather than one each.
  assert.equal(state.assistantPartial, "");

  runFrames();
  assert.equal(state.assistantPartial, "abc");
  runFrames();
  assert.equal(state.assistantPartial, "abc", "the buffer is emptied by the frame that read it");
});

test("a poll landing between a delta and its frame does not double the tail", () => {
  const { state, streams, runFrames } = assistantState({ manualFrames: true });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 1, text: "hello" });
  es.emit("delta", { gen: 1, text: " world" });
  // The poll resolves before the frame runs, carrying the same words the
  // buffer holds. Appending the buffer on top of it would read "worldworld".
  state._mergeAssistantActivity({ running: true, partial: "hello world", partial_gen: 1, tape: { events: [] } });
  runFrames();
  assert.equal(state.assistantPartial, "hello world");

  // Same window, but the message reached the session file: the buffered delta
  // must not survive as a tail under the settled row.
  es.emit("delta", { gen: 1, text: "!" });
  state._mergeAssistantActivity({ running: true, tape: { events: [{ kind: "text", text: "hello world!" }] } });
  runFrames();
  assert.equal(state.assistantPartial, "");
});

test("a thread switch drops frames still in flight", async () => {
  const { state, streams } = assistantState({ routes: { "/api/assistant/threads/t2": { id: "t2" } } });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const stale = streams[0];
  stale.emit("reset", { gen: 1, text: "belongs to t1" });

  await state.assistantSelectThread("t2");
  assert.equal(stale.closed, true);

  // The old connection can still be mid-callback when the switch lands.
  stale.emit("delta", { gen: 1, text: " and must not appear" });
  assert.equal(state.assistantPartial, "");
});

test("a clean end of turn does not reconnect", () => {
  const { state, streams, runTimers } = assistantState({ manualTimers: true });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 1, text: "all done" });
  es.emit("done");
  assert.equal(es.closed, true);

  // The browser fires error on the server's EOF whether it was clean or not.
  // The poll has not said the turn is over yet, so without the close above
  // this would open a connection into a turn that has ended.
  es.fail();
  runTimers();
  assert.equal(streams.length, 1);
});

test("the stream stops retrying after five failures", () => {
  const { state, streams, runTimers } = assistantState({ manualTimers: true });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  assert.equal(streams.length, 1);

  for (let i = 0; i < 4; i++) {
    const es = streams[streams.length - 1];
    es.fail();
    // A clean server EOF does not set readyState to CLOSED: the browser fires
    // error and reconnects on its own, so onerror closes before it decides.
    assert.equal(es.closed, true);
    runTimers();
  }
  assert.equal(streams.length, 5, "the first connection and four retries");

  // The fifth consecutive failure opens nothing more for this turn: the poll
  // carries the text at its own granularity from here.
  streams[4].fail();
  runTimers();
  assert.equal(streams.length, 5);
});

test("streaming text follows the reader but leaves a scrolled-up reader alone", () => {
  const scroller = { scrollTop: 940, scrollHeight: 1000, clientHeight: 60 };
  const { state, streams } = assistantState({ scroller });
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 1, text: "a" });
  assert.equal(scroller.scrollTop, 1000);

  // The reader scrolls up. The latch is the scroll event, not a geometry read
  // on every frame: sampling per frame sees the grown scrollHeight against the
  // un-updated scrollTop and silently un-sticks the transcript mid-reply.
  scroller.scrollTop = 100;
  scroller.scrollHeight = 1400;
  state.assistantThreadScrolled();
  es.emit("delta", { gen: 1, text: "bbbb" });
  assert.equal(scroller.scrollTop, 100);

  // Scrolling back to the bottom resumes the follow.
  scroller.scrollTop = 1340;
  state.assistantThreadScrolled();
  es.emit("delta", { gen: 1, text: "cccc" });
  assert.equal(scroller.scrollTop, 1400);
});

test("deleting the open chat stops the pane saying it is working", async () => {
  const { state, streams } = assistantState();
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);

  await state.assistantDeleteThread("t1");
  assert.equal(state.assistantStreaming, false);
  assert.equal(streams[0].closed, true);
});

test("the tool being called is previewed in both copies and clears with the partial", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.equal([...html.matchAll(/x-if="assistantPartialTool"/g)].length, 2);
  assert.equal([...html.matchAll(/x-text="assistantPartialTool"/g)].length, 2);
  assert.match(html, /id="assistant-partial-tool"/);
  assert.match(html, /id="assistant-sheet-partial-tool"/);

  const { state, streams } = assistantState();
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };
  state._assistantSetStreaming(true);
  const es = streams[0];

  es.emit("reset", { gen: 1, text: "Running it." });
  es.emit("tool", { gen: 1, name: "Bash" });
  assert.equal(state.assistantPartialTool, "Bash");

  // Only the most recent, when a message makes several calls.
  es.emit("tool", { gen: 1, name: "Read" });
  assert.equal(state.assistantPartialTool, "Read");

  // A new block clears it, and so does the turn ending.
  es.emit("reset", { gen: 2, text: "Next.", tool: "" });
  assert.equal(state.assistantPartialTool, "");
  es.emit("tool", { gen: 2, name: "Grep" });
  state._assistantSetStreaming(false);
  assert.equal(state.assistantPartialTool, "");
});

test("the pending tool row clears when the message lands in the tape", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };

  state._mergeAssistantActivity({ running: true, partial: "Running it.", partial_gen: 1, partial_tool: "Bash", tape: { events: [] } });
  assert.equal(state.assistantPartialTool, "Bash");

  state._mergeAssistantActivity({ running: true, tape: { events: [{ kind: "text", text: "Running it." }] } });
  assert.equal(state.assistantPartial, "");
  assert.equal(state.assistantPartialTool, "");
});

test("both transcripts latch the scroll on their own scroll event", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.equal([...html.matchAll(/@scroll\.passive="assistantThreadScrolled\(\)"/g)].length, 2);
});

test("the text being typed is in both the docked pane and the sheet", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  const at = (needle) => [...html.matchAll(new RegExp(needle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g"))].map((m) => m.index);
  const rows = at('x-for="row in assistantThreadRows()"');
  const working = at('x-if="assistantStreaming && !assistantGate"');
  const partial = at('x-if="assistantPartial"');
  assert.equal(partial.length, 2);
  assert.equal(rows.length, 2);
  assert.equal(working.length, 2);
  for (let i = 0; i < 2; i++) {
    // Between the two, so the swap to the settled row does not move the text
    // sideways: both land immediately above the working row.
    assert.ok(rows[i] < partial[i] && partial[i] < working[i], "copy " + i + " has the partial out of order");
  }
  assert.match(html, /id="assistant-partial"/);
  assert.match(html, /id="assistant-sheet-partial"/);
  assert.equal([...html.matchAll(/x-text="assistantPartial"/g)].length, 2);
  assert.equal([...html.matchAll(/x-show="assistantPartialTyping"/g)].length, 2);

  // Plain text, not setProse: the markdown cache holds 16 entries keyed on
  // their source, and mid-stream markdown renders an unterminated fence as a
  // code block swallowing the rest of the reply.
  const block = html.slice(partial[0], working[0]);
  assert.ok(!block.includes("setProse"), "the typed text must not go through the markdown renderer");
});

test("the poll carries the parked write alongside the tape", () => {
  const { state } = assistantState();
  state.assistantThread = { id: "t1" };

  state._mergeAssistantActivity({ running: true, gate: { id: "g1", tool: "Bash" }, tape: { events: [] } });
  assert.equal(state.assistantGate.id, "g1");

  state._mergeAssistantActivity({ running: false, tape: { events: [] } });
  assert.equal(state.assistantGate, null);
  assert.equal(state.assistantStreaming, false);
});

test("the pane says it is working from the moment the message goes out", async () => {
  const { state } = assistantState();
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.assistantDraft = "what is running?";

  // The agent writes nothing for the first seconds of a turn, so without this
  // the thread is silent and the pane reads as dead.
  await state.sendAssistantMessage();
  assert.equal(state.assistantStreaming, true);
  assert.equal(state.assistantWorkingLabel(), "working… 0s");

  state.assistantElapsed = 75;
  assert.equal(state.assistantWorkingLabel(), "working… 1m 15s");

  state._mergeAssistantActivity({ running: false, tape: { events: [] } });
  assert.equal(state.assistantStreaming, false);
  assert.equal(state._assistantTurnStart, null, "the clock stops with the turn");
});

test("a turn the pane did not start is working with no count", () => {
  const { state } = assistantState();

  // Reopening onto a running turn: the pane has no start it can honestly count
  // from, so it says it is working and leaves the number off.
  state._assistantAdopt({ id: "t1", running: true }, []);
  assert.equal(state.assistantStreaming, true);
  assert.equal(state.assistantWorkingLabel(), "working…");
});

test("the thread follows the tape unless the reader has scrolled up", () => {
  const scroller = { scrollTop: 940, scrollHeight: 1000, clientHeight: 60 };
  const { state } = assistantState({ scroller });
  state.assistantThread = { id: "t1" };

  // At the bottom: the new events carry the reader along.
  state._mergeAssistantActivity({ running: true, tape: { events: [{ kind: "text", text: "a" }] } });
  assert.equal(scroller.scrollTop, 1000);

  // Scrolled up to read a tool row: the poll leaves the place alone.
  scroller.scrollTop = 100;
  scroller.scrollHeight = 1400;
  state._mergeAssistantActivity({ running: true, offset: 1, tape: { events: [{ kind: "text", text: "b" }] } });
  assert.equal(scroller.scrollTop, 100);
});

test("sending scrolls to the bottom whatever the reader was reading", async () => {
  const scroller = { scrollTop: 0, scrollHeight: 1000, clientHeight: 60 };
  const { state } = assistantState({ scroller });
  await state.initAssistant();
  state.assistantThread = { id: "t1" };
  state.assistantDraft = "hello";

  await state.sendAssistantMessage();
  assert.equal(scroller.scrollTop, 1000, "the message just typed is on screen");
});

test("the working row is in both the docked pane and the sheet", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // A parked write is its own card with its own buttons. Two live states at
  // once would read as the agent still working on what it has already stopped
  // for, so the row is off while the gate is up.
  // x-if, not x-show: Alpine's x-show strips the display property when it
  // shows an element again, and the sheet's row carries its display inline.
  assert.equal([...html.matchAll(/x-if="assistantStreaming && !assistantGate"/g)].length, 2);
  assert.equal([...html.matchAll(/assistantWorkingLabel\(\)/g)].length, 2);
  assert.match(html, /id="assistant-sheet-thread"/, "the sheet's thread is named so the autoscroll finds it");
});

test("the poll re-arms only while something can still change", () => {
  const { state } = assistantState();
  state.assistantOpen = true;
  state.assistantThread = { id: "t1" };

  state.assistantStreaming = true;
  state._armAssistantPoll();
  assert.notEqual(state._assistantPoll, null);
  state._stopAssistantPoll();

  // A finished turn with a write still waiting keeps polling: the answer is
  // what un-blocks the agent.
  state.assistantStreaming = false;
  state.assistantGate = { id: "g1" };
  state._armAssistantPoll();
  assert.notEqual(state._assistantPoll, null);
  state._stopAssistantPoll();

  state.assistantGate = null;
  state._armAssistantPoll();
  assert.equal(state._assistantPoll, null);

  // Closing the pane stops it whatever the turn is doing.
  state.assistantStreaming = true;
  state.closeAssistant();
  assert.equal(state._assistantPoll, null);
});

test("the slash menu opens on a leading slash and inserts the phrasing", () => {
  const { state } = assistantState();

  state.assistantDraft = "/st";
  state.assistantDraftChanged();
  assert.equal(state.assistantMenu.kind, "slash");
  assert.deepEqual(vmValue(state.assistantMenu.rows.map((r) => r.label)), ["/status", "/stuck"]);

  state.moveAssistantMenu(1);
  assert.equal(state.assistantMenu.index, 1);
  state.acceptAssistantMenu();
  assert.match(state.assistantDraft, /paused, failed/);
  assert.equal(state.assistantMenu, null);

  // A slash that matches nothing shows no menu rather than an empty one.
  state.assistantDraft = "/zzz";
  state.assistantDraftChanged();
  assert.equal(state.assistantMenu, null);
});

test("the mention menu ranks tickets and inserts the id", () => {
  const { state } = assistantState();
  state.tickets = [
    { id: "kon-7d21", title: "Assistant pane", status: "todo" },
    { id: "kon-9a02", title: "Bump deps", status: "done" },
  ];

  state.assistantDraft = "look at @bump";
  state.assistantDraftChanged();
  assert.equal(state.assistantMenu.kind, "mention");
  assert.deepEqual(vmValue(state.assistantMenu.rows.map((r) => r.label)), ["kon-9a02"]);

  state.acceptAssistantMenu();
  assert.equal(state.assistantDraft, "look at @kon-9a02 ");

  // A space after the @ closes it: the user moved on.
  state.assistantDraft = "look at @ something";
  state.assistantDraftChanged();
  assert.equal(state.assistantMenu, null);
});

test("enter sends unless a menu is open, and shift+enter is a newline", () => {
  const { state } = assistantState();
  let sent = 0;
  state.sendAssistantMessage = () => { sent += 1; };
  const press = (init) => {
    let prevented = 0;
    state.assistantComposerKey({ preventDefault() { prevented += 1; }, ...init });
    return prevented;
  };

  assert.equal(press({ key: "Enter" }), 1);
  assert.equal(sent, 1);

  assert.equal(press({ key: "Enter", shiftKey: true }), 0);
  assert.equal(sent, 1, "shift+enter types a newline");

  state.assistantDraft = "/st";
  state.assistantDraftChanged();
  assert.equal(press({ key: "Enter" }), 1);
  assert.equal(sent, 1, "enter picked the highlighted row instead of sending");
  assert.equal(state.assistantMenu, null);
});

test("the context strip says what a bare 'it' would mean", () => {
  const { state } = assistantState();
  state.selectedTicket = { id: "kon-7d21" };
  state.searchQuery = "project:kontora";

  assert.deepEqual(vmValue(state.assistantRecomputeContext().map((c) => c.label)), ["kon-7d21", "project:kontora"]);

  state.selectedTicket = null;
  assert.deepEqual(vmValue(state.assistantRecomputeContext().map((c) => c.label)), ["project:kontora"]);

  state.searchQuery = "  ";
  assert.deepEqual(vmValue(state.assistantRecomputeContext()), []);
});

test("no assistant agent replaces the composer with the configure hint", async () => {
  const { state } = assistantState({
    routes: { "/api/assistant": { enabled: false, hint: "Set assistant.agent to one of your claude or pi agents." } },
  });
  await state.initAssistant();

  assert.equal(state.assistantEnabled(), false);
  assert.equal(state.assistantAgent.hint, "Set assistant.agent to one of your claude or pi agents.");

  // Nothing can be sent, and opening the pane asks the daemon for no threads.
  state.assistantDraft = "hello";
  await state.sendAssistantMessage();
  assert.equal(state.assistantMessages.length, 0);

  // The two are complementary templates, so the composer is not merely hidden:
  // it is not built, and neither is the input the pane would otherwise focus.
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /x-if="!assistantEnabled\(\)"[\s\S]{0,200}?id="assistant-configure"/);
  const composer = html.slice(html.indexOf('<!-- Composer -->'));
  assert.match(composer.slice(0, 200), /x-if="assistantEnabled\(\)"/);
  assert.ok(composer.includes('id="assistant-input"'));
});

test("a daemon that never answers leaves the pane in the configure state", async () => {
  const { state } = assistantState({ routes: { "/api/assistant": { __status: 500 } } });
  await state.initAssistant();

  assert.equal(state.assistantEnabled(), false);
  assert.ok(state.assistantAgent.hint);
});

test("a remembered thread the daemon has dropped opens empty and quietly", async () => {
  const { state, store } = assistantState({
    store: { "kontora-assistant-thread": "gone" },
    fallback: { __status: 404 },
  });
  await state.initAssistant();

  assert.equal(state.assistantThread, null);
  assert.equal(state.assistantError, null, "a thread the user cannot see gone is not an error to show");
  assert.equal(store["kontora-assistant-thread"], "");
});
