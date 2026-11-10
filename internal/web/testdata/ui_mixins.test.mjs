import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const dirname = path.dirname(fileURLToPath(import.meta.url));
const uiDir = path.join(dirname, "../ui");
const entry = path.join(uiDir, "index.js");

// The factories read a few browser globals while building their initial state.
// They only have to run, so the stubs answer with nothing rather than model a
// document; index_html.test.mjs is where behaviour is driven.
globalThis.window = { innerWidth: 1280, matchMedia: () => ({ matches: false, addEventListener() {} }) };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
Object.defineProperty(globalThis, "navigator", { value: { platform: "" }, configurable: true });
globalThis.getStoredTheme = () => "dark";

// Every kontora* factory the tree exports, found by importing each module
// rather than by reading index.js. A module that index.js forgot to merge is
// then still checked, and reformatting the import list changes nothing here.
// The walk is recursive because the bundler resolves ./sub/mod.js too.
async function mixins() {
  const files = fs
    .readdirSync(uiDir, { recursive: true })
    .filter((f) => f.endsWith(".js") && f !== "index.js");
  const out = [];
  for (const file of files) {
    const mod = await import(path.join(uiDir, file));
    for (const [name, value] of Object.entries(mod)) {
      if (name.startsWith("kontora") && typeof value === "function") out.push([name, value]);
    }
  }
  return out.sort();
}

// One flat component across a dozen modules: a key defined twice means one
// mixin's state or method disappears. merge() throws on it at runtime; this
// names both owners, which the exception cannot.
test("no two mixins define the same key", async () => {
  const factories = await mixins();
  assert.ok(factories.length > 10, `only ${factories.length} mixin modules found`);

  const owner = new Map();
  const clashes = [];
  for (const [name, factory] of factories) {
    for (const key of Object.keys(factory())) {
      if (owner.has(key)) clashes.push(`${key}: ${owner.get(key)} and ${name}`);
      owner.set(key, name);
    }
  }
  assert.deepEqual(clashes, []);
});

test("automation mixin owns prefixed state and the full route/action contract", async () => {
  const { kontoraAutomations } = await import(path.join(uiDir, "automations.js"));
  const state = kontoraAutomations();
  assert.deepEqual(Object.keys(state).filter(key => !key.startsWith("automations")), []);
  for (const name of ["Open", "Refresh", "Select", "New", "Edit", "Back", "ApplyRoute", "Dirty", "ConfirmReplace", "Reload",
    "SaveDraft", "Run", "Stop", "Toggle", "OpenDelete", "CloseDelete", "Delete", "SchedulePreview", "LoadHistory", "OpenRun", "StepRun", "RawURL", "OutputSubject", "Dispose"]) {
    assert.equal(typeof state["automations" + name], "function", name);
  }
  assert.deepEqual(Object.keys(state.automationsDraft).sort(), ["agent", "enabled", "model", "name", "overlap", "path", "prompt", "schedule", "timeout", "timezone", "workspace"]);
  assert.equal(state.automationsDraft.schedule, "0 9 * * 1-5");
  assert.equal(state.automationsDraft.timezone, "UTC");
  assert.equal(state.automationsDraft.overlap, "skip");
  assert.equal(state.automationsDraft.enabled, true);
  const other = kontoraAutomations();
  state.automationsDraft.name = "Unsaved"; state.automationsRunReceipts.example = "request-1";
  assert.equal(other.automationsDraft.name, ""); assert.deepEqual(other.automationsRunReceipts, {});
});

test("automation markup bindings resolve against the merged component", async () => {
  await import(entry);
  const merged = globalThis.kontora();
  const html = fs.readFileSync(path.join(dirname, "../static/index.html"), "utf8");
  const names = new Set();
  for (const [, attribute, expression] of html.matchAll(/(x-[\w.:-]+|@[\w.:-]+|:[\w.:-]+)="([^"]*)"/g)) {
    if (attribute === "x-ref") continue;
    for (const [name] of expression.matchAll(/\bautomations[A-Z]\w*/g)) names.add(name);
  }
  assert.ok(names.size > 60, `only ${names.size} automation bindings found`);
  assert.deepEqual([...names].filter(name => !Object.hasOwn(merged, name)), []);
});

test("companion mixin owns only uniquely prefixed companion keys", async () => {
  const { kontoraCompanion } = await import(path.join(uiDir, "companion.js"));
  const keys = Object.keys(kontoraCompanion());
  assert.ok(keys.length > 20);
  assert.deepEqual(keys.filter(key => !/^_?companion/.test(key)), []);
});

// The pane collects page context by name, so a renamed or deleted hook goes
// quiet rather than failing: the assistant just stops being told about that
// view. Named here so the build catches it instead.
test("every page-context hook the assistant names exists", async () => {
  await import(entry);
  const merged = globalThis.kontora();

  assert.ok(Array.isArray(merged._assistantContextHooks));
  assert.ok(merged._assistantContextHooks.length > 0);
  const missing = merged._assistantContextHooks.filter((name) => typeof merged[name] !== "function");
  assert.deepEqual(missing, []);
});

// A mixin left out of the merge is invisible: its half of the component simply
// never exists, and the template silently renders nothing where it was used.
// Checked against the component index.js actually builds, so a factory named
// only in a comment does not pass for a merged one.
test("index.js merges every mixin the tree exports", async () => {
  await import(entry);
  const merged = globalThis.kontora();

  const missing = [];
  for (const [name, factory] of await mixins()) {
    const absent = Object.keys(factory()).filter((key) => !Object.hasOwn(merged, key));
    if (absent.length) missing.push(`${name}: ${absent.join(", ")}`);
  }
  assert.deepEqual(missing, []);
});
