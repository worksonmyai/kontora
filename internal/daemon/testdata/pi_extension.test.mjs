// State-machine tests for the Kontora pi extension.
//
// These evaluate the rendered JavaScript extension against a fake ExtensionAPI
// without starting pi or an LLM. The Go wrapper passes both rendered variants
// through environment variables. A direct Node run renders the same variants
// from ../pi_extension.js.
//
// Run with:  node --test internal/daemon/testdata/pi_extension.test.mjs

import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// ─── Fake ExtensionAPI ──────────────────────────────────────────────────────

function createFakeAPI() {
  const handlers = {};
  const tools = {};
  const entries = [];
  const messages = [];
  const execCalls = [];

  const api = {
    handlers,
    tools,
    entries,
    messages,
    execCalls,
    execResult: { code: 0, killed: false, stdout: "", stderr: "" },

    async exec(command, args, options) {
      execCalls.push({ command, args, options });
      if (api.execHandler) return api.execHandler(command, args, options);
      return api.execResult;
    },

    on(event, handler) {
      if (!handlers[event]) handlers[event] = [];
      handlers[event].push(handler);
    },

    registerTool(def) {
      tools[def.name] = def;
    },

    appendEntry(customType, data) {
      entries.push({ customType, data });
    },

    sendMessage(msg, opts) {
      messages.push({ msg, opts });
    },

    // Helpers for tests to fire events.
    async fire(event, eventObj, ctx) {
      const hs = handlers[event] || [];
      let result;
      for (const h of hs) {
        const next = await h(eventObj || {}, ctx);
        if (next !== undefined) result = next;
      }
      return result;
    },
  };

  return api;
}

function createFakeContext(overrides) {
  let shutdownCalled = 0;
  let abortCalled = 0;
  let compactCalls = [];

  const ctx = {
    cwd: "/scratch/worktree",
    mode: "tui",
    shutdownCalled: () => shutdownCalled,
    abortCalled: () => abortCalled,
    compactCalls: () => compactCalls,

    abort() {
      abortCalled++;
      return new Promise(() => {});
    },

    shutdown() {
      shutdownCalled++;
    },

    getContextUsage() {
      if (overrides && "contextUsage" in overrides) {
        return overrides.contextUsage;
      }
      return { tokens: 50000, contextWindow: 200000, percent: 25 };
    },

    compact(opts) {
      compactCalls.push(opts);
    },

    sessionManager: {
      getBranch() {
        if (overrides && overrides.branch) return overrides.branch;
        return [];
      },
    },

    isIdle() {
      return true;
    },
  };

  return ctx;
}

// Load the rendered extension source from the Go test wrapper.
const RESULT_ENV_KEYS = [
  "KONTORA_RESULT_VERSION", "KONTORA_INVOCATION_ID", "KONTORA_RESULT_TICKET",
  "KONTORA_RESULT_STAGE", "KONTORA_RESULT_FILE",
];

