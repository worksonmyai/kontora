// Run separately with an installed Pi SDK:
// node --test internal/daemon/testdata/pi_extension.sdk.test.mjs
// KONTORA_PI_SDK_DIR can select the installation. No provider transport is used.

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

const sdkDir = process.env.KONTORA_PI_SDK_DIR || path.join(
  execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim(),
  "@earendil-works/pi-coding-agent"
);
const sdkImport = (file) => import(pathToFileURL(path.join(sdkDir, file)).href);
const { createAgentSession, createExtensionRuntime, createEventBus, ModelRuntime, SessionManager, SettingsManager } = await sdkImport("dist/index.js");
const { loadExtensionFromFactory } = await sdkImport("dist/core/extensions/loader.js");
const { Agent } = await sdkImport("node_modules/@earendil-works/pi-agent-core/dist/agent.js");
const { convertToLlm } = await sdkImport("dist/core/messages.js");
const { InteractiveMode } = await sdkImport("dist/modes/interactive/interactive-mode.js");
const { AssistantMessageEventStream } = await sdkImport("node_modules/@earendil-works/pi-ai/dist/utils/event-stream.js");
const sdkVersion = JSON.parse(readFileSync(path.join(sdkDir, "package.json"), "utf8")).version;
const raw = readFileSync(process.env.KONTORA_PI_SDK_TEST_SOURCE || new URL("../pi_extension.js", import.meta.url), "utf8");
const factory = new Function(raw.replaceAll("__CHECKPOINT_THRESHOLD__", "1")
  .replaceAll("__CHECKPOINT_ENABLED__", "true").replaceAll("__WAIT_MARKER_PATH__", '""')
  .replace("export default function", "return function"))();
