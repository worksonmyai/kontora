import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const dirname = path.dirname(fileURLToPath(import.meta.url));
const htmlPath = path.join(dirname, "../static/index.html");
const appPath = path.join(dirname, "../static/app.js");

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function loadKontoraState() {
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

test("agent running count ignores non-Kontora in-progress tickets", () => {
  const state = loadKontoraState();
  state.tickets = [
    { id: "ext-001", agent: "claude", status: "in_progress", kontora: false },
    { id: "kon-001", agent: "claude", status: "in_progress", kontora: true },
  ];

  assert.equal(state.agentRunningCount("claude"), 1);
});
