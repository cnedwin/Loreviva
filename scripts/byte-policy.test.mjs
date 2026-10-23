import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,writeFile,mkdir,rm,readFile,symlink} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {verifyBytes,checkBytes} from './check-byte-policy.mjs';
import * as byteGate from './check-byte-policy.mjs';
import {refreshSourceScope} from './refresh-source-byte-scope.mjs';

test('source scope includes upstream licenses without rewriting their bytes',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-license-scope-'));
 try{
  await mkdir(path.join(root,'LICENSES','example'),{recursive:true});
  await mkdir(path.join(root,'scripts'));
  const license=Buffer.from('Upstream license\r\n');
  await writeFile(path.join(root,'LICENSES','example','LICENSE.txt'),license);
  const paths=['LICENSES/example/LICENSE.txt','scripts/source-byte-inventory.json'];
  await writeFile(path.join(root,'scripts','source-byte-inventory.json'),JSON.stringify({format:'loreviva.source-byte-inventory.v1',paths},null,2)+'\n');
  assert.deepEqual(await byteGate.discoverSource(root),paths);
  const refreshed=await refreshSourceScope(root,{init:true});
  assert.equal(refreshed.files,2);
  await byteGate.checkSourceBytes(root,path.join(root,'.cache','source-byte-scope.json'));
  assert.deepEqual(await readFile(path.join(root,'LICENSES','example','LICENSE.txt')),license);
  await mkdir(path.join(root,'unknown-domain'));
  await assert.rejects(byteGate.discoverSource(root),/Unknown source directory/);
 }finally{await rm(root,{recursive:true,force:true});}
});

test('project LF, binary and upstream raw classes preserve bytes',()=>{
 assert.equal(verifyBytes('a.go',Buffer.from('package a\n')),'TEXT_LF');
 for(const name of ['a.ts','a.mts','a.d.cts','a.tsx'])assert.equal(verifyBytes(name,Buffer.from('export {};\n')),'TEXT_LF');
 assert.throws(()=>verifyBytes('a.d.cts',Buffer.from('export {};\r\n')));
 assert.equal(verifyBytes('a.png',Buffer.from([0,13,10,255])),'BINARY_RAW');
 assert.equal(verifyBytes('LICENSES/original/LICENSE.txt',Buffer.from('original\r\n')),'UPSTREAM_RAW');
});
test('invalid text, unknown kinds and aliases are rejected, never repaired',()=>{
 for(const [name,body] of [['a.go','a\r\nb'],['a.go','a\rb'],['a.go','\ufeffa'],['a.go','a\0'],['unknown.xyz','x'],['../a.go','x'],['docs\\a.md','x'],['LICENSES/typescript-5.9.3-LICENSE.txt','changed']])assert.throws(()=>verifyBytes(name,Buffer.from(body)));
 assert.throws(()=>verifyBytes('a.ts',Buffer.from([255])));
});
test('working source and real index/tree bytes remain separate under Git transforms',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-byte-policy-'));
 const git=process.env.LOREVIVA_SYNTHETIC_GIT||'git';
 function run(...args){const r=spawnSync(git,['-C',root,...args],{encoding:'utf8'});assert.equal(r.status,0,'Git '+args[0]);return r.stdout.trim();}
 try{
  run('init','-b','main');run('config','user.name','Byte policy test');run('config','user.email','test@example.invalid');run('config','core.autocrlf','true');
  await writeFile(path.join(root,'.gitattributes'),'*.ts text eol=lf\n');await writeFile(path.join(root,'a.ts'),'const a=1;\r\n');run('add','.');
  await assert.rejects(checkBytes(root,{git}),/UTF-8 LF/);
  assert.equal((await checkBytes(root,{index:true,git})).length,2);
  run('commit','-m','synthetic transformed index');assert.equal((await checkBytes(root,{revision:'HEAD',git})).length,2);
  await writeFile(path.join(root,'a.ts'),'const a=1;\n');assert.equal((await checkBytes(root,{git})).length,2);
  // A clean filter changes the actual blob. Exact input equality must notice.
  await writeFile(path.join(root,'filter.cjs'),'process.stdin.resume();process.stdin.on("end",()=>process.stdout.write("changed\\n"));\n');
  run('config','filter.bytecheck.clean','"'+process.execPath.replaceAll('\\','/')+'" filter.cjs');
  await writeFile(path.join(root,'.gitattributes'),'*.ts filter=bytecheck\n');run('add','a.ts','.gitattributes');
  const blob=spawnSync(git,['-C',root,'show',':a.ts'],{encoding:null});assert.equal(blob.status,0);assert.notDeepEqual(blob.stdout,await readFile(path.join(root,'a.ts')));
 }finally{await rm(root,{recursive:true,force:true});}
});

