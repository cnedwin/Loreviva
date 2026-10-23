import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { spawnSync } from "node:child_process";

// Exercise the real CLI and child lifecycle. Only the Go executable is replaced;
// this suite uses no database, application fixture, or private material.
function fixture(scenario, args = []) {
  const dir = mkdtempSync(join(tmpdir(), "loreviva-shard-test-"));
  const fake = join(dir, "go.cjs"), preload = join(dir, "preload.mjs");
  writeFileSync(preload, [
    "import cp from 'node:child_process';",
    "import {syncBuiltinESMExports} from 'node:module';",
    "const original = cp.spawn;",
    "const scenario=" + JSON.stringify(scenario) + ";",
    "if(scenario==='spawn-error') cp.spawn=(cmd,args,options)=>original(args.includes('-list')?process.execPath:'loreviva-nonexistent-executable', [" + JSON.stringify(fake) + ",...args],options);",
    "else",
    "cp.spawn = (cmd,args,options) => original(cmd === 'go' ? process.execPath : cmd,",
    "cmd === 'go' ? [" + JSON.stringify(fake) + ",...args] : args, options);",
    "syncBuiltinESMExports();"
  ].join("\n"));
  writeFileSync(fake, [
    "const {appendFileSync} = require('node:fs');",
    "const args=process.argv.slice(2), scenario=" + JSON.stringify(scenario) + ";",
    "if(args.includes('-list')) { console.log('TestAlpha\\nTestBeta'); process.exit(scenario==='discovery-fail'?1:0); }",
    "const pattern=args[args.indexOf('-run')+1], retry=(process.env.TEMP||'').includes('retry');",
    "appendFileSync(" + JSON.stringify(join(dir, "calls.jsonl")) + ",JSON.stringify({pattern,retry,temp:process.env.TEMP,custody:process.env.LOREVIVA_TEST_CUSTODY_FILE})+'\\n');",
    "const names=['TestAlpha','TestBeta'].filter(n=>new RegExp(pattern).test(n));",
    "const event=(Action,Test,Package='example/integration')=>console.log(JSON.stringify({Action,Package,...(Test?{Test}:{})}));",
    "if(scenario==='compile'||(scenario==='retry-compile'&&retry)) {console.error('synthetic compile failure');process.exit(1);}",
    "if(scenario==='signal') process.kill(process.pid);",
    "if(scenario==='truncated') {console.log('{\"Action\":');process.exit(0);}",
    "const initialFailure=scenario.startsWith('retry-')&&!retry&&names.includes('TestAlpha');",
    "for(const name of names) {",
    "if((scenario==='missing'||(scenario==='retry-missing'&&retry))&&name==='TestAlpha') continue;",
    "if(initialFailure&&name==='TestAlpha') {event('fail',name+'/child');event('fail',name);}",
    "else event(scenario==='skip'||(scenario==='retry-skip'&&retry)?'skip':'pass',name);",
    "}",
    "if(scenario==='duplicate') event('pass',names[0]);",
    "if(scenario==='unexpected') event('pass','TestUnplanned');",
    "if(scenario==='foreign') event('pass','TestAlpha','unrelated/package');",
    "if(scenario!=='no-package') event(initialFailure||scenario==='package-fail'?'fail':'pass');",
    "process.exit(initialFailure||scenario==='package-fail'||(scenario==='retry-exit'&&retry)?1:0);"
  ].join("\n"));
  const result = spawnSync(process.execPath, ["--import", pathToFileURL(preload).href,
    resolve(import.meta.dirname, "run-integration-shards.mjs"), "--out", join(dir, "out"),
    ...(args.includes("--shards") ? [] : ["--shards", "2"]), ...args], { encoding: "utf8", timeout: 20000 });
  const calls = existsSync(join(dir, "calls.jsonl"))
    ? readFileSync(join(dir, "calls.jsonl"), "utf8").trim().split("\n").map(JSON.parse) : [];
  const summaryPath = join(dir, "out", "summary.json");
  const summary = existsSync(summaryPath) ? JSON.parse(readFileSync(summaryPath, "utf8")) : undefined;
  rmSync(dir, { recursive: true, force: true });
  assert.ifError(result.error);
  return { ...result, calls, summary };
}

test("complete passes succeed with evidence", () => {
  const r = fixture("pass"); assert.equal(r.status, 0, r.stderr);
  assert.equal(r.summary?.initialPassed.length, 2);
  assert.equal(r.summary?.attempts.length, 2);
});
for (const scenario of ["compile", "missing", "no-package", "truncated", "signal", "spawn-error", "package-fail", "duplicate", "unexpected", "foreign", "discovery-fail"]) {
  test(scenario + " cannot produce a successful gate", () => {
    const r = fixture(scenario); assert.notEqual(r.status, 0, r.stdout);
    assert.equal(r.calls.filter(c => c.retry).length, 0);
  });
}
for (const scenario of ["retry-compile", "retry-missing", "retry-skip", "retry-exit"]) {
  test(scenario + " does not erase the original failure", () => {
    const r = fixture(scenario); assert.equal(r.status, 1, r.stdout);
    assert.deepEqual(r.summary?.failed, ["TestAlpha"]);
    assert.equal(r.calls.filter(c => c.retry).length, 1);
  });
}
test("proven serial recovery remains a distinct non-clean exit", () => {
  const r = fixture("retry-pass"); assert.equal(r.status, 2, r.stdout);
  assert.deepEqual(r.summary.serialRecovered, ["TestAlpha"]);
  assert.deepEqual(r.summary.initialFailed, ["TestAlpha"]);
  const retry = r.calls.filter(c => c.retry);
  assert.equal(retry.length, 1); assert.equal(retry[0].pattern, "^(TestAlpha)$");
  assert.ok(retry[0].custody.includes("retry"));
});
test("no-retry preserves a named failure", () => {
  const r = fixture("retry-pass", ["--no-retry"]); assert.equal(r.status, 1);
  assert.equal(r.calls.length, 2);
});
test("skipped targets remain explicit", () => {
  const r = fixture("skip"); assert.equal(r.status, 0); assert.equal(r.summary.skipped.length, 2);
});
for (const shards of ["0", "-1", "NaN", "Infinity", "1.5"]) {
  test("reject invalid shard count " + shards, () => {
    const r = fixture("pass", ["--shards", shards]); assert.equal(r.status, 1);
    assert.equal(r.calls.length, 0);
  });
}
test("more shards than targets never starts empty shards", () => {
  const r = fixture("pass", ["--shards", "5"]); assert.equal(r.status, 0);
  assert.equal(r.calls.length, 2);
});
