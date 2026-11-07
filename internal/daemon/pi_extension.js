// This source stays import-free so Node can run its state-machine tests without
// pi's loader aliases. Go replaces the three placeholders before writing it.
// Node builtins are reached through process.getBuiltinModule for the same
// reason: it needs no import and works under the tests' Function wrapper.

export default function (pi) {
  const THRESHOLD = __CHECKPOINT_THRESHOLD__;
  const ENABLED = __CHECKPOINT_ENABLED__;
  const TICKET_ID = process.env.KONTORA_TICKET_ID;
  // Capture the daemon's invocation, never identities supplied by the model.
  const RESULT_CONTEXT = {
    KONTORA_RESULT_VERSION: process.env.KONTORA_RESULT_VERSION,
    KONTORA_INVOCATION_ID: process.env.KONTORA_INVOCATION_ID,
    KONTORA_RESULT_TICKET: process.env.KONTORA_RESULT_TICKET,
    KONTORA_RESULT_STAGE: process.env.KONTORA_RESULT_STAGE,
    KONTORA_RESULT_FILE: process.env.KONTORA_RESULT_FILE,
  };
  const RESULT_ENABLED = Object.values(RESULT_CONTEXT).some((v) => v !== undefined && v !== "");
  if (RESULT_ENABLED) {
    for (var key in RESULT_CONTEXT) {
      if (!RESULT_CONTEXT[key] || !RESULT_CONTEXT[key].trim()) {
        throw new Error("Missing result execution context: " + key);
      }
    }
    if (RESULT_CONTEXT.KONTORA_RESULT_VERSION !== "1") {
      throw new Error("Unsupported KONTORA_RESULT_VERSION.");
    }
  }
  // Where to publish the "blocked on a question" marker the daemon polls.
  // Empty for a run the daemon does not watch.
  const WAIT_MARKER = __WAIT_MARKER_PATH__;
  // The question only has to fit a board badge and one line of last_error.
  // Bytes, not characters, so Go and this file cut identically.
  const QUESTION_BYTES = 200;

  // Tool call id -> the marker entry for that question, for the calls still in
  // flight. A null prototype so a tool call id of "constructor" cannot read a
  // function where an entry is meant.
  let openQuestions = Object.create(null);
  let shutdownRequested = false;
  let checkpointPending = false;
  let compactionInFlight = false;
  let continuationPending = false;
  let deferredContinuation = null;
  let generation = 0;
  let consumedCheckpoints = {};
  let pendingCheckpoint = null;
  let terminalReported = false;
  let terminalAtTurnStart = false;

  function terminalResultPresent() {
    if (!terminalReported && RESULT_ENABLED) {
      var fs = nodeModule("node:fs");
      terminalReported = !!(fs && fs.existsSync(RESULT_CONTEXT.KONTORA_RESULT_FILE));
    }
    if (terminalReported) {
      checkpointPending = false;
      pendingCheckpoint = null;
      continuationPending = false;
      deferredContinuation = null;
    }
    return terminalReported;
  }

  // The final-turn abort cancels queued work. Filtering also removes stale
  // checkpoint text from the ordinary response after result publication.
  pi.on("context", function (event, _ctx) {
    if (!terminalResultPresent()) return;
    return {
      messages: event.messages.filter((m) => m.customType !== "kontora-continuation"),
    };
  });

  pi.on("tool_call", function (_event, _ctx) {
    if (terminalResultPresent()) {
      return { block: true, reason: "A terminal Kontora result was published; further tools are blocked.", terminate: true };
    }
  });

  pi.on("turn_start", function () {
    terminalAtTurnStart = terminalResultPresent();
  });

  pi.on("turn_end", function (event, ctx) {
    if (ctx.mode !== "tui") return;
    var message = event.message;
    if (!message || message.role !== "assistant" || !terminalResultPresent()) return;
    if (!terminalAtTurnStart && message.content.some((part) => part.type === "toolCall")) return;
    // Keep the response after the publishing tool, then clear TUI queues even
    // when that response is truncated or attempts another tool call.
    // Awaiting idleness here would deadlock the active run.
    ctx.abort();
  });

  function continueWhenIdle(text, ctx) {
    if (terminalResultPresent()) {
      tryShutdown(ctx);
      return;
    }
    // ExtensionAPI has no queue-cancellation method. Do not put supervised
    // follow-ups in Pi's queue while the ordinary response can still report.
    if (RESULT_ENABLED && !ctx.isIdle()) {
      deferredContinuation = text;
      continuationPending = true;
      return;
    }
    deferredContinuation = null;
    continuationPending = true;
    pi.sendMessage(
      { customType: "kontora-continuation", content: text, display: false },
      { triggerTurn: true, deliverAs: "followUp" }
    );
  }

  // A call to one of these does not return until the human answers, and no pi
  // event says "a question is open", so the tool call is the only signal.
  function isQuestionTool(name) {
    return name === "ask_user_question" || name === "question";
  }

  function nodeModule(name) {
    try {
      return process.getBuiltinModule(name);
    } catch (e) {
      return null;
    }
  }

  // Cut to at most limit bytes without splitting a UTF-8 character: step back
  // off any continuation byte the cut landed on.
  function truncateBytes(s, limit) {
    var buf = Buffer.from(s, "utf8");
    if (buf.length <= limit) return s;
    var end = limit;
    while (end > 0 && (buf[end] & 0xc0) === 0x80) end--;
    return buf.toString("utf8", 0, end);
  }

  // ask_user_question takes {questions: [{question, ...}]}, pi's example
  // question tool takes {question, options}. An unknown shape leaves the badge
  // with the tool name alone.
  function questionText(args) {
    if (!args || typeof args !== "object") return "";
    var raw = "";
    if (Array.isArray(args.questions) && args.questions.length > 0) {
      var first = args.questions[0];
      if (first && typeof first.question === "string") raw = first.question;
      else if (typeof first === "string") raw = first;
    } else if (typeof args.question === "string") {
      raw = args.question;
    }
    return truncateBytes(raw.replace(/\s+/g, " ").trim(), QUESTION_BYTES);
  }

  // The daemon polls the marker path with no coordination, so the write goes to
  // a temp file in the same directory and is renamed in: a rename is atomic
  // within one directory, a partial write is not.
  function writeMarker(entry) {
    var fs = nodeModule("node:fs");
    if (!fs || !WAIT_MARKER) return;
    var tmp = WAIT_MARKER + ".tmp." + process.pid;
    try {
      fs.mkdirSync(WAIT_MARKER.replace(/[/\\][^/\\]*$/, ""), { recursive: true });
      fs.writeFileSync(tmp, JSON.stringify(entry));
      fs.renameSync(tmp, WAIT_MARKER);
    } catch (e) {
      try {
        fs.rmSync(tmp, { force: true });
      } catch (e2) {
        /* the temp file is the daemon's to ignore either way */
      }
    }
  }

  function removeMarker() {
    var fs = nodeModule("node:fs");
    if (!fs || !WAIT_MARKER) return;
    try {
      fs.rmSync(WAIT_MARKER, { force: true });
    } catch (e) {
      /* a marker left behind is cleared by the daemon when the run ends */
    }
  }

  // The oldest open question is the one that has blocked the longest, so it is
  // what the marker falls back to. ISO-8601 UTC strings sort chronologically.
  function oldestOpenQuestion() {
    var oldest = null;
    for (var id in openQuestions) {
      var e = openQuestions[id];
      if (!oldest || e.started_at < oldest.started_at) oldest = e;
    }
    return oldest;
  }

  function tryShutdown(ctx) {
    if (!shutdownRequested) return;
    if (!terminalResultPresent() &&
        (checkpointPending || compactionInFlight || continuationPending)) return;
    ctx.shutdown();
  }

  pi.on("agent_settled", function (_event, ctx) {
    shutdownRequested = true;
    if (!terminalResultPresent() && deferredContinuation !== null) {
      continueWhenIdle(deferredContinuation, ctx);
    }
    tryShutdown(ctx);
  });

  pi.on("agent_start", function (_event, _ctx) {
    shutdownRequested = false;
    continuationPending = false;
  });

  if (WAIT_MARKER) {
    pi.on("tool_execution_start", function (event, _ctx) {
      if (!event || !isQuestionTool(event.toolName)) return;
      var entry = {
        tool: event.toolName,
        tool_call_id: event.toolCallId,
        started_at: new Date().toISOString(),
        question: questionText(event.args),
      };
      openQuestions[event.toolCallId] = entry;
      // Rewritten on every question that opens, so the marker describes the
      // most recent one rather than one the human may already have answered.
      writeMarker(entry);
    });

    pi.on("tool_execution_end", function (event, _ctx) {
      if (!event || !isQuestionTool(event.toolName)) return;
      delete openQuestions[event.toolCallId];
      // Questions can be answered out of order, so the marker falls back to the
      // one still blocking rather than keeping the answered call's text.
      var next = oldestOpenQuestion();
      if (next) writeMarker(next);
      else removeMarker();
    });
  }

  pi.on("session_start", function (_event, ctx) {
    // Rebuild consumed checkpoints from the branch so we reject duplicates
    // after a daemon restart that re-delivers the same session.
    var branch = ctx.sessionManager.getBranch();
    for (var i = 0; i < branch.length; i++) {
      var entry = branch[i];
      if (
        entry.type === "custom" &&
        entry.customType === "kontora-phase-checkpoint"
      ) {
        var d = entry.data;
        if (d && d.completed_phase) {
          consumedCheckpoints[d.completed_phase] = true;
        }
      }
    }
  });

  pi.on("session_shutdown", function (_event, _ctx) {
    // A callback from the previous generation must not enqueue a message.
    generation++;
    deferredContinuation = null;
    // A clean /exit while a question is open must not leave the marker behind.
    openQuestions = Object.create(null);
    removeMarker();
  });

  function registerHandoff(verb, acknowledgement) {
    pi.registerTool({
      name: "kontora_" + verb,
      label: verb === "note" ? "Ticket Note" : "Ticket Summary",
      description:
        (verb === "note" ? "Append a note to" : "Set the summary of") +
        " the current Kontora ticket. Text is submitted literally without a shell. " +
        "Text must be nonblank, contain no NUL, and fit 32,768 UTF-8 bytes. " +
        "Text appears in process arguments; use file-to-stdin submission for sensitive or larger text. " +
        "If submission fails, read the ticket before resubmitting.",
      parameters: {
        type: "object",
        properties: { text: { type: "string", description: "The text to submit." } },
        required: ["text"],
      },
      executionMode: "sequential",
      execute: async function (_toolCallId, params, signal, _onUpdate, ctx) {
        var text = params && params.text;
        if (typeof text !== "string" || text.trim() === "") {
          throw new Error("Text must be a nonblank string.");
        }
        if (text.includes("\0")) {
          throw new Error("Text must not contain NUL.");
        }
        if (Buffer.byteLength(text, "utf8") > 32768) {
          throw new Error("Text must not exceed 32,768 UTF-8 bytes.");
        }
        if (signal && signal.aborted) {
          throw new Error("Submission cancelled; text was not submitted.");
        }

        var uncertain = "The write outcome may be unknown. Read the ticket before resubmitting. Do not retry automatically.";
        var result;
        try {
          result = await pi.exec("kontora", [verb, "--", TICKET_ID, text], {
            cwd: ctx.cwd,
            signal: signal,
            timeout: 30_000,
          });
        } catch (_error) {
          throw new Error("Kontora execution failed. " + uncertain);
        }
        if (result.code !== 0 || result.killed) {
          throw new Error("Kontora execution did not succeed. " + uncertain);
        }
        // A signal-killed process can report code 0, so require confirm's line too.
        var lines = result.stdout.replace(/\x1b\[[0-9;:]*m/g, "").split(/\r?\n/);
        if (!lines.includes(TICKET_ID + " " + acknowledgement)) {
          throw new Error("Kontora acknowledgement is missing. " + uncertain);
        }
        return {
          content: [{ type: "text", text: TICKET_ID + " " + acknowledgement + "." }],
          details: undefined,
        };
      },
    });
  }

  if (TICKET_ID) {
    registerHandoff("note", "note added");
    registerHandoff("summary", "summary set");
  }

  if (RESULT_ENABLED) {
    pi.registerTool({
      name: "kontora_result",
      label: "Stage Result",
      description:
        "Report the current Kontora stage as complete or blocked. Summary must be nonblank; " +
        "blocked also requires a nonblank next_step. Submits literal JSON without a shell. " +
        "The payload must fit 32,768 UTF-8 bytes and contain no NUL. " +
        "Reporting does not kill the agent. Do not retry automatically on uncertain submission.",
      parameters: {
        type: "object",
        properties: {
          outcome: { type: "string", enum: ["complete", "blocked"] },
          summary: { type: "string", minLength: 1 },
          next_step: { type: "string" },
        },
        required: ["outcome", "summary"],
        additionalProperties: false,
      },
      executionMode: "sequential",
      execute: async function (_toolCallId, params, signal, _onUpdate, ctx) {
        var outcome = params && params.outcome;
        if (outcome !== "complete" && outcome !== "blocked") {
          throw new Error("Outcome must be complete or blocked.");
        }
        var payload = { outcome: outcome, summary: params.summary };
        if (typeof payload.summary !== "string" || !payload.summary.trim()) {
          throw new Error("Summary must be a nonblank string.");
        }
        if (params.next_step !== undefined) payload.next_step = params.next_step;
        if (outcome === "blocked" &&
            (typeof payload.next_step !== "string" || !payload.next_step.trim())) {
          throw new Error("Blocked results require a nonblank next_step.");
        }
        if (payload.next_step !== undefined && typeof payload.next_step !== "string") {
          throw new Error("next_step must be a string.");
        }
        if (payload.summary.includes("\0") ||
            (payload.next_step !== undefined && payload.next_step.includes("\0"))) {
          throw new Error("Result text must not contain NUL.");
        }
        var json = JSON.stringify(payload);
        if (Buffer.byteLength(json, "utf8") > 32768) {
          throw new Error("Result payload must not exceed 32,768 UTF-8 bytes.");
        }
        if (signal && signal.aborted) {
          throw new Error("Submission cancelled; result was not submitted.");
        }
        var fs = nodeModule("node:fs");
        var os = nodeModule("node:os");
        var path = nodeModule("node:path");
        if (!fs || !os || !path) throw new Error("Result submission requires Node filesystem support.");
        var dir = fs.mkdtempSync(path.join(os.tmpdir(), "kontora-result-"));
        var inputFile = path.join(dir, "payload.json");
        var uncertain = "The result outcome may be unknown. Inspect " +
          JSON.stringify(RESULT_CONTEXT.KONTORA_RESULT_FILE) + " before resubmitting. Do not retry automatically.";
        try {
          fs.writeFileSync(inputFile, json, { mode: 0o600, flag: "wx" });
          var result;
          try {
            result = await pi.exec("kontora", [
              "result", "--input-file", inputFile, RESULT_CONTEXT.KONTORA_RESULT_TICKET,
            ], { cwd: ctx.cwd, signal: signal, timeout: 30_000, maxBuffer: 32768 });
          } catch (error) {
            throw new Error("Kontora execution failed: " +
              (error && error.message ? error.message : String(error)) + ". " + uncertain);
          }
          var execution = "Exit status=" + result.code + ", killed=" + !!result.killed +
            ", truncated=" + !!result.truncated + ". stderr: " + (result.stderr || "(empty)");
          if (result.code !== 0 || result.killed || result.truncated) {
            throw new Error("Kontora execution did not succeed. " + execution + "\n" + uncertain);
          }
          var ack;
          try {
            ack = JSON.parse(result.stdout);
          } catch (_error) {
            throw new Error("Invalid Kontora result acknowledgement. " + execution + "\n" + uncertain);
          }
          if (!ack || ack.accepted !== true ||
              ack.invocation_id !== RESULT_CONTEXT.KONTORA_INVOCATION_ID ||
              ack.outcome !== outcome) {
            throw new Error("Kontora result acknowledgement does not match. " + execution + "\n" + uncertain);
          }
          terminalReported = true;
          terminalResultPresent();
          return {
            content: [{ type: "text", text: "Kontora stage result accepted: " + outcome + "." }],
            details: ack,
          };
        } finally {
          fs.rmSync(dir, { recursive: true, force: true });
        }
      },
    });
  }

  if (ENABLED && THRESHOLD > 0) {
    pi.registerTool({
      name: "kontora_phase_complete",
      label: "Phase Complete",
      description:
        "Signal that a top-level ticket phase is complete and the next phase " +
        "should begin. Call this only at boundaries between top-level ticket " +
        "phases, after all phase-specific tests pass and a phase note has been " +
        "written. Do not call after the final phase.",
      parameters: {
        type: "object",
        properties: {
          completed_phase: {
            type: "string",
            description:
              "The phase that just completed, e.g. 'Phase 2: Expose the estimator as a local command'",
          },
          next_phase: {
            type: "string",
            description:
              "The phase to begin next, e.g. 'Phase 3: Add the agent option and the embedded extension'",
          },
        },
        required: ["completed_phase", "next_phase"],
      },
      execute: function (_toolCallId, params, _signal, _onUpdate, ctx) {
        if (terminalResultPresent()) {
          return {
            content: [{ type: "text", text: "A terminal result was reported; checkpoint continuation is suppressed." }],
            details: undefined,
          };
        }
        var completed = params && params.completed_phase;
        var next = params && params.next_phase;

        if (
          typeof completed !== "string" ||
          completed.trim() === "" ||
          typeof next !== "string" ||
          next.trim() === ""
        ) {
          return {
            content: [
              {
                type: "text",
                text: "Both completed_phase and next_phase must be non-empty strings.",
              },
            ],
            isError: true,
          };
        }

        if (consumedCheckpoints[completed]) {
          return {
            content: [
              {
                type: "text",
                text:
                  "Phase '" +
                  completed +
                  "' was already checkpointed. Continue with " +
                  next +
                  ".",
              },
            ],
          };
        }

        if (checkpointPending) {
          return {
            content: [
              {
                type: "text",
                text:
                  "A checkpoint is already pending. Continue with " +
                  next +
                  " without calling this tool again.",
              },
            ],
          };
        }

        if (compactionInFlight) {
          return {
            content: [
              {
                type: "text",
                text:
                  "A compaction is already in flight. Continue with " +
                  next +
                  " without calling this tool again.",
              },
            ],
          };
        }

        var usage = ctx.getContextUsage();
        var tokens =
          usage && typeof usage.tokens === "number" ? usage.tokens : null;

        var outcome;
        if (tokens === null) {
          outcome = "skipped";
        } else if (tokens <= THRESHOLD) {
          outcome = "skipped";
        } else {
          outcome = "requested";
        }

        pi.appendEntry("kontora-phase-checkpoint", {
          completed_phase: completed,
          next_phase: next,
          context_tokens: tokens,
          threshold: THRESHOLD,
          outcome: outcome,
        });

        consumedCheckpoints[completed] = true;

        if (outcome === "requested") {
          checkpointPending = true;
          pendingCheckpoint = {
            completed_phase: completed,
            next_phase: next,
            context_tokens: tokens,
          };
          return {
            content: [
              {
                type: "text",
                text:
                  "Checkpoint recorded for '" +
                  completed +
                  "'. Context (" +
                  tokens +
                  " tokens) exceeds threshold (" +
                  THRESHOLD +
                  "). Compaction will start after this turn. Continue with " +
                  next +
                  ".",
              },
            ],
          };
        }

        return {
          content: [
            {
              type: "text",
              text:
                "Checkpoint recorded for '" +
                completed +
                "'. Context " +
                (tokens === null
                  ? "is unknown"
                  : "(" + tokens + " tokens) is within threshold (" + THRESHOLD + ")") +
                ", no compaction needed. Continue with " +
                next +
                ".",
            },
          ],
        };
      },
    });

    pi.on("turn_end", function (_event, ctx) {
      if (terminalResultPresent()) return;
      if (!checkpointPending || !pendingCheckpoint) return;

      var cp = pendingCheckpoint;
      checkpointPending = false;
      pendingCheckpoint = null;

      if (compactionInFlight) return;

      var capturedGen = generation;
      var callbackHandled = false;
      compactionInFlight = true;

      var continuationText =
        "Continue with " +
        cp.next_phase +
        ". Re-read the ticket note, then inspect git status and the " +
        "current diff before editing. Do not redo completed phases and " +
        "do not rely on the compaction summary as the only source of truth.";

      ctx.compact({
        customInstructions:
          "Preserve the ticket goal, requirements, cross-phase invariants, " +
          "modified files, test results, decisions, unresolved failures, " +
          "and the named next phase: " +
          cp.next_phase +
          ".",
        onComplete: function (result) {
          if (callbackHandled) return;
          callbackHandled = true;
          compactionInFlight = false;

          if (capturedGen !== generation) return;
          if (terminalResultPresent()) {
            tryShutdown(ctx);
            return;
          }

          pi.appendEntry("kontora-phase-checkpoint", {
            completed_phase: cp.completed_phase,
            next_phase: cp.next_phase,
            context_tokens: cp.context_tokens,
            threshold: THRESHOLD,
            outcome: "compacted",
            compaction_usage: result && result.usage,
            estimated_post_compaction_tokens:
              result && result.estimatedTokensAfter,
          });

          if (!continuationPending) continueWhenIdle(continuationText, ctx);
        },
        onError: function (error) {
          if (callbackHandled) return;
          callbackHandled = true;
          compactionInFlight = false;

          if (capturedGen !== generation) return;
          if (terminalResultPresent()) {
            tryShutdown(ctx);
            return;
          }

          var msg = error && error.message ? error.message : "";
          var outcome = msg.toLowerCase().indexOf("already compacted") !== -1
            ? "compacted"
            : "failed";

          pi.appendEntry("kontora-phase-checkpoint", {
            completed_phase: cp.completed_phase,
            next_phase: cp.next_phase,
            context_tokens: cp.context_tokens,
            threshold: THRESHOLD,
            outcome: outcome,
            error: msg,
          });

          if (!continuationPending) continueWhenIdle(continuationText, ctx);
        },
      });
    });
  }
}