const usage = { input: 200, output: 10, cacheRead: 0, cacheWrite: 0, totalTokens: 210,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const checkpointArgs = { completed_phase: "one", next_phase: "two" };

function fixture(t, automation = false) {
  const cwd = mkdtempSync(path.join(tmpdir(), "kontora-pi-sdk-"));
  const resultFile = path.join(cwd, "result artifact 雪.json");
  const env = { KONTORA_RESULT_VERSION: "1", KONTORA_INVOCATION_ID: "sdk-invocation",
    KONTORA_RESULT_TICKET: automation ? "" : "sdk-ticket", KONTORA_RESULT_STAGE: automation ? "" : "review", KONTORA_RESULT_FILE: resultFile,
    KONTORA_RESULT_AUTOMATION: automation ? "sdk-automation" : "", KONTORA_RESULT_RUN_NUMBER: automation ? "7" : "" };
  const previous = Object.fromEntries(Object.keys(env).map((key) => [key, process.env[key]]));
  Object.assign(process.env, env);
  const originalFetch = globalThis.fetch;
  let networkAttempts = 0;
  globalThis.fetch = () => { networkAttempts++; throw new Error("Network disabled in Pi regression"); };
  t.after(() => {
    globalThis.fetch = originalFetch;
    for (const key of Object.keys(env)) {
      if (previous[key] === undefined) delete process.env[key];
      else process.env[key] = previous[key];
    }
    rmSync(cwd, { recursive: true, force: true });
    assert.equal(networkAttempts, 0);
  });
  return { cwd, resultFile, env };
}

function assistant(model, content, stopReason = "stop") {
  return { role: "assistant", content, api: model.api, provider: model.provider, model: model.id,
    usage, stopReason, timestamp: Date.now() };
}

function streamMessage(message) {
  const stream = new AssistantMessageEventStream();
  queueMicrotask(() => { stream.push({ type: "done", reason: message.stopReason, message }); stream.end(); });
  return stream;
}

for (const [callback, publish] of [["complete", true], ["error", true], ["complete", false], ["error", false]]) {
  test(`installed SDK ${sdkVersion}: real ${callback} callback, terminal=${publish}`, { timeout: 5000 }, async (t) => {
    const { cwd, resultFile } = fixture(t);
    const trace = [];
    let requests = 0, shutdowns = 0, settled = 0;
    const callbackDone = Promise.withResolvers();
    const runtime = createExtensionRuntime();
    const extension = await loadExtensionFromFactory((pi) => {
      const observed = Object.create(pi);
      observed.on = (name, handler) => pi.on(name, (event, ctx) => {
        if (name !== "turn_end") return handler(event, ctx);
        const context = Object.create(ctx);
        context.compact = (options) => ctx.compact({ ...options,
          onComplete: (result) => { trace.push("onComplete"); options.onComplete(result); callbackDone.resolve(); },
          onError: (error) => { trace.push("onError"); options.onError(error); callbackDone.resolve(); },
        });
        return handler(event, context);
      });
      factory(observed);
      pi.on("agent_settled", () => { settled++; trace.push("agent_settled"); });
      pi.on("session_before_compact", (event) => {
        assert.equal(settled, 1, "real settlement precedes compaction preparation");
        if (publish) {
          writeFileSync(resultFile, JSON.stringify({ outcome: "complete", summary: "Stage done" }));
          trace.push("terminal publication");
        }
        if (callback === "error") return { cancel: true };
        return { compaction: { summary: "Offline compacted history",
          firstKeptEntryId: event.preparation.firstKeptEntryId, tokensBefore: event.preparation.tokensBefore } };
      });
    }, cwd, createEventBus(), runtime);
    const resourceLoader = {
      getExtensions: () => ({ extensions: [extension], errors: [], runtime }),
      getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
      getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
      getSystemPrompt: () => "Offline test", getSystemPromptSource: () => undefined,
      getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources() {}, async reload() {},
    };
    const modelRuntime = await ModelRuntime.create({ authPath: path.join(cwd, "auth.json"), modelsPath: null,
      modelsStorePath: path.join(cwd, "models-cache"), refreshOnCreate: false, allowModelNetwork: false });
    modelRuntime.registerProvider("offline-test", { apiKey: "unused", baseUrl: "http://localhost:1", api: "openai-completions",
      models: [{ id: "fake", name: "Fake", reasoning: false, input: ["text"], contextWindow: 200000,
        maxTokens: 4000, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] });
    const model = modelRuntime.getModel("offline-test", "fake");
    const manager = SessionManager.inMemory(cwd);
    manager.appendMessage({ role: "user", content: "Previous work ".repeat(200), timestamp: Date.now() });
    manager.appendMessage(assistant(model, [{ type: "text", text: "Previous response ".repeat(200) }]));
    const { session } = await createAgentSession({ cwd, agentDir: cwd, model, thinkingLevel: "off", modelRuntime,
      resourceLoader, noTools: true, sessionManager: manager,
      settingsManager: SettingsManager.inMemory({ compaction: { enabled: false, keepRecentTokens: 1, reserveTokens: 1 },
        retry: { enabled: false }, cacheWarming: "off" }) });
    t.after(() => session.dispose());
    session.agent.streamFunction = () => {
      requests++;
      assert.equal(existsSync(resultFile), false, "no model request after terminal publication");
      trace.push(`request ${requests}`);
      return streamMessage(requests === 1
        ? assistant(model, [{ type: "toolCall", id: "cp", name: "kontora_phase_complete", arguments: checkpointArgs }], "toolUse")
        : assistant(model, [{ type: "text", text: "Done." }]));
    };
    await session.bindExtensions({ mode: "tui", shutdownHandler: () => { shutdowns++; trace.push("shutdown"); },
      onError: (error) => assert.fail(error.error) });
    await session.prompt("Call the phase checkpoint.");
    await callbackDone.promise;
    await session.waitForIdle();
    t.diagnostic(JSON.stringify({ trace, requests, settled, shutdowns, idle: session.isIdle }));
    assert.equal(session.isIdle, true);
    assert.equal(settled, publish ? 1 : 2);
    assert.equal(requests, publish ? 2 : 3);
    assert.equal(shutdowns, 1);
    if (publish) assert.deepEqual(trace.slice(-4), ["agent_settled", "terminal publication", callback === "complete" ? "onComplete" : "onError", "shutdown"]);
  });
}

for (const callback of ["onComplete", "onError"]) {
  for (const transport of ["tool", "file"]) {
    for (const [outcome, response] of ["complete", "blocked"].flatMap((outcome) =>
      ["stop", "length", "blocked tool"].map((response) => [outcome, response]))) {
      test(`installed TUI ${sdkVersion}: prequeued ${callback}, ${transport} ${outcome}, ${response}`, { timeout: 5000 }, async (t) => {
        const { cwd, resultFile, env } = fixture(t);
        const trace = [];
        let pi, compaction, requests = 0, aborts = 0, shutdowns = 0, sentinelExecutions = 0;
        const runtime = createExtensionRuntime();
        const extension = await loadExtensionFromFactory((api) => {
          pi = api;
          const observed = Object.create(api);
          observed.on = (name, handler) => api.on(name, (event, ctx) => {
            const context = Object.create(ctx);
            context.compact = (options) => { compaction = options; };
            return handler(event, context);
          });
          observed.exec = async (_command, args) => {
            const payload = JSON.parse(readFileSync(args[2], "utf8"));
            writeFileSync(resultFile, JSON.stringify(payload), { flag: "wx" });
            trace.push("terminal publication");
            return { code: 0, killed: false, truncated: false, stderr: "",
              stdout: JSON.stringify({ accepted: true, invocation_id: env.KONTORA_INVOCATION_ID, outcome }) };
          };
          factory(observed);
          api.registerTool({ name: "publish_file", label: "Publish file", description: "Offline file result",
            parameters: { type: "object", properties: {} },
            execute: async () => {
              writeFileSync(resultFile, JSON.stringify({ outcome, summary: "Done" }));
              trace.push("terminal publication");
              return { content: [{ type: "text", text: "Published" }], details: undefined };
            } });
          api.registerTool({ name: "sentinel", label: "Sentinel", description: "Count unexpected execution",
            parameters: { type: "object", properties: {} },
            execute: async () => {
              sentinelExecutions++;
              return { content: [{ type: "text", text: "Unexpected execution" }], details: undefined };
            } });
          api.on("tool_execution_end", (event) => {
            if (event.toolName === "sentinel") trace.push(`sentinel finalized, isError=${event.isError}`);
          });
          api.on("message_end", (event) => {
            if (event.message.role === "assistant") trace.push(`message_end ${event.message.stopReason}`);
          });
          api.on("agent_settled", () => trace.push("agent_settled"));
        }, cwd, createEventBus(), runtime);
        const resourceLoader = {
          getExtensions: () => ({ extensions: [extension], errors: [], runtime }),
          getSkills: () => ({ skills: [], diagnostics: [] }), getPrompts: () => ({ prompts: [], diagnostics: [] }),
          getThemes: () => ({ themes: [], diagnostics: [] }), getAgentsFiles: () => ({ agentsFiles: [] }),
          getSystemPrompt: () => "Offline test", getSystemPromptSource: () => undefined,
          getAppendSystemPrompt: () => [], getAppendSystemPromptSources: () => [], extendResources() {}, async reload() {},
        };
        const modelRuntime = await ModelRuntime.create({ authPath: path.join(cwd, "auth.json"), modelsPath: null,
          modelsStorePath: path.join(cwd, "models-cache"), refreshOnCreate: false, allowModelNetwork: false });
        modelRuntime.registerProvider("offline-test", { apiKey: "unused", baseUrl: "http://localhost:1", api: "openai-completions",
          models: [{ id: "fake", name: "Fake", reasoning: false, input: ["text"], contextWindow: 200000,
            maxTokens: 4000, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] });
        const model = modelRuntime.getModel("offline-test", "fake");
        const manager = SessionManager.inMemory(cwd);
        const { session } = await createAgentSession({ cwd, agentDir: cwd, model, thinkingLevel: "off", modelRuntime,
          resourceLoader, noTools: true, sessionManager: manager,
          settingsManager: SettingsManager.inMemory({ compaction: { enabled: false },
            retry: { enabled: false }, cacheWarming: "off" }) });
        t.after(() => session.dispose());
        // Use Pi's TUI queue cancellation code, with only terminal rendering replaced.
        const host = Object.create(InteractiveMode.prototype);
        Object.defineProperty(host, "session", { value: session });
        Object.assign(host, { compactionQueuedMessages: [], updatePendingMessagesDisplay() {},
          createExtensionUIContext() {}, setupAutocompleteProvider() {}, setupExtensionShortcuts() {},
          showLoadedResources() {}, showStartupNoticesIfNeeded() {},
          showExtensionError: (_path, error) => assert.fail(error),
          shutdown: () => { shutdowns++; trace.push("shutdown"); },
          restoreQueuedMessagesToEditor(options) {
            aborts++;
            trace.push("abort");
            return InteractiveMode.prototype.restoreQueuedMessagesToEditor.call(this, options);
          },
        });
        await host.bindCurrentSessionExtensions();
        session.agent.streamFunction = (_model, _context, options) => {
          requests++;
          trace.push(`request ${requests}`);
          assert.equal(options.signal.aborted, false, "ordinary response must not start aborted");
          if (requests === 1) return streamMessage(assistant(model,
            [{ type: "toolCall", id: "cp", name: "kontora_phase_complete", arguments: checkpointArgs }], "toolUse"));
          if (requests === 2) {
            compaction[callback](new Error("Offline compaction failure"));
            for (let i = 0; i < 2; i++) pi.sendMessage({ customType: "kontora-continuation",
              content: `Previously dispatched checkpoint ${i}`, display: false }, { triggerTurn: true, deliverAs: "followUp" });
            assert.equal(session.agent.hasQueuedMessages(), true, "SDK must own the queued continuations");
            trace.push("follow-ups queued");
            return streamMessage(assistant(model, [{ type: "toolCall", id: "result",
              name: transport === "tool" ? "kontora_result" : "publish_file",
              arguments: transport === "tool" ? { outcome, summary: "Done", next_step: "Provide access" } : {} }], "toolUse"));
          }
          if (requests > 3) return streamMessage(assistant(model,
            [{ type: "text", text: "Unexpected queued continuation." }]));
          if (response === "blocked tool") return streamMessage(assistant(model,
            [{ type: "toolCall", id: "sentinel", name: "sentinel", arguments: {} }], "toolUse"));
          const message = assistant(model, [{ type: "text", text: "Ordinary final response." }], response);
          const stream = new AssistantMessageEventStream();
          queueMicrotask(() => {
            stream.push({ type: "start", partial: { ...message, content: [] } });
            setImmediate(() => {
              assert.equal(options.signal.aborted, false, "do not abort while the ordinary final answer is streaming");
              trace.push("ordinary final response delivered");
              stream.push({ type: "done", reason: response, message });
              stream.end();
            });
          });
          return stream;
        };
        await session.prompt("Finish the stage.");
        await session.waitForIdle();
        t.diagnostic(JSON.stringify({ trace, requests, aborts, shutdowns, sentinelExecutions, idle: session.isIdle }));
        assert.equal(sentinelExecutions, 0, "tools must not execute after terminal publication");
        if (response === "blocked tool") {
          const blocked = session.messages.find((message) => message.role === "toolResult" && message.toolName === "sentinel");
          assert.ok(blocked, "the installed SDK must finalize the attempted tool call");
          assert.equal(blocked.isError, true);
          assert.match(blocked.content[0].text, /terminal Kontora result/);
        }
        assert.equal(requests, 3, "no provider request from a queued continuation after the terminal response");
        assert.equal(session.agent.hasQueuedMessages(), false);
        assert.equal(session.isIdle, true);
        assert.equal(aborts, 1);
        assert.equal(shutdowns, 1);
        const final = manager.getBranch().filter((entry) => entry.type === "message" && entry.message.role === "assistant").at(-1).message;
        assert.equal(final.stopReason, response === "blocked tool" ? "toolUse" : response);
        if (response === "blocked tool") {
          assert.ok(trace.indexOf("abort") > trace.indexOf("sentinel finalized, isError=true"));
        } else {
          assert.equal(session.getLastAssistantText(), "Ordinary final response.");
          assert.ok(trace.indexOf("abort") > trace.indexOf("ordinary final response delivered"));
        }
      });
    }
  }
}

for (const callback of ["onComplete", "onError"]) {
  for (const transport of ["tool", "automation tool", "file", "none", "tool malicious", "file malicious"]) {
    test(`installed Agent ${sdkVersion}: busy ${callback}, terminal=${transport}`, { timeout: 5000 }, async (t) => {
      const automation = transport === "automation tool";
      const { cwd, resultFile, env } = fixture(t, automation);
      const mode = automation ? "tool" : transport.split(" ")[0];
      const malicious = transport.endsWith("malicious");
      const handlers = {}, tools = {}, sent = [];
      let compaction, agent, requests = 0, sentinelExecutions = 0, finalResponses = 0, shutdowns = 0;
      const trace = [];
      const ctx = { cwd, mode: "tui", getContextUsage: () => ({ tokens: 200 }), compact: (options) => { compaction = options; },
        isIdle: () => !agent.state.isStreaming, abort: () => agent.abort(),
        shutdown: () => { shutdowns++; }, sessionManager: { getBranch: () => [] } };
      const api = {
        on: (name, handler) => { (handlers[name] ??= []).push(handler); },
        registerTool: (tool) => { tools[tool.name] = tool; }, appendEntry() {},
        sendMessage: (message) => {
          sent.push(message);
          trace.push("SDK follow-up dispatched");
          agent.followUp({ role: "custom", ...message, timestamp: Date.now() });
        },
        exec: async (_command, args) => {
          assert.equal(args[3], automation ? "sdk-automation" : "sdk-ticket");
          const payload = JSON.parse(readFileSync(args[2], "utf8"));
          writeFileSync(resultFile, JSON.stringify(payload), { flag: "wx" });
          trace.push("terminal publication");
          return { code: 0, killed: false, truncated: false, stderr: "",
            stdout: JSON.stringify({ accepted: true, invocation_id: env.KONTORA_INVOCATION_ID, outcome: payload.outcome }) };
        },
      };
      factory(api);
      const fire = async (event) => {
        for (const handler of handlers[event.type] ?? []) await handler(event, ctx);
      };
      const model = { id: "fake", provider: "offline-test", api: "offline-test" };
      agent = new Agent({ initialState: { model, tools: [
        { ...tools.kontora_result, execute: (...args) => tools.kontora_result.execute(...args, ctx) },
        { name: "sentinel", description: "In-memory counter", parameters: { type: "object", properties: {} },
          execute: async () => { sentinelExecutions++; return { content: [{ type: "text", text: "ran" }], details: undefined }; } },
      ] }, convertToLlm, beforeToolCall: async ({ toolCall, args }) => {
        for (const handler of handlers.tool_call ?? []) {
          const result = await handler({ toolName: toolCall.name, input: args }, ctx);
          if (result?.block) return result;
        }
      }, transformContext: async (messages) => {
        for (const handler of handlers.context ?? []) messages = (await handler({ messages }, ctx))?.messages ?? messages;
        return messages;
      }, streamFn: () => {
        requests++;
        trace.push(`request ${requests}`);
        if (requests === 1) {
          compaction[callback](new Error("Offline compaction failure"));
          if (mode === "tool") return streamMessage(assistant(model,
            [{ type: "toolCall", id: "result", name: "kontora_result", arguments: { outcome: "complete", summary: "Done" } }], "toolUse"));
          if (mode === "file") {
            writeFileSync(resultFile, "{}");
            trace.push("terminal publication");
          }
        }
        const responseIndex = finalResponses++;
        if (!malicious && responseIndex === 0) return streamMessage(assistant(model, [{ type: "text", text: "Ordinary final response." }]));
        if (responseIndex === (malicious ? 0 : 1)) return streamMessage(assistant(model,
          [{ type: "toolCall", id: "sentinel", name: "sentinel", arguments: {} }], "toolUse"));
        return streamMessage(assistant(model, [{ type: "text", text: "Unexpected continuation finished." }]));
      } });
      t.after(() => agent.abort());
      agent.subscribe(fire);
      await tools.kontora_phase_complete.execute("cp", checkpointArgs, undefined, undefined, ctx);
      await fire({ type: "turn_end" });
      await agent.prompt("Finish the stage.");
      // AgentSession owns settlement; the installed Agent above owns request/tool ordering.
      await fire({ type: "agent_settled" });
      t.diagnostic(JSON.stringify({ trace, requests, sentinelExecutions, dispatched: sent.length, shutdowns }));
      const expectedRequests = mode === "tool" ? 2 : 1;
      assert.equal(requests, expectedRequests);
      assert.equal(sentinelExecutions, 0);
      assert.equal(sent.length, mode === "none" ? 1 : 0);
      assert.equal(shutdowns, mode === "none" ? 0 : 1);
      if (malicious) {
        const blocked = agent.state.messages.find((message) => message.role === "toolResult" && message.toolName === "sentinel");
        assert.equal(blocked.isError, true);
        assert.match(blocked.content[0].text, /terminal Kontora result/);
      }
    });
  }
}