function loadExtension(source, ticketID, resultEnv = {}) {
  const api = createFakeAPI();

  // The extension uses `export default function (pi) { ... }`.
  // We need to strip the ESM export syntax and wrap it for eval.
  // Replace "export default function (pi)" with an assignment.
  const wrappedSource = source.replace(
    /export\s+default\s+function\s*\(/,
    "var __factory = function("
  );

  // Evaluate and extract the factory.
  const fullSource = wrappedSource + "\n__factory;";

  // Use Function constructor to avoid strict-mode issues with eval.
  const factory = new Function(fullSource + "\nreturn __factory;")();
  const env = { ...resultEnv, KONTORA_TICKET_ID: ticketID };
  const keys = [...RESULT_ENV_KEYS, "KONTORA_TICKET_ID"];
  const previous = Object.fromEntries(keys.map((key) => [key, process.env[key]]));
  try {
    for (const key of keys) {
      if (env[key] === undefined) delete process.env[key];
      else process.env[key] = env[key];
    }
    factory(api);
  } finally {
    for (const key of keys) {
      if (previous[key] === undefined) delete process.env[key];
      else process.env[key] = previous[key];
    }
  }
  return api;
}

const ENABLED_THRESHOLD = 150000;

function renderLocally(threshold, enabled, waitFile) {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const raw = readFileSync(path.join(here, "..", "pi_extension.js"), "utf8");
  return raw
    .replaceAll("__CHECKPOINT_THRESHOLD__", String(threshold))
    .replaceAll("__CHECKPOINT_ENABLED__", String(enabled))
    .replaceAll("__WAIT_MARKER_PATH__", JSON.stringify(waitFile || ""));
}

function checkRendered(name, source, threshold, enabled) {
  if (typeof source !== "string" || source.trim() === "") {
    throw new Error(`${name}: extension source is empty`);
  }
  if (source.includes("__CHECKPOINT_") || source.includes("__WAIT_MARKER_")) {
    throw new Error(`${name}: extension source still contains a placeholder`);
  }
  if (!source.includes(`const THRESHOLD = ${threshold};`)) {
    throw new Error(`${name}: expected threshold ${threshold}`);
  }
  if (!source.includes(`const ENABLED = ${enabled};`)) {
    throw new Error(`${name}: expected enabled=${enabled}`);
  }
  return source;
}

function loadSources() {
  const enabled = process.env.KONTORA_PI_EXT_ENABLED;
  const disabled = process.env.KONTORA_PI_EXT_DISABLED;
  if ((enabled && !disabled) || (!enabled && disabled)) {
    throw new Error(
      "partial environment wiring: set both KONTORA_PI_EXT_ENABLED and " +
        "KONTORA_PI_EXT_DISABLED, or set neither"
    );
  }

  const enabledSource = enabled || renderLocally(ENABLED_THRESHOLD, true);
  const disabledSource = disabled || renderLocally(0, false);
  return {
    enabled: checkRendered(
      "enabled extension",
      enabledSource,
      ENABLED_THRESHOLD,
      true
    ),
    disabled: checkRendered("disabled extension", disabledSource, 0, false),
  };
}

const { enabled: ENABLED_SOURCE, disabled: DISABLED_SOURCE } = loadSources();
if (!process.env.KONTORA_PI_EXT_ENABLED) {
  process.env.KONTORA_PI_EXT_ENABLED = ENABLED_SOURCE;
  process.env.KONTORA_PI_EXT_DISABLED = DISABLED_SOURCE;
}

const HANDOFF_TICKET = "test-abcd";
const HANDOFFS = [
  { verb: "note", acknowledgement: "note added" },
  { verb: "summary", acknowledgement: "summary set" },
];

for (const [checkpoint, source] of [["enabled", ENABLED_SOURCE], ["disabled", DISABLED_SOURCE]]) {
  for (const ticketID of [undefined, "", HANDOFF_TICKET]) {
    test(`handoff registration: checkpoint ${checkpoint}, ticket ${ticketID || "absent"}`, () => {
      const api = loadExtension(source, ticketID);
      for (const { verb } of HANDOFFS) {
        const tool = api.tools[`kontora_${verb}`];
        assert.equal(Boolean(tool), Boolean(ticketID));
        if (tool) {
          assert.equal(tool.executionMode, "sequential");
          assert.deepEqual(Object.keys(tool.parameters.properties), ["text"]);
          assert.deepEqual(tool.parameters.required, ["text"]);
          assert.equal(tool.parameters.properties.text.type, "string");
        }
      }
    });
  }
}

for (const { verb, acknowledgement } of HANDOFFS) {
  test(`${verb} sends literal text to the captured ticket`, async () => {
    const api = loadExtension(DISABLED_SOURCE, HANDOFF_TICKET);
    api.execResult.stdout = `${HANDOFF_TICKET} ${acknowledgement}\n`;
    const ctx = createFakeContext();
    const signal = new AbortController().signal;
    const text = "  `touch sentinel` $(touch sentinel) 'single' \"double\" \\ $HOME\nUnicode: 雪😀\n";
    const result = await api.tools[`kontora_${verb}`].execute(
      "call", { text, ticketID: "wrong-ticket" }, signal, undefined, ctx
    );
    assert.deepEqual(api.execCalls, [{
      command: "kontora",
      args: [verb, "--", HANDOFF_TICKET, text],
      options: { cwd: ctx.cwd, signal, timeout: 30_000 },
    }]);
    assert.equal(result.details, undefined);
    assert.equal(result.content[0].text, `${HANDOFF_TICKET} ${acknowledgement}.`);
  });

  for (const [name, text, valid] of [
    ["empty", "", false],
    ["whitespace", " \n\t\u2003", false],
    ["NUL", "text\0tail", false],
    ["non-string", 12, false],
    ["byte limit", "😀".repeat(8192), true],
    ["over byte limit", "😀".repeat(8192) + "a", false],
  ]) {
    test(`${verb} input: ${name}`, async () => {
      const api = loadExtension(ENABLED_SOURCE, HANDOFF_TICKET);
      api.execResult.stdout = `${HANDOFF_TICKET} ${acknowledgement}\n`;
      const call = () => api.tools[`kontora_${verb}`].execute(
        "call", { text }, undefined, undefined, createFakeContext()
      );
      if (valid) await call();
      else await assert.rejects(call, /text/i);
      assert.equal(api.execCalls.length, valid ? 1 : 0);
    });
  }

  for (const [name, result] of [
    ["nonzero", { code: 1, killed: false, stdout: `${HANDOFF_TICKET} ${acknowledgement}\n` }],
    ["killed", { code: 0, killed: true, stdout: `${HANDOFF_TICKET} ${acknowledgement}\n` }],
    ["no acknowledgement", { code: 0, killed: false, stdout: "" }],
    ["wrong ticket", { code: 0, killed: false, stdout: `test-other ${acknowledgement}\n` }],
    ["partial acknowledgement", { code: 0, killed: false, stdout: `prefix ${HANDOFF_TICKET} ${acknowledgement}\n` }],
    ["wrong verb", { code: 0, killed: false, stdout: `${HANDOFF_TICKET} other acknowledgement\n` }],
    ["rejected execution", null],
  ]) {
    test(`${verb} execution: ${name} never retries`, async () => {
      const api = loadExtension(DISABLED_SOURCE, HANDOFF_TICKET);
      api.execResult = result;
      if (result === null) api.execHandler = async () => { throw new Error("spawn failed"); };
      await assert.rejects(
        () => api.tools[`kontora_${verb}`].execute("call", { text: "payload" }, undefined, undefined, createFakeContext()),
        /write outcome may be unknown.*read the ticket before resubmitting/i
      );
      assert.equal(api.execCalls.length, 1);
    });
  }

  test(`${verb} accepts colored acknowledgement lines`, async () => {
    const api = loadExtension(DISABLED_SOURCE, HANDOFF_TICKET);
    api.execResult.stdout = `other output\n\x1b[36m${HANDOFF_TICKET}\x1b[0m \x1b[2m${acknowledgement}\x1b[0m\r\n`;
    await api.tools[`kontora_${verb}`].execute("call", { text: "payload" }, undefined, undefined, createFakeContext());
    assert.equal(api.execCalls.length, 1);
  });

  test(`${verb} rejects pre-execution cancellation`, async () => {
    const api = loadExtension(DISABLED_SOURCE, HANDOFF_TICKET);
    const controller = new AbortController();
    controller.abort();
    await assert.rejects(
      () => api.tools[`kontora_${verb}`].execute("call", { text: "payload" }, controller.signal, undefined, createFakeContext()),
      /cancelled.*not submitted/i
    );
    assert.equal(api.execCalls.length, 0);
  });

  test(`${verb} propagates in-flight cancellation without retry`, async () => {
    const api = loadExtension(DISABLED_SOURCE, HANDOFF_TICKET);
    const controller = new AbortController();
    api.execHandler = async (_command, _args, options) => {
      assert.equal(options.signal, controller.signal);
      return new Promise((resolve) => {
        options.signal.addEventListener("abort", () => resolve({ code: 0, killed: true, stdout: "" }), { once: true });
        controller.abort();
      });
    };
    await assert.rejects(
      () => api.tools[`kontora_${verb}`].execute("call", { text: "payload" }, controller.signal, undefined, createFakeContext()),
      /write outcome may be unknown.*read the ticket before resubmitting/i
    );
    assert.equal(api.execCalls.length, 1);
  });
}

// ─── Tests: invocation-bound terminal results ───────────────────────────────

function resultFixture(t, source = ENABLED_SOURCE) {
  const dir = mkdtempSync(path.join(tmpdir(), "kontora-result-test-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = {
    KONTORA_RESULT_VERSION: "1",
    KONTORA_INVOCATION_ID: "invocation-123",
    KONTORA_RESULT_TICKET: "result-ticket",
    KONTORA_RESULT_STAGE: "implement",
    KONTORA_RESULT_FILE: path.join(dir, "result artifact 雪.json"),
  };
  const api = loadExtension(source, "different-note-ticket", env);
  api.execResult = {
    code: 0, killed: false,
    stdout: JSON.stringify({ accepted: true, invocation_id: env.KONTORA_INVOCATION_ID, outcome: "complete" }),
  };
  const ctx = createFakeContext({ contextUsage: { tokens: 200000 } });
  const submit = (params = { outcome: "complete", summary: "Done." }, signal) =>
    api.tools.kontora_result.execute("result-call", params, signal, undefined, ctx);
  const checkpoint = () => api.tools.kontora_phase_complete.execute(
    "checkpoint-call", { completed_phase: "Phase 1", next_phase: "Phase 2" }, undefined, undefined, ctx
  );
  return { api, ctx, env, submit, checkpoint };
}

for (const [name, source] of [["enabled", ENABLED_SOURCE], ["disabled", DISABLED_SOURCE], ["zero threshold", renderLocally(0, true)]]) {
  test(`result registration is independent of checkpoints: ${name}`, (t) => {
    assert.equal(loadExtension(source, HANDOFF_TICKET).tools.kontora_result, undefined);
    const { api } = resultFixture(t, source);
    const tool = api.tools.kontora_result;
    assert.equal(tool.executionMode, "sequential");
    assert.deepEqual(tool.parameters.required, ["outcome", "summary"]);
    assert.deepEqual(tool.parameters.properties.outcome.enum, ["complete", "blocked"]);
    assert.deepEqual(Object.keys(tool.parameters.properties), ["outcome", "summary", "next_step"]);
  });
}

for (const key of RESULT_ENV_KEYS) {
  for (const value of [undefined, "", " \t"]) {
    test(`partial result context errors: ${key}=${JSON.stringify(value)}`, (t) => {
      const { env } = resultFixture(t);
      assert.throws(() => loadExtension(DISABLED_SOURCE, undefined, { ...env, [key]: value }), new RegExp(key));
    });
  }
}

test("unsupported result context version errors", (t) => {
  const { env } = resultFixture(t);
  assert.throws(() => loadExtension(DISABLED_SOURCE, undefined, { ...env, KONTORA_RESULT_VERSION: "2" }), /version/i);
});

for (const outcome of ["complete", "blocked"]) {
  test(`result ${outcome} uses a private temporary literal payload and JSON acknowledgement`, async (t) => {
    const { api, ctx, env, submit } = resultFixture(t, DISABLED_SOURCE);
    const signal = new AbortController().signal;
    const payload = {
      outcome,
      summary: "  `touch sentinel` $(touch sentinel) 'single' \"double\" \\ $HOME\nUnicode: 雪😀\n",
      next_step: "Provide the missing resource.\n",
    };
    let inputFile;
    api.execHandler = async (command, args, options) => {
      assert.equal(command, "kontora");
      assert.equal(args[0], "result");
      assert.equal(args[1], "--input-file");
      assert.equal(args[3], env.KONTORA_RESULT_TICKET);
      assert.equal(args.length, 4);
      inputFile = args[2];
      assert.ok(path.isAbsolute(inputFile));
      assert.notEqual(inputFile, env.KONTORA_RESULT_FILE);
      assert.equal(statSync(inputFile).mode & 0o777, 0o600);
      assert.equal(statSync(path.dirname(inputFile)).mode & 0o777, 0o700);
      assert.deepEqual(JSON.parse(readFileSync(inputFile, "utf8")), payload);
      assert.deepEqual(options, { cwd: ctx.cwd, signal, timeout: 30_000, maxBuffer: 32768 });
      return { code: 0, killed: false, stdout: JSON.stringify({ accepted: true, invocation_id: env.KONTORA_INVOCATION_ID, outcome }) + "\n" };
    };
    const result = await submit({ ...payload, invocation_id: "wrong", ticket: "wrong", stage: "wrong", version: 99 }, signal);
    assert.equal(result.isError, undefined);
    assert.equal(result.terminate, undefined);
    assert.match(result.content[0].text, /accepted/);
    assert.equal(existsSync(path.dirname(inputFile)), false);
    assert.equal(ctx.shutdownCalled(), 0, "report must not shut down the agent");
    await api.fire("agent_settled", {}, ctx);
    assert.equal(ctx.shutdownCalled(), 1, "normal settlement still shuts down");
  });
}

for (const [name, params] of [
  ["missing params", undefined],
  ["invalid outcome", { outcome: "done", summary: "Done" }],
  ["empty summary", { outcome: "complete", summary: "" }],
  ["blank summary", { outcome: "complete", summary: " \n\t" }],
  ["non-string summary", { outcome: "complete", summary: 42 }],
  ["missing next step", { outcome: "blocked", summary: "Blocked" }],
  ["blank next step", { outcome: "blocked", summary: "Blocked", next_step: " \n" }],
  ["non-string next step", { outcome: "complete", summary: "Done", next_step: 42 }],
  ["NUL summary", { outcome: "complete", summary: "a\0b" }],
  ["NUL next step", { outcome: "blocked", summary: "Blocked", next_step: "a\0b" }],
  ["oversized UTF-8", { outcome: "complete", summary: "😀".repeat(8192) }],
  ["oversized escaped JSON", { outcome: "complete", summary: "\n".repeat(16384) }],
]) {
  test(`result rejects ${name} without execution`, async (t) => {
    const { api, ctx } = resultFixture(t);
    await assert.rejects(() => api.tools.kontora_result.execute("call", params, undefined, undefined, ctx));
    assert.equal(api.execCalls.length, 0);
  });
}

test("result byte limit includes the JSON envelope", async (t) => {
  const { api, submit } = resultFixture(t);
  const overhead = Buffer.byteLength(JSON.stringify({ outcome: "complete", summary: "" }));
  await submit({ outcome: "complete", summary: "a".repeat(32768 - overhead) });
  await assert.rejects(() => submit({ outcome: "complete", summary: "a".repeat(32769 - overhead) }), /32,768/);
  assert.equal(api.execCalls.length, 1);
});

for (const [name, override] of [
  ["nonzero", { code: 7 }],
  ["killed", { killed: true }],
  ["truncated", { truncated: true }],
  ["missing acknowledgement", { stdout: "" }],
  ["malformed JSON", { stdout: "not JSON" }],
  ["non-JSON preamble", { stdout: 'accepted\n{"accepted":true}' }],
  ["null acknowledgement", { stdout: "null" }],
  ["not accepted", { stdout: JSON.stringify({ accepted: false, invocation_id: "invocation-123", outcome: "complete" }) }],
  ["string accepted", { stdout: JSON.stringify({ accepted: "true", invocation_id: "invocation-123", outcome: "complete" }) }],
  ["wrong invocation", { stdout: JSON.stringify({ accepted: true, invocation_id: "old", outcome: "complete" }) }],
  ["wrong outcome", { stdout: JSON.stringify({ accepted: true, invocation_id: "invocation-123", outcome: "blocked" }) }],
  ["execution throws", null],
]) {
  test(`result ${name}: error, no retry, temp cleanup, checkpoints remain enabled`, async (t) => {
    const { api, ctx, env, submit, checkpoint } = resultFixture(t);
    let inputFile;
    api.execHandler = async (_command, args) => {
      inputFile = args[2];
      assert.equal(existsSync(inputFile), true);
      if (override === null) throw new Error("spawn kontora ENOENT");
      return { ...api.execResult, stderr: "result rejected: invalid invocation", ...override };
    };
    await assert.rejects(submit, (error) => {
      assert.match(error.message, /outcome may be unknown.*do not retry automatically/i);
      assert.ok(error.message.includes(JSON.stringify(env.KONTORA_RESULT_FILE)));
      if (override === null) assert.match(error.message, /spawn kontora ENOENT/);
      else {
        const result = { ...api.execResult, ...override };
        assert.ok(error.message.includes(`Exit status=${result.code}, killed=${!!result.killed}, truncated=${!!result.truncated}`));
        assert.match(error.message, /result rejected: invalid invocation/);
      }
      return true;
    });
    assert.equal(api.execCalls.length, 1);
    assert.equal(existsSync(path.dirname(inputFile)), false);
    assert.equal(ctx.shutdownCalled(), 0);
    await checkpoint();
    await api.fire("turn_end", {}, ctx);
    ctx.compactCalls()[0].onComplete({});
    assert.equal(api.messages.length, 1);
  });
}

test("result rejects pre-execution cancellation", async (t) => {
  const { api, submit } = resultFixture(t);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(() => submit(undefined, controller.signal), /cancelled.*not submitted/i);
  assert.equal(api.execCalls.length, 0);
});

test("result in-flight cancellation cleans temp and never retries", async (t) => {
  const { api, submit } = resultFixture(t);
  const controller = new AbortController();
  let inputFile;
  api.execHandler = async (_command, args, options) => {
    inputFile = args[2];
    assert.equal(options.signal, controller.signal);
    controller.abort();
    return { code: 0, killed: true, stdout: api.execResult.stdout };
  };
  await assert.rejects(() => submit(undefined, controller.signal), /outcome may be unknown/i);
  assert.equal(api.execCalls.length, 1);
  assert.equal(existsSync(path.dirname(inputFile)), false);
});

for (const transport of ["tool complete", "tool blocked", "result file"]) {
  for (const timing of ["before checkpoint", "pending checkpoint", "compaction complete", "compaction error", "queued followup"]) {
    test(`terminal ${transport} clears checkpoint state and stale context: ${timing}`, async (t) => {
      const { api, ctx, env, submit, checkpoint } = resultFixture(t);
      if (timing !== "before checkpoint") await checkpoint();
      if (["compaction complete", "compaction error", "queued followup"].includes(timing)) {
        await api.fire("turn_end", {}, ctx);
      }
      if (timing === "queued followup") ctx.compactCalls()[0].onComplete({});
      if (transport.startsWith("tool")) {
        const outcome = transport.endsWith("blocked") ? "blocked" : "complete";
        api.execResult.stdout = JSON.stringify({ accepted: true, invocation_id: env.KONTORA_INVOCATION_ID, outcome });
        await submit({ outcome, summary: "Final stage result", ...(outcome === "blocked" ? { next_step: "Provide access" } : {}) });
      } else writeFileSync(env.KONTORA_RESULT_FILE, "{}");
      if (timing === "before checkpoint") {
        assert.match((await checkpoint()).content[0].text, /suppressed/);
        assert.equal(api.entries.length, 0);
      }
      await api.fire("turn_end", {}, ctx);
      if (timing === "pending checkpoint" || timing === "before checkpoint") {
        assert.equal(ctx.compactCalls().length, 0);
      }
      if (timing === "compaction complete") ctx.compactCalls()[0].onComplete({});
      if (timing === "compaction error") ctx.compactCalls()[0].onError(new Error("provider timeout"));
      if (timing !== "queued followup") assert.equal(api.messages.length, 0);
      const ordinary = { role: "user", content: "original request" };
      const terminal = { role: "toolResult", toolName: "kontora_result", content: "accepted" };
      const messages = [ordinary, ...api.messages.map(({ msg }) => ({ role: "custom", ...msg })), terminal];
      const filtered = await api.fire("context", { messages }, ctx);
      assert.deepEqual(filtered.messages, [ordinary, terminal], "filtering removes stale instructions, not the queued SDK request");
      assert.equal(messages.length, timing === "queued followup" ? 3 : 2, "do not mutate persisted context");
      assert.equal(ctx.shutdownCalled(), 0);
      await api.fire("agent_settled", {}, ctx);
      assert.equal(ctx.shutdownCalled(), 1, "terminal checkpoint state must not hold shutdown open");
      await api.fire("agent_start", {}, ctx);
      assert.match((await checkpoint()).content[0].text, /suppressed/);
    });
  }
}

for (const callback of ["onComplete", "onError"]) {
  for (const transport of ["tool", "file"]) {
    test(`terminal ${transport} after settlement finishes deferred shutdown: ${callback}`, async (t) => {
      const { api, ctx, env, submit, checkpoint } = resultFixture(t);
      await checkpoint();
      await api.fire("turn_end", {}, ctx);
      await api.fire("agent_settled", {}, ctx);
      assert.equal(ctx.shutdownCalled(), 0);
      if (transport === "tool") await submit();
      else writeFileSync(env.KONTORA_RESULT_FILE, "{}");
      const options = ctx.compactCalls()[0];
      options[callback](new Error("compaction failed"));
      options.onComplete({});
      options.onError(new Error("duplicate callback"));
      assert.equal(ctx.shutdownCalled(), 1);
      assert.equal(api.messages.length, 0);
    });
  }

  for (const terminal of [false, true]) {
    test(`busy ${callback} holds continuation outside Pi until settlement: terminal=${terminal}`, async (t) => {
      const { api, ctx, env, checkpoint } = resultFixture(t);
      let idle = false;
      ctx.isIdle = () => idle;
      await checkpoint();
      await api.fire("turn_end", {}, ctx);
      ctx.compactCalls()[0][callback](new Error("compaction failed"));
      assert.equal(api.messages.length, 0, "do not queue a model turn while busy");
      if (terminal) writeFileSync(env.KONTORA_RESULT_FILE, "{}");
      idle = true;
      await api.fire("agent_settled", {}, ctx);
      assert.equal(api.messages.length, terminal ? 0 : 1);
      assert.equal(ctx.shutdownCalled(), terminal ? 1 : 0);
      if (!terminal) {
        await api.fire("agent_start", {}, ctx);
        await api.fire("agent_settled", {}, ctx);
        assert.equal(ctx.shutdownCalled(), 1);
      }
    });
  }
}

for (const [checkpointMode, source] of [["enabled", ENABLED_SOURCE], ["disabled", DISABLED_SOURCE]]) {
  for (const terminal of ["none", "file", "accepted tool"]) {
    test(`final-turn abort: checkpoints ${checkpointMode}, terminal=${terminal}`, { timeout: 1000 }, async (t) => {
      const { api, ctx, env, submit } = resultFixture(t, source);
      await api.fire("turn_start", {}, ctx);
      if (terminal === "file") writeFileSync(env.KONTORA_RESULT_FILE, "{}");
      if (terminal === "accepted tool") await submit();
      const publishing = { role: "assistant", stopReason: "toolUse", content: [{ type: "toolCall", name: "kontora_result" }] };
      await api.fire("turn_end", { message: publishing }, ctx);
      assert.equal(ctx.abortCalled(), 0, "the publishing tool must still receive its final response");
      let expectedAborts = 0;
      for (const message of [
        { role: "user", content: "Request" },
        { role: "toolResult", content: [] },
        { role: "assistant", stopReason: "toolUse", content: [{ type: "toolCall", name: "probe_tool" }] },
        { role: "assistant", stopReason: "stop", content: [{ type: "toolCall", name: "probe_tool" }] },
        { role: "assistant", stopReason: "error", content: [] },
        { role: "assistant", stopReason: "length", content: [] },
        { role: "assistant", stopReason: "aborted", content: [] },
        { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "Done." }] },
      ]) {
        await api.fire("turn_start", {}, ctx);
        await api.fire("turn_end", { message }, ctx);
        if (terminal !== "none" && message.role === "assistant") expectedAborts++;
        assert.equal(ctx.abortCalled(), expectedAborts, "every post-publication response must cancel queues");
      }
      assert.equal(publishing.stopReason, "toolUse", "do not rewrite the publishing turn");
      assert.equal(ctx.shutdownCalled(), 0, "shutdown still waits for settlement");
      await api.fire("agent_settled", {}, ctx);
      assert.equal(ctx.shutdownCalled(), 1);
    });
  }
}

