#!/usr/bin/env node
// Runs the integration package as N concurrent shards.
//
// Every integration test creates its own randomly named MySQL database on the
// approved loopback server and drops it afterwards (tests/integration/dbfixture),
// so shards do not share state: they only contend for server capacity. Sharding
// turns a three-hour serial gate into a roughly N-way shorter one without
// weakening any assertion, because each test still runs exactly as written.
//
// Usage:
//   node scripts/run-integration-shards.mjs --shards 4 [--out dir] [--dry-run]
//   node scripts/run-integration-shards.mjs --shards 4 --run 'TestTurn|TestRecovery'
//
// Requires the same environment a serial run needs: LOREVIVA_TEST_DSN, and for
// the customer/CLI targets LOREVIVA_TEST_GO_EXE.
import { spawn } from "node:child_process";
import { mkdirSync, createWriteStream, openSync, closeSync, writeSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const args = process.argv.slice(2);
const option = (name, fallback) => {
  const at = args.indexOf("--" + name);
  return at === -1 ? fallback : args[at + 1];
};
const dryRun = args.includes("--dry-run");
const shards = Math.max(1, Number(option("shards", "4")));
const filter = option("run", "");
const timeout = option("timeout", "240m");
const outDir = resolve(option("out", join(tmpdir(), "loreviva-shards-" + Date.now())));
const root = resolve(import.meta.dirname, "..");

function run(cmd, argv, extraEnv = {}) {
  return new Promise((done) => {
    const child = spawn(cmd, argv, { cwd: root, env: { ...process.env, ...extraEnv }, shell: false });
    let stdout = "", stderr = "";
    child.stdout.on("data", (b) => (stdout += b));
    child.stderr.on("data", (b) => (stderr += b));
    child.on("close", (code) => done({ code, stdout, stderr }));
  });
}

const listed = await run("go", ["test", "-list", ".*", "./tests/integration"]);
if (listed.code !== 0) {
  console.error(listed.stderr || listed.stdout);
  process.exit(listed.code ?? 1);
}
let names = listed.stdout.split(/\r?\n/).filter((l) => /^Test[A-Za-z0-9_]+$/.test(l));
if (filter) {
  const re = new RegExp(filter);
  names = names.filter((n) => re.test(n));
}
if (names.length === 0) {
  console.error("no matching tests");
  process.exit(1);
}
// Heaviest first, so a slow shard starts early; rest round-robin by name.
const plan = Array.from({ length: shards }, () => []);
names.forEach((name, i) => plan[i % shards].push(name));

if (dryRun) {
  plan.forEach((shard, i) => console.log(`shard ${i + 1}: ${shard.length} tests  ${shard.slice(0, 3).join(", ")}...`));
  console.log(`total ${names.length} tests in ${shards} shards`);
  process.exit(0);
}

mkdirSync(outDir, { recursive: true });
const expression = (shard) => "^(" + shard.join("|") + ")$";
const started = Date.now();
console.log(`running ${names.length} tests in ${shards} shards; logs in ${outDir}`);

const results = await Promise.all(
  plan.map(async (shard, index) => {
    const shardRoot = join(outDir, "shard-" + (index + 1));
    mkdirSync(shardRoot, { recursive: true });
    const custody = join(shardRoot, "scan-material.jsonl");
    closeSync(openSync(custody, "w"));
    const logPath = join(shardRoot, "run.jsonl");
    const log = createWriteStream(logPath);
    const child = spawn(
      "go",
      ["test", "./tests/integration", "-p=1", "-count=1", "-timeout", timeout, "-json", "-run", expression(shard)],
      { cwd: root, env: { ...process.env, TEMP: shardRoot, TMP: shardRoot, LOREVIVA_TEST_CUSTODY_FILE: custody } },
    );
    child.stdout.pipe(log);
    child.stderr.pipe(log);
    const code = await new Promise((done) => child.on("close", done));
    log.end();
    return { index: index + 1, code, logPath, count: shard.length };
  }),
);

let passed = 0, failed = 0, skipped = 0;
const failures = [];
for (const shard of results) {
  const text = await import("node:fs").then((fs) => fs.readFileSync(shard.logPath, "utf8"));
  for (const line of text.split(/\r?\n/)) {
    if (!line.startsWith("{")) continue;
    let event;
    try { event = JSON.parse(line); } catch { continue; }
    const test = event.Test || "";
    if (!test || test.includes("/") || !["pass", "fail", "skip"].includes(event.Action)) continue;
    if (event.Action === "pass") passed++;
    else if (event.Action === "skip") skipped++;
    else { failed++; failures.push(test); }
  }
  console.log(`shard ${shard.index}: ${shard.count} tests, exit ${shard.code}`);
}

const minutes = ((Date.now() - started) / 60000).toFixed(1);
console.log(`\n${names.length} targets: ${passed} pass, ${failed} fail, ${skipped} skip in ${minutes} min`);
if (failures.length) {
  console.log("failed: " + failures.join(", "));
  process.exit(1);
}
