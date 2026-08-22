// The per-turn gate for a pi assistant thread. Every tool call is checked with
// the daemon before it runs, so a read-only thread is read-only at the tool
// boundary rather than by asking the agent nicely.
//
// Go replaces the four placeholders before writing the file. They are not read
// from the environment: the agent's own tools can read that, and the nonce is
// what stops an unrelated process approving its writes.

export default function (pi) {
  const BASE = "__KONTORA_URL__";
  const TOKEN = "__KONTORA_TOKEN__";
  const THREAD = "__KONTORA_THREAD__";
  const NONCE = "__KONTORA_NONCE__";

  pi.on("tool_call", async function (event) {
    if (!BASE || !THREAD || !NONCE) {
      return {
        block: true,
        reason:
          "The Kontora assistant gate is not reachable, so no tool call can be approved. Tell the user and stop.",
      };
    }

    const headers = { "content-type": "application/json" };
    if (TOKEN) headers.authorization = "Bearer " + TOKEN;

    let verdict;
    try {
      const res = await fetch(BASE + "/api/assistant/gate/ask", {
        method: "POST",
        headers: headers,
        body: JSON.stringify({
          thread: THREAD,
          nonce: NONCE,
          tool: event.toolName,
          input: event.input,
        }),
      });
      if (!res.ok) throw new Error("the gate answered " + res.status);
      verdict = await res.json();
    } catch (err) {
      return {
        block: true,
        reason:
          "The Kontora assistant gate could not be reached: " +
          ((err && err.message) || String(err)),
      };
    }

    if (verdict && verdict.allow) return;
    return {
      block: true,
      reason: (verdict && verdict.reason) || "The user did not approve this.",
    };
  });
}