// These are real Git and filesystem boundaries, not equality assertions outside the gate.
test('bridge rejects different valid LF bytes and exact inventory or mode drift',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-byte-bridge-'));
 const git=process.env.LOREVIVA_SYNTHETIC_GIT||'git';
 const run=(...args)=>{const r=spawnSync(git,['-C',root,...args],{encoding:'utf8'});assert.equal(r.status,0,'Git '+args[0]);return r.stdout.trim();};
 try{
  run('init','-b','main');run('config','user.name','Byte test');run('config','user.email','test@example.invalid');
  await writeFile(path.join(root,'a.ts'),'export {};\n');run('add','.');run('commit','-m','base');
  assert.equal(typeof byteGate.checkGitBridge,'function');
  assert.equal((await byteGate.checkGitBridge(root,{git})).length,1);
  await writeFile(path.join(root,'a.ts'),'export const changed=1;\n');await assert.rejects(byteGate.checkGitBridge(root,{git}),/byte/i);
  run('add','a.ts');await assert.rejects(byteGate.checkGitBridge(root,{git}),/byte/i);
  run('commit','-m','changed');await byteGate.checkGitBridge(root,{git});
  await writeFile(path.join(root,'extra.ts'),'export {};\n');await assert.rejects(byteGate.checkGitBridge(root,{git}),/inventory/i);await rm(path.join(root,'extra.ts'));
  run('update-index','--chmod=+x','a.ts');await assert.rejects(byteGate.checkGitBridge(root,{git}),/mode/i);run('update-index','--chmod=-x','a.ts');
  await writeFile(path.join(root,'filter.cjs'),'process.stdin.resume();process.stdin.on("end",()=>process.stdout.write("filtered\\n"));\n');
  run('config','filter.rawbridge.clean','"'+process.execPath.replaceAll('\\','/')+'" filter.cjs');
  await writeFile(path.join(root,'.gitattributes'),'*.ts filter=rawbridge\n');
  await writeFile(path.join(root,'a.ts'),'export const afterFilter=1;\n');run('add','.');run('commit','-m','filtered');
  const filtered=spawnSync(git,['-C',root,'show','HEAD:a.ts'],{encoding:null});assert.equal(filtered.status,0);assert.notDeepEqual(filtered.stdout,await readFile(path.join(root,'a.ts')),'The real filter must change the committed bytes before testing refusal');
  await assert.rejects(byteGate.checkGitBridge(root,{git}),/byte/i);
 }finally{await rm(root,{recursive:true,force:true});}
});
test('root and ancestor aliases fail before the first manifest read',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-byte-root-'));
 try{
  const target=path.join(root,'target');await mkdir(path.join(target,'child'),{recursive:true});
  await writeFile(path.join(target,'PUBLICATION.json'),'not json');await writeFile(path.join(target,'child','PUBLICATION.json'),'not json');
  const alias=path.join(root,'alias');await symlink(target,alias,process.platform==='win32'?'junction':'dir');
  await assert.rejects(checkBytes(alias),/Alias/);await assert.rejects(checkBytes(path.join(alias,'child')),/Alias/);
 }finally{await rm(root,{recursive:true,force:true});}
});

test('manifest leaf alias fails before reading or parsing its target',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-manifest-leaf-'));
 try{
  const target=path.join(root,'invalid-target.json');await writeFile(target,'not json');
  // Windows file-symlink privilege is not assumed: a real leaf junction must also be rejected before I/O.
  if(process.platform==='win32'){const directory=path.join(root,'target-directory');await mkdir(directory);await symlink(directory,path.join(root,'PUBLICATION.json'),'junction');}
  else await symlink(target,path.join(root,'PUBLICATION.json'),'file');
  await assert.rejects(checkBytes(root),/Alias refused/);
  assert.equal(await readFile(target,'utf8'),'not json');
 }finally{await rm(root,{recursive:true,force:true});}
});
// This explicit file-symlink metadata simulation verifies ordering without claiming native Windows privilege.
test('simulated manifest file-symlink refuses before invalid JSON read',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-manifest-simulation-')),manifest=path.join(root,'PUBLICATION.json');
 await writeFile(manifest,'not json');let reads=0;
 const fs=await import('node:fs/promises'),{syncBuiltinESMExports}=await import('node:module');
 const originalRead=fs.default.readFile,originalStat=fs.default.lstat;
 try{
  fs.default.readFile=async function(file,...rest){if(String(file)===manifest)reads++;return originalRead.call(this,file,...rest);};
  fs.default.lstat=async function(file,...rest){if(String(file)===manifest)return {isSymbolicLink:()=>true,isFile:()=>true};return originalStat.call(this,file,...rest);};
  syncBuiltinESMExports();
  await assert.rejects(checkBytes(root),/Alias refused/);assert.equal(reads,0,'The leaf target must never be read');
 }finally{fs.default.readFile=originalRead;fs.default.lstat=originalStat;syncBuiltinESMExports();await rm(root,{recursive:true,force:true});}
});

test('regular manifest is guarded and read without requiring a Git checkout',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'lv-manifest-regular-'));
 try{await writeFile(path.join(root,'a.ts'),'export {};\n');await writeFile(path.join(root,'PUBLICATION.json'),JSON.stringify({files:{'a.ts':{}}})+'\n');assert.equal((await checkBytes(root)).length,2);}
 finally{await rm(root,{recursive:true,force:true});}
});