for (const toolName of ["bash", "kontora_result", "kontora_summary", "probe_tool"]) {
  test(`terminal publication blocks further ${toolName} execution`, async (t) => {
    const { api, ctx, env } = resultFixture(t);
    assert.equal(await api.fire("tool_call", { toolName }, ctx), undefined);
    writeFileSync(env.KONTORA_RESULT_FILE, "{}");
    const decision = await api.fire("tool_call", { toolName }, ctx);
    assert.equal(decision.block, true);
    assert.equal(decision.terminate, true);
    assert.match(decision.reason, /terminal Kontora result/);
  });
}

test("accepted result without an artifact still invalidates both late compaction callbacks", async (t) => {
  const { api, ctx, submit, checkpoint } = resultFixture(t);
  await checkpoint();
  await api.fire("turn_end", {}, ctx);
  await submit();
  const entryCount = api.entries.length;
  ctx.compactCalls()[0].onComplete({});
  ctx.compactCalls()[0].onError(new Error("Already compacted"));
  assert.equal(api.entries.length, entryCount);
  assert.equal(api.messages.length, 0);
});

test("publication suppresses callbacks while result execution is still awaiting acknowledgement", async (t) => {
  const { api, ctx, env, submit, checkpoint } = resultFixture(t);
  await checkpoint();
  await api.fire("turn_end", {}, ctx);
  api.execHandler = async () => {
    writeFileSync(env.KONTORA_RESULT_FILE, "{}");
    ctx.compactCalls()[0].onError(new Error("provider timeout"));
    assert.equal(api.messages.length, 0);
    assert.equal(ctx.shutdownCalled(), 0);
    // Publication succeeded but the acknowledgement was lost. Presence, not
    // just a successful tool return, must stop continuation.
    return { code: 1, killed: false, stdout: "" };
  };
  await assert.rejects(submit, /outcome may be unknown/i);
  assert.match((await checkpoint()).content[0].text, /suppressed/);
  assert.equal(api.messages.length, 0);
});

