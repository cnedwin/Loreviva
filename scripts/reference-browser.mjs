import assert from 'node:assert/strict';
import {dirname,resolve,join} from 'node:path';
import {createRequire} from 'node:module';
import {spawn} from 'node:child_process';

// Only the owning Go fixture supplies this bounded, RAM-only local input.
let raw=Buffer.alloc(0);for await(const part of process.stdin){raw=Buffer.concat([raw,part]);assert.ok(raw.length<=16384)}
const input=JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(raw));
assert.deepEqual(Object.keys(input).sort(),['credential','fresh','login','origin','player','turn','world']);
assert.equal(input.origin,'http://127.0.0.1:27185');
for(const name of ['login','credential','player','turn','world'])assert.equal(typeof input[name],'string');
assert.deepEqual(Object.keys(input.fresh).sort(),['actor','login','memory','player','release','world']);
for(const value of Object.values(input.fresh))assert.equal(typeof value,'string');
const custody=process.env.LOREVIVA_TEST_CUSTODY_FILE;
assert.ok(custody&&dirname(resolve(custody))===resolve(process.env.TMP??''));
// The installed runner receives only this local synthetic login in process RAM.
// No platform Service key, database credential, persistent config or trace.
const require=createRequire(import.meta.url),cli=require.resolve('@playwright/test/cli');
const output=join(process.env.TMP,'reference-browser-output');
const env={...process.env,LOREVIVA_REFERENCE_BROWSER_INPUT:JSON.stringify(input),
 PLAYWRIGHT_LAST_RUN_OUTPUT_FILE:join(process.env.TMP,'reference-browser-last-run.json')};
const child=spawn(process.execPath,[cli,'test','--project=player','--grep=reference-player-real-loopback$','--workers=1','--reporter=line',`--output=${output}`],
 {stdio:['ignore','pipe','pipe'],env,windowsHide:true});
delete env.LOREVIVA_REFERENCE_BROWSER_INPUT;
child.stdout.pipe(process.stdout);child.stderr.pipe(process.stderr);
const timer=setTimeout(()=>child.kill(),185000);
try{
 const exit=await new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',resolve)});
 process.exitCode=exit===0?0:1;
}finally{clearTimeout(timer)}
