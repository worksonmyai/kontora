import assert from "node:assert/strict";
import test from "node:test";
import { statsDerive } from "../ui/stats.js";

const cases = [
  { name: "verified complete", counters: { complete: 1 }, text: "1 complete · 0 failed", pct: 100 },
  { name: "blocked exit zero", counters: { blocked: 1 }, text: "1 blocked · 0 failed", pct: 0 },
  { name: "protocol error", counters: { protocol_error: 1 }, text: "1 protocol error · 0 failed", pct: 0 },
  { name: "legacy unverified", counters: { unknown: 1 }, text: "0 failed · 1 unverified", pct: 0 },
  { name: "semantic failure", counters: { failed: 1 }, text: "1 failed", pct: 0 },
  {
    name: "runner failure does not imply semantic failure",
    counters: { unknown: 1, runner_failed: 1 },
    text: "0 failed · 1 unverified · 1 runner failure",
    pct: 0,
  },
];

for (const c of cases) {
  test(`stats presents ${c.name} separately from runner exits`, () => {
    const derived = statsDerive({
      totals: { runs: 1, first_pass_pct: c.pct },
      stages: [{
        name: "default", runs: 1, token_runs: 1, tokens: 100, token_share: 100,
        complete: 0, blocked: 0, protocol_error: 0, failed: 0, unknown: 0, runner_failed: 0,
        ...c.counters,
      }],
    });
    assert.equal(derived.stages[0].time.meta, `1 runs · ${c.text}`);
    assert.equal(derived.stages[0].tokens.meta, `1 runs · ${c.text}`);
    assert.equal(derived.stagesByTokens[0].tokens.meta, `1 runs · ${c.text}`);
    const card = derived.kpis.find(k => k.label === "first-pass");
    assert.equal(card.delta, `${100 - c.pct}% not verified first-pass`);
    assert.doesNotMatch(card.delta, /retry/);
  });
}

test("mixed outcomes retain every semantic counter and independent runner failures", () => {
  const derived = statsDerive({
    stages: [{
      name: "code", runs: 6, complete: 2, blocked: 1, protocol_error: 1,
      failed: 1, unknown: 1, runner_failed: 2, token_runs: 2, tokens: 200,
    }],
  });
  const outcomes = "2 complete · 1 blocked · 1 protocol error · 1 failed · 1 unverified · 2 runner failures";
  assert.equal(derived.stages[0].time.meta, `6 runs · ${outcomes}`);
  assert.equal(derived.stages[0].tokens.meta, `2 of 6 runs measured · ${outcomes}`);
});

test("an empty window does not claim a first-pass result", () => {
  const card = statsDerive({}).kpis.find(k => k.label === "first-pass");
  assert.equal(card.value, "—");
  assert.equal(card.delta, "no stage runs");
});

test("26-week heatmap labels identify rendered week columns across a year boundary", () => {
  const start = Date.UTC(2026, 9, 28); // Wednesday, so rendering starts with a partial week.
  const days = Array.from({ length: 182 }, (_, i) => ({
    date: new Date(start + i * 86400000).toISOString().slice(0, 10), runs: 1,
  }));
  const { heat } = statsDerive({ days });
  assert.equal(heat.weeks.length, 27);
  assert.deepEqual(heat.months, [
    { label: "Oct", weekIndex: 0 },
    { label: "Nov", weekIndex: 1 },
    { label: "Dec", weekIndex: 6 },
    { label: "Jan", weekIndex: 10 },
    { label: "Feb", weekIndex: 15 },
    { label: "Mar", weekIndex: 19 },
    { label: "Apr", weekIndex: 23 },
  ]);
});

test("bucket heights retain the same numeric scale for zero, split, and full buckets", () => {
  const buckets = [
    { start: "2026-11-18T06:00", done: 0, cancelled: 0, tokens_in: 0, tokens_out: 0 },
    { start: "2026-11-18T07:00", done: 2, cancelled: 1, tokens_in: 3, tokens_out: 1 },
    { start: "2026-11-18T08:00", done: 4, cancelled: 0, tokens_in: 0, tokens_out: 8 },
  ];
  const { throughput, tokens } = statsDerive({ buckets, window: { unit: "hour" } });
  assert.deepEqual(throughput.map(b => [b.doneH, b.cancelH]), [[0, 0], [43, 21.5], [86, 0]]);
  assert.deepEqual(tokens.map(b => [b.inH, b.outH]), [[0, 0], [16.5, 5.5], [0, 44]]);
});
