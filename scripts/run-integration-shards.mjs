#!/usr/bin/env node
// Exit 0: complete initial pass/skip evidence; 1: failure/incomplete evidence;
// 2: initial failures passed only on serial retry (review required, not clean).
// Requires the same environment as a serial integration run.
// Usage: node scripts/run-integration-shards.mjs --shards 4 [--run pattern]
//        [--timeout 240m] [--out new-directory] [--no-retry] [--dry-run]
import { spawn } from "node:child_process";
import { mkdirSync, createWriteStream, writeFileSync, readFileSync } from "node:fs";
import { finished } from "node:stream/promises";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const root = resolve(import.meta.dirname, "..");
function options(args) {
  const result = { shards: "4", run: "", timeout: "240m", out: join(tmpdir(), "loreviva-shards-" + Date.now()) };
  const seen = new Set();
  for (let i = 0; i < args.length; i++) {
    const key = args[i].slice(2);
    if (!args[i].startsWith("--") || seen.has(key)) throw new Error("invalid or duplicate option: " + args[i]);
    seen.add(key);
    if (["dry-run", "no-retry"].includes(key)) result[key] = true;
    else if (Object.hasOwn(result, key) && args[i + 1] && !args[i + 1].startsWith("--")) result[key] = args[++i];
    else throw new Error("unknown option or missing value: " + args[i]);
  }
  result.shards = Number(result.shards);
  if (!Number.isSafeInteger(result.shards) || result.shards < 1) throw new Error("--shards must be a positive integer");
  if (!/^(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$/.test(result.timeout) || !/[1-9]/.test(result.timeout)) {
    throw new Error("--timeout must be a positive Go duration");
  }
  result.out = resolve(result.out);
  if (result.run) result.filter = new RegExp(result.run);
  return result;
}
async function run(argv, directory) {
  const env = { ...process.env };
  let streams, paths;
  if (directory) {
    // Refuse to overwrite an earlier attempt's evidence or custody file.
    mkdirSync(directory);
    const custody = join(directory, "scan-material.jsonl");
    writeFileSync(custody, "", { flag: "wx" });
    Object.assign(env, { TEMP: directory, TMP: directory, LOREVIVA_TEST_CUSTODY_FILE: custody });
    paths = { stdout: join(directory, "stdout.jsonl"), stderr: join(directory, "stderr.log") };
    streams = Object.values(paths).map(p => createWriteStream(p, { flags: "wx" }));
  }
  let stdout = "", stderr = "", error = null;
  const child = spawn("go", argv, { cwd: root, env, shell: false });
  const writes = streams?.map(stream => finished(stream).catch(e => {
    error = e.message; child.kill();
  }));
  const completion = new Promise(done => {
    child.on("error", e => { error = e.message; });
    child.on("close", (code, signal) => done({ code, signal }));
  });
  if (streams) {
    child.stdout.pipe(streams[0], { end: false });
    child.stderr.pipe(streams[1], { end: false });
  } else {
    child.stdout.on("data", b => { stdout += b; });
    child.stderr.on("data", b => { stderr += b; });
  }
  const status = await completion;
  if (streams) {
    streams.forEach(stream => stream.end());
    await Promise.all(writes);
    // Process close alone does not mean the log streams have finished writing.
    stdout = readFileSync(paths.stdout, "utf8");
  }
  return { ...status, error, stdout, stderr, paths };
}
function assess(result, expected) {
  const issues = [], terminals = new Map(), packages = new Set(), packageEnds = [];
  const expectedSet = new Set(expected), subFailures = new Set();
  for (const line of result.stdout.split(/\r?\n/)) {
    if (!line.trim()) continue;
    let event;
    try { event = JSON.parse(line); } catch { issues.push("malformed JSON output"); continue; }
    if (!event || typeof event !== "object" || typeof event.Action !== "string") {
      issues.push("invalid test event"); continue;
    }
    // Go build events can precede test events and mention dependency packages.
    if (event.Action.startsWith("build-")) {
      if (event.Action === "build-fail") issues.push("package build failed");
      continue;
    }
    if (!event.Package) { issues.push("test event has no package"); continue; }
    packages.add(event.Package);
    if (!event.Test) {
      if (["pass", "fail", "skip"].includes(event.Action)) packageEnds.push(event.Action);
      continue;
    }
    if (typeof event.Test !== "string") { issues.push("invalid test name"); continue; }
    const top = event.Test.split("/")[0];
    if (!expectedSet.has(top)) { issues.push("unplanned test: " + top); continue; }
    if (event.Test.includes("/")) {
      if (event.Action === "fail") subFailures.add(top);
      continue;
    }
    if (!["pass", "fail", "skip"].includes(event.Action)) continue;
    if (terminals.has(top)) issues.push("duplicate terminal: " + top);
    else terminals.set(top, event.Action);
  }
  const missing = expected.filter(n => !terminals.has(n));
  const failed = expected.filter(n => terminals.get(n) === "fail");
  if (missing.length) issues.push("targets without terminal evidence: " + missing.join(", "));
  if (packages.size !== 1) issues.push("expected exactly one test package");
  if (packageEnds.length !== 1) issues.push("expected exactly one package terminal");
  if (result.error) issues.push("process/log error: " + result.error);
  if (result.signal) issues.push("terminated by signal: " + result.signal);
  for (const name of subFailures) if (terminals.get(name) !== "fail") issues.push("child failure contradicts parent: " + name);
  // Retry only a complete ordinary test failure, never infrastructure failure.
  if (failed.length) {
    if (result.code !== 1 || packageEnds[0] !== "fail") issues.push("failure exit/package mismatch");
  } else if (result.code !== 0 || packageEnds[0] !== "pass") issues.push("unsuccessful process or package");
  return { passed: expected.filter(n => terminals.get(n) === "pass"),
    skipped: expected.filter(n => terminals.get(n) === "skip"),
    failed, missing, issues: [...new Set(issues)] };
}
async function main() {
  const opt = options(process.argv.slice(2));
  const listed = await run(["test", "-list", ".*", "./tests/integration"]);
  if (listed.code !== 0 || listed.signal || listed.error) throw new Error("test discovery failed: " + (listed.error || listed.stderr || listed.stdout));
  let names = listed.stdout.split(/\r?\n/).filter(n => /^Test[A-Za-z0-9_]+$/.test(n));
  if (new Set(names).size !== names.length) throw new Error("duplicate discovery target");
  if (opt.filter) names = names.filter(n => opt.filter.test(n));
  if (!names.length) throw new Error("no matching tests");
  const plan = Array.from({ length: Math.min(opt.shards, names.length) }, () => []);
  names.forEach((name, i) => plan[i % plan.length].push(name));
  if (opt["dry-run"]) {
    plan.forEach((shard, i) => console.log("shard " + (i + 1) + ": " + shard.join(", ")));
    return 0;
  }
  mkdirSync(opt.out, { recursive: true });
  const execute = async (targets, label) => {
    const result = await run(["test", "./tests/integration", "-p=1", "-count=1",
      "-timeout", opt.timeout, "-json", "-run", "^(" + targets.join("|") + ")$"], join(opt.out, label));
    return { label, targets, code: result.code, signal: result.signal, error: result.error,
      logs: result.paths, ...assess(result, targets) };
  };
  const attempts = await Promise.all(plan.map((targets, i) => execute(targets, "shard-" + (i + 1))));
  const initialFailed = attempts.flatMap(r => r.failed);
  const summary = { targets: names, initialPassed: attempts.flatMap(r => r.passed), initialFailed,
    skipped: attempts.flatMap(r => r.skipped), missing: attempts.flatMap(r => r.missing),
    failed: [...initialFailed], serialRecovered: [], attempts };
  if (!opt["no-retry"] && attempts.every(r => r.issues.length === 0) && initialFailed.length) {
    // One invocation runs failed targets serially with fresh custody and JSON.
    const retry = await execute(initialFailed, "retry");
    attempts.push(retry);
    if (!retry.issues.length) {
      summary.serialRecovered = retry.passed;
      summary.failed = initialFailed.filter(n => !retry.passed.includes(n));
    }
  }
  const incomplete = attempts.some(r => r.issues.length > 0);
  summary.exitCode = incomplete || summary.failed.length ? 1 : summary.serialRecovered.length ? 2 : 0;
  writeFileSync(join(opt.out, "summary.json"), JSON.stringify(summary, null, 2) + "\n", { flag: "wx" });
  console.log(names.length + " targets: " + summary.initialPassed.length + " initial pass, "
    + summary.failed.length + " unresolved fail, " + summary.skipped.length + " initial skip, "
    + summary.missing.length + " missing, " + summary.serialRecovered.length + " serial recovery");
  for (const attempt of attempts) {
    console.log(attempt.label + ": exit " + attempt.code + (attempt.signal ? " signal " + attempt.signal : ""));
    for (const issue of attempt.issues) console.error(attempt.label + ": " + issue);
  }
  if (summary.serialRecovered.length) console.log("passed only on serial retry (review required): " + summary.serialRecovered.join(", "));
  console.log("evidence: " + opt.out);
  return summary.exitCode;
}
try { process.exitCode = await main(); }
catch (error) { console.error(error.message); process.exitCode = 1; }