test("terminal result presence is latched even if the artifact is later retired", async (t) => {
  const { api, ctx, env, checkpoint } = resultFixture(t);
  writeFileSync(env.KONTORA_RESULT_FILE, "{}");
  assert.match((await checkpoint()).content[0].text, /suppressed/);
  rmSync(env.KONTORA_RESULT_FILE);
  await api.fire("agent_start", {}, ctx);
  assert.match((await checkpoint()).content[0].text, /suppressed/);
  assert.equal(api.entries.length, 0);
});

test("unreported invocations preserve checkpoint messages in context", async (t) => {
  const { api, ctx, checkpoint } = resultFixture(t);
  await checkpoint();
  await api.fire("turn_end", {}, ctx);
  ctx.compactCalls()[0].onComplete({});
  const messages = api.messages.map(({ msg }) => msg);
  assert.equal(await api.fire("context", { messages }, ctx), undefined);
});

// ─── Tests: disabled extension ──────────────────────────────────────────────

test("disabled extension registers no checkpoint tool", () => {
  const api = loadExtension(DISABLED_SOURCE);
  assert.equal(api.tools["kontora_phase_complete"], undefined);
  assert.ok(api.handlers["agent_settled"], "should register agent_settled");
});

// ─── Tests: enabled extension ───────────────────────────────────────────────

