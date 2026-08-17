// State-machine tests for the Kontora pi extension.
//
// These evaluate the rendered JavaScript extension against a fake ExtensionAPI
// without starting pi or an LLM. The Go wrapper passes both rendered variants
// through environment variables. A direct Node run renders the same variants
// from ../pi_extension.js.
//
// Run with:  node --test internal/daemon/testdata/pi_extension.test.mjs

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// ─── Fake ExtensionAPI ──────────────────────────────────────────────────────

function createFakeAPI() {
  const handlers = {};
  const tools = {};
  const entries = [];
  const messages = [];

  const api = {
    handlers,
    tools,
    entries,
    messages,

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
      for (const h of hs) {
        await h(eventObj || {}, ctx);
      }
    },
  };

  return api;
}

function createFakeContext(overrides) {
  let shutdownCalled = 0;
  let compactCalls = [];

  const ctx = {
    shutdownCalled: () => shutdownCalled,
    compactCalls: () => compactCalls,

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
function loadExtension(source) {
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
  factory(api);
  return api;
}

const ENABLED_THRESHOLD = 150000;

function renderLocally(threshold, enabled) {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const raw = readFileSync(path.join(here, "..", "pi_extension.js"), "utf8");
  return raw
    .replaceAll("__CHECKPOINT_THRESHOLD__", String(threshold))
    .replaceAll("__CHECKPOINT_ENABLED__", String(enabled));
}

function checkRendered(name, source, threshold, enabled) {
  if (typeof source !== "string" || source.trim() === "") {
    throw new Error(`${name}: extension source is empty`);
  }
  if (source.includes("__CHECKPOINT_")) {
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
