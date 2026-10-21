import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import * as checker from './check-byte-policy.mjs';
const sha=b=>createHash('sha256').update(b).digest('hex');
async function fixture(t){const root=await fs.mkdtemp(path.join(os.tmpdir(),'guard-scope-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));return root;}
const encode=v=>Buffer.from(JSON.stringify(v,null,2)+'\n');
async function put(root,name,body){const p=path.join(root,name);await fs.mkdir(path.dirname(p),{recursive:true});await fs.writeFile(p,body);}
async function scope(root,names){const files=[];for(const name of names){const b=await fs.readFile(path.join(root,name));files.push({path:name,bytes:b.length,sha256:sha(b)});}const p=path.join(root,'scope.json');await fs.writeFile(p,encode({format:'loreviva.source-byte-scope.v1',files}));return p;}
test('explicit source scope includes legitimate untracked source without Git fallback',async t=>{
 const root=await fixture(t);await put(root,'new.ts','export const version=1;\n');const p=await scope(root,['new.ts']);
 assert.equal(typeof checker.checkSourceBytes,'function','explicit source API required');
 const rows=await checker.checkSourceBytes(root,p,{partial:true});assert.deepEqual(rows.map(x=>x.path),['new.ts']);
});
test('source scope rejects closed-shape order duplicate missing and stale bytes',async t=>{
 const root=await fixture(t);await put(root,'a.ts','a\n');await put(root,'b.ts','b\n');const p=await scope(root,['a.ts','b.ts']);const original=JSON.parse(await fs.readFile(p));
 for(const bad of [{...original,approval:true},{...original,files:[...original.files].reverse()},{...original,files:[original.files[0],original.files[0]]},{...original,files:[{...original.files[0],path:'../a.ts'}]},{...original,files:[{...original.files[0],sha256:'0'.repeat(64)}]}]){await fs.writeFile(p,encode(bad));await assert.rejects(checker.checkSourceBytes(root,p,{partial:true}));}
 await fs.writeFile(p,encode(original));await put(root,'a.ts','changed\n');await assert.rejects(checker.checkSourceBytes(root,p,{partial:true}),/stale|fingerprint/i);
 await fs.rm(path.join(root,'b.ts'));await assert.rejects(checker.checkSourceBytes(root,p,{partial:true}));
});
test('source scope guards root ancestor and leaf aliases before body reads',async t=>{
 const root=await fixture(t),outside=await fixture(t);await put(outside,'bad.ts','invalid\r\n');await fs.symlink(outside,path.join(root,'alias'),process.platform==='win32'?'junction':'dir');
 const p=path.join(root,'scope.json');await fs.writeFile(p,encode({format:'loreviva.source-byte-scope.v1',files:[{path:'alias/bad.ts',bytes:9,sha256:'0'.repeat(64)}]}));
 await assert.rejects(checker.checkSourceBytes(root,p,{partial:true}),/Alias/);await assert.rejects(checker.checkSourceBytes(path.join(root,'alias'),p,{partial:true}),/Alias/);
 await fs.rm(p);await fs.symlink(outside,p,process.platform==='win32'?'junction':'dir');await assert.rejects(checker.checkSourceBytes(root,p,{partial:true}),/Alias/);
});
test('missing scope never falls back and carries no approval fields',async t=>{
 const root=await fixture(t);await put(root,'a.ts','a\n');await assert.rejects(checker.checkSourceBytes(root,path.join(root,'missing.json')));
 const p=await scope(root,['a.ts']);const rows=await checker.checkSourceBytes(root,p,{partial:true});assert.equal('approved' in rows[0],false);
});
const invName='scripts/source-byte-inventory.json';
const revisionName='.cache/source-byte-scope.json';
async function lifecycle(t){
 const root=await fixture(t);await put(root,'scripts/untracked.ts','export const value=1;\n');
 await put(root,invName,encode({format:'loreviva.source-byte-inventory.v1',paths:[invName,'scripts/untracked.ts']}));
 const {refreshSourceScope}=await import('./refresh-source-byte-scope.mjs');return {root,refresh:options=>refreshSourceScope(root,options)};
}
test('scope lifecycle initializes untracked bytes and explicit refresh follows independent stale rejection',async t=>{
 const {root,refresh}=await lifecycle(t);const initial=await refresh({init:true});assert.equal(initial.approved,false);
 const old=await fs.readFile(path.join(root,initial.revision));assert.equal((await checker.checkSourceBytes(root,path.join(root,revisionName))).length,2);
 await put(root,'scripts/untracked.ts','export const value=2;\n');await assert.rejects(checker.checkSourceBytes(root,path.join(root,revisionName)),/stale/);
 const next=await refresh();assert.notEqual(next.sha256,initial.sha256);assert.deepEqual(await fs.readFile(path.join(root,initial.revision)),old);
 assert.equal((await refresh()).unchanged,true);await assert.rejects(refresh({init:true}),/Fresh/);
 await checker.checkSourceBytes(root,path.join(root,revisionName));
});
test('scope lifecycle requires exact prior delta for added and physically removed files including embed names',async t=>{
 const {root,refresh}=await lifecycle(t);const first=await refresh({init:true});
 const oldInv=await fs.readFile(path.join(root,invName));await put(root,'internal/app/studio_dist/assets/new.js','export{};\n');
 await assert.rejects(refresh(),/Undeclared/);
 const delta={format:'loreviva.source-byte-delta.v1',previous_scope_sha256:first.sha256,previous_inventory_sha256:sha(oldInv),added:['internal/app/studio_dist/assets/new.js'],removed:['scripts/untracked.ts']};
 const d=path.join(root,'.cache/delta.json');await fs.writeFile(d,encode(delta));await assert.rejects(refresh({deltaFile:d}),/physically absent/);
 await fs.rm(path.join(root,'scripts/untracked.ts'));const next=await refresh({deltaFile:d});
 const scope=JSON.parse(await fs.readFile(path.join(root,revisionName))),actualInv=await fs.readFile(path.join(root,invName));
 assert.equal(scope.files.find(x=>x.path===invName).sha256,sha(actualInv));assert.notEqual(next.sha256,first.sha256);
 await checker.checkSourceBytes(root,path.join(root,revisionName));await assert.rejects(refresh({deltaFile:d}),/prior identity/);
});
test('scope lifecycle rejects CR aliases arbitrary shortening and invalid input without writing cache',async t=>{
 const {root,refresh}=await lifecycle(t);await put(root,'scripts/untracked.ts','bad\r\n');await assert.rejects(refresh({init:true}),/LF/);await assert.rejects(fs.access(path.join(root,'.cache')),{code:'ENOENT'});
 await put(root,'scripts/untracked.ts','good\n');await refresh({init:true});const before=await fs.readFile(path.join(root,revisionName));
 const inv=JSON.parse(await fs.readFile(path.join(root,invName)));inv.paths=inv.paths.filter(x=>!x.endsWith('untracked.ts'));await put(root,invName,encode(inv));
 await assert.rejects(refresh(),/outside explicit delta/);assert.deepEqual(await fs.readFile(path.join(root,revisionName)),before);
});
test('scope lifecycle preserves prepared immutable bytes and recovers only the same interrupted update',async t=>{
 const {root,refresh}=await lifecycle(t);await refresh({init:true});await put(root,'scripts/untracked.ts','changed\n');
 const {syncBuiltinESMExports}=await import('node:module');const original=fs.rename;let calls=0;
 fs.rename=async(...args)=>{calls++;throw Error('controlled rename failure');};syncBuiltinESMExports();
 try{await assert.rejects(refresh(),/controlled rename failure/);}finally{fs.rename=original;syncBuiltinESMExports();}
 assert.equal(calls,1);await assert.rejects(checker.checkSourceBytes(root,path.join(root,revisionName)),/stale/);
 const pending=await fs.readFile(path.join(root,'.cache/source-byte-pending.json'));await put(root,'scripts/untracked.ts','another\n');await assert.rejects(refresh(),/Pending source changed/);
 assert.deepEqual(await fs.readFile(path.join(root,'.cache/source-byte-pending.json')),pending);await put(root,'scripts/untracked.ts','changed\n');await refresh();await checker.checkSourceBytes(root,path.join(root,revisionName));
 await assert.rejects(fs.access(path.join(root,'.cache/source-byte-pending.json')),{code:'ENOENT'});
});
test('development package requires explicit refresh while public manifest mode needs no private cache',async t=>{
 const pkg=JSON.parse(await fs.readFile(new URL('../package.json',import.meta.url)));
 assert.equal(pkg.scripts['scope:init'],'node scripts/refresh-source-byte-scope.mjs --init');
 assert.equal(pkg.scripts['scope:refresh'],'node scripts/refresh-source-byte-scope.mjs');
 assert.equal(pkg.scripts['check:bytes'],'node scripts/check-byte-policy.mjs --source-scope .cache/source-byte-scope.json');
 assert.equal(pkg.scripts.test,'pnpm check:bytes && pnpm test:contracts && pnpm test:sdk');
 const root=await fixture(t);await put(root,'a.ts','export {};\n');await put(root,'PUBLICATION.json',encode({files:{'a.ts':{}}}));
 assert.equal((await checker.checkBytes(root)).length,2);await assert.rejects(fs.access(path.join(root,'.cache')),{code:'ENOENT'});
});

async function rejectsWithoutWrites(action,pattern){
 const {syncBuiltinESMExports}=await import('node:module');const originals={},calls=[];
 for(const key of ['mkdir','writeFile','rename','unlink']){originals[key]=fs[key];fs[key]=async(...args)=>{calls.push(key);return originals[key](...args);};}syncBuiltinESMExports();
 try{await assert.rejects(action,pattern);assert.deepEqual(calls,[],'invalid group must perform zero writes');}
 finally{for(const [key,value] of Object.entries(originals))fs[key]=value;syncBuiltinESMExports();}
}
test('top-level additions require explicit inventory delta before check or refresh',async t=>{
 const {root,refresh}=await lifecycle(t);const first=await refresh({init:true});const oldInv=await fs.readFile(path.join(root,invName));
 await put(root,'new-root.ts','export const extra=1;\n');
 await assert.rejects(checker.checkSourceBytes(root,path.join(root,revisionName)),/Uninventoried/);
 await rejectsWithoutWrites(()=>refresh(),/Undeclared/);
 const delta={format:'loreviva.source-byte-delta.v1',previous_scope_sha256:first.sha256,previous_inventory_sha256:sha(oldInv),added:['new-root.ts'],removed:[]};
 const d=path.join(root,'.cache/delta.json');await put(root,'.cache/delta.json',encode(delta));
 await refresh({deltaFile:d});await checker.checkSourceBytes(root,path.join(root,revisionName));
 await put(root,'phase32-new-unapproved.txt','not a historical exclusion\n');await rejectsWithoutWrites(()=>refresh(),/Undeclared|Historical/);
 await fs.rm(path.join(root,'phase32-new-unapproved.txt'));await fs.mkdir(path.join(root,'new-unknown-domain'));
 await rejectsWithoutWrites(()=>refresh(),/Undeclared.*directory|Unknown.*directory/);
});
async function interrupted(t){
 const state=await lifecycle(t);const initial=await state.refresh({init:true});await put(state.root,'scripts/untracked.ts','prepared\n');
 const {syncBuiltinESMExports}=await import('node:module');const original=fs.rename;
 fs.rename=async()=>{throw Error('controlled interrupted rename');};syncBuiltinESMExports();
 try{await assert.rejects(state.refresh(),/controlled interrupted rename/);}finally{fs.rename=original;syncBuiltinESMExports();}
 const pendingPath=path.join(state.root,'.cache/source-byte-pending.json');const pending=JSON.parse(await fs.readFile(pendingPath));
 return {...state,initial,pendingPath,pending};
}
async function resamplePending(root,pending,names){
 const inventory=encode({format:'loreviva.source-byte-inventory.v1',paths:[...names].sort(checker.compareNames)});const files=[];
 for(const name of JSON.parse(inventory).paths){const body=name===invName?inventory:await fs.readFile(path.join(root,name));files.push({path:name,bytes:body.length,sha256:sha(body)});}
 pending.new_inventory=inventory.toString('utf8');pending.new_scope=encode({format:'loreviva.source-byte-scope.v1',files}).toString('utf8');
}
test('pending recovery rejects rewritten inventory source and prior immutable evidence without writes',async t=>{
 for(const mode of ['add','shrink','resample','prior-missing','prior-drift','inventory-drift','prepared-missing'])await t.test(mode,async t=>{
  const {root,refresh,initial,pendingPath,pending}=await interrupted(t);
  if(mode==='add'){await put(root,'new-root.ts','injected\n');await resamplePending(root,pending,[invName,'scripts/untracked.ts','new-root.ts']);}
  if(mode==='shrink'){await fs.rm(path.join(root,'scripts/untracked.ts'));await resamplePending(root,pending,[invName]);}
  if(mode==='resample'){await put(root,'scripts/untracked.ts','resampled\n');await resamplePending(root,pending,[invName,'scripts/untracked.ts']);}
  if(mode==='prior-missing')await fs.rm(path.join(root,initial.revision));
  if(mode==='prior-drift')await fs.writeFile(path.join(root,initial.revision),'{}\n');
  if(mode==='inventory-drift')await fs.writeFile(path.join(root,'.cache/source-byte-scopes/inventories',sha(Buffer.from(pending.old_inventory))+'.json'),'{}\n');
  if(mode==='prepared-missing')await fs.rm(path.join(root,'.cache/source-byte-scopes/transitions',sha(encode(pending))+'.json'));
  await fs.writeFile(pendingPath,encode(pending));const oldLocator=await fs.readFile(path.join(root,revisionName)),oldInventory=await fs.readFile(path.join(root,invName));
  await rejectsWithoutWrites(()=>refresh(),/prepared|revision|inventory|immutable|ENOENT|Pending|spelling/i);
  assert.deepEqual(await fs.readFile(path.join(root,revisionName)),oldLocator);assert.deepEqual(await fs.readFile(path.join(root,invName)),oldInventory);
 });
});
test('project scope rejects omission of its inventory while partial library data remains explicit',async t=>{
 const {root,refresh}=await lifecycle(t);await refresh({init:true});const p=path.join(root,revisionName),original=JSON.parse(await fs.readFile(p));
 await fs.writeFile(p,encode({...original,files:original.files.filter(x=>x.path!==invName)}));
 await assert.rejects(checker.checkSourceBytes(root,p),/inventory required/i);
 const partial=await checker.checkSourceBytes(root,p,{partial:true});assert.equal(partial.length,1);
 const {spawnSync}=await import('node:child_process');const script=fileURLToPath(new URL('./check-byte-policy.mjs',import.meta.url));
 const cli=spawnSync(process.execPath,[script,'--source-scope',p],{cwd:root,encoding:'utf8'});
 assert.notEqual(cli.status,0);assert.match(cli.stderr,/inventory required/i);
});
test('recovery derives inventory from prior delta even when an untrusted prepared archive is supplied',async t=>{
 for(const mode of ['add','shrink','resample'])await t.test(mode,async t=>{
  const {root,refresh,pendingPath,pending}=await interrupted(t);
  if(mode==='add'){await put(root,'new-root.ts','injected\n');await resamplePending(root,pending,[invName,'scripts/untracked.ts','new-root.ts']);}
  else if(mode==='shrink'){await fs.rm(path.join(root,'scripts/untracked.ts'));await resamplePending(root,pending,[invName]);}
  else{await put(root,'scripts/untracked.ts','resampled with matching archive\n');await resamplePending(root,pending,[invName,'scripts/untracked.ts']);}
  const body=encode(pending);await put(root,'.cache/source-byte-scopes/transitions/'+sha(body)+'.json',body);await fs.writeFile(pendingPath,body);
  await rejectsWithoutWrites(()=>refresh(),/not derived from prior delta|anchor|prepared/i);
 });
});
test('recovery completes the same explicit delta after inventory advances but locator rename fails',async t=>{
 const {root,refresh}=await lifecycle(t);const first=await refresh({init:true}),oldInv=await fs.readFile(path.join(root,invName));
 await put(root,'new-root.ts','new\n');await fs.rm(path.join(root,'scripts/untracked.ts'));
 const d=path.join(root,'.cache/delta.json');await put(root,'.cache/delta.json',encode({format:'loreviva.source-byte-delta.v1',previous_scope_sha256:first.sha256,previous_inventory_sha256:sha(oldInv),added:['new-root.ts'],removed:['scripts/untracked.ts']}));
 const {syncBuiltinESMExports}=await import('node:module');const original=fs.rename;let calls=0;
 fs.rename=async(...args)=>{if(++calls===2)throw Error('controlled locator failure');return original(...args);};syncBuiltinESMExports();
 try{await assert.rejects(refresh({deltaFile:d}),/controlled locator failure/);}finally{fs.rename=original;syncBuiltinESMExports();}
 assert.equal(calls,2);await assert.rejects(checker.checkSourceBytes(root,path.join(root,revisionName)),/stale|missing/);
 const pending=await fs.readFile(path.join(root,'.cache/source-byte-pending.json'));const prepared=JSON.parse(pending).new_scope;
 await refresh({deltaFile:d});assert.equal(await fs.readFile(path.join(root,revisionName),'utf8'),prepared);await checker.checkSourceBytes(root,path.join(root,revisionName));
});
test('active transition recovers creation before archive or pending without reseeding',async t=>{
 const {root,refresh}=await lifecycle(t);
 const active=path.join(root,'.cache/source-byte-active-transition.json');
 const pending=path.join(root,'.cache/source-byte-pending.json');
 const {syncBuiltinESMExports}=await import('node:module');const original=fs.writeFile;let failures=0;
 fs.writeFile=async(...args)=>{if(String(args[0]).replaceAll('\\','/').includes('/source-byte-scopes/transitions/')){failures++;throw Error('controlled archive creation interruption');}return original(...args);};syncBuiltinESMExports();
 try{await assert.rejects(refresh({init:true}),/controlled archive creation interruption/);}finally{fs.writeFile=original;syncBuiltinESMExports();}
 assert.equal(failures,1);const fixed=await fs.readFile(active),transaction=JSON.parse(fixed);assert.equal(transaction.old_scope,null);
 await assert.rejects(fs.access(pending),{code:'ENOENT'});await assert.rejects(fs.access(path.join(root,revisionName)),{code:'ENOENT'});
 await rejectsWithoutWrites(()=>refresh({init:true}),/Active update/);
 await put(root,'scripts/untracked.ts','different source after interruption\n');await rejectsWithoutWrites(()=>refresh(),/Pending source changed/);
 assert.deepEqual(await fs.readFile(active),fixed);
 await put(root,'scripts/untracked.ts','export const value=1;\n');await refresh();
 assert.equal(await fs.readFile(path.join(root,revisionName),'utf8'),transaction.new_scope);await checker.checkSourceBytes(root,path.join(root,revisionName));
 assert.deepEqual(await fs.readFile(path.join(root,'.cache/source-byte-scopes/transitions',sha(fixed)+'.json')),fixed);
 await assert.rejects(fs.access(active),{code:'ENOENT'});await assert.rejects(fs.access(pending),{code:'ENOENT'});
});
test('active transition recovers cleanup after pending removal only with fixed prepared bytes',async t=>{
 const {root,refresh}=await lifecycle(t);await refresh({init:true});await put(root,'scripts/untracked.ts','prepared cleanup\n');
 const active=path.join(root,'.cache/source-byte-active-transition.json'),pending=path.join(root,'.cache/source-byte-pending.json');
 const {syncBuiltinESMExports}=await import('node:module');const original=fs.unlink;let failures=0;
 fs.unlink=async(...args)=>{if(path.resolve(args[0])===active){failures++;throw Error('controlled anchor cleanup interruption');}return original(...args);};syncBuiltinESMExports();
 try{await assert.rejects(refresh(),/controlled anchor cleanup interruption/);}finally{fs.unlink=original;syncBuiltinESMExports();}
 assert.equal(failures,1);const fixed=await fs.readFile(active),transaction=JSON.parse(fixed);
 await assert.rejects(fs.access(pending),{code:'ENOENT'});assert.equal(await fs.readFile(path.join(root,revisionName),'utf8'),transaction.new_scope);
 await rejectsWithoutWrites(()=>refresh({init:true}),/Active update/);
 await put(root,'scripts/untracked.ts','resampled cleanup\n');await rejectsWithoutWrites(()=>refresh(),/Pending source changed/);
 assert.deepEqual(await fs.readFile(active),fixed);await put(root,'scripts/untracked.ts','prepared cleanup\n');
 await refresh();await checker.checkSourceBytes(root,path.join(root,revisionName));
 assert.equal(await fs.readFile(path.join(root,revisionName),'utf8'),transaction.new_scope);
 assert.deepEqual(await fs.readFile(path.join(root,'.cache/source-byte-scopes/transitions',sha(fixed)+'.json')),fixed);
 await assert.rejects(fs.access(active),{code:'ENOENT'});await assert.rejects(fs.access(pending),{code:'ENOENT'});
});