test("enabled extension registers checkpoint tool", () => {
  const api = loadExtension(ENABLED_SOURCE);
  assert.ok(api.tools["kontora_phase_complete"], "should register tool");
  assert.ok(api.handlers["agent_settled"], "should register agent_settled");
  assert.ok(api.handlers["turn_end"], "should register turn_end");
  assert.ok(api.handlers["session_start"], "should register session_start");
  assert.ok(
    api.handlers["session_shutdown"],
    "should register session_shutdown"
  );
});

test("invalid checkpoint call is rejected", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({ contextUsage: { tokens: 200000 } });
  const tool = api.tools["kontora_phase_complete"];
  const result = await tool.execute(
    "tc1",
    { completed_phase: "", next_phase: "Phase 2" },
    undefined,
    undefined,
    ctx
  );

  assert.equal(result.isError, true);
  assert.equal(api.entries.length, 0);
  assert.equal(ctx.compactCalls().length, 0);
});

test("duplicate checkpoint call is rejected", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({ contextUsage: { tokens: 200000 } });

  // Fire session_start to init state
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  const result1 = await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result1.content[0].text.includes("Compaction will start"),
    "first call should request compaction"
  );

  const result2 = await tool.execute("tc2", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result2.content[0].text.includes("already checkpointed"),
    "duplicate should be rejected"
  );
});

