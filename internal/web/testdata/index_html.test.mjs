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
  assert.equal(state.panelWidth, Math.floor(1200 * 0.66));
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

test("human_review column sorts by updated_at descending", () => {
  const state = loadKontoraState();
  state.tickets = [
    {
      id: "rev-old",
      title: "Old",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T09:00:00Z",
      updated_at: "2026-05-19T10:00:00Z",
    },
    {
      id: "rev-new",
      title: "New",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T08:00:00Z",
      updated_at: "2026-05-19T11:00:00Z",
    },
  ];

  const ids = state.ticketsByStatuses("human_review").map(t => t.id);

  assert.deepEqual(ids, ["rev-new", "rev-old"]);
});

test("human_review column falls back to created_at when updated_at is missing", () => {
  const state = loadKontoraState();
  state.tickets = [
    {
      id: "rev-no-update",
      title: "NoUpdate",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T12:00:00Z",
    },
    {
      id: "rev-updated",
      title: "Updated",
      status: "human_review",
      kontora: true,
      created_at: "2026-05-19T08:00:00Z",
      updated_at: "2026-05-19T11:00:00Z",
    },
  ];

  const ids = state.ticketsByStatuses("human_review").map(t => t.id);

  assert.deepEqual(ids, ["rev-no-update", "rev-updated"]);
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

test("agent running count ignores non-Kontora in-progress tickets", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "ext-001", agent: "claude", status: "in_progress", kontora: false },
    { id: "kon-001", agent: "claude", status: "in_progress", kontora: true },
  ];

  assert.equal(state.agentRunningCount("claude"), 1);
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
  assert.match(html, /class="pipe-tag shrink-0"/);
});
