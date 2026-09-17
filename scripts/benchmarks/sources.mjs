#!/usr/bin/env bun
import { join } from "node:path";
import { run, sourceInfo, summary } from "./common.mjs";

const [kind, baseline, candidate] = process.argv.slice(2);
if (!["transfer", "archive"].includes(kind) || !baseline || !candidate || process.argv.length !== 5) {
  throw new Error("Usage: bun scripts/benchmarks/sources.mjs <transfer|archive> <baseline-checkout> <candidate-checkout>");
}
if (kind === "archive" && process.platform !== "linux") throw new Error("Archive writes require Linux descriptor paths; use the documented isolated container.");
const sources = { baseline: await sourceInfo(baseline), candidate: await sourceInfo(candidate) };
const samples = [];
const scenarios = kind === "archive" ? ["large", "small"] : ["full-speed", "slow-consumer"];
for (const scenario of scenarios) {
  // Alternate independent processes. Discard the first pair to warm host caches.
  for (let trial = 0; trial < 6; trial++) {
    for (const label of trial % 2 ? ["candidate", "baseline"] : ["baseline", "candidate"]) {
      const output = await run([process.execPath, join(import.meta.dir, kind + "-worker.mjs"), sources[label].source, scenario]);
      const sample = JSON.parse(output);
      if (sample.bun !== Bun.version) throw new Error("Both versions must run on the same Bun executable");
      if (trial) samples.push({ label, scenario, ...sample });
    }
  }
}
console.log(JSON.stringify({ benchmark: kind, runtime: { bun: Bun.version, os: process.platform, arch: process.arch },
  method: "Five independent measured processes per variant/scenario after a discarded pair; alternating order; process launch and module imports excluded from timing.",
  sources, summary: Object.fromEntries(scenarios.map((scenario) => [scenario, Object.fromEntries(Object.keys(sources).map((label) => {
    const selected = samples.filter((sample) => sample.label === label && sample.scenario === scenario);
    return [label, { elapsedMs: summary(selected), ...(kind === "transfer" ? { peakRssMiB: summary(selected, "peakRssMiB") } : {}) }];
  }))])), samples }, null, 2));