test("pending checkpoint blocks second tool call", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({ contextUsage: { tokens: 200000 } });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  const result = await tool.execute("tc2", {
    completed_phase: "Phase 2",
    next_phase: "Phase 3",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("already pending"),
    "should reject while pending"
  );
});

test("unknown context usage skips compaction", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({ contextUsage: undefined });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  const result = await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("unknown"),
    "should mention unknown context"
  );
  assert.equal(api.entries.length, 1);
  assert.equal(api.entries[0].data.outcome, "skipped");
});

test("below-threshold usage skips compaction", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  // Threshold in the enabled source is 150000
  const ctx = createFakeContext({
    contextUsage: { tokens: 100000, contextWindow: 200000, percent: 50 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  const result = await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("within threshold"),
    "should indicate within threshold"
  );
  assert.equal(api.entries[0].data.outcome, "skipped");
});

test("above-threshold triggers single-flight compaction at turn_end", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.equal(api.entries[0].data.outcome, "requested");
  assert.equal(api.entries[0].data.context_tokens, 200000);

  // Fire turn_end to consume the pending checkpoint
  await api.fire("turn_end", {}, ctx);

  assert.equal(ctx.compactCalls().length, 1, "should call compact once");

  // Fire turn_end again — should NOT start another compaction
  await api.fire("turn_end", {}, ctx);
  assert.equal(
    ctx.compactCalls().length,
    1,
    "second turn_end should not start another compaction"
  );
});

