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
  const source = fs.readFileSync(appPath, "utf8");
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
    getStoredTheme() {
      return null;
    },
    setStoredTheme() {},
    applyTheme() {},
    window: {
      innerWidth: 1200,
      addEventListener() {},
    },
    document: {
      getElementById() {
        return null;
      },
      querySelector() {
        return null;
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
  vm.createContext(context);
  vm.runInContext(`${source}\nthis.kontora = kontora;`, context);
  return context.kontora();
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
  const state = loadKontoraState();
  const nextTick = deferred();
  const connectCalls = [];

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.$nextTick = () => nextTick.promise;
  state._TerminalClass = class {};
  state._FitAddonClass = class {};
  state._connectTerminal = (seq) => {
    connectCalls.push(seq);
  };

  const openPromise = state.openTerminal();
  assert.equal(state.terminalOpen, true);
  assert.equal(state._terminalOpening, true);

  state._teardownTransport();
  state.terminalOpen = false;

  nextTick.resolve();
  await openPromise;

  assert.deepEqual(connectCalls, []);
});

test("openTerminal clears terminalOpen if the tab changes before startup completes", async () => {
  const state = loadKontoraState();
  const nextTick = deferred();
  const connectCalls = [];
  let teardownCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.$nextTick = () => nextTick.promise;
  state._TerminalClass = class {};
  state._FitAddonClass = class {};
  state._connectTerminal = (seq) => {
    connectCalls.push(seq);
  };
  state._teardownTransport = () => {
    teardownCalls += 1;
  };

  const openPromise = state.openTerminal();
  state.activeTab = "ticket";

  nextTick.resolve();
  await openPromise;

  assert.equal(state.terminalOpen, false);
  assert.equal(state._terminalOpening, false);
  assert.equal(teardownCalls, 1);
  assert.deepEqual(connectCalls, []);
});

test("reconnectTerminal does nothing while the terminal is already opening", () => {
  const state = loadKontoraState();
  let teardownCalls = 0;
  let openCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.terminalOpen = true;
  state._terminalOpening = true;
  state._teardownTransport = () => {
    teardownCalls += 1;
  };
  state.openTerminal = () => {
    openCalls += 1;
  };

  state.reconnectTerminal();

  assert.equal(teardownCalls, 0);
  assert.equal(openCalls, 0);
});

test("reconnectTerminal tears down and reopens once when transport is ready", () => {
  const state = loadKontoraState();
  let teardownCalls = 0;
  let openCalls = 0;

  state.selectedTicket = { id: "tst-001" };
  state.activeTab = "terminal";
  state.terminalOpen = true;
  state._terminalOpening = false;
  state._teardownTransport = () => {
    teardownCalls += 1;
  };
  state.openTerminal = () => {
    openCalls += 1;
  };

  state.reconnectTerminal();

  assert.equal(teardownCalls, 1);
  assert.equal(openCalls, 1);
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
