import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { webcrypto } from "node:crypto";
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
      this.inputListeners = [];
      this.writes = [];
      this.operations = [];
      this.resizes = [];
      this.writeCallbacks = [];
      this.deferWrites = false;
      const register = kind => (id, handler) => {
        this.parserHandlers.push({ kind, id: vmValue(id), handler });
        return { dispose() {} };
      };
      this.parserHandlers = [];
      this.parser = {
        registerCsiHandler: register('csi'),
        registerDcsHandler: register('dcs'),
        registerOscHandler: register('osc'),
      };
      this.textarea = {
        readOnly: false,
        focuses: 0,
        blurs: 0,
        focus() { this.focuses += 1; },
        blur() { this.blurs += 1; },
      };
      created.push(this);
    }
    loadAddon(addon) { addon.term = this; }
    attachCustomKeyEventHandler(handler) {
      this.keyHandler = handler;
    }
    open(container) {
      this.element.parentNode = container;
    }
    reset() {
      this.resets += 1;
      this.operations.push(['reset']);
    }
    resize(cols, rows) {
      this.cols = cols;
      this.rows = rows;
      this.resizes.push([cols, rows]);
      this.operations.push(['resize', cols, rows]);
    }
    write(data, callback) {
      this.writes.push(data);
      this.operations.push(['write', typeof data === 'string' ? data : Buffer.from(data).toString()]);
      if (callback) {
        if (this.deferWrites) this.writeCallbacks.push(callback);
        else queueMicrotask(callback);
      }
    }
    input(data) {
      if (this.options.disableStdin) return;
      this.inputListeners.filter(listener => !listener.disposed).forEach(listener => listener.handler(data));
    }
    paste(text) {
      if (this.options.disableStdin) return;
      this.input('\x1b[200~' + text.replace(/\n/g, '\r') + '\x1b[201~');
    }
    onData(handler) {
      const listener = { handler, disposed: false, dispose() { this.disposed = true; } };
      this.inputListeners.push(listener);
      return listener;
    }
    focus() { this.textarea.focus(); }
    blur() { this.textarea.blur(); }
    scrollToBottom() {}
    dispose() {
      this.disposed = true;
    }
  };
  termState.Terminal.created = created;

  termState.FitAddon = class {
    constructor() {
      this.fits = 0;
      this.proposals = 0;
      this.dimensions = { cols: 80, rows: 24 };
    }
    proposeDimensions() {
      this.proposals += 1;
      return this.dimensions;
    }
    fit() {
      this.fits += 1;
      const dimensions = this.proposeDimensions();
      if (dimensions) this.term.resize(dimensions.cols, dimensions.rows);
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
async function openedTerminal(overrides = {}, { mobile = false } = {}) {
  const container = { textContent: "" };
  const { ctx, state } = loadKontoraContext({
    document: {
      getElementById(id) {
        return id === (mobile ? "terminal-container-mobile" : "terminal-session") ? container : null;
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
        this.sent = [];
      }
      send(data) { this.sent.push(JSON.parse(data)); }
      close() {
        this.closed = true;
        this.readyState = 3;
      }
    },
    ...overrides,
  });

  stubXterm(ctx.termState);
  state.isMobile = mobile;
  state.selectedTicket = { id: "tst-001", status: "in_progress" };
  state.activeTab = "session";
  state.detailTab = mobile ? "terminal" : "ticket";
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
    queueMicrotask,
    setTimeout,
    clearTimeout,
    structuredClone,
    crypto: webcrypto,
    TextEncoder,
    URLSearchParams,
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
      removeEventListener() {},
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
      querySelectorAll() { return []; },
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

  state.selectedTicket = { id: "tst-001", status: "in_progress" };
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

  state.selectedTicket = { id: "tst-001", status: "in_progress" };
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

  state.selectedTicket = { id: "tst-001", status: "in_progress" };
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

  state.selectedTicket = { id: "tst-001", status: "in_progress" };
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

for (const mobile of [false, true]) {
  test(`setting terminal permission is idempotent and keeps terminal handles: mobile=${mobile}`, async () => {
    const { ctx, state } = await openedTerminal({}, { mobile });
    const term = ctx.termState.term, webgl = ctx.termState.webgl, ro = ctx.termState.ws;
    state.setTerminalRW(false);
    assert.equal(ctx.termState.ws, ro, 'repeated RO does not reconnect');
    state.setTerminalRW(true);
    const rw = ctx.termState.ws;
    assert.notEqual(rw, ro);
    assert.equal(ro.closed, true);
    state.setTerminalRW(true);
    assert.equal(ctx.termState.ws, rw, 'repeated RW does not reconnect while connecting');
    rw.onopen();
    const listener = ctx.termState.inputDisposable;
    state.setTerminalRW(true);
    assert.equal(ctx.termState.ws, rw, 'repeated RW does not reconnect after opening');
    assert.equal(ctx.termState.inputDisposable, listener);
    assert.equal(state.terminalConnected, true);
    state.setTerminalRW(false);
    const next = ctx.termState.ws;
    assert.notEqual(next, rw);
    assert.equal(rw.closed, true);
    state.setTerminalRW(false);
    assert.equal(ctx.termState.ws, next);
    assert.equal(ctx.termState.term, term);
    assert.equal(ctx.termState.webgl, webgl);
    state.destroy();
  });
}

test('terminal permission can be requested before startup without opening a stream', () => {
  const state = loadKontoraState();
  const connects = [];
  state._connectWs = seq => connects.push(seq);
  state.setTerminalRW(true);
  state.setTerminalRW(true);
  assert.equal(state.terminalRW, true);
  assert.deepEqual(connects, []);
  state.toggleTerminalRW();
  assert.equal(state.terminalRW, false);
  assert.deepEqual(connects, []);
});

test("terminal key bar uses the current read-write stream without focus", async () => {
  const keys = [['Escape', '\x1b'], ['Tab', '\t'], ['Shift+Tab', '\x1b[Z'], ['Up', '\x1b[A'],
    ['Down', '\x1b[B'], ['Right', '\x1b[C'], ['Left', '\x1b[D'], ['Ctrl+C', '\x03'], ['Enter', '\r']];
  for (const mode of ['ready', 'read-only', 'connecting', 'stale-socket', 'stale-generation', 'closed-socket']) {
    const { ctx, state } = await openedTerminal({}, { mobile: true });
    const sent = [];
    if (mode !== 'read-only') state.toggleTerminalRW();
    const ws = ctx.termState.ws;
    ws.send = frame => sent.push(JSON.parse(frame));
    if (mode !== 'connecting') ws.onopen();
    if (mode === 'stale-socket') ctx.termState.ws = {};
    if (mode === 'stale-generation') state._terminalSeq++;
    if (mode === 'closed-socket') ws.readyState = 3;
    assert.deepEqual(vmValue(state.terminalKeys.map(key => [key.name, key.bytes])), keys);
    for (const [name, bytes] of keys) {
      state.sendTerminalKey(bytes);
      if (mode === 'ready') assert.deepEqual(sent.at(-1), { type: 'input', data: bytes }, name);
    }
    assert.equal(sent.length, mode === 'ready' ? keys.length : 0, mode);
    assert.equal(ctx.termState.term.textarea.focuses, 0, mode);
    assert.equal(state.terminalRW, mode !== 'read-only', mode);
  }
});

test("compose strips controls and sends Enter only to the captured session", async () => {
  const cases = [
    { name: 'multiline', text: 'первая\nвторая', clean: 'первая\rвторая' },
    { name: 'controls', text: 'a\x1b[201~b\x00\x03\x7f\x85\r\t\n', clean: 'a[201~b\t\r' },
    { name: 'socket replaced', replace: 'socket', noEnter: true },
    { name: 'terminal replaced', replace: 'terminal', noEnter: true },
    { name: 'input disabled', replace: 'read-only', noEnter: true },
    { name: 'disconnected', replace: 'disconnected', noEnter: true },
    { name: 'Done', replace: 'done', noEnter: true },
    { name: 'hardware Escape', replace: 'escape', noEnter: true },
    { name: 'generation replaced', replace: 'generation', noEnter: true },
    { name: 'connecting', before: 'connecting', noSend: true },
    { name: 'read-only', before: 'read-only', noSend: true },
    { name: 'empty', text: '\x1b\x00', noSend: true },
  ];
  for (const tc of cases) {
    const timers = [], sent = [];
    const { ctx, state } = await openedTerminal({ setTimeout(fn, ms) { timers.push({ fn, ms }); } }, { mobile: true });
    state.toggleTerminalRW();
    const ws = ctx.termState.ws;
    ws.send = frame => sent.push(JSON.parse(frame));
    if (tc.before !== 'connecting') ws.onopen();
    if (tc.before === 'read-only') state.toggleTerminalRW();
    timers.length = 0;
    assert.equal(state.sendTerminalCompose(tc.text ?? 'hello'), !tc.noSend, tc.name);
    const paste = '\x1b[200~' + (tc.clean ?? 'hello') + '\x1b[201~';
    assert.deepEqual(sent, tc.noSend ? [] : [{ type: 'input', data: paste }], tc.name);
    if (tc.noSend) { assert.equal(timers.length, 0, tc.name); continue; }
    assert.equal(timers.length, 1, tc.name);
    assert.equal(timers[0].ms, 150, tc.name);
    if (tc.replace === 'socket') {
      state.toggleTerminalRW();
      state.toggleTerminalRW();
      ctx.termState.ws.send = frame => sent.push(JSON.parse(frame));
      ctx.termState.ws.onopen();
      assert.notEqual(ctx.termState.ws, ws);
    }
    if (tc.replace === 'terminal') ctx.termState.term = {};
    if (tc.replace === 'read-only') state.toggleTerminalRW();
    if (tc.replace === 'disconnected') ws.onclose();
    if (tc.replace === 'done') state.mobileFinishTerminalInput();
    if (tc.replace === 'escape') ctx.termState.term.keyHandler({ type: 'keydown', key: 'Escape', preventDefault() {}, stopPropagation() {} });
    if (tc.replace === 'generation') state.closeTerminal();
    timers[0].fn();
    assert.deepEqual(sent, tc.noEnter ? [{ type: 'input', data: paste }] :
      [{ type: 'input', data: paste }, { type: 'input', data: '\r' }], tc.name);
    assert.equal(ctx.termState.term.textarea?.focuses || 0, 0, tc.name);
  }
});

test("phone terminal compact mode follows the selected ticket or supervisor session", () => {
  const cases = [
    { name: 'ticket composer', detailTab: 'terminal', target: 'composer', height: 900, compact: true },
    { name: 'ticket direct input', detailTab: 'terminal', target: 'terminal', height: 900, compact: true },
    { name: 'RW without input ownership', detailTab: 'terminal', rw: true, height: 900, compact: false },
    { name: 'ticket short viewport', detailTab: 'terminal', height: 360, compact: true },
    { name: 'ticket reading', detailTab: 'terminal', height: 900, compact: false },
    { name: 'supervisor composer', epic: true, epicTab: 'session', target: 'composer', height: 900, compact: true },
    { name: 'supervisor direct input', epic: true, epicTab: 'session', target: 'terminal', height: 900, compact: true },
    { name: 'supervisor short viewport', epic: true, epicTab: 'session', height: 360, compact: true },
    { name: 'supervisor reading', epic: true, epicTab: 'session', height: 900, compact: false },
    { name: 'supervisor activity', epic: true, epicTab: 'activity', detailTab: 'terminal', height: 360, compact: false },
    { name: 'ticket activity', detailTab: 'activity', height: 360, compact: false },
    { name: 'desktop', detailTab: 'terminal', mobile: false, rw: true, height: 360, compact: false },
  ];
  for (const tc of cases) {
    const state = loadKontoraState();
    const subject = { id: 'subject', status: 'in_progress', ...(tc.epic ? { kind: 'epic', epic_execution: 'agent' } : {}) };
    Object.assign(state, { isMobile: tc.mobile ?? true, selectedEpic: tc.epic ? subject : null, selectedTicket: tc.epic ? null : subject,
      epicTab: tc.epicTab, detailTab: tc.detailTab, terminalRW: !!tc.rw, mobileViewportHeight: tc.height,
      mobileTerminalInputTarget: tc.target ?? null });
    assert.equal(state.mobileTerminalCompact(), tc.compact, tc.name);
  }
});

test("terminal scrollback follows the layout that created it", async () => {
  for (const mobile of [true, false]) {
    const { ctx } = await openedTerminal({}, { mobile });
    assert.equal(ctx.termState.term.options.scrollback, mobile ? 0 : 5000);
    ctx.termState.term.options.disableStdin = true;
    const sent = [];
    ctx.termState.term.onData(data => sent.push(data));
    ctx.termState.term.input('ignored');
    ctx.termState.term.paste('ignored');
    assert.deepEqual(sent, []);
  }
});

test("terminal sockets opt into the pane stream in both input modes", async () => {
  const { ctx, state } = await liveTerminal();
  for (const rw of [false, true]) {
    if (rw) state.toggleTerminalRW();
    const url = new URL(ctx.termState.ws.url);
    assert.equal(url.pathname, '/ws/terminal/tst-001');
    assert.deepEqual(Object.fromEntries(url.searchParams), {
      cols: '80', rows: '24', stream: 'pane', ...(rw ? { rw: '1' } : {}),
    });
    assert.equal(ctx.termState.ws.binaryType, 'arraybuffer');
  }
  state.destroy();
});

test("the fitted grid stays until the first server reset", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, fit, ws } = ctx.termState;
  const resizes = term.resizes.length;
  fit.dimensions = { cols: 120, rows: 40 };
  state.refitTerminal();
  await flushMicrotasks();
  assert.deepEqual([term.cols, term.rows], [80, 24]);
  assert.equal(term.resizes.length, resizes);
  assert.equal(term.resets, 0);
  assert.deepEqual(ws.sent, [{ type: 'resize', cols: 120, rows: 40 }]);
  ws.onmessage({ data: '{"type":"reset","cols":110,"rows":35}' });
  await flushMicrotasks();
  assert.deepEqual([term.cols, term.rows], [110, 35], 'the pane size can differ from the proposed size');
  state.destroy();
});

test("a deferred initial fit cannot overwrite a reset from a racing input toggle", async () => {
  const frames = [];
  const { ctx, state } = await openedTerminal({
    requestAnimationFrame(callback) { frames.push(callback); return frames.length; },
  });
  state.toggleTerminalRW();
  ctx.termState.ws.onmessage({ data: '{"type":"reset","cols":110,"rows":35}' });
  await flushMicrotasks();
  frames.forEach(frame => frame());
  assert.equal(ctx.termState.fit.fits, 0);
  assert.deepEqual([ctx.termState.term.cols, ctx.termState.term.rows], [110, 35]);
  state.destroy();
});

test("pane resets wait for write callbacks and precede their binary snapshots", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws, fit } = ctx.termState;
  assert.equal(fit.fits, 1, 'the initial grid is fitted before connecting');
  assert.deepEqual([term.cols, term.rows], [80, 24]);
  term.operations.length = 0;
  term.deferWrites = true;
  const reset = (cols, rows) => ws.onmessage({ data: JSON.stringify({ type: 'reset', cols, rows }) });
  const output = text => ws.onmessage({ data: new TextEncoder().encode(text).buffer });

  reset(90, 30);
  output('snapshot');
  output('live');
  reset(100, 40);
  output('resized snapshot');
  await flushMicrotasks();
  assert.deepEqual(term.operations, [['reset'], ['resize', 90, 30], ['write', 'snapshot'], ['write', 'live']]);
  term.writeCallbacks.shift()();
  await flushMicrotasks();
  assert.deepEqual(term.operations.at(-1), ['write', 'live']);
  assert.deepEqual([term.cols, term.rows], [90, 30], 'the next reset waits for live output');
  term.writeCallbacks.shift()();
  await flushMicrotasks();
  assert.deepEqual(term.operations, [
    ['reset'], ['resize', 90, 30], ['write', 'snapshot'], ['write', 'live'],
    ['reset'], ['resize', 100, 40], ['write', 'resized snapshot'],
  ]);
  term.writeCallbacks.shift()();
  state.destroy();
});

test("consecutive binary frames batch inside xterm while write callbacks are pending", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws } = ctx.termState;
  term.deferWrites = true;
  const output = text => ws.onmessage({ data: new TextEncoder().encode(text).buffer });
  output('first');
  await flushMicrotasks();
  for (let i = 0; i < 27; i++) output(`snapshot-${i}`);
  await flushMicrotasks();
  output('live');
  await flushMicrotasks();
  assert.equal(term.writes.length, 29, 'no binary frame waits for an earlier write callback');
  assert.equal(term.writeCallbacks.length, 29);
  term.writeCallbacks.splice(0).forEach(callback => callback());
  state.destroy();
});

test("empty binary frames cannot block a later reset", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws } = ctx.termState;
  ws.onmessage({ data: new ArrayBuffer(0) });
  ws.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
  await flushMicrotasks();
  assert.deepEqual([term.cols, term.rows], [99, 31]);
  state.destroy();
});

test("a new stream reset waits for every write already batched in the reused terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws: old } = ctx.termState;
  term.deferWrites = true;
  for (const text of ['first', 'last']) {
    old.onmessage({ data: new TextEncoder().encode(text).buffer });
  }
  await flushMicrotasks();
  assert.equal(term.writeCallbacks.length, 2);
  state.toggleTerminalRW();
  const ws = ctx.termState.ws;
  ws.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
  ws.onmessage({ data: new TextEncoder().encode('new snapshot').buffer });
  term.writeCallbacks.shift()();
  await flushMicrotasks();
  assert.equal(term.resets, 0);
  term.writeCallbacks.shift()();
  await flushMicrotasks();
  assert.deepEqual(term.operations.slice(-3), [['reset'], ['resize', 99, 31], ['write', 'new snapshot']]);
  term.writeCallbacks.shift()();
  state.destroy();
});

test("received binary frames survive socket close before the queue drains", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws } = ctx.termState;
  for (const text of ['first output', 'final output']) {
    ws.onmessage({ data: new TextEncoder().encode(text).buffer });
  }
  ws.onclose();
  await flushMicrotasks();
  assert.deepEqual(term.writes.map(data => Buffer.from(data).toString()), ['first output', 'final output']);
  state.destroy();
});

for (const event of ['onclose', 'onerror']) {
  test(`received output survives ${event}, including frames behind a reset barrier`, async () => {
    const { ctx, state } = await liveTerminal();
    const { term, ws } = ctx.termState;
    term.operations.length = 0;
    term.deferWrites = true;
    const output = text => ws.onmessage({ data: new TextEncoder().encode(text).buffer });
    output('in flight');
    await flushMicrotasks();
    ws.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
    output('snapshot');
    output('final output');
    ws[event]();
    assert.equal(ctx.termState.ws, null);
    assert.equal(state.terminalConnected, false);
    assert.equal(state.terminalOpen, true);
    term.writeCallbacks.shift()();
    await flushMicrotasks();
    assert.deepEqual(term.operations, [
      ['write', 'in flight'], ['reset'], ['resize', 99, 31],
      ['write', 'snapshot'], ['write', 'final output'],
    ]);
    output('too late');
    term.writeCallbacks.splice(0).forEach(callback => callback());
    await flushMicrotasks();
    assert.equal(term.writes.length, 3, 'a closed socket cannot accept new output');
    state.destroy();
  });
}

for (const action of ['stream', 'replacement']) {
  test(`received output is invalidated by ${action} after socket close`, async () => {
    const { ctx, state } = await liveTerminal();
    const { term, ws } = ctx.termState;
    term.deferWrites = true;
    ws.onmessage({ data: new TextEncoder().encode('in flight').buffer });
    await flushMicrotasks();
    ws.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
    ws.onmessage({ data: new TextEncoder().encode('obsolete').buffer });
    ws.onclose();
    if (action === 'stream') state._disconnectStream();
    else {
      state._destroyTerminal();
      state._createTerminal(term.element.parentNode);
    }
    const resets = term.resets;
    term.writeCallbacks.shift()();
    await flushMicrotasks();
    assert.equal(term.resets, resets);
    assert.equal(term.writes.length, 1);
    state.destroy();
  });
}

test("unknown and malformed text frames leave the fitted grid unchanged", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, ws } = ctx.termState;
  const operations = term.operations.length;
  for (const data of ['not JSON', 'null', '{}', '{"type":"other"}',
    '{"type":"reset","cols":0,"rows":24}', '{"type":"reset","cols":80.5,"rows":24}',
    '{"type":"reset","cols":80}', '{"type":"reset","cols":80,"rows":-1}']) {
    ws.onmessage({ data });
  }
  await flushMicrotasks();
  assert.equal(term.operations.length, operations);
  assert.deepEqual([term.cols, term.rows], [80, 24]);
  state.destroy();
});

for (const action of ['toggle', 'ticket', 'reopen', 'disconnect']) {
  test(`old write callbacks cannot apply queued frames after ${action}`, async () => {
    const { ctx, state } = await liveTerminal();
    const { term, ws: old } = ctx.termState;
    term.deferWrites = true;
    old.onmessage({ data: new TextEncoder().encode('in flight').buffer });
    await flushMicrotasks();
    old.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
    old.onmessage({ data: new TextEncoder().encode('obsolete').buffer });
    const resets = term.resets;
    switch (action) {
      case 'toggle': state.toggleTerminalRW(); break;
      case 'ticket': await state.selectTicket({ id: 'tst-002', status: 'in_progress' }); break;
      case 'reopen': state.closeTerminal(); await state.openTerminal(); break;
      case 'disconnect': old.onclose(); state.reconnectTerminal(); break;
    }
    await flushMicrotasks();
    assert.equal(ctx.termState.term, term);
    const ws = ctx.termState.ws;
    assert.notEqual(ws, old);
    ws.onmessage({ data: '{"type":"reset","cols":120,"rows":40}' });
    ws.onmessage({ data: new TextEncoder().encode('current snapshot').buffer });
    await flushMicrotasks();
    assert.equal(term.resets, resets, 'even a reused terminal waits for the in-flight write');
    term.writeCallbacks.shift()();
    await flushMicrotasks();
    assert.equal(term.resizes.some(([cols]) => cols === 99), false);
    assert.deepEqual(term.writes.map(data => Buffer.from(data).toString()), ['in flight', 'current snapshot']);
    assert.deepEqual(term.operations.slice(-3), [['reset'], ['resize', 120, 40], ['write', 'current snapshot']]);
    old.onmessage({ data: '{"type":"reset","cols":99,"rows":31}' });
    term.writeCallbacks.shift()();
    await flushMicrotasks();
    assert.deepEqual([term.cols, term.rows], [120, 40]);
    state.destroy();
  });
}

test("refit requests proposed dimensions without resizing or clearing the terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const { term, fit, ws } = ctx.termState;
  ws.onmessage({ data: '{"type":"reset","cols":100,"rows":30}' });
  await flushMicrotasks();
  const operations = term.operations.length;
  fit.dimensions = { cols: 120, rows: 40 };
  state.refitTerminal();
  assert.deepEqual(ws.sent, [{ type: 'resize', cols: 120, rows: 40 }]);
  assert.equal(fit.fits, 1);
  assert.equal(term.operations.length, operations, 'no local write, reset or resize');
  assert.deepEqual([term.cols, term.rows], [100, 30]);
  fit.dimensions = undefined;
  state.refitTerminal();
  assert.equal(ws.sent.length, 1, 'an unmeasurable grid cannot request a size');
  state.destroy();
});

test("reply suppression is registered once and survives terminal reuse", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  const handlers = term.parserHandlers;
  assert.deepEqual(handlers.filter(h => h.kind === 'csi').map(h => h.id), [
    { final: 'c' }, { prefix: '>', final: 'c' },
    { final: 'n' }, { prefix: '?', final: 'n' },
    { intermediates: '$', final: 'p' }, { prefix: '?', intermediates: '$', final: 'p' },
  ]);
  assert.deepEqual(handlers.filter(h => h.kind === 'dcs').map(h => h.id), [{ intermediates: '$', final: 'q' }]);
  assert.deepEqual(handlers.filter(h => h.kind === 'osc').map(h => h.id), [4, 10, 11, 12]);
  for (const h of handlers.filter(h => h.kind !== 'osc')) assert.equal(h.handler(), true);
  for (const h of handlers.filter(h => h.kind === 'osc')) {
    const cases = h.id === 4 ? [
      ['0;?', true], ['0;?;15;?', true], ['0;rgb:ff/00/00', false],
      ['0;?;1;rgb:ff/00/00', false], ['0;rgb:ff/00/00;1;?', false], ['', false], ['0', false], ['0;?;1', false],
    ] : [['?', true], ['?;?', true], ['rgb:ff/00/00', false], ['?;rgb:ff/00/00', false], ['rgb:ff/00/00;?', false], ['', false]];
    for (const [data, want] of cases) assert.equal(h.handler(data), want, `OSC ${h.id}: ${data}`);
  }
  state.toggleTerminalRW();
  state.closeTerminal();
  await state.openTerminal();
  ctx.termState.ws.onmessage({ data: '{"type":"reset","cols":80,"rows":24}' });
  await flushMicrotasks();
  assert.equal(ctx.termState.term, term);
  assert.equal(term.parserHandlers.length, 11, 'reconnects and resets cannot add handlers');
  const ws = ctx.termState.ws;
  ws.onopen();
  term.inputListeners.at(-1).handler('\x1b[I');
  assert.deepEqual(ws.sent, [{ type: 'input', data: '\x1b[I' }], 'focus reports still reach the RW stream');
  state.destroy();
});

test("crossing the breakpoint rebuilds the terminal", async () => {
  const { ctx, state } = await liveTerminal();
  const term = ctx.termState.term;
  state._mountBoard = () => {};

  state._onBreakpointChange();

  assert.equal(ctx.termState.term, null);
  assert.equal(ctx.termState.webgl, null);
  await flushMicrotasks();
  assert.equal(term.disposed, true, "the old renderer is disposed after its queued viewport callbacks");
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
  const proposalsWhileVisible = ctx.termState.fit.proposals;

  ctx.termState.term.element.clientWidth = 0;
  state.refitTerminal();

  assert.equal(ctx.termState.fit.proposals, proposalsWhileVisible, "no size request for a hidden container");
});

for (const mobile of [false, true]) {
  for (const rw of [false, true]) {
    test(`terminal sends connection-time resizes on open: mobile=${mobile} rw=${rw}`, async () => {
      const cases = [
        { name: 'unchanged', sizes: [], want: [] },
        { name: 'columns changed', sizes: [[100, 24]], want: [{ type: 'resize', cols: 100, rows: 24 }] },
        { name: 'rows changed', sizes: [[80, 30]], want: [{ type: 'resize', cols: 80, rows: 30 }] },
        { name: 'resized', sizes: [[100, 30], [120, 40]], want: [{ type: 'resize', cols: 120, rows: 40 }] },
        { name: 'resized back', sizes: [[100, 30], [80, 24]], want: [] },
      ];
      for (const tc of cases) {
        const WebSocket = class {
          static OPEN = 1;
          constructor(url) {
            this.url = url;
            this.readyState = 0;
            this.sent = [];
          }
          send(data) { this.sent.push(JSON.parse(data)); }
          close() { this.readyState = 3; }
        };
        const { ctx, state } = await openedTerminal({ WebSocket }, { mobile });
        if (rw) state.toggleTerminalRW();
        const ws = ctx.termState.ws;
        const term = ctx.termState.term;
        assert.match(ws.url, /cols=80&rows=24/);
        for (const [cols, rows] of tc.sizes) {
          ctx.termState.fit.dimensions = { cols, rows };
          state.refitTerminal();
        }
        assert.deepEqual(ws.sent, [], `${tc.name}: nothing sent while connecting`);
        ws.readyState = WebSocket.OPEN;
        ws.onopen();
        assert.deepEqual(ws.sent, tc.want, `${tc.name}: latest size sent on open`);

        ctx.termState.fit.dimensions = { cols: 130, rows: 45 };
        state.refitTerminal();
        assert.deepEqual(ws.sent.at(-1), { type: 'resize', cols: 130, rows: 45 }, `${tc.name}: connected resize`);
        const sent = ws.sent.length;
        state.toggleTerminalRW();
        term.cols = 140;
        ws.readyState = WebSocket.OPEN;
        ws.onopen();
        assert.equal(ws.sent.length, sent, `${tc.name}: stale socket cannot send a resize`);
        state.destroy();
      }
    });
  }
}

async function mobileInputTerminal(subject, overrides = {}) {
  const sockets = [];
  const WebSocket = class {
    static OPEN = 1;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.sent = [];
      sockets.push(this);
    }
    send(data) { this.sent.push(JSON.parse(data)); }
    close() { this.readyState = 3; this.closed = true; }
  };
  if (subject === 'epic') {
    const ui = epicSupervisorUI({ mobile: true, overrides: { WebSocket, ...overrides } });
    await ui.state.openEpic(ui.epic.id);
    ui.state.epicWatchAgent();
    await flushMicrotasks();
    return { ...ui, sockets };
  }
  return { ...await openedTerminal({ WebSocket, ...overrides }, { mobile: true }), sockets };
}

test('mobile composer drafts follow output subject identity and stay in memory', () => {
  const state = loadKontoraState();
  state.isMobile = true;
  state.detailTab = 'terminal';
  const ticket = { id: 'same', status: 'in_progress' };
  state.selectedTicket = ticket;
  state.mobileComposeDraft = 'Ticket draft\nsecond line';
  assert.equal(state.mobileComposeDrafts['primary:same'], 'Ticket draft\nsecond line');
  state.selectedTicket = { id: 'other', status: 'in_progress' };
  assert.equal(state.mobileComposeDraft, '');
  state.mobileComposeDraft = 'Other draft';
  state.selectedTicket = { ...ticket, source: 'team' };
  assert.equal(state.mobileComposeDraft, '', 'secondary namesakes cannot read primary drafts');
  state.mobileComposeDraft = 'Rejected';
  assert.equal(state.mobileComposeDrafts['team:same'], undefined);
  state.selectedTicket = null;
  assert.equal(state.mobileComposeDraft, '');
  state.mobileComposeDraft = 'No subject';
  state.selectedTicket = ticket;
  assert.equal(state.mobileComposeDraft, 'Ticket draft\nsecond line');
  state.mobileFinishTerminalInput();
  assert.equal(state.mobileComposeDraft, 'Ticket draft\nsecond line');
  const epic = { id: 'supervisor', kind: 'epic', epic_execution: 'agent', status: 'in_progress' };
  state.selectedTicket = null;
  state.selectedEpic = epic;
  state.epicTab = 'session';
  state.mobileComposeDraft = 'Supervisor draft';
  state.selectedEpic = null;
  state.selectedTicket = { ...epic };
  assert.equal(state.mobileComposeDraft, 'Supervisor draft', 'supervisor overview and detail share a subject');
  assert.equal(loadKontoraState().mobileComposeDrafts['primary:same'], undefined, 'a new application has no old drafts');
});

for (const excluded of ['desktop', 'automation', 'other view', 'secondary', 'passive epic', 'completed', 'other tab', 'destroyed']) {
  test(`mobile input entry ignores excluded surface: ${excluded}`, () => {
    const state = loadKontoraState();
    Object.assign(state, { isMobile: true, detailTab: 'terminal', selectedTicket: { id: 'subject', status: 'in_progress' } });
    if (excluded === 'desktop') state.isMobile = false;
    if (excluded === 'automation') { state.currentView = 'automations'; state.automationsOutputSubject = () => ({ kind: 'automation', id: 'job', n: 1, status: 'in_progress' }); }
    if (excluded === 'other view') state.currentView = 'stats';
    if (excluded === 'secondary') state.selectedTicket.source = 'team';
    if (excluded === 'passive epic') Object.assign(state.selectedTicket, { kind: 'epic', epic_execution: 'passive' });
    if (excluded === 'completed') state.selectedTicket.status = 'done';
    if (excluded === 'other tab') state.detailTab = 'activity';
    if (excluded === 'destroyed') state._appDestroyed = true;
    assert.equal(state.mobileCanCompose(), false);
    state.setTerminalRW = () => assert.fail('excluded composer changed terminal permissions');
    state.mobileBeginTerminalCompose({});
    state.mobileBeginTerminalDirect();
    assert.equal(state.submitComposeMobile(), false);
    assert.equal(state.mobileTerminalInputTarget, null);
  });
}

async function inlineMobileTerminal(subject) {
  const dom = mobileLifecycleDOM();
  const host = dom.node({ class: 'mobile-terminal-host' });
  dom.elements[subject === 'epic' ? 'epic-terminal-mobile' : 'terminal-container-mobile'] = host;
  const computed = { getPropertyValue: () => '', visibility: 'visible', lineHeight: '22.4px', paddingTop: '10px', paddingBottom: '10px',
    borderTopWidth: '1px', borderBottomWidth: '1px' };
  const ui = await mobileInputTerminal(subject, { ...dom.overrides, getComputedStyle: () => computed });
  dom.runFrame();
  const dock = dom.node({ class: 'mobile-terminal-dock' });
  const composer = dom.node({ 'data-mobile-terminal-input': '', class: 'mobile-terminal-compose' }, dock);
  composer.scrollHeight = 42;
  const textarea = dom.node({ class: 'xterm-helper-textarea' }, host);
  ui.ctx.termState.term.textarea = textarea;
  ui.state._setTerminalInput(false);
  ui.state.$refs = { [subject === 'epic' ? 'mobileEpicTerminalCompose' : 'mobileTerminalCompose']: composer };
  ui.state.initMobile();
  ui.state.automationsDispose = () => {};
  return { ...ui, dom, composer, textarea, host, dock, computed };
}

for (const subject of ['ticket', 'epic']) {
  test(`mobile ${subject} composer owns input synchronously before connection and transfers without reconnecting`, async () => {
    const { state, ctx, sockets, dom, composer, textarea } = await inlineMobileTerminal(subject);
    composer.focus();
    state.mobileBeginTerminalCompose(composer);
    assert.equal(state.mobileTerminalInputTarget, 'composer');
    assert.equal(state.mobileTerminalTyping(), true);
    assert.equal(state.terminalRW, true);
    assert.equal(state.terminalConnected, false);
    assert.equal(dom.document.activeElement, composer);
    const rw = ctx.termState.ws, count = sockets.length;
    state.mobileComposeDraft = 'first\nsecond';
    assert.equal(state.submitComposeMobile(), false);
    state.sendTerminalKey('\r');
    ctx.termState.term.input('not queued');
    assert.deepEqual(rw.sent, []);
    assert.equal(state.mobileComposeDraft, 'first\nsecond');
    rw.readyState = 1;
    rw.onopen();
    assert.equal(dom.document.activeElement, composer, 'opening a socket cannot blur the native composer');
    assert.equal(composer.focuses.length, 1, 'opening a socket cannot focus the composer again');
    assert.equal(textarea.focuses.length, 0, 'opening a socket cannot focus xterm');
    assert.deepEqual(rw.sent, []);
    state.mobileBeginTerminalCompose(composer);
    state.mobileBeginTerminalDirect();
    assert.equal(dom.document.activeElement, composer, 'direct entry waits for a user tap on xterm');
    textarea.focus();
    dom.document.emit('focusin', { target: textarea });
    assert.equal(state.mobileTerminalInputTarget, 'terminal');
    composer.focus();
    state.mobileBeginTerminalCompose(composer);
    assert.equal(state.mobileTerminalInputTarget, 'composer');
    assert.equal(ctx.termState.ws, rw);
    assert.equal(sockets.length, count, 'focus transfer cannot replace the writable socket');
    assert.equal(state.mobileComposeDraft, 'first\nsecond');
    state.mobileFinishTerminalInput();
    const ro = ctx.termState.ws;
    state.mobileFinishTerminalInput();
    assert.equal(ctx.termState.ws, ro, 'Done is idempotent');
    assert.equal(state.mobileTerminalInputTarget, null);
    assert.equal(state.terminalRW, false);
    assert.equal(composer.blurs, 1);
    assert.equal(state.mobileComposeDraft, 'first\nsecond');
    state.destroy();
  });

  test(`mobile ${subject} inline Send clears only a locally accepted draft and retains focus`, async () => {
    const { state, ctx, composer, dom } = await inlineMobileTerminal(subject);
    composer.focus();
    state.mobileBeginTerminalCompose(composer);
    const ws = ctx.termState.ws;
    state.mobileComposeDraft = 'local\ntext';
    assert.equal(state.submitComposeMobile(), false);
    assert.equal(state.mobileComposeDraft, 'local\ntext');
    ws.readyState = 1;
    ws.onopen();
    const other = 'primary:unrelated';
    state.mobileComposeDrafts[other] = 'Other subject draft';
    state.mobileComposeDraft = '\x00\x1b';
    assert.equal(state.submitComposeMobile(), false, 'sanitation can reject a nonempty draft');
    assert.equal(state.mobileComposeDraft, '\x00\x1b');
    state.mobileComposeDraft = 'local\ntext';
    const key = state.terminalSubjectKey;
    state.terminalSubjectKey = 'primary:stale';
    assert.equal(state.submitComposeMobile(), false, 'Send cannot target another subject socket');
    assert.equal(state.mobileComposeDraft, 'local\ntext');
    state.terminalSubjectKey = key;
    assert.equal(state.submitComposeMobile(), true);
    assert.deepEqual(ws.sent, [{ type: 'input', data: '\x1b[200~local\rtext\x1b[201~' }]);
    assert.equal(state.mobileComposeDraft, '');
    assert.equal(state.mobileComposeDrafts[other], 'Other subject draft');
    assert.equal(composer.style.height, '44px');
    assert.equal(dom.document.activeElement, composer);
    assert.equal(composer.focuses.length, 1);
    assert.equal(state.sheet, null, 'inline Send never opens or closes a sheet');
    state.mobileFinishTerminalInput();
    state.destroy();
  });

  for (const action of ['disconnect', 'error', 'teardown', 'breakpoint']) {
    test(`mobile ${subject} native composer blurs and retains its draft on ${action}`, async () => {
      const { state, ctx, sockets, dom, composer } = await inlineMobileTerminal(subject);
      composer.focus();
      state.mobileBeginTerminalCompose(composer);
      state.mobileComposeDraft = 'Unsent native draft';
      const key = state.outputSubjectKey(), count = sockets.length, ws = ctx.termState.ws;
      if (action === 'disconnect') ws.onclose();
      if (action === 'error') ws.onerror();
      if (action === 'teardown') state.closeTerminal();
      if (action === 'breakpoint') {
        state.isMobile = false;
        state._mountBoard = () => {};
        state._onBreakpointChange();
      }
      assert.equal(state.terminalRW, false);
      assert.equal(state.mobileTerminalInputTarget, null);
      assert.equal(dom.document.activeElement, null);
      assert.equal(composer.blurs, 1);
      assert.equal(state.mobileComposeDrafts[key], 'Unsent native draft');
      assert.equal(sockets.length, count, 'cleanup does not open another stream');
      if (action === 'breakpoint') assert.ok(sockets.every(socket => socket.closed), 'no stream outlives the old layout');
      if (action === 'disconnect' || action === 'error') {
        assert.equal(state.terminalOpen, true, 'the terminal and Retry remain available');
        assert.match(state.terminalError, /disconnected.*Retry/);
        state.mobileFinishTerminalInput();
        assert.equal(ctx.termState.ws, null, 'late Done does not reconnect');
      }
      state.destroy();
    });
  }

  test(`mobile ${subject} composer height caps at four lines and recalculates after viewport changes`, async () => {
    const { state, dom, composer, computed } = await inlineMobileTerminal(subject);
    for (const [height, expected] of [[20, 44], [70, 72], [500, 111.6], [42, 44]]) {
      composer.scrollHeight = height;
      state.mobileResizeCompose(composer);
      assert.equal(composer.style.height, expected + 'px');
    }
    composer.scrollHeight = 80;
    dom.window.emit('resize');
    dom.runFrame();
    assert.equal(composer.style.height, '82px');
    computed.lineHeight = '24px';
    composer.scrollHeight = 500;
    state.mobileResizeCompose(composer);
    assert.equal(composer.style.height, '118px', 'the cap uses measured font metrics and borders');
    state.destroy();
  });

  test(`mobile ${subject} input waits for the current RW socket without focusing or queueing`, async () => {
    const { ctx, state, sockets } = await mobileInputTerminal(subject);
    const term = ctx.termState.term;
    assert.equal(term.options.disableStdin, true, 'RO terminal cannot accept input');
    assert.equal(term.textarea.readOnly, true, 'RO taps cannot open the keyboard');
    sockets[0].readyState = 1;
    sockets[0].onopen();
    assert.equal(term.textarea.readOnly, true, 'RO readiness does not enable input');

    state.toggleTerminalRW();
    const rw = ctx.termState.ws;
    const listener = term.inputListeners.at(-1);
    assert.match(rw.url, /rw=1/);
    assert.equal(term.options.disableStdin, true, 'connecting is still read-only');
    assert.equal(term.textarea.readOnly, true);
    listener.handler('before open');
    rw.readyState = 1;
    listener.handler('before callback');
    assert.deepEqual(rw.sent, []);
    term.focus();
    const connectingBlurs = term.textarea.blurs;
    rw.onopen();
    assert.ok(term.textarea.blurs > connectingBlurs, 'a tap during connection cannot retain focus when input becomes ready');
    assert.equal(state.terminalConnected, true);
    assert.equal(term.options.disableStdin, false);
    assert.equal(term.textarea.readOnly, false, 'the next terminal tap can open the keyboard');
    assert.equal(term.textarea.focuses, 1, 'socket readiness cannot focus the textarea');
    assert.deepEqual(rw.sent, [], 'input typed before readiness is not replayed');
    listener.handler('ready');
    assert.deepEqual(rw.sent, [{ type: 'input', data: 'ready' }]);

    term.focus();
    const blurs = term.textarea.blurs;
    state.toggleTerminalRW();
    assert.equal(state.terminalRW, false);
    assert.equal(listener.disposed, true);
    assert.ok(term.textarea.blurs > blurs, 'ending input dismisses the keyboard');
    assert.equal(term.options.disableStdin, true);
    assert.equal(term.textarea.readOnly, true);
    assert.doesNotMatch(ctx.termState.ws.url, /rw=1/);
    rw.readyState = 1;
    listener.handler('late input');
    rw.onopen();
    assert.equal(term.textarea.readOnly, true);
    assert.equal(state.terminalConnected, false);
    assert.equal(rw.sent.length, 1);
    state.destroy();
  });

  for (const action of ['stream', 'teardown', 'close', 'error', 'tab', 'selection', 'detail', 'breakpoint', 'escape', 'composer Escape', 'completion', 'navigation']) {
    test(`mobile ${subject} input resets on ${action} and stale callbacks stay inert`, async () => {
      const { ctx, state, sockets } = await mobileInputTerminal(subject);
      state.mobileBeginTerminalDirect();
      state.mobileComposeDraft = 'Retained unsent draft';
      const subjectKey = state.outputSubjectKey();
      const socketCount = sockets.length;
      const term = ctx.termState.term;
      const rw = ctx.termState.ws;
      rw.readyState = 1;
      rw.onopen();
      const listener = term.inputListeners.at(-1);
      term.focus();
      const blurs = term.textarea.blurs;
      switch (action) {
        case 'composer Escape': state.mobileFinishTerminalInput(); break;
        case 'completion': state.applyTicketUpdate({ ...state.outputSubject(), status: 'done' }); break;
        case 'navigation': await state.gotoView('stats'); break;
        case 'stream': state._disconnectStream(); break;
        case 'teardown': state.closeTerminal(); break;
        case 'close': rw.onclose(); break;
        case 'error': rw.onerror(); break;
        case 'tab':
          if (subject === 'epic') state.epicSwitchTab('children');
          else state.mobileSwitchTab('ticket');
          break;
        case 'selection':
          if (subject === 'epic') await state.openEpic(state.selectedEpic.id);
          else await state.selectTicket({ id: 'tst-002', status: 'in_progress' });
          break;
        case 'detail':
          if (subject === 'epic') await state.closeEpic();
          else state.closeDetail();
          break;
        case 'breakpoint':
          state.isMobile = false;
          state._mountBoard = () => {};
          state._onBreakpointChange();
          break;
        case 'escape': {
          let stopped = false;
          assert.equal(term.keyHandler({ type: 'keydown', key: 'Escape', preventDefault() {}, stopPropagation() { stopped = true; } }), false);
          assert.equal(stopped, true);
          assert.ok(state.outputSubject(), 'Escape leaves the detail open');
          break;
        }
      }
      await flushMicrotasks();
      assert.equal(state.terminalRW, false);
      assert.equal(state.mobileTerminalInputTarget, null);
      assert.equal(state.mobileComposeDrafts[subjectKey], 'Retained unsent draft');
      if (['stream', 'teardown', 'close', 'error'].includes(action)) {
        assert.equal(sockets.length, socketCount, 'cleanup must not reconnect the stream');
      }
      assert.equal(term.textarea.readOnly, true);
      assert.equal(term.options.disableStdin, true);
      assert.ok(term.textarea.blurs > blurs);
      assert.equal(listener.disposed, true, 'old input listener is disposed');
      const connected = state.terminalConnected;
      const writes = term.writes.length;
      rw.readyState = 1;
      rw.onopen();
      rw.onmessage({ data: new ArrayBuffer(0) });
      rw.onclose();
      rw.onerror();
      listener.handler('stale');
      assert.equal(state.terminalConnected, connected);
      assert.equal(term.textarea.readOnly, true);
      assert.deepEqual(rw.sent, []);
      assert.equal(term.writes.length, writes, 'old output cannot write into the reused terminal');
      assert.ok(sockets.filter(ws => !ws.closed).every(ws => !ws.url.includes('rw=1')));
      state.destroy();
    });
  }

  test(`mobile ${subject} reconnects RO after a disconnect and ignores a replaced RW socket`, async () => {
    const { ctx, state } = await mobileInputTerminal(subject);
    state.mobileBeginTerminalDirect();
    state.mobileComposeDraft = 'Keep this subject draft';
    const first = ctx.termState.ws;
    const term = ctx.termState.term;
    const listener = term.inputListeners.at(-1);
    state.toggleTerminalRW();
    state.toggleTerminalRW();
    const current = ctx.termState.ws;
    state.mobileBeginTerminalDirect();
    assert.equal(ctx.termState.ws, current, 'ownership changes do not replace the RW stream');
    assert.equal(state.mobileTerminalInputTarget, 'terminal');
    first.readyState = 1;
    first.onopen();
    listener.handler('replaced');
    assert.equal(state.terminalConnected, false);
    assert.equal(term.textarea.readOnly, true);
    assert.deepEqual(first.sent, []);
    current.readyState = 1;
    current.onopen();
    first.onclose();
    first.onerror();
    assert.equal(state.terminalConnected, true);
    assert.equal(term.textarea.readOnly, false);
    assert.equal(state.mobileTerminalInputTarget, 'terminal', 'stale disconnects cannot end current input');
    current.onclose();
    assert.equal(state.terminalRW, false);
    assert.equal(state.mobileTerminalInputTarget, null);
    assert.equal(state.mobileComposeDraft, 'Keep this subject draft');
    state.mobileFinishTerminalInput();
    assert.equal(ctx.termState.ws, null, 'Done after disconnect cannot reconnect or enable input');
    state.reconnectTerminal();
    await flushMicrotasks();
    assert.doesNotMatch(ctx.termState.ws.url, /rw=1/);
    assert.equal(ctx.termState.term, term, 'reconnect reuses xterm');
    state.destroy();
  });
}

for (const region of ['dock', 'host', 'bar', 'outside', 'unrelated blur']) for (const socketOpens of [false, true]) {
  test(`deferred terminal focusout reconciles the new focus in ${region}, socket opens=${socketOpens}`, async () => {
    const { state, ctx, dom, composer, dock, host } = await inlineMobileTerminal('ticket');
    composer.focus();
    state.mobileBeginTerminalCompose(composer);
    const rw = ctx.termState.ws;
    const ticks = [];
    state.$nextTick = cb => ticks.push(cb);
    dom.document.emit('focusout', { target: region === 'unrelated blur' ? dom.node({}) : composer });
    assert.equal(state.mobileTerminalInputTarget, 'composer', 'focusout must wait for the next active element');
    const parent = region === 'dock' ? dock : region === 'host' ? host
      : region === 'bar' ? dom.node({ class: 'mobile-terminal-bar' }) : null;
    const next = dom.node({}, parent);
    next.focus();
    dom.document.emit('focusin', { target: next });
    if (socketOpens) {
      rw.readyState = 1;
      rw.onopen();
    }
    ticks.splice(0).forEach(cb => cb());
    assert.equal(state.terminalRW, region !== 'outside');
    if (region !== 'outside') assert.equal(ctx.termState.ws, rw);
    else assert.equal(state.mobileTerminalInputTarget, null);
    state.destroy();
  });
}

for (const boundary of ['subject', 'teardown', 'breakpoint', 'lifecycle', 'destroyed', 'new input']) {
  test(`deferred terminal focusout cannot clear newer ownership after ${boundary}`, async () => {
    const { state, ctx, dom, composer } = await inlineMobileTerminal('ticket');
    state.mobileBeginTerminalCompose(composer);
    const ticks = [];
    state.$nextTick = cb => ticks.push(cb);
    state._mobileTerminalFocusout({ target: composer });
    if (boundary === 'subject') state.selectedTicket = { id: 'new', status: 'in_progress' };
    if (boundary === 'teardown') state.closeTerminal();
    if (boundary === 'breakpoint') state.mobileBreakpointChanged();
    if (boundary === 'lifecycle') { state.destroyMobile(); state.initMobile(); }
    if (boundary === 'destroyed') state._appDestroyed = true;
    if (boundary !== 'destroyed') state.mobileBeginTerminalDirect();
    const target = state.mobileTerminalInputTarget, socket = ctx.termState.ws;
    dom.document.activeElement = null;
    ticks.splice(0).forEach(cb => cb());
    assert.equal(state.mobileTerminalInputTarget, target);
    assert.equal(ctx.termState.ws, socket);
    state._appDestroyed = false;
    state.destroy();
  });
}

test('terminal input cleanup never blurs the description editor', async () => {
  const { state, dom, composer, textarea } = await inlineMobileTerminal('ticket');
  const description = dom.node({ id: 'mobile-description-editor' });
  state.mobileComposeDraft = 'Keep draft';
  for (const active of [composer, textarea, description]) {
    active.focus();
    state.mobileResetTerminalInput();
    assert.equal(dom.document.activeElement, active === description ? description : null);
  }
  assert.equal(description.blurs, 0);
  assert.equal(state.mobileComposeDraft, 'Keep draft');
  state.destroy();
});

for (const mobile of [false, true]) {
  test(`replaced input listeners cannot send to an old socket: mobile=${mobile}`, async () => {
    const { ctx, state } = await openedTerminal({}, { mobile });
    const term = ctx.termState.term;
    state.toggleTerminalRW();
    const first = ctx.termState.ws;
    first.onopen();
    const listener = term.inputListeners.at(-1);
    const sent = [];
    first.send = data => sent.push(JSON.parse(data));
    listener.handler('current');
    assert.equal(sent.length, 1);

    state._connectWs(state._terminalSeq - 1);
    assert.equal(listener.disposed, false, 'a stale connect cannot remove the current listener');
    assert.equal(ctx.termState.ws, first);
    state._connectWs(state._terminalSeq);
    assert.equal(listener.disposed, true);
    const current = ctx.termState.ws;
    current.onopen();
    first.readyState = 1;
    listener.handler('stale');
    first.onopen();
    first.onclose();
    first.onerror();
    assert.equal(sent.length, 1, 'socket identity matters even within the same generation');
    assert.equal(ctx.termState.ws, current);
    assert.equal(state.terminalConnected, true);
    assert.equal(term.inputListeners.filter(l => !l.disposed).length, 1);

    state.closeTerminal();
    assert.equal(state.terminalRW, !mobile, 'desktop keeps its existing mode across stream teardown');
    assert.equal(term.inputListeners.filter(l => !l.disposed).length, 0);
    await state.openTerminal();
    assert.equal(ctx.termState.term, term);
    assert.equal(ctx.termState.ws.url.includes('rw=1'), !mobile);
    state.destroy();
  });
}

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
    { name: "a project token", q: "project:kontora", want: { text: "", project: [tok("kontora")], agent: [], epic: [] } },
    { name: "an agent token", q: "agent:Claude", want: { text: "", project: [], agent: [tok("claude")], epic: [] } },
    { name: "an exact token", q: "project:=kontora", want: { text: "", project: [tok("kontora", true)], agent: [], epic: [] } },
    // A name with a space in it only survives the term split quoted, which is
    // how the sidebar writes it.
    { name: "a quoted value", q: 'project:="my notes"', want: { text: "", project: [tok("my notes", true)], agent: [], epic: [] } },
    { name: "quoted free text", q: '"two words"', want: { text: "two words", project: [], agent: [], epic: [] } },
    { name: "a token plus free text", q: "project:Kontora Fonts", want: { text: "fonts", project: [tok("kontora")], agent: [], epic: [] } },
    { name: "two tokens", q: "project:kontora agent:claude", want: { text: "", project: [tok("kontora")], agent: [tok("claude")], epic: [] } },
    { name: "a repeated key", q: "project:kontora project:widget-api", want: { text: "", project: [tok("kontora"), tok("widget-api")], agent: [], epic: [] } },
    // Between the colon and the first typed character the key constrains
    // nothing, rather than emptying the board.
    { name: "a bare prefix", q: "project:", want: { text: "", project: [], agent: [], epic: [] } },
    { name: "a bare exact prefix", q: "project:=", want: { text: "", project: [], agent: [], epic: [] } },
    { name: "a bare prefix and free text", q: "project: fonts", want: { text: "fonts", project: [], agent: [], epic: [] } },
    { name: "an unknown prefix", q: "foo:bar", want: { text: "foo:bar", project: [], agent: [], epic: [] } },
    { name: "an epic token", q: "epic:kon-e7c1", want: { text: "", project: [], agent: [], epic: [tok("kon-e7c1")] } },
    { name: "an empty box", q: "  ", want: { text: "", project: [], agent: [], epic: [] } },
  ];

  for (const c of cases) {
    assert.deepEqual(vmValue(state.parseFilterQuery(c.q)), c.want, c.name);
  }

  // An unknown prefix is matched as the literal it looks like.
  assert.equal(state.ticketMatchesQuery({ title: "see foo:bar" }, "foo:bar"), true);
  assert.equal(state.ticketMatchesQuery({ title: "see bar" }, "foo:bar"), false);

  // epic: reads the child's parent, so it narrows the board to one epic's work
  // without the epic itself having to be on it.
  const child = { title: "a child", parent: { id: "kon-e7c1" } };
  assert.equal(state.ticketMatchesQuery(child, "epic:kon-e7c1"), true);
  assert.equal(state.ticketMatchesQuery(child, "epic:kon-9999"), false);
  assert.equal(state.ticketMatchesQuery({ title: "no parent" }, "epic:kon-e7c1"), false);
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

  // Hold derived work while every event updates the model.
  state._boardRaf = 1;
  state.queueTicketUpdate({ id: "kon-1", title: "One", status: "todo", kontora: true });
  state.queueTicketUpdate({ id: "kon-2", title: "Two", status: "todo", kontora: true });

  assert.equal(state._boardDirty, true);
  assert.equal(state.tickets.length, 2);
  assert.equal('_pendingTicketUpdates' in state, false);
  assert.equal(recomputes, 0);

  state.flushTicketUpdates();

  assert.equal(recomputes, 1);
  assert.equal(state._boardDirty, false);
  assert.equal(state._boardRaf, null);
  assert.deepEqual(bt(state, "in_progress").map(t => t.id).sort(), ["kon-1", "kon-2"]);
});

test("queueTicketUpdate applies immediately and refreshes the cached board", () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.tickets = [];

  // The harness runs requestAnimationFrame synchronously, so this flushes now.
  state.queueTicketUpdate({ id: "kon-1", title: "One", status: "human_review", kontora: true, updated_at: "2026-05-19T09:00:00Z" });

  assert.deepEqual(bt(state, "human_review").map(t => t.id), ["kon-1"]);
  assert.equal(state._boardDirty, false);
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

const CARD_PROGRESS_TICKET = {
  id: "kon-progress", title: "Pipeline progress", kontora: true,
  status: "in_progress", stage: "code", stages: ["plan", "code", "review"],
};
const CARD_PROGRESS_CASES = [
  { name: "first of three", patch: { stage: "plan" }, percent: 17, label: "Stage 1 of 3: plan" },
  { name: "middle of three", patch: {}, percent: 50, label: "Stage 2 of 3: code" },
  { name: "last of three", patch: { stage: "review" }, percent: 83, label: "Stage 3 of 3: review" },
  { name: "first of two", patch: { stage: "plan", stages: ["plan", "code"] }, percent: 25, label: "Stage 1 of 2: plan" },
  { name: "last of two", patch: { stages: ["plan", "code"] }, percent: 75, label: "Stage 2 of 2: code" },
  { name: "child ticket", patch: { parent: { id: "kon-epic" } }, percent: 50, label: "Stage 2 of 3: code" },
  { name: "uninitialized ticket", patch: { kontora: false }, percent: 50, label: "Stage 2 of 3: code" },
  { name: "standalone supervisor", patch: { kind: "epic", epic_execution: "agent" } },
  { name: "supervisor with children", patch: { kind: "epic", epic_execution: "agent", children: [{ id: "kon-child" }] } },
  ...["open", "todo", "paused", "human_review", "done", "cancelled", "archived"].map(status => ({ name: status, patch: { status } })),
  { name: "missing status", patch: { status: undefined } },
  { name: "missing stages", patch: { stages: undefined } },
  { name: "null stages", patch: { stages: null } },
  { name: "empty stages", patch: { stages: [] } },
  { name: "single stage", patch: { stages: ["code"] } },
  { name: "unknown stage", patch: { stage: "retired" } },
  { name: "missing stage", patch: { stage: undefined } },
];

test("cardStageProgress returns null without a ticket", () => {
  const state = loadKontoraState();
  assert.equal(state.cardStageProgress(null), null);
  assert.equal(state.cardStageProgress(undefined), null);
});

for (const c of CARD_PROGRESS_CASES) {
  test(`cardStageProgress and desktop render: ${c.name}`, () => {
    const state = loadKontoraState();
    const ticket = { ...CARD_PROGRESS_TICKET, ...c.patch };
    const progress = state.cardStageProgress(ticket);
    const html = state._cardHTML(ticket, { key: "in_progress" });
    if (c.percent === undefined) {
      assert.equal(progress, null);
      assert.doesNotMatch(html, /class="card-progress"/);
      return;
    }
    assert.deepEqual({ ...progress }, { percent: c.percent, label: c.label });
    const bar = html.match(/<div class="card-progress"[^>]*>/)?.[0];
    assert.ok(bar);
    assert.match(bar, /role="img"/);
    assert.ok(bar.includes(`aria-label="${c.label}"`));
    assert.ok(bar.includes(`title="${c.label}"`));
    assert.doesNotMatch(bar, /aria-valuenow|aria-valuemin|aria-valuemax/);
    assert.ok(html.includes(`style="width:${c.percent}%"`));
  });
}

test("_cardHTML uses the progress helper and escapes its accessible stage label", () => {
  const state = loadKontoraState();
  const stage = `review <&> "quoted" 'stage'`;
  const ticket = { ...CARD_PROGRESS_TICKET, stage, stages: ["plan", stage] };
  const html = state._cardHTML(ticket, { key: "in_progress" });
  const label = 'Stage 2 of 2: review &lt;&amp;&gt; &quot;quoted&quot; &#39;stage&#39;';
  assert.ok(html.includes(`aria-label="${label}"`));
  assert.ok(html.includes(`title="${label}"`));

  let received;
  state.cardStageProgress = t => { received = t; return { percent: 42, label: "Shared progress" }; };
  const shared = state._cardHTML(ticket, { key: "in_progress" });
  assert.equal(received, ticket);
  assert.match(shared, /aria-label="Shared progress"/);
  assert.match(shared, /width:42%/);
  state.cardStageProgress = () => null;
  assert.doesNotMatch(state._cardHTML(ticket, { key: "in_progress" }), /class="card-progress"/);
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
    { name: "scheduled_at", ticket: { scheduled_at: "2026-05-20T09:00:00Z" } },

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

test("a drop into a busy column rebuilds only the card that moved", async () => {
  let opts = null;
  const { board, state, built } = renderedBoard(
    [
      { id: "kon-move", title: "Move", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
      { id: "kon-stay", title: "Stay", status: "todo", kontora: true, created_at: "2026-05-19T08:00:00Z" },
      { id: "rev-1", title: "R1", status: "human_review", kontora: true, updated_at: "2026-05-19T12:00:00Z" },
      { id: "rev-2", title: "R2", status: "human_review", kontora: true, updated_at: "2026-05-19T11:00:00Z" },
      { id: "rev-3", title: "R3", status: "human_review", kontora: true, updated_at: "2026-05-19T10:00:00Z" },
    ],
    {
      Sortable: class { constructor(el, o) { opts = o; } option() {} },
      fetch: async () => ({ ok: true, json: async () => ({}) }),
    },
  );
  state.$nextTick = () => Promise.resolve();
  state.initSortable(board.els["col-in_progress"]);
  const keptReview = board.uids("human_review");

  // The drop Sortable has already made when onEnd runs: the card node sits in
  // the middle of the target column, above where the sort will put it.
  const card = board.els["col-in_progress"].children[0];
  board.els["col-human_review"].insertBefore(card, board.els["col-human_review"].children[1]);
  board.ops.length = 0;
  opts.onEnd({
    item: card,
    from: { dataset: { dropStatus: "todo" } },
    to: { dataset: { dropStatus: "human_review" } },
  });
  await flushMicrotasks();

  // The moved card is the only one rebuilt: the cards already in the column are
  // moved into sorted order node-for-node, not re-rendered from board data.
  assert.deepEqual(built, ["kon-move"]);
  assert.deepEqual(board.ops, ["insert:rev-2", "insert:rev-3", "replace:kon-move"]);
  assert.deepEqual(board.ids("human_review"), ["rev-1", "rev-2", "rev-3", "kon-move"]);
  assert.deepEqual(board.ids("in_progress"), ["kon-stay"]);
  assert.deepEqual(board.uids("human_review").slice(0, 3), keptReview);
});

test("an update during a drag lands when the drag ends", async () => {
  let opts = null;
  const { board, state, built } = renderedBoard(
    [
      { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
      { id: "rev-1", title: "R1", status: "human_review", kontora: true, updated_at: "2026-05-19T12:00:00Z" },
    ],
    { Sortable: class { constructor(el, o) { opts = o; } option() {} } },
  );
  state.initSortable(board.els["col-in_progress"]);
  const dragged = { dataset: { dropStatus: "todo" }, closest: () => null };
  opts.onStart({ from: dragged });

  // An SSE update mid-drag: the board data takes it, but the cards do not, or
  // the reconcile could pull the node out from under the cursor.
  state.tickets[1].title = "R1 renamed";
  state.recomputeBoard();
  assert.deepEqual(board.ops, []);
  assert.deepEqual(built, []);

  // Dropped back where it came from: no move to post, but the held render runs.
  opts.onEnd({ item: { dataset: { ticketId: "kon-a" } }, from: dragged, to: dragged });
  await flushMicrotasks();

  assert.deepEqual(built, ["rev-1"]);
  assert.deepEqual(board.ops, ["replace:rev-1"]);
});

test("ticket updates during a drag update the model without touching card DOM", async () => {
  let opts = null;
  const { board, state, built } = renderedBoard(
    [
      { id: "kon-a", title: "A", status: "todo", kontora: true, created_at: "2026-05-19T09:00:00Z" },
      { id: "rev-1", title: "R1", status: "human_review", kontora: true, updated_at: "2026-05-19T12:00:00Z" },
    ],
    { Sortable: class { constructor(el, o) { opts = o; } option() {} } },
  );
  state.initSortable(board.els["col-in_progress"]);
  const dragged = { dataset: { dropStatus: "todo" }, closest: () => null };
  opts.onStart({ from: dragged });
  const query = board.document.querySelectorAll;
  board.document.querySelectorAll = () => { throw new Error('drag-owned card access'); };
  state._markSelectedCard();
  state._updateCardTimers();
  board.document.querySelectorAll = query;

  state.queueTicketUpdate({ id: "rev-1", title: "R1 renamed", status: "human_review", kontora: true, updated_at: "2026-05-19T12:00:00Z" });
  assert.deepEqual(board.ops, [], "an update mid-drag must not touch the cards");
  assert.equal(state.tickets[1].title, "R1 renamed", "the canonical model updates immediately");
  assert.equal(state._boardDirty, true);

  opts.onEnd({ item: { dataset: { ticketId: "kon-a" } }, from: dragged, to: dragged });
  await flushMicrotasks();

  assert.equal(state.tickets[1].title, "R1 renamed");
  assert.deepEqual(built, ["rev-1"]);
  assert.deepEqual(board.ops, ["replace:rev-1"]);
  assert.equal(state._boardDirty, false);
});

test("opening a ticket recomputes the board only when the card would change", async () => {
  const cases = [
    { name: "same fields", patch: {}, recomputes: 0 },
    { name: "stage moved on", patch: { stage: "review" }, recomputes: 1 },
    { name: "status moved on", patch: { status: "done" }, recomputes: 1 },
    { name: "agent reassigned", patch: { agent: "codex" }, recomputes: 1 },
    { name: "waiting for input", patch: { waiting_for_input: true }, recomputes: 1 },
  ];

  for (const c of cases) {
    const base = {
      id: "kon-a", title: "A", status: "in_progress", stage: "code", kontora: true,
      agent: "claude", pipeline: "kontora", created_at: "2026-05-19T08:00:00Z",
      started_at: "2026-05-19T09:00:00Z",
    };
    const full = { ...base, ...c.patch, body: "# Body" };
    const { state } = renderedBoard([{ ...base }], {
      fetch: async () => ({ ok: true, status: 200, json: async () => full }),
    });
    let recomputes = 0;
    const realRecompute = state.recomputeBoard.bind(state);
    state.recomputeBoard = () => { recomputes += 1; realRecompute(); };

    await state.selectTicket(state.tickets[0]);

    assert.equal(recomputes, c.recomputes, `${c.name}: recomputeBoard calls`);
    assert.equal(state.selectedTicket.id, "kon-a", `${c.name}: ticket opened`);
    assert.equal(state.tickets[0].stage, full.stage, `${c.name}: board entry replaced`);
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

test("index.html shares routed pages and builds board layers exclusively", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /<main id="main-content" class="app-main"/);
  assert.match(html, /<template x-if="!isMobile">\s*<!-- Board view:/);
  assert.match(html, /<template x-if="isMobile">\s*<div x-show="currentView === 'board'" class="mobile-board-layer"/);
  assert.equal(/<main x-show=/.test(html), false);
  for (const view of ['stats', 'settings', 'archive']) {
    assert.equal(html.split(`<template x-if="!loading && currentView === '${view}'">`).length - 1, 1, view);
  }
  const css = fs.readFileSync(path.join(dirname, '../../../hack/tailwind.css'), 'utf8');
  for (const name of ['mobile-detail', 'mobile-detail-actions', 'mobile-terminal-pane', 'mobile-assistant-thread']) {
    const tag = [...html.matchAll(/<[^>]+(?<!:)class="([^"]*)"[^>]*>/g)]
      .find(match => match[1].split(/\s+/).includes(name))?.[0];
    assert.ok(tag, name);
    assert.doesNotMatch(tag.match(/(?<!:)style="([^"]*)"/)?.[1] || '', /display\s*:/, name);
    assert.match(css, new RegExp('\\.' + name + '(?=[\\s,{])[^{}]*\\{[^}]*display:flex'), name);
  }
  for (const tag of html.matchAll(/<button\b[^>]*class="mobile-sheet-primary"[^>]*>/g)) {
    assert.doesNotMatch(tag[0], /\s:style=|\sstyle=/);
  }
});

test("mobile cards keep their content and state hooks without decorative indicators", () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const cards = html.split('x-for="ticket in mobileTickets()"')[1].split('<!-- ===== Ticket detail')[0];
  assert.match(html, /x-for="ticket in mobileTickets\(\)" :key="ticketKey\(ticket\)"/);
  const card = cards.match(/<button\b[^>]*@click="openMobileTicket\(ticket\)"[^>]*>/)?.[0];
  assert.ok(card);
  assert.match(card, /class="[^"]*\bshrink-0\b/);
  assert.match(card, /:data-pipe-color="ticketPipeColor\(ticket\)"/);
  for (const name of ['mobile-card-row', 'mobile-card-meta', 'mobile-card-id']) {
    assert.match(cards, new RegExp(`class="[^"]*\\b${name}\\b`));
  }
  const title = cards.match(/<p\b[^>]*class="mobile-card-title"[^>]*>([\s\S]*?)<\/p>/)?.[1];
  assert.ok(title, 'mobile title has separate tag and text spans');
  assert.match(title, /<template x-if="parseTitleTag\(ticket\)\.tag">[\s\S]*?<span\b[^>]*class="title-tag"/);
  assert.match(title, /:data-pipe-color="pipelineColorByName\(parseTitleTag\(ticket\)\.tag\)"/);
  assert.match(title, /x-text="'\[' \+ parseTitleTag\(ticket\)\.tag \+ '\] '"/);
  assert.match(title, /<span\b[^>]*x-text="parseTitleTag\(ticket\)\.rest"/);
  assert.doesNotMatch(title, /x-html=|x-text="ticket\.title"/);
  assert.match(cards, /x-show="showPipelineBadges"/);
  assert.match(cards, /x-if="showAgentMeta && ticket\.agent"/);
  for (const expression of ['ticket.id', 'ticket.agent', 'sourceLabel(ticket)']) {
    assert.ok(cards.includes(`x-text="${expression}"`), expression);
  }
  assert.match(cards, /x-if="!ticket\.kontora"/);
  assert.match(cards, /not init'd/);
  assert.match(cards, /ticket\.status\s*===\s*'cancelled'[\s\S]*?text-decoration:line-through/);
  assert.match(cards, /ticket\.status\s*===\s*'open' && ticket\.scheduled_at/);
  assert.match(cards, /scheduleChip\(ticket\)\.abs/);
  assert.match(cards, /scheduleChip\(ticket\)\.rel/);
  assert.match(cards, /epicIsAgent\(ticket\) \? 'supervising' : ticket\.stage/);
  assert.match(cards, /epicCardInfo\(ticket\)\.done/);
  assert.match(cards, /formatDuration\(ticket\)/);
  assert.doesNotMatch(cards, /border-left:|pulse-dot|waiting-dot/);
});

test("mobile cards use the shared progress helper behind a conditional template", () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const cards = html.split('x-for="ticket in mobileTickets()"')[1].split('<!-- ===== Ticket detail')[0];
  const progress = [...cards.matchAll(/<template x-if="([^"]+)">([\s\S]*?)<\/template>/g)]
    .find(([, , body]) => /class="card-progress"/.test(body));
  assert.ok(progress, 'excluded tickets must not mount a progress bar');
  assert.equal(progress[1], 'cardStageProgress(ticket)');
  assert.match(progress[2], /:aria-label="cardStageProgress\(ticket\)\.label"/);
  assert.match(progress[2], /:style="[^"]*cardStageProgress\(ticket\)\.percent[^"]*%[^"]*"/);
  assert.doesNotMatch(progress[2], /indexOf\(|Math\.round\(/);
});

test("mobile sheets retain their dialog heading and actions without repeated titles or dots", () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const sheets = html.split('<template x-if="isMobile && sheet">')[1];
  assert.ok(sheets);
  assert.match(sheets, /role="dialog" aria-modal="true" aria-labelledby="mobile-sheet-title"/);
  assert.match(sheets, /<h2 id="mobile-sheet-title"[^>]*x-text="[^"]*schedule: 'Start later'[^"]*new: 'New ticket'/);
  assert.match(sheets, /@click="closeSheet\(\)" class="mobile-sheet-close" data-mobile-sheet-initial-focus>Close<\/button>/);
  const creation = sheets.split('<template x-if="sheet.type===\'new\'">')[1];
  const schedule = sheets.split('<template x-if="sheet.type===\'schedule\' && ticketCanControl(sheet.ticket)">')[1]
    ?.split('<!-- assistant sheet')[0];
  assert.ok(creation);
  assert.ok(schedule);
  assert.doesNotMatch(creation, />\s*New ticket\s*</i);
  assert.doesNotMatch(schedule, />\s*Start later\s*</i);
  assert.match(creation, /@click="submitCreateTicketMobile\(\)"/);
  assert.match(schedule, /@click="submitScheduleMobile\(sheet\.ticket\)"/);
  assert.match(sheets, /x-if="sheet.type==='actions' && ticketCanControl\(sheet\.ticket\)"/);
  assert.match(sheets, /x-for="\(a, ai\) in mobileSheetActions\(sheet\.ticket\)"/);
  assert.match(sheets, /@click="a\.run\(\)"/);
  const actions = html.slice(html.indexOf('<!-- action bar -->'), html.indexOf('<!-- schedule sheet:'));
  assert.match(actions, /@click="mobilePrimaryAction\(selectedTicket\)\.run\(\)"/);
  assert.match(actions, /@click="mobileSecondaryAction\(selectedTicket\)\.run\(\)"/);
  assert.doesNotMatch(actions, /width:[67]px;height:[67]px;border-radius:9999px|mobileActionDotColor/);
  assert.doesNotMatch(uiSource + html, /mobileActionDotColor/);
});

test('both phone terminals use mounted native inline composers without a Compose sheet', () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  for (const [id, ref] of [['mobile-terminal-compose', 'mobileTerminalCompose'], ['mobile-epic-terminal-compose', 'mobileEpicTerminalCompose']]) {
    const field = html.match(new RegExp(`<textarea\\b[^>]*id="${id}"[\\s\\S]*?<\\/textarea>`))?.[0];
    assert.ok(field, id);
    assert.match(field, new RegExp(`x-ref="${ref}"`));
    assert.match(field, /data-mobile-terminal-input/);
    assert.match(field, /x-model="mobileComposeDraft"/);
    assert.match(field, /@focus="mobileBeginTerminalCompose\(\$el\)"/);
    assert.match(field, /@input="mobileResizeCompose\(\$el\)"/);
    assert.match(field, /x-effect="mobileComposeDraft;[^"\n]*mobileResizeCompose\(\$el\)/);
    assert.match(field, /@keydown.escape.stop.prevent="mobileFinishTerminalInput\(\)"/);
    assert.match(field, /rows="1"/);
    assert.doesNotMatch(field, /disabled|readonly|x-if|x-show|keydown.enter|autocorrect="off"|spellcheck="false"/i);
    const dock = html.slice(html.indexOf(field), html.indexOf(field) + 1500);
    assert.match(dock, /@click="mobileBeginTerminalDirect\(\)"[^>]*>Type in terminal<\/button>/);
    assert.match(dock, /@pointerdown.prevent @click="submitComposeMobile\(\)"/);
    assert.match(dock, /:disabled="!terminalRW \|\| !terminalConnected \|\| !mobileComposeDraft.trim\(\)"/);
  }
  assert.equal([...html.matchAll(/@pointerdown.prevent @click="sendTerminalKey\(key.bytes\)"/g)].length, 2);
  assert.equal([...html.matchAll(/@pointerdown.prevent @click="mobileFinishTerminalInput\(\)"/g)].length, 2);
  assert.doesNotMatch(html + uiSource, /openComposeSheet|sheet\.type\s*===?\s*'compose'|compose:\s*'Compose'/);
  assert.doesNotMatch(html, />Compose<\/button>|Enable input|Done typing/);
  for (const id of ['terminal-container-mobile', 'epic-terminal-mobile']) {
    assert.match(html, new RegExp(`id="${id}" class="mobile-terminal-host"`));
  }
});

test('focused phone description uses the shared editor and acknowledgement state', () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const field = html.match(/<textarea\b[^>]*id="mobile-description-editor"[^>]*>/)?.[0];
  assert.ok(field);
  assert.match(field, /x-model="editForm.body"/);
  assert.match(field, /@input="mobileDescriptionInput\(\$event.target.value\)"/);
  assert.match(field, /@keydown.escape.stop.prevent="mobileFinishDescriptionEdit\(\)"/);
  assert.doesNotMatch(field, /rows="10"|min-height:220px/);
  assert.match(html, /'mobile-editor-focus': mobileDescriptionFocused\(\)/);
  assert.match(html, /x-show="mobileDescriptionFocused\(\)" class="mobile-editor-bar"/);
  assert.match(html, /role="status" x-text="editSubmitting \? 'Saving…' : \(editSaved \? 'Saved' : ''\)"/);
  assert.match(html, /role="alert" class="mobile-editor-error" x-text="error"/);
});

test("mobile typography, focus and motion contracts stay phone-scoped", () => {
  const css = fs.readFileSync(path.join(dirname, '../../../hack/tailwind.css'), 'utf8');
  assert.match(css, /--font-body:\s*'DM Sans',sans-serif/);
  assert.match(css, /--font-technical:\s*'JetBrains Mono',monospace/);
  assert.match(css, /--mobile-text-body:\s*16px/);
  assert.match(css, /--mobile-text-label:\s*14px/);
  assert.match(css, /--mobile-text-meta:\s*13px/);
  const mobile = [...css.matchAll(/@media \(max-width:767px\) \{((?:[^{}]|\{[^{}]*\})*)\}/g)]
    .map(([, rules]) => rules).join('\n');
  assert.ok(mobile);
  assert.match(mobile, /\.mobile-chrome, \.mobile-board-layer, \.mobile-sheet-panel \{[^}]*font-family:var\(--font-body\)/);
  assert.match(mobile, /\.mobile-board-layer \.mobile-technical[^{}]*\{[^}]*font-family:var\(--font-technical\)/);
  assert.match(mobile, /\.mobile-board-layer \.ticket-prose :is\(code,pre\) \{[^}]*font-family:var\(--font-technical\)/);
  assert.match(mobile, /\.mobile-tab \{[^}]*flex:none;[^}]*white-space:nowrap;[^}]*var\(--font-body\)/);
  assert.match(mobile, /\.mobile-tab\[aria-pressed="true"\] \{[^}]*border-bottom-color:/);
  assert.match(mobile, /\.mobile-stage-control \{[^}]*font:[^}]*padding:[^}]*border-radius:/);
  assert.match(mobile, /\.mobile-card-row, \.mobile-card-meta \{[^}]*flex-wrap:wrap;[^}]*min-width:0/);
  assert.match(mobile, /\.mobile-card :is\(span,p\) \{[^}]*overflow-wrap:anywhere/);
  const focus = mobile.match(/([^{}]*\.mobile-chrome[^{}]*:focus-visible[^{}]*)\{([^}]*)\}/);
  assert.ok(focus);
  assert.match(focus[1], /\.mobile-board-layer[^{}]*:focus-visible/);
  assert.match(focus[1], /\.mobile-sheet-panel[^{}]*:focus-visible/);
  assert.match(focus[2], /outline:2px solid rgb\(var\(--accent\)\)/);
  assert.doesNotMatch(focus[2], /transition|animation/);
  assert.match(mobile, /\.mobile-board-layer button:disabled[^{}]*\{[^}]*opacity:[^}]*cursor:not-allowed/);
  assert.match(mobile, /\.mobile-detail button[^{}]*\{[^}]*min-width:44px;[^}]*min-height:44px/);
  assert.match(mobile, /input:not\(\.xterm-helper-textarea\)[^{}]*\{[^}]*font-size:16px !important/);
  assert.match(mobile, /\.mobile-chrome \.pulse-dot[^{}]*\.mobile-board-layer \.pulse-dot[^{}]*\{[^}]*animation:none/);
  assert.match(mobile, /\.mobile-board-layer \.detail-enter[^{}]*\{[^}]*animation-duration:var\(--mobile-duration\)/);
  const reduced = css.match(/@media \(max-width:767px\) and \(prefers-reduced-motion:reduce\) \{((?:[^{}]|\{[^{}]*\})*)\}/)?.[1];
  assert.ok(reduced);
  assert.match(reduced, /\.mobile-chrome \*[^{}]*\.mobile-board-layer \*[^{}]*\.mobile-sheet-panel \*[^{}]*\{[^}]*animation:none !important;[^}]*transition:none !important;[^}]*scroll-behavior:auto/);
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
  assert.match(html, /x-ref="colList"[\s\S]*?x-data="boardSortableOwner\(col\.key\)"/);
});

test("board width ignores folder path length while columns retain their minimum", () => {
  const html = htmlNoComments();
  const board = elementWithId(html, "board-cols");
  const classes = board.match(/^<div\b[^>]*\bclass="([^"]*)"/)[1].split(/\s+/);

  assert.ok(classes.includes("min-w-0"));
  assert.ok(!classes.includes("min-w-min"));
  assert.match(board, /:class="isCollapsed\(col\.key\) \? 'w-9 flex-none' : 'flex-1 min-w-\[280px\] basis-0'"/);
});

test("index.html column owner drops the render state Alpine just invalidated", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const app = uiSource;

  // Alpine builds the column element empty, so the owner's init must drop that
  // column's cached signatures or renderColumn would skip filling it. The name
  // has to be the field app.js actually keeps, hence the pair of assertions.
  assert.match(html, /x-data="boardSortableOwner\(col\.key\)"/);
  assert.match(app, /boardSortableOwner\(key\)[\s\S]*?delete self\._rendered\[key\]/);
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
  assert.match(desktop, /x-show="allBoardTickets\(\)\.length === 0 && !sourcesError && !ticketSources\.some\(s => s\.available === false\)"/);
  // The panel covers the board instead of replacing it: it is a sibling of
  // #board-cols inside the same board area, so Alpine keeps the column DOM and
  // its Sortable bindings mounted while the panel shows.
  assert.equal(desktop.includes('id="board-cols"'), false);
  const boardArea = elementAt(html, html.indexOf(':inert="!!selectedTicket || !!selectedEpic"'));
  assert.ok(boardArea.includes('id="board-cols"'), "panel and columns share the board area");

  // Desktop inherits !loading from the board view; mobile has no such wrapper,
  // so its own gate carries it. Without that, every phone load paints the
  // panel while the first fetch is still in flight.
  const boardView = elementAt(html, html.indexOf(`x-show="!loading && currentView === 'board'"`));
  assert.ok(boardView.includes('id="board-empty-desktop"'), "desktop panel sits under the !loading board view");
  const mobileGate = '<template x-if="!loading && allBoardTickets().length === 0 && !sourcesError && !ticketSources.some(s => s.available === false)">';
  const mobileIdx = html.indexOf(mobileGate);
  assert.ok(mobileIdx > 0, "mobile panel needs its own !loading gate");
  assert.match(html.slice(mobileIdx + mobileGate.length, mobileIdx + mobileGate.length + 40), /^\s*<div id="board-empty-mobile"/);

  // Mobile's per-column empty state stands down while the panel shows, so the
  // two never stack.
  assert.match(html, /x-if="allBoardTickets\(\)\.length > 0 && boardTickets\(mobileColumn\(\)\.key\)\.length === 0"/);
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
  state.configCache = {};
  state.$nextTick = async () => {};
  await state.startEditing();
  state.editForm.body = 'typed';

  // closeDetail flushes the pending body, then clears the selection while the
  // save is still pending.
  const pending = state.saveEdit();
  state.closeTerminal = () => {};
  state._resetActivity = () => {};
  state.closeDetail(false);
  await pending;

  assert.equal(state.selectedTicket, null);
  assert.equal(state.editSubmitting, false);
  assert.equal(sent.description, "typed");
  assert.equal(state._editorSaves['kon-1'].ack.description, 'typed');
});

function descriptionEditorUI(kind = '', id = 'kon-desc', source = '') {
  let stored = {
    ...(source ? { source, revision: 'reviewed', can_edit: true, can_claim: true, can_control: false } : {}),
    id, title: 'Description', status: 'open', kontora: true, kind,
    description: '# Description\n\n',
    description_tail: '## Notes\r\n\r\n**custom · person · a1b2**\r\nkeep raw\r\n\r\n## Later\r\nkeep later\r\n',
    notes: [{ id: 'a1b2', text: 'keep raw' }],
  };
  stored.body = stored.description + stored.description_tail;
  const writes = [];
  const requests = [];
  let intercept = async () => true;
  let respond = async (_url, _opts, full) => ({ ok: true, json: async () => full });
  const { state, ctx } = loadKontoraContext({
    fetch: async (url, opts) => {
      requests.push({ url, opts });
      if (opts?.method === 'PUT') {
        const request = JSON.parse(opts.body);
        writes.push(request);
        if (!await intercept(request, writes.length)) return { ok: false, json: async () => ({ error: 'save rejected' }) };
        stored = { ...stored, ...request };
        if (Object.hasOwn(request, 'body')) {
          const prefix = request.body.split(/^[^\S\n]*## Notes[^\S\n]*$/m)[0];
          stored.description = prefix;
          stored.description_tail = request.body.slice(prefix.length);
          stored.notes = stored.notes.filter(note => request.body.includes(note.text));
        }
        stored.body = stored.description + stored.description_tail;
      } else if (opts?.method === 'POST' && url.endsWith('/note')) {
        const note = { id: 'c3d4', text: JSON.parse(opts.body).text };
        stored = { ...stored, notes: [...stored.notes, note], description_tail: stored.description_tail + '\n' + note.text };
        stored.body = stored.description + stored.description_tail;
      }
      return respond(url, opts, { ...stored });
    },
  });
  state.configCache = {};
  state.$nextTick = async () => {};
  state.closeTerminal = () => {};
  state.openTerminal = () => {};
  state.fetchTicketCost = () => {};
  state.fetchChain = () => {};
  state.updateFavicon = () => {};
  state.recomputeBoard = () => {};
  state.ticketSources = source ? [{ id: source, path: '/source', available: true }] : [];
  state.tickets = source ? [{ id, title: 'Primary namesake', status: 'open' }] : [state.boardEntry(stored)];
  state.secondaryTickets = source ? [state.boardEntry(stored)] : [];
  const open = async () => {
    if (kind === 'epic' && !source) await state.openEpic(stored.id);
    else await state.selectTicket(source ? state.secondaryTickets[0] : state.tickets[0]);
    await flushMicrotasks();
  };
  return { state, ctx, writes, requests, open, stored: () => stored,
    intercept: fn => { intercept = fn; },
    respond: fn => { respond = fn; },
    save: () => kind === 'epic' ? state.epicSaveBrief() : state.saveEdit(),
    close: () => kind === 'epic' ? state.closeEpic(false) : state.closeDetail(false),
  };
}

// A primary paused ticket with history takes the output path in selectTicket,
// so no desktop editor is initialized.
function mobileDescriptionUI({ begin = true } = {}) {
  const ui = descriptionEditorUI();
  const { state } = ui;
  Object.assign(ui.stored(), { status: 'paused', history: [{ stage: 'code', run: 0, imported: true }] });
  state.isMobile = true;
  state.tickets = [state.boardEntry(ui.stored())];
  state.fetchStageLogs = () => {};
  state.$refs = { mobileDescriptionEditor: { focus() {} } };
  ui.open = async () => {
    await state.openMobileTicket(state.tickets[0]);
    state.mobileSwitchTab('ticket');
    if (begin) assert.equal(await state.mobileBeginDescriptionEdit(), true);
  };
  ui.save = () => state.mobileFinishDescriptionEdit();
  return ui;
}

for (const mobile of [false, true]) {
  test(`secondary namesake editing stays source-qualified: mobile=${mobile}`, async () => {
    const ui = descriptionEditorUI('', 'same', 'team');
    await ui.open();
    const { state } = ui;
    state.isMobile = mobile;
    if (mobile) { state.closeDetail(false); await state.openMobileTicket(state.secondaryTickets[0]); }
    state.editForm.body = '# Secondary draft';
    assert.equal(await state.saveEdit(), true);
    assert.equal(state.tickets[0].title, 'Primary namesake');
    assert.equal(state.outputSubject(), null);
    assert.equal(state.ticketCanControl(state.selectedTicket), false);
    assert.ok(ui.requests.every(r => r.url.startsWith('/api/sources/team/tickets/same')));
    state.applyTicketUpdate({ id: 'same', title: 'Changed primary', status: 'open' });
    assert.equal(state.selectedTicket.source, 'team');
    assert.equal(state.editForm.body, '# Secondary draft');
    assert.ok(state._editorSaves['team/same']);
  });
}

for (const control of ['x-model="sourceFilter"', '@submit.prevent="saveSourceRelation()"', 'aria-label="Relationship action"']) {
  test(`secondary folder controls are absent from desktop and mobile markup: ${control}`, () => {
    assert.equal(htmlNoComments().includes(control), false);
  });
}

test('combined secondary statuses never count as primary execution', () => {
  const state = loadKontoraState();
  state.updateFavicon = () => {};
  state.renderBoard = () => {};
  state.tickets = [{ id: 'same', status: 'in_progress', kontora: true, agent: 'test' }];
  state.secondaryTickets = [
    { id: 'same', source: 'team', status: 'in_progress', kontora: true, agent: 'test' },
    { id: 'old', source: 'team', status: 'archived' },
    { id: 'foreign', source: 'team', status: 'external' },
  ];
  state.recomputeBoard();
  assert.equal(state.runningAgents, 1);
  assert.equal(state._statusCounts.in_progress, 1);
  assert.equal(state.boardTickets('in_progress').length, 2);
  assert.equal(state.boardTickets('archived').length, 1);
  assert.equal(state.boardTickets('external').length, 1);
  state.sourceFilter = 'primary';
  state.recomputeBoard();
  assert.equal(state._boardTotal, 1);
  assert.equal(state.runningAgents, 1);
  assert.equal(state.tickets.length, 1);
});

test('secondary Claim retains a rejected draft and submits nothing', async () => {
  const ui = descriptionEditorUI('', 'same', 'team');
  await ui.open();
  ui.ctx.window.confirm = () => true;
  ui.intercept(async () => false);
  ui.state.editForm.body = '# Keep failed draft';
  assert.equal(await ui.state.claimTicket(), false);
  assert.equal(ui.state.selectedTicket.source, 'team');
  assert.equal(ui.state.editForm.body, '# Keep failed draft');
  assert.equal(ui.requests.filter(r => r.url.endsWith('/claim')).length, 0);
  assert.equal(ui.state.editorClaimLocked(), false);
});

test('secondary Claim survives a deletion event before its response and opens primary', async () => {
  const ui = descriptionEditorUI('', 'claim', 'team');
  await ui.open();
  ui.ctx.window.confirm = () => true;
  const primary = { ...ui.stored(), source: undefined, can_control: true, status: 'open' };
  ui.respond(async (url, opts, full) => {
    if (url.endsWith('/claim')) {
      assert.equal(JSON.parse(opts.body).revision, 'reviewed');
      ui.state.deleteSecondaryTicket({ id: 'claim', source: 'team' });
      assert.equal(ui.state.selectedTicket.source, 'team');
      return { ok: true, json: async () => primary };
    }
    return { ok: true, json: async () => url.startsWith('/api/tickets/') ? primary : full };
  });
  assert.equal(await ui.state.claimTicket(), true);
  assert.equal(ui.state.isSecondary(ui.state.selectedTicket), false);
  assert.equal(ui.state.selectedTicket.id, 'claim');
  assert.equal(ui.state.secondaryTickets.length, 0);
});

test('secondary notes, reactions and relationships never address primary namesakes', async () => {
  const ui = descriptionEditorUI('', 'same', 'team');
  await ui.open();
  const { state } = ui;
  state.secondaryTickets.push({ id: 'related', source: 'team', title: 'Source relation', status: 'open' });
  state.tickets.push({ id: 'related', title: 'Primary relation', status: 'open' });
  let opened;
  state._paletteOpenTicket = async ticket => { opened = ticket; };
  await state.openTicketRef({ id: 'related', title: 'Source relation', status: 'open' });
  assert.equal(opened.source, 'team');
  await state.moveTicketVia(state.ticketKey(state.selectedTicket), 'run');
  assert.equal(ui.requests.filter(r => r.url.endsWith('/run')).length, 0);
  state.switchTab('notes');
  assert.equal(state.activeTab, 'notes');
  state.noteDraft = 'Only in source';
  await state.submitNote();
  assert.ok(ui.requests.some(r => r.url === '/api/sources/team/tickets/same/note' && r.opts?.method === 'POST'));
  assert.equal(state.tickets[0].title, 'Primary namesake');
  state.sourceRelationDraft().targets = 'related';
  ui.respond(async (url, opts, full) => {
    if (url.endsWith('/dep')) {
      assert.equal(url, '/api/sources/team/tickets/same/dep');
      assert.deepEqual(JSON.parse(opts.body), { related: ['related'] });
      full.deps = [{ id: 'related', title: 'Source relation', status: 'open' }];
    }
    return { ok: true, json: async () => full };
  });
  assert.equal(await state.saveSourceRelation(), true);
  assert.equal(state.selectedTicket.deps[0].title, 'Source relation');
  assert.equal(state.sourceRelationDraft(state.tickets[0]).targets, '');
  assert.equal(state.tickets[0].title, 'Primary namesake');
  state.applySecondaryUpdate({ ...ui.stored(), title: 'Source event' });
  assert.equal(state.tickets[0].title, 'Primary namesake');
  assert.equal(state.secondaryTickets[0].title, 'Source event');
});

for (const source of ['', 'team', 'other']) {
  test(`late schedule completion only rebinds the matching primary editor: source=${source || 'primary'}`, async () => {
    const ui = descriptionEditorUI('', 'same', source);
    await ui.open();
    const { state } = ui;
    const scheduled = deferred();
    ui.respond(async (url, _opts, full) => url.endsWith('/schedule')
      ? scheduled.promise : { ok: true, json: async () => full });
    const pending = state._postSchedule({ id: 'same', status: 'open', kontora: true }, { clear: true });
    const form = state.editForm;
    const generation = state._editorGeneration;
    scheduled.resolve({ ok: true, json: async () => ({ id: 'same', status: 'open', kontora: true, description: 'Scheduled primary body' }) });
    await pending;
    if (source) {
      assert.equal(state.selectedTicket.source, source);
      assert.equal(state.editForm, form);
      assert.equal(state._editorGeneration, generation);
      state.editForm.body = 'Source draft after scheduling';
      await state.saveEdit();
      assert.equal(ui.requests.at(-1).url, `/api/sources/${source}/tickets/same`);
      assert.equal(state.tickets[0].description, undefined);
    } else {
      assert.equal(state.editForm.body, 'Scheduled primary body');
      assert.ok(state._editorGeneration > generation);
    }
  });
}

test('a schedule started before navigating to a secondary namesake cannot change its save address', async () => {
  const ui = descriptionEditorUI('', 'same', 'team');
  const { state } = ui;
  const primary = { id: 'same', status: 'open', kontora: true, description: 'Primary body' };
  const scheduled = deferred();
  ui.respond(async (url, _opts, full) => url.endsWith('/schedule') ? scheduled.promise
    : { ok: true, json: async () => url.startsWith('/api/tickets/') ? primary : full });
  await state.selectTicket(primary);
  const pending = state._postSchedule(primary, { clear: true });
  await state.selectTicket(state.secondaryTickets[0]);
  state.editForm.body = 'Secondary input while primary schedule waits';
  const generation = state._editorGeneration;
  scheduled.resolve({ ok: true, json: async () => primary });
  await pending;
  assert.equal(state.selectedTicket.source, 'team');
  assert.equal(state._editorGeneration, generation);
  await state.saveEdit();
  assert.equal(ui.requests.at(-1).url, '/api/sources/team/tickets/same');
  assert.equal(ui.writes.at(-1).description, 'Secondary input while primary schedule waits');
});

test('schedule completion preserves a dirty matching primary draft', async () => {
  const ui = descriptionEditorUI('', 'same');
  await ui.open();
  ui.state.editForm.body = 'Unsaved primary draft';
  ui.state._applyScheduled({ id: 'same', status: 'open', kontora: true, description: 'Server body' });
  assert.equal(ui.state.editForm.body, 'Unsaved primary draft');
});

for (const editing of [false, true]) {
  test(`secondary SSE keeps the revision of the displayed description: editing=${editing}`, async () => {
    const ui = descriptionEditorUI('', 'same', 'team');
    await ui.open();
    const { state } = ui;
    const description = state.selectedTicket.description;
    state.editing = editing;
    const sources = [];
    ui.ctx.EventSource = makeEventSourceFake(sources);
    state.connectSSE();
    sources[0].emit('secondary_ticket_updated', { ...ui.stored(), revision: 'unseen', description: 'Unreviewed body', title: 'New title' });
    assert.equal(state.secondaryTickets[0].revision, 'unseen');
    assert.equal(state.selectedTicket.title, 'New title');
    assert.equal(state.selectedTicket.description, description);
    assert.equal(state.selectedTicket.revision, 'reviewed');
    ui.ctx.window.confirm = () => true;
    ui.respond(async (url, opts, full) => {
      if (url.endsWith('/claim')) {
        assert.equal(JSON.parse(opts.body).revision, 'reviewed');
        return { ok: false, json: async () => ({ error: 'stale reviewed revision' }) };
      }
      return { ok: true, json: async () => full };
    });
    assert.equal(await state.claimTicket(), false);
    assert.equal(state.selectedTicket.source, 'team');
    assert.equal(state.selectedTicket.description, description);
  });
}

test('Claim refuses an unseen description fetched after SSE overtakes its editor save', async () => {
  const ui = descriptionEditorUI('', 'same', 'team');
  await ui.open();
  const { state } = ui;
  ui.ctx.window.confirm = () => true;
  ui.intercept(async () => {
    state.applySecondaryUpdate({ ...ui.stored(), revision: 'unseen', title: 'Concurrent update' });
    return true;
  });
  ui.respond(async (_url, opts, full) => ({ ok: true, json: async () => opts?.method === 'PUT'
    ? full : { ...full, revision: 'unseen', description: 'Concurrent external description' } }));
  state.editForm.body = 'Reviewed local input';
  assert.equal(await state.claimTicket(), false);
  assert.equal(state.editForm.body, 'Reviewed local input');
  assert.equal(state.selectedTicket.source, 'team');
  assert.match(state.error, /description changed/);
  assert.equal(ui.requests.filter(r => r.url.endsWith('/claim')).length, 0);
});

for (const mobile of [false, true]) {
  test(`Claim locks the source editor before loading it: mobile=${mobile}`, async () => {
    const ui = descriptionEditorUI('', 'same', 'team');
    const { state } = ui;
    state.isMobile = mobile;
    ui.ctx.window.confirm = () => false;
    const loading = deferred();
    ui.respond(async (_url, _opts, full) => {
      await loading.promise;
      return { ok: true, json: async () => full };
    });
    const pending = state.claimTicket(state.secondaryTickets[0]);
    assert.equal(state.claimSubmitting, true);
    assert.equal(state.editorClaimLocked({ id: 'same', source: 'team' }), true);
    assert.equal(state.editorClaimLocked({ id: 'same', source: 'other' }), false);
    await flushMicrotasks();
    assert.equal(state.editorClaimLocked(), true);
    loading.resolve();
    assert.equal(await pending, false);
    assert.equal(state.editorClaimLocked(), false);
    assert.equal(state.editing, true);
  });

  test(`Claim locks input through debounced save, response and JSON waits: mobile=${mobile}`, async () => {
    const ui = descriptionEditorUI('', 'same', 'team');
    await ui.open();
    const { state } = ui;
    state.isMobile = mobile;
    ui.ctx.window.confirm = () => true;
    const save = deferred(), response = deferred(), json = deferred();
    const primary = { ...ui.stored(), source: undefined, can_control: true };
    ui.intercept(async () => { await save.promise; return true; });
    ui.respond(async (url, _opts, full) => {
      if (url.endsWith('/claim')) {
        await response.promise;
        return { ok: true, json: async () => { await json.promise; return primary; } };
      }
      return { ok: true, json: async () => url.startsWith('/api/tickets/') ? primary : full };
    });
    state.editForm.body = 'Draft before Claim';
    state.debounceSaveEdit();
    const timer = state._editDebounce;
    const pending = state.claimTicket();
    assert.equal(state._editDebounce, null);
    assert.equal(timer._destroyed, true);
    await flushMicrotasks();
    assert.equal(state.editorClaimLocked(), true);
    assert.equal(ui.requests.filter(r => r.url.endsWith('/claim')).length, 0);
    state.editingBody = false;
    state.beginBodyEdit({});
    assert.equal(state.editingBody, false);
    save.resolve();
    await flushMicrotasks();
    assert.equal(ui.writes.length, 1);
    assert.equal(ui.writes[0].description, 'Draft before Claim');
    assert.equal(ui.requests.filter(r => r.url.endsWith('/claim')).length, 1);
    assert.equal(state.editorClaimLocked(), true);
    response.resolve();
    await flushMicrotasks();
    assert.equal(state.editorClaimLocked(), true);
    json.resolve();
    assert.equal(await pending, true);
    assert.equal(state.editorClaimLocked(), false);
  });

  test(`Claim checks the live form after awaiting a save: mobile=${mobile}`, async () => {
    const ui = descriptionEditorUI('', 'same', 'team');
    await ui.open();
    const { state } = ui;
    state.isMobile = mobile;
    ui.ctx.window.confirm = () => true;
    const save = deferred();
    ui.intercept(async () => { await save.promise; return true; });
    state.editForm.body = 'Submitted draft';
    const pending = state.claimTicket();
    await flushMicrotasks();
    state.editForm.body = 'Later input';
    save.resolve();
    assert.equal(await pending, false);
    assert.equal(state.editForm.body, 'Later input');
    assert.equal(state.selectedTicket.source, 'team');
    assert.equal(state.editing, true);
    assert.equal(ui.requests.filter(r => r.url.endsWith('/claim')).length, 0);
  });

  for (const source of ['', 'other']) {
    test(`Claim completion leaves another editor and its debounce untouched: mobile=${mobile}, source=${source || 'primary'}`, async () => {
      const ui = descriptionEditorUI('', 'same', 'team');
      await ui.open();
      const { state } = ui;
      state.isMobile = mobile;
      ui.ctx.window.confirm = () => true;
      const claimed = deferred();
      const primary = { ...ui.stored(), source: undefined, can_control: true };
      const other = { ...ui.stored(), id: 'other-ticket', source: source || undefined, description: 'Other body' };
      ui.respond(async (url, _opts, full) => url.endsWith('/claim') ? claimed.promise
        : { ok: true, json: async () => url.endsWith('/other-ticket') ? other : full });
      const pending = state.claimTicket();
      await flushMicrotasks();
      if (mobile) await state.openMobileTicket(other);
      else await state.selectTicket(other);
      state.editForm.body = 'Unrelated pending input';
      state.debounceSaveEdit();
      const form = state.editForm, generation = state._editorGeneration, timer = state._editDebounce;
      assert.equal(state.editorClaimLocked(), false);
      claimed.resolve({ ok: true, json: async () => primary });
      assert.equal(await pending, true);
      assert.equal(state.editing, true);
      assert.equal(state.editForm, form);
      assert.equal(state.editForm.body, 'Unrelated pending input');
      assert.equal(state._editorGeneration, generation);
      assert.equal(state._editDebounce, timer);
      clearTimeout(timer);
      assert.equal(await state.flushEditSave(), true);
      assert.equal(ui.requests.at(-1).url, source ? '/api/sources/other/tickets/other-ticket' : '/api/tickets/other-ticket');
      assert.equal(ui.writes.at(-1).description, 'Unrelated pending input');
    });
  }
}

test('desktop and mobile native fieldsets disable every description and settings input during Claim', () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const fieldsets = [...html.matchAll(/<fieldset :disabled="editorClaimLocked\(\)"[^>]*>([\s\S]*?)<\/fieldset>/g)];
  assert.equal(fieldsets.length, 2);
  for (const [index, [, content]] of fieldsets.entries()) {
    assert.match(content, /x-model="editForm.body"/);
    assert.match(content, index ? /x-model="editForm\[field\]"/ : /x-model="editForm.pipeline"/);
  }
  const tooltip = 'Move into primary without starting work. Related tickets are not moved; references resolve in primary.';
  assert.equal(html.split(`selectedTicket?.refusal_reason || '${tooltip}'`).length - 1, 2);
});

test('secondary __proto__ status survives recomputation, rendering and later primary refresh', () => {
  const board = fakeBoard(['open', '__proto__']);
  const { state } = loadKontoraContext({ document: board.document });
  state.updateFavicon = () => {};
  state._boardInit = true;
  state.tickets = [{ id: 'primary', title: 'Primary', status: 'open' }];
  state.secondaryTickets = [{ id: 'foreign', title: 'Foreign', source: 'team', status: '__proto__' }];
  state.recomputeBoard();
  assert.equal(state.boardTickets('__proto__')[0].id, 'foreign');
  assert.equal(Object.getPrototypeOf(state._board), null);
  assert.match(board.els['col-__proto__'].innerHTML, /Foreign/);
  state._cardNode = ticket => board.node(state.ticketKey(ticket));
  state._dirtyCols.__proto__ = true;
  state.applyTicketUpdate({ id: 'primary', title: 'Refreshed primary', status: 'open' });
  state.recomputeBoard();
  assert.equal(state.boardTickets('open')[0].title, 'Refreshed primary');
  assert.equal(state.boardTickets('__proto__').length, 1);
  assert.equal(Object.getPrototypeOf(state._rendered), null);
});

for (const deleted of [{ id: 'same', source: 'team' }, { id: 'same' }]) {
  test(`deletion retains namesakes under ordered updates during a drag: source=${deleted.source || 'primary'}`, () => {
    const sources = [];
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(sources) });
    state.updateFavicon = () => {};
    state.renderBoard = () => {};
    state.tickets = [{ id: 'same', title: 'Primary', status: 'open' }];
    state.secondaryTickets = ['team', 'other'].map(source => ({ id: 'same', source, title: source, status: 'open' }));
    state.connectSSE();
    state._dragging = true;
    for (const ticket of state.allBoardTickets()) state.queueTicketUpdate({ ...ticket, title: 'Buffered ' + ticket.title });
    sources[0].emit(deleted.source ? 'secondary_ticket_deleted' : 'ticket_deleted', deleted);
    assert.equal('_pendingTicketUpdates' in state, false);
    assert.equal(state.allBoardTickets().length, 2);
    state._dragging = false;
    state.flushTicketUpdates();
    assert.equal(state.allBoardTickets().some(t => state.sameTicket(t, deleted)), false);
    assert.equal(state.allBoardTickets().length, 2);
    assert.ok(state.allBoardTickets().every(t => t.title.startsWith('Buffered ')));
  });
}

test('imported summaries remain visible without artifact navigation', async () => {
  const { state } = loadKontoraContext({ fetch: async () => { throw new Error('unexpected artifact request'); } });
  state.selectedTicket = { id: 'imported', status: 'open', history: [{ stage: 'code', run: 0, imported: true, summary: 'Stored summary' }] };
  assert.equal(state.summaryCards()[0].summary, 'Stored summary');
  assert.equal(state.summaryCards()[0].imported, true);
  assert.equal(state.latestCompletedRun(), null);
  assert.equal(state.outputRuns().length, 0);
  await state.fetchActivity('code', 0);
  assert.equal(state.activity, null);
});

for (const mode of ['ordinary', 'epic', 'mobile']) {
  const kind = mode === 'epic' ? 'epic' : '';
  const editorUI = () => mode === 'mobile' ? mobileDescriptionUI() : descriptionEditorUI(kind);
  test(`description acknowledgement preserves newer typing and raw tail: ${mode}`, async () => {
    const ui = editorUI();
    await ui.open();
    const { state } = ui;
    const tail = ui.stored().description_tail;
    assert.equal(state.editForm.body, '# Description\n\n');
    const pending = deferred();
    ui.intercept(async () => { await pending.promise; return true; });
    state.editForm.body = '# Submitted\n\n';
    const saving = ui.save();
    await flushMicrotasks();
    state.editForm.body = '# Newer typing\n\n';
    pending.resolve();
    await saving;
    assert.equal(state.editForm.body, '# Newer typing\n\n');
    if (!kind) {
      assert.equal(state._editorSaves['kon-desc'].ack.description, '# Submitted\n\n');
      assert.equal(state.editSaved, false);
      await ui.save();
    }
    assert.equal(ui.stored().description, '# Newer typing\n\n');
    assert.equal(ui.stored().description_tail, tail);
    assert.ok(ui.writes.every(request => !Object.hasOwn(request, 'body')));
    await ui.close();
  });

  test(`description saves remain ordered through close and reopen: ${mode}`, async () => {
    const ui = editorUI();
    await ui.open();
    const pending = deferred();
    ui.intercept(async () => { await pending.promise; return true; });
    ui.state.editForm.body = '# First\n';
    const first = ui.save();
    await flushMicrotasks();
    const closing = ui.close();
    const reopening = (async () => { await closing; await ui.open(); })();
    await flushMicrotasks();
    assert.equal(ui.writes.length, 1);
    pending.resolve();
    await first;
    await reopening;
    ui.state.editForm.body = '# Second\n';
    await ui.save();
    assert.deepEqual(ui.writes.map(write => write.description), ['# First\n', '# Second\n']);
    assert.equal(ui.stored().description, '# Second\n');
    await ui.close();
  });

  test(`failed description draft can be retried without replacing newer text: ${mode}`, async () => {
    const ui = editorUI();
    await ui.open();
    ui.intercept(async () => false);
    ui.state.editForm.body = '# Recover me\n';
    assert.equal(await ui.save(), false);
    if (!kind) assert.equal(ui.state.editSaved, false, 'a rejected save must not show Saved');
    assert.equal(ui.state.editForm.body, '# Recover me\n');
    assert.equal(ui.state._editorSaves['kon-desc'].ack.description, '# Description\n\n');
    if (!kind) {
      ui.close();
      await ui.open();
      assert.equal(ui.state.editForm.body, '# Recover me\n');
    }
    ui.intercept(async () => true);
    assert.equal(await ui.save(), true);
    assert.equal(ui.stored().description, '# Recover me\n');
    await ui.close();
  });
}

test('mobile paused-with-history description starts an editor and focuses without scrolling', async () => {
  const ui = mobileDescriptionUI({ begin: false });
  await ui.open();
  const { state } = ui;
  assert.equal(state.mobileHasRun(state.selectedTicket), true);
  assert.equal(state.editing, false);
  assert.equal(state.mobileCanEditDescription(), true);
  const focused = [];
  state.$refs.mobileDescriptionEditor.focus = options => focused.push(vmValue(options));
  const generation = state._editorGeneration;
  assert.equal(await state.mobileBeginDescriptionEdit(), true);
  assert.equal(state._editorGeneration, generation + 1);
  assert.equal(state.editing, true);
  assert.equal(state.editingBody, true);
  assert.equal(state.mobileDescriptionFocused(), true);
  assert.equal(state.editForm.body, ui.stored().description);
  assert.deepEqual(focused, [{ preventScroll: true }]);
  const tail = ui.stored().description_tail, notes = vmValue(ui.stored().notes);
  state.mobileDescriptionInput('# Phone draft\n');
  assert.equal(state.editSaved, false);
  assert.equal(await state.mobileFinishDescriptionEdit(), true);
  assert.equal(state._editDebounce, null);
  assert.equal(state.editingBody, false);
  assert.equal(state.mobileDescriptionFocused(), false);
  assert.equal(ui.stored().description, '# Phone draft\n');
  assert.equal(ui.stored().description_tail, tail);
  assert.deepEqual(vmValue(ui.stored().notes), notes);
  assert.deepEqual(ui.writes, [{ description: '# Phone draft\n' }]);
  ui.close();
});

for (const failure of ['network', 'http', 'json']) {
  test(`phone description editing requires a successful detail read after ${failure} failure`, async () => {
    const ui = mobileDescriptionUI({ begin: false });
    let connected = false;
    ui.respond(async (_url, _opts, full) => {
      if (connected) return { ok: true, json: async () => full };
      if (failure === 'network') throw new Error('offline');
      return { ok: failure !== 'http', json: async () => { throw new Error('invalid JSON'); } };
    });
    await ui.open();
    const { state } = ui;
    assert.equal(state.detailLoading, false);
    assert.equal(state.mobileCanEditDescription(), false);
    assert.equal(await state.mobileBeginDescriptionEdit(), false);
    state.mobileDescriptionInput('An addition');
    await state.mobileFinishDescriptionEdit();
    assert.deepEqual(ui.writes, []);
    connected = true;
    assert.equal(await state.mobileBeginDescriptionEdit(), false, 'reconnection alone does not hydrate the board projection');
    state.closeDetail(false);
    await ui.open();
    assert.equal(await state.mobileBeginDescriptionEdit(), true, 'reopening retries the detail read');
    assert.equal(state.editForm.body, ui.stored().description);
    state.mobileDescriptionInput('# Hydrated addition\n');
    assert.equal(await state.mobileFinishDescriptionEdit(), true);
    assert.equal(ui.stored().description, '# Hydrated addition\n');
    assert.ok(ui.stored().description_tail.includes('keep raw'));
    ui.close();
  });
}

test('phone description editing permits a successfully loaded empty description', async () => {
  const ui = mobileDescriptionUI();
  ui.stored().description = '';
  ui.stored().body = ui.stored().description_tail;
  await ui.open();
  assert.equal(ui.state.editForm.body, '');
  ui.state.mobileDescriptionInput('First description');
  assert.equal(await ui.save(), true);
  assert.equal(ui.stored().description, 'First description');
  assert.ok(ui.stored().description_tail.includes('keep raw'));
  ui.close();
});

for (const boundary of ['breakpoint', 'navigation', 'destroy']) {
  test(`Archive selection cannot submit another ticket's editor on ${boundary}`, async () => {
    const writes = [];
    const a = { id: 'kon-a', title: 'A', status: 'open', description: '# A\n', path: '/repo' };
    const b = { id: 'kon-b', title: 'B', status: 'done', kind: 'epic', epic_mode: 'passive', description: '# B\n', path: '/repo', can_edit: true };
    const state = loadKontoraState({ fetch: async (url, opts = {}) => {
      const record = url.endsWith('/kon-b') ? b : a;
      if (opts.method === 'PUT') {
        const body = JSON.parse(opts.body);
        writes.push({ id: record.id, body });
        Object.assign(record, body);
      }
      return { ok: true, json: async () => ({ ...record }) };
    } });
    state.configCache = {};
    state.$nextTick = async fn => fn?.();
    state.fetchTicketCost = state.fetchChain = state.fetchChanges = state.fetchActivity = () => {};
    state.closeTerminal = state.openTerminal = state.recomputeBoard = () => {};
    state.tickets = [state.boardEntry(a), state.boardEntry(b)];
    await state.openEpic(b.id);
    assert.ok(state._editorSaves[b.id], 'ordinary overview primes the editor cache');
    await state.closeEpic(false);
    b.status = 'archived';
    await state.selectTicket(state.tickets[0]);
    assert.equal(state.editing, true);
    state.openArchive = () => {};
    await state.gotoView('archive');
    await state.navigateDestination({ view: 'archive', ticketId: null });
    await state.archiveOpenRow(b);
    assert.equal(state.selectedTicket.id, b.id);
    assert.equal(state.editing, false);
    if (boundary === 'breakpoint') { state.isMobile = true; state.mobileBreakpointChanged(); }
    if (boundary === 'navigation') { state.mobileBeforeNavigate('board'); state.closeArchive(); }
    if (boundary === 'destroy') state.destroyMobile();
    await state._waitEditorSaves(b.id);
    assert.equal(b.description, '# B\n');
    assert.deepEqual(writes, []);
  });
}

for (const source of ['primary', 'team']) {
  test(`editor saves reject a changed ${source} selection even with a cached draft`, async () => {
    const ui = descriptionEditorUI();
    await ui.open();
    const { state } = ui;
    const other = { ...ui.stored(), source, can_edit: true, id: source === 'primary' ? 'other' : ui.stored().id };
    state._loadEditorDraft(other, { ...state._editorSaves['kon-desc'].ack });
    state.selectedTicket = other;
    state.editForm.body = '# Wrong owner\n';
    assert.equal(await state.flushEditSave(), false);
    assert.deepEqual(ui.writes, []);
    state.editing = false;
    ui.close();
  });
}

test('Archive opens a primary namesake of a retained secondary selection', async () => {
  const requests = [];
  const row = { id: 'same', title: 'Primary archived', status: 'archived' };
  const state = loadKontoraState({ fetch: async url => {
    requests.push(url);
    return { ok: true, json: async () => ({ ...row, description: '# Primary\n' }) };
  } });
  state.currentView = 'archive';
  state.selectedTicket = { id: row.id, source: 'team', title: 'Secondary', status: 'open' };
  state.fetchTicketCost = () => {};
  await state.archiveOpenRow(row);
  assert.equal(state.archiveDetailOpen(), true);
  assert.equal(state.isSecondary(state.selectedTicket), false);
  assert.equal(state.selectedTicket.description, '# Primary\n');
  assert.deepEqual(requests, ['/api/tickets/same']);
});

test('mobile description reuses an active draft while an older save is pending', async () => {
  const ui = mobileDescriptionUI();
  await ui.open();
  const { state } = ui;
  const pending = deferred();
  ui.intercept(async () => { await pending.promise; return true; });
  state.mobileDescriptionInput('# Submitted\n');
  const saving = state.mobileFinishDescriptionEdit();
  await flushMicrotasks();
  const form = state.editForm, generation = state._editorGeneration;
  state.mobileDescriptionInput('# Still typing\n');
  state.startEditing = () => assert.fail('must not reload an active draft');
  assert.equal(await state.mobileBeginDescriptionEdit(), true);
  assert.equal(state.editForm, form);
  assert.equal(state._editorGeneration, generation);
  assert.equal(state.editForm.body, '# Still typing\n');
  pending.resolve();
  await saving;
  assert.equal(state.editSaved, false, 'older acknowledgement is not Saved for the live draft');
  await state.mobileFinishDescriptionEdit();
  assert.deepEqual(ui.writes.map(w => w.description), ['# Submitted\n', '# Still typing\n']);
  ui.close();
});

for (const boundary of ['tab', 'close', 'selection', 'breakpoint']) {
  test(`mobile description flushes on ${boundary} without reinitializing the editor`, async () => {
    const ui = mobileDescriptionUI();
    await ui.open();
    const { state } = ui;
    const pending = deferred();
    ui.intercept(async () => { await pending.promise; return true; });
    state.mobileDescriptionInput('# Flush this draft\n');
    const form = state.editForm;
    state.startEditing = () => assert.fail('a boundary must flush, not restart the editor');
    state._mountBoard = () => {};
    const other = { id: 'next', status: 'in_progress', body: 'Other ticket' };
    ui.respond(async (url, _opts, full) => ({ ok: true, json: async () => url.endsWith('/next') ? other : full }));
    if (boundary === 'tab') state.mobileSwitchTab('logs');
    if (boundary === 'close') state.closeDetail(false);
    if (boundary === 'selection') await state.selectTicket(other);
    if (boundary === 'breakpoint') { state.isMobile = false; state._onBreakpointChange(); }
    assert.equal(state._editDebounce, null);
    assert.equal(state.editingBody, false);
    assert.equal(state.editForm, form);
    await flushMicrotasks();
    assert.deepEqual(ui.writes, [{ description: '# Flush this draft\n' }]);
    pending.resolve();
    await state._waitEditorSaves('kon-desc');
    assert.equal(ui.stored().description, '# Flush this draft\n');
    if (boundary === 'close') assert.equal(state.selectedTicket, null);
    else if (boundary === 'selection') assert.equal(state.selectedTicket.id, 'next');
    else {
      state.isMobile = true;
      state.mobileSwitchTab('ticket');
      assert.equal(await state.mobileBeginDescriptionEdit(), true);
      assert.equal(state.editForm, form);
    }
    ui.close();
  });
}

for (const lock of ['desktop', 'logs tab', 'loading', 'missing ticket', 'secondary', 'permission', 'running', 'claim']) {
  test(`mobile description entry and input respect ${lock} lock`, async () => {
    const ui = mobileDescriptionUI();
    await ui.open();
    const { state } = ui;
    if (lock === 'desktop') state.isMobile = false;
    if (lock === 'logs tab') state.detailTab = 'logs';
    if (lock === 'loading') state.detailLoading = true;
    if (lock === 'missing ticket') state.selectedTicket = null;
    if (lock === 'secondary') Object.assign(state.selectedTicket, { source: 'team', can_edit: true });
    if (lock === 'permission') state.selectedTicket.can_edit = false;
    if (lock === 'running') state.selectedTicket.status = 'in_progress';
    if (lock === 'claim') { state.claimSubmitting = true; state._claimKey = state.ticketKey(state.selectedTicket); }
    const body = state.editForm.body;
    state.editingBody = false;
    assert.equal(state.mobileCanEditDescription(), false);
    assert.equal(state.mobileDescriptionFocused(), false);
    assert.equal(await state.mobileBeginDescriptionEdit(), false);
    state.mobileDescriptionInput('must not enter draft');
    assert.equal(state.editForm.body, body);
    assert.equal(state.editingBody, false);
    assert.equal(state._editDebounce, null);
    assert.deepEqual(ui.writes, []);
    // No pending draft to flush after changing the fixture's identity/permissions.
    state.editing = false;
    ui.close();
  });
}

for (const { editing, boundary } of [false, true].flatMap(editing =>
  ['tab', 'close', 'selection', 'breakpoint', 'destroy', 'permission', 'claim'].map(boundary => ({ editing, boundary })))) {
  test(`mobile description does not focus after delayed entry crosses ${boundary}: editing=${editing}`, async () => {
    const ui = mobileDescriptionUI({ begin: editing });
    await ui.open();
    const { state } = ui;
    const tick = deferred();
    state.$nextTick = () => tick.promise;
    state.$refs.mobileDescriptionEditor.focus = () => assert.fail('stale entry focused');
    const opening = state.mobileBeginDescriptionEdit();
    if (boundary === 'tab') state.mobileSwitchTab('logs');
    if (boundary === 'close') state.closeDetail(false);
    if (boundary === 'selection') { ++state._ticketDetailSeq; state.selectedTicket = { id: 'other', status: 'paused' }; }
    if (boundary === 'breakpoint') state.mobileBreakpointChanged();
    if (boundary === 'destroy') { state.destroyMobile(); state._appDestroyed = true; }
    if (boundary === 'permission') state.selectedTicket.can_edit = false;
    if (boundary === 'claim') { state.claimSubmitting = true; state._claimKey = state.ticketKey(state.selectedTicket); }
    tick.resolve();
    assert.equal(await opening, false);
    state.editing = false;
    ui.close();
  });
}

test('ordinary description draft survives a note acknowledgement without SSE, then a delayed SSE', async () => {
  const ui = descriptionEditorUI();
  await ui.open();
  const { state } = ui;
  state.editForm.body = '# Local description\n';
  state.noteDraft = 'acknowledged note';
  await state.submitNote();
  assert.equal(state.editForm.body, '# Local description\n');
  assert.equal(state.selectedTicket.notes.length, 2);
  const event = state.boardEntry(ui.stored());
  event.notes = ui.stored().notes;
  state.applyTicketUpdate(event);
  assert.equal(state.editForm.body, '# Local description\n');
  await state.saveEdit();
  assert.equal(ui.stored().description, '# Local description\n');
  assert.equal(ui.stored().notes.length, 2);
  assert.match(ui.stored().body, /acknowledged note/);
  assert.match(ui.stored().body, /## Later\r\nkeep later/);
  state.closeDetail(false);
});

test('description queues accept ticket IDs that match Object.prototype names', async () => {
  for (const id of ['constructor', 'toString']) {
    const ui = descriptionEditorUI('', id);
    await ui.open();
    ui.state.editForm.body = '# Saved\n';
    assert.equal(await ui.save(), true);
    assert.equal(ui.stored().description, '# Saved\n');
    ui.close();
  }
});

test('editor re-entry keeps changed saves and queued reverts in order', async () => {
  const ui = descriptionEditorUI();
  await ui.open();
  const { state } = ui;
  const pending = deferred();
  ui.respond(async (_url, opts, full) => {
    if (opts?.method === 'PUT' && full.description === '# Change\n') await pending.promise;
    return { ok: true, json: async () => full };
  });
  state.editForm.body = '# Change\n';
  const changed = ui.save();
  await flushMicrotasks();
  assert.equal(ui.stored().description, '# Change\n');
  state.editForm.body = '# Description\n\n';
  const reverted = ui.save();
  state.applyTicketUpdate({ ...state.boardEntry(ui.stored()), status: 'human_review' });
  state.applyTicketUpdate({ ...state.boardEntry(ui.stored()), status: 'open' });
  state.switchTab('ticket');
  await flushMicrotasks();
  state.editForm.body = '# Latest\n';
  const latest = ui.save();
  await flushMicrotasks();
  assert.equal(ui.writes.length, 1, 're-entry must not create a parallel queue');
  pending.resolve();
  assert.deepEqual(await Promise.all([changed, reverted, latest]), [true, true, true]);
  assert.deepEqual(ui.writes.map(write => write.description), ['# Change\n', '# Description\n\n', '# Latest\n']);
  assert.equal(ui.stored().description, '# Latest\n');
  assert.equal(state.editForm.body, '# Latest\n');
  assert.equal(state._editorSaves['kon-desc'].ack.description, '# Latest\n');
  await ui.close();
});

for (const kind of ['', 'epic']) {
  for (const failure of ['truncated', 'missing identity']) {
    test(`successful description response must decode a ticket: ${kind || 'ordinary'}, ${failure}`, async () => {
      const ui = descriptionEditorUI(kind);
      await ui.open();
      const original = ui.state.selectedTicket || ui.state.selectedEpic;
      ui.respond(async (_url, opts, full) => ({
        ok: true,
        json: async () => {
          if (opts?.method !== 'PUT') return full;
          if (failure === 'truncated') throw new Error('terminated');
          return {};
        },
      }));
      ui.state.editForm.body = '# Persisted but unacknowledged\n';
      assert.equal(await ui.save(), false);
      assert.equal(ui.stored().description, '# Persisted but unacknowledged\n');
      assert.equal((ui.state.selectedTicket || ui.state.selectedEpic).id, original.id);
      assert.equal(ui.state.tickets[0].id, original.id);
      assert.equal(ui.state._editorSaves[original.id].ack.description, '# Description\n\n');
      assert.equal(ui.state.editForm.body, '# Persisted but unacknowledged\n');
      assert.ok(ui.state.error);
      ui.respond(async (_url, _opts, full) => ({ ok: true, json: async () => full }));
      assert.equal(await ui.save(), true);
      await ui.close();
    });
  }

  test(`description acknowledgement refreshes projections after SSE: ${kind || 'ordinary'}`, async () => {
    const ui = descriptionEditorUI(kind);
    await ui.open();
    const { state } = ui;
    const pending = deferred();
    ui.respond(async (_url, opts, full) => {
      if (opts?.method === 'PUT') {
        state.applyTicketUpdate(state.boardEntry(full));
        state.applyTicketUpdate({ ...state.boardEntry(full), status: 'human_review', title: 'Newer status event' });
        await pending.promise;
      }
      return { ok: true, json: async () => full };
    });
    state.editForm.body = '# Saved before SSE\n';
    const saving = ui.save();
    await flushMicrotasks();
    Object.assign(ui.stored(), { status: 'human_review', title: 'Newer status event' });
    pending.resolve();
    assert.equal(await saving, true);
    const selected = state.selectedTicket || state.selectedEpic;
    assert.equal(selected.description, '# Saved before SSE\n');
    assert.equal(selected.body, ui.stored().body);
    assert.equal(selected.status, 'human_review');
    assert.equal(selected.title, 'Newer status event');
    assert.equal(state.tickets[0].status, 'human_review');
    await ui.close();
  });
}

for (const editing of [true, false]) {
  test(`delayed note response preserves an acknowledged description: editing=${editing}`, async () => {
    const ui = descriptionEditorUI();
    await ui.open();
    const { state } = ui;
    const pending = deferred();
    ui.respond(async (_url, opts, full) => {
      if (opts?.method === 'POST') await pending.promise;
      if (opts?.method === 'PUT') {
        full.title = 'New description';
        ui.stored().title = full.title;
      }
      return { ok: true, json: async () => full };
    });
    state.noteDraft = 'Delayed note';
    const noting = state.submitNote();
    await flushMicrotasks();
    state.editForm.body = '# New description\n';
    assert.equal(await ui.save(), true);
    state.editing = editing;
    pending.resolve();
    await noting;
    assert.equal(state.selectedTicket.description, '# New description\n');
    assert.equal(state.selectedTicket.title, 'New description');
    assert.equal(state.tickets[0].title, 'New description');
    assert.equal(state.selectedTicket.body, ui.stored().body);
    assert.equal(state.selectedTicket.notes.length, 2);
    assert.equal(state.editForm.body, '# New description\n');
    assert.match(state.descriptionTail(state.selectedTicket), /Delayed note/);
    await ui.close();
  });
}

test('a queued description revert still follows an outstanding changed draft', async () => {
  const ui = descriptionEditorUI();
  await ui.open();
  const pending = deferred();
  ui.intercept(async () => { await pending.promise; return true; });
  ui.state.editForm.body = '# Change\n';
  const changed = ui.save();
  ui.state.editForm.body = '# Description\n\n';
  const reverted = ui.save();
  await flushMicrotasks();
  assert.equal(ui.writes.length, 1);
  pending.resolve();
  await Promise.all([changed, reverted]);
  assert.equal(ui.stored().description, '# Description\n\n');
  assert.equal(ui.writes.length, 2);
  ui.close();
});

test('description projections stay off browser board entries and protected tails remain visible', () => {
  const state = loadKontoraState();
  const entry = state.boardEntry({ id: 'a', body: 'all', notes: [], description: '', description_tail: '## Notes' });
  assert.deepEqual(vmValue(entry), { id: 'a' });
  const html = fs.readFileSync(htmlPath, 'utf8');
  for (const subject of ['selectedTicket', 'selectedEpic']) {
    assert.ok(html.includes('setProse($el, descriptionTail(' + subject + '))'));
  }
  const mobileBrief = html.slice(html.indexOf('aria-label="Epic brief"'), html.indexOf('<div x-show="epicTab === \'notes\'"'));
  assert.ok(mobileBrief.includes('setProse($el, descriptionTail(selectedEpic))'));
  assert.match(mobileBrief, /Notes and later sections are read only here/);
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
    assert.equal(sent.description, c.body + "typed", c.name);
    assert.equal(Object.hasOwn(sent, 'body'), false, c.name);
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
  state.editForm = {
    body: "", path: fromPath, pipeline: "", branch: "", ...fields,
    agent: fields.agent_override ? (fields.agent || "") : "",
  };
  state._editInherited = state.projectDefaultsFor(fromPath);
  state._editAgentFollowsProject = !fields.agent_override;
  state.saveEdit = () => { state.savedPath = state.editForm.path; };
  return state;
}

test("the edit form preserves pipelines and applies agent defaults without changing the branch", () => {
  const cases = [
    {
      name: "a blank pipeline stays standalone",
      from: "", fields: {}, to: "~/projects/kontora",
      want: { pipeline: "", agent: "claude", branch: "" },
    },
    {
      name: "an absolute path matches the same project",
      from: "", fields: {}, to: "/home/u/projects/kontora",
      want: { pipeline: "", agent: "claude", branch: "" },
    },
    {
      name: "dot segments are cleaned before matching",
      from: "", fields: {}, to: "/home/u/projects/other/../kontora/.",
      want: { pipeline: "", agent: "claude", branch: "" },
    },
    {
      name: "inherited values follow the retarget",
      from: "~/projects/kontora",
      fields: { pipeline: "implement", agent: "claude", branch: "" },
      to: "~/projects/widget-api",
      want: { pipeline: "implement", agent: "pi-opus", branch: "" },
    },
    {
      name: "values the user chose are kept",
      from: "~/projects/kontora",
      fields: { pipeline: "review-only", agent: "codex", agent_override: true, branch: "wip/experiment" },
      to: "~/projects/widget-api",
      want: { pipeline: "review-only", agent: "codex", branch: "wip/experiment" },
    },
    {
      name: "a path outside every project clears inherited defaults",
      from: "~/projects/kontora",
      fields: { pipeline: "implement", agent: "claude", branch: "old/branch" },
      to: "~/projects/other",
      want: { pipeline: "implement", agent: "", branch: "old/branch" },
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

test("create and init pipeline choices keep preview strings separate from request overrides", async () => {
  const cases = [
    { choice: 'inherit', want: undefined, preview: 'child', source: 'epic_child_pipeline' },
    { choice: 'none', want: 'none', preview: '', source: 'epic_child_pipeline' },
    { choice: 'pipeline:child', want: 'child', preview: 'child', source: 'epic_child_pipeline' },
    { choice: 'pipeline:inherit', want: 'inherit', preview: 'inherit', source: 'epic_child_pipeline' },
    { choice: 'pipeline:pipeline:child', want: 'pipeline:child', preview: 'pipeline:child', source: 'epic_child_pipeline' },
    { choice: 'inherit', epicChild: false, want: undefined, preview: 'ordinary', source: 'pipeline' },
    { choice: 'inherit', projectPipeline: '', epicChild: false, want: undefined, preview: 'global', source: 'default_pipeline' },
    { choice: 'inherit', projectPipeline: '', childPipeline: '', want: undefined, preview: 'global', source: 'default_pipeline' },
    { choice: 'inherit', projectPipeline: '', childPipeline: '', global: '', want: undefined, preview: '', source: '' },
    ...['desktop', 'mobile'].flatMap(kindTransition => [
      { kindTransition, choice: 'inherit', want: undefined, preview: 'child', source: 'epic_child_pipeline' },
      { kindTransition, choice: 'none', want: 'none', preview: '', source: 'epic_child_pipeline' },
      { kindTransition, choice: 'pipeline:child', want: 'child', preview: 'child', source: 'epic_child_pipeline' },
    ]),
  ];
  for (const c of cases) {
    const sent = [];
    const ticket = { id: 'kon-child', path: '/repos/api', parent: { id: 'kon-parent' }, epic_child: c.epicChild !== false };
    const state = loadKontoraState({ fetch: async (url, options = {}) => {
      if (!options.method) return { ok: true, json: async () => ticket };
      sent.push({ url, body: JSON.parse(options.body) });
      return { ok: true, json: async () => ({}) };
    } });
    state.configCache = {
      projects: [{ name: 'api', path: ticket.path, pipeline: c.projectPipeline ?? 'ordinary', epic_child_pipeline: c.childPipeline ?? 'child' }],
      default_pipeline: c.global ?? 'global',
      pipelines: ['ordinary', 'child', 'inherit', 'pipeline:child'],
      pipeline_infos: [{ name: 'child', stages: ['implement', 'review'] }],
    };
    state.$nextTick = async () => {};
    await state.openCreateModal();
    state.tickets = [{ id: 'kon-parent', kind: ticket.epic_child ? 'epic' : '' }];
    Object.assign(state.createForm, { title: 'Child', path: ticket.path, parent: 'kon-parent', pipelineChoice: c.choice });
    state.onPipelineChange();
    if (c.kindTransition) {
      const html = fs.readFileSync(htmlPath, 'utf8');
      const handler = c.kindTransition === 'desktop'
        ? html.match(/@click="(createForm\.kind = k\.value[^"]*)"/)[1]
        : html.match(/<select x-model="createForm\.kind"[^>]*>/)[0].match(/@change="([^"]*)"/)?.[1] || '';
      const changeKind = new Function('state', 'k', `with (state) { ${handler} }`);
      state.createForm.kind = 'epic';
      state.createForm.path = '/other';
      state.onCreatePathChange();
      state.createForm.path = ticket.path;
      state.onCreatePathChange();
      if (c.kindTransition === 'mobile') state.createForm.kind = '';
      changeKind(state, { value: '' });
      assert.equal(state.createForm.pipelineChoice, c.choice);
      assert.equal(state.agentPickerPipeline(c.kindTransition === 'mobile' ? 'mobile-create' : 'create'), c.preview);
      assert.match(state.agentPickerHint('create'), c.preview ? /each stage/ : /single run/);
    }
    assert.equal(state.createForm.pipeline, c.preview, JSON.stringify(c));
    assert.equal(state.createPipelineDefaults().pipelineSource, c.source);
    assert.equal(state.createPreviewYaml.includes('pipeline: ' + c.preview), !!c.preview);
    await state.openInitModal({ ...ticket, epic_child: false });
    state.initForm.pipelineChoice = c.choice;
    state.onInitPipelineChange();
    assert.equal(state.initForm.pipeline, c.preview);
    assert.deepEqual(vmValue(state.initStages()), c.preview === 'child' ? ['implement', 'review'] : []);
    if (c.source === 'default_pipeline') {
      assert.equal(state.initPipelineProject(), null);
      assert.match(state.inheritedPipelineLabel(state._initInherited), /global default/);
    }
    state.createForm.path = '/other';
    state.onCreatePathChange();
    state.initForm.path = '/other';
    state.onInitPathChange();
    const retargeted = c.choice === 'inherit' ? (c.global ?? 'global') : c.preview;
    assert.equal(state.createForm.pipeline, retargeted, 'path changes only replace inherited previews');
    assert.equal(state.initForm.pipeline, retargeted, 'init preserves explicit choices');
    // A preview can be stale at submission. The server must still resolve inheritance.
    state.configCache.default_pipeline = 'changed-after-preview';
    await state.submitCreateTicket();
    await state.submitInitTicket();
    assert.equal(sent.length, 2);
    for (const { body } of sent) {
      assert.equal(body.pipeline, c.want);
      assert.equal(Object.hasOwn(body, 'pipeline'), c.want !== undefined);
    }
  }
  const html = fs.readFileSync(htmlPath, 'utf8');
  assert.equal((html.match(/x-model="createForm.pipelineChoice"/g) || []).length, 2, 'desktop and mobile use choice tokens');
  assert.match(html, /x-model="initForm.pipelineChoice" @change="onInitPipelineChange\(\)"/);
});

test("init refreshes child membership on relevant events without changing explicit choices or stored pipelines", async () => {
  for (const choice of ['inherit', 'none', 'pipeline:child']) {
    const listeners = {};
    const requests = [];
    let fresh = { id: 'kon-child', status: 'open', path: '/repo', pipeline: '', description: '', parent: { id: 'kon-parent' }, epic_child: true };
    const state = loadKontoraState({
      EventSource: class { addEventListener(name, fn) { listeners[name] = fn; } close() {} },
      fetch: async (url, options = {}) => {
        requests.push({ url, ...options });
        return { ok: true, json: async () => ({ ...fresh }) };
      },
    });
    state.configCache = { projects: [{ name: 'repo', path: '/repo', pipeline: 'ordinary', epic_child_pipeline: 'child' }] };
    state.$nextTick = async () => {};
    state.recomputeBoard = () => {};
    state.updateFavicon = () => {};
    state.connectSSE();
    await state.openInitModal({ ...fresh, epic_child: false });
    assert.equal(requests[0].url, '/api/tickets/kon-child');
    assert.equal(state.initForm.pipeline, 'child', 'opening uses fresh detail membership');
    state.initForm.pipelineChoice = choice;
    state.onInitPipelineChange();
    state.selectedTicket = { ...fresh };
    await state.startEditing();
    state.editForm.body = 'local body';
    for (const event of ['unrelated', 'parent-update', 'child-update', 'parent-delete', 'parent-restore']) {
      fresh = { ...fresh, epic_child: event === 'parent-restore' };
      const before = requests.length;
      if (event === 'parent-delete') listeners.ticket_deleted({ data: JSON.stringify({ id: 'kon-parent' }) });
      else state.applyTicketUpdate(event === 'child-update' ? { ...fresh }
        : { id: event === 'unrelated' ? 'kon-other' : 'kon-parent', kind: event === 'parent-restore' ? 'epic' : '', status: 'open' });
      await flushMicrotasks();
      assert.equal(requests.length, before + (event === 'unrelated' ? 0 : 1), event);
      const inherited = event === 'unrelated' || event === 'parent-restore' ? 'child' : 'ordinary';
      assert.equal(state.initForm.pipeline, choice === 'inherit' ? inherited : choice === 'none' ? '' : 'child', event);
      assert.equal(state.initForm.pipelineChoice, choice, event);
      assert.equal(state.editForm.pipeline, '', event);
      assert.equal(state.selectedTicket.pipeline, '', event);
    }
    await state.saveEdit();
    const put = requests.find(r => r.method === 'PUT');
    assert.ok(put);
    assert.equal(Object.hasOwn(JSON.parse(put.body), 'pipeline'), false, 'body autosave does not write the preview');
    listeners.ticket_deleted({ data: JSON.stringify({ id: 'kon-child' }) });
    assert.equal(state.initModal, false, 'deleting the child closes its start form');
  }
});

test("init ignores stale refresh responses and responses after closing", async () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const escape = new Function('state', `with (state) { ${html.match(/@keydown\.escape\.window="([^"]+)"/)[1]} }`);
  const cancellations = [
    { name: 'close init', cancel: state => state.closeInitModal() },
    { name: 'Escape', cancel: state => escape(state) },
    { name: 'Escape from board', cancel: state => { state.selectedTicket = null; escape(state); } },
    { name: 'close detail', cancel: state => state.closeDetail() },
    { name: 'navigate to Stats', cancel: state => state.gotoView('stats') },
  ];
  for (const phase of ['response', 'body']) {
    for (const cancellation of cancellations) {
      const delayed = deferred();
      const ticket = { id: 'kon-child', status: 'open', path: '/repo' };
      const state = loadKontoraState({ fetch: async () => {
        if (phase === 'response') await delayed.promise;
        return { ok: true, json: async () => {
          if (phase === 'body') await delayed.promise;
          return ticket;
        } };
      } });
      state.configCache = { projects: [] };
      state.$nextTick = async () => {};
      state.writeHash = () => {};
      state.openStats = () => {};
      state.selectedTicket = ticket;
      const opening = state.openInitModal(ticket);
      await flushMicrotasks();
      await cancellation.cancel(state);
      delayed.resolve();
      await opening;
      assert.equal(state.initModal, false, `${phase}: ${cancellation.name}`);
      assert.equal(state.initForm.ticketId, '', 'canceled opening does not replace the form');
      assert.equal(state.initSubmitting, false);
      assert.equal(state._initOpening, false);
    }
  }
  const openings = [];
  const superseded = loadKontoraState({ fetch: () => {
    const response = deferred();
    openings.push(response);
    return response.promise;
  } });
  superseded.configCache = { projects: [] };
  superseded.$nextTick = async () => {};
  const first = superseded.openInitModal({ id: 'kon-old', path: '/repo' });
  const second = superseded.openInitModal({ id: 'kon-new', path: '/repo' });
  openings[0].resolve({ status: 404, ok: false });
  await first;
  assert.equal(superseded._initOpening, true, 'stale completion cannot clear the newer pending opening');
  assert.equal(superseded.error, null, 'a stale 404 does not report an error');
  assert.equal(superseded.initModal, false);
  openings[1].resolve({ ok: true, json: async () => ({ id: 'kon-new', path: '/repo' }) });
  await second;
  assert.equal(superseded.initModal, true);
  assert.equal(superseded.initForm.ticketId, 'kon-new');
  assert.equal(superseded._initOpening, false);
  const pending = [];
  const ticket = { id: 'kon-child', path: '/repo', parent: { id: 'kon-parent' }, epic_child: false };
  let hold = false;
  const controlled = loadKontoraState({ fetch: () => hold
    ? new Promise(resolve => pending.push(resolve))
    : Promise.resolve({ ok: true, json: async () => ticket }) });
  controlled.configCache = { projects: [{ name: 'repo', path: '/repo', pipeline: 'ordinary', epic_child_pipeline: 'child' }] };
  controlled.$nextTick = async () => {};
  await controlled.openInitModal(ticket);
  hold = true;
  const old = controlled.refreshInitContext('kon-parent');
  const latest = controlled.refreshInitContext('kon-child');
  pending[1]({ ok: true, json: async () => ({ ...ticket, epic_child: true }) });
  await latest;
  assert.equal(controlled.initForm.pipeline, 'child');
  pending[0]({ ok: true, json: async () => ticket });
  await old;
  assert.equal(controlled.initForm.pipeline, 'child', 'older membership cannot replace the latest preview');
  const closing = controlled.refreshInitContext('kon-parent');
  controlled.closeInitModal();
  pending[2]({ ok: true, json: async () => ticket });
  await closing;
  assert.equal(controlled.initModal, false);
  assert.equal(controlled.initForm.pipeline, 'child');
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
    state.configCache = {};
    await state.openCreateModal();
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

test("edit path requests preserve pipelines, omit empty branches and send custom branches", async () => {
  const cases = [
    { name: "empty branch", selected: "", edited: "", wantBranch: undefined },
    { name: "custom branch", selected: "", edited: "my/custom-branch", wantBranch: "my/custom-branch" },
    { name: "stored pipeline equals the old default", pipeline: "implement", selected: "", edited: "", wantBranch: undefined },
    { name: "explicit named pipeline", pipeline: "review-only", selected: "", edited: "", wantBranch: undefined },
  ];

  for (const c of cases) {
    let sent = null;
    const state = loadKontoraState({
      fetch: async (_url, options) => {
        sent = JSON.parse(options.body);
        return { ok: true, json: async () => ({ id: "kon-1", path: "~/projects/widget-api", branch: c.edited, pipeline: c.pipeline || '' }) };
      },
    });
    state.configCache = { projects: EDIT_PROJECTS };
    state.selectedTicket = { id: "kon-1", status: 'open', description: '', path: "~/projects/kontora", branch: c.selected, pipeline: c.pipeline || '' };
    state.$nextTick = async () => {};
    await state.startEditing();
    state.editForm = {
      body: "",
      pipeline: c.pipeline || '',
      path: "~/projects/widget-api",
      agent: "",
      branch: c.edited,
      base_branch: "",
    };
    state.tickets = [];

    await state.onEditPathChange();

    assert.ok(sent, c.name);
    assert.equal(Object.hasOwn(sent, 'pipeline'), false, c.name);
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
  assert.ok(options.includes(`<option value="inherit" x-text="inheritedPipelineLabel(_initInherited)">`));
  assert.ok(options.includes(`<option value="none">none (single run, no stages)</option>`));
  assert.ok(options.includes(`<option :value="'pipeline:' + p" x-text="p">`));
  assert.equal(options.includes("pipelineLabel"), false);
  // Agent shares a row with Path. The shared picker keeps the shorter label
  // and puts its resolved hint on a separate line.
  assert.match(html, /x-data="\{ pickerHost: 'init' \}" x-html="agentPickerControl"/);
  assert.match(html, /x-text="agentPickerHint\('init'\)"/);
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
      name: "a stored pipeline equal to the old default is still explicit",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora", pipeline: "implement" },
      retarget: "~/projects/widget-api",
      want: null,
      wantPipeline: "implement",
    },
    {
      name: "selecting the default by name is explicit",
      ticket: { id: "kon-1", status: "open", path: "~/projects/kontora" },
      pipeline: "implement",
      want: null,
    },
    {
      name: "global fallback is not a project default",
      ticket: { id: "kon-1", status: "open", path: "/repos/global" },
      want: null,
      wantPipeline: "implement",
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
    let submitted;
    const state = loadKontoraState({ fetch: async (_url, options = {}) => {
      if (options.method) submitted = JSON.parse(options.body);
      return { ok: true, json: async () => options.method ? {} : c.ticket };
    } });
    state.configCache = {
      projects: [...EDIT_PROJECTS, { name: 'global', path: '/repos/global' }],
      pipelines: ["implement", "commit-no-push"], agents: [], default_pipeline: 'implement',
    };
    state.$nextTick = async () => {};

    await state.openInitModal(c.ticket);
    if (c.retarget) {
      state.initForm.path = c.retarget;
      state.onInitPathChange();
    }
    if (c.pipeline !== undefined) {
      state.initForm.pipelineChoice = c.pipeline ? 'pipeline:' + c.pipeline : 'none';
      state.onInitPipelineChange();
    }

    assert.equal(state.initPipelineProject()?.name ?? null, c.want, c.name);
    if (c.wantPipeline) assert.equal(state.initForm.pipeline, c.wantPipeline, c.name);
    assert.equal(state.initForm.canInheritPipeline, !c.ticket.pipeline, c.name);
    await state.submitInitTicket();
    const wantOverride = c.pipeline !== undefined ? (c.pipeline || 'none') : (c.ticket.pipeline || undefined);
    assert.equal(submitted.pipeline, wantOverride, c.name);
    assert.equal(Object.hasOwn(submitted, 'pipeline'), wantOverride !== undefined, c.name);
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
    state.configCache = {};
    state.$nextTick = async () => {};
    await state.startEditing();
    state.editingBody = true;
    state.editForm.body = 'typed';

    state.debounceSaveEdit();
    await state.selectTicket(other);
    c.act(state);
    await new Promise((resolve) => setTimeout(resolve, c.wait));

    assert.deepEqual(puts, [{ url: "/api/tickets/kon-1", body: { description: "typed" } }], c.name);
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
  // any combined total the same. The assistant's own prose runs through
  // the same write in both layers, and a streaming pane is exactly the case a
  // rebuild-per-effect would ruin; the archive's read-only ticket tab renders
  // its body through it too; and the notes tab's own note and reply bodies
  // are markdown like every other authored block; and the epic page renders
  // both its brief and its own notes the same way in both layouts.
  assert.equal(html.match(/x-effect="setProse\(\$el, /g).length, 20);
  assert.equal(html.match(/setSummaryProse\(\$el, /g).length, 2);
  assert.match(html, /card\.imported \? setProse\(\$el, card\.summary\) : setSummaryProse/);
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

test("a patched archive row is filtered and sorted on its new values", () => {
  // archiveDerive caches the lowercased text and the parsed timestamp per row
  // object, so every path that changes a row has to replace it rather than
  // write into it. archivePatchRow is the one that does, on the SSE event for a
  // ticket archived while the view is open.
  const state = loadKontoraState();
  state.archiveRows = [
    archiveRow({ id: "kon-001", title: "Old work", archived_at: "2026-08-20T09:00:00Z" }),
    archiveRow({ id: "kon-002", title: "Other work", archived_at: "2026-08-19T09:00:00Z" }),
  ];
  state.archiveQuery = "renamed";
  assert.deepEqual(state.archiveView().rows.map((r) => r.id), []);

  state.archivePatchRow({
    id: "kon-001", title: "Renamed work", archived_from: "cancelled",
    archived_at: "2026-08-18T09:00:00Z", archived_by: "web",
  });

  assert.deepEqual(state.archiveView().rows.map((r) => r.id), ["kon-001"]);
  state.archiveQuery = "";
  // The new archived_at is older than kon-002's, so it sorts second now.
  assert.deepEqual(state.archiveView().rows.map((r) => r.id), ["kon-002", "kon-001"]);
  assert.equal(state.archiveView().rows[1].status, "cancelled");
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

test("the heavy views are built and torn down, not hidden in place", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  // x-show leaves every node and every binding the view ever drew on the page.
  for (const view of ["stats", "archive", "settings"]) {
    assert.match(html, new RegExp(`<template x-if="!loading && currentView === '${view}'">`), view);
    assert.equal(html.includes(`x-show="!loading && currentView === '${view}'"`), false, view + " is not x-show");
  }
  // The desktop ticket page goes the same way. closeDetail calls closeTerminal
  // before it clears selectedTicket, so the xterm session is disposed before
  // this subtree is removed rather than left attached to a detached container.
  assert.match(html, /<template x-if="selectedTicket">/);
  assert.equal(html.includes('x-show="selectedTicket" x-cloak'), false, "the ticket page is not x-show");

  // The board is the one view worth keeping around: it is toggled constantly
  // and it is not the one that draws thousands of rows.
  assert.match(html, /x-show="!loading && currentView === 'board'"/);
});

// x-if removes the ticket page the moment selectedTicket goes, and the xterm
// instance lives inside it. Disposing it after would leave the session attached
// to a detached container.
test("closing the ticket page disposes the terminal while the page is still there", () => {
  const state = loadKontoraState();
  state.selectedTicket = { id: "kon-001", title: "Some ticket" };
  let selectedAtDisposal = "closeTerminal was never called";
  state.closeTerminal = () => { selectedAtDisposal = state.selectedTicket; };

  state.closeDetail();

  assert.equal(selectedAtDisposal && selectedAtDisposal.id, "kon-001");
  assert.equal(state.selectedTicket, null);
});

test("the archive list draws its window, and offers the rest behind show more", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /x-for="row in archiveVisibleRows\(\)"/);
  assert.equal(html.includes('x-for="row in archiveView().rows"'), false, "the whole list is never drawn");
  assert.match(html, /@click="archiveShowMore\(\)"/);
  assert.match(html, /x-show="archiveHiddenCount\(\) > 0"/);

  // One click binding for the row and its three buttons, not four.
  const rowStart = html.indexOf('x-for="row in archiveVisibleRows()"');
  const row = html.slice(rowStart, html.indexOf("</template>", rowStart));
  assert.match(row, /@click="archiveRowClick\(row, \$event\)"/);
  assert.equal((row.match(/@click="/g) || []).length, 1, "the action buttons carry data-act, not a binding");
  assert.equal((row.match(/data-act="/g) || []).length, 3);
});

// Rendering the whole archive is what made the page cost hundreds of megabytes:
// Alpine holds every binding of every row it draws, and the list is thousands of
// rows long on a real tickets_dir.
test("the list renders a window of the filtered rows and grows it a page at a time", () => {
  const state = loadKontoraState();
  state.archiveRows = Array.from({ length: 250 }, (_, i) =>
    archiveRow({ id: "kon-" + String(i).padStart(3, "0") }));

  assert.equal(state.archiveView().shown, 250, "every row is still filtered and counted");
  assert.equal(state.archiveVisibleRows().length, 100, "only the window is drawn");
  assert.equal(state.archiveHiddenCount(), 150);
  assert.equal(state.archiveCountLine(), "250 archived · 250 shown · 100 drawn");

  state.archiveShowMore();
  assert.equal(state.archiveVisibleRows().length, 200);
  assert.equal(state.archiveHiddenCount(), 50);

  state.archiveShowMore();
  assert.equal(state.archiveVisibleRows().length, 250, "the window never runs past the list");
  assert.equal(state.archiveHiddenCount(), 0);
  assert.equal(state.archiveCountLine(), "250 archived · 250 shown", "no drawn count once it all is");
});

test("every change that re-derives the archive puts the window back", () => {
  const rows = Array.from({ length: 250 }, (_, i) =>
    archiveRow({ id: "kon-" + String(i).padStart(3, "0"), project: "web", agent: "claude" }));

  const cases = [
    { name: "a pill", act: s => s.archiveSetFilter("status", "done") },
    { name: "the date range", act: s => s.archiveSetRange("30d") },
    { name: "a sidebar facet", act: s => s.archiveToggleFacet("project", "web") },
    { name: "a sort column", act: s => s.archiveSort("title") },
    { name: "the sort direction", act: s => s.archiveToggleSortDir() },
    { name: "clear all", act: s => s.archiveClearFilters() },
    { name: "cutting a chip", act: s => { s.archiveQuery = "agent:codex"; s.archiveRemoveToken("agent:codex"); } },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.archiveRows = rows;
    state.archiveShowMore();
    assert.equal(state.archiveVisibleRows().length, 200, c.name + ": grown first");
    c.act(state);
    assert.equal(state.archiveWindow, 100, c.name + " puts the window back");
  }
});

test("leaving the archive drops the rows and the list derived from them", () => {
  const state = loadKontoraState();
  state.currentView = "archive";
  state.archiveRows = [archiveRow({ id: "kon-001" }), archiveRow({ id: "kon-002" })];
  state.archiveShowMore();
  assert.equal(state.archiveView().shown, 2);

  state.closeArchive();
  assert.deepEqual([...state.archiveRows], []);
  assert.equal(state.archiveWindow, 100);
  // The memo is module scope, so a stale entry would outlive the rows it holds.
  assert.equal(state.archiveView().shown, 0);
  assert.equal(state.archiveView().total, 0);
});

test("one row click serves the row and its three action buttons", () => {
  const state = loadKontoraState();
  const calls = [];
  state.archiveOpenRow = row => calls.push(["open", row.id]);
  state.archiveRestore = id => calls.push(["restore", id]);
  state.archiveCopyRow = row => calls.push(["copy", row.id]);
  const row = archiveRow({ id: "kon-001" });

  // The buttons carry a data-act rather than a click binding of their own.
  const click = act => ({ target: { closest: sel => (sel === "[data-act]" && act ? { getAttribute: () => act } : null) } });
  state.archiveRowClick(row, click(null));
  state.archiveRowClick(row, click("restore"));
  state.archiveRowClick(row, click("copy"));
  state.archiveRowClick(row, click("open"));

  assert.deepEqual(calls, [["open", "kon-001"], ["restore", "kon-001"], ["copy", "kon-001"], ["open", "kon-001"]]);
});

test("the pipeline dot binds its colour and its presence together, and hides with no pipeline", () => {
  const state = loadKontoraState();
  const dot = state.archivePipeDot(archiveRow({ pipeline: "review" }));
  assert.equal(dot["data-pipe-color"], state.pipelineColorByName("review"));
  assert.match(dot.style, /--pipe-h/);

  assert.deepEqual({ ...state.archivePipeDot(archiveRow({ pipeline: "" })) }, { style: "display: none" });
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
    companion: { enabled: false, name: "Kotik" },
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

  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /Set to \[\]\. Output pattern matching is off\. Structured session errors from Claude and Pi still pause the ticket, as do non-zero exits\./);

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
  const closed = fs.readFileSync(htmlPath, "utf8").split(`x-if="settingsOpenStage !== name"`)[1].split("</button>")[0];
  assert.match(closed, /x-for="field in \['model', 'effort'\]"/);

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

test("config_reloaded re-reads a clean Settings form, including a hidden form", async (t) => {
  for (const view of ["settings", "board"]) await t.test(view, async () => {
    const listeners = {};
    const text = SETTINGS_FIXTURE + "instance_name: edited-elsewhere\n";
    const daemon = settingsFetch({ raw: text });
    const state = settingsReactive(await settingsState(SETTINGS_FIXTURE, {
      fetch: daemon.fetch,
      EventSource: class {
        addEventListener(name, fn) { listeners[name] = fn; }
        close() {}
      },
    }));
    state.currentView = view;
    state.settingsSection = "general";
    state.connectSSE();
    listeners.config_reloaded({ data: "{}" });
    await flushMicrotasks();

    assert.equal(state.currentView, view);
    assert.equal(state.settingsSection, "general");
    assert.equal(state.settingsConfig.general.instance_name, "edited-elsewhere");
    assert.equal(state.settingsBaseline.general.instance_name, "edited-elsewhere");
    assert.equal(state._settingsRawText, text);
    assert.equal(state.settingsDirty(), false);
    assert.equal(state.settingsLoading, false);
    assert.equal(await state._settingsWrite(), text);
    if (view === "board") {
      await state.openSettings();
      assert.equal(state.settingsConfig.general.instance_name, "edited-elsewhere");
    }
  });
});

test("a reload leaves unopened, dirty, or saving Settings forms alone", async (t) => {
  for (const mode of ["unopened", "dirty", "saving"]) await t.test(mode, async () => {
    const daemon = settingsFetch();
    const state = mode === "unopened" ? loadKontoraState({ fetch: daemon.fetch }) :
      await settingsState(SETTINGS_FIXTURE, { fetch: daemon.fetch });
    if (mode === "dirty") state.settingsConfig.general.branch_prefix = "unsaved";
    if (mode === "saving") state.settingsSaving = true;
    const config = state.settingsConfig;
    const raw = state._settingsRawText;
    await state._settingsRefreshForm();
    assert.equal(daemon.sent.length, 0);
    assert.equal(state.settingsConfig, config);
    assert.equal(state._settingsRawText, raw);
    if (mode === "dirty") assert.equal(state.settingsConfig.general.branch_prefix, "unsaved");
  });
});

test("edits made during any refresh await keep their model and baseline", async (t) => {
  for (const phase of ["fetch", "json", "yaml"]) await t.test(phase, async () => {
    const gate = deferred();
    const response = { ok: true, status: 200, json: async () => ({ content: "instance_name: external\n" }) };
    const state = await settingsState(SETTINGS_FIXTURE, {
      fetch: async () => phase === "fetch" ? gate.promise :
        phase === "json" ? { ...response, json: () => gate.promise } : response,
    });
    const yaml = state._settingsYAML;
    if (phase === "yaml") state._settingsLoadYAML = () => gate.promise;
    const config = state.settingsConfig;
    const baseline = state.settingsBaseline;
    const refresh = state._settingsRefreshForm();
    await flushMicrotasks();
    state.settingsConfig.general.branch_prefix = "unsaved";
    gate.resolve(phase === "fetch" ? response : phase === "json" ? { content: "instance_name: external\n" } : yaml);
    await refresh;

    assert.equal(state.settingsConfig, config);
    assert.equal(state.settingsBaseline, baseline);
    assert.equal(state._settingsRawText, SETTINGS_FIXTURE);
    assert.equal(state.settingsConfig.general.branch_prefix, "unsaved");
    assert.deepEqual([...state.settingsChangedPaths()], ["branch_prefix"]);
    assert.equal(state.settingsLoading, false);
  });
});

test("the newest reload wins over a delayed response or error", async (t) => {
  for (const outcome of ["success", "error"]) await t.test(outcome, async () => {
    const gate = deferred();
    let reads = 0;
    const state = await settingsState(SETTINGS_FIXTURE, {
      fetch: async () => ++reads === 1 ? gate.promise :
        { ok: true, status: 200, json: async () => ({ content: "instance_name: newest\n" }) },
    });
    const first = state._settingsRefreshForm();
    await state._settingsRefreshForm();
    assert.equal(state.settingsConfig.general.instance_name, "newest");
    if (outcome === "error") gate.reject(new Error("old failure"));
    else gate.resolve({ ok: true, status: 200, json: async () => ({ content: "instance_name: oldest\n" }) });
    await first;
    assert.equal(state.settingsConfig.general.instance_name, "newest");
    assert.equal(state._settingsRawText, "instance_name: newest\n");
    assert.equal(state.settingsState, "ok");
    assert.equal(state.settingsLoadError, "");
    assert.equal(state.settingsLoading, false);
  });
});

test("a reload skipped for edits invalidates an older response even after undo", async () => {
  const gate = deferred();
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: () => gate.promise });
  const config = state.settingsConfig;
  const original = config.general.branch_prefix;
  const refresh = state._settingsRefreshForm();
  config.general.branch_prefix = "draft";
  await state._settingsRefreshForm();
  config.general.branch_prefix = original;
  gate.resolve({ ok: true, status: 200, json: async () => ({ content: "instance_name: stale\n" }) });
  await refresh;
  assert.equal(state.settingsConfig, config);
  assert.equal(state._settingsRawText, SETTINGS_FIXTURE);
  assert.equal(state.settingsLoading, false);
});

test("draft inputs survive reloads before and during a fetch", async (t) => {
  const drafts = {
    channel: state => { state.settingsNewChannelName = "new-channel"; },
    header: state => { state.settingsNewHeaderKey.ops = "X-Draft"; },
    agent: state => { state.settingsNewPerAgentKey.summary_model = "pi-fast"; },
    stage: state => { state.settingsNewStage = "new-stage"; },
    environment: state => { state.settingsNewEnvKey = "NEW_KEY"; },
    status: state => { state.settingsNewStatus = "needs_check"; },
  };
  for (const [name, edit] of Object.entries(drafts)) {
    for (const phase of ["before", "during"]) await t.test(`${name}: ${phase}`, async () => {
      const gate = deferred();
      let reads = 0;
      const state = await settingsState(SETTINGS_FIXTURE, { fetch: () => { reads++; return gate.promise; } });
      const config = state.settingsConfig;
      if (phase === "before") edit(state);
      const refresh = state._settingsRefreshForm();
      if (phase === "during") edit(state);
      const draft = JSON.stringify([state.settingsNewChannelName, state.settingsNewHeaderKey,
        state.settingsNewPerAgentKey, state.settingsNewStage, state.settingsNewEnvKey, state.settingsNewStatus]);
      gate.resolve({ ok: true, status: 200, json: async () => ({ content: "instance_name: external\n" }) });
      await refresh;
      assert.equal(reads, phase === "before" ? 0 : 1);
      assert.equal(state.settingsConfig, config);
      assert.equal(JSON.stringify([state.settingsNewChannelName, state.settingsNewHeaderKey,
        state.settingsNewPerAgentKey, state.settingsNewStage, state.settingsNewEnvKey, state.settingsNewStatus]), draft);
      assert.equal(state.settingsLoading, false);
    });
  }
});

test("a failed background refresh keeps the editor and can retry on reopen", async (t) => {
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.match(html, /x-show="!settingsLoading && settingsLoadError"/);
  assert.match(html, /@click="_settingsRefreshForm\(true\)"/);
  for (const failure of ["network", "http", "yaml"]) await t.test(failure, async () => {
    let reads = 0;
    const state = await settingsState(SETTINGS_FIXTURE, {
      fetch: async () => {
        if (++reads > 1) return { ok: true, status: 200, json: async () => ({ content: "instance_name: recovered\n" }) };
        if (failure === "network") throw new Error("offline");
        if (failure === "http") return { ok: false, status: 503 };
        return { ok: true, status: 200, json: async () => ({ content: "invalid: [\n" }) };
      },
    });
    const config = state.settingsConfig;
    await state._settingsRefreshForm();
    assert.equal(state.settingsState, "ok");
    assert.equal(state.settingsConfig, config);
    assert.equal(state._settingsRawText, SETTINGS_FIXTURE);
    assert.ok(state.settingsLoadError);
    assert.equal(state.settingsLoading, false);
    await state.openSettings();
    assert.equal(reads, 2);
    assert.equal(state.settingsConfig.general.instance_name, "recovered");
    assert.equal(state.settingsLoadError, "");
  });
});

test("an older refresh cannot replace a saved or discarded form", async (t) => {
  for (const action of ["save", "discard"]) await t.test(action, async () => {
    const gate = deferred();
    const daemon = settingsFetch();
    let reads = 0;
    const state = await settingsState(SETTINGS_FIXTURE, {
      fetch: (url, init) => url === "/api/config/raw" && !init?.method && ++reads === 1 ? gate.promise : daemon.fetch(url, init),
    });
    const refresh = state._settingsRefreshForm();
    state.settingsConfig.general.branch_prefix = "saved";
    if (action === "save") assert.equal(await state.saveSettings(), true);
    else state.discardSettings();
    const raw = state._settingsRawText;
    const config = state.settingsConfig;
    gate.resolve({ ok: true, status: 200, json: async () => ({ content: "instance_name: old-response\n" }) });
    await refresh;
    assert.equal(state.settingsConfig, config);
    assert.equal(state._settingsRawText, raw);
    assert.equal(state.settingsDirty(), false);
  });
});

test("duplicate reload events keep the save confirmation and open controls", async () => {
  const state = await settingsState(SETTINGS_FIXTURE, { fetch: settingsFetch().fetch });
  state.settingsSavedAt = "12:34";
  state.settingsSavedRestart = true;
  state.settingsOpenStage = "plan";
  const config = state.settingsConfig;
  await Promise.all([state._settingsRefreshForm(), state._settingsRefreshForm()]);
  assert.equal(state.settingsConfig, config);
  assert.equal(state.settingsSavedAt, "12:34");
  assert.equal(state.settingsSavedRestart, true);
  assert.equal(state.settingsOpenStage, "plan");
  assert.equal(state.settingsLoading, false);
});

test("a reload during the initial Settings load supersedes the old read", async () => {
  const gate = deferred();
  let reads = 0;
  const state = loadKontoraState({
    fetch: async () => ++reads === 1 ? gate.promise :
      { ok: true, status: 200, json: async () => ({ content: "instance_name: newest\n" }) },
  });
  state._settingsYAML = await import(yamlPath);
  const open = state.openSettings();
  await state._settingsRefreshForm();
  gate.resolve({ ok: true, status: 200, json: async () => ({ content: "instance_name: oldest\n" }) });
  await open;
  assert.equal(state.settingsConfig.general.instance_name, "newest");
  assert.equal(state.settingsLoading, false);
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

test("saving a restart-only field says so", async (t) => {
  for (const [section, key, value] of [["web", "port", "9090"], ["plannotator", "reviews_dir", "~/new-reviews"]]) {
    await t.test(section + "." + key, async () => {
      const state = await settingsState(SETTINGS_FIXTURE, { fetch: settingsFetch().fetch });
      state.settingsConfig[section][key] = value;
      assert.equal(await state.saveSettings(), true);
      assert.equal(state.settingsSavedRestart, true);
    });
  }
});

test("the restart list contains exactly the pinned fields the form edits", async () => {
  const state = await settingsState();
  const pinned = [
    "tickets_dir", "secondary_tickets_dirs", "worktrees_dir", "logs_dir", "instance_name",
    "web.host", "web.port", "web.token", "plannotator.reviews_dir",
  ];
  for (const path of pinned) assert.equal(state.settingsIsRestartOnly(path), true, path);
  const live = [
    "max_concurrent_agents", "max_concurrent_epic_supervisors", "auto_pick_up", "branch_prefix",
    "plannotator.binary", "plannotator.timeout", "notifications", "notifications.enabled",
    "notifications.default", "notifications.attempts", "notifications.backoff", "notifications.timeout",
    "notifications.channels.ops", "notifications.channels.ops.url", "projects.kontora.notify_channels",
  ];
  for (const path of live) assert.equal(state.settingsIsRestartOnly(path), false, path);

  state.settingsConfig.plannotator.reviews_dir = "~/new-reviews";
  assert.deepEqual([...state.settingsChangedPaths()], ["plannotator.reviews_dir"]);
  assert.equal(state.settingsRestartPending(), 1);
});

test("the Settings markup has no stale notification or worker restart markers", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const notifications = html.split("<!-- ===== notifications ===== -->")[1].split("<!-- =====")[0];
  assert.ok(notifications);
  assert.doesNotMatch(notifications, /restart/i);
  const workers = html.split("x.key === 'max_concurrent_agents'")[1].split("x.key === 'default_agent'")[0];
  assert.ok(workers);
  assert.doesNotMatch(workers, /restart/i);
  const plannotator = html.split("<!-- ===== plannotator ===== -->")[1].split("<!-- =====")[0];
  assert.match(plannotator, /x-show="settingsIsRestartOnly\('plannotator\.' \+ f\.key\)"[^>]*>restart<\/span>/);
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
  assert.match(html, /else if \(settingsGuard\) settingsGuard = false;[\s\S]*?else if \(automationsDeleteOpen\) automationsCloseDelete\(\);[\s\S]*?else if \(sheet\) closeSheet\(\); else if \(currentView === 'new'\)/);

  assert.match(html, /<template x-if="!loading && currentView === 'settings'">/);
  assert.match(html, /w-\[214px\][^"]*bg-surface-frame border-r border-surface-700\/50/);

  // The board's search box shrinks so the three-column settings layout fits.
  assert.match(html, /flex-1 min-w-\[120px\] max-w-\[300px\] relative/);
  assert.equal(html.includes('class="w-[300px] relative"'), false);

  // The 501 state offers no way to edit or save: it renders before the
  // settingsState === 'ok' template, and every editable control is inside it.
  const empty = html.slice(
    html.indexOf(`settingsState === 'unavailable'`),
    html.indexOf(`!settingsLoading && settingsLoadError`),
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
    "projects", "web", "assistant", "plannotator", "notifications", "statuses", "display",
  ]);
  for (const key of rail) {
    assert.match(html, new RegExp(`x-show="settingsSection === '${key}'"`), key);
  }

  // Read-only sections carry no editable control. Each runs to the next
  // section's x-show, so the slice does not depend on how the markup indents.
  for (const key of ["pipelines", "projects"]) {
    const start = html.indexOf(`x-show="settingsSection === '${key}'"`);
    const end = html.indexOf(`x-show="settingsSection === '`, start + 1);
    assert.ok(start > 0 && end > start, key);
    assert.equal(/x-model/.test(html.slice(start, end)), false, key);
  }

  // A stage has no default timeout, so a placeholder naming one reads as a
  // fallback that does not exist. Blank means the stage runs unbounded.
  assert.equal(html.includes(`x-model="settingsConfig.stages[name].timeout"`), true);
  assert.equal(/settingsConfig\.stages\[name\]\.timeout"[\s\S]{0,300}?placeholder="30m"/.test(html), false);

  // The daemon token is the credential for every /api and /ws call, so it is
  // masked until asked for.
  assert.match(html, /id="web-token" :type="settingsShowToken \? 'text' : 'password'"/);

  // agent and autonomy are choice cards writing straight into the model, in a
  // labelled radiogroup. No select, so no x-model applied before its x-for has
  // added an option.
  assert.equal(html.includes("<select id=\"as-"), false);
  for (const [field, group] of [["agent", "assistant agent"], ["autonomy", "assistant autonomy"]]) {
    const start = html.indexOf(`role="radiogroup" aria-label="${group}"`);
    assert.ok(start > 0, group);
    const cards = html.slice(start, html.indexOf("</div>", start));
    assert.match(cards, new RegExp(`@click="settingsConfig\\.assistant\\.${field} = `), field);
    assert.match(cards, new RegExp(`:aria-checked="[^"]*settingsConfig\\.assistant\\.${field}`), field);
  }
});

test("every choice-card group can be put back to unset", async () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // A card group replaced a text input and a <select>, both of which could be
  // emptied. Without a card for the empty value the first click writes the key
  // into config.yaml for good.
  for (const path of ["general.default_agent", "assistant.autonomy"]) {
    assert.match(html, new RegExp(`@click="settingsConfig\\.${path} = ''"`), path);
  }

  // A default_agent that names an agent the file no longer declares keeps a
  // card of its own. settingsClientErrors blocks every save while it is set,
  // so the value has to be on screen to be fixable.
  const state = await settingsState();
  assert.deepEqual([...state.settingsDefaultAgentChoices()], ["claude", "pi"]);

  state.settingsConfig.general.default_agent = "gone";
  assert.deepEqual([...state.settingsDefaultAgentChoices()], ["claude", "pi", "gone"]);
  assert.deepEqual(
    [...state.settingsClientErrors()],
    ['default_agent "gone": not found in agents'],
  );

  state.settingsConfig.general.default_agent = "";
  assert.deepEqual([...state.settingsClientErrors()], []);
});

// Board tickets for the live counts the redesigned rail, tiles and status
// pills read. Only kontora tickets count towards a status column.
const SETTINGS_TICKETS = [
  { id: "kon-1", status: "in_progress", kontora: true, project: "kontora" },
  { id: "kon-2", status: "in_progress", kontora: true, project: "kontora" },
  { id: "kon-3", status: "todo", kontora: true, project: "kontora" },
  { id: "kon-4", status: "done", kontora: true, project: "other" },
  { id: "kon-5", status: "todo", kontora: false, project: "kontora" },
];

test("the rail summarizes each section from the loaded config", async () => {
  const state = await settingsState();

  assert.deepEqual(
    Object.fromEntries(state.settingsSections.map((s) => [s.key, state.settingsSectionSummary(s.key)])),
    {
      general: "paths, concurrency",
      environment: "1 variable injected",
      agents: "claude, pi",
      stages: "3 prompt templates",
      pipelines: "1 · 2 steps",
      projects: "kontora",
      web: "127.0.0.1:8080 · no token",
      assistant: "disabled",
      plannotator: "review + annotate",
      notifications: "no channels",
      statuses: "7 built-in · 0 custom",
      display: "badges, dark theme",
    },
  );

  state.settingsConfig.web.token = "s3cret";
  assert.equal(state.settingsSectionSummary("web"), "127.0.0.1:8080 · token set");
  state.settingsConfig.assistant.agent = "claude";
  assert.equal(state.settingsSectionSummary("assistant"), "claude · ask first");
});

test("the rail and the tiles render against a config that is not loaded yet", () => {
  // The rail, the header and the tiles are outside the settingsState === 'ok'
  // template, so they render while the file is still being read and after a
  // parse error, when settingsConfig is null.
  const state = loadKontoraState();
  assert.equal(state.settingsConfig, null);

  for (const s of state.settingsSections) {
    assert.equal(state.settingsSectionSummary(s.key), "", s.key);
    assert.equal(state.settingsSectionCount(s.key), "", s.key);
  }
  assert.deepEqual([...state.settingsOverviewTiles()], []);
  assert.equal(state.settingsRestartPending(), 0);
  assert.equal(state.settingsSectionMatches("web"), true);
  assert.deepEqual([...state.settingsAgentStages("claude")], []);
  assert.deepEqual([...state.settingsPipelineSteps("full")], []);
  assert.equal(state.settingsConcurrency(), "");
});

test("the rail and the tiles read a config whose collections are all empty", async () => {
  // Every collection key present with a null value, which is how one parses
  // when all of its entries are commented out.
  const state = await settingsState(SETTINGS_BARE_FIXTURE);

  assert.equal(state.settingsSectionSummary("environment"), "0 variables injected");
  assert.equal(state.settingsSectionSummary("pipelines"), "0 · 0 steps");
  assert.equal(state.settingsSectionSummary("statuses"), "7 built-in · 0 custom");
  assert.equal(state.settingsSectionSummary("web"), "127.0.0.1:8080 · no token");
  assert.equal(state.settingsOverviewTiles()[2].unit, "0 steps");
  assert.deepEqual([...state.settingsAgentStages("claude")], []);
  assert.equal(state.settingsSectionMatches("agents"), true);
});

test("the overview tiles count what the config is made of", async () => {
  const state = await settingsState();

  assert.deepEqual(vmValue(state.settingsOverviewTiles()), [
    { label: "agents", hue: "indigo", value: 2, unit: "configured", foot: "claude · pi" },
    { label: "stages", hue: "cyan", value: 3, unit: "templates", foot: "1 built-in (rework)" },
    { label: "pipelines", hue: "amber", value: 1, unit: "2 steps", foot: "full" },
    { label: "projects", hue: "green", value: 1, unit: "repos", foot: "kontora" },
  ]);
});

test("the header counts only the unsaved edits a reload would not pick up", async () => {
  const state = await settingsState();
  assert.equal(state.settingsRestartPending(), 0);

  // reloadConfig pins web.port to the running value, so the save has to say a
  // restart is needed. branch_prefix it re-reads, so that one does not count.
  state.settingsConfig.web.port = "9090";
  assert.equal(state.settingsRestartPending(), 1);

  state.settingsConfig.web.port = "8080";
  state.settingsConfig.general.branch_prefix = "agent";
  assert.deepEqual([...state.settingsChangedPaths()], ["branch_prefix"]);
  assert.equal(state.settingsRestartPending(), 0);
});

test("the search filters the rail on keys, help and the live names in the file", async () => {
  const state = await settingsState();

  const matches = () => [...state.settingsSections].filter((s) => state.settingsSectionMatches(s.key)).map((s) => s.key);

  // An empty query shows everything, rail rows and field rows alike.
  assert.equal(state.settingsSectionMatches("display"), true);
  assert.equal(state.settingsRowVisible("anything", "", "display"), true);

  // A key only the markup carries.
  state.settingsQuery = "token";
  assert.equal(state.settingsSectionMatches("web"), true);
  assert.equal(state.settingsSectionMatches("display"), false);

  // A word that appears only in a field table's help string.
  state.settingsQuery = "tmux";
  assert.deepEqual(matches(), ["general"]);

  // A name that exists only because this config declares it.
  state.settingsQuery = "implement";
  assert.deepEqual(matches(), ["stages", "pipelines"]);

  // Per-row: the key or its help, nothing else.
  state.settingsQuery = "worktree";
  assert.equal(state.settingsRowVisible("worktrees_dir", "One git worktree per ticket lives here.", "general"), true);
  assert.equal(state.settingsRowVisible("logs_dir", "Per-stage agent output.", "general"), false);
});

test("a section matched on its own words keeps its rows instead of emptying", async () => {
  const state = await settingsState();

  // Each of these matches a section through its key, its blurb or its rail
  // summary — words no row of that section carries. Hiding every row would
  // leave the card headers standing over nothing.
  for (const [query, section, row] of [
    ["general", "general", ["logs_dir", "Per-stage agent output."]],
    ["paths", "general", ["branch_prefix", "Fallback when the ticket's project sets none."]],
    ["concurrency", "general", ["instance_name", "Identifies this daemon."]],
    ["web", "web", ["host", "Bind to a tailnet IP."]],
  ]) {
    state.settingsQuery = query;
    assert.equal(state.settingsSectionMatches(section), true, query);
    assert.equal(state.settingsRowVisible(row[0], row[1], section), true, query);
  }

  // A section the query does not reach at all keeps its rows too: filtering
  // them says nothing the dimmed rail row has not already said.
  state.settingsQuery = "nothing matches this";
  assert.equal(state.settingsSectionMatches("web"), false);
  assert.equal(state.settingsRowVisible("host", "Bind to a tailnet IP.", "web"), true);

  // A query that does reach a row still narrows the section to it.
  state.settingsQuery = "port";
  assert.equal(state.settingsRowVisible("port", "Port for the dashboard.", "web"), true);
  assert.equal(state.settingsRowVisible("host", "Bind to a tailnet IP.", "web"), false);
});

test("every row filter names the section it is written in", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  // settingsRowVisible reads the row terms of the section it is given, so a
  // call naming the wrong one — or none — filters against another section's
  // rows. Each call is attributed to the last section boundary above it.
  const bounds = [...html.matchAll(/x-show="settingsSection === '([a-z]+)'"/g)];
  const calls = [...html.matchAll(/settingsRowVisible\(/g)].map((m) => {
    let i = m.index + m[0].length;
    for (let depth = 1; depth; i++) {
      if (html[i] === "(") depth++;
      else if (html[i] === ")") depth--;
    }
    return { at: m.index, text: html.slice(m.index, i) };
  });
  assert.ok(calls.length >= 20);

  for (const call of calls) {
    const section = bounds.filter((b) => b.index < call.at).pop();
    assert.ok(section, call.text);
    assert.match(call.text, new RegExp(`, '${section[1]}'\\)$`), call.text);
  }
});

test("every general field is drawn by exactly one card of the section", () => {
  const state = loadKontoraState();
  const html = fs.readFileSync(htmlPath, "utf8");

  const groups = ["locations", "execution"];
  const placed = groups.flatMap((g) => [...state.settingsGeneralGroup(g)].map((f) => f.key));
  assert.deepEqual(placed.slice().sort(), [...state.settingsGeneralFields].map((f) => f.key).sort());
  assert.equal(new Set(placed).size, placed.length);

  // A field is drawn either by its group's text-input template or by a control
  // written out in the markup. Nothing may fall between the two.
  const text = new Set(groups.flatMap((g) => [...state.settingsGeneralTextRows(g)].map((f) => f.key)));
  for (const f of state.settingsGeneralFields) {
    if (text.has(f.key)) continue;
    assert.match(html, new RegExp(`settingsGeneralFields\\.find\\(x => x\\.key === '${f.key}'\\)`), f.key);
  }
});

test("the concurrency stepper edits and saves the same path the text input does", async () => {
  const state = await settingsState();
  state.runningAgents = 2;
  assert.equal(state.settingsConcurrency(), "");
  // Unset, so no option is selected and the free-text input stays on screen.
  assert.equal(state.settingsConcurrencyOther(), true);
  // An unset cap is the daemon's own default of 3.
  assert.deepEqual(vmValue(state.settingsSlots()), { busy: 2, total: 3, pills: 3 });

  state.settingsSetConcurrency(6);
  assert.deepEqual([...state.settingsChangedPaths()], ["max_concurrent_agents"]);
  assert.equal(state.settingsConcurrencyOther(), false);
  assert.deepEqual(vmValue(state.settingsSlots()), { busy: 2, total: 6, pills: 6 });
  assert.equal(state.settingsSlotsLabel(), "2 of 6 slots busy right now");
  assert.match(await state._settingsWrite(), /^max_concurrent_agents: 6$/m);

  // A value the stepper does not offer keeps the free-text input, and is still
  // written through unchanged for the daemon to reject.
  state.settingsConfig.general.max_concurrent_agents = "lots";
  assert.equal(state.settingsConcurrencyOther(), true);
  assert.equal(state.settingsSlots().total, 0);
  assert.match(await state._settingsWrite(), /^max_concurrent_agents: lots$/m);
});

test("the concurrency stepper can be left again for a value it does not offer", async () => {
  const state = await settingsState();

  // Every option the stepper offers is a value it can also leave: the "other"
  // option puts the free-text input back, and typing into it stays there.
  state.settingsSetConcurrency(6);
  assert.equal(state.settingsConcurrencyOther(), false);
  state.settingsConcurrencyCustom = true;
  assert.equal(state.settingsConcurrencyOther(), true);
  state.settingsConfig.general.max_concurrent_agents = "12";
  assert.equal(state.settingsConcurrencyOther(), true);
  assert.match(await state._settingsWrite(), /^max_concurrent_agents: 12$/m);

  // Picking an option again closes it.
  state.settingsSetConcurrency(4);
  assert.equal(state.settingsConcurrencyOther(), false);
  assert.equal(state.settingsConcurrencyIs(4), true);

  // "03" is text the user typed, not the option 3: the stepper shows nothing
  // selected and the input keeps what was typed, until the save writes the
  // number the daemon reads.
  state.settingsConfig.general.max_concurrent_agents = "03";
  assert.equal(state.settingsConcurrencyIs(3), false);
  assert.equal(state.settingsConcurrencyOther(), true);
  assert.match(await state._settingsWrite(), /^max_concurrent_agents: 3$/m);
});

test("the capacity meter survives a cap no board could fill", async () => {
  const state = await settingsState();
  state.runningAgents = 3;

  // The cap is free text. Without the clamp the template draws one span per
  // slot, and 5000 of them stall the tab on every keystroke.
  state.settingsConfig.general.max_concurrent_agents = "5000";
  assert.deepEqual(vmValue(state.settingsSlots()), { busy: 3, total: 5000, pills: 24 });
  assert.equal(state.settingsSlotsLabel(), "3 of 5000 slots busy right now");

  // A cap edited below what is already running reads as an overrun rather than
  // as "3 of 1 slots busy right now".
  state.settingsSetConcurrency(1);
  assert.equal(state.settingsSlotsLabel(), "3 running, over the cap of 1");
});

test("the live counts come from the board the page already holds", async () => {
  const state = await settingsState();
  state.tickets = SETTINGS_TICKETS;

  // Both fixture steps run on claude, so pi is named by none of them.
  assert.deepEqual([...state.settingsAgentStages("claude")], ["implement", "plan"]);
  assert.deepEqual([...state.settingsAgentStages("pi")], []);

  // A non-kontora ticket sits in no kontora column.
  assert.equal(state.settingsStatusCount("in_progress"), 2);
  assert.equal(state.settingsStatusCount("todo"), 1);
  assert.equal(state.settingsStatusCount("human_review"), 0);

  // Every ticket on the board counts here, kontora-managed or not: the card
  // exists either way, unlike a status column, which only kontora fills.
  assert.deepEqual(vmValue(state.settingsProjectTickets("kontora")), { total: 4, running: 2 });
  assert.deepEqual(vmValue(state.settingsProjectTickets("nothing")), { total: 0, running: 0 });
});

test("a pipeline step node resolves the stage's model for the agent that runs it", async () => {
  const state = await settingsState();

  assert.deepEqual(vmValue(state.settingsPipelineSteps("full")), [
    {
      index: 0, stage: "plan", agent: "claude", kind: "claude", model: "",
      timeout: "45m", on_success: "next", on_failure: "retry", max_retries: 2, running: false,
    },
    {
      // stages.implement.model is a map; claude's entry wins over the kind.
      index: 1, stage: "implement", agent: "claude", kind: "claude", model: "opus",
      timeout: "2h", on_success: "human_review", on_failure: "pause", max_retries: 0, running: false,
    },
  ]);
});

test("every settings section is in exactly one rail group", () => {
  const state = loadKontoraState();
  const grouped = [...state.settingsNavGroups].flatMap((g) => [...g.keys]);

  assert.deepEqual(grouped.slice().sort(), [...state.settingsSections].map((s) => s.key).sort());
  assert.equal(new Set(grouped).size, grouped.length);
});

const SETTINGS_NOTIFY_FIXTURE = `tickets_dir: ~/org/tickets

notifications:
  # where a ticket can reach me
  enabled: true
  timeout: 10s
  attempts: 3
  default:
    - tg-me
  channels:
    tg-me:
      type: telegram
      secret_env: KONTORA_TG_TOKEN
      chat_id: "123456"
    ops:
      type: webhook
      url: https://example.com/hooks/kontora
      method: POST
      headers:
        X-Kontora: board
`;

test("a channel written under Alpine's proxy reaches the daemon as YAML", async () => {
  // The document cannot be read back off the component while it is being
  // written: a node stored through the reactive Proxy fails yaml's own isMap
  // check, and stringify then throws instead of writing the file.
  const daemon = settingsFetch({ raw: SETTINGS_NOTIFY_FIXTURE });
  const state = settingsReactive(loadKontoraState({ fetch: daemon.fetch }));
  state._settingsYAML = await import(yamlPath);
  await state._settingsParse(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNewChannelName = "mm";
  state.settingsNotifAddChannel();
  const ch = state.settingsNotifChannel("mm");
  ch.type = "webhook";
  ch.url = "https://example.com/mm";
  state.settingsNewHeaderKey.mm = "X-Token";
  state.settingsNotifAddHeader("mm");
  ch.headers["X-Token"] = "abc";

  assert.equal(await state.saveSettings(), true);
  const sent = JSON.parse(daemon.puts()[0].init.body).content;
  assert.match(sent, /^ {4}mm:\n {6}type: webhook\n {6}url: https:\/\/example\.com\/mm\n {6}headers:\n {8}X-Token: abc$/m);
  assert.equal(state.settingsDirty(), false);
});

test("the notifications block round-trips with no edits", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(await state._settingsWrite(), SETTINGS_NOTIFY_FIXTURE);
});

test("the notifications model reads the block as the form holds it", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);
  const n = state.settingsConfig.notifications;

  assert.equal(n.enabled, true);
  assert.deepEqual([n.timeout, n.attempts, n.backoff], ["10s", "3", ""]);
  assert.deepEqual([...n.default], ["tg-me"]);
  assert.deepEqual([...state.settingsNotifChannelNames()], ["ops", "tg-me"]);
  assert.equal(state.settingsNotifChannel("tg-me").chat_id, "123456");
  assert.deepEqual({ ...state.settingsNotifChannel("ops").headers }, { "X-Kontora": "board" });
  // Read off the type rather than stored: a webhook draws url, method and
  // headers, and nothing a telegram channel would.
  assert.equal(state.settingsNotifHasField("ops", "url"), true);
  assert.equal(state.settingsNotifHasField("ops", "chat_id"), false);
  assert.equal(state.settingsNotifTarget("ops"), "POST https://example.com/hooks/kontora");
  assert.equal(state.settingsNotifSecretLabel("tg-me"), "secret from $KONTORA_TG_TOKEN");
});

test("an absent notifications block reads as enabled with nothing configured", async () => {
  const state = await settingsState(SETTINGS_BARE_FIXTURE);

  assert.equal(state.settingsConfig.notifications.enabled, true);
  assert.deepEqual([...state.settingsNotifChannelNames()], []);
  // The default is what applyDefaults would supply, so it is not an edit and
  // nothing is written for it.
  assert.deepEqual([...state.settingsChangedPaths()], []);
  assert.equal(state.settingsSectionSummary("notifications"), "no channels");
});

test("notification and concurrency edits save without restart markers", async () => {
  const daemon = settingsFetch({ raw: SETTINGS_NOTIFY_FIXTURE });
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE, { fetch: daemon.fetch });

  state.settingsConfig.notifications.attempts = "5";
  state.settingsNotifChannel("ops").url = "https://example.com/hooks/other";
  state.settingsConfig.general.max_concurrent_agents = "6";
  state.settingsConfig.general.max_concurrent_epic_supervisors = "2";
  assert.equal(state.settingsRestartPending(), 0);
  assert.equal(state.settingsIsRestartOnly("notifications"), false);
  assert.equal(state.settingsIsRestartOnly("notifications.channels.ops"), false);
  assert.equal(await state.saveSettings(), true);
  assert.equal(state.settingsSavedRestart, false);
});

test("editing the notifications block writes only the keys that changed", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsConfig.notifications.enabled = false;
  state.settingsConfig.notifications.attempts = "5";
  state.settingsConfig.notifications.backoff = "2s";
  const out = await state._settingsWrite();

  assert.match(out, /^ {2}enabled: false$/m);
  assert.match(out, /^ {2}attempts: 5$/m);
  assert.match(out, /^ {2}backoff: 2s$/m);
  // The comment inside the block, and the keys nothing touched, survive.
  assert.match(out, /^ {2}# where a ticket can reach me$/m);
  assert.match(out, /^ {2}timeout: 10s$/m);
});

test("a channel added in the form is written whole", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNewChannelName = "mm";
  state.settingsNotifAddChannel();
  const ch = state.settingsNotifChannel("mm");
  ch.type = "mattermost";
  ch.secret_file = "~/.config/kontora/mm-url";
  ch.channel = "kontora";
  state.settingsNotifToggleDefault("mm");
  const out = await state._settingsWrite();

  assert.match(out, /^ {4}mm:\n {6}type: mattermost\n {6}secret_file: ~\/\.config\/kontora\/mm-url\n {6}channel: kontora$/m);
  assert.match(out, /^ {2}default:\n {4}- tg-me\n {4}- mm$/m);
  assert.equal(state.settingsNewChannelName, "");
});

test("a removed channel takes its whole node and its place in the default list", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNotifRemoveChannel("tg-me");
  const out = await state._settingsWrite();

  assert.equal(out.includes("tg-me"), false);
  // Not an empty mapping left behind: config.Validate rejects a channel with
  // no type, so a save that left `tg-me: {}` could never be reloaded.
  assert.equal(/tg-me:\s*\{\}/.test(out), false);
  // An emptied list is the key going, not `default: []` left behind.
  assert.equal(out.includes("default"), false);
  assert.match(out, /^ {4}ops:$/m);
});

test("a header added and one removed are written entry by entry", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNewHeaderKey.ops = "X-Token";
  state.settingsNotifAddHeader("ops");
  state.settingsNotifChannel("ops").headers["X-Token"] = "abc";
  state.settingsNotifRemoveHeader("ops", "X-Kontora");
  const out = await state._settingsWrite();

  assert.match(out, /^ {8}X-Token: abc$/m);
  assert.equal(out.includes("X-Kontora"), false);
  assert.equal(state.settingsNewHeaderOpen.ops, false);
});

test("switching a channel's type drops the fields the new type does not use", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNotifChannel("tg-me").type = "webhook";
  state.settingsNotifChannel("tg-me").url = "https://example.com/tg";
  const out = await state._settingsWrite();

  assert.match(out, /^ {4}tg-me:\n {6}type: webhook\n {6}secret_env: KONTORA_TG_TOKEN\n {6}url: https:\/\/example\.com\/tg$/m);
  assert.equal(out.includes("chat_id"), false);
  // The value is kept in the form, so switching back does not lose it.
  assert.equal(state.settingsNotifChannel("tg-me").chat_id, "123456");
});

test("picking one secret source clears the other", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  assert.equal(state.settingsNotifSecretMode("tg-me"), "env");
  // A webhook with no credential shows none rather than an empty env var row.
  assert.equal(state.settingsNotifSecretMode("ops"), "none");
  state.settingsNotifSetSecretMode("tg-me", "file");
  state.settingsNotifChannel("tg-me").secret_file = "~/.config/kontora/tg";
  const out = await state._settingsWrite();

  assert.equal(state.settingsNotifChannel("tg-me").secret_env, "");
  assert.equal(out.includes("secret_env"), false);
  assert.match(out, /^ {6}secret_file: ~\/\.config\/kontora\/tg$/m);
});

test("the default picker and the opt-out clear each other", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);
  const n = state.settingsConfig.notifications;

  assert.deepEqual([...state.settingsNotifDefaultChoices()], ["ops", "tg-me", "none"]);
  state.settingsNotifToggleDefault("ops");
  assert.deepEqual([...n.default], ["tg-me", "ops"]);
  state.settingsNotifToggleDefault("none");
  assert.deepEqual([...n.default], ["none"]);
  state.settingsNotifToggleDefault("ops");
  assert.deepEqual([...n.default], ["ops"]);
  assert.deepEqual([...state._settingsNotificationErrors()], []);
});

test("the notifications section blocks a save the daemon would refuse", async () => {
  const cases = [
    {
      name: "a type nothing delivers over",
      edit: (s) => { s.settingsNotifChannel("tg-me").type = "signal"; },
      want: 'notifications.channels "tg-me": unknown type "signal" (use telegram, mattermost or webhook)',
    },
    {
      name: "no type at all",
      edit: (s) => { s.settingsNotifChannel("tg-me").type = ""; },
      want: 'notifications.channels "tg-me": type is required (telegram, mattermost or webhook)',
    },
    {
      name: "telegram with no chat",
      edit: (s) => { s.settingsNotifChannel("tg-me").chat_id = ""; },
      want: 'notifications.channels "tg-me": telegram needs chat_id',
    },
    {
      name: "telegram with no credential",
      edit: (s) => { s.settingsNotifChannel("tg-me").secret_env = ""; },
      want: 'notifications.channels "tg-me": telegram needs the bot token in secret_env or secret_file',
    },
    {
      name: "both secret sources",
      edit: (s) => { s.settingsNotifChannel("tg-me").secret_file = "~/tg"; },
      want: 'notifications.channels "tg-me": set secret_env or secret_file, not both',
    },
    {
      name: "a webhook with no url",
      edit: (s) => { s.settingsNotifChannel("ops").url = ""; },
      want: 'notifications.channels "ops": webhook needs url',
    },
    {
      name: "a url nothing can post to",
      edit: (s) => { s.settingsNotifChannel("ops").url = "ftp://example.com/hook"; },
      want: 'notifications.channels "ops": url "ftp://example.com/hook": scheme "ftp" is not supported (use http or https)',
    },
    {
      name: "a url with no scheme",
      edit: (s) => { s.settingsNotifChannel("ops").url = "example.com/hook"; },
      want: 'notifications.channels "ops": url "example.com/hook": scheme "" is not supported (use http or https)',
    },
    {
      name: "a body-less method",
      edit: (s) => { s.settingsNotifChannel("ops").method = "GET"; },
      want: 'notifications.channels "ops": method "GET" is not supported (use POST, PUT, PATCH)',
    },
    {
      name: "two headers that are one header",
      edit: (s) => { s.settingsNotifChannel("ops").headers["x-kontora"] = "board"; },
      want: 'notifications.channels "ops": headers "X-Kontora" and "x-kontora" are the same header',
    },
    {
      name: "attempts outside the range",
      edit: (s) => { s.settingsConfig.notifications.attempts = "0"; },
      want: "notifications.attempts 0: must be between 1 and 10 (1 sends once and does not retry)",
    },
    {
      name: "a default naming no channel",
      edit: (s) => { s.settingsConfig.notifications.default = ["nope"]; },
      want: 'notifications.default: unknown channel "nope"',
    },
    {
      name: "the opt-out beside a channel",
      edit: (s) => { s.settingsConfig.notifications.default = ["tg-me", "none"]; },
      want: 'notifications.default: "none" silences the list and cannot be combined with a channel',
    },
  ];

  for (const c of cases) {
    const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);
    c.edit(state);
    assert.deepEqual([...state.settingsClientErrors()], [c.want], c.name);
  }
});

test("a channel named after the opt-out is refused", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsNewChannelName = "none";
  state.settingsNotifAddChannel();
  assert.deepEqual([...state.settingsClientErrors()], [
    'notifications.channels "none": name is reserved for the opt-out',
  ]);
});

test("a rejected save leaves the notifications form as it was typed", async () => {
  const daemon = settingsFetch({
    raw: SETTINGS_NOTIFY_FIXTURE,
    put: { status: 400, ok: false, json: async () => ({ error: 'notifications.channels "ops": webhook needs url' }) },
  });
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE, { fetch: daemon.fetch });

  state.settingsNotifChannel("ops").url = "https://example.com/other";
  assert.equal(await state.saveSettings(), false);
  assert.deepEqual([...state.settingsErrors], ['notifications.channels "ops": webhook needs url']);
  assert.equal(state.settingsNotifChannel("ops").url, "https://example.com/other");
});

test("the notifications summary and the advisory say what the daemon will do", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  assert.equal(state.settingsSectionSummary("notifications"), "2 channels · default tg-me");
  assert.match(state.settingsNotifAdvisory(), /applies notification changes without a restart/);
  assert.match(state.settingsNotifAdvisory(), /A new or changed channel whose secret cannot be read is skipped/);
  assert.match(state.settingsNotifAdvisory(), /Unchanged channel definitions keep their running credentials/);
  state.settingsConfig.notifications.enabled = false;
  assert.match(state.settingsNotifAdvisory(), /^Delivery is off/);
  state.settingsConfig.notifications.default = [];
  assert.equal(state.settingsSectionSummary("notifications"), "2 channels · no default");
});

test("the notifications search reaches a channel by name and by type", async () => {
  const state = await settingsState(SETTINGS_NOTIFY_FIXTURE);

  state.settingsQuery = "mattermost";
  assert.equal(state.settingsSectionMatches("notifications"), true);
  state.settingsQuery = "tg-me";
  assert.equal(state.settingsSectionMatches("notifications"), true);
  assert.equal(state.settingsRowVisible("ops", "webhook", "notifications"), false);
  assert.equal(state.settingsRowVisible("tg-me", "telegram", "notifications"), true);
});

test("read-only project defaults are searchable and preserve YAML on unrelated saves", async () => {
  const text = `${SETTINGS_NOTIFY_FIXTURE}
projects:
  kontora:
    path: ~/projects/kontora
    epic_child_pipeline: "child-workflow" # keep this comment and quoting
    notify_channels:
      - ops
`;
  const daemon = settingsFetch({ raw: text });
  const state = await settingsState(text, { fetch: daemon.fetch });

  assert.deepEqual([...state.settingsConfig.projects.kontora.notify_channels], ["ops"]);
  assert.equal(state.settingsConfig.projects.kontora.epic_child_pipeline, "child-workflow");
  for (const query of ['epic_child_pipeline', 'child-workflow']) {
    state.settingsQuery = query;
    assert.equal(state.settingsSectionMatches('projects'), true, query);
    assert.equal(state.settingsRowVisible('kontora', '~/projects/kontora epic_child_pipeline child-workflow', 'projects'), true, query);
  }
  state.settingsQuery = '';
  state.settingsConfig.general.branch_prefix = 'agent';
  assert.deepEqual([...state.settingsChangedPaths()], ['branch_prefix']);
  assert.equal(await state.saveSettings(), true);
  const saved = JSON.parse(daemon.puts()[0].init.body).content;
  assert.ok(saved.includes('    epic_child_pipeline: "child-workflow" # keep this comment and quoting'));
  assert.match(saved, /notify_channels:\n      - ops/);
  assert.match(fs.readFileSync(htmlPath, 'utf8'), /x-text="settingsConfig\.projects\[name\]\.epic_child_pipeline \|\| 'inherits pipeline default'"/);
});

test("escape clears the settings search without unwinding the global escape chain", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const input = html.slice(html.indexOf('id="settings-search"'), html.indexOf('id="settings-search"') + 400);

  assert.match(input, /x-model="settingsQuery"/);
  // A query to clear consumes the keystroke, so it never reaches
  // @keydown.escape.window, which would close a detail panel or the assistant
  // pane at the same time. An empty box still falls through to that chain.
  assert.match(input, /@keydown\.escape="if \(settingsQuery\) \{ \$event\.stopPropagation\(\); settingsQuery = '' \}"/);
});

test("slash focuses the settings search, and only there", () => {
  const focused = [];
  const search = { focus: () => focused.push("settings-search") };
  const state = loadKontoraState({
    document: {
      getElementById: (id) => (id === "settings-search" ? search : null),
      querySelector: () => null,
      hasFocus: () => true,
      addEventListener() {},
      removeEventListener() {},
    },
  });
  const press = (key, target) => {
    let prevented = false;
    state._handleKeySequence({ key, target, preventDefault: () => { prevented = true; } });
    return prevented;
  };

  state.currentView = "board";
  assert.equal(press("/", { tagName: "DIV" }), false);
  assert.deepEqual(focused, []);

  state.currentView = "settings";
  assert.equal(press("/", { tagName: "DIV" }), true);
  assert.deepEqual(focused, ["settings-search"]);

  // Typing a "/" into the search box itself, or into the terminal, is a "/".
  assert.equal(press("/", { tagName: "INPUT" }), false);
  assert.equal(press("/", { tagName: "TEXTAREA" }), false);
  assert.deepEqual(focused, ["settings-search"]);
});

// ── Agent picker ─────────────────────────────────────────────────────────

const PICKER_AGENTS = [
  { name: "claude", model: "sonnet", effort: "", running: 2 },
  { name: "claude-opus", model: "opus", effort: "high", running: 0 },
  { name: "codex", model: "gpt-5-codex", effort: "medium", running: 1 },
  { name: "pi", model: "gpt-5.6", effort: "high", running: 0 },
  { name: "haiku", model: "haiku", effort: "", running: 0 },
  { name: "aider", model: "deepseek-v3", effort: "", running: 0 },
];

function pickerState({ recents = [], agents = PICKER_AGENTS, config = {}, overrides = {} } = {}) {
  const stored = { "kontora-recent-agents": JSON.stringify(recents) };
  const state = loadKontoraState({
    localStorage: {
      getItem(key) { return Object.hasOwn(stored, key) ? stored[key] : null; },
      setItem(key, value) { stored[key] = String(value); },
    },
    ...overrides,
  });
  state.$nextTick = (callback) => { if (callback) callback(); return Promise.resolve(); };
  state.configCache = {
    agents: agents.map((agent) => agent.name),
    agent_infos: agents.map(({ name, model, effort }) => ({ name, model, effort })),
    default_agent: "claude",
    pipelines: ["default"],
    pipeline_infos: [{ name: "default", stages: ["implement", "review"], default_agent: "claude" }],
    projects: [],
    ...config,
  };
  state._agentRunning = Object.create(null);
  agents.forEach((agent) => { state._agentRunning[agent.name] = agent.running || 0; });
  state.createForm.pipeline = "default";
  return { state, stored };
}

// Only the APIs consumed by mobile/picker lifecycle methods. This does not
// emulate layout, native popover, or browser event propagation; browser tests
// own those. Frames stay queued so cancellation and stale callbacks are visible.
function mobileLifecycleDOM({ visualViewport = true } = {}) {
  function events() {
    const listeners = [];
    return {
      listeners,
      addEventListener(name, handler, options) { listeners.push({ name, handler, options }); },
      removeEventListener(name, handler, options) {
        const i = listeners.findIndex(l => l.name === name && l.handler === handler && l.options === options);
        assert.notEqual(i, -1, 'remove the exact registered listener/options');
        listeners.splice(i, 1);
      },
      emit(name, event = {}) { [...listeners].filter(l => l.name === name).forEach(l => l.handler(event)); },
    };
  }
  const frames = new Map(), cancelled = [], elements = {}, selectors = {};
  let frameID = 0;
  const properties = new Map();
  const style = {
    setProperty(name, value, priority = '') { properties.set(name, { value, priority }); },
    getPropertyValue(name) { return properties.get(name)?.value || ''; },
    getPropertyPriority(name) { return properties.get(name)?.priority || ''; },
    removeProperty(name) { properties.delete(name); },
  };
  const document = Object.assign(events(), {
    activeElement: null,
    documentElement: { style },
    getElementById: id => elements[id] || null,
    querySelector: selector => selectors[selector] || null,
    querySelectorAll: selector => selectors[selector] || [],
  });
  const viewport = visualViewport ? Object.assign(events(), { offsetTop: 20, offsetLeft: 10, width: 360, height: 500 }) : undefined;
  const window = Object.assign(events(), { innerWidth: 600, innerHeight: 800, visualViewport: viewport });
  function node(attrs = {}, parent = null) {
    const attributes = new Map(Object.entries(attrs));
    const el = {
      parent, isConnected: true, tabIndex: 0, offsetParent: {}, style: {}, focuses: [], blurs: 0,
      hasAttribute: name => attributes.has(name),
      getAttribute: name => attributes.get(name) ?? null,
      setAttribute(name, value) { attributes.set(name, value); },
      removeAttribute(name) { attributes.delete(name); },
      contains(target) { for (; target; target = target.parent) if (target === el) return true; return false; },
      closest(selector) {
        for (let target = el; target; target = target.parent) {
          if (target.matches(selector)) return target;
          if (selector === '[data-agent-picker-panel]' && target.hasAttribute('data-agent-picker-panel')) return target;
          if (selector === '[inert]' && target.hasAttribute('inert')) return target;
          if (selector === '[inert], [hidden], [aria-hidden="true"]' &&
              (target.hasAttribute('inert') || target.hasAttribute('hidden') || target.getAttribute('aria-hidden') === 'true')) return target;
        }
        return null;
      },
      matches: selector => selector.split(',').some(part => {
        const s = part.trim();
        if (s === ':disabled') return el.hasAttribute('disabled');
        if (s === '[data-mobile-terminal-input]') return el.hasAttribute('data-mobile-terminal-input');
        if (s === '.mobile-terminal-host .xterm-helper-textarea') {
          return el.matches('.xterm-helper-textarea') && !!el.parent?.closest('.mobile-terminal-host');
        }
        return /^\.[\w-]+$/.test(s) && (el.getAttribute('class') || '').split(/\s+/).includes(s.slice(1));
      }),
      getClientRects: () => [{}],
      focus(options = {}) { el.focuses.push(vmValue(options)); document.activeElement = el; },
      blur() { ++el.blurs; if (document.activeElement === el) document.activeElement = null; },
    };
    return el;
  }
  return {
    window, document, viewport, elements, selectors, style, node, frames, cancelled,
    runFrame() { const callbacks = [...frames.values()]; frames.clear(); callbacks.forEach(cb => cb()); },
    overrides: {
      window, document,
      requestAnimationFrame(callback) { const id = ++frameID; frames.set(id, callback); return id; },
      cancelAnimationFrame(id) { cancelled.push(id); frames.delete(id); },
      getComputedStyle: () => ({ visibility: 'visible' }),
    },
  };
}

for (const scenario of [
  { name: 'right overflow', requested: 3, expected: 3, left: 310, right: 450, scroll: 130 },
  { name: 'left overflow', requested: 1, expected: 1, left: -70, right: 70, scroll: -70 },
  { name: 'already visible', requested: 1, expected: 1, left: 30, right: 170 },
  { name: 'oversized right overflow', requested: 999, expected: 4, left: 310, right: 680, scroll: 310, repeat: true },
  { name: 'oversized left overflow', requested: -1, expected: 0, left: -50, right: 320, scroll: -50, repeat: true },
  { name: 'oversized spanning both edges', requested: 4, expected: 4, left: -25, right: 345, scroll: -25, repeat: true },
  { name: 'oversized aligned start', requested: 4, expected: 4, left: 0, right: 370, repeat: true },
  { name: 'right safe padding', requested: 3, expected: 3, left: 210, right: 310, paddingRight: 44, scroll: 34, repeat: true },
  { name: 'left safe padding', requested: 1, expected: 1, left: 10, right: 150, paddingLeft: 44, scroll: -34, repeat: true },
  { name: 'inside safe edges', requested: 1, expected: 1, left: 44, right: 276, paddingLeft: 44, paddingRight: 44, repeat: true },
  { name: 'oversized within padded strip', requested: 4, expected: 4, left: 44, right: 300, paddingLeft: 44, paddingRight: 44, repeat: true },
  { name: 'padded border offset', requested: 3, expected: 3, left: 210, right: 310, paddingRight: 44, clientLeft: 2, scroll: 32, repeat: true },
  { name: 'past last column', requested: 999, expected: 4, left: 310, right: 450, scroll: 130 },
  { name: 'before first column', requested: -1, expected: 0, left: -70, right: 70, scroll: -70 },
  { name: 'columns removed before tick', requested: 4, expected: 1, shrink: 2, left: 310, right: 450, scroll: 130 },
  { name: 'empty columns before tick', requested: 4, expected: 0, shrink: 0 },
  { name: 'superseded selection', requested: 3, next: 0, expected: 0, left: -70, right: 70, scroll: -70 },
  ...['route', 'route roundtrip', 'ticket', 'epic', 'sheet', 'desktop', 'breakpoint roundtrip', 'removed', 'replaced', 'hidden', 'destroyed', 'app destroyed'].map(stop =>
    ({ name: stop, requested: 3, expected: 3, left: 310, right: 450, stop })),
]) {
  test(`mobile status selection scrolls only the live strip: ${scenario.name}`, () => {
    const dom = mobileLifecycleDOM();
    dom.overrides.getComputedStyle = () => ({ paddingLeft: `${scenario.paddingLeft || 0}px`, paddingRight: `${scenario.paddingRight || 0}px` });
    const { state } = pickerState({ overrides: dom.overrides });
    state.initMobile();
    const ticks = [], scrolls = [], reads = [];
    let offset = 0;
    state.$nextTick = cb => { ticks.push(cb); };
    const button = { getBoundingClientRect() { reads.push('button'); return { left: scenario.left - offset, right: scenario.right - offset }; } };
    const strip = { isConnected: true, clientLeft: scenario.clientLeft || 0, clientWidth: 320,
      getClientRects: () => [{}],
      getBoundingClientRect() { reads.push('strip'); return { left: 0, right: 320 }; },
      querySelectorAll: () => state.columns.map(() => button),
      scrollBy(options) { scrolls.push(vmValue(options)); offset += options.left; },
    };
    state.$refs = { mobileStatusTabs: strip };
    state.setActiveColumn(scenario.requested);
    assert.equal(scrolls.length, 0, 'wait for Alpine to update the selected status');
    if (scenario.shrink !== undefined) state._builtinColumns = state._builtinColumns.slice(0, scenario.shrink);
    if (scenario.next !== undefined) state.setActiveColumn(scenario.next);
    if (scenario.stop === 'route' || scenario.stop === 'route roundtrip') {
      state.mobileBeforeNavigate('stats');
      state.currentView = scenario.stop === 'route' ? 'stats' : 'board';
    }
    if (scenario.stop === 'ticket') state.selectedTicket = { id: 'kon-a' };
    if (scenario.stop === 'epic') state.selectedEpic = { id: 'kon-a' };
    if (scenario.stop === 'sheet') state.sheet = { type: 'navigation' };
    if (scenario.stop === 'desktop') state.isMobile = false;
    if (scenario.stop === 'breakpoint roundtrip') state.mobileBreakpointChanged();
    if (scenario.stop === 'removed') strip.isConnected = false;
    if (scenario.stop === 'replaced') state.$refs.mobileStatusTabs = { ...strip };
    if (scenario.stop === 'hidden') strip.getClientRects = () => [];
    if (scenario.stop === 'destroyed') { state.destroyMobile(); state.initMobile(); }
    if (scenario.stop === 'app destroyed') state._appDestroyed = true;
    ticks.splice(0).forEach(cb => cb());
    assert.equal(state.activeColumn, scenario.expected);
    assert.deepEqual(scrolls, scenario.scroll === undefined ? [] : [{ left: scenario.scroll, behavior: 'instant' }]);
    if (scenario.stop || scenario.shrink === 0) assert.deepEqual(reads, [], 'teardown must not measure the old board');
    else assert.deepEqual(reads, ['strip', 'button'], 'only the latest selection measures the strip');
    if (scenario.repeat) {
      const count = scrolls.length;
      for (let i = 0; i < 3; ++i) {
        state.setActiveColumn(scenario.requested);
        ticks.splice(0).forEach(cb => cb());
        assert.equal(state.activeColumn, scenario.expected);
        assert.equal(scrolls.length, count, 'repeating the same selection must not oscillate');
      }
    }
    state.destroyMobile();
  });
}

for (const scenario of [
  { name: 'left swipe', start: 0, dx: -100, dy: 5, expected: 1 },
  { name: 'right swipe', start: 1, dx: 100, dy: 5, expected: 0 },
  { name: 'last column', start: 4, dx: -100, dy: 5, expected: 4 },
  { name: 'first column', start: 0, dx: 100, dy: 5, expected: 0 },
  { name: 'short swipe', start: 0, dx: -55, dy: 0, expected: 0 },
  { name: 'vertical swipe', start: 0, dx: -100, dy: 100, expected: 0 },
  ...['ticket', 'epic', 'sheet'].map(stop => ({ name: stop, start: 0, dx: -100, dy: 0, expected: 0, stop })),
]) {
  test(`mobile board swipe preserves thresholds and clamps: ${scenario.name}`, () => {
    const { state } = pickerState();
    state.$nextTick = cb => cb();
    state.activeColumn = scenario.start;
    if (scenario.stop === 'ticket') state.selectedTicket = { id: 'kon-a' };
    if (scenario.stop === 'epic') state.selectedEpic = { id: 'kon-a' };
    if (scenario.stop === 'sheet') state.sheet = { type: 'navigation' };
    const touch = (x, y) => ({ changedTouches: [{ clientX: x, clientY: y }] });
    state.onBoardTouchStart(touch(150, 200));
    state.onBoardTouchEnd(touch(150 + scenario.dx, 200 + scenario.dy));
    assert.equal(state.activeColumn, scenario.expected);
  });
}

for (const geometry of [
  { name: 'no visualViewport', viewport: undefined, expected: { top: 0, left: 0, width: 600, height: 800, bottom: 0 } },
  { name: 'offset visualViewport', viewport: { offsetTop: 30, offsetLeft: 15, width: 320, height: 400 }, expected: { top: 30, left: 15, width: 320, height: 400, bottom: 370 } },
  { name: 'invalid visualViewport values', viewport: { offsetTop: -1, offsetLeft: NaN, width: 0, height: Infinity }, expected: { top: 0, left: 0, width: 600, height: 800, bottom: 0 } },
  { name: 'viewport extends below layout', viewport: { offsetTop: 100, offsetLeft: 0, width: 500, height: 900 }, expected: { top: 100, left: 0, width: 500, height: 900, bottom: 0 } },
]) {
  test(`mobile and picker viewport geometry: ${geometry.name}`, () => {
    const dom = mobileLifecycleDOM({ visualViewport: false });
    dom.window.visualViewport = geometry.viewport;
    const { state } = pickerState({ overrides: dom.overrides });
    assert.deepEqual(vmValue(state.mobileViewportGeometry()), geometry.expected);
    const { bottom, ...pickerGeometry } = geometry.expected;
    assert.deepEqual(vmValue(state.agentPickerViewport()), pickerGeometry);
    state.initMobile();
    for (const [key, value] of Object.entries(geometry.expected)) {
      assert.equal(dom.style.getPropertyValue('--mobile-vv-' + key), value + 'px');
    }
    state.destroyMobile();
  });
}

for (const visualViewport of [false, true]) {
  test(`mobile lifecycle coalesces geometry, uses layout breakpoint and cleans up: visualViewport=${visualViewport}`, () => {
    const dom = mobileLifecycleDOM({ visualViewport });
    const { state } = pickerState({ overrides: dom.overrides });
    const watches = new Map();
    let unwatched = 0;
    state.$watch = (key, cb) => { watches.set(key, cb); return () => { watches.delete(key); ++unwatched; }; };
    dom.style.setProperty('--mobile-vv-top', '7px', 'important');
    state.initMobile();
    state.initMobile();
    assert.equal(dom.window.listeners.length, 1);
    assert.deepEqual(dom.document.listeners.map(l => l.name), ['keydown', 'focusin', 'focusout']);
    assert.ok(dom.document.listeners.every(l => l.options === true));
    assert.equal(dom.viewport?.listeners.length || 0, visualViewport ? 2 : 0);
    assert.equal(watches.size, 1);
    assert.equal(state.isMobile, true);
    dom.window.innerHeight = 400;
    if (dom.viewport) { dom.viewport.height = 200; dom.viewport.width = 100; }
    dom.window.emit('resize');
    dom.viewport?.emit('resize');
    dom.viewport?.emit('scroll');
    assert.equal(state.isMobile, true, 'keyboard height/pinch width is not a breakpoint');
    assert.equal(dom.frames.size, 1);
    dom.runFrame();
    assert.equal(dom.style.getPropertyValue('--mobile-vv-height'), visualViewport ? '200px' : '400px');
    dom.window.innerWidth = 768;
    dom.window.emit('resize');
    assert.equal(state.isMobile, false, 'desktop begins at exactly 768 layout pixels');
    const staleFrame = [...dom.frames.values()][0];
    const staleResize = dom.window.listeners[0].handler;
    const pendingFrame = [...dom.frames.keys()][0];
    // Removal must use the viewport captured at registration, not this getter.
    dom.window.visualViewport = undefined;
    state.destroyMobile();
    state.destroyMobile();
    assert.deepEqual(dom.cancelled, [pendingFrame]);
    assert.equal(dom.frames.size, 0);
    assert.equal(dom.window.listeners.length, 0);
    assert.equal(dom.document.listeners.length, 0);
    assert.equal(dom.viewport?.listeners.length || 0, 0);
    assert.equal(unwatched, 1);
    assert.equal(dom.style.getPropertyValue('--mobile-vv-top'), '7px');
    assert.equal(dom.style.getPropertyPriority('--mobile-vv-top'), 'important');
    assert.equal(dom.style.getPropertyValue('--mobile-vv-height'), '');
    staleResize();
    staleFrame();
    assert.equal(dom.frames.size, 0);
    assert.equal(dom.style.getPropertyValue('--mobile-vv-height'), '');
    state._appDestroyed = true;
    state.initMobile();
    assert.equal(dom.window.listeners.length, 0);
  });

  test(`picker lifecycle removes listeners and cancels resize/focus RAF: visualViewport=${visualViewport}`, () => {
    const dom = mobileLifecycleDOM({ visualViewport });
    const { state } = pickerState({ overrides: dom.overrides });
    let positioned = 0;
    state.positionAgentPicker = () => { ++positioned; };
    state.setupAgentPicker();
    state.setupAgentPicker();
    assert.equal(dom.document.listeners.length, 2);
    assert.equal(dom.document.listeners.find(l => l.name === 'scroll').options, true);
    assert.equal(dom.window.listeners.length, 1);
    assert.equal(dom.viewport?.listeners.length || 0, visualViewport ? 2 : 0);
    state._agentPickerOpenHost = 'create';
    state.agentPickerState('create').open = true;
    dom.window.emit('resize');
    dom.viewport?.emit('resize');
    dom.viewport?.emit('scroll');
    assert.equal(dom.frames.size, 1);
    dom.runFrame();
    assert.equal(positioned, 1);
    // No focus target exists yet.
    state.focusAgentPicker('create');
    dom.window.emit('resize');
    assert.equal(dom.frames.size, 2);
    const staleFrames = [...dom.frames.values()];
    dom.window.visualViewport = undefined;
    state.destroyAgentPicker();
    state.destroyAgentPicker();
    assert.equal(dom.cancelled.length, 2);
    assert.equal(dom.frames.size, 0);
    assert.equal(dom.document.listeners.length, 0);
    assert.equal(dom.window.listeners.length, 0);
    assert.equal(dom.viewport?.listeners.length || 0, 0);
    assert.equal(state._agentPickerOpenHost, null);
    staleFrames.forEach(cb => cb());
    assert.equal(positioned, 1);
    assert.equal(dom.frames.size, 0);
    assert.equal(state._agentPickerFocusFrame, null);
    assert.equal(state._agentPickerResizeFrame, null);
  });
}

test("the agent picker groups inherit, recent, and remaining configured agents", () => {
  const { state } = pickerState({ recents: ["codex", "gone", "pi"] });

  const groups = vmValue(state.agentPickerGroups("create"));
  assert.deepEqual(groups.map((group) => group.name), ["inherit", "recent", "all agents"]);
  assert.deepEqual(groups[1].rows.map((row) => row.value), ["codex", "pi"]);
  assert.deepEqual(groups[2].rows.map((row) => row.value), ["claude", "claude-opus", "haiku", "aider"]);
  assert.equal(groups[0].rows[0].meta, "implement → claude");
  assert.equal(groups[1].rows[0].meta, "gpt-5-codex · medium");
  assert.equal(groups[2].rows[0].isDefault, true);
  assert.equal(state.agentPickerRunningTotal(), 3);
  assert.match(state.agentPickerDotStyle("codex"), /--st-progress/);
});

test("a no-pipeline picker names the standalone default without mentioning stages", () => {
  const { state } = pickerState();
  state.createForm.pipeline = "";

  assert.equal(state.agentPickerBlankLabel("create"), "default agent");
  assert.equal(state.agentPickerInheritMeta("create"), "single run → claude");
  assert.equal(state.agentPickerHint("create"), "single run → claude");
});

test("the picker falls back to name-only agent rows from an older daemon", () => {
  const { state } = pickerState({ config: { agent_infos: undefined } });

  const claude = state.agentPickerRows("create").find((row) => row.value === "claude");
  assert.equal(claude.meta, "");
  assert.equal(claude.isDefault, true);
});

test("agent filtering starts at six agents and matches names or models", () => {
  const { state } = pickerState();
  assert.equal(state.agentPickerHasFilter(), true);

  state.agentPickerState("create").query = "GPT-5";
  let rows = vmValue(state.agentPickerRows("create"));
  assert.deepEqual(rows.map((row) => row.value), ["codex", "pi"]);
  assert.equal(rows.some((row) => row.inherit), false);

  state.agentPickerState("create").query = "nothing";
  assert.equal(state.agentPickerRows("create").length, 0);
  state.agentPickerPickActive("create");
  assert.equal(state.createForm.agent, "");

  state.configCache.agents = state.configCache.agents.slice(0, 5);
  state.configCache.agent_infos = state.configCache.agent_infos.slice(0, 5);
  assert.equal(state.agentPickerHasFilter(), false);
});

test("agent choices are remembered most recent first, deduplicated, and capped", () => {
  const { state, stored } = pickerState({ recents: ["pi", "codex"] });

  for (const name of ["claude", "pi", "haiku", "aider"]) {
    state.agentPickerChoose("create", name, false);
  }

  assert.deepEqual([...state.recentAgents], ["aider", "haiku", "pi"]);
  assert.deepEqual(JSON.parse(stored["kontora-recent-agents"]), ["aider", "haiku", "pi"]);
  assert.equal(state.createTouched.agent, true);

  state.agentPickerChoose("create", "", false);
  assert.deepEqual([...state.recentAgents], ["aider", "haiku", "pi"]);
});

test("malformed or unavailable recent-agent storage leaves an empty list", () => {
  const malformed = loadKontoraState({
    localStorage: { getItem: () => "{", setItem() { throw new Error("full"); } },
  });
  assert.deepEqual([...malformed.recentAgents], []);
  assert.doesNotThrow(() => malformed.rememberAgent("claude"));

  const unavailable = loadKontoraState({
    localStorage: { getItem() { throw new Error("blocked"); }, setItem() {} },
  });
  assert.deepEqual([...unavailable.recentAgents], []);
});

test("the four picker adapters preserve their host side effects", () => {
  const { state } = pickerState();
  let saves = 0;
  state.saveEdit = () => { saves += 1; };

  state.agentPickerChoose("create", "codex", false);
  assert.equal(state.createForm.agent, "codex");
  assert.equal(state.createTouched.agent, true);

  state.initForm.agent = "";
  state.agentPickerChoose("init", "pi", false);
  assert.equal(state.initForm.agent, "pi");
  assert.equal(saves, 0);

  state.editForm.agent = "";
  state.agentPickerChoose("edit", "claude", false);
  assert.equal(state.editForm.agent, "claude");
  assert.equal(saves, 1);

  state.createTouched.agent = false;
  state.agentPickerChoose("mobile-create", "haiku", false);
  assert.equal(state.createForm.agent, "haiku");
  assert.equal(state.createTouched.agent, true);
});

test("init and detail edit the stored agent override, not the resolved agent", async () => {
  const { state } = pickerState();
  state.parseTitleTag = () => ({ tag: "", rest: "Ticket" });

  await state.openInitModal({
    id: "kon-init", status: "open", path: "/work/repo", pipeline: "default",
    agent: "claude",
  });
  assert.equal(state.initForm.agent, "");

  state.configCache.projects = [{
    name: "repo", path: "/work/repo", resolved_path: "/work/repo", pipeline: "default", agent: "codex",
  }];
  await state.openInitModal({
    id: "kon-project", status: "open", path: "/work/repo", pipeline: "default",
    agent: "claude",
  });
  assert.equal(state.initForm.agent, "codex");

  state.selectedTicket = {
    id: "kon-edit", status: "open", path: "/work/other", pipeline: "default",
    agent: "claude", body: "",
  };
  await state.startEditing();
  assert.equal(state.editForm.agent, "");

  state.selectedTicket.agent_override = true;
  state.selectedTicket.agent = "codex";
  await state.startEditing();
  assert.equal(state.editForm.agent, "codex");
});

test("path changes preserve an explicit override equal to the old project default", async () => {
  const { state } = pickerState({
    config: {
      projects: [
        { name: "old", path: "/work/old", resolved_path: "/work/old", pipeline: "default", agent: "claude" },
        { name: "new", path: "/work/new", resolved_path: "/work/new", pipeline: "default", agent: "pi" },
      ],
    },
  });
  state.parseTitleTag = () => ({ tag: "", rest: "Ticket" });
  state.saveEdit = () => {};

  await state.openInitModal({
    id: "kon-init", status: "open", path: "/work/old", pipeline: "default",
    agent: "claude", agent_override: true,
  });
  state.initForm.path = "/work/new";
  state.onInitPathChange();
  assert.equal(state.initForm.agent, "claude");

  state.agentPickerChoose("init", "", false);
  state.initForm.path = "/work/old";
  state.onInitPathChange();
  assert.equal(state.initForm.agent, "", "an explicit inherit choice must stay blank");

  state.selectedTicket = {
    id: "kon-edit", status: "open", path: "/work/old", pipeline: "default",
    agent: "claude", agent_override: true, body: "",
  };
  await state.startEditing();
  state.editForm.path = "/work/new";
  state.onEditPathChange();
  assert.equal(state.editForm.agent, "claude");

  state.selectedTicket = {
    id: "kon-inherit", status: "open", path: "/work/old", pipeline: "default",
    agent: "claude", body: "",
  };
  await state.startEditing();
  state.editForm.path = "/work/new";
  state.onEditPathChange();
  assert.equal(state.editForm.agent, "pi", "an untouched project value follows the new path");
});

test("a status update closes the detail picker before removing its edit host", () => {
  const { state } = pickerState();
  const closed = [];
  state.closeAgentPickerHost = (host) => { closed.push(host); };
  state.flushEditSave = () => {};
  state.selectedTicket = { id: "kon-edit", status: "open", body: "", children: [] };
  state.tickets = [{ id: "kon-edit", status: "open" }];
  state.editing = true;
  state.activeTab = "ticket";

  state.applyTicketUpdate({ id: "kon-edit", status: "in_progress", body: "" });

  assert.deepEqual(closed, ["edit"]);
  assert.equal(state.editing, false);
});

test("detail saves adding or clearing an override even when the resolved name is unchanged", async () => {
  const requests = [];
  const { state } = pickerState({
    overrides: {
      fetch: async (_url, init) => {
        requests.push(JSON.parse(init.body));
        const agent = requests.at(-1).agent;
        return {
          ok: true,
          json: async () => ({
            id: "kon-edit", status: "open", path: "/work/repo", pipeline: "default",
            agent: agent || "claude", agent_override: !!agent, body: "",
          }),
        };
      },
    },
  });
  state.tickets = [];
  state.selectedTicket = {
    id: "kon-edit", status: "open", path: "/work/repo", pipeline: "default",
    agent: "claude", body: "",
  };
  await state.startEditing();
  state.editForm.agent = "claude";
  await state.saveEdit();
  assert.equal(requests[0].agent, "claude");

  state.selectedTicket = {
    id: "kon-edit", status: "open", path: "/work/repo", pipeline: "default",
    agent: "codex", agent_override: true, body: "",
  };
  await state.startEditing();
  state.editForm.agent = "";
  await state.saveEdit();
  assert.equal(requests[1].agent, "");
});

test("agent picker keyboard movement wraps and scrolls with list scrollTop", () => {
  const list = {
    scrollTop: 10,
    getBoundingClientRect: () => ({ top: 20, bottom: 100 }),
  };
  const row = { getBoundingClientRect: () => ({ top: 95, bottom: 120 }) };
  const { state } = pickerState({
    overrides: {
      document: {
        getElementById(id) { return id.endsWith("-list") ? list : id.includes("-row-") ? row : null; },
        querySelector: () => null,
        documentElement: { style: {} },
      },
    },
  });
  state.agentPickerState("create").active = 0;
  state.agentPickerMove("create", -1);

  assert.equal(state.agentPickerActiveIndex("create"), state.agentPickerRows("create").length - 1);
  assert.equal(/\s/.test(state.agentPickerActiveID("create")), false);
  assert.equal(list.scrollTop, 30);
});

test("agent picker setup closes on outside mouse and host scrolling", () => {
  const listeners = {};
  let hidden = 0;
  const panel = { hidePopover() { hidden += 1; } };
  const document = {
    getElementById(id) { return id === "agent-picker-create" ? panel : null; },
    querySelector: () => null,
    documentElement: { style: {} },
    addEventListener(type, handler) { listeners[type] = handler; },
  };
  const { state } = pickerState({
    overrides: {
      document,
      window: { innerWidth: 600, innerHeight: 500, addEventListener() {} },
    },
  });
  state.setupAgentPicker();
  state._agentPickerOpenHost = "create";
  state.agentPickerState("create").open = true;

  listeners.mousedown({ target: { closest: () => ({}) } });
  assert.equal(state.agentPickerIsOpen("create"), true);

  listeners.scroll({ target: { closest: () => null } });
  assert.equal(state.agentPickerIsOpen("create"), false);
  assert.equal(hidden, 1);

  state._agentPickerOpenHost = "create";
  state.agentPickerState("create").open = true;
  listeners.mousedown({ target: { closest: () => null } });
  assert.equal(state.agentPickerIsOpen("create"), false);
  assert.equal(hidden, 2);
});

test("filtering resets scroll and repositions an above-trigger picker", () => {
  const { state } = pickerState();
  let positioned = 0;
  state.positionAgentPicker = () => { positioned += 1; };

  state.agentPickerSetQuery("create", "codex");

  assert.equal(state.agentPickerState("create").active, 0);
  assert.equal(positioned, 1);
});

for (const placement of [
  { name: 'flips above and clamps right', rect: { left: 280, top: 440, bottom: 475 }, viewport: { left: 0, top: 0, width: 400, height: 500 }, expected: { placeAbove: true, left: 92, top: 214 } },
  { name: 'places below with visual viewport offsets', rect: { left: 15, top: 100, bottom: 130 }, viewport: { left: 40, top: 80, width: 400, height: 500 }, expected: { placeAbove: false, left: 48, top: 136 } },
  { name: 'clamps to keyboard viewport bottom', rect: { left: 280, top: 440, bottom: 475 }, viewport: { left: 40, top: 80, width: 340, height: 300 }, expected: { placeAbove: true, left: 72, top: 152 } },
]) {
  test(`agent picker placement ${placement.name}`, () => {
    const { state } = pickerState();
    const result = state.agentPickerPlacement(placement.rect, { width: 300, height: 220 }, placement.viewport);
    const { available, ...position } = vmValue(result);
    assert.deepEqual(position, placement.expected);
    assert.ok(available >= 0);
  });
}

test("opening a supported picker uses the top layer and restores focus", async () => {
  class PopoverElement {}
  PopoverElement.prototype.showPopover = function() {};
  const calls = [];
  const trigger = {
    focus() { calls.push("trigger-focus"); },
    getBoundingClientRect: () => ({ left: 20, top: 20, right: 170, bottom: 56 }),
  };
  const panel = {
    style: {}, offsetHeight: 180,
    showPopover() { calls.push("show"); },
    hidePopover() { calls.push("hide"); },
  };
  const list = {
    style: {}, offsetHeight: 100, offsetParent: {},
    focus() { calls.push("list-focus"); }, blur() {},
  };
  const filter = { offsetParent: {}, focus() { calls.push("filter-focus"); }, blur() {} };
  const elements = {
    "new-agent": trigger,
    "agent-picker-create": panel,
    "agent-picker-create-list": list,
    "agent-picker-create-filter": filter,
  };
  const { state } = pickerState({
    overrides: {
      HTMLElement: PopoverElement,
      window: { innerWidth: 600, innerHeight: 500, addEventListener() {} },
      document: {
        getElementById(id) { return elements[id] || null; },
        querySelector: () => null,
        documentElement: { style: {} },
      },
    },
  });

  let activeScrolled = 0;
  state.scrollAgentPickerActive = () => { activeScrolled += 1; };

  assert.equal(state._agentPickerPopoverSupported, true);
  await state.openAgentPicker("create");
  assert.equal(state.agentPickerIsOpen("create"), true);
  assert.deepEqual(calls.slice(0, 2), ["show", "filter-focus"]);
  assert.match(panel.style.left, /px$/);
  assert.match(panel.style.top, /px$/);
  assert.equal(activeScrolled, 1);

  state.closeAgentPicker(true);
  assert.deepEqual(calls.slice(-2), ["hide", "trigger-focus"]);
});

function sheetLifecycleUI() {
  const dom = mobileLifecycleDOM();
  const { node, document, selectors, elements } = dom;
  const opener = node(), fallback = node();
  const dialog = node();
  const first = node({ 'data-mobile-sheet-initial-focus': '' }, dialog);
  const trigger = node({}, dialog), last = node({}, dialog);
  const panel = node({ 'data-agent-picker-panel': '' }, dialog);
  const filter = node({}, panel), list = node({}, panel);
  const backgrounds = [node(), node({ inert: '', 'aria-hidden': 'false' }), node({ 'aria-hidden': 'true' })];
  const ancestor = node();
  dialog.parent = ancestor;
  dialog.querySelectorAll = () => [trigger, first, last, filter, list];
  selectors['[data-mobile-sheet-dialog]'] = dialog;
  selectors['[data-mobile-sheet-background]'] = [...backgrounds, ancestor, first];
  selectors['[data-mobile-focus-fallback]'] = fallback;
  Object.assign(elements, {
    'mobile-new-agent': trigger,
    'agent-picker-mobile-create': panel,
    'agent-picker-mobile-create-filter': filter,
    'agent-picker-mobile-create-list': list,
  });
  Object.assign(panel, { offsetHeight: 180, showPopover() {}, hidePopover() {} });
  list.offsetHeight = 100;
  trigger.getBoundingClientRect = () => ({ left: 20, top: 200, right: 200, bottom: 240 });
  class PopoverElement {}
  PopoverElement.prototype.showPopover = function() {};
  const { state } = pickerState({ overrides: { ...dom.overrides, HTMLElement: PopoverElement } });
  document.activeElement = opener;
  state.initMobile();
  state.openActionsSheet({ id: 'sheet-ticket', status: 'paused' });
  return { dom, state, opener, fallback, dialog, first, trigger, last, panel, filter, list, backgrounds, ancestor };
}

function mobileKey(key, shiftKey = false) {
  return {
    key, shiftKey, defaultPrevented: false, stopped: false,
    preventDefault() { this.defaultPrevented = true; },
    stopImmediatePropagation() { this.stopped = true; },
  };
}

for (const exit of ['close', 'replace', 'breakpoint', 'destroy']) {
  test(`mobile sheet inerts only background siblings and restores prior attributes: ${exit}`, () => {
    const ui = sheetLifecycleUI();
    const { dom, state, backgrounds, ancestor, dialog, first, opener } = ui;
    assert.equal(dom.document.activeElement, first, 'explicit initial focus beats DOM order');
    backgrounds.forEach(el => {
      assert.equal(el.hasAttribute('inert'), true);
      assert.equal(el.getAttribute('aria-hidden'), 'true');
    });
    assert.equal(ancestor.hasAttribute('inert'), false, 'never inert the dialog ancestor');
    assert.equal(dialog.hasAttribute('inert'), false);
    assert.equal(first.hasAttribute('inert'), false, 'never inert descendants');
    if (exit === 'replace') {
      state.sheet = { type: 'schedule' };
      state._syncMobileSheet();
      assert.equal(opener.focuses.length, 0, 'sheet replacement must not flash focus onto the background');
      state.closeSheet();
    } else if (exit === 'close') state.closeSheet();
    else if (exit === 'breakpoint') { state.isMobile = false; state.mobileBreakpointChanged(); }
    else state.destroyMobile();
    assert.equal(state.sheet, null);
    assert.equal(backgrounds[0].hasAttribute('inert'), false);
    assert.equal(backgrounds[0].getAttribute('aria-hidden'), null);
    assert.equal(backgrounds[1].hasAttribute('inert'), true);
    assert.equal(backgrounds[1].getAttribute('aria-hidden'), 'false');
    assert.equal(backgrounds[2].hasAttribute('inert'), false);
    assert.equal(backgrounds[2].getAttribute('aria-hidden'), 'true');
    assert.equal(opener.focuses.length, ['close', 'replace'].includes(exit) ? 1 : 0);
    state.destroyMobile();
  });
}

for (const unavailable of ['disconnected', 'inert', 'disabled', 'hidden', 'no rects']) {
  test(`sheet focus restoration falls back when opener is ${unavailable}`, () => {
    const { dom, state, opener, fallback } = sheetLifecycleUI();
    if (unavailable === 'disconnected') opener.isConnected = false;
    if (unavailable === 'inert') opener.setAttribute('inert', '');
    if (unavailable === 'disabled') opener.setAttribute('disabled', '');
    if (unavailable === 'hidden') opener.setAttribute('aria-hidden', 'true');
    if (unavailable === 'no rects') opener.getClientRects = () => [];
    state.closeSheet();
    assert.equal(dom.document.activeElement, fallback);
    assert.equal(opener.focuses.length, 0);
    state.destroyMobile();
  });
}

test('sheet traps Tab/focus without including native picker options; Escape closes picker first', async () => {
  const ui = sheetLifecycleUI();
  const { dom, state, first, trigger, last, filter, panel, opener } = ui;
  assert.deepEqual([...state._mobileSheetFocusable()], [trigger, first, last]);
  last.focus();
  const tab = mobileKey('Tab');
  dom.document.emit('keydown', tab);
  assert.equal(tab.defaultPrevented, true);
  assert.equal(dom.document.activeElement, trigger);
  const backwards = mobileKey('Tab', true);
  dom.document.emit('keydown', backwards);
  assert.equal(dom.document.activeElement, last);
  dom.document.emit('focusin', { target: opener });
  assert.equal(dom.document.activeElement, trigger, 'redirect escaped focus inside the sheet');
  await state.openAgentPicker('mobile-create');
  assert.equal(dom.document.activeElement, filter);
  assert.equal(state.agentPickerOwnsTarget(filter), true);
  assert.equal(state.agentPickerOwnsTarget(trigger), true);
  assert.equal(state.agentPickerOwnsTarget(opener), false);
  dom.document.emit('focusin', { target: filter });
  assert.equal(dom.document.activeElement, filter, 'sheet must not steal native picker focus');
  const handled = mobileKey('Escape');
  handled.preventDefault();
  dom.document.emit('keydown', handled);
  assert.equal(state.agentPickerIsOpen('mobile-create'), true);
  const escape = mobileKey('Escape');
  dom.document.emit('keydown', escape);
  assert.equal(escape.defaultPrevented, true);
  assert.equal(escape.stopped, true, 'capture handler prevents global Escape from also closing the sheet');
  assert.equal(state._agentPickerOpenHost, null);
  assert.ok(state.sheet);
  assert.equal(dom.document.activeElement, trigger);
  assert.equal(state.agentPickerOwnsTarget(panel), false, 'closed picker owns no targets');
  const sheetEscape = mobileKey('Escape');
  dom.document.emit('keydown', sheetEscape);
  assert.equal(sheetEscape.stopped, true);
  assert.equal(state.sheet, null);
  assert.equal(dom.document.activeElement, opener);
  state.destroyMobile();
});

for (const shiftKey of [false, true]) {
  test(`native picker Tab advances within the sheet without a delayed close: shift=${shiftKey}`, async () => {
    const { dom, state, first, last } = sheetLifecycleUI();
    await state.openAgentPicker('mobile-create');
    const tab = mobileKey('Tab', shiftKey);
    state.agentPickerTab('mobile-create', tab);
    assert.equal(tab.defaultPrevented, true);
    assert.equal(state._agentPickerOpenHost, null);
    assert.equal(state._agentPickerTabTimer, null);
    assert.equal(dom.document.activeElement, shiftKey ? last : first);
    assert.ok(state.sheet);
    state.destroyMobile();
  });
}

for (const boundary of ['close', 'replace', 'destroy']) {
  test(`deferred sheet focus cannot inert/focus a superseded sheet: ${boundary}`, () => {
    const { dom, state, dialog, opener, backgrounds } = sheetLifecycleUI();
    state.closeSheet();
    const callbacks = [];
    state.$nextTick = cb => { if (cb) callbacks.push(cb); return Promise.resolve(); };
    state.openActionsSheet({ id: 'old' });
    if (boundary === 'close') state.closeSheet();
    if (boundary === 'replace') state.openActionsSheet({ id: 'new' });
    if (boundary === 'destroy') state.destroyMobile();
    const focuses = dialog.focuses.length;
    callbacks[0]();
    assert.equal(dom.document.activeElement, opener);
    assert.equal(dialog.focuses.length, focuses);
    assert.equal(backgrounds[0].hasAttribute('inert'), false);
    if (boundary === 'replace') {
      callbacks[1]();
      assert.equal(backgrounds[0].hasAttribute('inert'), true);
      assert.notEqual(dom.document.activeElement, opener);
    }
    state.destroyMobile();
  });
}

for (const target of ['filter', 'list']) {
  test(`native picker retries ${target} focus only while its opening is current`, async () => {
    const { dom, state, filter, list, trigger } = sheetLifecycleUI();
    if (target === 'list') state.configCache.agents = state.configCache.agents.slice(0, 5);
    const control = target === 'filter' ? filter : list;
    control.offsetParent = null;
    await state.openAgentPicker('mobile-create');
    assert.equal(dom.frames.size, 1);
    control.offsetParent = {};
    dom.runFrame();
    assert.equal(dom.document.activeElement, control);
    assert.equal(state._agentPickerFocusFrame, null);
    state.closeAgentPicker(true);
    assert.equal(dom.document.activeElement, trigger);
    control.offsetParent = null;
    await state.openAgentPicker('mobile-create');
    const stale = [...dom.frames.values()][0];
    state.closeAgentPicker(false);
    control.offsetParent = {};
    const focused = control.focuses.length;
    stale();
    assert.equal(control.focuses.length, focused);
    assert.equal(dom.frames.size, 0);
    state.destroyMobile();
  });
}

for (const failure of ['unsupported', 'missing panel', 'missing method', 'show throws', 'closed before tick']) {
  test(`native picker opening safely falls back or cancels: ${failure}`, async () => {
    const { dom, state, panel } = sheetLifecycleUI();
    const ticks = deferred();
    state.$nextTick = () => ticks.promise;
    if (failure === 'unsupported') state._agentPickerPopoverSupported = false;
    if (failure === 'missing panel') delete dom.elements['agent-picker-mobile-create'];
    if (failure === 'missing method') panel.showPopover = undefined;
    if (failure === 'show throws') panel.showPopover = () => { throw new Error('not available'); };
    const opening = state.openAgentPicker('mobile-create');
    if (failure === 'closed before tick') state.closeAgentPicker(false);
    ticks.resolve();
    await opening;
    assert.equal(state._agentPickerOpenHost, null);
    assert.equal(state.agentPickerState('mobile-create').open, false);
    assert.equal(state._agentPickerPopoverSupported, failure === 'closed before tick');
    assert.equal(dom.frames.size, 0);
    assert.ok(state.sheet, 'picker failure does not dismiss the containing sheet');
    state.destroyMobile();
  });
}

test("the shared picker markup wires all hosts, ARIA, Popover, and fallback behavior", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  const hosts = [...html.matchAll(/x-data="\{ pickerHost: '([^']+)' \}" x-html="agentPickerControl"/g)].map((match) => match[1]);
  assert.deepEqual(hosts.sort(), ["automations", "create", "edit", "init", "mobile-create"]);
  assert.match(uiSource, /popover="manual" role="presentation"/);
  assert.match(uiSource, /role="listbox" tabindex="-1"/);
  assert.match(uiSource, /role="option" :aria-selected="row\.selected"/);
  assert.match(uiSource, /:aria-activedescendant="agentPickerActiveID\(pickerHost\)"/);
  assert.match(uiSource, /:aria-labelledby="agentPickerLabelID\(pickerHost\) \+ ' ' \+ agentPickerValueID\(pickerHost\)"/);
  assert.match(uiSource, /x-show="!_agentPickerPopoverSupported"/);
  assert.match(uiSource, /<div x-show="_agentPickerPopoverSupported" :id="agentPickerPanelID/);
  assert.match(uiSource, /typeof HTMLElement\.prototype\.showPopover === 'function'/);
  assert.equal((html.match(/:for="_agentPickerPopoverSupported \? agentPickerTriggerID/g) || []).length, hosts.length);
  assert.equal(/x-model="(?:initForm|editForm|createForm)\.agent"/.test(html), false);
  assert.match(html, /\.agent-picker-pop \{ animation: agent-picker-pop 0\.12s ease-out; \}/);
  assert.match(html, /\.agent-picker-pop \{ animation: none; \}/);
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

  assert.match(html, /@keydown\.escape\.window="if \(_agentPickerOpenHost\) closeAgentPicker\(true\); else if \(paletteOpen\) palettePop\(\); else if \(deleteModal\)/);
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
function routerState(hash = "", overrides = {}) {
  const location = { protocol: "http:", host: "localhost:8080", hash };
  const state = loadKontoraState({ location, ...overrides });
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
    { hash: "#/automations", view: "automations", ticket: null },
    { hash: "#/automations?state=paused&q=notes", view: "automations", ticket: null },
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

const automationSpecFields = ['agent', 'enabled', 'model', 'name', 'overlap', 'path', 'prompt', 'schedule', 'timeout', 'timezone', 'workspace'];
const automationFires = ['2026-11-10T08:00:00Z', '2026-11-11T08:00:00Z', '2026-11-12T08:00:00Z', '2026-11-13T08:00:00Z'];

function automationUI() {
  const definitions = new Map(), receipts = new Map(), runReceipts = new Map(), requests = [], timers = new Map(), events = [], listeners = new Map();
  let timer = 0, next = 1;
  const behavior = { available: true, hook: null, confirm: true, createResponse: null, runResponse: null };
  const response = (data, status = 200) => ({ ok: status < 400, status, json: async () => structuredClone(data) });
  const history = Array.from({ length: 29 }, (_, i) => ({ automation_id: 'auto-1', key: 'morning-review', n: 29 - i, revision: 3,
    invocation_id: 'invocation-' + i, phase: 'terminal', outcome: ['complete', 'failed', 'skipped'][i % 3], trigger: i % 2 ? 'manual' : 'schedule',
    scheduled_for: automationFires[0], accepted_at: automationFires[0], started_at: automationFires[0], completed_at: '2026-11-10T08:04:12Z',
    exit_code: i % 3 === 1 ? 1 : 0, result: 'Result from run ' + (29 - i), cost: i % 3 === 2 ? null : '0.31' }));
  const lines = [{ at: automationFires[0], source: 'kontora', kind: 'lifecycle', text: 'automation auto-1 · run #29 · trigger schedule' },
    { at: automationFires[0], source: 'agent', kind: 'tool_call', text: 'Inspect repository' },
    { at: automationFires[0], source: 'agent', kind: 'success', text: 'No regressions found <script>not markup</script>' }];
  const fetch = async (url, options = {}) => {
    const method = options.method || 'GET', body = options.body ? JSON.parse(options.body) : undefined;
    requests.push({ url, method, body });
    const intercepted = await behavior.hook?.(url, method, body);
    if (intercepted) return intercepted;
    if (url === '/api/automations' && method === 'GET') return response({ available: behavior.available, error: behavior.available ? undefined : 'Store locked', definitions: [...definitions.values()] });
    if (url === '/api/automations/preview' && method === 'POST') {
      assert.deepEqual(Object.keys(body).sort(), ['schedule', 'timezone']);
      assert.equal(typeof body.schedule, 'string');
      return body.timezone === 'Invalid/Zone' ? response({ error: 'Invalid timezone' }, 400) : response({ occurrences: automationFires, human: 'Every weekday at 09:00' });
    }
    if (url === '/api/automations' && method === 'POST') {
      assert.deepEqual(Object.keys(body).sort(), ['definition', 'key', 'request_id']);
      assert.ok(body.request_id && body.key);
      assert.deepEqual(Object.keys(body.definition).sort(), automationSpecFields);
      if (receipts.has(body.request_id)) return behavior.createResponse?.(receipts.get(body.request_id)) || response(receipts.get(body.request_id));
      const row = { ...body.definition, id: 'auto-' + next++, key: body.key, revision: 1, created_at: automationFires[0], next: automationFires[0], next_runs: automationFires,
        schedule_human: 'Every weekday at 09:00', project: 'Repository', running: false, runs_total: 214, success_rate: 95, avg_wall: 252, spend_30d: '12.34', priced_runs: 19, tracked_runs: 20 };
      definitions.set(row.id, row); receipts.set(body.request_id, structuredClone(row));
      return behavior.createResponse?.(row) || response(row, 201);
    }
    const parsed = new URL(url, 'http://fixture'), match = parsed.pathname.match(/^\/api\/automations\/([^/]+)(?:\/(run|stop|pause|resume|runs)(?:\/(\d+)\/log)?)?$/);
    assert.ok(match, 'Unexpected automation request: ' + method + ' ' + url);
    const row = definitions.get(match[1]), action = match[2];
    if (!row) return response({ error: 'Not found' }, 404);
    if (method === 'GET' && action === 'runs') {
      if (match[3]) return response({ lines, raw: lines.map(line => line.text).join('\n') });
      const offset = Number(parsed.searchParams.get('offset')), limit = Number(parsed.searchParams.get('limit'));
      assert.ok(limit >= 20 && limit <= 100);
      return response({ runs: history.slice(offset, offset + limit), total: history.length });
    }
    if (method === 'POST' && action === 'run') {
      assert.deepEqual(Object.keys(body), ['request_id']); assert.ok(body.request_id);
      const key = row.id + ':' + body.request_id;
      if (!runReceipts.has(key)) {
        const run = { automation_id: row.id, key: row.key, n: history.length + 1, revision: 1, invocation_id: body.request_id, trigger: 'manual', accepted_at: automationFires[0],
          phase: row.running && row.overlap === 'queue' ? 'queued' : row.running && row.overlap === 'skip' ? 'terminal' : 'running', outcome: row.running && row.overlap === 'skip' ? 'skipped' : '' };
        runReceipts.set(key, run); history.unshift(run);
        if (run.phase === 'running') definitions.set(row.id, { ...row, running: true, current_run: run });
      }
      return behavior.runResponse?.(runReceipts.get(key)) || response(runReceipts.get(key), 202);
    }
    if (method === 'POST' && action === 'stop') {
      assert.deepEqual(body, { n: row.current_run.n });
      const run = { ...row.current_run, phase: 'terminal', outcome: 'failed', exit_code: 143, result: 'stopped by user' };
      definitions.set(row.id, { ...row, running: false, current_run: null, last_run: run });
      return response(run, 202);
    }
    if (method === 'GET' && !action) return response(row);
    if (body.revision !== row.revision) return response({ error: 'Revision conflict' }, 409);
    if (method === 'DELETE' && !action) { definitions.delete(row.id); return response(null, 204); }
    if (method === 'POST' && ['pause', 'resume'].includes(action)) {
      assert.deepEqual(Object.keys(body), ['revision']);
      const saved = { ...row, revision: row.revision + 1, enabled: action === 'resume' };
      definitions.set(row.id, saved); return response(saved);
    }
    assert.equal(method, 'PUT'); assert.equal(action, undefined);
    assert.deepEqual(Object.keys(body).sort(), ['definition', 'revision']);
    assert.deepEqual(Object.keys(body.definition).sort(), automationSpecFields);
    const saved = { ...row, ...body.definition, revision: row.revision + 1 };
    definitions.set(saved.id, saved); return response(saved);
  };
  function mount(hash = '#/automations') {
    const location = { protocol: 'http:', host: 'fixture', hash };
    const state = loadKontoraState({ fetch, location, EventSource: makeEventSourceFake(events),
      setTimeout: fn => { timers.set(++timer, fn); return timer; }, clearTimeout: id => timers.delete(id),
      window: { innerWidth: 1280, confirm: () => behavior.confirm, addEventListener: (name, fn) => listeners.set(name, fn), removeEventListener: name => listeners.delete(name) } });
    state.$nextTick = callback => { callback?.(); return Promise.resolve(); }; state.$refs = {};
    state.configCache = { projects: [{ name: 'Repository', path: '/example/repository', agent: 'example-agent' }], agents: ['example-agent'] };
    state.openTerminal = state.closeTerminal = () => {};
    state.currentView = 'automations';
    return state;
  }
  return { state: mount(), mount, definitions, requests, history, lines, events, listeners, receipts, runReceipts, behavior, response,
    async preview() { const jobs = [...timers.values()]; timers.clear(); await Promise.all(jobs.map(fn => fn())); } };
}

async function automationDraft(ui) {
  await ui.state.automationsOpen(); ui.state.automationsNew();
  Object.assign(ui.state.automationsDraft, { name: 'Morning review', prompt: 'Review recent changes.', timezone: 'Europe/Berlin' });
  ui.state.automationsSchedulePreview(); await ui.preview();
}
async function createAutomation(ui) { await automationDraft(ui); await ui.state.automationsSaveDraft(); }
async function editAutomation(ui) { await createAutomation(ui); ui.state.automationsEdit(); await ui.preview(); }

test('automations create, open detail, edit and reload the full spec without legacy fields', async () => {
  const ui = automationUI(), { state } = ui;
  await automationDraft(ui);
  assert.equal(state.automationsDraft.enabled, true);
  assert.equal(state.automationsDraft.workspace, 'worktree'); assert.equal(state.automationsDraft.overlap, 'skip'); assert.equal(state.automationsDraft.timeout, '30m');
  state.automationsDraft.model = 'sonnet'; state.automationsDraft.workspace = 'repo'; state.automationsDraft.overlap = 'replace';
  await state.automationsSaveDraft();
  assert.equal(state.automationsEditing, false); assert.equal(state.automationsDetail, true); assert.equal(state.routeHash(), '#/automations/auto-1');
  state.automationsEdit(); await ui.preview();
  state.automationsDraft.prompt = 'Updated instructions'; await state.automationsSaveDraft();
  const row = ui.definitions.get('auto-1');
  assert.equal(row.revision, 2); assert.equal(row.prompt, 'Updated instructions'); assert.equal(row.model, 'sonnet'); assert.equal(row.overlap, 'replace');
  const reloaded = ui.mount('#/automations/auto-1/edit'); await reloaded.applyRoute();
  assert.equal(reloaded.automationsDraft.prompt, row.prompt); assert.equal(reloaded.automationsDraft.revision, 2); assert.equal(reloaded.automationsDirty(), false);
  state.automationsDispose(); reloaded.automationsDispose();
});

for (const [hash, mode] of [['#/automations', 'list'], ['#/automations/new', 'new'], ['#/automations/auto-1', 'detail'], ['#/automations/auto-1/edit', 'edit']]) {
  test('automation route round-trip and query preservation: ' + mode, async () => {
    const ui = automationUI(); await createAutomation(ui);
    const route = hash + '?state=enabled&q=project%3ARepository';
    const state = ui.mount(route); await state.applyRoute();
    assert.equal(state.currentView, 'automations'); assert.equal(state.routeHash(), route);
    assert.equal(state.automationsFilter, 'enabled'); assert.equal(state.automationsQuery, 'project:Repository');
    assert.equal(state.automationsEditing, mode === 'new' || mode === 'edit');
    assert.equal(state.automationsDetail, mode === 'detail' || mode === 'edit');
    if (mode === 'detail' || mode === 'edit') assert.equal(state.automationsSelected, 'auto-1');
    state.automationsDispose(); ui.state.automationsDispose();
  });
}

test('automation saves require each spec field and an authoritative preview for the current timezone', async () => {
  const ui = automationUI(), { state } = ui; await automationDraft(ui);
  assert.equal(!!state.automationsCanSave(), true);
  for (const field of ['name', 'prompt', 'path', 'agent']) {
    const saved = state.automationsDraft[field]; state.automationsDraft[field] = '  ';
    assert.equal(!!state.automationsCanSave(), false, field); state.automationsDraft[field] = saved;
  }
  state.automationsDraft.timezone = 'UTC'; assert.equal(!!state.automationsCanSave(), false);
  state.automationsSchedulePreview(); await ui.preview(); assert.equal(!!state.automationsCanSave(), true);
  ui.behavior.available = false; await state.automationsRefresh(); assert.equal(!!state.automationsCanSave(), false);
  state.automationsDispose();
});

test('automation preview debounces cron and intervals, formats timezone and rejects stale responses', async () => {
  const ui = automationUI(), { state } = ui; await automationDraft(ui);
  ui.requests.length = 0;
  state.automationsDraft.schedule = '0 3 * * *'; state.automationsSchedulePreview();
  state.automationsDraft.schedule = '30m'; state.automationsSchedulePreview();
  assert.equal(ui.requests.length, 0); await ui.preview();
  assert.deepEqual(ui.requests.at(-1).body, { schedule: '30m', timezone: 'Europe/Berlin' });
  assert.deepEqual([...state.automationsPreview], automationFires); assert.match(state.automationsDateLabel(automationFires[0]), /09:00/);
  assert.equal(state.automationsPreviewHuman, 'Every weekday at 09:00');
  const older = deferred();
  ui.behavior.hook = (url, method, body) => url.endsWith('/preview') && body.schedule === '6h' && body.timezone === 'Europe/Berlin' ? older.promise : undefined;
  state.automationsDraft.schedule = '6h'; state.automationsSchedulePreview(); const pending = ui.preview();
  state.automationsDraft.timezone = 'Invalid/Zone'; state.automationsSchedulePreview(); await ui.preview();
  older.resolve(ui.response({ occurrences: ['2030-01-01T00:00:00Z'], human: 'Stale' })); await pending;
  assert.equal(state.automationsPreview.length, 0); assert.match(state.automationsPreviewError, /Invalid timezone/); assert.equal(!!state.automationsCanSave(), false);
  for (const schedule of ['', '0 9 * *', '@daily']) {
    state.automationsDraft.schedule = schedule; state.automationsSchedulePreview();
    assert.ok(state.automationsPreviewError); assert.equal(state.automationsPreviewLoading, false);
  }
  state.automationsDispose();
});

for (const kind of ['lost body', 'invalid identity', 'invalid revision']) {
  test('automation create receipt retry is idempotent after ' + kind, async () => {
    const ui = automationUI(), { state } = ui; await automationDraft(ui);
    ui.behavior.createResponse = row => kind === 'lost body' ? { ok: true, status: 201, json: async () => { throw new Error('Lost body'); } }
      : ui.response({ ...row, ...(kind === 'invalid identity' ? { id: '' } : { revision: 0 }) });
    await state.automationsSaveDraft(true);
    assert.equal(ui.definitions.size, 1); assert.equal(state.automationsPending, false); assert.match(state.automationsNotice, /Retry save/);
    assert.equal(ui.runReceipts.size, 0);
    ui.behavior.createResponse = null; await state.automationsSaveDraft();
    const posts = ui.requests.filter(r => r.method === 'POST' && r.url === '/api/automations');
    assert.equal(posts.length, 2); assert.deepEqual(posts[0].body, posts[1].body); assert.equal(ui.definitions.size, 1);
    assert.equal(ui.runReceipts.size, 1, 'Retry keeps the original save-and-run intent'); assert.equal(state.automationsTab, 'session');
    state.automationsDispose();
  });
}

test('pending saves block duplicate submission and navigation', async () => {
  const ui = automationUI(), { state } = ui; await automationDraft(ui);
  const gate = deferred(); ui.behavior.hook = (url, method) => method === 'POST' && url === '/api/automations' ? gate.promise : undefined;
  const saving = state.automationsSaveDraft();
  assert.equal(state.automationsPending, true); assert.equal(state.automationsNew(), false);
  await state.automationsSaveDraft(); await state.gotoView('board');
  assert.equal(state.currentView, 'automations'); assert.equal(ui.requests.filter(r => r.url === '/api/automations' && r.method === 'POST').length, 1);
  gate.resolve(ui.response({ error: 'Store unavailable' }, 503)); await saving;
  assert.equal(state.automationsPending, false); assert.match(state.automationsNotice, /Retry save/); state.automationsDispose();
});

for (const status of [400, 401, 404, 409, 503]) {
  test(`automation save ${status} preserves the draft and saved revision`, async () => {
    const ui = automationUI(), { state } = ui; await editAutomation(ui);
    state.automationsDraft.prompt = 'Unsaved change'; const baseline = state.automationsBaseline;
    ui.behavior.hook = (url, method) => method === 'PUT' ? ui.response({ error: 'Rejected ' + status }, status) : undefined;
    await state.automationsSaveDraft();
    assert.equal(state.automationsDraft.prompt, 'Unsaved change'); assert.equal(state.automationsBaseline, baseline); assert.equal(ui.definitions.get('auto-1').revision, 1);
    assert.equal(state.automationsPending, false); assert.match(state.automationsNotice, /Rejected/); assert.equal(!!state.automationsConflict, status === 409);
    if (status === 401) assert.equal(state.needsAuth, true);
    state.automationsDispose();
  });
}

test('automation SSE invalidation and reconnect preserve dirty drafts and require an explicit reload', async () => {
  const ui = automationUI(), { state } = ui; await editAutomation(ui);
  state.companionRefresh = () => {}; state.connectSSE();
  state.automationsDraft.prompt = 'Unsaved instructions';
  ui.definitions.set('auto-1', { ...ui.definitions.get('auto-1'), revision: 2, prompt: 'Other browser' });
  ui.events[0].emit('automations_updated'); await flushMicrotasks(); ui.events[0].emit('open'); await flushMicrotasks();
  assert.equal(state.automationsDraft.prompt, 'Unsaved instructions'); assert.equal(state.automationsDraft.revision, 1); assert.match(state.automationsConflict, /changed/);
  const puts = ui.requests.filter(r => r.method === 'PUT').length; await state.automationsSaveDraft(); assert.equal(ui.requests.filter(r => r.method === 'PUT').length, puts);
  ui.behavior.confirm = false; await state.automationsReload(); assert.equal(state.automationsDraft.prompt, 'Unsaved instructions');
  ui.behavior.confirm = true; await state.automationsReload(); assert.equal(state.automationsDraft.prompt, 'Other browser'); assert.equal(state.automationsConflict, '');
  state.automationsDispose(); assert.equal(ui.listeners.has('beforeunload'), false); state._eventSource?.close();
});

test('late automation detail, list and log responses cannot overwrite a newer view', async () => {
  const ui = automationUI(), { state } = ui; await createAutomation(ui); await state.automationsBack();
  const detail = deferred(); ui.behavior.hook = (url, method) => method === 'GET' && url === '/api/automations/auto-1' ? detail.promise : undefined;
  const selecting = state.automationsSelect(state.automations[0]); state.automationsNew(); state.automationsDraft.prompt = 'New draft';
  detail.resolve(ui.response(ui.definitions.get('auto-1'))); await selecting;
  assert.equal(state.automationsSelected, null); assert.equal(state.automationsDraft.prompt, 'New draft');
  ui.behavior.hook = null; await state.automationsSelect(state.automations[0]); state.automationsEdit(); await ui.preview();
  const list = deferred(); ui.behavior.hook = (url, method) => method === 'GET' && url === '/api/automations' ? list.promise : undefined;
  const refreshing = state.automationsRefresh(); state.automationsDraft.prompt = 'Saved during refresh'; await state.automationsSaveDraft();
  list.resolve(ui.response({ available: true, definitions: [{ ...ui.definitions.get('auto-1'), revision: 1, prompt: 'Stale' }] })); await refreshing;
  assert.equal(state.automationsRow().revision, 2); assert.equal(state.automationsRow().prompt, 'Saved during refresh');
  const log = deferred(); ui.behavior.hook = url => url.endsWith('/log') ? log.promise : undefined;
  const loading = state.automationsOpenRun(state.automationsHistory[0]); state.automationsNew();
  log.resolve(ui.response({ lines: [{ text: 'Stale log' }] })); await loading;
  assert.equal(state.automationsLog.some(line => line.text === 'Stale log'), false); state.automationsDispose();
});

test('automation refresh does not regress a run snapshot at the same definition revision', async () => {
  const ui = automationUI(), { state } = ui; await createAutomation(ui);
  const row = ui.definitions.get('auto-1');
  const run = { automation_id: row.id, n: 30, revision: 4, phase: 'running', trigger: 'manual' };
  state.automationsUpsert({ ...row, running: true, current_run: run });
  ui.definitions.set(row.id, { ...row, running: true, current_run: { ...run, revision: 2, phase: 'preparing' } });
  await state.automationsRefresh(); assert.equal(state.automationsRow().current_run.revision, 4); assert.equal(state.automationsRow().current_run.phase, 'running');
  state.automationsUpsert({ ...row, running: false, current_run: null, last_run: { ...run, revision: 5, phase: 'terminal', outcome: 'complete' } });
  await state.automationsRefresh(); assert.equal(state.automationsRow().running, false); assert.equal(state.automationsRow().last_run.revision, 5);
  state.automationsDispose();
});

test('automation run rejection clears only the rejected request identity', async () => {
  const ui = automationUI(), { state } = ui; await createAutomation(ui);
  ui.behavior.hook = (url, method) => method === 'POST' && url.endsWith('/run') ? ui.response({ error: 'Rejected run' }, 409) : undefined;
  await state.automationsRun(); assert.equal(Object.keys(state.automationsRunReceipts).length, 0); assert.equal(ui.runReceipts.size, 0);
  ui.behavior.hook = null; await state.automationsRun();
  const posts = ui.requests.filter(r => r.url.endsWith('/run')); assert.equal(posts.length, 2); assert.notEqual(posts[0].body.request_id, posts[1].body.request_id);
  assert.equal(ui.runReceipts.size, 1); state.automationsDispose();
});

test('automation toggles use saved revisions and structured filters without persisting a dirty draft', async () => {
  const ui = automationUI(), { state } = ui; await editAutomation(ui);
  state.automationsFilter = 'enabled'; state.automationsQuery = 'project:Repository agent:example MORNING'; assert.equal(state.automationsRows().length, 1);
  state.automationsDraft.prompt = 'Unsaved prompt'; await state.automationsToggle(state.automationsRow());
  assert.equal(ui.definitions.get('auto-1').enabled, false); assert.equal(ui.definitions.get('auto-1').prompt, 'Review recent changes.');
  assert.equal(state.automationsDraft.prompt, 'Unsaved prompt'); assert.match(state.automationsConflict, /draft is preserved/); assert.equal(state.automationsRows().length, 0);
  await state.automationsToggle(state.automationsRow()); assert.equal(ui.definitions.get('auto-1').enabled, true);
  assert.deepEqual(ui.requests.filter(r => /\/(pause|resume)$/.test(r.url)).map(r => r.body), [{ revision: 1 }, { revision: 2 }]); state.automationsDispose();
});

for (const policy of ['skip', 'queue', 'replace']) {
  test('automation run retries retain identity and apply overlap: ' + policy, async () => {
    const ui = automationUI(), { state } = ui; await createAutomation(ui);
    const row = { ...ui.definitions.get('auto-1'), overlap: policy, running: true, current_run: { n: 29, revision: 1, phase: 'running' } };
    ui.definitions.set(row.id, row); await state.automationsRefresh();
    ui.behavior.runResponse = () => { throw new Error('Response lost after allocation'); };
    await state.automationsRun(); assert.match(state.automationsError, /Retry run now/); assert.equal(ui.runReceipts.size, 1);
    ui.behavior.runResponse = null; await state.automationsRun();
    const posts = ui.requests.filter(r => r.url.endsWith('/run'));
    assert.equal(posts.length, 2); assert.deepEqual(posts[0].body, posts[1].body); assert.equal(ui.runReceipts.size, 1); assert.equal(Object.keys(state.automationsRunReceipts).length, 0);
    const run = [...ui.runReceipts.values()][0];
    assert.equal(run.phase, policy === 'skip' ? 'terminal' : policy === 'queue' ? 'queued' : 'running');
    if (policy === 'skip') assert.equal(run.outcome, 'skipped');
    await state.automationsStop(); const stop = ui.requests.find(r => r.url.endsWith('/stop'));
    assert.deepEqual(stop.body, { n: policy === 'replace' ? run.n : 29 }); assert.equal(state.automationsRow().running, false); assert.equal(state.automationsRow().last_run.exit_code, 143);
    state.automationsDispose();
  });
}

for (const phase of ['queued', 'preparing', 'running', 'stopping', 'recovery_required']) {
  test('automation stop remains available for unfinished phase: ' + phase, async () => {
    const ui = automationUI(), { state } = ui; await createAutomation(ui);
    const row = { ...ui.definitions.get('auto-1'), running: phase === 'running', current_run: { n: 29, phase } };
    ui.definitions.set(row.id, row); await state.automationsRefresh();
    assert.equal(state.automationsCanStop(), true);
    await state.automationsStop();
    assert.deepEqual(ui.requests.find(request => request.url.endsWith('/stop')).body, { n: 29 });
    assert.equal(state.automationsCanStop(), false); state.automationsDispose();
  });
}

test('automation terminal subjects use server-resolved run paths and change identity between runs', async () => {
  const container = { textContent: '' }, sockets = [];
  const { ctx, state } = loadKontoraContext({
    document: { getElementById: id => id === 'automation-terminal-session' ? container : null, querySelector: () => null, documentElement: { style: {} } },
    WebSocket: class {
      static OPEN = 1;
      constructor(url) { this.url = url; this.readyState = 1; sockets.push(this); }
      send() {}
      close() { this.readyState = 3; }
    },
  });
  stubXterm(ctx.termState);
  state.$nextTick = callback => { callback?.(); return Promise.resolve(); };
  state.currentView = 'automations'; state.automationsDetail = true; state.automationsAvailable = true; state.automationsSelected = 'auto/encoded'; state.automationsTab = 'session';
  state.selectedTicket = { id: 'unrelated-ticket', status: 'in_progress' };
  state.automations = [{ id: 'auto/encoded', running: true, current_run: { n: 7, phase: 'running', session: 'server-only-session', window: 'do-not-send' } }];
  assert.equal(state.outputSubjectKey(), 'automation:auto/encoded:7'); assert.equal(state.outputContainerID('terminal'), 'automation-terminal-session'); assert.equal(state.terminalWanted(), true);
  await state.openTerminal();
  assert.equal(sockets.length, 1); assert.match(sockets[0].url, /\/ws\/automations\/auto%2Fencoded\/7\?/);
  assert.equal(new URL(sockets[0].url).searchParams.has('stream'), false);
  assert.equal(/server-only-session|do-not-send|unrelated-ticket|[?&](session|window)=/.test(sockets[0].url), false);
  const term = ctx.termState.term;
  for (const handler of term.parserHandlers) assert.equal(handler.handler('?'), false);
  ctx.termState.fit.dimensions = { cols: 120, rows: 40 };
  sockets[0].onopen();
  assert.deepEqual([term.cols, term.rows], [120, 40]);
  ctx.termState.fit.dimensions = { cols: 100, rows: 30 };
  state.refitTerminal();
  assert.deepEqual([term.cols, term.rows], [100, 30]);
  state.closeTerminal(); state.automations[0].current_run = { n: 8, phase: 'running' };
  assert.equal(state.outputSubjectKey(), 'automation:auto/encoded:8'); await state.openTerminal();
  assert.equal(sockets.length, 2); assert.match(sockets[1].url, /\/ws\/automations\/auto%2Fencoded\/8\?/); assert.equal(sockets[0].readyState, 3);
  state.closeTerminal(); state.automationsEditing = true; assert.equal(state.outputSubject(), null); assert.equal(state.terminalWanted(), false);
  state.currentView = 'board'; assert.equal(state.outputSubjectKey(), 'primary:unrelated-ticket');
});

test('automation history, strip, logs and meters use returned run data, not ticket occurrences', async () => {
  const ui = automationUI(), { state } = ui;
  ui.behavior.available = false; await state.automationsOpen(); assert.equal(state.automationsAvailable, false); assert.match(state.automationsError, /Store locked/);
  ui.behavior.available = true; await createAutomation(ui);
  assert.equal(state.automationsHistory.length, 24); assert.equal(state.automationsHistoryTotal, 29);
  assert.deepEqual([...state.automationsStrip()].map(run => run.n), ui.history.slice(0, 24).map(run => run.n).reverse());
  assert.equal(state.automationsStripSummary(), '8 complete · 8 failed · 8 skipped');
  assert.equal(state.automationsRow().runs_total, 214); assert.equal(state.automationsRow().success_rate, 95); assert.equal(state.automationsDuration(state.automationsRow().avg_wall), '4m 12s'); assert.equal(state.automationsCost(state.automationsRow().spend_30d), '$12.34');
  assert.equal(state.automationsWall(state.automationsHistory[0]), '4m 12s'); assert.equal(state.automationsCost(null), '—');
  await state.automationsOpenRun(state.automationsHistory[0]); assert.deepEqual(vmValue(state.automationsLog), ui.lines);
  assert.equal(state.automationsRawURL(), '/api/automations/auto-1/runs/29/log?raw=1');
  await state.automationsStepRun(1); assert.equal(state.automationsLogRun.n, 28); await state.automationsStepRun(-1); assert.equal(state.automationsLogRun.n, 29);
  state.automationsAllRuns(); assert.equal(state.automationsLogRun, null);
  await state.automationsLoadHistory(true); assert.equal(state.automationsHistory.length, 29); assert.equal(new Set(state.automationsHistory.map(run => run.n)).size, 29);
  state.automationsDispose();
});

test('automation duplicate resets identity and schedule state; delete uses the captured revision', async () => {
  const ui = automationUI(), { state } = ui; await createAutomation(ui);
  state.automationsNew(state.automationsRow()); assert.equal(state.automationsDraft.name, 'Morning review (copy)'); assert.equal(state.automationsDraft.enabled, false); assert.equal(state.automationsDraft.id, undefined);
  await ui.preview(); await state.automationsSaveDraft(); assert.equal(ui.definitions.size, 2); assert.equal(state.automationsSelected, 'auto-2');
  state.automationsOpenDelete(); assert.equal(state.automationsDeleteOpen, true); state.automationsCloseDelete(); assert.equal(ui.definitions.size, 2);
  state.automationsOpenDelete(); await state.automationsDelete(); assert.equal(ui.definitions.size, 1); assert.equal(state.automationsDetail, false); assert.equal(state.routeHash(), '#/automations');
  assert.deepEqual(ui.requests.find(r => r.method === 'DELETE').body, { revision: 1 }); assert.match(state.automationsToast, /logs kept/); state.automationsDispose();
});

for (const action of ['select', 'new', 'back', 'navigate', 'hash']) {
  test('dirty automation drafts guard ' + action, async () => {
    const ui = automationUI(), { state } = ui; await editAutomation(ui); ui.behavior.confirm = false;
    state.automationsDraft.prompt = 'Unsaved instructions';
    if (action === 'select') await state.automationsSelect(state.automationsRow());
    if (action === 'new') state.automationsNew();
    if (action === 'back') await state.automationsBack();
    if (action === 'navigate') await state.gotoView('board');
    if (action === 'hash') await state.navigateDestination(state.parseHash('#/automations/new'));
    assert.equal(state.currentView, 'automations'); assert.equal(state.automationsSelected, 'auto-1'); assert.equal(state.automationsEditing, true); assert.equal(state.automationsDraft.prompt, 'Unsaved instructions');
    let prevented = false; ui.listeners.get('beforeunload')({ preventDefault() { prevented = true; } }); assert.equal(prevented, true);
    ui.behavior.confirm = true; await state.automationsBack(); assert.equal(state.automationsEditing, false); assert.equal(state.automationsDetail, true); state.automationsDispose();
  });
}

function archiveRouteUI(hash = '#/archive/t/kon-history', hook) {
  const requests = [];
  const archived = { id: 'kon-history', source: 'primary', status: 'archived', archived_from: 'done', title: 'Historical ticket', description: '# Archived body', body: '# Archived body', history: [] };
  const live = { id: 'kon-visible', source: 'primary', status: 'open', title: 'Visible ticket', history: [] };
  const { state, location } = routerState(hash, { fetch: async (url, options) => {
    requests.push(url);
    const result = await hook?.(url, options);
    if (result) return result;
    if (url === '/api/tickets/archived') return { ok: true, json: async () => ({ tickets: [] }) };
    if (url === '/api/tickets/kon-history') return { ok: true, json: async () => ({ ...archived }) };
    if (url === '/api/tickets/kon-visible') return { ok: true, json: async () => ({ ...live }) };
    assert.equal(url, '/api/tickets/kon-deleted');
    return { ok: false, status: 404, json: async () => ({ error: 'Not found' }) };
  } });
  state.fetchTicketCost = state.fetchChain = state.fetchActivity = () => {};
  state.tickets = [live];
  return { state, location, requests, archived };
}

test('primary history lookup opens an addressable read-only archive without inheriting a secondary source', async () => {
  const ui = archiveRouteUI('#/t/kon-history'), { state, location } = ui;
  state.selectedTicket = { id: 'kon-history', source: 'team', status: 'open' };
  await state.applyRoute();
  assert.equal(state.currentView, 'archive');
  assert.equal(state.archiveDetailOpen(), true);
  assert.equal(state.isSecondary(state.selectedTicket), false);
  assert.equal(state.editing, false);
  assert.equal(location.hash, '#/archive/t/kon-history');
  assert.equal(state.selectedTicket.body, '# Archived body');
  state.applyTicketUpdate({ ...ui.archived, title: 'Updated through SSE', body: undefined });
  assert.equal(state.selectedTicket.title, 'Updated through SSE');
  assert.equal(state.selectedTicket.body, '# Archived body');
  assert.equal(state.routeHash(), '#/archive/t/kon-history');
  assert.ok(ui.requests.every(url => !url.includes('/sources/')));
  const reloaded = archiveRouteUI(location.hash);
  await reloaded.state.applyRoute();
  assert.equal(reloaded.state.archiveDetailOpen(), true);
  assert.equal(reloaded.location.hash, '#/archive/t/kon-history');
  location.hash = '#/archive'; await state.applyRoute();
  assert.equal(state.selectedTicket, null);
  location.hash = '#/archive/t/kon-history'; await state.applyRoute();
  assert.equal(state.selectedTicket.id, 'kon-history');
  state.archiveCloseDetail();
  assert.equal(location.hash, '#/archive');
});

test('archive reconnect refreshes the selected primary detail and deletion marks its reference missing', async () => {
  const ui = archiveRouteUI(), { state, location } = ui;
  await state.applyRoute();
  const opened = [];
  state.companionRefresh = () => {};
  state.resyncTickets = () => {};
  state.connectSSE = loadKontoraState({ EventSource: makeEventSourceFake(opened) }).connectSSE;
  state.connectSSE();
  ui.archived.body = '# Updated after reconnect';
  opened[0].emit('open'); await flushMicrotasks();
  assert.equal(state.archiveDetailOpen(), true);
  assert.equal(state.selectedTicket.body, '# Updated after reconnect');
  assert.equal(location.hash, '#/archive/t/kon-history');
  opened[0].emit('ticket_deleted', { id: 'kon-history' });
  assert.equal(state.selectedTicket, null);
  assert.equal(location.hash, '#/archive');
  assert.match(state.error, /Historical ticket kon-history is missing/);
});

test('deleted history references report a missing ticket instead of opening a successful run', async () => {
  const { state, location } = archiveRouteUI('#/t/kon-deleted');
  state.selectedTicket = { id: 'other', source: 'team', status: 'open' };
  await state.applyRoute();
  assert.equal(state.selectedTicket, null);
  assert.match(state.error, /Historical ticket kon-deleted is missing/);
  assert.equal(location.hash, '#/');
});

test('a slow primary history lookup cannot replace a later navigation', async () => {
  const pending = deferred();
  const { state, location, archived } = archiveRouteUI('#/t/kon-history', url => url === '/api/tickets/kon-history' ? pending.promise : undefined);
  const lookup = state.applyRoute();
  location.hash = '#/t/kon-visible'; await state.applyRoute();
  pending.resolve({ ok: true, json: async () => archived }); await lookup;
  assert.equal(state.currentView, 'board');
  assert.equal(state.selectedTicket.id, 'kon-visible');
  assert.equal(location.hash, '#/t/kon-visible');
});

for (const resolveInArchive of [false, true]) {
  test(`Archive without an overlay preserves pending board detail resolved ${resolveInArchive ? 'in Archive' : 'after returning'}`, async () => {
    const detail = deferred();
    const { state } = archiveRouteUI('#/', url => url === '/api/tickets/kon-visible' ? detail.promise : undefined);
    const opening = state.selectTicket(state.tickets[0]);
    await flushMicrotasks();
    assert.equal(state.detailLoading, true);
    await state.gotoView('archive');
    assert.equal(state.archiveDetailOpen(), false);
    if (!resolveInArchive) await state.gotoView('board');
    detail.resolve({ ok: true, json: async () => ({ id: 'kon-visible', status: 'open', title: 'Loaded detail', body: '# Board body', history: [] }) });
    await opening;
    if (resolveInArchive) await state.gotoView('board');
    assert.equal(state.currentView, 'board');
    assert.equal(state.selectedTicket.id, 'kon-visible');
    assert.equal(state.selectedTicket.body, '# Board body');
    assert.equal(state.detailLoading, false);
    assert.equal(state.error, null);
  });
}

test('leaving Archive with its own pending detail still discards the late response', async () => {
  const detail = deferred();
  const { state, archived } = archiveRouteUI('#/archive/t/kon-history', url => url === '/api/tickets/kon-history' ? detail.promise : undefined);
  const opening = state.applyRoute();
  await flushMicrotasks();
  assert.equal(state.archiveDetailLoading, true);
  await state.gotoView('board');
  await state.selectTicket(state.tickets[0]);
  detail.resolve({ ok: true, json: async () => archived }); await opening;
  assert.equal(state.currentView, 'board');
  assert.equal(state.selectedTicket.id, 'kon-visible');
  assert.equal(state.archiveDetailLoading, false);
  assert.equal(state.detailLoading, false);
});

test('the archive route remains selected through its own hash event and ignores a late list after leaving', async () => {
  const detail = deferred(), list = deferred();
  const { state, location, archived } = archiveRouteUI('#/archive/t/kon-history', url =>
    url === '/api/tickets/kon-history' ? detail.promise : url === '/api/tickets/archived' ? list.promise : undefined);
  const opening = state.applyRoute();
  await flushMicrotasks();
  assert.equal(state.archiveDetailOpen(), true);
  await state.applyRoute();
  detail.resolve({ ok: true, json: async () => archived }); await opening;
  assert.equal(state.selectedTicket.body, '# Archived body');
  assert.equal(state.archiveDetailLoading, false);
  location.hash = '#/'; await state.applyRoute();
  list.resolve({ ok: true, json: async () => ({ tickets: [archived] }) }); await flushMicrotasks();
  assert.equal(state.archiveRows.length, 0);
  assert.equal(state.selectedTicket, null);
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
      name: "opening the mobile activity transcript",
      tab: "activity",
      status: "in_progress",
      check: (s, calls) => {
        assert.equal(s.detailTab, "activity");
        assert.deepEqual(calls, ["close", "activity:plan:0"]);
        assert.equal(s.terminalWanted(), false);
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
    state.fetchActivity = (stage, run) => calls.push(`activity:${stage}:${run}`);

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

  assert.deepEqual(Array.from(segs, (s) => s.name), ["plan", "review", "commit"]);
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

test("cost formatting distinguishes zero, small values and unknown estimates", () => {
  const state = pageState(TAPE_TICKET);

  assert.equal(state.formatCostUSD(null), "");
  assert.equal(state.formatCostUSD("0.000000"), "$0.00");
  assert.equal(state.formatCostUSD("0.000001"), "<$0.0001");
  assert.equal(state.formatCostUSD("0.001234"), "$0.0012");
  assert.equal(state.formatCostUSD("0.009999"), "$0.0100");
  assert.equal(state.formatCostUSD("1.005000"), "$1.01");
  assert.equal(state.formatCostUSD("2.675000"), "$2.68");
  assert.equal(state.formatCostUSD("1.239999"), "$1.24");
  assert.equal(state.formatCostUSD("10000000000000.000000"), "$10000000000000.00");
  assert.equal(state.formatCostUSD("invalid"), "");
  assert.equal(state.costLabel({ cost_usd: "0.420000", priced_runs: 1, tracked_runs: 2 }), "$0.42+");
  assert.equal(state.costLabel({ cost_usd: null, priced_runs: 0, tracked_runs: 1 }), "unknown");
  assert.equal(state.costLabel({ cost_usd: null }), "", "an unpriced History row shows no value");
  assert.equal(
    state.costTooltip({ cost_usd: "0.000000499" }),
    "Estimated using the bundled snapshot of standard model rates",
    "a run value is still identified as an estimate",
  );
  assert.equal(
    state.costTooltip({ cost_usd: "0.420000", priced_runs: 1, tracked_runs: 2 }),
    "Known subtotal using the bundled snapshot of standard model rates · 1 of 2 runs priced",
  );
  assert.equal(
    state.costTooltip({ cost_usd: null, priced_runs: 0, tracked_runs: 2 }),
    "No estimate using the bundled snapshot of standard model rates · 0 of 2 runs priced",
  );
});

test("cost helpers reject state from another selected ticket", () => {
  const state = pageState({ id: "kon-current", status: "done", stages: ["code"], history: [{ stage: "code" }] });
  state.ticketCost = {
    id: "kon-stale", cost_usd: "9.000000", priced_runs: 1, tracked_runs: 1,
    stages: [{ name: "code", cost_usd: "9.000000", priced_runs: 1, tracked_runs: 1 }],
    runs: [{ history_index: 0, cost_usd: "9.000000" }],
  };

  assert.equal(state.currentTicketCost(), null);
  assert.equal(state.ticketStageCost("code"), null);
  assert.equal(state.ticketRunCost(0), null);
  assert.deepEqual(Array.from(state.ribbonMeters(), (meter) => meter.k), []);
});

test("the stage ribbon includes historical and synthetic cost stages", () => {
  const state = pageState({
    id: "kon-cost", status: "done", stages: ["plan"],
    history: [{ stage: "retired", run: 0, started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:10Z" }],
  });
  state.ticketCost = {
    id: "kon-cost", cost_usd: "0.300000", priced_runs: 2, tracked_runs: 2,
    stages: [
      { name: "retired", cost_usd: "0.200000", priced_runs: 1, tracked_runs: 1 },
      { name: "default", cost_usd: "0.100000", priced_runs: 1, tracked_runs: 1 },
    ],
  };

  const segs = state.stageRibbon();
  assert.deepEqual(Array.from(segs, (seg) => seg.name), ["plan", "retired", "default"]);
  assert.equal(segs[1].runs, 1);
  assert.equal(state.costLabel(segs[1].cost), "$0.20");
  assert.equal(segs[2].runs, 0, "the synthetic default run does not invent History");
  assert.equal(segs[2].state, "done");
});

test("stage cost coverage includes retries, annotations and unpriced runs", () => {
  const state = pageState({
    id: "kon-stage-cost", status: "done", stages: ["code"],
    history: [
      { stage: "code", run: 0 },
      { stage: "code", run: 0, kind: "annotation" },
      { stage: "code", run: 0 },
    ],
  });
  state.ticketCost = {
    id: "kon-stage-cost", cost_usd: "0.100000", priced_runs: 2, tracked_runs: 3,
    stages: [{ name: "code", cost_usd: "0.100000", priced_runs: 2, tracked_runs: 3 }],
  };

  const seg = state.stageRibbon()[0];
  assert.equal(seg.runs, 3);
  assert.equal(state.costLabel(seg.cost), "$0.10+");
  assert.equal(state.ribbonMeters()[0].v, "$0.10+");
  assert.match(state.ribbonMeters()[0].title, /2 of 3 runs priced/);
});

test("run estimates use the History index instead of legacy run numbers", () => {
  const state = pageState({
    id: "kon-cost", status: "done", stages: ["code"],
    history: [
      { stage: "code", run: 0, summary: "first" },
      { stage: "code", run: 0, kind: "annotation", summary: "annotation" },
    ],
  });
  state.ticketCost = {
    id: "kon-cost",
    runs: [
      { history_index: 0, cost_usd: "0.100000", priced_runs: 1, tracked_runs: 1 },
      { history_index: 1, cost_usd: "0.200000", priced_runs: 1, tracked_runs: 1 },
    ],
  };

  assert.equal(state.costLabel(state.ticketRunCost(0)), "$0.10");
  assert.equal(state.costLabel(state.ticketRunCost(1)), "$0.20");
  const cards = state.summaryCards();
  assert.equal(cards[0].historyIndex, 1);
  assert.match(state.stageCardMeta(cards[0]), /\$0\.20/);
});

for (const [ticket, label] of [
  [{ pipeline: 'build-And-Test', path: '/repo' }, 'build-And-Test'],
  [{ path: '/projects/Repo-Name' }, 'Repo-Name'],
  [{}, '—'],
  [null, '—'],
]) {
  test(`mobile pipeline metadata preserves source case: ${label}`, () => {
    assert.equal(loadKontoraState().mobileTagLabel(ticket), label);
  });
}

test("mobile output tab labels are plain without changing their targets", () => {
  const state = pageState({ id: 'kon-mobile', status: 'in_progress' });
  for (const [status, tab, label] of [
    ['in_progress', 'terminal', 'Terminal'],
    ['paused', 'logs', 'Logs'],
    ['done', 'logs', 'Logs'],
    ['todo', 'logs', 'Logs'],
  ]) {
    state.selectedTicket.status = status;
    assert.equal(state.mobileFirstTab(), tab, status);
    assert.equal(state.mobileFirstTabLabel(), label, status);
  }
  state.selectedTicket = null;
  assert.equal(state.mobileFirstTab(), 'logs');
  assert.equal(state.mobileFirstTabLabel(), 'Logs');
});

for (const [status, name, segmentState, token, fill, label] of [
  ['in_progress', 'code', 'running', '--st-progress', '50%', 'Running'],
  ['in_progress', 'retired', 'done', '--st-done', '100%', 'Recorded or earlier stage'],
  ['in_progress', 'cost-only', 'done', '--st-done', '100%', 'Recorded or earlier stage'],
  ['in_progress', 'test', 'queued', '--surface-600', '0%', 'Not started'],
  ['paused', 'code', 'done', '--st-paused', '50%', 'paused'],
  ['human_review', 'code', 'done', '--st-review', '50%', 'human review'],
]) {
  test(`mobile ribbon segment styling: ${status} ${name}`, () => {
    const state = pageState({ id: 'kon-mobile', status, stage: 'code', stages: ['plan', 'code', 'test'] });
    const seg = { name, state: segmentState };
    const color = token === '--surface-600' ? `rgb(var(${token}))` : `hsl(var(${token}))`;
    const foreground = segmentState === 'queued' ? 'rgb(var(--tx-2))' : color;
    assert.equal(state.mobileStageStyle(seg), `color:${foreground};--mobile-stage-color:${color};--mobile-stage-fill:${fill};`);
    assert.equal(state.mobileStageState(seg), label);
    state.logViewStage = name;
    assert.equal(state.mobileStageStyle(seg), `color:${foreground};--mobile-stage-color:${color};--mobile-stage-fill:${fill};`, 'viewed log does not change execution color');
  });
}

for (const [name, history, cost, expected, current = 'code'] of [
  ['plan', [], null, 'Recorded or earlier stage'],
  ['plan', [], null, 'Ticket marked done; no recorded run', 'removed'],
  ['code', [], null, 'Ticket marked done; no recorded run'],
  ['test', [], null, 'Ticket marked done; no recorded run'],
  ['retired', [], null, 'Ticket marked done; no recorded run'],
  ['test', [{ stage: 'test' }], null, 'Recorded or earlier stage'],
  ['historical', [{ stage: 'historical' }], null, 'Recorded or earlier stage'],
  ['default', [], { name: 'default', tracked_runs: 1 }, 'Recorded or earlier stage'],
]) {
  test(`mobile done-stage evidence: ${name}, current ${current}, history ${history.length}, cost ${!!cost}`, () => {
    const state = pageState({ id: 'kon-done', status: 'done', stage: current, stages: ['plan', 'code', 'test', 'retired'], history });
    state.ticketCost = { id: 'kon-done', stages: cost ? [cost] : [] };
    const seg = state.stageRibbon().find(seg => seg.name === name);
    assert.equal(state.mobileStageState(seg), expected);
  });
}

for (const [history, tracked, status, expected] of [
  [[], 1, 'done', '1 tracked run · duration unavailable'],
  [[], 2, 'done', '2 tracked runs · duration unavailable'],
  [[], 1, 'in_progress', '1 tracked run · 0s'],
  [[{ stage: 'code', started_at: '2026-08-08T11:00:00Z', completed_at: '2026-08-08T11:00:10Z' }], 1, 'done', '10s · 1 runs'],
  [[], 0, 'done', '0s · 0 runs'],
]) {
  test(`mobile stage metadata: history ${history.length}, tracked ${tracked}, ${status}`, () => {
    const state = pageState({ id: 'kon-cost', status, stage: 'code', stages: ['code'], history });
    state.ticketCost = { id: 'kon-cost', stages: [{ name: 'code', tracked_runs: tracked }] };
    const seg = state.stageRibbon()[0];
    assert.equal(state.mobileStageMeta(seg), expected);
    assert.equal(seg.runs, history.length, 'presentation must not change History-based navigation');
  });
}

test('mobile progress strip is noninteractive and hidden with secondary metadata', () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const mobile = html.slice(html.indexOf('<div x-show="isMobile && selectedTicket'));
  const strip = mobile.split('aria-label="Pipeline stage progress"')[1].split('</div>\n          </template>')[0];
  assert.match(strip, /x-for="seg in stageRibbon\(\)"/);
  assert.doesNotMatch(strip, /<button|@click/);
  assert.match(mobile, /!isSecondary\(selectedTicket\) && !epicIsAgent\(selectedTicket\) && detailTab !== 'logs'/);
  assert.match(mobile, /:aria-current="logViewStage === seg.name \? 'step' : null"/);
  assert.match(mobile, /@click="clickStage\(seg.name\)"/);
});

test("mobile schedule text keeps absolute and relative times with plain punctuation", () => {
  const state = pageState({ id: 'kon-mobile', status: 'open' });
  for (const [chip, expected] of [
    [null, ''],
    [{ abs: 'Fri 09:00', rel: '' }, 'starts Fri 09:00'],
    [{ abs: 'Fri 09:00', rel: '18h' }, 'starts Fri 09:00, in 18h'],
  ]) {
    state.scheduleChip = ticket => {
      assert.equal(ticket, state.selectedTicket);
      return chip;
    };
    assert.equal(state.mobileScheduleCurrent(state.selectedTicket), expected);
  }
});

test("mobile costs remain visible for running, retried and unpriced tickets", () => {
  const state = pageState({
    id: "kon-mobile", status: "in_progress", stage: "code", stages: ["plan", "code"],
    history: [{ stage: "plan", run: 0 }],
  });
  state.ticketCost = {
    id: "kon-mobile", cost_usd: "0.100000", priced_runs: 1, tracked_runs: 2,
    stages: [
      { name: "plan", cost_usd: "0.100000", priced_runs: 1, tracked_runs: 1 },
      { name: "retired", cost_usd: null, priced_runs: 0, tracked_runs: 1 },
    ],
  };

  assert.equal(state.mobileHasRun(state.selectedTicket), true);
  assert.equal(state.mobileTicketCostLabel(), "est. $0.10+ (1/2 runs priced)");
  assert.deepEqual(Array.from(state.stageRibbon(), (seg) => state.mobileStageLabel(seg)),
    ["plan: $0.10", "code", "retired: unknown (0/1 runs priced)"]);

  state.selectedTicket.status = "todo";
  assert.equal(state.mobileHasRun(state.selectedTicket), true, "a retried ticket keeps its run tabs");
});

test("mobile stage strip preserves stage navigation with and without costs", () => {
  for (const row of [
    { stage: "plan", tab: "logs", open: false, logs: [["kon-mobile", "plan"]] },
    { stage: "retired", tab: "logs", open: false, logs: [["kon-mobile", "retired"]] },
    { stage: "code", tab: "terminal", open: true, logs: [] },
    { stage: "test", tab: "logs", open: false, logs: [] },
  ]) {
    const state = pageState({
      id: "kon-mobile", status: "in_progress", stage: "code", stages: ["plan", "code", "test"],
      history: [{ stage: "plan", run: 0 }, { stage: "retired", run: 0 }],
    });
    state.ticketCost = { id: "kon-mobile", stages: [{ name: "plan", cost_usd: "0.10", priced_runs: 1, tracked_runs: 1 }] };
    const calls = [];
    state.detailTab = "logs";
    state.terminalOpen = false;
    state.openTerminal = () => { state.terminalOpen = true; };
    state.closeTerminal = () => { state.terminalOpen = false; };
    state.fetchStageLogs = (id, stage) => calls.push([id, stage]);

    state.clickStage(row.stage);

    assert.equal(state.detailTab, row.tab, row.stage);
    assert.equal(state.terminalOpen, row.open, row.stage);
    assert.deepEqual(calls, row.logs, row.stage);
  }
});

test("normal and archive detail open paths request ticket cost", async () => {
  for (const archive of [false, true]) {
    const state = loadKontoraState();
    const requested = [];
    state.fetchTicketCost = (id) => requested.push(id);
    if (archive) {
      state.currentView = 'archive';
      await state.archiveOpenRow({ id: "kon-archive", title: "Archived", path: "/repo" });
      assert.deepEqual(requested, ["kon-archive"]);
    } else {
      await state.selectTicket({ id: "kon-board", title: "Board", status: "done" });
      assert.deepEqual(requested, ["kon-board"]);
    }
  }
});

test("overlapping detail requests keep the newest selected ticket", async () => {
  const first = deferred();
  const second = deferred();
  var calls = 0;
  const state = loadKontoraState({ fetch: () => (++calls === 1 ? first.promise : second.promise) });
  state.fetchTicketCost = () => {};

  const oldRequest = state.selectTicket({ id: "kon-first", title: "First", status: "done" });
  const newRequest = state.selectTicket({ id: "kon-second", title: "Second", status: "done" });
  second.resolve({ ok: true, json: async () => ({ id: "kon-second", title: "Second full", status: "done" }) });
  await newRequest;
  first.resolve({ ok: true, json: async () => ({ id: "kon-first", title: "First full", status: "done" }) });
  await oldRequest;

  assert.equal(state.selectedTicket.id, "kon-second");
  assert.equal(state.selectedTicket.title, "Second full");
});

test("same-ticket SSE updates outrank older detail responses", async () => {
  for (const archive of [false, true]) {
    const first = deferred();
    const second = deferred();
    const secondStarted = deferred();
    var calls = 0;
    const state = loadKontoraState({
      fetch: () => {
        calls++;
        if (calls === 2) secondStarted.resolve();
        return calls === 1 ? first.promise : second.promise;
      },
    });
    state.fetchTicketCost = () => {};
    state.fetchChain = () => {};
    state.fetchChanges = () => {};
    state.fetchActivity = () => {};
    if (archive) state.currentView = "archive";

    const row = { id: "kon-current", title: "Initial", status: archive ? "archived" : "in_progress", history: [] };
    const request = archive ? state.archiveOpenRow(row) : state.selectTicket(row);
    state.applyTicketUpdate({
      ...row,
      title: "New event",
      status: archive ? "archived" : "human_review",
      history: [{ stage: "plan" }, { stage: "code" }],
      kontora: true,
    });
    first.resolve({ ok: true, json: async () => ({ ...row, title: "Old detail", body: "# Old" }) });
    await secondStarted.promise;
    second.resolve({
      ok: true,
      json: async () => ({
        ...row,
        title: "New detail",
        status: archive ? "archived" : "human_review",
        history: [{ stage: "plan" }, { stage: "code" }],
        body: "# Current",
      }),
    });
    await request;

    assert.equal(state.selectedTicket.title, "New detail", archive ? "archive" : "normal");
    assert.equal(state.selectedTicket.history.length, 2, archive ? "archive" : "normal");
    assert.equal(state.selectedTicket.body, "# Current", archive ? "archive" : "normal");
  }
});

test("an archived detail response cannot cross a close and reopen", async () => {
  const first = deferred();
  const second = deferred();
  var calls = 0;
  const state = loadKontoraState({ fetch: () => (++calls === 1 ? first.promise : second.promise) });
  state.fetchTicketCost = () => {};
  const row = { id: "kon-archive", title: "Archive", status: "done" };
  state.currentView = 'archive';

  const oldRequest = state.archiveOpenRow(row);
  state.archiveCloseDetail();
  const newRequest = state.archiveOpenRow(row);
  second.resolve({ ok: true, json: async () => ({ id: row.id, title: "New detail", status: "archived" }) });
  await newRequest;
  first.resolve({ ok: true, json: async () => ({ id: row.id, title: "Old detail", status: "archived" }) });
  await oldRequest;

  assert.equal(state.selectedTicket.id, row.id);
  assert.equal(state.selectedTicket.title, "New detail");
});

test("ticket cost requests reject older responses for the same ticket", async () => {
  const first = deferred();
  const second = deferred();
  var calls = 0;
  const state = loadKontoraState({ fetch: () => (++calls === 1 ? first.promise : second.promise) });
  state.selectedTicket = { id: "kon-cost" };

  const oldRequest = state.fetchTicketCost("kon-cost");
  const newRequest = state.fetchTicketCost("kon-cost");
  second.resolve({ ok: true, json: async () => ({ id: "kon-cost", cost_usd: "2.000000" }) });
  await newRequest;
  first.resolve({ ok: true, json: async () => ({ id: "kon-cost", cost_usd: "1.000000" }) });
  await oldRequest;

  assert.equal(state.ticketCost.cost_usd, "2.000000");
  state.closeTerminal = () => {};
  state.flushEditSave = () => {};
  state.closeDetail();
  assert.equal(state.ticketCost, null);
});

test("a failed ticket cost refresh clears stale coverage", async () => {
  const cases = [
    { name: "non-OK response", fetch: async () => ({ ok: false }) },
    { name: "request error", fetch: async () => { throw new Error("offline"); } },
  ];
  for (const tc of cases) {
    const state = loadKontoraState({ fetch: tc.fetch });
    state.selectedTicket = { id: "kon-cost" };
    state.ticketCost = { id: "kon-cost", cost_usd: "1.000000", priced_runs: 1, tracked_runs: 1 };

    await state.fetchTicketCost("kon-cost");

    assert.equal(state.ticketCost, null, tc.name);
  }
});

test("ticket cost requests cannot cross ticket or archive close boundaries", async () => {
  const first = deferred();
  const second = deferred();
  var calls = 0;
  const state = loadKontoraState({ fetch: () => (++calls === 1 ? first.promise : second.promise) });
  state.selectedTicket = { id: "kon-first" };
  const oldRequest = state.fetchTicketCost("kon-first");
  state.selectedTicket = { id: "kon-second" };
  const newRequest = state.fetchTicketCost("kon-second");

  first.resolve({ ok: true, json: async () => ({ id: "kon-first", cost_usd: "1.000000" }) });
  await oldRequest;
  assert.equal(state.ticketCost, null);

  state._archiveClearDetail();
  second.resolve({ ok: true, json: async () => ({ id: "kon-second", cost_usd: "2.000000" }) });
  await newRequest;
  assert.equal(state.ticketCost, null);
});

test("archive meters include complete and measured-zero estimates", () => {
  const state = pageState({ id: "kon-archive", status: "archived", stages: [] });
  state.ticketCost = { id: "kon-archive", cost_usd: "0.000000", priced_runs: 1, tracked_runs: 1, stages: [] };

  const estimate = state.archiveMeters().find((meter) => meter.k === "estimate");
  assert.equal(estimate.v, "$0.00");
  assert.match(estimate.title, /Estimated using the bundled snapshot of standard model rates/);
});

test("relevant SSE changes refetch cost once per batch", () => {
  const cases = [
    {
      name: "History changed",
      initial: { id: "kon-cost", status: "todo", history: [] },
      update: { id: "kon-cost", status: "todo", history: [{ stage: "plan", run: 0 }] },
    },
    {
      name: "standalone status changed without History",
      initial: { id: "kon-cost", status: "in_progress", history: [] },
      update: { id: "kon-cost", status: "done", history: [] },
    },
  ];

  for (const tc of cases) {
    const state = loadKontoraState();
    state.selectedTicket = { ...tc.initial };
    state.tickets = [{ ...tc.initial }];
    state.updateFavicon = () => {};
    const fetched = [];
    state.fetchTicketCost = (id) => fetched.push(id);
    state._boardRaf = 1;
    state.queueTicketUpdate(tc.update);
    state.queueTicketUpdate({ id: "other", status: "todo", history: [] });
    state.flushTicketUpdates();

    assert.deepEqual(fetched, ["kon-cost"], tc.name);
  }
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
  // Desktop and phone ticket activity plus both embedded epic layouts.
  assert.equal((html.match(/toolExpanded\(ev, idx\)/g) || []).length, 8);
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
  assert.equal((html.match(/Waiting for the agent's first step/g) || []).length, 4);
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

test("phone activity fetches its target, polls, and re-arms the cached live transcript", async () => {
  const h = activityHarness([
    [200, livePayload([tapeEvent(0)]), '"a"'],
    [200, livePayload([tapeEvent(1)], { offset: 1 }), '"b"'],
    [200, livePayload([tapeEvent(2)], { offset: 2 }), '"c"'],
  ]);
  Object.assign(h.state, { isMobile: true, activeTab: 'ticket', detailTab: 'terminal', terminalOpen: true });
  let closed = 0;
  h.state.closeTerminal = () => { closed++; h.state.terminalOpen = false; };

  h.state.mobileSwitchTab('activity');
  await flushMicrotasks();
  assert.equal(closed, 1);
  assert.equal(h.state.activeTab, 'ticket');
  assert.equal(h.state.outputContainerID('activity'), 'activity-scroll');
  assert.match(h.calls[0].url, /\/api\/tickets\/kon-1\/activity\?stage=commit&run=0$/);
  assert.equal(h.pending(), 1);
  assert.equal(h.delay(), 2000);
  await h.tick();
  assert.deepEqual(h.state.tapeEvents().map(e => e.text), ['line 0', 'line 1']);

  h.state.mobileSwitchTab('ticket');
  assert.equal(h.pending(), 0, 'leaving the phone pane stops the poll');
  h.state.mobileSwitchTab('activity');
  assert.equal(h.calls.length, 2, 'returning to a cached live tape does not replace it');
  assert.equal(h.pending(), 1);
  await h.tick();
  assert.deepEqual(h.state.tapeEvents().map(e => e.text), ['line 0', 'line 1', 'line 2']);
  h.state.mobileSwitchTab('ticket');
});

for (const hiddenTab of ['ticket', 'terminal']) for (const live of [false, true]) {
  for (const advance of ['stage', 'attempt']) for (const explicit of [false, true]) {
    test(`phone activity resumes the correct target after hidden ${advance}: tab=${hiddenTab}, live=${live}, explicit=${explicit}`, async () => {
      const initial = { ...RUNNING_TICKET, stage: 'plan', history: [], started_at: '2026-08-08T11:00:00Z' };
      const h = activityHarness([
        [200, livePayload([tapeEvent(0)], { live })],
        [200, livePayload([tapeEvent(1)])],
      ], initial);
      Object.assign(h.state, { isMobile: true, activeTab: 'ticket', detailTab: 'ticket', tickets: [initial] });
      h.state.openTerminal = () => {};
      h.state.mobileSwitchTab('activity');
      await flushMicrotasks();
      h.state._activityExplicit = explicit;
      h.state.mobileSwitchTab(hiddenTab);
      h.state.applyTicketUpdate({ ...initial, stage: advance === 'stage' ? 'code' : 'plan',
        started_at: '2026-08-08T11:01:00Z', history: [{ stage: 'plan', run: 0, exit_code: 0 }] });
      assert.equal(h.calls.length, 1, 'hidden activity does not fetch');

      h.state.mobileSwitchTab('activity');
      await flushMicrotasks();
      if (explicit) {
        assert.equal(h.calls.length, 1, 'intentional historical selection survives');
        assert.equal(h.state.activityStage, 'plan');
        assert.equal(h.state.activityRun, 0);
        assert.equal(h.pending(), live ? 1 : 0);
      } else {
        assert.equal(h.calls.length, 2, 'automatic selection refreshes a stale cached transcript');
        assert.match(h.calls[1].url, advance === 'stage' ? /activity\?stage=code&run=0$/ : /activity\?stage=plan&run=1$/);
        assert.deepEqual(h.state.tapeEvents().map(e => e.text), ['line 1']);
        assert.equal(h.pending(), 1);
      }
      assert.equal(h.state.activeTab, 'ticket');
      h.state.mobileSwitchTab('ticket');
    });
  }
}

for (const initialTab of ['activity', 'terminal']) {
  test(`phone ${initialTab} shows the completed transcript when the run ends`, async () => {
    const h = activityHarness([
      [200, livePayload([tapeEvent(0)]), '"a"'],
      [200, livePayload([tapeEvent(0), tapeEvent(1)], { live: false }), '"done"'],
    ]);
    Object.assign(h.state, { isMobile: true, activeTab: 'ticket', detailTab: 'activity', tickets: [RUNNING_TICKET] });
    h.state.mobileSwitchTab('activity');
    await flushMicrotasks();
    if (initialTab === 'terminal') {
      h.state.openTerminal = () => {};
      h.state.mobileSwitchTab('terminal');
    }

    h.state.applyTicketUpdate({ ...RUNNING_TICKET, status: 'human_review',
      history: [...RUNNING_TICKET.history, { stage: 'commit', run: 0, exit_code: 0 }] });
    await flushMicrotasks();

    assert.equal(h.state.detailTab, 'activity');
    assert.equal(h.state.activeTab, 'ticket');
    assert.deepEqual(h.state.tapeEvents().map(e => e.text), ['line 0', 'line 1']);
    assert.equal(h.state.activity.live, false);
    assert.equal(h.calls.length, 2, 'completion reads the final transcript once');
    assert.equal(h.pending(), 0);
  });
}

test("phone activity on a finished ticket opens the last completed run without polling", async () => {
  const h = activityHarness([[200, { source: 'log', content: 'finished' }]], TAPE_TICKET);
  Object.assign(h.state, { isMobile: true, activeTab: 'summary' });
  h.state.mobileSwitchTab('activity');
  await flushMicrotasks();
  assert.match(h.calls[0].url, /activity\?stage=review&run=1$/);
  assert.equal(h.state.activity.content, 'finished');
  assert.equal(h.state.activeTab, 'summary');
  assert.equal(h.pending(), 0);
});

test("activity survives a breakpoint round trip and refreshes in both layouts", () => {
  const state = pageState(RUNNING_TICKET);
  const asked = [];
  state.activeTab = 'activity';
  state.activityStage = 'commit';
  state.activityRun = 0;
  state.closeTerminal = () => {};
  state._destroyTerminal = () => {};
  state._mountBoard = () => {};
  state.fetchActivity = (stage, run) => asked.push([stage, run]);
  state.$nextTick = fn => fn();

  state.isMobile = true;
  state._onBreakpointChange();
  assert.equal(state.detailTab, 'activity');
  assert.equal(state.outputTab(), 'activity');
  state.isMobile = false;
  state._onBreakpointChange();
  assert.equal(state.activeTab, 'activity');
  assert.deepEqual(asked, [['commit', 0], ['commit', 0]]);
});

test("phone activity markup has a follow toggle, shared scroller, retry and no run picker", () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const pane = html.slice(html.indexOf('<div x-show="!isSecondary(selectedTicket) && detailTab===\'activity\'"')).split('<!-- logs (stage history) -->')[0];
  assert.match(pane, /detailTab==='activity'/);
  assert.match(pane, /id="activity-scroll"/);
  assert.match(pane, /@click="toggleLogFollow\(\)" :aria-pressed="logFollow"/);
  assert.match(pane, /@click="fetchActivity\(activityStage, activityRun\)"/);
  assert.match(pane, /x-for="\{ ev, idx \} in visibleTapeEvents\(\)"/);
  assert.match(pane, /x-html="renderLogHTML\(activity.content\)"/);
  assert.doesNotMatch(pane, /<select|selectOutputRun|openActivity/);
  assert.equal((html.match(/id="activity-scroll"/g) || []).length, 2);

  const tabs = html.split('<template x-if="!isSecondary(selectedTicket) && mobileHasRun(selectedTicket)">')[1].split('</template>')[0];
  assert.match(tabs, /class="mobile-detail-tabs flex overflow-x-auto"/);
  assert.equal((tabs.match(/class="mobile-tab"/g) || []).length, 4);
  assert.match(tabs, /@click="mobileSwitchTab\('activity'\)" :aria-pressed="detailTab === 'activity'"/);
  assert.match(tabs, /@click="mobileSwitchTab\(mobileFirstTab\(\)\)" :aria-pressed="detailTab === mobileFirstTab\(\)"[\s\S]*?x-text="mobileFirstTabLabel\(\)"/);
  for (const [tab, label] of [['logs', 'Logs'], ['activity', 'Activity'], ['ticket', 'Ticket']]) {
    const button = [...tabs.matchAll(/<button\b[^>]*>([\s\S]*?)<\/button>/g)]
      .find(([markup]) => markup.includes(`@click="mobileSwitchTab('${tab}')"`));
    assert.ok(button, tab + ' remains reachable');
    assert.equal(button[1].trim(), label);
    assert.ok(button[0].includes(`:aria-pressed="detailTab === '${tab}'"`));
    if (tab === 'logs') assert.match(button[0], /x-show="selectedTicket\?\.status === 'in_progress'"/);
  }
  assert.doesNotMatch(tabs, /text-transform:uppercase|letter-spacing:|JetBrains Mono/);
});

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

  // The run has ended in the ticket state as well as in its final payload.
  // A still-running ticket must retry even if a startup payload is not live.
  h.state.selectedTicket = { ...RUNNING_TICKET, status: 'done' };
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
    { name: "a secondary holder cannot run here", status: "open", source: "other-folder", want: null },
  ];

  for (const c of cases) {
    const chain = { ...CHAIN_PAYLOAD, nodes: CHAIN_PAYLOAD.nodes.map((n) => (n.holds_chain ? { ...n, status: c.status, source: c.source } : n)) };
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
    { name: "a secondary namesake is unrelated", pending: [{ id: "pca-qv0y", source: "other-folder", status: "done" }], want: false },
    { name: "secondary relations do not add primary dependents", pending: [{ id: "other", source: "other-folder", deps: [{ id: "pca-d3q9" }] }], want: false },
    {
      name: "an external prerequisite moved",
      chain: { ...CHAIN_PAYLOAD, nodes: CHAIN_PAYLOAD.nodes.map(n => n.id === "pca-qv0y" ? { ...n, source: "other-folder" } : n) },
      pending: [{ id: "pca-qv0y", source: "other-folder", status: "done" }], want: true,
    },
  ];

  for (const c of cases) {
    const state = chainState(c.chain || CHAIN_PAYLOAD);
    assert.equal(state._chainTouchedBy(c.pending), c.want, c.name);
    state._boardRaf = 1;
    state.fetchTicketCost = () => {};
    state.updateFavicon = () => {};
    const fetched = [];
    state.fetchChain = id => fetched.push(id);
    for (const ticket of c.pending) state.queueTicketUpdate(ticket);
    state.flushTicketUpdates();
    assert.deepEqual(fetched, c.want ? ['pca-d3q9'] : [], c.name + ': queued updates');
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

test("the chain opens expanded and refetches preserve the user's choice", async () => {
  const cases = [
    { name: "a blocked six-node chain opens expanded", chain: CHAIN_PAYLOAD },
    { name: "a ready chain opens expanded", chain: { ...CHAIN_PAYLOAD, verdict: "ready" } },
    { name: "a two-node chain opens expanded", chain: { ...CHAIN_PAYLOAD, total: 2 } },
  ];

  for (const c of cases) {
    const state = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => c.chain }) });
    state.selectedTicket = { id: "pca-d3q9", status: "todo" };
    await state.fetchChain("pca-d3q9");
    assert.equal(state.chainCollapsed, false, c.name);
    assert.equal(state.chainError, null, c.name);
  }

  // An SSE refetch must not undo the user's own expand or collapse.
  const kept = loadKontoraState({ fetch: async () => ({ ok: true, json: async () => ({ ...CHAIN_PAYLOAD, verdict: "ready" }) }) });
  kept.selectedTicket = { id: "pca-d3q9", status: "todo" };
  for (const collapsed of [true, false]) {
    kept.chainCollapsed = collapsed;
    await kept.fetchChain("pca-d3q9");
    assert.equal(kept.chainCollapsed, collapsed, "a refetch leaves the ladder as the user left it");
  }

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

  const external = { id: "kon-wait", source: "other-folder", title: "Completed elsewhere", status: "done" };
  state.secondaryTickets = [external];
  await state.openTicketRef(external);
  assert.equal(opened, external);
  await state.openTicketRef({ id: "hidden-external", source: "other-folder", title: "Archived elsewhere", status: "archived" });
  assert.deepEqual({ ...opened }, { id: "hidden-external", source: "other-folder", title: "Archived elsewhere", status: "archived" });

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
  assert.match(html, /x-show="ticketCanControl\(selectedTicket\) && row\.holds_chain && chainResumeMove\(\)"/);
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
  // Desktop ribbons and History lists use costLabel; mobile labels also show pricing coverage.
  assert.equal((html.match(/costLabel\(seg\.cost\)/g) || []).length, 4);
  assert.equal((html.match(/costLabel\(ticketRunCost\(hi\)\)/g) || []).length, 4);
  assert.match(html, /x-text="mobileTicketCostLabel\(\)"/);
  assert.match(html, /aria-label="Stages and cost estimates"/);
  assert.match(html, /x-text="mobileStageLabel\(seg\)"/);
  assert.doesNotMatch(html, /mobileCostStages\(\)|mobileOpenCostStage\(/);
  assert.match(html, /x-for="\(seg, si\) in stageRibbon\(\)" :key="seg\.name"/);
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
    ["plan · unknown/unverified", "indigo"],
    ["retried · stage review", "amber"],
    ["review · unknown/unverified", "indigo"],
    ["moved to human review", "mauve"],
  ]);
  assert.equal(rows[1].meta, "runner exit 0 · 40s");
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

test("a note mutation refreshes the snapshot without replacing the editor draft", () => {
  const state = pageState(NOTES_TICKET, { tickets: [{ id: "kon-1" }], recomputeBoard: () => {} });
  const full = { ...NOTES_TICKET, title: "renamed", notes: [{ id: "z9z9", text: "new" }] };

  state._noteApplyTicket("kon-1", full);
  assert.equal(state.selectedTicket.title, "renamed");

  state.editing = true;
  state.editForm.body = 'being typed';
  state._noteApplyTicket("kon-1", full);
  assert.equal(state.selectedTicket.title, 'renamed');
  assert.equal(state.editForm.body, 'being typed');
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

// The pipeline was repointed after the run, so review comes from History
// rather than the current stage list.
const DROPPED_STAGE_TICKET = { ...RETRY_TICKET, id: "kon-s5", stages: ["implement"] };

test("semantic outcomes stay separate from runner exits in summary and lifecycle", () => {
  const cases = [
    { outcome: "complete", label: "complete", hue: "--st-done", color: "green", cls: "text-ok" },
    { outcome: "blocked", label: "blocked", hue: "--st-paused", color: "amber", cls: "text-tx-faint" },
    { outcome: "protocol_error", label: "protocol_error", hue: "--st-error", color: "rose", cls: "text-st-cancel" },
    { outcome: "failed", label: "failed", hue: "--st-error", color: "rose", cls: "text-st-cancel" },
    { outcome: undefined, label: "unknown/unverified", hue: "--st-open", color: "indigo", cls: "text-tx-faint" },
  ];
  for (const c of cases) {
    for (const exit of [0, 7]) {
      const h = { stage: "default", summary: "Run summary", run_outcome: c.outcome,
        exit_code: exit, outcome_reason: "reason text", invocation_id: "inv-1",
        started_at: "2026-08-08T11:00:00Z", completed_at: "2026-08-08T11:00:40Z" };
      const state = pageState({ id: "kon-outcome", status: "done", history: [h], stages: ["default"] });
      const card = state.summaryCards()[0];
      assert.equal(state.runOutcomeLabel(h), c.label);
      assert.equal(state.runOutcomeClass(h), c.cls);
      assert.equal(state.stageHue(card, 0), c.hue);
      assert.match(state.stageCardMeta(card), new RegExp("outcome: " + c.label + " · runner exit " + exit + " · reason text · invocation inv-1"));
      const end = state.noteLifecycleRows().find((row) => row.key === "e:0:end");
      assert.equal(end.label, "default · " + c.label);
      assert.equal(end.color, c.color);
      assert.equal(end.meta, "runner exit " + exit + " · reason text · invocation inv-1 · 40s");
    }
  }
  const html = fs.readFileSync(htmlPath, "utf8");
  assert.equal((html.match(/x-text="runOutcomeLabel\(h\)"/g) || []).length, 2, "live and archive history both show semantics");
  assert.doesNotMatch(html, /h\.exit_code === 0 \? '✓'/);
});

test("summaryCards builds one card per summarised run", () => {
  const cases = [
    {
      name: "finished four-stage pipeline",
      ticket: SUMMARY_TICKET,
      keys: ["commit#0", "fix-review#0", "review#0", "implement#0"],
      hues: ["--st-open", "--st-paused", "--st-review", "--st-open"],
    },
    {
      name: "stage that ran twice",
      ticket: RETRY_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
      hues: ["--st-review", "--st-review", "--st-open"],
      meta: ["50m 00s · ×2 · codex · outcome: unknown/unverified · runner exit 0", "50m 00s · ×2 · codex · outcome: unknown/unverified · runner exit 1", "5m 00s · claude · outcome: unknown/unverified · runner exit 0"],
    },
    {
      // Alpine throws on a duplicate x-for key, so the whole tab would render
      // empty if the two review runs both keyed review#0.
      name: "history recorded before the run field existed",
      ticket: LEGACY_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
    },
    {
      name: "stage dropped from the pipeline",
      ticket: DROPPED_STAGE_TICKET,
      keys: ["review#1", "review#0", "implement#0"],
      meta: ["50m 00s · ×2 · codex · outcome: unknown/unverified · runner exit 0", "50m 00s · ×2 · codex · outcome: unknown/unverified · runner exit 1", "5m 00s · claude · outcome: unknown/unverified · runner exit 0"],
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
      name: "blocked result without separate prose still has a summary card",
      ticket: { id: "kon-result-only", status: "paused", history: [{stage: "default", exit_code: 0, run_outcome: "blocked", outcome_reason: "Provide device access"}] },
      keys: ["default#0"],
      hues: ["--st-paused"],
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

test("report-only history keeps the Summary tab reachable", () => {
  for (const outcome of ["complete", "blocked", "protocol_error", "failed", undefined]) {
    const ticket = { id: "kon-report-only", status: "paused", history: [
      { stage: "default", run_outcome: outcome, outcome_reason: "Provide device access" },
    ] };
    const state = pageState(ticket);
    assert.equal(state.showSummaryTab(), true, outcome);
    assert.equal(state.summaryCards()[0].summary, "Provide device access", outcome);
    state.activeTab = "summary";
    state.applyTicketUpdate({ ...ticket, title: "Updated title" });
    assert.equal(state.activeTab, "summary", outcome);
  }
  for (const ticket of [null, { history: [] }, { history: [{ stage: "default", run_outcome: "blocked" }] }]) {
    assert.equal(pageState(ticket).showSummaryTab(), false);
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
  assert.match(tab, /x-text="isSecondary\(selectedTicket\) \|\| card\.imported \? .* : stageCardMeta\(card\)"/);

  // The error banner keeps its shape and sits above the outcome rule.
  assert.ok(tab.indexOf('x-text="selectedTicket.last_error"') < tab.indexOf(">outcome<"));
  assert.match(tab, /border-l-\[3px\] border-err pl-3 text-xs/);

  // The rail is dropped, not hidden, when the branch has no commit, and every
  // fact on it comes out of the one changes payload.
  assert.match(tab, /<template x-if="!isSecondary\(selectedTicket\) && ticketChanges\?\.commits\?\.length">/);
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
  assert.match(mobile, /x-if="selectedTicket\?\.last_error"[\s\S]*?role="alert"[^>]*x-text="selectedTicket\.last_error"/);
  assert.ok(mobile.indexOf('x-text="selectedTicket.last_error"') < mobile.indexOf('x-if="selectedTicket?.summary"'));
  assert.ok(mobile.indexOf('x-if="finalSummary()"') < mobile.indexOf('x-if="selectedTicket?.summary"'));
  assert.match(mobile, /<template x-if="summaryStage\(\)">\s*<span\b[^>]*x-text="summaryStage\(\)"/);
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
    { status: "paused", review: true, want: false },
    { status: "human_review", kind: "epic", want: false },
  ];

  for (const c of cases) {
    const state = loadKontoraState();
    state.configCache = c.custom ? { custom_statuses: c.custom } : null;
    state.selectedTicket = { id: "kon-a", kontora: true, kind: c.kind || "", status: c.status, stage: "plan", stages: ["plan", "code"] };
    if (c.review) state.plannotatorInFlight["kon-a"] = true;
    assert.equal(state.canSetStage(), c.want, `${c.status} custom=${c.custom || "none"}`);
    assert.equal(state.canSkipStage(), c.want, `${c.status} review=${!!c.review}`);
    assert.equal(state.mobileSheetActions(state.selectedTicket).some(a => a.label === "Skip stage"), c.want);
  }

  // Nothing selected, nothing to move.
  assert.equal(loadKontoraState().canSetStage(), false);
});

test("stage actions refuse active execution and review without making requests", async () => {
  for (const operation of ["skip", "set-stage"]) {
    for (const review of [false, true]) {
      const calls = [];
      const { state } = loadKontoraContext({ fetch: url => { calls.push(url); throw new Error("unexpected request"); } });
      state.selectedTicket = { id: "kon-a", kontora: true, status: review ? "paused" : "in_progress", stage: "plan", stages: ["plan", "code"] };
      if (review) state.plannotatorInFlight["kon-a"] = true;
      if (operation === "skip") await state.action("skip");
      else await state.setStage("code");
      assert.deepEqual(calls, []);
      assert.match(state.error, /Pause first/);
      assert.equal(state.selectedTicket.stage, "plan");
    }
  }
});

test("the stage row is gated on the predicate and on the pipeline, not on the stage", () => {
  const html = fs.readFileSync(htmlPath, "utf8");

  assert.equal(html.includes("['paused', 'open'].includes"), false, "the hardcoded status list is gone");
  // The row now hangs off the pipeline alone, so a ticket that gained one
  // through the detail form still gets somewhere to set its stage.
  assert.ok(html.includes(`<template x-if="selectedTicket?.stages?.length > 1"><span class="text-surface-600">stage</span></template>`));
  assert.ok(html.includes(`<template x-if="ticketCanControl(selectedTicket) && canSetStage() && setStageOpen">`));
  assert.ok(html.includes(`@click="if (ticketCanControl(selectedTicket) && canSetStage()) setStageOpen = true"`));
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
  assert.ok(html.includes(`<template x-if="ticketCanControl(selectedTicket) && ['paused', 'done', 'cancelled'].includes(selectedTicket?.status) && selectedTicket?.kontora && (selectedTicket?.kind !== 'epic' || epicIsAgent(selectedTicket))">`));
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
  // Three ticket badges with elapsed time, plus both embedded epic headers.
  assert.equal(html.match(/class="waiting-chip"/g).length, 5);
  assert.equal(html.match(/waitingFor\((selectedTicket|ticket)\)/g).length, 3);
  // The chip reuses the review hue and, on desktop, the shared pulse animation.
  assert.match(html, /\.waiting-chip \{[^}]*--st-review/s);
  const mobile = html.slice(html.indexOf('class="mobile-board-layer"'));
  for (const subject of ['ticket', 'selectedTicket', 'selectedEpic']) {
    const badge = [...mobile.matchAll(/<span\b[^>]*class="waiting-chip"[^>]*>([\s\S]*?)<\/span>\s*<\/span>/g)]
      .find(([, content]) => content.includes(`waitingFor(${subject})`));
    assert.ok(badge, subject + ' retains an attention badge');
    assert.match(badge[1], /needs input/);
    assert.match(badge[1], new RegExp(`x-text="waitingFor\\(${subject}\\)"`));
    assert.doesNotMatch(badge[0], /pulse-dot|waiting-dot/);
  }
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
      if (opts.fetch) return opts.fetch(url, init);
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
  return { state, store, requests, listeners, scroller, streams, runFrames, runTimers, routes };
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

test("Escape returns ticket details to the board before closing the assistant", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const chain = html.match(/@keydown\.escape\.window="([^"]+)"/)[1];
  const cases = [
    { name: "assistant closed", assistantOpen: false },
    { name: "assistant open", assistantOpen: true },
    { name: "detail menu open", assistantOpen: true, detailMenuOpen: true },
    { name: "delete dialog open", assistantOpen: true, deleteModal: true },
  ];

  for (const tc of cases) {
    const { state, location } = routerState("#/t/kon-1");
    state.selectedTicket = { id: "kon-1", status: "open" };
    state.assistantOpen = tc.assistantOpen;
    state.detailMenuOpen = !!tc.detailMenuOpen;
    state.deleteModal = !!tc.deleteModal;
    state.closeDeleteModal = () => { state.deleteModal = false; };
    const pressEscape = new Function("state", `with (state) { ${chain} }`);

    pressEscape(state);

    if (tc.detailMenuOpen || tc.deleteModal) {
      assert.equal(state.selectedTicket?.id, "kon-1", tc.name);
      assert.equal(state.detailMenuOpen, false, tc.name);
      assert.equal(state.deleteModal, false, tc.name);
      pressEscape(state);
    }
    assert.equal(state.selectedTicket, null, tc.name);
    assert.equal(state.currentView, "board", tc.name);
    assert.equal(location.hash, "#/", tc.name);
    assert.equal(state.assistantOpen, tc.assistantOpen, tc.name);
  }
});

test("the body escape chain reaches the pane after sheets and ticket details", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const chain = html.match(/@keydown\.escape\.window="([^"]+)"/)[1];

  assert.ok(chain.indexOf("closeSheet()") < chain.indexOf("assistantEscape()"));
  assert.ok(chain.indexOf("closeDetail()") < chain.indexOf("assistantEscape()"));
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

test("approval labels distinguish deletion, writes and unclassified calls", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const { state } = assistantState();
  for (const [kind, label] of [["delete", "delete"], ["write", "change"], ["unknown", "unclassified call"], ["unexpected", "unclassified call"]]) {
    state.assistantGate = { kind };
    assert.equal(state.assistantApprovalLabel(), label);
  }
  assert.equal((html.match(/x-text="assistantApprovalLabel\(\)"/g) || []).length, 2);
  assert.match(state.assistantAutonomies().find((a) => a.key === "auto").hint, /asks for deletes and unclassified calls/);
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
  assert.deepEqual(urls, ["/api/assistant", "/api/assistant/companion", "/api/config"]);
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
  assert.match(html, /x-if="!assistantEnabled\(\) && !companionThread\(\) && assistantView === 'thread'"[\s\S]{0,200}?id="assistant-configure"/);
  const composer = html.slice(html.indexOf('<!-- Composer -->'));
  assert.match(composer.slice(0, 200), /x-if="\(assistantEnabled\(\) \|\| companionThread\(\)\) && assistantView === 'thread' && assistantThreadVisible\(\)"/);
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

// The local wall time the schedule fields hold, and the instant it means. Built
// with the runner's own zone so the assertions hold wherever the suite runs.
const SCHED_LOCAL = "2099-09-01T09:00";
const SCHED_ISO = new Date(2099, 8, 1, 9, 0, 0, 0).toISOString().replace(/\.\d{3}Z$/, "Z");

// A component whose fetch records what was posted where.
function scheduleState() {
  const posted = [];
  const state = loadKontoraState({
    // Only the writes are recorded: opening the create modal fetches the
    // config, and that GET is not what these tests are asserting on.
    fetch: async (url, opts) => {
      if (opts && opts.method === "POST") posted.push({ url, body: JSON.parse(opts.body) });
      return { ok: true, json: async () => ({ id: "kon-1", status: "open", kontora: true, scheduled_at: SCHED_ISO }) };
    },
  });
  state._boardInit = false;
  return { state, posted };
}

// Fills in the create form and switches Start at to "later", which is what
// turns the text field into a schedule.
async function createLater(state, text, entry = "desktop") {
  if (entry === "mobile") {
    state.isMobile = true;
    await state.openNewSheet();
  } else {
    await state.openCreateModal();
  }
  state.createForm.title = "Later";
  state.createForm.path = "/repo";
  state.setCreateScheduleMode("later");
  state.createForm.scheduled_at = text;
  state.onCreateScheduleChange();
}

for (const entry of ["desktop", "mobile"]) {
  test(`the create form sends the instant its start-at field means: ${entry}`, async () => {
    const { state, posted } = scheduleState();
    await createLater(state, SCHED_LOCAL, entry);
    const submit = () => entry === "mobile" ? state.submitCreateTicketMobile() : state.submitCreateTicket();

    assert.equal(await submit(), true);

    assert.equal(posted.length, 1);
    assert.equal(posted[0].url, "/api/tickets");
    assert.equal(posted[0].body.scheduled_at, SCHED_ISO);
    assert.equal(posted[0].body.status, "open", "a schedule is what moves the ticket, so it starts open");
    assert.equal(state.createForm.status, "open");
    assert.equal(state.createSubmitting, false);
    if (entry === "mobile") assert.equal(state.sheet, null);
    else assert.equal(state.currentView, "board");
  });
}

test("the start-at field takes both grammars kontora schedule takes", async () => {
  const cases = [
    { name: "a space between date and time", text: "2099-09-01 09:00", want: SCHED_ISO },
    { name: "a T between date and time", text: SCHED_LOCAL, want: SCHED_ISO },
    { name: "a delay in hours", text: "24h", want: null },
    { name: "a delay in days", text: "3d", want: null },
    { name: "a composite delay", text: "1w2d3h", want: null },
  ];
  for (const c of cases) {
    const { state, posted } = scheduleState();
    await createLater(state, c.text);
    await state.submitCreateTicket();
    if (c.want) {
      assert.equal(posted[0].body.scheduled_at, c.want, c.name);
    } else {
      // A delay is measured from the clock, so only its shape is asserted.
      assert.match(posted[0].body.scheduled_at, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/, c.name);
    }
    assert.equal(posted[0].body.status, "open", c.name);
  }
});

for (const entry of ["desktop", "mobile"]) {
  test(`start-at refuses what it cannot read rather than creating an unscheduled ticket: ${entry}`, async () => {
    // A bare date must not become midnight in another time zone.
    for (const bad of ["", "2099-09", "2099-09-01", "not a date", "3 days", "2020-01-01 09:00"]) {
      const { state, posted } = scheduleState();
      await createLater(state, bad, entry);
      const form = state.createForm;
      const sheet = state.sheet;

      if (bad) assert.ok(state.createScheduleError(), bad);
      assert.equal(state.createBlocked(), true, bad);
      const result = entry === "mobile" ? await state.submitCreateTicketMobile() : await state.submitCreateTicket();
      assert.equal(result, false, bad);
      assert.equal(posted.length, 0, bad);
      assert.equal(state.createSubmitting, false, bad);
      assert.equal(state.createForm, form, bad);
      if (entry === "mobile") assert.equal(state.sheet, sheet, bad);
      else assert.equal(state.currentView, "new", bad);
    }
  });

  test(`start-at on now creates an ordinary ticket, whatever the field holds: ${entry}`, async () => {
    const { state, posted } = scheduleState();
    await createLater(state, SCHED_LOCAL, entry);
    state.setCreateScheduleMode("now");

    assert.equal(state.createForm.scheduled_at, "", "leaving later drops the time rather than hiding it");
    const result = entry === "mobile" ? await state.submitCreateTicketMobile() : await state.submitCreateTicket();
    assert.equal(result, true);

    assert.equal(posted[0].body.scheduled_at, undefined);
    assert.equal(posted[0].body.status, "todo");
  });
}

test("the mobile create input submits future times, clears to now and rejects invalid times", async () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const mobile = html.slice(html.indexOf('<select x-model="createForm.kind"'));
  const input = mobile.match(/<input\b[^>]*(?:(?:x-model|:value)="createForm\.scheduled_at"|aria-label="Start at")[^>]*>/)?.[0];
  assert.ok(input, "the mobile create form has a schedule input");
  const handler = input.match(/@(?:change|input)="([^"]*)"/)?.[1];
  assert.ok(handler, "the schedule input updates the create schedule mode");
  const change = new Function("state", "$event", `with (state) { ${handler} }`);

  for (const value of [SCHED_LOCAL, "", "2099-09-01"]) {
    const { state, posted } = scheduleState();
    state.isMobile = true;
    await state.openNewSheet();
    Object.assign(state.createForm, { title: "Later", path: "/repo", scheduled_at: SCHED_LOCAL });
    change(state, { target: { value: SCHED_LOCAL } });
    assert.equal(state.createForm.scheduleMode, "later");
    assert.equal(state.createScheduleISO(), SCHED_ISO);
    state.createForm.scheduled_at = value;
    change(state, { target: { value } });
    const blocked = value === "2099-09-01";
    assert.equal(state.createForm.scheduleMode, value ? "later" : "now");
    assert.equal(state.createBlocked(), blocked);
    const sheet = state.sheet;
    assert.equal(await state.submitCreateTicketMobile(), !blocked);
    assert.equal(posted.length, blocked ? 0 : 1);
    if (blocked) {
      assert.equal(state.sheet, sheet);
      assert.ok(state.createScheduleError());
    } else {
      assert.equal(posted[0].body.scheduled_at, value ? SCHED_ISO : undefined);
      assert.equal(posted[0].body.status, value ? "open" : "todo");
    }
  }
});

for (const entry of ["desktop", "mobile"]) {
  test(`create errors belong to the form and reset when another form opens: ${entry}`, async () => {
    const { state } = scheduleState();
    assert.equal(state.createError, null);
    state.error = "Unrelated failure";
    state.createError = "Old create failure";
    state.createSubmitting = true;
    if (entry === "mobile") { state.isMobile = true; await state.openNewSheet(); }
    else await state.openCreateModal();
    assert.equal(state.createError, null);
    assert.equal(state.createSubmitting, false);
    assert.equal(state.error, "Unrelated failure");
    state.createError = "Another create failure";
    if (entry === "mobile") state.closeSheet();
    else state.closeCreateModal();
    assert.equal(state.createError, null);
  });
}

for (const kind of ["", "epic"]) {
  for (const scenario of ["desktop:http", "desktop:unreadable", "desktop:network", "mobile:http", "mobile:unreadable", "mobile:network"]) {
    const [entry, failure] = scenario.split(":");
    test(`create failures keep the draft and allow a retry: kind=${kind || "ticket"}, ${scenario}`, async () => {
      const { state, ctx } = loadKontoraContext();
      state.configCache = {};
      if (entry === "mobile") { state.isMobile = true; await state.openNewSheet(); }
      else await state.openCreateModal();
      Object.assign(state.createForm, { title: "Draft", path: "/repo", kind });
      const submit = () => entry === "mobile" ? state.submitCreateTicketMobile() : state.submitCreateTicket();
      const sheet = state.sheet;
      state.error = "Unrelated failure";
      ctx.fetch = async () => {
        if (failure === "network") throw new Error("offline");
        return { ok: false, json: async () => {
          if (failure === "unreadable") throw new Error("bad JSON");
          return { error: "Create rejected" };
        } };
      };
      const form = state.createForm;
      assert.equal(await submit(), false);
      assert.equal(state.createError, failure === "http" ? "Create rejected"
        : failure === "network" ? "Failed to create ticket: offline" : "Failed to create ticket");
      assert.equal(state.error, "Unrelated failure");
      if (entry === "mobile") assert.equal(state.sheet, sheet);
      else assert.equal(state.currentView, "new");
      assert.equal(state.createForm, form);
      assert.equal(state.createSubmitting, false);
      ctx.fetch = async () => ({ ok: true });
      assert.equal(await submit(), true);
      assert.equal(state.createError, null);
      assert.equal(state.createSubmitting, false);
      assert.equal(state.currentView, "board");
      if (entry === "mobile") assert.equal(state.sheet, null);
    });
  }
}

for (const entry of ["desktop", "mobile"]) {
  test(`create rejects missing fields without closing the form: ${entry}`, async () => {
    for (const missing of ["title", "path"]) {
      const { state, posted } = scheduleState();
      await createLater(state, SCHED_LOCAL, entry);
      state.createForm[missing] = " \t ";
      const sheet = state.sheet;
      const result = entry === "mobile" ? await state.submitCreateTicketMobile() : await state.submitCreateTicket();
      assert.equal(result, false);
      assert.equal(posted.length, 0);
      assert.equal(state.createSubmitting, false);
      if (entry === "mobile") assert.equal(state.sheet, sheet);
      else assert.equal(state.currentView, "new");
    }
  });

  test(`create ignores a second submit while the first POST is pending: ${entry}`, async () => {
    const { state, ctx } = loadKontoraContext();
    state.configCache = {};
    await createLater(state, SCHED_LOCAL, entry);
    const gate = deferred();
    let posts = 0;
    ctx.fetch = () => { posts++; return gate.promise; };
    const submit = () => entry === "mobile" ? state.submitCreateTicketMobile() : state.submitCreateTicket();
    const sheet = state.sheet;
    const first = submit();
    assert.equal(state.createSubmitting, true);
    assert.equal(await submit(), false);
    assert.equal(posts, 1);
    if (entry === "mobile") assert.equal(state.sheet, sheet);
    gate.resolve({ ok: true });
    assert.equal(await first, true);
    assert.equal(state.createSubmitting, false);
  });
}

function pendingCreateReply({ ok = true, delayJSON = false, network = false, unreadable = false }, data) {
  const gate = deferred();
  return {
    release: () => gate.resolve(),
    fetch: async () => {
      if (!delayJSON) await gate.promise;
      if (network) throw new Error('offline');
      return { ok, json: async () => {
        if (delayJSON) await gate.promise;
        if (unreadable) throw new Error('bad JSON');
        return data;
      } };
    },
  };
}

const pendingCreateCases = {
  config: [
    { name: 'response success' },
    { name: 'JSON success', delayJSON: true },
    { name: 'HTTP failure', ok: false },
    { name: 'network failure', network: true },
    { name: 'JSON failure', delayJSON: true, unreadable: true },
  ],
  POST: [
    { name: 'response success' },
    { name: 'HTTP failure', ok: false },
    { name: 'network failure', network: true },
    { name: 'error JSON success', ok: false, delayJSON: true },
    { name: 'error JSON failure', ok: false, delayJSON: true, unreadable: true },
  ],
};

for (const phase of ['config', 'POST']) {
  for (const reply of pendingCreateCases[phase]) {
    for (const overlay of ['navigation', 'assistant']) {
      for (const closeOverlay of [false, true]) {
        test(`routed New keeps pending ${phase} ${reply.name} across ${overlay}: close=${closeOverlay}`, async () => {
          const { state, ctx } = loadKontoraContext();
          state.isMobile = true;
          state.error = 'Unrelated failure';
          const data = phase === 'config'
            ? { projects: [{ path: '/repo', agent: 'project-agent', pipeline: 'project-pipeline' }] }
            : { error: 'Create rejected' };
          const pending = pendingCreateReply(reply, data);
          state.configCache = {};
          await state.gotoView('new');
          Object.assign(state.createForm, { title: 'Draft', path: '/repo' });
          const showOverlay = () => {
            // The mobile navigation button assigns the sheet directly.
            if (overlay === 'navigation') state.sheet = { type: 'navigation' };
            else {
              state.assistantLoadThreads = async () => {};
              state.openAssistantSheet();
            }
          };
          if (closeOverlay) showOverlay();
          if (phase === 'config') state.configCache = null;
          ctx.fetch = pending.fetch;
          const request = phase === 'config' ? state.loadCreateConfig() : state.submitCreateTicket();
          await flushMicrotasks();
          const form = state.createForm;
          if (closeOverlay) state.closeSheet();
          else showOverlay();
          const sheet = state.sheet;
          assert.equal(state.currentView, 'new');
          if (phase === 'POST') assert.equal(state.createSubmitting, true);
          pending.release();
          const result = await request;
          const success = reply.ok !== false && !reply.network && !reply.unreadable;
          assert.equal(state.createForm, form);
          assert.equal(state.sheet, sheet, 'the routed form does not own the other sheet');
          assert.equal(state.error, 'Unrelated failure');
          assert.equal(state.createSubmitting, false);
          if (phase === 'config') {
            assert.equal(state.currentView, 'new');
            assert.equal(state.configCache, success ? data : null);
            assert.equal(state.createError, success ? null : 'Failed to load config');
            if (success) {
              assert.equal(form.agent, 'project-agent');
              assert.equal(form.pipeline, 'project-pipeline');
            }
          } else {
            assert.equal(result, success);
            assert.equal(state.currentView, success ? 'board' : 'new');
            assert.equal(state.createError, success ? null : reply.network ? 'Failed to create ticket: offline'
              : reply.unreadable ? 'Failed to create ticket' : 'Create rejected');
          }
        });
      }
    }

    for (const clear of ['primary deletion', 'secondary deletion', 'primary archive']) {
      test(`New sheet keeps pending ${phase} ${reply.name} across SSE ${clear}`, async () => {
        const sources = [];
        const { state, ctx } = loadKontoraContext({ EventSource: makeEventSourceFake(sources) });
        state.isMobile = true;
        state.updateFavicon = () => {};
        const primary = { id: 'same', title: 'Primary', status: 'open', kontora: true };
        const secondary = { ...primary, source: 'team', title: 'Secondary' };
        state.tickets = [primary];
        state.secondaryTickets = [secondary];
        state.selectedTicket = clear === 'secondary deletion' ? secondary : primary;
        state.connectSSE();
        const data = phase === 'config'
          ? { projects: [{ path: '/repo', agent: 'project-agent', pipeline: 'project-pipeline' }] }
          : { error: 'Create rejected' };
        const pending = pendingCreateReply(reply, data);
        let request;
        if (phase === 'config') {
          ctx.fetch = pending.fetch;
          request = state.openNewSheet();
        } else {
          state.configCache = {};
          await state.openNewSheet();
          Object.assign(state.createForm, { title: 'Draft', path: '/repo' });
          ctx.fetch = pending.fetch;
          request = state.submitCreateTicketMobile();
        }
        await flushMicrotasks();
        const form = state.createForm;
        form.path = '/repo';
        const sheet = state.sheet;
        const selected = state.selectedTicket;
        if (clear === 'secondary deletion') {
          sources[0].emit('ticket_deleted', primary);
          assert.equal(state.selectedTicket, selected, 'primary namesake deletion leaves the secondary selection');
          sources[0].emit('secondary_ticket_deleted', { ticket: secondary });
        } else {
          sources[0].emit('secondary_ticket_deleted', { ticket: secondary });
          assert.equal(state.selectedTicket, selected, 'secondary namesake deletion leaves the primary selection');
          if (clear === 'primary deletion') sources[0].emit('ticket_deleted', primary);
          else {
            sources[0].emit('ticket_updated', { ...primary, status: 'archived' });
            state.flushTicketUpdates();
          }
        }
        assert.equal(state.selectedTicket, null);
        assert.equal(state.sheet, sheet);
        if (phase === 'POST') assert.equal(state.createSubmitting, true);
        pending.release();
        const result = await request;
        const success = reply.ok !== false && !reply.network && !reply.unreadable;
        assert.equal(state.createForm, form);
        assert.equal(state.createSubmitting, false);
        assert.equal(state.currentView, 'board');
        if (phase === 'config') {
          assert.equal(state.sheet, sheet);
          assert.equal(state.configCache, success ? data : null);
          assert.equal(state.createError, success ? null : 'Failed to load config');
          if (success) {
            assert.equal(form.agent, 'project-agent');
            assert.equal(form.pipeline, 'project-pipeline');
          }
        } else {
          assert.equal(result, success);
          assert.equal(state.sheet, success ? null : sheet);
          assert.equal(state.createError, success ? null : reply.network ? 'Failed to create ticket: offline'
            : reply.unreadable ? 'Failed to create ticket' : 'Create rejected');
        }
      });
    }
  }
}

for (const delayed of ["response", "json", "failure", "http"]) {
  for (const scenario of ["desktop:close", "desktop:reopen", "desktop:navigate", "desktop:destroy", "mobile:close", "mobile:reopen", "mobile:navigate", "mobile:replacement", "mobile:destroy"]) {
    const [entry, supersession] = scenario.split(":");
    test(`create config ignores delayed ${delayed} after ${scenario}`, async () => {
      const gate = deferred();
      const { state } = loadKontoraContext({ fetch: async () => {
        if (delayed === "response") return gate.promise;
        if (delayed === "failure") { await gate.promise; throw new Error("offline"); }
        if (delayed === "http") { await gate.promise; return { ok: false }; }
        return { ok: true, json: () => gate.promise };
      } });
      state.isMobile = entry === "mobile";
      const open = () => entry === "mobile" ? state.openNewSheet() : state.openCreateModal();
      const opening = open();
      await flushMicrotasks();
      const oldForm = state.createForm;
      if (supersession === "close") {
        if (entry === "mobile") state.closeSheet();
        else state.closeCreateModal();
      } else if (supersession === "navigate") await state.gotoView("stats");
      else if (supersession === "destroy") state.destroy();
      else if (supersession === "replacement") state.sheet = { type: 'new' };
      else {
        state.configCache = { projects: [{ path: "/repo", agent: "new-agent", pipeline: "new-pipeline" }] };
        await open();
      }
      Object.assign(state.createForm, { path: "/repo", agent: "draft-agent", pipeline: "draft-pipeline" });
      state.createError = "Current create failure";
      state.error = "Current global failure";
      const config = state.configCache;
      const staleConfig = { projects: [{ path: "/repo", agent: "old-agent", pipeline: "old-pipeline" }] };
      gate.resolve(delayed === "response" ? { ok: true, json: async () => staleConfig } : staleConfig);
      await opening;
      assert.equal(state.configCache, config);
      assert.equal(state.createForm.agent, "draft-agent");
      assert.equal(state.createForm.pipeline, "draft-pipeline");
      assert.equal(state.createError, "Current create failure");
      assert.equal(state.error, "Current global failure");
      if (supersession === "reopen") assert.notEqual(state.createForm, oldForm);
    });
  }
}

for (const scenario of ["desktop:http", "desktop:network", "mobile:http", "mobile:network"]) {
  const [entry, failure] = scenario.split(":");
  test(`create config reports ${scenario} failure only on the current form`, async () => {
    const { state } = loadKontoraContext({ fetch: async () => {
      if (failure === "network") throw new Error("offline");
      return { ok: false };
    } });
    state.error = "Unrelated failure";
    if (entry === "mobile") { state.isMobile = true; await state.openNewSheet(); }
    else await state.openCreateModal();
    assert.equal(state.createError, "Failed to load config");
    assert.equal(state.error, "Unrelated failure");
    if (entry === "mobile") assert.equal(state.sheet?.type, "new");
    else assert.equal(state.currentView, "new");
  });
}

for (const outcome of ["success", "http", "network", "error JSON"]) {
  for (const supersession of ["close", "reopen", "navigate", "mobile replacement", "destroy"]) {
    test(`create POST ignores stale ${outcome} after ${supersession}`, async () => {
      const { state, ctx } = loadKontoraContext();
      state.configCache = {};
      await createLater(state, SCHED_LOCAL, supersession === "mobile replacement" ? "mobile" : "desktop");
      const gate = deferred();
      ctx.fetch = async () => {
        if (outcome === "error JSON") return { ok: false, json: () => gate.promise };
        await gate.promise;
        if (outcome === "network") throw new Error("offline");
        return { ok: outcome === "success", json: async () => ({ error: "Old failure" }) };
      };
      const posting = state.submitCreateTicket();
      await flushMicrotasks();
      if (supersession === "close") state.closeCreateModal();
      else if (supersession === "reopen") await state.openCreateModal();
      else if (supersession === "navigate") await state.gotoView("board");
      else if (supersession === "destroy") state._appDestroyed = true;
      else state.sheet = { type: "actions", ticket: { id: "other" } };
      state.createForm.title = "New draft";
      state.createSubmitting = true;
      state.createError = "New create failure";
      state.error = "New global failure";
      const form = state.createForm;
      const sheet = state.sheet;
      const view = state.currentView;
      gate.resolve({ error: "Old failure" });
      assert.equal(await posting, false);
      assert.equal(state.createForm, form);
      assert.equal(state.createForm.title, "New draft");
      assert.equal(state.sheet, sheet);
      assert.equal(state.currentView, view);
      assert.equal(state.createSubmitting, true);
      assert.equal(state.createError, "New create failure");
      assert.equal(state.error, "New global failure");
    });
  }
}

test("closing create invalidates a POST even when the same draft object is reused", async () => {
  const { state, ctx } = loadKontoraContext();
  state.configCache = {};
  await createLater(state, SCHED_LOCAL);
  const gate = deferred();
  ctx.fetch = () => gate.promise;
  const posting = state.submitCreateTicket();
  state.closeCreateModal();
  state.currentView = "new";
  state.createSubmitting = true;
  state.createError = "Current failure";
  gate.resolve({ ok: true });
  assert.equal(await posting, false);
  assert.equal(state.currentView, "new");
  assert.equal(state.createSubmitting, true);
  assert.equal(state.createError, "Current failure");
});

test("the frontmatter preview shows the schedule under the open it forces", async () => {
  const { state } = scheduleState();
  await createLater(state, SCHED_LOCAL);
  state.createForm.status = "todo";

  const yaml = state.createPreviewYaml;
  assert.ok(yaml.includes('status: open\nscheduled_at: "' + SCHED_ISO + '"'), yaml);
  // The highlighted line is the one the preview paints in the open hue.
  const hue = state.createPreviewLines.filter(l => l.hue).map(l => l.text).join("|");
  assert.equal(hue, 'scheduled_at: "' + SCHED_ISO + '"');
});

test("the echo line names the instant in the reader's own zone", async () => {
  const { state } = scheduleState();
  await createLater(state, SCHED_LOCAL);

  const echo = state.createScheduleEcho();
  const local = new Date(2099, 8, 1, 9, 0, 0, 0);
  assert.match(echo.long, /^Tue 1 Sep 2099, 09:00$/, echo.long);
  assert.equal(new Date(echo.rfc).getTime(), local.getTime(), "the echoed instant is the one being sent");
  assert.match(echo.distance, /^in /);
});

test("presets write a time the field itself can read back", async () => {
  const { state, posted } = scheduleState();
  await state.openCreateModal();
  state.createForm.title = "Later";
  state.createForm.path = "/repo";
  state.setCreateScheduleMode("later");

  const presets = state.createSchedulePresets();
  assert.ok(presets.length >= 4, "tonight may have passed, the rest cannot");
  state.pickCreatePreset(presets[0]);

  assert.equal(state.createPresetSelected(presets[0]), true);
  assert.equal(state.createScheduleError(), "", "a preset is always readable");
  await state.submitCreateTicket();
  assert.equal(posted[0].body.scheduled_at, presets[0].iso);
});

test("the schedule editor round-trips the stored instant through local time", () => {
  const { state } = scheduleState();
  state.selectedTicket = { id: "kon-1", status: "open", kontora: true, scheduled_at: SCHED_ISO };

  state.openScheduleEditor();

  assert.equal(state.scheduleEditing, true);
  assert.equal(state.scheduleDraft, SCHED_LOCAL);
});

test("saving a schedule posts the instant and folds the answer back in", async () => {
  const { state, posted } = scheduleState();
  state.tickets = [{ id: "kon-1", title: "T", status: "todo", kontora: true }];
  state.selectedTicket = { id: "kon-1", title: "T", status: "todo", kontora: true };
  state.openScheduleEditor();
  state.scheduleDraft = SCHED_LOCAL;

  await state.submitSchedule();

  assert.deepEqual(posted, [{ url: "/api/tickets/kon-1/schedule", body: { scheduled_at: SCHED_ISO } }]);
  assert.equal(state.scheduleEditing, false);
  assert.equal(state.selectedTicket.scheduled_at, SCHED_ISO);
  assert.equal(state.selectedTicket.status, "open", "scheduling a queued ticket returns it to open");
  assert.equal(state.tickets[0].scheduled_at, SCHED_ISO);
});

test("clearing a schedule says so rather than sending an empty instant", async () => {
  const { state, posted } = scheduleState();
  state.selectedTicket = { id: "kon-1", status: "open", kontora: true, scheduled_at: SCHED_ISO };

  await state.clearTicketSchedule();

  assert.deepEqual(posted, [{ url: "/api/tickets/kon-1/schedule", body: { clear: true } }]);
});

test("an empty draft is refused without a request", async () => {
  const { state, posted } = scheduleState();
  state.selectedTicket = { id: "kon-1", status: "open", kontora: true };
  state.openScheduleEditor();
  state.scheduleDraft = "";

  await state.submitSchedule();

  assert.equal(posted.length, 0);
  assert.ok(state.scheduleError);
});

test("only a managed open or todo ticket offers the schedule controls", () => {
  const { state } = scheduleState();
  const can = (t) => state.canSchedule(t);

  assert.equal(can({ status: "open", kontora: true }), true);
  assert.equal(can({ status: "todo", kontora: true }), true);
  assert.equal(can({ status: "open", kontora: false }), false);
  assert.equal(can({ status: "in_progress", kontora: true }), false);
  assert.equal(can({ status: "done", kontora: true }), false);
  assert.equal(can(null), false);
});

test("a schedule the daemon could not parse is shown as it stands", () => {
  const { state } = scheduleState();

  assert.equal(state.scheduleLabel({ scheduled_at: "next tuesday" }), "next tuesday");
  assert.equal(state.scheduleLabel({}), "");
});

test("a schedule change reaches the panel while the body is being edited", () => {
  const { state } = scheduleState();
  state.tickets = [{ id: "kon-1", title: "T", status: "open", kontora: true }];
  state.selectedTicket = { id: "kon-1", title: "T", status: "open", kontora: true, body: "typing" };
  state.editing = true;

  state.applyTicketUpdate({ id: "kon-1", title: "T", status: "open", kontora: true, scheduled_at: SCHED_ISO });

  assert.equal(state.selectedTicket.scheduled_at, SCHED_ISO);
  assert.equal(state.selectedTicket.body, "typing", "the edited body survives");
});

// A component holding one schedulable ticket, opened in the palette's schedule
// scope. Records what each row posts.
function scheduleScopeState(overrides) {
  const ticket = Object.assign({ id: "kon-1", title: "Later", status: "open", kontora: true, scheduled_at: "" }, overrides || {});
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => {
      if (opts && opts.method === "POST") posted.push({ url, body: opts.body ? JSON.parse(opts.body) : null });
      return { ok: true, json: async () => Object.assign({}, ticket, { scheduled_at: SCHED_ISO }) };
    },
  });
  state._boardInit = false;
  state.tickets = [ticket];
  state.paletteOpen = true;
  state.palettePush(ticket.id, "schedule");
  return { state, posted, ticket };
}

const rowsOf = (state) => state.paletteGroups.flatMap(g => g.items.map(r => r.id));

test("the schedule scope is offered only for a ticket that can carry a schedule", () => {
  const cases = [
    { name: "open", ticket: { status: "open" }, want: true },
    { name: "todo", ticket: { status: "todo" }, want: true },
    { name: "running", ticket: { status: "in_progress" }, want: false },
    { name: "done", ticket: { status: "done" }, want: false },
    { name: "not initialized", ticket: { status: "open", kontora: false }, want: false },
  ];
  for (const c of cases) {
    const state = loadKontoraState({});
    state._boardInit = false;
    state.tickets = [Object.assign({ id: "kon-1", title: "T", kontora: true }, c.ticket)];
    state.paletteOpen = true;
    state.palettePush("kon-1", "actions");
    const drill = rowsOf(state).includes("scheduledrill:kon-1");
    assert.equal(drill, c.want, c.name);
  }
});

test("the schedule scope offers the presets and pops back to the ticket", () => {
  const { state } = scheduleScopeState();

  assert.equal(state.paletteScopeKind, "schedule");
  assert.equal(state.paletteScopeLabel(), "schedule kon-1");
  assert.deepEqual(state.paletteGroups.map(g => g.label).join(), "when");
  const rows = rowsOf(state);
  assert.ok(rows.length >= 3, rows.join());
  assert.ok(rows.includes("schedule:kon-1:type"), "the free-text hint row is offered");

  // Back out one level at a time: the ticket first, then the root.
  state.palettePop();
  assert.equal(state.paletteScopeKind, "actions");
  assert.equal(state.paletteScope, "kon-1");
  state.palettePop();
  assert.equal(state.paletteScope, null);
});

test("free text in the schedule scope is parsed live and leads the list", () => {
  const { state } = scheduleScopeState();

  state.paletteQuery = "3d";
  state.onPaletteQueryChanged();
  const top = state._paletteRows[0];
  assert.equal(top.id, "schedule:kon-1:typed");
  assert.match(top.scheduleISO, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  assert.ok(!rowsOf(state).includes("schedule:kon-1:type"), "a query that parses replaces the hint row");

  // Text that is not a time says so on the hint row rather than vanishing.
  state.paletteQuery = "nonsense";
  state.onPaletteQueryChanged();
  const hint = state._paletteRows.find(r => r.id === "schedule:kon-1:type");
  assert.equal(hint.title, "not a time or a duration");
});

test("running a schedule row posts the instant it carries", async () => {
  const { state, posted } = scheduleScopeState();
  const row = state._paletteRows.find(r => r.scheduleISO);

  await state.paletteRun(row, false);

  assert.deepEqual(posted, [{ url: "/api/tickets/kon-1/schedule", body: { scheduled_at: row.scheduleISO } }]);
  assert.equal(state.paletteOpen, false, "the palette closes so its error line is not the surface");
});

test("enter on the hint row leaves the palette open rather than posting", async () => {
  const { state, posted } = scheduleScopeState();
  const hint = state._paletteRows.find(r => r.id === "schedule:kon-1:type");

  await state.paletteRun(hint, false);

  assert.equal(posted.length, 0);
  assert.equal(state.paletteOpen, true);
});

test("clear and run-now are offered only once a schedule exists", async () => {
  const bare = scheduleScopeState();
  assert.ok(!rowsOf(bare.state).includes("schedclear:kon-1"), "nothing to clear yet");

  const { state, posted } = scheduleScopeState({ scheduled_at: SCHED_ISO });
  assert.deepEqual(state.paletteGroups.map(g => g.label).join(), "when,also");

  await state.paletteRun(state._paletteRows.find(r => r.id === "schedclear:kon-1"), false);
  assert.deepEqual(posted, [{ url: "/api/tickets/kon-1/schedule", body: { clear: true } }]);

  // run-now is the ordinary start action — the daemon drops the timestamp in
  // the same save — so it is asserted at that call, as the other action rows are.
  const runner = scheduleScopeState({ scheduled_at: SCHED_ISO });
  const calls = [];
  runner.state.moveTicketVia = async (id, endpoint) => { calls.push([id, endpoint]); };
  await runner.state.paletteRun(runner.state._paletteRows.find(r => r.id === "schedrun:kon-1"), false);
  assert.deepEqual(calls.join(), "kon-1,run");
});

test("the schedule scope pops out when the ticket stops being schedulable", () => {
  const { state, ticket } = scheduleScopeState();

  // An SSE update starts the ticket while the scope is open.
  ticket.status = "in_progress";
  state.recomputePalette();

  assert.equal(state.paletteScopeKind, "actions", "an empty schedule scope is not left on screen");
});

test("the phone sheet reads both grammars and sends the instant", async () => {
  const { state, posted, ticket } = scheduleScopeState({ scheduled_at: SCHED_ISO });
  state.closePalette();
  state.openScheduleSheet(ticket);

  assert.equal(state.sheet.type, "schedule");
  assert.match(state.scheduleDraft, /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/, "the draft is the grammar the field takes");
  assert.match(state.mobileScheduleCurrent(ticket), /^starts /);

  state.scheduleDraft = "24h";
  assert.ok(state.mobileScheduleConsequence().startsWith("Sent as "), state.mobileScheduleConsequence());
  await state.submitScheduleMobile(ticket);

  assert.equal(posted[0].url, "/api/tickets/kon-1/schedule");
  assert.match(posted[0].body.scheduled_at, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  assert.equal(state.sheet, null);
});

test("the phone sheet refuses what it cannot read rather than posting", async () => {
  const { state, posted, ticket } = scheduleScopeState();
  state.closePalette();
  state.openScheduleSheet(ticket);
  state.scheduleDraft = "2026-09-01";

  await state.submitScheduleMobile(ticket);

  assert.equal(posted.length, 0);
  assert.match(state.scheduleError, /add a time/);
  assert.equal(state.mobileScheduleConsequence(), "", "no instant, no consequence line");
});

test("a phone preset fills the field with a time the field can read back", () => {
  const { state, ticket } = scheduleScopeState();
  state.closePalette();
  state.openScheduleSheet(ticket);

  const preset = state.mobileSchedulePresets()[0];
  state.mobileSchedulePick(preset);

  assert.equal(state.mobileSchedulePresetSelected(preset), true);
  assert.equal(state.mobileScheduleParsed().iso, preset.iso);
});


// --- epics ------------------------------------------------------------------

// One epic in in_progress and three children, the shape the spec names.
const EPIC_TICKETS = [
  { id: "kon-e7c1", title: "Epics", status: "in_progress", kontora: true, kind: "epic", path: "/r/kontora", child_order: ["kon-c04d", "kon-b12e"] },
  { id: "kon-b12e", title: "Rollup counts", status: "in_progress", kontora: true, stage: "implement", agent: "claude", created_at: "2026-01-01T00:00:00Z", started_at: "2026-01-03T00:00:00Z", parent: { id: "kon-e7c1" } },
  { id: "kon-c04d", title: "Rail section", status: "human_review", kontora: true, created_at: "2026-01-02T00:00:00Z", parent: { id: "kon-e7c1" } },
  { id: "kon-9b31", title: "Epic page", status: "done", kontora: true, created_at: "2026-01-03T00:00:00Z", parent: { id: "kon-e7c1" } },
];

test("a passive epic takes no column and does not count toward the status tallies", () => {
  const state = loadKontoraState();
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();

  for (const col of state.columns) {
    assert.equal(
      state.boardTickets(col.key).some((t) => t.id === "kon-e7c1"), false,
      `${col.key} must not hold the epic`);
  }
  // The epic's own derived in_progress must not double-count its running child.
  assert.equal(state._statusCounts.in_progress, 1);
  assert.deepEqual(vmValue(state.epics.map((e) => e.id)), ["kon-e7c1"]);
});

test("the roll-up derives the count, the order, the meter and what holds the epic", () => {
  const state = loadKontoraState();
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();

  const r = state.epicRollup("kon-e7c1");
  assert.equal(r.done, 1);
  assert.equal(r.total, 3);
  assert.equal(r.pct, 33);
  // child_order names two of the three; the third sorts after both, by created.
  assert.deepEqual(vmValue(r.children.map((c) => c.id)), ["kon-c04d", "kon-b12e", "kon-9b31"]);
  assert.deepEqual(vmValue(r.segs.map((s) => s.hue)), ["--st-review", "--st-progress", "--st-done"]);
  // Running is what is moving; review is what stops the epic closing.
  assert.equal(r.next.id, "kon-b12e");
  assert.equal(r.holder.id, "kon-c04d");
  assert.equal(state.epicNote("kon-e7c1"), "kon-c04d holds it");
  assert.equal(state.epicNoteClass("kon-e7c1"), "text-st-review");
  assert.equal(state.epicBreakdown("kon-e7c1"), "1 running · 1 review");
});

// The sub-ticket table reads stage position and wall bounds, which a board
// ticket does not carry: the roll-up derives them the way the daemon's childInfo
// does, so the stage and elapsed columns are not blank for every row.
test("the roll-up gives each child the stage position and the wall the table reads", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "e", title: "E", status: "in_progress", kontora: true, kind: "epic" },
    {
      id: "run", title: "Running", status: "in_progress", kontora: true,
      stage: "implement", stages: ["plan", "implement", "review"],
      created_at: "2026-01-01T00:00:00Z", started_at: "2026-01-03T09:00:00Z",
      parent: { id: "e" },
    },
    {
      id: "fin", title: "Finished", status: "done", kontora: true,
      stage: "review", stages: ["plan", "implement", "review"],
      created_at: "2026-01-02T00:00:00Z", started_at: "2026-01-03T11:00:00Z",
      history: [
        { started_at: "2026-01-03T10:00:00Z", completed_at: "2026-01-03T10:30:00Z" },
        { started_at: "2026-01-03T11:00:00Z", completed_at: "2026-01-03T11:15:00Z" },
      ],
      parent: { id: "e" },
    },
  ];
  state.recomputeBoard();

  const rows = state.epicRollup("e").children;
  const run = rows.find((c) => c.id === "run");
  const fin = rows.find((c) => c.id === "fin");
  assert.equal(state.childStageLine(run), "implement 2/3");
  // The wall spans every stage, so it starts at the first pickup in the
  // history rather than at the frontmatter's current-stage started_at.
  assert.equal(state.childElapsed(fin), state.formatElapsed("2026-01-03T10:00:00Z", "2026-01-03T11:15:00Z"));
  assert.notEqual(state.childElapsed(fin), "\u2014");
});

test("an epic with nothing left to do reads as ready to close", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "e", title: "E", status: "done", kontora: true, kind: "epic" },
    { id: "a", title: "A", status: "done", kontora: true, created_at: "2026-01-01T00:00:00Z", parent: { id: "e" } },
    { id: "b", title: "B", status: "cancelled", kontora: true, created_at: "2026-01-02T00:00:00Z", parent: { id: "e" } },
    // The daemon's derivation counts the legacy "closed" status as finished, so
    // the meter has to agree or it reads 1/3 on an epic the daemon calls done.
    { id: "c", title: "C", status: "closed", kontora: true, created_at: "2026-01-03T00:00:00Z", parent: { id: "e" } },
  ];
  state.recomputeBoard();

  assert.equal(state.epicNote("e"), "ready to close");
  assert.equal(state.epicNoteClass("e"), "text-st-done");
  assert.equal(state.epicRollup("e").done, 2);
  assert.equal(state.epicRollup("e").pct, 67, "a cancelled child is in the total, not in done");
  assert.equal(state.epicRollup("e").holder, null);
});

test("an epic with no children says so rather than reading as finished", () => {
  const state = loadKontoraState();
  state.tickets = [{ id: "e", title: "E", status: "open", kontora: true, kind: "epic" }];
  state.recomputeBoard();

  assert.equal(state.epicNote("e"), "no sub-tickets");
  assert.equal(state.epicRollup("e").pct, 0);
  assert.equal(state.epicBreakdown("e"), "");
});

test("a child's card carries the epic spine and the epic count", () => {
  const { state } = renderedBoard(EPIC_TICKETS);
  const col = state.columns.find((c) => c.key === "in_progress");
  const child = EPIC_TICKETS.find((t) => t.id === "kon-b12e");
  const orphan = { id: "kon-solo", title: "Alone", status: "in_progress", kontora: true };

  const html = state._cardHTML(child, col);
  // A 3px spine in the epic's hue, on its own span so the card's own --pipe-h
  // still paints the pipeline tag.
  assert.match(html, /class="absolute left-0 top-0 bottom-0 w-\[3px\]" data-pipe-color="[a-z]+"/);
  assert.match(html, /pl-\[14px\]/);
  assert.match(html, />Epic: 1 of 3 done</);
  assert.equal(html.includes('data-pipe-color'), true);

  // A ticket in no epic gets neither.
  const plain = state._cardHTML(orphan, col);
  assert.equal(/w-\[3px\]/.test(plain), false);
  assert.equal(/Epic:/.test(plain), false);
  assert.equal(/pl-\[14px\]/.test(plain), false);
});

test("a sibling finishing changes the card signature of every other child", () => {
  const state = loadKontoraState();
  state.tickets = EPIC_TICKETS.map((t) => ({ ...t }));
  state.recomputeBoard();
  const col = state.columns.find((c) => c.key === "in_progress");
  const child = () => state.tickets.find((t) => t.id === "kon-b12e");

  const before = state._cardSig(child(), col);
  state.tickets.find((t) => t.id === "kon-c04d").status = "done";
  state.recomputeBoard();

  assert.notEqual(state._cardSig(child(), col), before,
    "the epic count on this card moved, so its signature has to move with it");
  for (const sibling of state.tickets.filter(t => t.parent?.id === 'kon-e7c1')) {
    const html = state._cardHTML(sibling, state._columnForStatus(sibling.status));
    assert.match(html, />Epic: 2 of 3 done</, 'all siblings share the exact updated rollup label');
  }
});

test("the epics rail row opens the page and draws one block per child", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const section = html.match(/<div class="p-2\.5 pt-2" x-show="epics\.length > 0">[\s\S]*?<\/div>\n      <\/div>/);
  assert.ok(section, "the rail must carry an epics section");
  const rail = section[0];

  assert.match(rail, /epicRollup\(e\.id\)\.done \+ '\/' \+ epicRollup\(e\.id\)\.total/);
  assert.match(rail, /x-for="\(seg, i\) in epicRollup\(e\.id\)\.segs"/);
  assert.match(rail, /:class="epicNoteClass\(e\.id\)" x-text="epicNote\(e\.id\)"/);
  assert.match(rail, /@click="openEpic\(e\.id\)"/);
  assert.match(rail, /:data-pipe-color="pipelineColorByName\(e\.id\)"/);
  // Above projects, which is where the design puts it.
  assert.ok(html.indexOf(rail) < html.indexOf(">projects<"), "epics sit above projects");
});

test("the epic page routes, opens and closes as an overlay over the board", () => {
  const state = loadKontoraState();
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();

  assert.deepEqual(vmValue(state.parseHash("#/e/kon-e7c1")), { view: "board", ticketId: null, epicId: "kon-e7c1" });

  state.openEpic("kon-e7c1");
  assert.equal(state.epicPageOpen(), true);
  assert.equal(state.selectedEpic.id, "kon-e7c1");
  assert.equal(state.routeHash(), "#/e/kon-e7c1");

  state.closeEpic();
  assert.equal(state.epicPageOpen(), false);
  assert.equal(state.routeHash(), "#/");

  // An id that is not an epic opens nothing rather than an empty page.
  state.openEpic("kon-b12e");
  assert.equal(state.epicPageOpen(), false);
});

test("the epic page groups its sub-tickets by status and numbers them", () => {
  const state = loadKontoraState();
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  state.openEpic("kon-e7c1");

  const groups = state.epicGroups();
  assert.deepEqual(vmValue(groups.map((g) => g.key)), ["in_progress", "human_review", "open", "done"]);
  assert.deepEqual(vmValue(groups.find((g) => g.key === "in_progress").rows.map((r) => r.id)), ["kon-b12e"]);
  assert.deepEqual(vmValue(groups.find((g) => g.key === "done").rows.map((r) => r.id)), ["kon-9b31"]);
  // The open group is always drawn: it is where the inline add row lives.
  const open = groups.find((g) => g.key === "open");
  assert.equal(open.count, 0);
  assert.equal(open.addRow, true);
  assert.deepEqual(vmValue(groups.map((g) => g.rows.map((r) => r.ord)).flat()), [1, 1, 1]);
});

test("a drop inside a group writes the epic's children and touches no child", async () => {
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => { posted.push({ url, ...opts }); return { ok: true, json: async () => ({}) }; },
  });
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  await state.openEpic("kon-e7c1");
  // openEpic fetches the brief; only the writes are of interest here.
  posted.length = 0;

  await state.epicSaveOrder(["kon-b12e", "kon-c04d", "kon-9b31"]);

  assert.equal(posted.length, 1);
  assert.equal(posted[0].url, "/api/tickets/kon-e7c1/children");
  assert.equal(posted[0].method, "PUT");
  assert.deepEqual(JSON.parse(posted[0].body), { children: ["kon-b12e", "kon-c04d", "kon-9b31"] });
});

test("pulling a ticket in and moving one out write the child's parent", async () => {
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => { posted.push({ url, ...opts }); return { ok: true, json: async () => ({}) }; },
  });
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  await state.openEpic("kon-e7c1");
  posted.length = 0;

  await state.epicPullIn("kon-solo");
  await state.epicMoveOut("kon-9b31");

  assert.deepEqual(vmValue(posted.map((p) => p.url)), [
    "/api/tickets/kon-solo/parent",
    "/api/tickets/kon-9b31/unparent",
  ]);
  assert.deepEqual(JSON.parse(posted[0].body), { parent: "kon-e7c1" });
  assert.equal(posted[1].body, undefined, "unparent takes no body");
});

test("the pull-in picker offers what could join the epic and nothing else", () => {
  const state = loadKontoraState();
  state.tickets = [
    ...EPIC_TICKETS,
    { id: "kon-solo", title: "Alone", status: "open", kontora: true },
    { id: "kon-e999", title: "Another epic", status: "open", kontora: true, kind: "epic" },
  ];
  state.recomputeBoard();
  state.openEpic("kon-e7c1");
  state.openPaletteScope("epic-pull");

  const rows = state.paletteGroups.flatMap((g) => g.items);
  const offered = rows.map((r) => r.ticketId);
  assert.ok(offered.includes("kon-solo"));
  // An epic cannot be a child, the epic itself is not a candidate, and a ticket
  // already in it has nothing to pull.
  assert.equal(offered.includes("kon-e999"), false);
  assert.equal(offered.includes("kon-e7c1"), false);
  assert.equal(offered.includes("kon-b12e"), false);
  assert.equal(rows.every((r) => r.kind === "epicpull"), true);
});

test("the inline add row posts one sub-ticket under the epic", async () => {
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => { posted.push({ url, ...opts }); return { ok: true, json: async () => ({}) }; },
  });
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  await state.openEpic("kon-e7c1");
  posted.length = 0;

  // An empty draft posts nothing, so a stray Enter does not create a ticket.
  await state.epicAddChild();
  assert.equal(posted.length, 0);

  state.epicAddDraft = "  A new sub-ticket  ";
  await state.epicAddChild();

  assert.equal(posted.length, 1);
  assert.equal(posted[0].url, "/api/tickets");
  assert.deepEqual(JSON.parse(posted[0].body), {
    title: "A new sub-ticket",
    path: "/r/kontora",
    parent: "kon-e7c1",
    status: "open",
  });
  assert.equal(state.epicAddDraft, "", "a landed post clears the draft");
});

test("the create preview drops the fields an epic does not have", () => {
  const state = loadKontoraState();
  // Everything the form may still hold from before the kind was switched.
  state.createForm = {
    ...state.createForm, title: "An epic", path: "/r/kontora", pipeline: "default",
    agent: "claude", status: "todo", kind: "epic", scheduleMode: "later", scheduled_at: "2099-01-01 09:00",
  };

  const yaml = state.createPreviewYaml;
  assert.match(yaml, /kind: epic/);
  assert.match(yaml, /status: open/);
  assert.equal(/pipeline:/.test(yaml), false);
  assert.match(yaml, /agent: claude/);
  assert.equal(/epic_execution:/.test(yaml), false);
  assert.equal(/scheduled_at:/.test(yaml), false);
});

test("creating an epic selects its agent without activating supervision", async () => {
  const posted = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => { posted.push({ url, ...opts }); return { ok: true, json: async () => ({ id: "kon-e7c1" }) }; },
  });
  state.configCache = {};
  for (const schedule of ["2099-01-01 09:00", "not a date", ""]) {
    for (const choice of ["inherit", "none", "pipeline:default"]) {
      await state.openCreateModal();
      Object.assign(state.createForm, {
        title: "An epic", path: "/r/kontora", body: "## Description\n",
        pipeline: "default", agent: "claude", status: "todo", kind: "epic",
        scheduleMode: "later", scheduled_at: schedule, pipelineChoice: choice,
      });
      assert.equal(state.createScheduleISO(), "");
      assert.equal(state.createScheduleError(), "");
      assert.equal(state.createSchedulePinned(), false);
      assert.equal(state.createScheduleEcho(), null);
      assert.equal(state.createBlocked(), false);
      assert.equal(await state.submitCreateTicket(), true);
      assert.equal(state.createSubmitting, false);
    }
  }

  assert.equal(posted.length, 9);
  for (const request of posted) {
    assert.equal(request.url, "/api/tickets");
    assert.deepEqual(JSON.parse(request.body), {
      title: "An epic", path: "/r/kontora", kind: "epic", status: "open", agent: "claude", body: "## Description\n",
    });
  }
});

test("switching from an epic restores the ticket schedule without activating the epic", async () => {
  const { state, posted } = scheduleState();
  await createLater(state, SCHED_LOCAL);
  state.createForm.kind = "epic";
  state.onCreateScheduleChange();
  assert.equal(state.createForm.status, "open");
  assert.equal(state.createScheduleISO(), "");
  assert.equal(state.createBlocked(), false);
  assert.doesNotMatch(state.createPreviewYaml, /scheduled_at:|pipeline:|epic_execution:/);
  state.createForm.kind = "";
  state.onCreateScheduleChange();
  assert.equal(state.createScheduleISO(), SCHED_ISO);
  assert.equal(state.createSchedulePinned(), true);
  assert.equal(await state.submitCreateTicket(), true);
  assert.equal(posted[0].body.scheduled_at, SCHED_ISO);
  assert.equal(posted[0].body.status, "open");
});

test("the brief seeds the description editor and saves only the description", async () => {
  const seen = [];
  const state = loadKontoraState({
    fetch: async (url, opts) => {
      seen.push({ url, ...opts });
      if (!opts || !opts.method || opts.method === "GET") {
        return { ok: true, json: async () => ({ ...EPIC_TICKETS[0], body: "\n# Epics\n\n## Description\n\nWhy.\n", notes: [] }) };
      }
      return { ok: true, json: async () => ({ ...EPIC_TICKETS[0], body: JSON.parse(opts.body).description }) };
    },
  });
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  await state.openEpic("kon-e7c1");

  // The leading newline is held aside, so a round-trip through the editor does
  // not move the first line.
  assert.equal(state.editForm.body, "# Epics\n\n## Description\n\nWhy.\n");
  assert.equal(state.epicEditingBrief, false);

  seen.length = 0;
  state.epicEditingBrief = true;
  state.editForm.body = "# Epics\n\n## Description\n\nBecause.\n";
  await state.epicSaveBrief();

  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, "/api/tickets/kon-e7c1");
  assert.equal(seen[0].method, "PUT");
  assert.deepEqual(JSON.parse(seen[0].body), { description: "\n# Epics\n\n## Description\n\nBecause.\n" });
  assert.equal(state.selectedEpic.body, "\n# Epics\n\n## Description\n\nBecause.\n");
  assert.equal(state.epicBriefSaved, true);

  // A save with nothing changed writes nothing: the debounce fires on every
  // keystroke burst, and an unchanged body is not an edit.
  seen.length = 0;
  await state.epicSaveBrief();
  assert.equal(seen.length, 0);
});

test("opening a ticket replaces the epic page rather than stacking on it", async () => {
  const state = loadKontoraState({
    fetch: async (url) => ({
      ok: true,
      json: async () => (String(url).includes("kon-e7c1")
        ? { ...EPIC_TICKETS[0], body: "", notes: [] }
        : { id: "kon-b12e", status: "in_progress", body: "" }),
    }),
  });
  state.tickets = EPIC_TICKETS;
  state.recomputeBoard();
  await state.openEpic("kon-e7c1");
  assert.equal(state.epicPageOpen(), true);

  await state.selectTicket({ id: "kon-b12e", status: "in_progress" });
  await flushMicrotasks();

  // Both pages carry a body editor, and only one of them can own the refs
  // behind it.
  assert.equal(state.epicPageOpen(), false);
  assert.equal(state.selectedTicket.id, "kon-b12e");
});

function epicSupervisorUI({ mobile = false, status = 'in_progress', mode = 'agent', overrides = {} } = {}) {
  const requests = [], sockets = [];
  const epic = { id: 'kon-e7c1', title: 'Supervised epic', kind: 'epic', kontora: true, path: '/r/kontora',
    status, epic_execution: mode, agent: 'claude', agent_override: true,
    stage: mode === 'agent' ? 'default' : '', stages: mode === 'agent' ? ['default'] : [],
    body: '\n# Brief\n', branch: 'epic/integration', history: [] };
  const location = { protocol: 'http:', host: 'localhost:8080', hash: '#/' };
  const containers = Object.fromEntries([
    'terminal-session', 'terminal-container-mobile', 'epic-terminal-session', 'epic-terminal-mobile',
    'activity-scroll', 'epic-activity-scroll', 'epic-activity-scroll-mobile',
  ].map(id => [id, { textContent: '', scrollTop: 0, scrollHeight: 1000 }]));
  const { ctx, state } = loadKontoraContext({
    location,
    document: {
      getElementById: id => containers[id] || null,
      querySelector: () => null,
      querySelectorAll: () => [],
      hasFocus: () => true,
      documentElement: { style: {} },
    },
    WebSocket: class {
      static OPEN = 1;
      constructor(url) { this.url = url; this.readyState = 1; this.closed = false; sockets.push(this); }
      send() {}
      close() { this.closed = true; this.readyState = 3; }
    },
    fetch: async (url, opts = {}) => {
      requests.push({ url, ...opts });
      if (opts.method === 'PUT') {
        Object.assign(epic, JSON.parse(opts.body));
        if (typeof epic.description === 'string') epic.body = epic.description;
      }
      if (url.endsWith('/init')) Object.assign(epic, { status: 'todo', epic_execution: 'agent' });
      if (url.endsWith('/pause')) epic.status = 'paused';
      if (url.endsWith('/retry')) epic.status = 'todo';
      if (url.endsWith('/cost')) return { ok: true, json: async () => ({ id: epic.id, tracked_runs: 0 }) };
      if (url.endsWith('/chain')) return { ok: true, json: async () => ({ id: epic.id, nodes: [] }) };
      if (url.endsWith('/changes')) return { ok: true, json: async () => ({ files: [], commits: [] }) };
      if (url.endsWith('/logs')) return { ok: true, json: async () => ({ content: 'supervisor log' }) };
      if (url.includes('/activity')) return { ok: true, headers: { get: () => null }, json: async () => ({ source: 'log', content: 'supervisor activity' }) };
      return { ok: true, json: async () => ({ ...epic }) };
    },
    ...overrides,
  });
  stubXterm(ctx.termState);
  state.isMobile = mobile;
  state.$nextTick = callback => { if (callback) callback(); return Promise.resolve(); };
  state.configCache = { agents: ['claude'], default_agent: 'claude', pipelines: ['build'],
    pipeline_infos: [{ name: 'build', stages: ['code'], default_agent: 'worker' }],
    projects: [{ name: 'repo', path: epic.path, pipeline: 'build', agent: 'claude' }] };
  const slimEpic = state.boardEntry(epic);
  // /api/tickets is deliberately slimmer than detail and SSE payloads.
  for (const key of ['history', 'last_error', 'last_log']) delete slimEpic[key];
  state.tickets = [slimEpic, ...['child-a', 'child-b', 'child-c'].map(id => ({
    id, title: id, kontora: true, agent: 'claude', status: 'open', path: epic.path, parent: { id: epic.id },
  }))];
  state.recomputeBoard();
  return { state, ctx, location, epic, requests, sockets, containers };
}

// Keep activity retries deterministic without leaving real poll timers alive.
function supervisorClock() {
  const timers = new Map();
  let next = 1;
  return {
    timers,
    overrides: {
      setTimeout(fn, ms) { const id = next++; timers.set(id, { fn, ms }); return id; },
      clearTimeout(id) { timers.delete(id); },
    },
    async tick(id) {
      const timer = timers.get(id);
      assert.ok(timer, 'expected an armed timer');
      timers.delete(id);
      timer.fn();
      await flushMicrotasks();
    },
  };
}

for (const mobile of [false, true]) {
  test(`embedded epic watch keeps the epic, route and brief draft: mobile=${mobile}`, async () => {
    const { state, ctx, epic, location, requests, sockets, containers } = epicSupervisorUI({ mobile, status: 'paused' });
    await state.openEpic(epic.id);
    state.epicEditingBrief = true;
    state.editForm.body = 'Do not flush this draft';
    state.epicDebounceBrief();
    const debounce = state._epicBriefDebounce;
    state.applyTicketUpdate({ ...epic, status: 'in_progress' });
    // Running briefs are normally flushed by recompute; this test isolates watch.
    state.epicEditingBrief = true;
    requests.length = 0;
    state.epicWatchAgent();
    await flushMicrotasks();
    assert.equal(state.selectedEpic.id, epic.id);
    assert.equal(state.selectedTicket, null);
    assert.equal(location.hash, '#/e/' + epic.id);
    assert.equal(state.outputSubject(), state.selectedEpic);
    assert.equal(state.outputTab(), 'session');
    assert.equal(state.epicTab, 'session');
    assert.equal(state.terminalWanted(), true);
    assert.equal(state.editForm.body, 'Do not flush this draft');
    assert.equal(state._epicBriefDebounce, debounce, 'watch neither cancels nor flushes a pending save');
    assert.equal(requests.length, 0);
    const terminalID = mobile ? 'epic-terminal-mobile' : 'epic-terminal-session';
    const activityID = mobile ? 'epic-activity-scroll-mobile' : 'epic-activity-scroll';
    assert.equal(state.outputContainerID('terminal'), terminalID);
    assert.equal(state.outputContainerID('activity'), activityID);
    assert.equal(ctx.termState.term.element.parentNode, containers[terminalID]);
    assert.equal(sockets.length, 1);
    assert.match(sockets[0].url, new RegExp('/ws/terminal/' + epic.id + '\\?'));
    clearTimeout(debounce);
    state._epicBriefDebounce = null;
    state.destroy();
  });

  test(`passive epic never exposes output or attaches a terminal: mobile=${mobile}`, async () => {
    const { state, epic, sockets, requests } = epicSupervisorUI({ mobile, mode: '' });
    await state.openEpic(epic.id);
    requests.length = 0;
    state.epicWatchAgent();
    state.epicSwitchTab('session');
    state.epicSwitchTab('activity');
    await state.openTerminal();
    assert.equal(state.epicTab, 'children');
    assert.equal(state.outputSubject(), null);
    assert.equal(!!state.terminalWanted(), false);
    assert.deepEqual(vmValue(state.outputRuns()), []);
    assert.equal(sockets.length, 0);
    assert.equal(requests.length, 0);
    state.applyTicketUpdate({ ...epic, epic_execution: 'agent', status: 'in_progress', stage: 'default' });
    state.recomputeBoard();
    state.epicWatchAgent();
    await flushMicrotasks();
    state.epicSwitchTab('activity');
    await flushMicrotasks();
    assert.equal(sockets.length, 1);
    state.applyTicketUpdate({ ...epic });
    state.recomputeBoard();
    assert.equal(sockets[0].closed, true);
    assert.equal(state.outputSubject(), null);
    assert.equal(state._activityPoll, null);
    state.destroy();
  });

  for (const { status, history, error, message } of [
    { status: 'open', history: [], message: /has not recorded a run/ },
    { status: 'todo', history: [], message: /queued.*when it starts/ },
    { status: 'paused', history: [], error: 'startup failed', message: /failed before recording a run/ },
    { status: 'human_review', history: [{ stage: 'default', run: 0 }], message: /No live session.*recorded run/ },
  ]) {
    test(`epic session names its no-run state without connecting: mobile=${mobile}, status=${status}`, async () => {
      const { state, epic, sockets } = epicSupervisorUI({ mobile, status });
      epic.history = history;
      epic.last_error = error;
      await state.openEpic(epic.id);
      state.epicSwitchTab('session');
      await flushMicrotasks();
      await state.openTerminal();
      assert.match(state.outputNoRunText(), message);
      assert.equal(state.terminalWanted(), false);
      assert.equal(state.terminalOpen, false);
      assert.equal(state.selectedTicket, null);
      assert.equal(sockets.length, 0);
      state.destroy();
    });
  }

  for (const failure of ['http', 'network']) {
    test(`epic detail reports ${failure} failure without abandoning the overview: mobile=${mobile}`, async () => {
      const { state, ctx, epic, location } = epicSupervisorUI({ mobile, status: 'paused' });
      ctx.fetch = async () => {
        if (failure === 'network') throw new Error('offline');
        return { ok: false };
      };
      await state.openEpic(epic.id);
      assert.equal(state.error, 'Failed to load epic details');
      assert.equal(state.selectedEpic.id, epic.id);
      assert.equal(state.selectedTicket, null);
      assert.equal(location.hash, '#/e/' + epic.id);
      assert.equal(state.epicCanEditBrief(), false);
      state.destroy();
    });
  }

  test(`queued epic session attaches when readiness precedes the buffered running update: mobile=${mobile}`, async () => {
    const frames = [];
    const { state, epic, sockets } = epicSupervisorUI({ mobile, status: 'todo', overrides: {
      requestAnimationFrame: fn => { frames.push(fn); return frames.length; },
      cancelAnimationFrame() {},
    } });
    await state.openEpic(epic.id);
    state.epicSwitchTab('session');
    state.connectSSE();
    state._eventSource.emit('ticket_updated', { ...epic, status: 'in_progress' });
    state._eventSource.emit('terminal_ready', { id: epic.id });
    while (frames.length) frames.shift()();
    await flushMicrotasks();
    while (frames.length) frames.shift()();
    assert.equal(state.terminalWanted(), true);
    assert.equal(sockets.length, 1);
    assert.match(sockets[0].url, new RegExp('/ws/terminal/' + epic.id + '\\?'));
    state.destroy();
  });

  for (const status of ['paused', 'done', 'human_review']) {
    test(`stopped epic activity loads when initial history arrives: mobile=${mobile}, status=${status}`, async () => {
      const { state, ctx, epic, requests } = epicSupervisorUI({ mobile, status });
      epic.history = [{ stage: 'default', run: 0 }];
      const detail = deferred();
      const originalFetch = ctx.fetch;
      ctx.fetch = async (url, opts) => {
        if (url === '/api/tickets/' + epic.id) await detail.promise;
        return originalFetch(url, opts);
      };
      const opening = state.openEpic(epic.id);
      state.epicSwitchTab('activity');
      assert.equal(state.activityStage, null);
      detail.resolve();
      await opening;
      await flushMicrotasks();
      assert.equal(state.outputRunKey(), 'default#0');
      assert.equal(state.activity.content, 'supervisor activity');
      assert.equal(requests.filter(r => r.url.includes('/activity')).length, 1);
      assert.equal(state._activityPoll, null);
      state.destroy();
    });
  }

  for (const tab of ['session', 'activity']) {
    test(`history before final status keeps the completed epic attempt: mobile=${mobile}, tab=${tab}`, async () => {
      const clock = supervisorClock();
      const { state, epic, requests, sockets } = epicSupervisorUI({ mobile, overrides: clock.overrides });
      await state.openEpic(epic.id);
      state.epicSwitchTab(tab);
      await flushMicrotasks();
      requests.length = 0;
      const finished = { ...epic, history: [{ stage: 'default', run: 0, exit_code: 0 }] };
      state.applyTicketUpdate(finished);
      await flushMicrotasks();
      state.applyTicketUpdate({ ...finished, title: 'A later note update' });
      await flushMicrotasks();
      state.applyTicketUpdate({ ...finished, status: 'human_review' });
      await flushMicrotasks();
      assert.equal(state.epicTab, 'activity');
      assert.equal(state.outputRunKey(), 'default#0');
      assert.equal(state.activity.content, 'supervisor activity');
      assert.ok(requests.some(r => r.url.includes('/activity?stage=default&run=0')));
      assert.ok(requests.every(r => !r.url.endsWith('run=1')), 'completion cannot request an unrecorded next attempt');
      assert.equal(state._activityPoll, null);
      assert.ok(sockets.every(s => s.closed));
      state.destroy();
    });
  }

  for (const status of ['todo', 'paused']) {
    test(`same-status epic updates preserve a session with no recorded run: mobile=${mobile}, status=${status}`, async () => {
      const { state, epic, requests } = epicSupervisorUI({ mobile, status });
      if (status === 'paused') epic.last_error = 'startup failed';
      await state.openEpic(epic.id);
      state.epicSwitchTab('session');
      const explanation = state.outputNoRunText();
      requests.length = 0;
      state.applyTicketUpdate({ ...epic, notes: [{ text: 'new note' }] });
      await flushMicrotasks();
      assert.equal(state.epicTab, 'session');
      assert.equal(state.outputNoRunText(), explanation);
      assert.equal(state.activityStage, null);
      assert.equal(state.activityError, null);
      assert.equal(requests.length, 0);
      state.destroy();
    });
  }

  test(`epic output startup failure retries and terminal_ready refreshes activity: mobile=${mobile}`, async () => {
    const clock = supervisorClock();
    const { state, ctx, epic } = epicSupervisorUI({ mobile, overrides: clock.overrides });
    await state.openEpic(epic.id);
    let reads = 0;
    ctx.fetch = async url => {
      assert.match(url, new RegExp('/api/tickets/' + epic.id + '/activity\\?stage=default&run=0'));
      reads++;
      if (reads === 1) return { ok: false, json: async () => ({ error: 'session not created yet' }) };
      return { ok: true, headers: { get: () => null }, json: async () => ({ source: 'log', content: 'ready ' + reads }) };
    };
    state.epicSwitchTab('activity');
    await flushMicrotasks();
    assert.equal(state.activityError, 'session not created yet');
    assert.equal(clock.timers.get(state._activityPoll).ms, 2000);
    await clock.tick(state._activityPoll);
    assert.equal(state.activity.content, 'ready 2');
    assert.equal(state.activityError, null);
    assert.ok(state._activityPoll !== null, 'running status polls even before the payload reports live');
    state.connectSSE();
    state._eventSource.emit('terminal_ready', { id: 'child-a' });
    await flushMicrotasks();
    assert.equal(reads, 2, 'another ticket cannot refresh this output');
    state._eventSource.emit('terminal_ready', { id: epic.id });
    await flushMicrotasks();
    assert.equal(reads, 3);
    assert.equal(state.activity.content, 'ready 3');
    state.epicSwitchTab('children');
    assert.equal(state._activityPoll, null);
    state.destroy();
    assert.equal(clock.timers.size, 0);
  });
}

for (const mobile of [false, true]) for (const structured of [false, true]) {
  test(`activity Raw ignores a different cached Logs stage: mobile=${mobile}, structured=${structured}`, async () => {
    const opened = [], blobs = [], reads = [];
    const { state } = loadKontoraContext({
      Blob,
      URL: { createObjectURL: blob => { blobs.push(blob); return 'blob:activity-log'; } },
      window: { open: url => opened.push(url) },
      fetch: async url => {
        reads.push(url);
        return { ok: true, json: async () => ({ content: 'code raw output' }) };
      },
    });
    Object.assign(state, { isMobile: mobile, selectedTicket: { id: 'kon-1', status: 'in_progress', stage: 'code' },
      detailTab: 'activity', activeTab: 'activity', activityStage: 'code', activityRun: 0,
      activity: structured ? { tape: { events: [] } } : { content: 'code plaintext output' },
      logViewStage: 'plan', logViewContent: 'plan cached output' });

    await state.openRawLog();

    assert.deepEqual(opened, ['blob:activity-log']);
    assert.equal(await blobs[0].text(), structured ? 'code raw output' : 'code plaintext output');
    assert.deepEqual(reads, structured ? ['/api/tickets/kon-1/logs?stage=code'] : []);
  });
}

for (const owner of ['worker', 'epic']) {
  for (const pollStatus of [200, 304]) {
    for (const delayed of ['response', 'json']) {
      test(`raw ${owner} output survives a same-run ${pollStatus} activity poll during ${delayed}`, async () => {
        const clock = supervisorClock();
        const opened = [];
        const { state, ctx, epic } = epicSupervisorUI({ overrides: { ...clock.overrides,
          Blob, URL: { createObjectURL: () => 'blob:raw-output' },
        } });
        await state.openEpic(epic.id);
        if (owner === 'worker') {
          state.selectedEpic = null;
          state.selectedTicket = { ...epic, kind: 'ticket' };
          state.activeTab = 'activity';
        } else state.epicSwitchTab('activity', false);
        ctx.window.open = url => opened.push(url);
        const gate = deferred();
        let reads = 0;
        ctx.fetch = async url => {
          if (url.includes('/logs')) {
            if (delayed === 'response') await gate.promise;
            return { ok: true, json: async () => {
              if (delayed === 'json') await gate.promise;
              return { content: 'raw stage output' };
            } };
          }
          reads++;
          return { ok: true, status: reads > 1 ? pollStatus : 200, headers: { get: () => 'etag' },
            json: async () => ({ source: 'events', live: true, tape: { events: [] } }) };
        };
        await state.fetchActivity('default', 0);
        const raw = state.openRawLog();
        await flushMicrotasks();
        await clock.tick(state._activityPoll);
        assert.equal(reads, 2);
        gate.resolve();
        await raw;
        assert.deepEqual(opened, ['blob:raw-output']);
        assert.equal(state.error, null);
        state.destroy();
      });
    }
  }
}

for (const mobile of [false, true]) {
  for (const supersession of ['same run', 'tab round trip', 'run round trip', 'close/reopen', 'destroy']) {
    for (const ok of [true, false]) {
      test(`raw epic output ignores stale ${ok ? 'success' : 'failure'} after ${supersession}: mobile=${mobile}`, async () => {
        const clock = supervisorClock();
        const opened = [];
        const { state, ctx, epic } = epicSupervisorUI({ mobile, overrides: { ...clock.overrides,
          Blob, URL: { createObjectURL: () => 'blob:stale-output' },
        } });
        epic.history = [{ stage: 'default', run: 0 }];
        await state.openEpic(epic.id);
        ctx.window.open = url => opened.push(url);
        const originalFetch = ctx.fetch;
        const gate = deferred();
        ctx.fetch = async (url, opts) => {
          if (!url.includes('/logs')) return originalFetch(url, opts);
          await gate.promise;
          return { ok, json: async () => ({ content: 'stale raw output' }) };
        };
        state.epicSwitchTab('activity', false);
        await state.fetchActivity('default', 0);
        state.activity = { tape: { events: [] } };
        const raw = state.openRawLog();
        if (supersession === 'same run') state.selectOutputRun('default#0');
        else if (supersession === 'tab round trip') {
          state.epicSwitchTab('children');
          state.epicSwitchTab('activity', false);
        } else if (supersession === 'run round trip') {
          state.selectOutputRun('default#1');
          state.selectOutputRun('default#0');
        } else if (supersession === 'close/reopen') {
          await state.closeEpic();
          await state.openEpic(epic.id);
          state.epicSwitchTab('activity', false);
          await state.fetchActivity('default', 0);
        } else state.destroy();
        await flushMicrotasks();
        gate.resolve();
        await raw;
        assert.deepEqual(opened, []);
        assert.equal(state.error, null);
        state.destroy();
      });
    }
  }
}

for (const recordedRun of [false, true]) {
  test(`epic detail retains errors across slim board recomputes: recordedRun=${recordedRun}`, async () => {
    const { state, epic } = epicSupervisorUI({ status: 'paused' });
    epic.history = recordedRun ? [{ stage: 'default', run: 0, exit_code: 1 }] : [];
    epic.last_error = 'supervisor failed';
    epic.last_log = 'failure log';
    assert.equal('history' in state.tickets[0], false);
    assert.equal('last_error' in state.tickets[0], false);
    assert.equal('last_log' in state.tickets[0], false);
    await state.openEpic(epic.id);
    state.recomputeBoard();
    assert.deepEqual(vmValue(state.selectedEpic.history), epic.history);
    assert.equal(state.selectedEpic.last_error, epic.last_error);
    assert.equal(state.selectedEpic.last_log, epic.last_log);
    assert.deepEqual(vmValue(state.outputRuns().map(r => r.key)), recordedRun ? ['default#0'] : []);
    if (!recordedRun) assert.match(state.outputNoRunText(), /failed before recording/);
    state.destroy();
  });
}

test('epic detail overtaken by SSE repeats without restoring a cleared error or replacing a brief edit', async () => {
  const { state, ctx, epic } = epicSupervisorUI({ status: 'paused' });
  epic.last_error = 'old failure';
  await state.openEpic(epic.id);
  state.epicEditingBrief = true;
  state.editForm.body = 'Local brief in progress';
  const stale = { ...epic, history: [], last_error: 'old failure', body: 'stale server brief' };
  const current = { ...epic, history: [{ stage: 'default', run: 0 }], last_error: '', last_log: '', body: 'remote brief' };
  const gate = deferred();
  let reads = 0;
  ctx.fetch = async () => ({ ok: true, json: async () => {
    if (++reads === 1) { await gate.promise; return stale; }
    return current;
  } });
  const loading = state.epicFetchDetail(epic.id);
  await flushMicrotasks();
  state.applyTicketUpdate(current);
  state.recomputeBoard();
  assert.equal(state.selectedEpic.last_error, '');
  gate.resolve();
  await loading;
  assert.equal(reads, 2, 'SSE invalidates the in-flight detail snapshot');
  state.recomputeBoard();
  assert.equal(state.selectedEpic.last_error, '');
  assert.equal(state.selectedEpic.last_log, '');
  assert.deepEqual(vmValue(state.selectedEpic.history), current.history);
  assert.equal(state.epicEditingBrief, true);
  assert.equal(state.editForm.body, 'Local brief in progress');
  const withoutErrors = { ...current };
  delete withoutErrors.last_error;
  delete withoutErrors.last_log;
  state.applyTicketUpdate(withoutErrors);
  state.applyTicketUpdate({ ...state.tickets[1], status: 'done' });
  state.recomputeBoard();
  assert.equal(state.selectedEpic.last_error, undefined, 'omitted optional fields clear previous errors');
  assert.equal(state.selectedEpic.last_log, undefined);
  assert.deepEqual(vmValue(state.selectedEpic.history), current.history);
  assert.equal(state.editForm.body, 'Local brief in progress');
  state.destroy();
});

for (const mobile of [false, true]) {
  for (const supersession of ['close/reopen', 'tab', 'run', 'destroy']) {
    for (const delayed of ['response', 'json', 'error json']) {
      test(`epic activity generation drops delayed ${delayed} after ${supersession}: mobile=${mobile}`, async () => {
        const clock = supervisorClock();
        const { state, ctx, epic } = epicSupervisorUI({ mobile, overrides: clock.overrides });
        epic.history = [{ stage: 'default', run: 0 }];
        await state.openEpic(epic.id);
        const gate = deferred();
        const originalFetch = ctx.fetch;
        let reads = 0;
        ctx.fetch = async (url, opts) => {
          if (!url.includes('/activity')) return originalFetch(url, opts);
          if (++reads !== 1) return { ok: true, headers: { get: () => null }, json: async () => ({ source: 'log', content: 'current output' }) };
          if (delayed === 'response') await gate.promise;
          return {
            ok: delayed !== 'error json', headers: { get: () => 'stale-etag' },
            json: async () => {
              if (delayed !== 'response') await gate.promise;
              return delayed === 'error json' ? { error: 'stale error' } : { source: 'log', content: 'stale output', live: true };
            },
          };
        };
        state.epicSwitchTab('activity', false);
        const first = state.fetchActivity('default', 0);
        await flushMicrotasks();
        if (supersession === 'close/reopen') {
          await state.closeEpic();
          await state.openEpic(epic.id);
          state.epicSwitchTab('activity', false);
          await state.fetchActivity('default', 1);
        } else if (supersession === 'run') {
          state.selectOutputRun('default#1');
          await flushMicrotasks();
        } else if (supersession === 'destroy') state.destroy();
        else state.epicSwitchTab('children');
        const poll = state._activityPoll;
        gate.resolve();
        await first;
        assert.equal(state.selectedEpic.id, epic.id);
        assert.equal(state.selectedTicket, null);
        assert.equal(state.activityError, null);
        assert.equal(state.activityLoading, false);
        assert.equal(state._activityPoll, poll, 'stale responses cannot re-arm the poll');
        if (supersession === 'tab' || supersession === 'destroy') {
          assert.equal(state.activity, null);
          if (supersession === 'tab') assert.equal(state.epicTab, 'children');
          else assert.equal(state._appDestroyed, true);
          assert.equal(poll, null);
        } else {
          assert.equal(state.activity.content, 'current output');
          assert.equal(state.outputRunKey(), 'default#1');
        }
        state.destroy();
        assert.equal(clock.timers.size, 0);
      });
    }
  }

  test(`detail hydration corrects an automatic activity target while its first read is pending: mobile=${mobile}`, async () => {
    const clock = supervisorClock();
    const { state, ctx, epic } = epicSupervisorUI({ mobile, overrides: clock.overrides });
    epic.history = [{ stage: 'default', run: 0 }];
    const detail = deferred(), oldRead = deferred(), currentRead = deferred();
    ctx.fetch = async url => {
      if (!url.includes('/activity')) {
        await detail.promise;
        return { ok: true, json: async () => ({ ...epic }) };
      }
      const old = url.endsWith('run=0');
      await (old ? oldRead : currentRead).promise;
      return { ok: true, headers: { get: () => null }, json: async () => ({ content: old ? 'old run' : 'current run', live: !old }) };
    };
    const opening = state.openEpic(epic.id);
    state.epicSwitchTab('activity');
    await flushMicrotasks();
    assert.equal(state.outputRunKey(), 'default#0');
    detail.resolve();
    await opening;
    assert.equal(state.outputRunKey(), 'default#1');
    assert.equal(state.activityLoading, true);
    oldRead.resolve();
    await flushMicrotasks();
    assert.equal(state.activityLoading, true, 'an older read cannot finish the current loading state');
    currentRead.resolve();
    await flushMicrotasks();
    assert.equal(state.activityLoading, false);
    assert.equal(state.activity.content, 'current run');
    state.destroy();
  });

  for (const pendingActivity of [false, true]) {
    test(`epic completion, retry and historical selection keep run identities: mobile=${mobile}, pendingActivity=${pendingActivity}`, async () => {
      const clock = supervisorClock();
      const { state, ctx, epic, requests, sockets } = epicSupervisorUI({ mobile, overrides: clock.overrides });
      await state.openEpic(epic.id);
      state.epicWatchAgent();
      await flushMicrotasks();
      const socket = sockets[0];
      const pending = deferred();
      if (pendingActivity) {
        const originalFetch = ctx.fetch;
        let reads = 0;
        ctx.fetch = async (url, opts) => {
          if (url.includes('/activity') && ++reads === 1) await pending.promise;
          return originalFetch(url, opts);
        };
        state.epicSwitchTab('activity');
        await flushMicrotasks();
        assert.equal(state.activityLoading, true);
      }
      const history = [{ stage: 'default', run: 0, exit_code: 1 }];
      Object.assign(epic, { status: 'paused', history, last_error: 'run failed' });
      state.applyTicketUpdate({ ...epic });
      state.recomputeBoard();
      await flushMicrotasks();
      assert.equal(socket.closed, true);
      assert.equal(state.epicTab, 'activity');
      assert.equal(state.outputRunKey(), 'default#0');
      assert.equal(state.activityLoading, false);
      pending.resolve();
      await flushMicrotasks();
      assert.equal(state.activityLoading, false);
      assert.equal(state._activityPoll, null);
      assert.deepEqual(vmValue(state.outputRuns().map(r => r.key)), ['default#0']);
      await state.epicAction('retry');
      assert.equal(state.selectedEpic.status, 'todo');
      assert.match(state.outputNoRunText(), /queued/i);
      state.applyTicketUpdate({ ...epic, status: 'in_progress', last_error: '' });
      state.recomputeBoard();
      assert.deepEqual(vmValue(state.outputRuns().map(r => r.key)), ['default#1', 'default#0']);
      requests.length = 0;
      state.selectOutputRun('default#0');
      await flushMicrotasks();
      assert.equal(state.outputRunKey(), 'default#0');
      assert.equal(state._activityPoll, null, 'historical runs do not poll a newer live run');
      const beforeReady = requests.length;
      state.outputReady(epic.id);
      await flushMicrotasks();
      assert.equal(requests.length, beforeReady, 'ready cannot replace an explicitly selected historical run');
      state.selectOutputRun('default#1');
      await flushMicrotasks();
      assert.equal(state.outputRunKey(), 'default#1');
      assert.ok(requests.at(-1).url.endsWith('/activity?stage=default&run=1'));
      assert.ok(state._activityPoll !== null);
      state.epicWatchAgent();
      await flushMicrotasks();
      assert.equal(state.epicTab, 'session');
      assert.equal(sockets.filter(s => !s.closed).length, 1);
      assert.equal(state._activityPoll, null);
      assert.equal(state.outputRunKey(), 'default#1', 'the live session selector must not label a historical run');
      state.destroy();
    });
  }

  test(`epic socket disconnect requires explicit reconnect and ignores stale callbacks: mobile=${mobile}`, async () => {
    const { state, ctx, epic, sockets } = epicSupervisorUI({ mobile });
    await state.openEpic(epic.id);
    state.epicWatchAgent();
    await flushMicrotasks();
    const first = sockets[0];
    first.onopen();
    assert.equal(state.terminalConnected, true);
    first.close();
    first.onclose();
    assert.equal(state.terminalConnected, false);
    assert.match(state.terminalError, /disconnected.*retry/i);
    assert.equal(ctx.termState.ws, null);
    assert.equal(sockets.length, 1, 'disconnect does not spin an automatic retry');
    state.reconnectTerminal();
    await flushMicrotasks();
    assert.equal(sockets.length, 2);
    sockets[1].onopen();
    first.onclose();
    assert.equal(state.terminalConnected, true);
    assert.equal(state.terminalError, null);
    assert.equal(ctx.termState.ws, sockets[1]);
    state.connectSSE();
    state._eventSource.emit('terminal_ready', { id: 'child-a' });
    await flushMicrotasks();
    assert.equal(sockets.length, 2);
    state._eventSource.emit('terminal_ready', { id: epic.id });
    await flushMicrotasks();
    assert.equal(sockets.length, 3, 'ready for the selected supervisor reconnects its session');
    assert.equal(sockets[1].closed, true);
    state.destroy();
    assert.equal(sockets[2].closed, true);
    assert.equal(ctx.termState.term, null);
  });

  test(`epic terminal opening generation survives close and reopen of the same ID: mobile=${mobile}`, async () => {
    const { state, ctx, epic, sockets } = epicSupervisorUI({ mobile });
    await state.openEpic(epic.id);
    const gate = deferred();
    let ticks = 0;
    state.$nextTick = callback => {
      if (callback) { callback(); return Promise.resolve(); }
      return ++ticks === 1 ? gate.promise : Promise.resolve();
    };
    state.epicWatchAgent();
    assert.equal(state._terminalOpening, true);
    await state.closeEpic();
    await state.openEpic(epic.id);
    state.epicWatchAgent();
    await flushMicrotasks();
    const current = ctx.termState.ws;
    assert.equal(sockets.length, 1);
    gate.resolve();
    await flushMicrotasks();
    assert.equal(sockets.length, 1);
    assert.equal(ctx.termState.ws, current);
    assert.equal(state.terminalOpen, true);
    state.destroy();
  });
}

test('epic terminal remounts into the replacement breakpoint container and destroy cancels output', async () => {
  const clock = supervisorClock();
  const { state, ctx, epic, sockets, containers } = epicSupervisorUI({ overrides: clock.overrides });
  await state.openEpic(epic.id);
  state.epicWatchAgent();
  await flushMicrotasks();
  const desktop = ctx.termState.term;
  assert.equal(desktop.element.parentNode, containers['epic-terminal-session']);
  state.isMobile = true;
  state._onBreakpointChange();
  assert.equal(sockets[0].closed, true);
  assert.equal(desktop.disposed, false);
  assert.equal(ctx.termState.term, null);
  await clock.tick([...clock.timers].find(([, timer]) => timer.ms === 0)[0]);
  assert.equal(desktop.disposed, true);
  assert.equal(state.selectedEpic.id, epic.id);
  await flushMicrotasks();
  const phone = ctx.termState.term;
  assert.notEqual(phone, desktop);
  assert.equal(phone.element.parentNode, containers['epic-terminal-mobile']);
  state.epicSwitchTab('activity');
  await flushMicrotasks();
  assert.ok(state._activityPoll !== null);
  state.destroy();
  assert.equal(ctx.termState.ws, null);
  await clock.tick([...clock.timers].find(([, timer]) => timer.ms === 0)[0]);
  assert.equal(phone.disposed, true);
  assert.equal(ctx.termState.term, null);
  assert.equal(state.activity, null);
  assert.equal(state._activityPoll, null);
  assert.equal(clock.timers.size, 0);
});

for (const derived of ['open', 'in_progress', 'done', 'cancelled']) {
  test(`start epic confirms one supervisor, not children: passive ${derived}`, async () => {
    const { state, epic, requests } = epicSupervisorUI({ status: derived, mode: '' });
    await state.openEpic(epic.id);
    requests.length = 0;
    await state.epicSupervisorActions().find(a => a.label === 'start epic').run();
    assert.equal(state.initModal, true);
    assert.equal(state.initForm.ticketId, epic.id);
    assert.equal(state.initForm.kind, 'epic');
    assert.equal(state.initForm.pipeline, '');
    assert.deepEqual(vmValue(state.initStages()), []);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/tickets/' + epic.id, 'opening init refreshes detail');
    requests.length = 0;
    state.onInitPathChange();
    assert.equal(state.initForm.pipeline, '', 'project defaults cannot add a child pipeline');
    state.initForm.branch = 'epic/new';
    await state.submitInitTicket();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/tickets/' + epic.id + '/init');
    assert.deepEqual(JSON.parse(requests[0].body), {
      path: epic.path, agent: 'claude', branch: 'epic/new', status: 'todo', notify: [], notify_channels: [],
    });
    assert.equal(state.initModal, false);
    assert.ok(state.tickets.slice(1).every(t => t.status === 'open'));
  });
}

for (const mobile of [false, true]) {
  test(`epic run details, return, Back and reload share the epic session: mobile=${mobile}`, async () => {
    const { state, ctx, epic, location, sockets } = epicSupervisorUI({ mobile });
    location.hash = '#/e/' + epic.id;
    await state.applyRoute();
    assert.equal(state.selectedEpic.id, epic.id);
    assert.equal(state.selectedTicket, null);
    const hashWrites = [];
    const writeHash = state.writeHash;
    state.writeHash = function () {
      const before = location.hash;
      writeHash.call(this);
      if (location.hash !== before) {
        hashWrites.push(location.hash);
        queueMicrotask(() => state.applyRoute());
      }
    };
    await state.epicRunDetails();
    await flushMicrotasks();
    assert.deepEqual(hashWrites, ['#/t/' + epic.id], 'overlay switches never publish an intermediate board route');
    assert.equal(state.selectedEpic, null);
    assert.equal(state.selectedTicket.id, epic.id);
    assert.equal(location.hash, '#/t/' + epic.id);
    assert.equal(state.terminalWanted(), true);
    assert.equal(state.displayStage(), 'default', 'first-run activity uses the supervisor session key');
    assert.equal(state.detailTab === 'terminal' || !mobile, true);
    assert.equal(sockets.length, 1);
    assert.ok(sockets[0].url.includes('/ws/terminal/' + epic.id));
    const first = sockets[0];
    location.hash = '#/e/' + epic.id;
    await state.applyRoute();
    assert.equal(first.closed, true);
    assert.equal(ctx.termState.ws, null);
    assert.equal(state.terminalOpen, false);
    assert.equal(state.selectedTicket, null);
    assert.equal(state.selectedEpic.id, epic.id);
    location.hash = '#/t/' + epic.id;
    await state.applyRoute();
    await flushMicrotasks();
    assert.equal(sockets.filter(s => !s.closed).length, 1);
    assert.equal(state.selectedEpic, null);
    state.closeDetail();
    location.hash = '#/t/' + epic.id;
    await state.applyRoute();
    await flushMicrotasks();
    assert.equal(sockets.filter(s => !s.closed).length, 1, 'reload-style deep link creates one attachment');
    await state.action('pause');
    assert.equal(sockets.filter(s => !s.closed).length, 0);
    assert.equal(state.selectedTicket.status, 'paused');
    assert.ok(state.tickets.slice(1).every(t => t.status === 'open'));
    state.closeDetail();
  });
}

test('brief navigation saves the previous epic draft and ignores late detail responses', async () => {
  const { state, epic, requests } = epicSupervisorUI({ status: 'paused' });
  await state.openEpic(epic.id);
  state.epicEditingBrief = true;
  state.editForm.body = '# Updated brief';
  state.epicDebounceBrief();
  requests.length = 0;
  await state.epicRunDetails();
  assert.equal(requests.filter(r => r.method === 'PUT').length, 1);
  assert.equal(requests[0].url, '/api/tickets/' + epic.id);
  assert.deepEqual(JSON.parse(requests[0].body), { description: '\n# Updated brief' });
  assert.equal(state.selectedTicket.body, '\n# Updated brief');
  assert.equal(state.selectedEpic, null);
  assert.equal(state._epicBriefDebounce, null);
  state.closeDetail();

  const late = deferred();
  const delayed = loadKontoraState({ fetch: () => late.promise });
  delayed.tickets = [epic];
  const opening = delayed.openEpic(epic.id);
  delayed.closeEpic();
  late.resolve({ ok: true, json: async () => ({ ...epic, body: 'late body' }) });
  await opening;
  assert.equal(delayed.selectedEpic, null);
  assert.equal(delayed._epicDetail, null);
});

test('a failed brief save keeps the draft instead of starting or opening supervisor run details', async () => {
  const { state, ctx, epic } = epicSupervisorUI({ status: 'open' });
  await state.openEpic(epic.id);
  state.epicEditingBrief = true;
  state.editForm.body = 'Unsaved brief';
  ctx.fetch = async () => ({ ok: false, json: async () => ({ error: 'save rejected' }) });
  await state.epicStart();
  assert.equal(state.initModal, false);
  assert.equal(state.selectedEpic.id, epic.id);
  await state.epicRunDetails();
  assert.equal(state.selectedTicket, null);
  assert.equal(state.editForm.body, 'Unsaved brief');
  assert.equal(state.error, 'save rejected');
});

test('a later execution selection supersedes navigation waiting on an epic brief save', async () => {
  const { state, ctx, epic } = epicSupervisorUI({ status: 'paused' });
  await state.openEpic(epic.id);
  state.epicEditingBrief = true;
  state.editForm.body = 'Saved before leaving';
  const gate = deferred();
  const originalFetch = ctx.fetch;
  ctx.fetch = async (url, opts) => {
    if (opts?.method === 'PUT') await gate.promise;
    return originalFetch(url, opts);
  };
  const first = state.selectTicket(state.tickets[1]);
  const second = state.selectTicket(state.tickets[2]);
  gate.resolve();
  await Promise.all([first, second]);
  assert.equal(state.selectedTicket.id, 'child-b');
  assert.equal(state.selectedEpic, null);
  state.closeDetail();
});

for (const action of ['start', 'run details', 'ticket', 'epic', 'close', 'route', 'view', 'new']) {
  for (const ok of [true, false]) {
    test(`blurred pending brief save gates ${action}: success=${ok}`, async () => {
      const { state, ctx, epic, location } = epicSupervisorUI({ status: 'open', mobile: action === 'run details' });
      await state.openEpic(epic.id);
      const other = { ...epic, id: 'other' };
      state.tickets.push(other);
      state.$refs = {};
      state.epicEditingBrief = true;
      state.editForm.body = 'Draft saved after blur';
      state.epicDebounceBrief();
      const gate = deferred();
      const originalFetch = ctx.fetch;
      let puts = 0;
      ctx.fetch = async (url, opts) => {
        if (opts?.method === 'PUT') {
          puts++;
          await gate.promise;
          if (!ok) return { ok: false, json: async () => ({ error: 'save rejected' }) };
        }
        return originalFetch(url, opts);
      };
      state.epicBlurBriefEdit();
      assert.equal(state.epicEditingBrief, false);
      assert.equal(state.epicBriefSaving, true);
      assert.equal(state._epicBriefDebounce, null);
      let finished = false;
      const actions = {
        start: () => state.epicStart(),
        'run details': () => state.epicRunDetails(),
        new: () => state.openCreateModal(),
        ticket: () => state.selectTicket(state.tickets[1]),
        epic: () => state.openEpic(other.id),
        close: () => state.closeEpic(),
        route: () => { location.hash = '#/stats'; return state.applyRoute(); },
        view: () => state.gotoView('stats'),
      };
      const navigating = Promise.resolve(actions[action]()).then(() => { finished = true; });
      await flushMicrotasks();
      assert.equal(finished, false);
      assert.equal(state.selectedEpic.id, epic.id);
      assert.equal(state.selectedTicket, null);
      assert.equal(state.initModal, false);
      assert.equal(puts, 1, 'navigation shares the save started by blur');
      gate.resolve();
      await navigating;
      assert.equal(puts, 1);
      assert.equal(state.epicBriefSaving, false);
      if (!ok) {
        assert.equal(state.selectedEpic.id, epic.id);
        assert.equal(state.selectedTicket, null);
        assert.equal(state.initModal, false);
        assert.equal(state.currentView, 'board');
        assert.equal(location.hash, '#/e/' + epic.id);
        assert.equal(state.editForm.body, 'Draft saved after blur');
        assert.equal(state.epicBriefSaved, false);
        assert.equal(state.error, 'save rejected');
        ctx.fetch = originalFetch;
        assert.equal(await state.epicFlushBrief(), true);
        assert.equal(epic.body, '\nDraft saved after blur');
      } else {
        assert.equal(epic.body, '\nDraft saved after blur');
        if (action === 'start') {
          assert.equal(state.initModal, true);
          assert.equal(state.initForm.ticketId, epic.id);
        } else if (action === 'run details' || action === 'ticket') {
          assert.equal(state.selectedEpic, null);
          assert.equal(state.selectedTicket.id, action === 'run details' ? epic.id : 'child-a');
        } else if (action === 'epic') assert.equal(state.selectedEpic.id, other.id);
        else assert.equal(state.selectedEpic, null);
        if (action === 'route' || action === 'view') assert.equal(state.currentView, 'stats');
        if (action === 'new') {
          assert.equal(state.currentView, 'new');
          assert.equal(location.hash, '#/new');
        }
      }
      state.closeStats();
      if (state.selectedTicket) state.closeDetail();
      if (state.selectedEpic) await state.closeEpic();
    });
  }
}

for (const ok of [true, false]) {
  test(`brief writes serialize newer drafts and Start waits for the last write: success=${ok}`, async () => {
    const { state, ctx, epic } = epicSupervisorUI({ status: 'open' });
    await state.openEpic(epic.id);
    state.$refs = {};
    state.epicEditingBrief = true;
    state.editForm.body = 'First draft';
    const gates = [deferred(), deferred()];
    const bodies = [];
    const originalFetch = ctx.fetch;
    ctx.fetch = async (url, opts) => {
      if (opts?.method === 'PUT') {
        const index = bodies.length;
        bodies.push(JSON.parse(opts.body).description);
        await gates[index].promise;
        if (index === 1 && !ok) return { ok: false, json: async () => ({ error: 'new draft rejected' }) };
      }
      return originalFetch(url, opts);
    };
    const saving = state.epicSaveBrief();
    state.editForm.body = 'Newest draft';
    state.epicDebounceBrief();
    state.epicBlurBriefEdit();
    const starting = state.epicStart();
    await flushMicrotasks();
    assert.deepEqual(bodies, ['\nFirst draft']);
    gates[0].resolve();
    await flushMicrotasks();
    assert.deepEqual(bodies, ['\nFirst draft', '\nNewest draft']);
    assert.equal(state.epicBriefSaving, true);
    assert.equal(state.initModal, false);
    gates[1].resolve();
    assert.equal(await saving, ok);
    await starting;
    assert.equal(state.epicBriefSaving, false);
    assert.equal(state.epicBriefSaved, ok);
    assert.equal(state.initModal, ok);
    assert.equal(state.editForm.body, 'Newest draft');
    assert.equal(epic.body, ok ? '\nNewest draft' : '\nFirst draft');
    if (!ok) {
      ctx.fetch = originalFetch;
      await state.epicStart();
      assert.equal(epic.body, '\nNewest draft');
      assert.equal(state.initModal, true);
    }
    await state.closeEpic();
  });
}

for (const target of ['ticket', 'epic', 'board']) {
  for (const routed of [true, false]) {
    test(`a newer ${routed ? 'route' : 'selection'} to ${target} supersedes a delayed epic route load`, async () => {
      const { state, ctx, epic, location } = epicSupervisorUI({ status: 'paused', mobile: routed });
      const other = { ...epic, id: 'other' };
      state.tickets.push(other);
      const gate = deferred();
      const originalFetch = ctx.fetch;
      ctx.fetch = async (url, opts) => {
        if (url === '/api/tickets/' + epic.id) await gate.promise;
        return originalFetch(url, opts);
      };
      location.hash = '#/e/' + epic.id;
      const first = state.applyRoute();
      if (routed) {
        location.hash = target === 'ticket' ? '#/t/child-b' : target === 'epic' ? '#/e/other' : '#/';
        await state.applyRoute();
      } else if (target === 'ticket') await state.selectTicket(state.tickets[2]);
      else if (target === 'epic') await state.openEpic(other.id);
      else await state.closeEpic();
      const latestHash = location.hash;
      gate.resolve();
      await first;
      assert.equal(location.hash, latestHash);
      assert.equal(location.hash, target === 'ticket' ? '#/t/child-b' : target === 'epic' ? '#/e/other' : '#/');
      assert.equal(state.selectedTicket?.id || null, target === 'ticket' ? 'child-b' : null);
      assert.equal(state.selectedEpic?.id || null, target === 'epic' ? other.id : null);
      assert.equal(state._applyingRoute, null);
      if (state.selectedTicket) state.closeDetail();
      if (state.selectedEpic) await state.closeEpic();
    });
  }
}

for (const older of ['#/t/child-a', '#/e/other', '#/']) {
  for (const newer of ['#/t/child-b', '#/e/other', '#/']) {
    test(`a newer route ${newer} supersedes ${older} waiting on a blurred brief save`, async () => {
      const { state, ctx, epic, location } = epicSupervisorUI({ status: 'paused', mobile: newer.startsWith('#/t/') });
      state.tickets.push({ ...epic, id: 'other' });
      await state.openEpic(epic.id);
      state.$refs = {};
      state.epicEditingBrief = true;
      state.editForm.body = 'Saved before either route';
      const gate = deferred();
      const originalFetch = ctx.fetch;
      let puts = 0;
      ctx.fetch = async (url, opts) => {
        if (opts?.method === 'PUT') { puts++; await gate.promise; }
        return originalFetch(url, opts);
      };
      state.epicBlurBriefEdit();
      location.hash = older;
      const first = state.applyRoute();
      location.hash = newer;
      const second = state.applyRoute();
      await flushMicrotasks();
      assert.equal(state.selectedEpic.id, epic.id);
      assert.equal(puts, 1);
      gate.resolve();
      await Promise.all([first, second]);
      assert.equal(puts, 1);
      assert.equal(epic.body, '\nSaved before either route');
      assert.equal(location.hash, newer);
      assert.equal(state.selectedTicket?.id || null, newer.startsWith('#/t/') ? 'child-b' : null);
      assert.equal(state.selectedEpic?.id || null, newer.startsWith('#/e/') ? 'other' : null);
      assert.equal(state._applyingRoute, null);
      if (state.selectedTicket) state.closeDetail();
      if (state.selectedEpic) await state.closeEpic();
    });
  }
}

for (const mobile of [false, true]) {
  for (const newer of ['#/t/child-b', '#/e/kon-e7c1']) {
    test(`a newer route ${newer} supersedes a delayed ticket route: mobile=${mobile}`, async () => {
      const { state, ctx, location } = epicSupervisorUI({ status: 'paused', mobile });
      const gate = deferred();
      const originalFetch = ctx.fetch;
      ctx.fetch = async (url, opts) => {
        if (url === '/api/tickets/child-a') await gate.promise;
        return originalFetch(url, opts);
      };
      location.hash = '#/t/child-a';
      const first = state.applyRoute();
      location.hash = newer;
      await state.applyRoute();
      const latestTab = state.detailTab;
      gate.resolve();
      await first;
      assert.equal(location.hash, newer);
      assert.equal(state.selectedTicket?.id || null, newer.startsWith('#/t/') ? 'child-b' : null);
      assert.equal(state.selectedEpic?.id || null, newer.startsWith('#/e/') ? 'kon-e7c1' : null);
      assert.equal(state.detailTab, latestTab);
      assert.equal(state._applyingRoute, null);
      if (state.selectedTicket) state.closeDetail();
      if (state.selectedEpic) await state.closeEpic();
    });
  }
}

for (const action of ['start', 'run details', 'route', 'view', 'new']) {
  for (const ok of [true, false]) {
    test(`a newer route supersedes ${action} during a blurred save: success=${ok}`, async () => {
      const { state, ctx, epic, location } = epicSupervisorUI({ status: 'open' });
      await state.openEpic(epic.id);
      state.$refs = {};
      state.epicEditingBrief = true;
      state.editForm.body = 'Retained draft';
      const gate = deferred();
      const originalFetch = ctx.fetch;
      let puts = 0;
      ctx.fetch = async (url, opts) => {
        if (opts?.method === 'PUT') {
          puts++;
          await gate.promise;
          if (!ok) return { ok: false, json: async () => ({ error: 'save rejected' }) };
        }
        return originalFetch(url, opts);
      };
      state.epicBlurBriefEdit();
      let first;
      if (action === 'start') first = state.epicStart();
      else if (action === 'run details') first = state.epicRunDetails();
      else if (action === 'new') first = state.openCreateModal();
      else if (action === 'view') first = state.gotoView('stats');
      else { location.hash = '#/t/child-a'; first = state.applyRoute(); }
      location.hash = '#/t/child-b';
      const second = state.applyRoute();
      gate.resolve();
      await Promise.all([first, second]);
      assert.equal(puts, 1);
      assert.equal(state.initModal, false);
      assert.equal(state.currentView, 'board');
      assert.equal(state.selectedTicket?.id || null, ok ? 'child-b' : null);
      assert.equal(state.selectedEpic?.id || null, ok ? null : epic.id);
      assert.equal(location.hash, ok ? '#/t/child-b' : '#/e/' + epic.id);
      assert.equal(state._applyingRoute, null);
      if (!ok) {
        assert.equal(state.editForm.body, 'Retained draft');
        ctx.fetch = originalFetch;
        await state.epicFlushBrief();
      }
      if (state.selectedTicket) state.closeDetail();
      if (state.selectedEpic) await state.closeEpic();
    });
  }
}

test('a superseded route cannot release hash suppression owned by a newer delayed route', async () => {
  const { state, ctx, epic, location } = epicSupervisorUI({ status: 'paused' });
  state.tickets.push({ ...epic, id: 'other' });
  const gates = [deferred(), deferred()];
  const originalFetch = ctx.fetch;
  ctx.fetch = async (url, opts) => {
    if (url === '/api/tickets/' + epic.id) await gates[0].promise;
    if (url === '/api/tickets/other') await gates[1].promise;
    return originalFetch(url, opts);
  };
  location.hash = '#/e/' + epic.id;
  const first = state.applyRoute();
  location.hash = '#/e/other';
  const second = state.applyRoute();
  gates[0].resolve();
  await first;
  assert.equal(location.hash, '#/e/other');
  assert.equal(state._applyingRoute, state._navigationSeq);
  gates[1].resolve();
  await second;
  assert.equal(state.selectedEpic.id, 'other');
  assert.equal(location.hash, '#/e/other');
  assert.equal(state._applyingRoute, null);
  await state.closeEpic();
});

test('passive execution links offer activation without attaching a nonexistent supervisor', async () => {
  const { state, epic, sockets, location } = epicSupervisorUI({ mode: '', mobile: true });
  location.hash = '#/t/' + epic.id;
  await state.applyRoute();
  await flushMicrotasks();
  assert.equal(state.selectedTicket.id, epic.id);
  assert.equal(state.detailTab, 'ticket');
  assert.equal(sockets.length, 0);
  assert.equal(state.mobilePrimaryAction(state.selectedTicket).label, 'start epic');
  assert.deepEqual(vmValue(state.detailMoves(state.selectedTicket)), [{ label: 'Start epic', endpoint: 'run' }]);
  await state.moveTicketVia(epic.id, 'run', null);
  assert.equal(state.initForm.ticketId, epic.id);
  state.closeDetail();
});

test('activation SSE refreshes execution mode without replacing an open body draft', async () => {
  const { state, epic, location } = epicSupervisorUI({ status: 'open', mode: '' });
  location.hash = '#/t/' + epic.id;
  await state.applyRoute();
  await flushMicrotasks();
  assert.equal(state.editing, true);
  state.editForm.body = 'Local draft';
  const activated = { ...epic, status: 'todo', epic_execution: 'agent' };
  state.applyTicketUpdate(activated);
  state.recomputeBoard();
  assert.equal(state.epicIsAgent(state.selectedTicket), true);
  assert.equal(state.epicCanStart(state.selectedTicket), false);
  assert.equal(state.editForm.body, 'Local draft');
  assert.equal(state.boardEntryChanged(epic, { ...epic, epic_execution: 'agent' }), true);
  state.editing = false;
  state.closeDetail();
});

for (const supervisorsOnly of [true, false]) {
  test(`supervisor board counts and filters work with hidden sidebar and badges: supervisorsOnly=${supervisorsOnly}`, () => {
    const { state, epic } = epicSupervisorUI();
    state.sidebarHidden = true;
    state.showPipelineBadges = false;
    const passive = { ...epic, id: 'passive', epic_execution: '', waiting_for_input: true };
    state.tickets = [state.tickets[0], passive, ...(supervisorsOnly ? [] : state.tickets.slice(1))];
    if (!supervisorsOnly) {
      state.tickets[2].status = 'in_progress';
      state.tickets[3].status = 'done';
    }
    state.recomputeBoard();
    const total = supervisorsOnly ? 1 : 4;
    const running = supervisorsOnly ? 1 : 2;
    assert.equal(state._boardTotal, total);
    assert.equal(state.runningAgents, running);
    assert.equal(state._statusCounts.in_progress, running);
    assert.equal(state._statusCounts.needsInput, 0, 'passive rollups never count as waiting processes');
    assert.equal(state.agentRunningCount('claude'), running);
    assert.equal(state.epics.length, 2);
    assert.equal(bt(state, 'in_progress').filter(t => t.id === epic.id).length, 1);
    const col = state.columns.find(c => c.key === 'in_progress');
    const card = state._cardHTML(state.tickets[0], col);
    assert.match(card, /aria-label="Supervisor /);
    assert.match(card, />supervisor<\/span>/, 'identity survives disabled pipeline badges');
    assert.match(card, />supervising<\/span>/);
    assert.match(state._cardMenuHTML(epic), />Epic overview<\/button>/);
    assert.match(state._cardMenuHTML(epic), />Pause supervisor<\/button>/);
    for (const [query, expected] of [
      ['epic:kon-e7c1', total], ['epic:=kon-e7c1', total],
      ['project:=repo', total], ['agent:=claude', total],
      ['Supervised', 1], ['epic:passive', 0], ['project:missing', 0],
    ]) {
      state.searchQuery = query;
      state.recomputeBoard();
      assert.equal(state._boardTotal, expected, query);
      assert.equal(state.runningAgents, running, 'process tallies ignore filters');
    }
    state.destroy();
  });
}

for (const [status, label] of [['in_progress', 'supervising'], ['todo', 'queued'], ['paused', 'paused']]) {
  test(`supervisor board uses ${label}, not a pipeline stage`, () => {
    const { state, epic } = epicSupervisorUI({ status });
    const ticket = { ...epic, stage: 'code', stages: ['plan', 'code', 'review'] };
    state.showPipelineBadges = false;
    const col = state.columns.find(c => c.key === 'in_progress');
    const card = state._cardHTML(ticket, col);
    assert.match(card, />supervisor<\/span>/);
    assert.ok(card.includes('>' + label + '</span>'));
    assert.equal(card.includes('card-progress'), false, 'supervisors have no child pipeline progress');
    assert.equal(card.includes('>code</span>'), false);
    if (status !== 'in_progress') {
      const mobileCards = fs.readFileSync(htmlPath, 'utf8').split('x-for="ticket in mobileTickets()"')[1]
        .split('<!-- ===== Ticket detail')[0];
      const statusTemplate = [...mobileCards.matchAll(/<template x-if="([^"]+)">([\s\S]*?)<\/template>/g)]
        .find(([, , content]) => content.includes('>' + label + '</span>'));
      assert.ok(statusTemplate, 'mobile supervisor card names ' + label);
      assert.doesNotMatch(statusTemplate[2], /⏸|◌|pulse-dot/);
      const visible = t => vm.runInNewContext(statusTemplate[1], {
        ticket: t, epicIsAgent: t => state.epicIsAgent(t), mobileColumn: () => ({ key: 'in_progress' }),
        showPipelineBadges: false, showAgentMeta: false,
      });
      assert.equal(visible(ticket), true);
      assert.equal(visible({ ...ticket, status: 'in_progress' }), false);
      assert.equal(visible({ ...ticket, kind: 'ticket' }), false);
    }
    state.destroy();
  });
}

for (const [status, label] of [['paused', 'Paused'], ['todo', 'Queued']]) {
  test(`ordinary mobile cards name ${label} separately from supervisors`, () => {
    const { state, epic } = epicSupervisorUI({ status });
    const cards = fs.readFileSync(htmlPath, 'utf8').split('x-for="ticket in mobileTickets()"')[1]
      .split('<!-- ===== Ticket detail')[0];
    const statusTemplate = [...cards.matchAll(/<template x-if="([^"]+)">([\s\S]*?)<\/template>/g)]
      .find(([, , content]) => content.includes('>' + label + '</span>'));
    assert.ok(statusTemplate, 'ordinary card names ' + label);
    const visible = ticket => vm.runInNewContext(statusTemplate[1], {
      ticket, epicIsAgent: t => state.epicIsAgent(t), mobileColumn: () => ({ key: 'in_progress' }),
      showPipelineBadges: false, showAgentMeta: false,
    });
    const ordinary = { ...epic, kind: 'ticket' };
    assert.equal(visible(ordinary), true, 'state remains visible with metadata preferences off');
    assert.equal(visible(epic), false, 'supervisors retain their existing wording');
    for (const other of ['open', 'in_progress', 'paused', 'todo', 'done', 'cancelled'].filter(s => s !== status)) {
      assert.equal(visible({ ...ordinary, status: other }), false, other);
    }
    state.destroy();
  });
}

test('supervisor status and process counts stay separate from child progress', async () => {
  const { state, epic } = epicSupervisorUI();
  state.tickets.push({ id: 'passive', kind: 'epic', status: 'in_progress', kontora: true, agent: 'claude' });
  state.tickets[1].status = 'in_progress';
  state.recomputeBoard();
  assert.equal(state.runningAgents, 2);
  assert.equal(state.agentRunningCount('claude'), 2);
  assert.equal(state.agentPickerRunningTotal(), 2);
  assert.equal(state._boardTotal, 4);
  assert.equal(state.epicRollup(epic.id).total, 3);
  assert.deepEqual(vmValue(state.columns.flatMap(col => [...state.boardTickets(col.key)]).filter(t => t.kind === 'epic').map(t => t.id)), [epic.id]);
  await state.openEpic(epic.id);
  state.applyTicketUpdate({ ...state.tickets[1], status: 'done' });
  state.recomputeBoard();
  assert.equal(state.selectedEpic.status, 'in_progress');
  assert.equal(state.epicRollup(epic.id).done, 1);
  assert.equal(state.runningAgents, 1);
  assert.equal(state.epicCanEditBrief(), false);
});

for (const operation of ['pause', 'drag']) {
  test(`rejected supervisor ${operation} leaves children unchanged`, async () => {
    const { state, ctx, epic, requests } = epicSupervisorUI();
    await state.openEpic(epic.id);
    const children = JSON.stringify(state.tickets.slice(1));
    ctx.fetch = async (url, opts) => {
      requests.push({ url, ...opts });
      return { ok: false, json: async () => ({ error: 'supervisor transition refused' }) };
    };
    requests.length = 0;
    if (operation === 'drag') await state.moveTask(epic.id, 'done');
    else await state.epicAction('pause');
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/tickets/' + epic.id + '/' + (operation === 'drag' ? 'move' : 'pause'));
    assert.equal(state.selectedEpic.status, 'in_progress');
    assert.equal(JSON.stringify(state.tickets.slice(1)), children);
    assert.equal(state.error, 'supervisor transition refused');
    state.destroy();
  });
}

test('supervisor controls use its lifecycle, without stage skip or schedule', async () => {
  const { state, epic, requests } = epicSupervisorUI();
  await state.openEpic(epic.id);
  assert.deepEqual(vmValue(state.epicSupervisorActions().map(a => a.label)), ['watch agent', 'run details', 'pause supervisor']);
  requests.length = 0;
  await state.epicSupervisorActions().find(a => a.label === 'pause supervisor').run();
  assert.deepEqual(requests.map(r => r.url), ['/api/tickets/' + epic.id + '/pause']);
  assert.equal(state.selectedEpic.status, 'paused');
  assert.deepEqual(vmValue(state.epicSupervisorActions().map(a => a.label)), ['watch agent', 'run details', 'retry supervisor']);
  await state.epicAction('retry');
  assert.equal(state.selectedEpic.status, 'todo');
  state.applyTicketUpdate({ ...epic, status: 'human_review' });
  state.recomputeBoard();
  assert.deepEqual(vmValue(state.epicSupervisorActions().map(a => a.label)), ['watch agent', 'run details', 'review supervisor', 'approve', 'send back']);
  assert.equal(state.canSchedule(epic), false);
  await state.epicRunDetails();
  assert.equal(state.canSetStage(), false);
  assert.ok(state.mobileSheetActions(state.selectedTicket).every(a => a.label !== 'Skip stage'));
  state.closeDetail();
});

test('finished supervisors reuse summaries, activity and the integration diff', async () => {
  const { state, epic, requests } = epicSupervisorUI({ status: 'human_review' });
  epic.summary = 'Combined result is ready for review';
  epic.history = [{ stage: 'default', run: 0, agent: 'claude', exit_code: 0, summary: epic.summary }];
  await state.openEpic(epic.id);
  requests.length = 0;
  await state.epicRunDetails();
  await flushMicrotasks();
  assert.equal(state.activeTab, 'summary');
  assert.equal(state.showSummaryTab(), true);
  assert.equal(state.showDiffTab(), true);
  assert.equal(state.summaryCards()[0].summary, epic.summary);
  assert.ok(requests.some(r => r.url.startsWith('/api/tickets/' + epic.id + '/activity')));
  assert.ok(requests.some(r => r.url === '/api/tickets/' + epic.id + '/changes'));
  state.closeDetail();
});

test('switching epics flushes the previous brief without writing onto the next epic', async () => {
  const { state, epic, requests } = epicSupervisorUI({ mode: '' });
  const other = { ...epic, id: 'other' };
  state.tickets.push(other);
  await state.openEpic(epic.id);
  state.epicEditingBrief = true;
  state.editForm.body = '# Previous draft';
  state.epicDebounceBrief();
  requests.length = 0;
  await state.openEpic(other.id);
  assert.equal(state.selectedEpic.id, other.id);
  const saves = requests.filter(r => r.method === 'PUT');
  assert.deepEqual(saves.map(r => r.url), ['/api/tickets/' + epic.id]);
  assert.deepEqual(JSON.parse(saves[0].body), { description: '\n# Previous draft' });
  assert.equal(state.epicEditingBrief, false);
  state.closeEpic();
});

test('epic controls and brief surfaces are available in both responsive layouts', () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  assert.equal(html.includes('epicQueueOpen'), false);
  assert.equal(html.includes('queue all open'), false);
  assert.equal(html.match(/x-for="a in epicSupervisorActions\(\)"/g).length, 2);
  assert.equal(html.match(/@click="openEpic\(selectedTicket.id\)"/g).length, 2);
  assert.match(html, /class="flex flex-col gap-\[7px\]" x-show="initForm.kind !== 'epic'"/);
  assert.match(html, /aria-label="Epic overview"/);
  for (const id of ['epic-terminal-session', 'epic-terminal-mobile', 'epic-activity-scroll', 'epic-activity-scroll-mobile']) {
    assert.equal(html.split('id="' + id + '"').length - 1, 1, id + ' is a distinct embedded output container');
  }
  assert.equal(html.match(/x-for="r in outputRuns\(\)"/g).length, 2);
  assert.equal(html.match(/@change="selectOutputRun\(\$event.target.value\)"/g).length, 2);
  assert.equal(html.split('Transcript truncated. Open the full raw stage log; it may show a different attempt.').length - 1, 3);
});

test('supervisor capacity reloads live and defaults to four', async () => {
  const state = await settingsState();
  const field = state.settingsGeneralFields.find(f => f.key === 'max_concurrent_epic_supervisors');
  assert.equal(field.placeholder, '4');
  assert.equal(state.settingsIsRestartOnly(field.key), false);
  assert.doesNotMatch(field.help, /restart/i);
  assert.ok(state.settingsGeneralTextRows('execution').some(f => f.key === field.key));
  state.settingsConfig.general.max_concurrent_epic_supervisors = '6';
  assert.match(await state._settingsWrite(), /^max_concurrent_epic_supervisors: 6$/m);
  state.settingsConfig.general.max_concurrent_epic_supervisors = 'many';
  assert.match(await state._settingsWrite(), /^max_concurrent_epic_supervisors: many$/m);
});

// ─── notify row ───

// One channel, one project, so the resolution has all three levels to walk.
function notifyState(overrides = {}, config = {}) {
  const state = loadKontoraState(overrides);
  state.configCache = {
    pipelines: ["build"],
    agents: ["claude"],
    channels: ["tg"],
    default_channels: ["tg"],
    projects: [{ name: "kontora", path: "~/projects/kontora", resolved_path: "/home/a/projects/kontora" }],
    ...config,
  };
  return state;
}

for (const [name, config, expected] of [
  ["loading", null, false],
  ["omitted channels", {}, false],
  ["null channels", { channels: null }, false],
  ["empty channels", { channels: [] }, false],
  ["one channel without a route", { channels: ["tg"] }, true],
  ["multiple channels", { channels: ["tg", "ops"] }, true],
]) {
  test(`notification availability follows configured channels: ${name}`, () => {
    const state = notifyState();
    state.configCache = config;
    assert.equal(state.notifyHasChannels(), expected);
  });
}

for (const [name, config, key, allowed, channels] of [
  ["loading Off", null, "off", false, ["removed"]],
  ["loading enabled", null, "needs", false, ["removed"]],
  ["unavailable enabled", { channels: [] }, "done", false, ["removed"]],
  ["removed channel Off", { channels: [] }, "off", true, []],
  ["omitted channels Off", {}, "off", true, []],
  ["null channels Off", { channels: null }, "off", true, []],
  ["configured channel Off", { channels: ["tg"] }, "off", true, ["tg"]],
  ["configured channel enabled", { channels: ["tg"] }, "needs", true, ["tg"]],
]) {
  test(`the init notification preset handles ${name}`, () => {
    const state = notifyState();
    state.configCache = config;
    const selected = config?.channels?.length ? ["tg"] : ["removed"];
    state.initForm = { notify: ["done"], notifyChannels: selected };
    state.initNotifyOpen = true;
    state.initError = "previous error";
    state.initPickNotifyMode(key);
    assert.deepEqual(vmValue(state.initForm.notify), allowed ? (key === "off" ? [] : ["paused", "human_review", "waiting"]) : ["done"]);
    assert.deepEqual(vmValue(state.initForm.notifyChannels), channels);
    assert.equal(state.initNotifyOpen, !allowed);
    assert.equal(state.initError, allowed ? null : "previous error");
  });
}

for (const recover of [false, true]) {
  test(`the init modal starts without channels${recover ? " after explicit Off recovery" : " when already off"}`, async () => {
    const seen = [];
    const ticket = { id: "kon-1", path: "~/projects/kontora", notify: recover ? ["done"] : [], notify_channels: recover ? ["removed"] : [] };
    const state = notifyState({ fetch: async (url, options) => {
      seen.push({ url, options });
      return { ok: true, json: async () => ticket };
    } }, { channels: [], default_channels: [] });
    state.$nextTick = () => Promise.resolve();
    await state.openInitModal(ticket);
    assert.deepEqual(vmValue(state.initForm.notify), ticket.notify);
    assert.deepEqual(vmValue(state.initForm.notifyChannels), ticket.notify_channels);
    assert.equal(state.initNotifyOpen, false);
    if (recover) state.initPickNotifyMode("off");
    await state.submitInitTicket();
    const posts = seen.filter(r => r.options?.method === "POST");
    assert.equal(posts.length, 1);
    assert.equal(posts[0].url, "/api/tickets/kon-1/init");
    const body = JSON.parse(posts[0].options.body);
    assert.deepEqual(body.notify, []);
    assert.deepEqual(body.notify_channels, []);
    assert.equal(state.initError, null);
    assert.equal(state.initModal, false);
  });
}

test("refreshing running channels changes availability without changing the init draft", () => {
  const state = notifyState();
  state.initModal = true;
  state.initForm = { notify: ["done"], notifyChannels: ["removed"] };
  state.settingsConfig = { notifications: { channels: { unsaved: {} } } };
  for (const channels of [[], ["tg"], [], ["tg", "ops"]]) {
    state.configCache = { ...state.configCache, channels };
    assert.equal(state.notifyHasChannels(), channels.length > 0);
    assert.deepEqual(vmValue(state.initForm.notify), ["done"]);
    assert.deepEqual(vmValue(state.initForm.notifyChannels), ["removed"]);
  }
});

test("the start dialog binds notification availability and accessible configuration guidance", () => {
  const html = fs.readFileSync(htmlPath, 'utf8');
  const modal = html.slice(html.indexOf('role="group" aria-label="notify me"'), html.indexOf('<!-- Delete ticket modal -->'));
  assert.match(modal, /:aria-describedby="!configCache \? 'init-notify-loading' : \(!notifyHasChannels\(\) \? 'init-notify-unavailable' : null\)"/);
  assert.match(modal, /:disabled="!configCache \|\| \(!notifyHasChannels\(\) && m\.key !== 'off'\)"/);
  assert.match(modal, /@click="initPickNotifyMode\(m\.key\)"/);
  assert.doesNotMatch(modal, /notifyPickMode\(initForm/);
  assert.equal((modal.match(/:disabled="!configCache \|\| !notifyHasChannels\(\)"/g) || []).length, 3);
  assert.equal((modal.match(/disabled:opacity-50 disabled:cursor-not-allowed/g) || []).length, 5);
  assert.match(modal, /id="init-notify-help" x-show="!configCache \|\| !notifyHasChannels\(\)"/);
  assert.match(modal, /id="init-notify-loading" x-show="!configCache">Waiting for notification configuration\. If loading does not finish, close and reopen this dialog to retry\./);
  assert.match(modal, /id="init-notify-unavailable" x-show="configCache && !notifyHasChannels\(\)">No notification channels/);
  assert.match(modal, /To start without notifications, select off first, even if it is already selected\./);
  assert.match(modal, /notifications\.channels<\/code>, then restart the daemon/);
  assert.match(modal, /<a href="https:\/\/worksonmyai\.github\.io\/kontora\/configuration\/#notifications" target="_blank" rel="noopener noreferrer"[^>]*>Configure notifications<\/a>/);
  assert.match(modal, /<\/template>\s*<p id="init-notify-help"/);
});

test("the notify mode is read off the statuses instead of being stored beside them", () => {
  const state = notifyState();
  const mode = (notify) => state.notifyModeOf({ notify });

  assert.equal(mode([]), "off");
  // Order is not part of the answer: the file may list them any way round.
  assert.equal(mode(["waiting", "paused", "human_review"]), "needs");
  assert.equal(mode(["done", "human_review"]), "done");
  // One status short of a mode is not that mode.
  assert.equal(mode(["paused", "human_review"]), "custom");
  assert.equal(mode(["done"]), "custom");
  assert.equal(mode(["done", "cancelled"]), "custom");
});

test("picking a mode replaces the statuses and leaves the channels alone", () => {
  const state = notifyState();
  const form = { notify: ["done"], notifyChannels: ["tg"], path: "~/projects/kontora" };

  state.notifyPickMode(form, "needs");
  assert.deepEqual(vmValue(form.notify), ["paused", "human_review", "waiting"]);

  // Off clears the statuses only: turning notifications back on must not
  // re-ask where they go.
  state.notifyPickMode(form, "off");
  assert.deepEqual(vmValue(form.notify), []);
  assert.deepEqual(vmValue(form.notifyChannels), ["tg"]);
});

test("toggling a chip moves the row to custom without clearing what is there", () => {
  const state = notifyState();
  const form = { notify: ["paused", "human_review", "waiting"], notifyChannels: [], path: "" };

  state.notifyToggleStatus(form, "done");
  assert.deepEqual(vmValue(form.notify), ["paused", "human_review", "waiting", "done"]);
  assert.equal(state.notifyModeOf(form), "custom");

  state.notifyToggleStatus(form, "done");
  assert.equal(state.notifyModeOf(form), "needs");
});

test("the row states the channel it resolves to, through every level", () => {
  const state = notifyState();
  const where = (form) => state.notifyWhere(form);

  // The ticket's own list wins.
  assert.equal(where({ notify: ["done"], notifyChannels: ["tg"], path: "" }), "→ tg");
  // Then the project's, matched on either spelling of the path.
  state.configCache.projects[0].notify_channels = ["ops"];
  assert.equal(where({ notify: ["done"], notifyChannels: [], path: "/home/a/projects/kontora" }), "→ ops");
  // Then the global default, for a path no project owns.
  assert.equal(where({ notify: ["done"], notifyChannels: [], path: "~/elsewhere" }), "→ tg");
  // `none` stops the walk rather than falling through to the level below.
  assert.equal(where({ notify: ["done"], notifyChannels: ["none"], path: "~/projects/kontora" }), "→ no channel");
  // Nothing to route while no status is picked.
  assert.equal(where({ notify: [], notifyChannels: ["tg"], path: "" }), "");
});

test("the echo line and the rail value say the same thing two ways", () => {
  const state = notifyState();
  const form = { notify: [], notifyChannels: [], path: "~/projects/kontora" };

  assert.equal(state.notifyEcho(form), "no notify: field written");
  assert.equal(state.notifyLabel(form), "off");

  state.notifyPickMode(form, "needs");
  assert.equal(state.notifyEcho(form), "notify: [paused, human_review, waiting]");
  assert.equal(state.notifyLabel(form), "when it needs me → tg");

  state.notifyPickMode(form, "done");
  assert.deepEqual(vmValue(form.notify), ["human_review", "done"]);
  assert.equal(state.notifyLabel(form), "when finished → tg");

  // A custom list spells its statuses out: a label nobody can read back is a
  // label nobody can trust.
  state.notifyToggleStatus(form, "paused");
  assert.equal(state.notifyLabel(form), "human_review, done, paused → tg");
});

test("the channel row appears for a choice or an unrouted notification", () => {
  const state = notifyState();
  const form = { notify: ["human_review", "done"], notifyChannels: [], path: "~/projects/kontora" };
  assert.equal(state.notifyPicksChannel(form), false, "one inherited channel needs no choice");

  state.configCache.default_channels = [];
  assert.equal(state.notifyPicksChannel(form), true, "the only channel must be offered when inheritance finds nothing");

  state.configCache.channels = [];
  assert.equal(state.notifyPicksChannel(form), true, "silence remains available when no channel is configured");
  assert.deepEqual(
    vmValue(state.notifyChannelChips(form)).map((c) => c.key),
    ["inherit", "none"],
  );

  state.configCache.default_channels = ["tg"];
  state.configCache.channels = ["tg", "ops"];
  assert.equal(state.notifyPicksChannel(form), true, "multiple channels are always a choice");

  const chips = () => vmValue(state.notifyChannelChips(form)).map((c) => [c.key, c.on]);

  // Inherit is what a ticket that names nothing is on.
  assert.deepEqual(chips(), [["inherit", true], ["tg", false], ["ops", false], ["none", false]]);

  // Picking an explicit channel drops the inherit; unpicking the last one
  // falls back to it.
  state.notifyToggleChannel(form, { key: "ops", kind: "channel", on: false });
  assert.deepEqual(chips(), [["inherit", false], ["tg", false], ["ops", true], ["none", false]]);
  state.notifyToggleChannel(form, { key: "ops", kind: "channel", on: true });
  assert.deepEqual(chips(), [["inherit", true], ["tg", false], ["ops", false], ["none", false]]);

  // Silence replaces the list rather than joining it, which is the one
  // combination the daemon refuses.
  state.notifyToggleChannel(form, { key: "tg", kind: "channel", on: false });
  state.notifyToggleChannel(form, { key: "none", kind: "silence", on: false });
  assert.deepEqual(vmValue(form.notifyChannels), ["none"]);
});

test("an unrouted notification is invalid unless it is explicitly silenced", () => {
  const state = notifyState();
  const form = { notify: ["human_review", "done"], notifyChannels: [], path: "~/projects/kontora" };

  assert.equal(state.notifyRouteError(form), "");
  state.configCache.default_channels = [];
  assert.equal(state.notifyRouteError(form), "Choose a notification channel, configure an inherited route, or turn notifications off.");

  state.configCache.projects[0].notify_channels = ["tg"];
  form.path = "/home/a/projects/other/../kontora/.";
  assert.equal(state.notifyRouteError(form), "", "project matching cleans dot segments like the daemon");
  state.configCache.projects[0].notify_channels = [];
  form.path = "~/projects/kontora";

  form.notifyChannels = ["none"];
  assert.equal(state.notifyRouteError(form), "");

  form.notifyChannels = [];
  state.configCache.default_channels = ["none"];
  assert.equal(state.notifyRouteError(form), "", "an inherited silence is deliberate");
  assert.match(state.notifyChannelHint(form), /^Inherited: silenced by notifications\.default\./);

  state.configCache.default_channels = [];
  state.configCache.projects[0].notify_channels = ["none"];
  assert.equal(state.notifyRouteError(form), "", "a project silence is deliberate");
  assert.match(state.notifyChannelHint(form), /^Inherited: silenced by project kontora\./);

  form.notify = [];
  state.configCache.projects[0].notify_channels = [];
  assert.equal(state.notifyRouteError(form), "");
});

test("the init modal does not start a ticket with an accidental silent route", async () => {
  const seen = [];
  const state = notifyState({ fetch: async (...args) => { seen.push(args); return { ok: true }; } });
  state.configCache.default_channels = [];
  state.initForm = {
    ticketId: "kon-1", path: "/home/a/projects/kontora", pipeline: "build", agent: "claude",
    branch: "", notify: ["human_review", "done"], notifyChannels: [],
  };

  await state.submitInitTicket();

  assert.equal(seen.length, 0);
  assert.equal(state.initSubmitting, false);
  assert.equal(state.initError, "Choose a notification channel, configure an inherited route, or turn notifications off.");

  state.notifyToggleChannel(state.initForm, { key: "tg", kind: "channel", on: false });
  assert.equal(state.initError, null);
  await state.submitInitTicket();
  assert.equal(seen.length, 1);
  assert.deepEqual(JSON.parse(seen[0][1].body).notify_channels, ["tg"]);
});

test("changing the init path clears an unrouted error when the project supplies a route", async () => {
  const seen = [];
  const state = notifyState({ fetch: async (...args) => { seen.push(args); return { ok: true }; } });
  state.configCache.default_channels = [];
  state.configCache.projects[0].notify_channels = ["tg"];
  state.initForm = {
    ticketId: "kon-1", path: "/elsewhere", pipeline: "build", agent: "claude", branch: "",
    notify: ["human_review", "done"], notifyChannels: [],
  };
  state._initInherited = { pipeline: "", agent: "" };

  await state.submitInitTicket();
  assert.ok(state.initError);

  state.initForm.path = "/home/a/projects/kontora";
  state.onInitPathChange();
  assert.equal(state.initError, null);

  await state.submitInitTicket();
  assert.equal(seen.length, 1);
});

test("the rail waits for a channel before saving an enabled notification", async () => {
  const seen = [];
  const state = notifyState({
    fetch: async (url, opts) => {
      seen.push({ url, body: JSON.parse(opts.body) });
      return { ok: true, json: async () => ({ id: "kon-1", status: "todo", notify: ["human_review", "done"], notify_channels: ["tg"], body: "" }) };
    },
  });
  state.configCache.default_channels = [];
  state.tickets = [{ id: "kon-1", status: "todo" }];
  state.selectedTicket = { id: "kon-1", status: "todo", path: "/home/a/projects/kontora", notify: [], notify_channels: [] };
  state.notifyDraft = {
    notify: ["human_review", "done"], notifyChannels: [], path: "/home/a/projects/kontora", autosave: true,
  };

  await state.saveNotify();
  assert.equal(seen.length, 0);
  assert.equal(state.notifyError, "Choose a notification channel, configure an inherited route, or turn notifications off.");

  state.notifyToggleChannel(state.notifyDraft, { key: "tg", kind: "channel", on: false });
  await flushMicrotasks();
  assert.equal(seen.length, 1);
  assert.deepEqual(seen[0].body, { notify: ["human_review", "done"], notify_channels: ["tg"] });
  assert.equal(state.notifyError, null);
});

test("the channel hint names what the ticket will actually do", () => {
  const state = notifyState();
  state.configCache.channels = ["tg", "ops"];
  const hint = (form) => state.notifyChannelHint(form);

  // No status beats every channel state.
  assert.match(hint({ notify: [], notifyChannels: ["ops"], path: "~/projects/kontora" }), /^Silent\./);
  assert.match(hint({ notify: ["done"], notifyChannels: ["none"], path: "" }), /^Silenced for this ticket\./);
  assert.match(hint({ notify: ["done"], notifyChannels: ["ops"], path: "~/projects/kontora" }), /^Overrides notifications\.default /);
  assert.match(hint({ notify: ["done"], notifyChannels: [], path: "~/projects/kontora" }), /^Inherited: resolves to tg, from notifications\.default\./);
  state.configCache.projects[0].notify_channels = ["ops"];
  assert.match(hint({ notify: ["done"], notifyChannels: [], path: "~/projects/kontora" }), /^Inherited: resolves to ops, from project kontora\./);
  state.configCache.projects[0].notify_channels = [];
  state.configCache.default_channels = [];
  assert.match(hint({ notify: ["done"], notifyChannels: ["ops"], path: "~/projects/kontora" }), /^Set for this ticket because no project or default route applies\./);
});

test("the rail row is read-only exactly where the API refuses a frontmatter edit", () => {
  const state = notifyState();
  const canEdit = (status) => { state.selectedTicket = { id: "kon-1", status }; return state.notifyCanEdit(); };

  ["open", "todo", "paused", "human_review"].forEach((s) => assert.equal(canEdit(s), true, s));
  // An agent owns the file while it runs, and the terminal statuses are over.
  ["in_progress", "done", "cancelled", "archived"].forEach((s) => assert.equal(canEdit(s), false, s));

  state.configCache.custom_statuses = ["blocked"];
  assert.equal(canEdit("blocked"), true);
});

test("a rail edit saves at once and folds the answer back into the board", async () => {
  const seen = [];
  const state = notifyState({
    fetch: async (url, opts) => {
      seen.push({ url, body: JSON.parse(opts.body) });
      return { ok: true, json: async () => ({ id: "kon-1", status: "todo", notify: ["human_review", "done"], notify_channels: [], body: "" }) };
    },
  });
  state.tickets = [{ id: "kon-1", status: "todo" }];
  state.selectedTicket = { id: "kon-1", status: "todo", path: "~/projects/kontora", notify: ["paused"], notify_channels: ["tg"] };

  state.openNotifyEditor();
  assert.deepEqual(vmValue(state.notifyDraft.notify), ["paused"]);

  state.notifyPickMode(state.notifyDraft, "done");
  await flushMicrotasks();

  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, "/api/tickets/kon-1");
  // Both fields on every request: they are edited together, and the channel
  // list has to survive a change to the statuses.
  assert.deepEqual(seen[0].body, { notify: ["human_review", "done"], notify_channels: ["tg"] });
  assert.deepEqual(vmValue(state.selectedTicket.notify), ["human_review", "done"]);
});

test("the rail editor does not carry one ticket's statuses onto the next", async () => {
  const state = notifyState({
    fetch: async () => ({ ok: true, json: async () => ({ id: "kon-2", status: "todo", body: "", notes: [] }) }),
  });
  state.$nextTick = () => Promise.resolve();
  state.selectedTicket = { id: "kon-1", status: "todo", path: "", notify: ["paused"], notify_channels: [] };
  state.openNotifyEditor();
  assert.equal(state.notifyEditing, true);

  await state.selectTicket({ id: "kon-2", status: "todo" });
  await flushMicrotasks();

  assert.equal(state.notifyEditing, false);
  assert.equal(state.notifyDraft, null);
});

// No refresh path may POST a message.
const COMPANION_SNAPSHOT = {
  enabled: true, name: "Kotik", ai_available: false, hint: "Set assistant.agent for explanations.",
  health: "ok", revision: 7, caught_up: 0, observed_at: "2026-11-03T10:00:00Z",
  items: [
    { id: "q/1", ticket_id: "kon-1", condition: "question", title: "A question", detail: "Continue?", revision: 3, active: true, read: false },
    { id: "r2", ticket_id: "kon-2", condition: "human_review", title: "Review work", revision: 4, active: true, read: true },
    { id: "p3", ticket_id: "kon-3", condition: "paused", title: "Paused", revision: 2, active: false, read: false, inactive_reason: "no_longer_observed", identity_limited: true },
  ],
};

function companionState(opts = {}) {
  return assistantState({ ...opts, routes: {
    "/api/assistant/companion": COMPANION_SNAPSHOT,
    "/api/assistant": { enabled: false, hint: "No agent" },
    ...opts.routes,
  } });
}

test("companion inbox works without an agent and never starts a passive turn", async () => {
  const { state, requests, listeners } = companionState();
  await state.initAssistant();
  await state.companionInit();
  assert.equal(state.assistantEnabled(), false);
  assert.equal(state.assistantView, "inbox");
  assert.equal(state.companionState(), "needs you");
  assert.equal(state.companionCounts(), "2 active / 2 unread");
  assert.equal(state.companionCanAsk(), false);
  assert.equal(state.companionCanWrite(), true);
  await state.companionSend("briefing", "Brief me", []);
  assert.equal(requests.some(r => r.init?.method === "POST"), false);
  assert.ok(listeners.some(l => l.type === "focus"));
  state.destroy();
  assert.equal(listeners.some(l => l.type === "focus"), false);
  assert.equal(state.companionTimer, null);
});

for (const path of ["GET", "PATCH", "PATCH with failed refresh"]) {
  for (const view of ["inbox", "thread", "history"]) {
    test(`disabled ${path} snapshot reconciles ${view} without changing a running ordinary chat`, async () => {
      const { state, routes, requests } = companionState();
      await state.companionRefresh();
      state.assistantThread = { id: "abc123", running: true };
      state.assistantMessages = [{ n: 1, text: "Ordinary question" }];
      state.assistantDraft = "Unsent text";
      state.assistantView = view;
      state._assistantSetStreaming(true);
      const thread = state.assistantThread, messages = state.assistantMessages;
      const disabled = { ...COMPANION_SNAPSHOT, enabled: false, revision: 8 };
      routes["/api/assistant/companion"] = path === "PATCH with failed refresh" ? { __status: 503, error: "offline" } : disabled;
      routes["/api/assistant/companion/items/q%2F1"] = disabled;
      if (path === "GET") await state.companionRefresh();
      else await state.companionRead(COMPANION_SNAPSHOT.items[0], true);
      await flushMicrotasks();
      assert.equal(state.companionEnabled(), false);
      assert.equal(state.assistantView, view === "inbox" ? "thread" : view);
      assert.equal(state.assistantThread, thread);
      assert.equal(state.assistantMessages, messages);
      assert.equal(state.assistantDraft, "Unsent text");
      assert.equal(state.assistantStreaming, true);
      assert.equal(state.companionDisconnected, path === "PATCH with failed refresh");
      state.companionOpenInbox();
      assert.notEqual(state.assistantView, "inbox");
      assert.equal(requests.some(r => r.init?.method === "POST"), false);
      state.destroy();
    });
  }
}

for (const snapshot of [null, { ...COMPANION_SNAPSHOT, enabled: false }, COMPANION_SNAPSHOT,
  { ...COMPANION_SNAPSHOT, health: "degraded" }]) {
  test(`companion visibility follows resolved snapshot: ${snapshot ? snapshot.enabled + '/' + snapshot.health : 'unknown'}`, () => {
    const { state, requests } = companionState();
    state.companionSnapshot = snapshot;
    state.settingsConfig = { assistant: { companion: { enabled: !snapshot?.enabled } } };
    assert.equal(state.companionEnabled(), Boolean(snapshot?.enabled));
    if (!snapshot?.enabled) {
      state.companionOpenInbox();
      assert.equal(state.assistantView, "thread");
      assert.equal(requests.length, 0);
    }
    state.destroy();
  });
}

for (const running of [false, true]) {
  test(`disabling a selected companion hides its transcript without changing state: running=${running}`, async () => {
    const { state, requests } = companionState();
    state.assistantThread = { id: "ksaved", purpose: "companion", running, evidence: [{ revision: 1 }] };
    state.assistantMessages = [{ n: 1, text: "Saved question" }];
    state.assistantDraft = "Unsent follow-up";
    state._assistantSetStreaming(running);
    const thread = state.assistantThread, messages = state.assistantMessages;
    state._companionAdoptSnapshot({ ...COMPANION_SNAPSHOT, enabled: false });
    assert.equal(state.assistantThreadVisible(), false);
    assert.equal(state.companionThread(), true, "hidden companion threads must not become ordinary chats");
    assert.equal(state.assistantStreaming, running);
    assert.equal(state.assistantThread, thread);
    assert.equal(state.assistantMessages, messages);
    assert.equal(state.assistantDraft, "Unsent follow-up");
    await state.sendAssistantMessage();
    assert.equal(requests.some(r => r.init?.method === "POST"), false);
    state._companionAdoptSnapshot(COMPANION_SNAPSHOT);
    assert.equal(state.assistantThreadVisible(), true);
    assert.equal(state.assistantThread, thread);
    state._companionAdoptSnapshot({ ...COMPANION_SNAPSHOT, enabled: false });
    state._assistantCursor = 42;
    state._assistantETag = "companion-activity";
    state.assistantShowChat();
    assert.equal(state.assistantThread, null);
    assert.equal(state.assistantThreadVisible(), true);
    assert.equal(state.assistantView, "thread");
    assert.equal(state.assistantDraft, "Unsent follow-up");
    assert.equal(state._assistantCursor, 0);
    assert.equal(state._assistantETag, "");
    state.destroy();
  });
}

for (const delayed of ["response", "json"]) {
  test(`companion activity cannot enter ordinary Chat after hidden selection is cleared: delayed ${delayed}`, async () => {
    const response = deferred();
    const body = deferred();
    const payload = { running: true, messages: [{ n: 1, text: "Hidden companion question" }],
      tape: { events: [{ kind: "text", text: "Hidden companion reply" }] }, partial: "Hidden partial", partial_gen: 1 };
    const { state } = companionState({ fetch: async () => response.promise });
    state.assistantThread = { id: "ksaved", purpose: "companion" };
    state._companionAdoptSnapshot(COMPANION_SNAPSHOT);
    const pending = state._loadAssistantActivity();
    if (delayed === "json") {
      response.resolve({ ok: true, headers: { get: () => "old-companion" }, json: () => body.promise });
      await flushMicrotasks();
    }
    state._companionAdoptSnapshot({ ...COMPANION_SNAPSHOT, enabled: false });
    state.assistantShowChat();
    body.resolve(payload);
    response.resolve({ ok: true, headers: { get: () => "old-companion" }, json: async () => payload });
    await pending;
    assert.equal(state.assistantThread, null);
    assert.equal(state.assistantThreadVisible(), true);
    assert.deepEqual(vmValue(state.assistantMessages), []);
    assert.deepEqual(vmValue(state.assistantEvents), []);
    assert.equal(state.assistantPartial, "");
    assert.equal(state.assistantStreaming, false);
    assert.equal(state._assistantCursor, 0);
    assert.equal(state._assistantETag, "");
    assert.equal(state._assistantPoll, null);
    state.destroy();
  });
}

test("reading an item changes only companion state with its expected revision", async () => {
  const { state, requests, routes } = companionState();
  await state.companionRefresh();
  const item = state.companionSnapshot.items[0];
  routes["/api/assistant/companion/items/q%2F1"] = {
    ...COMPANION_SNAPSHOT, revision: 8,
    items: COMPANION_SNAPSHOT.items.map(i => i.id === item.id ? { ...i, read: true, revision: 4 } : i),
  };
  routes["/api/assistant/companion"] = routes["/api/assistant/companion/items/q%2F1"];
  await state.companionRead(item, true);
  await flushMicrotasks();
  const writes = requests.filter(r => r.init?.method);
  assert.equal(writes.length, 1);
  assert.equal(writes[0].url, "/api/assistant/companion/items/q%2F1");
  assert.deepEqual(JSON.parse(writes[0].init.body), { revision: 3, read: true });
  assert.equal(state.companionSnapshot.items[0].active, true);
  assert.equal(state.companionSnapshot.items[0].read, true);
  assert.equal(state.companionCounts(), "2 active / 1 unread");
  assert.equal(state.companionState(), "needs you", "read attention still needs a person");
});

test("caught-up checkpoint sends the snapshot revision, not a ticket operation", async () => {
  const { state, requests } = companionState();
  await state.companionRefresh();
  await state.companionCatchUp();
  const write = requests.find(r => r.init?.method === "PATCH");
  assert.equal(write.url, "/api/assistant/companion");
  assert.deepEqual(JSON.parse(write.init.body), { revision: 7 });
  assert.equal(state.companionCounts(), "2 active / 2 unread", "a checkpoint does not mark items read");
});

test("read conflicts refetch shared state and keep the failed-save message", async () => {
  const { state, routes, requests } = companionState();
  await state.companionRefresh();
  routes["/api/assistant/companion/items/q%2F1"] = { __status: 409, error: "revision changed" };
  routes["/api/assistant/companion"] = { ...COMPANION_SNAPSHOT, revision: 9 };
  await state.companionRead(state.companionSnapshot.items[0], true);
  assert.equal(state.companionSnapshot.revision, 9);
  assert.match(state.companionError, /Read state was not saved.*revision changed/);
  assert.equal(requests.at(-1).url, "/api/assistant/companion");
  assert.equal(state.companionSnapshot.items[0].read, false);
});

test("degraded and disconnected attention stays visible and blocks read writes", async () => {
  const { state, routes } = companionState();
  await state.companionRefresh();
  routes["/api/assistant/companion"] = { ...COMPANION_SNAPSHOT, health: "degraded", error: "storage is read-only" };
  await state.companionRefresh();
  assert.equal(state.companionState(), "degraded");
  assert.equal(state.companionEnabled(), true);
  assert.equal(state.companionCanWrite(), false);
  routes["/api/assistant/companion"] = { __status: 503, error: "offline" };
  await state.companionRefresh();
  assert.equal(state.companionState(), "disconnected");
  assert.equal(state.companionItems().length, 3, "a failed fetch must not empty saved attention");
  assert.equal(state.companionEnabled(), true);
  assert.match(state.companionError, /may be stale/);
  routes["/api/assistant/companion"] = COMPANION_SNAPSHOT;
  await state.companionRefresh();
  assert.equal(state.companionDisconnected, false);
  assert.equal(state.companionError, "");
});

test("SSE invalidation and reconnect rebuild the inbox without payload data or model calls", async () => {
  const { state, streams, routes, requests, runTimers } = companionState({ manualTimers: true });
  state.connectSSE();
  streams[0].emit("open", {});
  await flushMicrotasks();
  routes["/api/assistant/companion"] = { ...COMPANION_SNAPSHOT, enabled: false, revision: 8, items: [] };
  streams[0].emit("companion_updated", "");
  await flushMicrotasks();
  assert.equal(state.companionItems().length, 0);
  assert.equal(state.companionEnabled(), false);
  assert.equal(state.assistantView, "thread");
  streams[0].onerror();
  assert.equal(state.companionState(), "disconnected");
  runTimers();
  routes["/api/assistant/companion"] = { ...COMPANION_SNAPSHOT, revision: 10 };
  streams[1].emit("open", {});
  await flushMicrotasks();
  assert.equal(state.companionSnapshot.revision, 10);
  assert.equal(state.companionEnabled(), true);
  assert.equal(state.assistantView, "thread");
  assert.equal(state.companionCounts(), "2 active / 2 unread");
  assert.equal(requests.some(r => r.init?.method === "POST"), false);
  state.destroy();
});

test("focus and the 30 second fallback revalidate settled evidence without attention changes", async () => {
  const handlers = new Map();
  const intervals = new Map();
  const calls = [];
  let outdated = false, enabled = false;
  const state = loadKontoraState({
    window: { innerWidth: 1200, addEventListener: (name, fn) => handlers.set(name, fn), removeEventListener: name => handlers.delete(name) },
    setInterval: (fn, ms) => { intervals.set(ms, fn); return ms; },
    clearInterval: id => intervals.delete(id),
    fetch: async (url, init) => {
      calls.push({ url, init });
      const body = url === "/api/assistant/companion" ? { ...COMPANION_SNAPSHOT, enabled } :
        { running: false, evidence: [{ revision: 7, outdated }] };
      return { ok: true, json: async () => body };
    },
  });
  state.$nextTick = cb => { if (cb) cb(); return Promise.resolve(); };
  state.assistantOpen = true;
  state.assistantThread = { id: "ksettled", purpose: "companion", running: false };
  await state.companionInit();
  await flushMicrotasks();
  assert.ok(intervals.has(30000));
  assert.equal(state.companionEnabled(), false);
  assert.equal(state.companionEvidence()[0].outdated, false);
  enabled = true;
  outdated = true;
  handlers.get("focus")();
  await flushMicrotasks();
  assert.equal(state.companionEvidence()[0].outdated, true);
  assert.equal(state.companionEnabled(), true);
  assert.equal(state.assistantView, "thread");
  state.assistantThread.evidence[0].outdated = false;
  intervals.get(30000)();
  await flushMicrotasks();
  assert.equal(state.companionEvidence()[0].outdated, true);
  assert.equal(calls.filter(r => r.url === "/api/assistant/companion").length, 3);
  assert.equal(calls.filter(r => r.url.includes("/ksettled/activity")).length, 3);
  assert.equal(calls.some(r => r.init?.method === "POST"), false);
  const count = calls.length;
  state.destroy();
  assert.equal(intervals.size, 0);
  assert.equal(handlers.size, 0);
  await state.companionRefresh();
  assert.equal(calls.length, count);
});

test("an old disabled inbox response cannot overwrite a newer snapshot or selection", async () => {
  let release;
  const readSnapshot = { ...COMPANION_SNAPSHOT, revision: 8, items: [] };
  const { state } = companionState({ fetch: async (url, init) => {
    if (init?.method === "PATCH") return { ok: true, json: async () => readSnapshot };
    return new Promise(resolve => { release = () => resolve({ ok: true, json: async () => ({ ...COMPANION_SNAPSHOT, enabled: false }) }); });
  } });
  state.companionSnapshot = COMPANION_SNAPSHOT;
  state.assistantView = "inbox";
  const pending = state.companionRefresh();
  await state.companionRead(COMPANION_SNAPSHOT.items[0], true);
  release();
  await pending;
  // The queued reconciliation may now fetch again; the fenced first GET cannot paint revision 7.
  assert.equal(state.companionSnapshot.revision, 8);
  assert.equal(state.companionEnabled(), true);
  assert.equal(state.assistantView, "inbox");
  state.destroy();
});

test("an old disabled PATCH response cannot replace a newer snapshot or selection", async () => {
  const patch = deferred();
  const { state } = companionState({ fetch: async (url, init) => {
    if (init?.method === "PATCH") return patch.promise;
    return { ok: true, json: async () => ({ ...COMPANION_SNAPSHOT, revision: 9 }) };
  } });
  state.companionSnapshot = COMPANION_SNAPSHOT;
  state.assistantView = "inbox";
  const pending = state.companionRead(COMPANION_SNAPSHOT.items[0], true);
  await state.companionRefresh();
  patch.resolve({ ok: true, json: async () => ({ ...COMPANION_SNAPSHOT, enabled: false, revision: 8 }) });
  await pending;
  await flushMicrotasks();
  assert.equal(state.companionSnapshot.revision, 9);
  assert.equal(state.companionEnabled(), true);
  assert.equal(state.assistantView, "inbox");
  state.destroy();
});

test("explicit companion explanations keep counts visible while thinking and use the dedicated followup route", async () => {
  const thread = { id: "k123", purpose: "companion", running: true, evidence: [{ captured_at: "2026-11-03T10:00:00Z", sources: { "kon-1": "r1" }, revision: 7, omissions: ["Activity truncated"], outdated: false }] };
  const { state, requests } = companionState({ routes: {
    "/api/assistant/companion": { ...COMPANION_SNAPSHOT, ai_available: true },
    "/api/assistant/companion/messages": thread,
    "/api/assistant/threads/k123/activity": { running: true, evidence: thread.evidence },
  } });
  await state.companionRefresh();
  state.assistantOpen = true;
  await state.companionSend("investigation", "Explain this ticket", ["kon-1"]);
  assert.equal(state.assistantThread.purpose, "companion");
  assert.equal(state.assistantView, "thread");
  assert.equal(state.assistantMessages[0].text, "Explain this ticket", "requested text stays visible before the turn is recorded");
  assert.equal(state.companionState(), "thinking");
  assert.equal(state.companionCounts(), "2 active / 2 unread");
  assert.equal(state.assistantAutonomy, "ask", "companion admission does not change general chat autonomy");
  state.setAssistantAutonomy("auto");
  assert.equal(state.assistantAutonomy, "ask");
  state._assistantSetStreaming(false);
  state.assistantDraft = "What evidence is missing?";
  await state.sendAssistantMessage();
  const sends = requests.filter(r => r.init?.method === "POST");
  assert.equal(sends.length, 2);
  assert.ok(sends.every(r => r.url === "/api/assistant/companion/messages"));
  assert.deepEqual(JSON.parse(sends[1].init.body), { thread_id: "k123", kind: "followup", text: "What evidence is missing?", ticket_ids: [] });
  state._mergeAssistantActivity({ running: false, evidence: [{ ...thread.evidence[0], outdated: true }] });
  assert.equal(state.companionEvidence()[0].outdated, true);
  state.destroy();
});

for (const failure of [null, "network", "HTTP 503"]) {
  test(`a disabled companion cannot start another explanation but its running turn stays stoppable: ${failure || 'success'}`, async () => {
    let stopFailure = failure;
    const { state, requests } = companionState({ fetch: async () => {
      if (stopFailure === "network") throw new Error("Stop network failure");
      if (stopFailure) return { ok: false, status: 503, json: async () => ({ error: "Stop unavailable" }) };
      return { ok: true };
    } });
    state.companionSnapshot = { ...COMPANION_SNAPSHOT, enabled: false, ai_available: true };
    state.assistantThread = { id: "krunning", purpose: "companion", running: true };
    state._assistantSetStreaming(true);
    assert.equal(state.companionState(), "thinking");
    assert.equal(state.companionCanAsk(), false);
    await state.companionSend("followup", "More detail", []);
    await state.stopAssistantTurn();
    const sends = requests.filter(r => r.init?.method === "POST");
    assert.equal(sends.length, 1);
    assert.equal(sends[0].url, "/api/assistant/threads/krunning/stop");
    if (failure) {
      assert.match(state.assistantError, /^Stop /);
      assert.equal(state.assistantStreaming, true);
      stopFailure = null;
      await state.stopAssistantTurn();
    }
    assert.equal(state.assistantError, null);
    state.destroy();
  });
}

for (const kind of ["briefing", "investigation"]) {
  for (const navigation of ["unchanged", "Chat", "History", "History then Chat", "re-enabled"]) {
    test(`disabled snapshot preserves an admitted Inbox ${kind} unless navigation changes: ${navigation}`, async () => {
      const admission = deferred();
      const thread = { id: "kadmitted", purpose: "companion", running: true };
      const { state, requests } = companionState({ fetch: async url => {
        if (url === "/api/assistant/companion/messages") return admission.promise;
        return { ok: true, json: async () => url.includes("/activity") ? { running: true } : {} };
      } });
      state.companionSnapshot = { ...COMPANION_SNAPSHOT, ai_available: true };
      state.assistantOpen = true;
      state.assistantView = "inbox";
      const pending = state.companionSend(kind, "Explain this attention", ["kon-1"]);
      state._companionAdoptSnapshot({ ...COMPANION_SNAPSHOT, enabled: false });
      assert.equal(state.assistantView, "thread");
      if (navigation === "Chat") state.assistantShowChat();
      if (navigation.startsWith("History")) state.assistantShowHistory();
      if (navigation === "History then Chat") state.assistantShowChat();
      if (navigation === "re-enabled") state._companionAdoptSnapshot(COMPANION_SNAPSHOT);
      const adopts = navigation === "unchanged" || navigation === "re-enabled";
      admission.resolve({ ok: true, json: async () => thread });
      await pending;
      assert.equal(state.assistantThread?.id || null, adopts ? thread.id : null);
      assert.equal(state.assistantStreaming, adopts);
      assert.equal(state.assistantThreadVisible(), !adopts || navigation === "re-enabled");
      assert.equal(state.assistantView, navigation === "History" ? "history" : "thread");
      if (adopts) {
        await state.stopAssistantTurn();
        assert.equal(requests.at(-1).url, "/api/assistant/threads/kadmitted/stop");
      }
      state.destroy();
    });
  }
}

for (const revision of [7, 8]) {
  test(`attention refresh revalidates settled evidence at revision ${revision}`, async () => {
    const { state, requests, routes, listeners } = companionState({ etag: "current", routes: {
      "/api/assistant/threads/kold/activity": { running: false, evidence: [{ revision: 7, outdated: false }] },
    } });
    state.assistantThread = { id: "kold", purpose: "companion", running: false };
    state.assistantOpen = true;
    await state.companionInit();
    await flushMicrotasks();
    assert.equal(state.companionEvidence()[0].outdated, false);
    assert.equal(state._assistantPoll, null, "settled chats have no activity poll");
    routes["/api/assistant/companion"] = { ...COMPANION_SNAPSHOT, revision };
    routes["/api/assistant/threads/kold/activity"] = { running: false, evidence: [{ revision: 7, outdated: true }] };
    listeners.find(l => l.type === "focus").handler();
    await flushMicrotasks();
    assert.equal(state.companionEvidence()[0].outdated, true);
    const reads = requests.filter(r => r.url.includes("/kold/activity"));
    assert.equal(reads.length, 2);
    assert.ok(reads.every(r => !r.init.headers["If-None-Match"]), "evidence must be revalidated without the old activity ETag");
    assert.equal(requests.some(r => r.init?.method === "POST"), false);
    state.destroy();
  });
}

for (const change of ["chat", "draft", "view", "roundtrip", "pending selection", "new chat"]) {
  test(`delayed companion POST preserves a changed ${change}`, async () => {
    const admission = deferred();
    const selection = deferred();
    const created = deferred();
    const original = { id: "ksource", purpose: "companion", running: false };
    const other = { id: "abc123", running: false, autonomy: "ask" };
    const { state, requests, store } = companionState({ store: { "kontora-assistant-thread": original.id }, fetch: async (url, init) => {
      if (url === "/api/assistant/companion/messages") return admission.promise;
      if (url === "/api/assistant/threads" && init?.method === "POST") return created.promise;
      if (url === "/api/assistant/threads/abc123" && change === "pending selection") return selection.promise;
      const body = url === "/api/assistant/threads/abc123" ? other :
        url === "/api/assistant/threads/ksource" ? original :
        url === "/api/assistant/threads" ? { threads: [original] } : { running: false };
      return { ok: true, json: async () => body };
    } });
    state.assistantAgent = { enabled: true };
    state.companionSnapshot = { ...COMPANION_SNAPSHOT, ai_available: true };
    state.assistantThread = original;
    state.assistantDraft = "Explain the capture";
    const pending = state.sendAssistantMessage();
    assert.equal(state.companionBusy, true);
    let navigation;
    if (change === "chat") await state.assistantSelectThread(other.id);
    if (change === "roundtrip") {
      await state.assistantSelectThread(other.id);
      await state.assistantSelectThread(original.id);
    }
    if (change === "view") state.assistantShowHistory();
    if (change === "pending selection") navigation = state.assistantSelectThread(other.id);
    if (change === "new chat") navigation = state.assistantNewThread();
    state.assistantDraft = "New unsent text";
    const selected = state.assistantThread;
    const view = state.assistantView;
    const savedID = store["kontora-assistant-thread"];
    admission.resolve({ ok: true, json: async () => ({ ...original, turns: 2, messages: [] }) });
    await pending;
    assert.equal(state.assistantDraft, "New unsent text");
    if (change === "draft") assert.equal(state.assistantThread.id, selected.id);
    else assert.equal(state.assistantThread, selected, "the delayed response must not adopt over a newer selection");
    assert.equal(state.assistantView, view);
    assert.equal(store["kontora-assistant-thread"], savedID);
    assert.equal(state.companionBusy, false);
    assert.ok(requests.some(r => r.url === "/api/assistant/threads" && !r.init?.method), "the accepted turn remains discoverable in history");
    if (change === "pending selection") selection.resolve({ ok: true, json: async () => other });
    if (change === "new chat") created.resolve({ ok: true, json: async () => other });
    if (navigation) await navigation;
    if (navigation) assert.equal(state.assistantThread.id, other.id);
    state.destroy();
  });
}

test("an inbox investigation preserves an unrelated draft", async () => {
  const { state } = companionState({ routes: {
    "/api/assistant/companion/messages": { id: "kinvestigation", purpose: "companion", running: false },
  } });
  state.companionSnapshot = { ...COMPANION_SNAPSHOT, ai_available: true };
  state.assistantView = "inbox";
  state.assistantDraft = "An unsent general question";
  await state.companionSend("investigation", "Investigate this ticket", ["kon-1"]);
  assert.equal(state.assistantThread.id, "kinvestigation");
  assert.equal(state.assistantView, "thread");
  assert.equal(state.assistantDraft, "An unsent general question");
  state.destroy();
});

for (const clickChat of [false, true]) {
  test(`a successful companion followup clears only its submitted draft: repeat Chat=${clickChat}`, async () => {
    const admission = deferred();
    const { state } = companionState({ fetch: async url => {
      if (url === "/api/assistant/companion/messages") return admission.promise;
      return { ok: true, json: async () => url.includes("/activity") ? { running: true } : {} };
    } });
    state.assistantOpen = true;
    state.companionSnapshot = { ...COMPANION_SNAPSHOT, ai_available: true };
    state.assistantThread = { id: "kfollowup", purpose: "companion" };
    state.assistantDraft = "  Explain the capture  ";
    const pending = state.sendAssistantMessage();
    if (clickChat) state.assistantShowChat();
    admission.resolve({ ok: true, json: async () => ({ id: "kfollowup", purpose: "companion", turns: 2, running: true }) });
    await pending;
    assert.equal(state.assistantDraft, "");
    assert.equal(state.assistantMessages[0].text, "Explain the capture");
    assert.equal(state.assistantStreaming, true);
    assert.notEqual(state._assistantPoll, null);
    state.destroy();
  });
}

for (const entry of ["startup", "desktop", "mobile", "history"]) {
  test(`disabled observation hides saved companion history and restores it on re-enable: ${entry}`, async () => {
    const thread = { id: "ksaved", purpose: "companion", running: false };
    const ordinary = { id: "abc123", running: false };
    const { state, requests, routes, store } = companionState({ store: {
      "kontora-assistant-open": entry === "startup" ? "1" : "0",
      "kontora-assistant-thread": entry === "startup" ? thread.id : "",
    }, routes: {
      "/api/assistant/companion": { ...COMPANION_SNAPSHOT, enabled: false },
      "/api/assistant/threads": { threads: [thread, ordinary] },
      "/api/assistant/threads/ksaved": thread,
      "/api/assistant/threads/ksaved/activity": { running: false, evidence: [{ revision: 1, outdated: true }] },
    } });
    await state.initAssistant();
    if (entry === "desktop") state.toggleAssistant();
    if (entry === "mobile") { state.isMobile = true; state.openAssistantSheet(); }
    if (entry === "history") state.assistantView = "history";
    await state.assistantLoadThreads();
    assert.equal(state.assistantEnabled(), false);
    assert.deepEqual(vmValue(state.assistantVisibleThreads()), [ordinary]);
    assert.equal(state.assistantThread, null);
    assert.equal(await state.assistantSelectThread(thread.id), false);
    assert.equal(requests.some(r => r.url.includes("/ksaved/activity")), false);
    if (entry === "startup") assert.equal(store["kontora-assistant-thread"], thread.id);
    const view = state.assistantView;
    routes["/api/assistant/companion"] = COMPANION_SNAPSHOT;
    await state.companionRefresh();
    assert.equal(state.assistantView, view);
    assert.deepEqual(vmValue(state.assistantVisibleThreads()), [thread, ordinary]);
    assert.equal(await state.assistantSelectThread(thread.id), true);
    assert.equal(state.companionEvidence()[0].outdated, true);
    assert.equal(state.companionCanAsk(), false);
    assert.equal(requests.some(r => r.init?.method), false);
    state.destroy();
  });
}

for (const enabled of [false, true]) {
  test(`ordinary remembered chats restore independently of observation: ${enabled}`, async () => {
    const thread = { id: "abc123", running: false };
    const { state } = companionState({ store: { "kontora-assistant-thread": thread.id }, routes: {
      "/api/assistant": { enabled: true },
      "/api/assistant/companion": { ...COMPANION_SNAPSHOT, enabled },
      "/api/assistant/threads/abc123": thread,
    } });
    await state.initAssistant();
    assert.equal(state.assistantThread.id, thread.id);
    assert.equal(state.assistantThreadVisible(), true);
    state.destroy();
  });
}

for (const enabled of [false, true, null]) {
  for (const navigation of ["unchanged", "Chat", "History", "ordinary thread"]) {
    test(`remembered companion waits for an initial snapshot without replacing navigation: ${enabled}/${navigation}`, async () => {
      const snapshot = deferred();
      const thread = { id: "ksaved", purpose: "companion", running: false };
      const ordinary = { id: "abc123", running: false };
      const { state } = companionState({ store: { "kontora-assistant-thread": thread.id }, fetch: async url => {
        if (url === "/api/assistant/companion") return snapshot.promise;
        const body = url === "/api/assistant/threads/ksaved" ? thread :
          url === "/api/assistant/threads/abc123" ? ordinary : {};
        return { ok: true, json: async () => body };
      } });
      const refresh = state.companionRefresh();
      const restore = state.initAssistant();
      await flushMicrotasks();
      assert.equal(state.assistantThread, null);
      assert.equal(state.companionEnabled(), false);
      if (navigation === "Chat") state.assistantShowChat();
      if (navigation === "History") state.assistantShowHistory();
      if (navigation === "ordinary thread") await state.assistantSelectThread(ordinary.id);
      snapshot.resolve({ ok: enabled !== null, status: enabled === null ? 503 : 200,
        json: async () => enabled === null ? { error: "offline" } : { ...COMPANION_SNAPSHOT, enabled } });
      await Promise.all([refresh, restore]);
      const expected = navigation === "ordinary thread" ? ordinary.id :
        enabled && navigation === "unchanged" ? thread.id : null;
      assert.equal(state.assistantThread?.id || null, expected);
      assert.equal(state.assistantView, navigation === "History" ? "history" : "thread");
      state.destroy();
    });
  }
}

for (const mobile of [false, true]) {
  for (const alreadySelected of [false, true]) {
    test(`inbox links leave Stats and show ticket detail (mobile=${mobile}, selected=${alreadySelected})`, async () => {
      const ticket = { id: "kon-1", title: "A question", status: "open", body: "" };
      const { state, requests } = companionState({ routes: { "/api/tickets/kon-1": ticket } });
      state.isMobile = mobile;
      state.currentView = "stats";
      state.sheet = mobile ? { type: "assistant" } : null;
      state.tickets = [ticket];
      if (alreadySelected) state.selectedTicket = ticket;
      await state.companionOpenTicket(COMPANION_SNAPSHOT.items[0]);
      assert.equal(state.currentView, "board");
      assert.equal(state.selectedTicket.id, ticket.id);
      assert.equal(state.routeHash(), "#/t/kon-1");
      assert.equal(state.sheet, null);
      assert.equal(requests.filter(r => r.url === "/api/tickets/kon-1").length, alreadySelected ? 0 : 1);
      state.destroy();
    });
  }
}

test("inbox links respect dirty Settings without selecting a hidden ticket", async () => {
  const state = await settingsState();
  state.settingsConfig.general.branch_prefix = "unsaved";
  state.currentView = "settings";
  state.isMobile = true;
  state.sheet = { type: "assistant" };
  assert.equal(state.settingsDirty(), true);
  await state.companionOpenTicket(COMPANION_SNAPSHOT.items[0]);
  assert.equal(state.currentView, "settings");
  assert.equal(state.settingsGuard, true);
  assert.deepEqual(vmValue(state.settingsGuardTarget), { view: 'board', ticketId: 'kon-1' });
  assert.equal(state.selectedTicket, null);
  assert.equal(state.sheet, null, 'the sheet must release focus so the Settings guard is operable');
  assert.equal(state.settingsConfig.general.branch_prefix, "unsaved");
  state.destroy();
});

test("companion inbox distinguishes disabled storage, completion, and the changes checkpoint", () => {
  const { state } = companionState();
  assert.match(state.companionInboxMarkup, /Observation is disabled\. Saved attention remains on disk\. Re-enable the companion in Assistant settings to load it\./);
  assert.match(state.companionInboxMarkup, />Set changes checkpoint<\/button>/);
  assert.match(state.companionInboxMarkup, /Future explanations omit inactive changes at or before this checkpoint\. Read state does not change\./);
  assert.match(state.companionInboxMarkup, /x-text="companionInactiveReason\(item\)"/);
  for (const [reason, label] of [["completed", "Completed"], ["interrupted", "Interrupted"], ["no_longer_observed", "No longer observed"], ["", "No longer observed"]]) {
    assert.equal(state.companionInactiveReason({ inactive_reason: reason }), label);
  }
});

test("desktop and mobile reuse the companion inbox, state text, evidence, and missing-agent hint", () => {
  const html = fs.readFileSync(htmlPath, "utf8");
  const { state } = companionState();
  for (const key of ["companionHeaderMarkup", "companionInboxMarkup", "companionEvidenceMarkup"]) {
    assert.equal(html.split(`x-html="${key}"`).length - 1, 2, key);
  }
  assert.match(state.companionHeaderMarkup, /role="status" x-text="companionState\(\)"/);
  assert.match(state.companionHeaderMarkup, /companionCounts\(\)/);
  assert.match(state.companionHeaderMarkup, /class="companion-header" x-show="companionEnabled\(\)" x-cloak/);
  assert.match(state.companionHeaderMarkup, /x-show="companionEnabled\(\)" x-cloak @click="companionOpenInbox\(\)"/);
  assert.match(state.companionHeaderMarkup, /@click="assistantShowChat\(\)"/);
  assert.match(state.companionHeaderMarkup, /@click="assistantShowHistory\(\)"/);
  assert.match(html, /id="assistant-history" @click="assistantView === 'history' \? assistantShowChat\(\) : assistantShowHistory\(\)"/);
  assert.match(state.companionHeaderMarkup, /x-text="companionEnabled\(\) && companionThread\(\) \? companionName\(\) \+ ' chat' : 'Chat'"/);
  assert.match(state.companionHeaderMarkup, /x-show="companionThread\(\) && assistantStreaming" @click="stopAssistantTurn\(\)"/);
  assert.match(state.companionHeaderMarkup, /x-show="companionEnabled\(\) && companionThread\(\) && !companionSnapshot\?\.ai_available"/);
  assert.equal(html.split(`x-show="companionEnabled() && assistantView === 'inbox'" x-cloak`).length - 1, 2);
  assert.equal(html.split(`x-show="assistantView === 'thread' && assistantThreadVisible()"`).length - 1, 2);
  assert.equal(html.split(`x-for="t in assistantVisibleThreads()"`).length - 1, 2);
  assert.match(html, /class="companion-rail-cat" x-show="companionEnabled\(\)" x-cloak/);
  assert.match(state.companionEvidenceMarkup, /x-show="companionEnabled\(\) && companionThread\(\)" x-cloak/);
  assert.match(state.companionInboxMarkup, /Reading an item does not answer a question or change a ticket/);
  assert.match(state.companionInboxMarkup, /companionSnapshot\?\.hint/);
  assert.match(state.companionEvidenceMarkup, /packet\.sources/);
  const css = fs.readFileSync(path.join(dirname, "../../../hack/tailwind.css"), "utf8");
  assert.match(css, /prefers-reduced-motion:reduce/);
  assert.match(css, /image-rendering:pixelated/);
  const sprite = fs.readFileSync(path.join(dirname, "../static/kotik.png"));
  assert.equal(sprite.subarray(1, 4).toString(), "PNG");
  assert.equal(sprite.readUInt32BE(16), 96);
  assert.equal(sprite.readUInt32BE(20), 32);
});

test("companion settings round-trip independently without rewriting the assistant config", async () => {
  const state = await settingsState(SETTINGS_ASSISTANT_FIXTURE);
  state.settingsConfig.assistant.agent = "";
  state.settingsConfig.assistant.companion.enabled = true;
  state.settingsConfig.assistant.companion.name = "  Miso  ";
  assert.equal(state.settingsSectionSummary("assistant"), "Miso inbox");
  assert.deepEqual([...state.settingsChangedPaths()].sort(), ["assistant.agent", "assistant.companion.enabled", "assistant.companion.name"]);
  const out = await state._settingsWrite();
  assert.match(out, /companion:\n\s+enabled: true\n\s+name: Miso/);
  assert.match(out, /# the chat beside the board/);
  state.settingsConfig.assistant.companion.name = "x".repeat(65);
  assert.match(state._settingsAssistantErrors().join(" "), /assistant.companion.name/);
});

for (const sameStage of [false, true]) {
  for (const delayed of ['response', 'json']) {
    for (const outcome of ['success', 'http', 'json', 'network']) {
      if (delayed === 'json' && outcome === 'network') continue;
      test(`stage logs ignore obsolete ${outcome}: delayed ${delayed}, same stage=${sameStage}`, async () => {
        const requests = [];
        const state = loadKontoraState({ fetch: () => {
          const pending = deferred();
          requests.push(pending);
          return pending.promise;
        } });
        state.selectedTicket = { id: 'logs-a', status: 'paused' };
        state.isMobile = true;
        state.detailTab = 'logs';
        const oldJSON = deferred();
        const old = state.fetchStageLogs('logs-a', 'first');
        const response = { ok: outcome !== 'http', json: () => oldJSON.promise };
        if (delayed === 'json') {
          requests[0].resolve(response);
          await flushMicrotasks();
        }
        const fresh = state.fetchStageLogs('logs-a', sameStage ? 'first' : 'second');
        const expectedStage = sameStage ? 'first' : 'second';
        if (delayed === 'response') {
          if (outcome === 'network') requests[0].reject(new Error('old connection'));
          else requests[0].resolve(response);
        }
        if (outcome === 'json') oldJSON.reject(new Error('old JSON'));
        else oldJSON.resolve(outcome === 'http' ? { error: '<old error>' } : { content: 'old content' });
        await old;
        assert.equal(state.logViewLoading, true, 'obsolete completion cannot finish the current request');
        assert.equal(state.logViewStage, expectedStage);
        assert.equal(state.logViewContent, null);
        assert.equal(state.error, null);
        requests[1].resolve({ ok: true, json: async () => ({ content: 'latest content' }) });
        await fresh;
        assert.equal(state.logViewLoading, false);
        assert.equal(state.logViewContent, 'latest content');
        assert.equal(state.error, null);
      });
    }
  }
}

for (const boundary of ['close/reopen', 'source', 'mobile tab', 'desktop tab', 'route', 'epic', 'breakpoint', 'destroy', 'activity']) {
  for (const delayed of ['response', 'json']) {
    test(`stage-log ownership ends at ${boundary}, delayed ${delayed}`, async () => {
      const pending = deferred();
      const json = deferred();
      const identity = { id: 'logs-a', status: 'paused', kind: 'ticket', source: 'primary' };
      const epic = { id: 'epic-a', status: 'open', kind: 'epic' };
      const state = loadKontoraState({ fetch: async url => {
        if (/\/logs(?:\?|$)/.test(url)) return pending.promise;
        return { ok: true, json: async () => url.includes('epic-a') ? epic : identity };
      } });
      state.$nextTick = cb => { cb?.(); return Promise.resolve(); };
      state.selectedTicket = identity;
      state.tickets = [identity, epic];
      state.isMobile = true;
      state.detailTab = 'logs';
      state.activeTab = 'activity';
      state.fetchChain = () => {};
      state.fetchTicketCost = () => {};
      state._mountBoard = () => {};
      state.openStats = () => {};
      state._loadActivity = async () => {};
      const old = state.fetchStageLogs(identity.id, 'first');
      const response = { ok: true, json: () => json.promise };
      if (delayed === 'json') { pending.resolve(response); await flushMicrotasks(); }
      if (boundary === 'close/reopen') { state.closeDetail(false); await state.selectTicket(identity); }
      if (boundary === 'source') await state.selectTicket({ ...identity, source: 'other' });
      if (boundary === 'mobile tab') state.mobileSwitchTab('ticket');
      if (boundary === 'desktop tab') state.switchTab('ticket');
      if (boundary === 'route') await state.gotoView('stats');
      if (boundary === 'epic') await state.openEpic(epic.id);
      if (boundary === 'breakpoint') { state.isMobile = false; state._onBreakpointChange(); }
      if (boundary === 'destroy') state.destroy();
      if (boundary === 'activity') await state.fetchActivity('first', 0);
      state.logViewContent = 'current context';
      state.logViewStage = 'current';
      state.logViewLoading = false;
      state.error = null;
      pending.resolve(response);
      json.resolve({ content: 'obsolete content' });
      await old;
      assert.equal(state.logViewContent, 'current context');
      assert.equal(state.logViewStage, 'current');
      assert.equal(state.logViewLoading, false);
      assert.equal(state.error, null);
    });
  }
}

for (const destination of [
  { view: 'stats' },
  { view: 'board', ticketId: 'route-a' },
  { view: 'board', ticketId: 'route-a', source: 'other' },
  { view: 'board', epicId: 'epic-a' },
]) {
  for (const continuation of ['stay', 'discard', 'save', 'failed save']) {
    test(`dirty Settings preserves complete ${JSON.stringify(destination)} after ${continuation}`, async () => {
      const location = { hash: '#/settings' };
      const primary = { id: 'route-a', status: 'in_progress', kind: 'ticket' };
      const secondary = { ...primary, source: 'other', status: 'open' };
      const epic = { id: 'epic-a', kind: 'epic', status: 'open' };
      const state = loadKontoraState({ location, fetch: async url => ({ ok: true, json: async () => url.includes('epic-a') ? epic : url.includes('/sources/') ? secondary : primary }) });
      state.$nextTick = cb => { cb?.(); return Promise.resolve(); };
      state.currentView = 'settings';
      state.isMobile = true;
      state.tickets = [primary, epic];
      state.secondaryTickets = [secondary];
      let dirty = true;
      let opened = 0;
      state.settingsDirty = () => dirty;
      state.discardSettings = () => { dirty = false; };
      state.saveSettings = async () => { if (continuation === 'failed save') return false; dirty = false; return true; };
      state.openStats = () => {};
      state.fetchChain = () => {};
      state.fetchTicketCost = () => {};
      state.openTerminal = () => { assert.equal(state.currentView, 'board'); opened++; };
      await state.navigateDestination(destination);
      assert.equal(state.currentView, 'settings');
      assert.equal(state.selectedTicket, null);
      assert.equal(state.selectedEpic, null);
      assert.equal(opened, 0);
      assert.deepEqual(vmValue(state.settingsGuardTarget), destination);
      assert.equal(location.hash, '#/settings');
      if (continuation === 'stay') { state.settingsGuard = false; return; }
      if (continuation === 'discard') await state.settingsGuardDiscard();
      else await state.settingsGuardSave();
      if (continuation === 'failed save') {
        assert.equal(state.currentView, 'settings');
        assert.equal(state.settingsGuard, true);
        assert.equal(dirty, true);
        return;
      }
      assert.equal(state.currentView, destination.view);
      assert.equal(state.settingsGuard, false);
      if (destination.ticketId) {
        assert.equal(state.selectedTicket.id, destination.ticketId);
        assert.equal(state.isSecondary(state.selectedTicket), !!destination.source);
        if (!destination.source) assert.ok(opened > 0, 'running output attaches after Board activation');
      }
      if (destination.epicId) assert.equal(state.selectedEpic.id, destination.epicId);
    });
  }
}

for (const mobile of [false, true]) {
  test(`cross-page ticket navigation publishes only its final destination: mobile=${mobile}`, async () => {
    const detail = deferred();
    const previous = { id: 'route-old', status: 'in_progress' };
    const next = { id: 'route-next', status: 'in_progress' };
    let state;
    let hash = '#/stats';
    const writes = [];
    const location = { get hash() { return hash; }, set hash(value) {
      if (hash === value) return;
      hash = value;
      writes.push(value);
      queueMicrotask(() => state.applyRoute());
    } };
    state = loadKontoraState({ location, fetch: async url => {
      assert.equal(url, '/api/tickets/route-next');
      await detail.promise;
      return { ok: true, json: async () => next };
    } });
    state.$nextTick = cb => { cb?.(); return Promise.resolve(); };
    state.isMobile = mobile;
    state.currentView = 'stats';
    state.selectedTicket = previous;
    state.tickets = [previous, next];
    state.fetchTicketCost = () => {};
    state.fetchChain = () => {};
    state.closeStats = () => {};
    const opened = [];
    state.openTerminal = () => { assert.equal(state.currentView, 'board'); opened.push(state.selectedTicket.id); };
    const opening = state._paletteOpenTicket(next);
    await flushMicrotasks();
    assert.deepEqual(writes, [], 'do not publish the old selection during a pending destination');
    assert.deepEqual(opened, [], 'do not reattach the ticket left behind Stats');
    detail.resolve();
    assert.equal(await opening, true);
    await flushMicrotasks();
    assert.deepEqual(writes, ['#/t/route-next']);
    assert.equal(state.selectedTicket.id, next.id);
    assert.ok(opened.length > 0);
    assert.ok(opened.every(id => id === next.id));
  });
}

for (const filter of ['', 'secondary-1741']) {
  test(`large corpus metadata reads scan zero tickets: ${filter || 'full board'}`, () => {
    const state = loadKontoraState();
    state.tickets = Array.from({ length: 5 }, (_, i) => ({ id: 'primary-' + i, status: 'open', title: 'Primary' }));
    let reads = 0;
    state.secondaryTickets = Array.from({ length: 2161 }, (_, i) => ({
      id: 'secondary-' + i, source: 'team', title: 'Secondary ' + i,
      get status() { reads++; return i < 1742 ? 'archived' : 'open'; },
    }));
    state.searchQuery = filter;
    state.recomputeBoard();
    const columns = state.columns;
    const archived = columns.findIndex(c => c.key === 'archived');
    state.setActiveColumn(archived);
    reads = 0;
    for (let i = 0; i < 100; i++) {
      assert.equal(state.columns, columns);
      state.mobileColumn();
      state.mobileTickets();
    }
    assert.equal(reads, 0);
    assert.ok(state.mobileTickets().length <= 100);
    assert.equal(state.secondaryTickets.length, 2161);
    if (!filter) {
      assert.equal(state.boardTickets('archived').length, 1742);
      assert.equal(state.mobileTickets().length, 100);
      state.mobileLoadMore();
      assert.equal(state.mobileTickets().length, 200);
      state.searchQuery = 'secondary-1741';
      state.recomputeBoard();
      assert.equal(state.mobileTickets().length, 1);
    } else assert.equal(state.mobileTickets().length, 1);
    state.applySecondaryUpdate({ id: 'secondary-0', source: 'team', title: 'Renamed', status: 'archived' });
    state.recomputeBoard();
    assert.equal(state.columns, columns, 'title changes retain column identity');
    state.applySecondaryUpdate({ id: 'new', source: 'other', title: 'New', status: 'tombstone' });
    state.recomputeBoard();
    assert.ok(state.columns.some(c => c.key === 'tombstone'));
    state.deleteSecondaryTicket({ id: 'new', source: 'other' });
    state.recomputeBoard();
    assert.equal(state.columns.some(c => c.key === 'tombstone'), false);
  });
}

test('withheld frames retain no event snapshots and preserve intermediate editor transitions', () => {
  const frames = new Map();
  let sequence = 0;
  const state = loadKontoraState({ requestAnimationFrame: cb => { frames.set(++sequence, cb); return sequence; }, cancelAnimationFrame: id => frames.delete(id) });
  state.updateFavicon = () => {};
  state.selectedTicket = { id: 'same', status: 'open', history: [], body: 'Reviewed' };
  state.tickets = [{ ...state.selectedTicket }];
  state.editing = true;
  state.editingBody = true;
  let saves = 0;
  state.flushEditSave = () => { saves++; return Promise.resolve(true); };
  state.queueTicketUpdate({ id: 'same', title: 'Running', status: 'in_progress', history: [] });
  state.queueTicketUpdate({ id: 'same', title: 'Editable', status: 'open', history: [] });
  for (let i = 0; i < 10000; i++) state.queueTicketUpdate({ id: 'same', title: String(i), status: 'open', history: [] });
  assert.equal(saves, 1);
  assert.equal(state.editing, false);
  assert.equal(state.editingBody, false);
  assert.equal(state.selectedTicket.title, '9999');
  assert.equal(state.selectedTicket.body, 'Reviewed');
  assert.equal(state.tickets[0].title, '9999');
  assert.equal('_pendingTicketUpdates' in state, false);
  assert.equal(frames.size, 1);
  state.flushTicketUpdates();
  assert.equal(frames.size, 0);
  assert.equal(state._boardDirty, false);
});

test('secondary sweep uses indexed slots and has no primary cost or chain effects', () => {
  const state = loadKontoraState();
  state._boardRaf = 1;
  state.tickets = [{ id: 'same', title: 'Primary', status: 'open', history: [] }];
  state.selectedTicket = { ...state.tickets[0] };
  state.chain = { nodes: [{ id: 'same' }] };
  state.secondaryTickets = Array.from({ length: 2161 }, (_, i) => ({ id: i ? 's-' + i : 'same', source: 'team', status: 'archived', title: 'Old' }));
  state.secondarySlot({ id: 'same', source: 'team' });
  state.secondaryTickets.findIndex = () => { throw new Error('full-array ingestion search'); };
  const calls = [];
  state.fetchTicketCost = () => calls.push('cost');
  state.fetchChain = () => calls.push('chain');
  state.updateFavicon = () => {};
  for (let i = 0; i < 2161; i++) state.queueTicketUpdate({ id: i ? 's-' + i : 'same', source: 'team', status: 'archived', title: 'New ' + i });
  state.flushTicketUpdates();
  assert.deepEqual(calls, []);
  assert.equal(state.selectedTicket.title, 'Primary');
  state.selectedTicket = { id: 'same', source: 'team' };
  assert.equal(state._ticketById('same').title, 'New 0');
  const slot = state.secondarySlot({ id: 'same', source: 'team' });
  state.secondaryTickets[slot] = { id: 'same', source: 'team', status: 'archived', title: 'Detail replacement' };
  assert.equal(state._ticketById('same').title, 'Detail replacement');
  state.applySecondaryUpdate({ id: 'same', source: 'other', status: 'tombstone', title: 'Namesake' });
  state.deleteSecondaryTicket({ id: 'same', source: 'team' });
  assert.equal(state.sourceTicketById('same', { source: 'team' }), undefined);
  assert.equal(state.sourceTicketById('same', { source: 'other' }).title, 'Namesake');
});

for (const secondary of [false, true]) {
  test(`snapshot decoding cannot overwrite newer events or resurrect deletions: ${secondary ? 'secondary' : 'primary'}`, async () => {
    const decode = deferred();
    const events = [];
    const identity = secondary ? { source: 'team' } : {};
    const stale = ['updated', 'deleted', 'untouched'].map(id => ({ ...identity, id, title: 'Stale', status: 'open', revision: 'old' }));
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(events), fetch: async url => {
      if (url === '/api/ticket-sources') return { ok: true, json: async () => [{ id: 'team', available: true }] };
      return { ok: true, json: () => decode.promise };
    } });
    state._boardRaf = 1;
    state.updateFavicon = () => {};
    state.fetchTicketCost = () => {};
    if (secondary) state.secondaryTickets = stale.map(t => ({ ...t }));
    else state.tickets = stale.map(t => ({ ...t }));
    state.selectedTicket = { ...stale[0], body: 'Reviewed body' };
    state.editing = true;
    state.editForm = { titleRest: 'Unsaved draft', body: 'Unsaved body' };
    state.connectSSE();
    const loading = secondary ? state.fetchTicketSources() : state.fetchTasks();
    await flushMicrotasks();
    const newer = { ...stale[0], title: 'Newer', revision: 'new' };
    events[0].emit(secondary ? 'secondary_ticket_updated' : 'ticket_updated', newer);
    events[0].emit(secondary ? 'secondary_ticket_deleted' : 'ticket_deleted', stale[1]);
    decode.resolve(secondary ? stale : { tickets: stale });
    await loading;
    const entries = secondary ? state.secondaryTickets : state.tickets;
    assert.equal(entries.find(t => t.id === 'updated').title, 'Newer');
    assert.equal(entries.some(t => t.id === 'deleted'), false);
    assert.equal(entries.some(t => t.id === 'untouched'), true);
    assert.equal(state.selectedTicket.body, 'Reviewed body');
    assert.equal(state.editForm.body, 'Unsaved body');
    if (secondary) assert.equal(state.selectedTicket.revision, 'old', 'Claim still requires the reviewed revision');
  });
}

for (const secondary of [false, true]) {
  test(`snapshot refreshes coalesce one trailing read: ${secondary ? 'secondary' : 'primary'}`, async () => {
    const first = deferred();
    let calls = 0, active = 0, maximum = 0;
    const state = loadKontoraState({ fetch: async url => {
      if (url === '/api/ticket-sources') return { ok: true, json: async () => [{ id: 'team', available: true }] };
      calls++; active++; maximum = Math.max(maximum, active);
      if (calls === 1) await first.promise;
      active--;
      return { ok: true, json: async () => secondary ? [] : { tickets: [] } };
    } });
    state.updateFavicon = () => {};
    const refresh = () => secondary ? state.fetchTicketSources() : state.fetchTasks();
    const loading = refresh();
    await flushMicrotasks();
    for (let i = 0; i < 20; i++) refresh();
    first.resolve();
    await loading;
    assert.equal(calls, 2);
    assert.equal(maximum, 1);
  });
}

for (const secondary of [false, true]) for (const editing of [false, true]) {
  test(`reconnect refreshes detail without replacing drafts or reviewed revisions: secondary=${secondary}, editing=${editing}`, async () => {
    const ticket = { id: 'reviewed', source: secondary ? 'team' : 'primary', status: 'open', title: 'Current', revision: 'new', can_edit: true };
    const requests = [];
    const state = loadKontoraState({ fetch: async url => {
      requests.push(url);
      if (url === '/api/ticket-sources') return { ok: true, json: async () => [{ id: 'team', available: true }] };
      if (url === '/api/tickets') return { ok: true, json: async () => ({ tickets: secondary ? [] : [ticket] }) };
      if (url === '/api/sources/team/tickets') return { ok: true, json: async () => secondary ? [ticket] : [] };
      assert.equal(state.selectedTicket.history.length, 1, 'list snapshots preserve detail-only fields until detail refresh');
      assert.equal(state.selectedTicket.notes.length, 1);
      return { ok: true, json: async () => ({ ...ticket, body: 'Current body', description: 'Current body', description_tail: '' }) };
    } });
    state._boardRaf = 1;
    state.updateFavicon = () => {};
    state.selectedTicket = { ...ticket, revision: 'reviewed', body: 'Reviewed body', history: [{ stage: 'code' }], notes: [{ id: 'kept' }] };
    state.editForm.body = 'Unsaved draft';
    state.editing = editing;
    if (secondary) state.secondaryTickets = [ticket];
    else state.tickets = [ticket];
    await state.resyncTickets();
    assert.equal(requests.filter(url => url.endsWith('/reviewed')).length, 1);
    assert.equal(state.selectedTicket.body, editing ? 'Reviewed body' : 'Current body');
    assert.equal(state.editForm.body, 'Unsaved draft');
    assert.equal(state.selectedTicket.revision, secondary || editing ? 'reviewed' : 'new');
  });
}

test('reconnect detail retries an overtaken decode and cannot reopen a different selection', async () => {
  const decode = deferred();
  let calls = 0;
  const state = loadKontoraState({ fetch: async () => {
    calls++;
    return { ok: true, json: () => calls === 1 ? decode.promise : Promise.resolve({ id: 'selected', status: 'open', body: 'Latest body' }) };
  } });
  state._boardRaf = 1;
  state.selectedTicket = { id: 'selected', status: 'open', body: 'Old body' };
  const refresh = state.refreshSelectedTicket();
  await flushMicrotasks();
  state.queueTicketUpdate({ id: 'selected', status: 'open', title: 'New event' });
  decode.resolve({ id: 'selected', status: 'open', body: 'Stale body' });
  await refresh;
  assert.equal(calls, 2);
  assert.equal(state.selectedTicket.body, 'Latest body');
  const late = deferred();
  state.selectedTicket = { id: 'selected', status: 'open' };
  const context = loadKontoraContext({ fetch: async () => ({ ok: true, json: () => late.promise }) });
  context.state.selectedTicket = state.selectedTicket;
  const opening = context.state.refreshSelectedTicket();
  await flushMicrotasks();
  context.state.selectedTicket = { id: 'different', body: 'Different body' };
  context.state._ticketDetailSeq++;
  late.resolve({ id: 'selected', status: 'open', body: 'Late body' });
  await opening;
  assert.equal(context.state.selectedTicket.id, 'different');
  assert.equal(context.state.selectedTicket.body, 'Different body');
});

for (const failure of ['unavailable', 'HTTP failure', 'rejection']) {
  test(`source refresh preserves an editor and its pending draft after ${failure}`, async () => {
    const streams = [], timers = new Map(), writes = [];
    let mode = failure, timer = 0, recovery;
    const ticket = { id: 'draft', source: 'team', title: 'Draft', status: 'open', can_edit: true,
      can_claim: true, revision: 'reviewed', description: 'Server text', description_tail: '' };
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams),
      setTimeout: cb => { timers.set(++timer, cb); return timer; },
      clearTimeout: id => timers.delete(id),
      fetch: async (url, options) => {
        if (options?.method === 'PUT') {
          writes.push({ url, body: JSON.parse(options.body) });
          return { ok: true, json: async () => ({ ...ticket, ...writes.at(-1).body }) };
        }
        if (url === '/api/ticket-sources') {
          if (recovery) await recovery.promise;
          return { ok: true, json: async () => [
            { id: 'team', available: mode !== 'unavailable' }, { id: 'healthy', available: true },
          ] };
        }
        if (url === '/api/sources/healthy/tickets') return { ok: true, json: async () => [{ id: 'fresh', source: 'healthy', status: 'open' }] };
        assert.equal(url, '/api/sources/team/tickets');
        if (mode === 'rejection') throw new Error('offline');
        return { ok: mode !== 'HTTP failure', json: async () => mode === 'deleted' ? [] : [ticket] };
      } });
    state.configCache = {};
    state.$nextTick = async () => {};
    state._boardRaf = 1;
    state.selectedTicket = { ...ticket };
    state.secondaryTickets = [{ ...ticket }];
    state.ticketSources = [{ id: 'team', available: true }];
    await state.startEditing();
    state.editingBody = true;
    state.editForm.body = 'Unsaved text';
    state.debounceSaveEdit();
    state.connectSSE();
    streams[0].emit('ticket_sources_changed');
    await flushMicrotasks();
    assert.equal(state.selectedTicket?.id, 'draft');
    assert.equal(state.editing, true);
    assert.equal(state.editingBody, true);
    assert.equal(state.editForm.body, 'Unsaved text');
    assert.equal(state.secondaryTickets.some(t => t.source === 'healthy'), true);
    assert.equal(state.secondaryTickets.some(t => state.sameTicket(t, ticket)), true, 'failed sources retain their last successful rows');
    assert.equal(state.ticketCanEdit(state.selectedTicket), false);
    assert.equal(state.ticketCanClaim(state.selectedTicket), false);
    assert.equal(timers.size, 1, 'source failure does not cancel the pending debounce');
    timers.clear();
    assert.equal(await state.flushEditSave(), false);
    assert.equal(state._editorSaves['team/draft'].draft.description, 'Unsaved text');
    assert.deepEqual(writes, [], 'unavailable sources refuse writes');
    mode = 'healthy';
    recovery = deferred();
    state.debounceSaveEdit();
    streams[0].emit('ticket_sources_changed');
    streams[0].emit('secondary_ticket_updated', ticket);
    await flushMicrotasks();
    assert.equal(state.editing, true, 'a recovery update must not treat stale source availability as lost edit permission');
    assert.equal(state.editingBody, true);
    assert.equal(timers.size, 1, 'recovery updates preserve the pending debounce');
    recovery.resolve();
    await flushMicrotasks();
    assert.equal(state.selectedTicket?.id, 'draft');
    assert.equal(state.editing, true);
    assert.equal(state.editForm.body, 'Unsaved text');
    assert.equal(await state.flushEditSave(), true);
    assert.deepEqual(writes, [{ url: '/api/sources/team/tickets/draft', body: { description: 'Unsaved text' } }]);
    assert.equal(state._editorSaves['team/draft'].ack.description, 'Unsaved text');
    mode = 'deleted';
    streams[0].emit('ticket_sources_changed');
    await flushMicrotasks();
    assert.equal(state.selectedTicket, null, 'healthy authoritative absence still closes the editor');
  });
}

for (const secondary of [false, true]) for (const editing of [false, true]) {
  test(`overlapping SSE opens share detail work and retry only newer events: secondary=${secondary}, editing=${editing}`, async () => {
    const streams = [], detailReads = [], lists = deferred();
    const ticket = { id: 'shared', source: secondary ? 'team' : 'primary', status: 'open', title: 'Shared',
      revision: 'new', can_edit: true };
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams), fetch: async url => {
      if (url === '/api/tickets') { await lists.promise; return { ok: true, json: async () => ({ tickets: secondary ? [] : [ticket] }) }; }
      if (url === '/api/ticket-sources') { await lists.promise; return { ok: true, json: async () => [{ id: 'team', available: true }] }; }
      if (url === '/api/sources/team/tickets') return { ok: true, json: async () => secondary ? [ticket] : [] };
      assert.equal(url, secondary ? '/api/sources/team/tickets/shared' : '/api/tickets/shared');
      const decode = deferred();
      detailReads.push(decode);
      return { ok: true, json: () => decode.promise };
    } });
    state._boardRaf = 1;
    state.updateFavicon = () => {};
    state.companionRefresh = () => {};
    state.selectedTicket = { ...ticket, revision: 'reviewed', body: 'Reviewed body' };
    state.editing = editing;
    state.editForm.body = 'Unsaved draft';
    if (secondary) state.secondaryTickets = [ticket]; else state.tickets = [ticket];
    state.connectSSE();
    for (let i = 0; i < 20; i++) streams[0].emit('open');
    lists.resolve();
    await flushMicrotasks();
    assert.equal(detailReads.length, 1, 'overlapping resync readers share one detail decode');
    streams[0].emit(secondary ? 'secondary_ticket_updated' : 'ticket_updated', { ...ticket, title: 'Newer event' });
    state.editForm.body = 'Typed during decode';
    detailReads[0].resolve({ ...ticket, body: 'Overtaken body' });
    await flushMicrotasks();
    assert.equal(detailReads.length, 2, 'one event requires one shared retry');
    const entries = secondary ? state.secondaryTickets : state.tickets;
    assert.equal(entries.find(t => t.id === 'shared').title, 'Newer event');
    if (secondary || !editing) assert.equal(state.selectedTicket.title, 'Newer event');
    detailReads[1].resolve({ ...ticket, title: 'Latest', body: 'Latest body' });
    await flushMicrotasks();
    assert.equal(detailReads.length, 2, 'successful peers cannot invalidate each other');
    assert.equal(state.selectedTicket.body, editing ? 'Reviewed body' : 'Latest body');
    assert.equal(state.selectedTicket.revision, secondary || editing ? 'reviewed' : 'new');
    assert.equal(state.editForm.body, 'Typed during decode');
    streams[0].emit('open');
    await flushMicrotasks();
    assert.equal(detailReads.length, 3, 'a later reconnect still refreshes detail');
    state.selectedTicket = { id: 'other', status: 'open', body: 'Other body' };
    state._ticketDetailSeq++;
    detailReads[2].resolve({ ...ticket, body: 'Late body' });
    await flushMicrotasks();
    assert.equal(state.selectedTicket.body, 'Other body');
  });
}

for (const failure of ['HTTP failure', 'rejection', 'decode failure']) {
  test(`SSE reconnect retries its queued primary refresh after ${failure}`, async () => {
    const streams = [], first = deferred();
    let reads = 0;
    const ticket = { id: 'recovered', status: 'open' };
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams), fetch: async url => {
      if (url === '/api/ticket-sources') return { ok: true, json: async () => [] };
      assert.equal(url, '/api/tickets');
      if (++reads === 1) return first.promise;
      return { ok: true, json: async () => ({ tickets: [ticket] }) };
    } });
    state._boardRaf = 1;
    state.updateFavicon = () => {};
    state.companionRefresh = () => {};
    state.connectSSE();
    streams[0].emit('open');
    streams[0].emit('open');
    if (failure === 'rejection') first.reject(new Error('offline'));
    else first.resolve({ ok: failure === 'decode failure', json: async () => { throw new Error('decode'); } });
    await flushMicrotasks();
    assert.equal(reads, 2);
    assert.equal(state.tickets[0]?.id, 'recovered');
    assert.equal(state.error, null);
  });
}

test('a pending reconnect detail cannot close an editor after its source becomes unavailable', async () => {
  const streams = [], decoding = deferred();
  let available = true, detailReads = 0;
  const ticket = { id: 'editing', source: 'team', status: 'open', can_edit: true, revision: 'reviewed', description: 'Reviewed' };
  const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams), fetch: async url => {
    if (url === '/api/tickets') return { ok: true, json: async () => ({ tickets: [] }) };
    if (url === '/api/ticket-sources') return { ok: true, json: async () => [{ id: 'team', available }] };
    if (url === '/api/sources/team/tickets') return { ok: true, json: async () => [ticket] };
    assert.equal(url, '/api/sources/team/tickets/editing');
    detailReads++;
    return { ok: true, json: () => decoding.promise };
  } });
  state.configCache = {};
  state.$nextTick = async () => {};
  state._boardRaf = 1;
  state.updateFavicon = () => {};
  state.companionRefresh = () => {};
  state.secondaryTickets = [ticket];
  state.selectedTicket = { ...ticket };
  await state.startEditing();
  state.editingBody = true;
  state.editForm.body = 'Pending draft';
  state.connectSSE();
  streams[0].emit('open');
  await flushMicrotasks();
  assert.equal(detailReads, 1);
  available = false;
  streams[0].emit('ticket_sources_changed');
  await flushMicrotasks();
  decoding.resolve({ ...ticket, description: 'Stale detail', revision: 'unreviewed' });
  await flushMicrotasks();
  assert.equal(state.selectedTicket?.id, 'editing');
  assert.equal(state.selectedTicket.description, 'Reviewed');
  assert.equal(state.selectedTicket.revision, 'reviewed');
  assert.equal(state.editing, true);
  assert.equal(state.editingBody, true);
  assert.equal(state.editForm.body, 'Pending draft');
  assert.equal(state.ticketCanEdit(state.selectedTicket), false);
});

test('a failed shared reconnect detail releases its flight for the next reconnect', async () => {
  const streams = [], failing = deferred();
  let reads = 0;
  const ticket = { id: 'retry-detail', status: 'open' };
  const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams), fetch: async url => {
    if (url === '/api/tickets') return { ok: true, json: async () => ({ tickets: [ticket] }) };
    if (url === '/api/ticket-sources') return { ok: true, json: async () => [] };
    assert.equal(url, '/api/tickets/retry-detail');
    if (++reads === 1) return failing.promise;
    return { ok: true, json: async () => ({ ...ticket, body: 'Recovered body' }) };
  } });
  state._boardRaf = 1;
  state.updateFavicon = () => {};
  state.companionRefresh = () => {};
  state.selectedTicket = { ...ticket, body: 'Reviewed body' };
  state.connectSSE();
  for (let i = 0; i < 20; i++) streams[0].emit('open');
  await flushMicrotasks();
  assert.equal(reads, 1);
  failing.resolve({ ok: false });
  await flushMicrotasks();
  assert.equal(state.error, 'Failed to refresh ticket details');
  assert.equal(state.selectedTicket.body, 'Reviewed body');
  streams[0].emit('open');
  await flushMicrotasks();
  assert.equal(reads, 2);
  assert.equal(state.selectedTicket.body, 'Recovered body');
});

test('a failed primary refresh without a queued reconnect still reports its error', async () => {
  const state = loadKontoraState({ fetch: async () => ({ ok: false }) });
  await assert.rejects(state.fetchTasks(), /Failed to fetch tickets/);
});

for (const [selected, arriving] of [['archived', 'abandoned'], ['tombstone', 'closed']]) {
  test(`secondary SSE sorted insertion preserves phone column ${selected} when ${arriving} arrives`, () => {
    const streams = [];
    const { state } = loadKontoraContext({ EventSource: makeEventSourceFake(streams) });
    state.isMobile = true;
    state._boardRaf = 1;
    state.updateFavicon = () => {};
    state.secondaryTickets = [{ id: 'existing', source: 'team', title: 'Existing', status: selected }];
    state.recomputeBoard();
    state.setActiveColumn(state.columns.findIndex(c => c.key === selected));
    const before = state.activeColumn;
    state.connectSSE();
    streams[0].emit('secondary_ticket_updated', { id: 'new', source: 'team', title: 'New', status: arriving });
    state.flushTicketUpdates();
    assert.equal(state.mobileColumn().key, selected);
    assert.ok(state.activeColumn > before, 'the sorted schema inserted a preceding column');
    assert.equal(state.mobileTickets()[0].id, 'existing');
    streams[0].emit('secondary_ticket_deleted', { id: 'new', source: 'team' });
    state.flushTicketUpdates();
    assert.equal(state.mobileColumn().key, selected);
    streams[0].emit('secondary_ticket_deleted', { id: 'existing', source: 'team' });
    state.flushTicketUpdates();
    assert.ok(state.activeColumn >= 0 && state.activeColumn < state.columns.length, 'removed selection has a valid fallback');
  });
}