test("completion callback enqueues one continuation", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  const opts = ctx.compactCalls()[0];
  assert.ok(opts.onComplete, "should have onComplete callback");

  opts.onComplete({
    usage: { input: 30000, output: 900, cacheRead: 0, cacheWrite: 0 },
    estimatedTokensAfter: 38000,
  });

  assert.equal(api.messages.length, 1, "should send one continuation");
  assert.ok(
    api.messages[0].msg.content.includes("Continue with Phase 2"),
    "continuation should reference next phase"
  );
  assert.equal(
    api.messages[0].msg.customType,
    "kontora-continuation"
  );
  assert.equal(api.messages[0].opts.triggerTurn, true);
  assert.equal(api.messages[0].opts.deliverAs, "followUp");
  const compacted = api.entries.find(
    (entry) => entry.data && entry.data.outcome === "compacted"
  );
  assert.deepEqual(compacted.data.compaction_usage, {
    input: 30000,
    output: 900,
    cacheRead: 0,
    cacheWrite: 0,
  });
  assert.equal(compacted.data.estimated_post_compaction_tokens, 38000);
  assert.equal(compacted.data.context_tokens, 200000);

  // Second callback should not enqueue another continuation
  opts.onComplete({});
  assert.equal(api.messages.length, 1, "should not send duplicate continuation");
});

test("failure callback enqueues one continuation", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  const opts = ctx.compactCalls()[0];
  opts.onError(new Error("provider timeout"));

  assert.equal(api.messages.length, 1, "should send one continuation on error");
  // Should record a "failed" entry
  const failedEntries = api.entries.filter(
    (e) => e.data && e.data.outcome === "failed"
  );
  assert.equal(failedEntries.length, 1, "should record failed outcome");
});

test("agent_settled does not shut down during compaction", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  // Compaction is in flight — agent_settled should NOT shut down
  await api.fire("agent_settled", {}, ctx);
  assert.equal(ctx.shutdownCalled(), 0, "should not shut down during compaction");
});

test("final settlement shuts down", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext();
  await api.fire("session_start", { reason: "startup" }, ctx);

  // No checkpoint, no compaction — settlement should trigger shutdown.
  await api.fire("agent_settled", {}, ctx);
  assert.equal(ctx.shutdownCalled(), 1, "should shut down on final settlement");
});

test("branch reconstruction rejects consumed checkpoint", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    branch: [
      {
        type: "custom",
        customType: "kontora-phase-checkpoint",
        data: { completed_phase: "Phase 1", outcome: "compacted" },
      },
    ],
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });

  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  const result = await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("already checkpointed"),
    "should reject consumed checkpoint from branch"
  );
});

test("shutdown invalidates late callbacks", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  const opts = ctx.compactCalls()[0];

  // Simulate session shutdown before the callback fires
  await api.fire("session_shutdown", {}, ctx);

  // Now fire the callback — it should be discarded
  opts.onComplete({});
  assert.equal(
    api.messages.length,
    0,
    "late callback should not enqueue continuation after shutdown"
  );
});

test("already-compacted error continues once", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  const opts = ctx.compactCalls()[0];
  opts.onError(new Error("Already compacted"));

  assert.equal(api.messages.length, 1, "should enqueue continuation");
  const compactedEntries = api.entries.filter(
    (e) => e.data && e.data.outcome === "compacted"
  );
  assert.equal(
    compactedEntries.length,
    1,
    "already-compacted should record compacted, not failed"
  );
});

test("continuation-pending state remains non-terminal until agent_start", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  const opts = ctx.compactCalls()[0];
  opts.onComplete({});

  // Continuation has been queued but agent_start hasn't fired yet
  assert.equal(api.messages.length, 1);

  // agent_settled fires before agent_start — should NOT shut down
  await api.fire("agent_settled", {}, ctx);
  assert.equal(
    ctx.shutdownCalled(),
    0,
    "should not shut down while continuation is pending"
  );

  // agent_start fires — clears continuation pending
  await api.fire("agent_start", {}, ctx);

  // Now agent_settled should shut down
  await api.fire("agent_settled", {}, ctx);
  assert.equal(ctx.shutdownCalled(), 1, "should shut down after agent_start");
});

test("disabled extension still shuts down on agent_settled", async () => {
  const api = loadExtension(DISABLED_SOURCE);
  const ctx = createFakeContext();
  await api.fire("session_start", { reason: "startup" }, ctx);

  await api.fire("agent_settled", {}, ctx);
  assert.equal(ctx.shutdownCalled(), 1, "disabled ext should shut down");
});

test("compaction in flight blocks second checkpoint from starting", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: 200000, contextWindow: 200000, percent: 100 },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);
  await api.fire("turn_end", {}, ctx);

  // Compaction is in flight — try another checkpoint
  const result = await tool.execute("tc2", {
    completed_phase: "Phase 2",
    next_phase: "Phase 3",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("already in flight"),
    "should reject while compaction in flight"
  );
});

test("null tokens in context usage skips compaction", async () => {
  const api = loadExtension(ENABLED_SOURCE);
  const ctx = createFakeContext({
    contextUsage: { tokens: null, contextWindow: 200000, percent: null },
  });
  await api.fire("session_start", { reason: "startup" }, ctx);

  const tool = api.tools["kontora_phase_complete"];
  const result = await tool.execute("tc1", {
    completed_phase: "Phase 1",
    next_phase: "Phase 2",
  }, undefined, undefined, ctx);

  assert.ok(
    result.content[0].text.includes("unknown"),
    "null tokens should be treated as unknown"
  );
  assert.equal(api.entries[0].data.outcome, "skipped");
});

// ─── Tests: waiting marker ──────────────────────────────────────────────────
//
// These render their own source against a real temp path and assert on the file
// the extension writes, which is what the daemon's poller reads.

function withMarker(t, body) {
  const dir = mkdtempSync(path.join(tmpdir(), "kontora-wait-"));
  const marker = path.join(dir, "implement.waiting.json");
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const api = loadExtension(renderLocally(0, false, marker));
  return body(api, marker);
}

function readMarker(marker) {
  return JSON.parse(readFileSync(marker, "utf8"));
}

function start(toolName, toolCallId, args) {
  return { toolName, toolCallId, args };
}

function end(toolName, toolCallId) {
  return { toolName, toolCallId };
}

test("no marker path leaves the question handlers unregistered", () => {
  const api = loadExtension(DISABLED_SOURCE);
  assert.equal(api.handlers["tool_execution_start"], undefined);
  assert.equal(api.handlers["tool_execution_end"], undefined);
});

test("ask_user_question writes a marker describing the call", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", {
        questions: [{ question: "Which database?", header: "DB" }],
      })
    );

    const m = readMarker(marker);
    assert.equal(m.tool, "ask_user_question");
    assert.equal(m.tool_call_id, "call-1");
    assert.equal(m.question, "Which database?");
    assert.match(m.started_at, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
  });
});

test("the example question tool writes a marker too", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("question", "call-q", { question: "Ship it?", options: [] })
    );

    const m = readMarker(marker);
    assert.equal(m.tool, "question");
    assert.equal(m.tool_call_id, "call-q");
    assert.equal(m.question, "Ship it?");
  });
});

test("an unrelated tool writes no marker", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire("tool_execution_start", start("bash", "call-b", { cmd: "ls" }));
    assert.equal(existsSync(marker), false);
  });
});

test("an unrelated tool leaves an open question's marker alone", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    await api.fire("tool_execution_start", start("bash", "call-b", {}));
    await api.fire("tool_execution_end", end("bash", "call-b"));

    assert.equal(readMarker(marker).tool_call_id, "call-1");
  });
});

test("the marker is gone once the question returns", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    assert.equal(existsSync(marker), true);

    await api.fire("tool_execution_end", end("ask_user_question", "call-1"));
    assert.equal(existsSync(marker), false);
  });
});

test("overlapping questions: the marker follows the newest, then the one left open", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-2", { questions: [{ question: "Q2" }] })
    );
    assert.equal(readMarker(marker).tool_call_id, "call-2");
    assert.equal(readMarker(marker).question, "Q2");

    await api.fire("tool_execution_end", end("ask_user_question", "call-2"));
    assert.equal(existsSync(marker), true, "one call is still open");
    assert.equal(
      readMarker(marker).tool_call_id,
      "call-1",
      "the marker must describe the question still blocking, not the answered one"
    );
    assert.equal(readMarker(marker).question, "Q1");

    await api.fire("tool_execution_end", end("ask_user_question", "call-1"));
    assert.equal(existsSync(marker), false);
  });
});

test("answering the oldest question first leaves the newest on the marker", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-2", { questions: [{ question: "Q2" }] })
    );
    await api.fire("tool_execution_end", end("ask_user_question", "call-1"));

    assert.equal(readMarker(marker).tool_call_id, "call-2");
    assert.equal(readMarker(marker).question, "Q2");
  });
});

test("session_shutdown clears a marker left by an open question", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    await api.fire("session_shutdown", {});
    assert.equal(existsSync(marker), false);
  });
});

test("question text is collapsed to one line and cut to 200 bytes", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", {
        questions: [{ question: "  first line\n\n  second   line  " }],
      })
    );
    assert.equal(readMarker(marker).question, "first line second line");

    // A 4-byte character straddling the limit must not be split: 198 ASCII
    // bytes then an emoji leaves the cut mid-character.
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-2", {
        questions: [{ question: "a".repeat(198) + "\u{1F600}" + "tail" }],
      })
    );
    const q = readMarker(marker).question;
    assert.equal(Buffer.byteLength(q, "utf8"), 198);
    assert.equal(q, "a".repeat(198));
  });
});

test("an unreadable question shape falls back to the tool name alone", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { unexpected: true })
    );
    const m = readMarker(marker);
    assert.equal(m.tool, "ask_user_question");
    assert.equal(m.question, "");
  });
});

test("the rename leaves no temp file behind", async (t) => {
  await withMarker(t, async (api, marker) => {
    await api.fire(
      "tool_execution_start",
      start("ask_user_question", "call-1", { questions: [{ question: "Q1" }] })
    );
    assert.deepEqual(readdirSync(path.dirname(marker)), [path.basename(marker)]);
  });
});
