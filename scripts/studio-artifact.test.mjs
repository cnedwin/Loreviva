import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, writeFile, cp, rm, symlink} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawnSync} from 'node:child_process';

const repo=fileURLToPath(new URL('../',import.meta.url));
test('checking an unproven build fails without relabelling or replacing embedded files',async t=>{
  const root=await mkdtemp(path.join(tmpdir(),'loreviva-studio-'));
  t.after(()=>rm(root,{recursive:true,force:true}));
  for(const dir of ['scripts','apps/studio/dist/assets','contracts/generated','internal/app/studio_dist'])await mkdir(path.join(root,dir),{recursive:true});
  await cp(path.join(repo,'scripts/build-studio.mjs'),path.join(root,'scripts/build-studio.mjs'));
  // The production helper is copied when present; the original copier has none.
  await cp(path.join(repo,'scripts/studio-artifact.mjs'),path.join(root,'scripts/studio-artifact.mjs')).catch(e=>{if(e.code!=='ENOENT')throw e;});
  await writeFile(path.join(root,'apps/studio/dist/index.html'),'<script src="/studio/assets/old.js"></script>');
  await writeFile(path.join(root,'apps/studio/dist/assets/old.js'),'old-contract');
  await writeFile(path.join(root,'contracts/generated/manifest.json'),JSON.stringify({source_hash:'a'.repeat(64)}));
  const target=path.join(root,'internal/app/studio_dist/index.html');
  await writeFile(target,'retain existing artifact');
  const p=spawnSync(process.execPath,[path.join(root,'scripts/build-studio.mjs'),'--check'],{cwd:tmpdir(),encoding:'utf8',timeout:10000});
  assert.ifError(p.error);
  assert.notEqual(p.status,0,'unproven artifacts must not be stamped with the current contract');
  assert.equal(await readFile(target,'utf8'),'retain existing artifact');
});

const digest=bytes=>createHash('sha256').update(bytes).digest('hex');
async function fixture(t){
  const root=await mkdtemp(path.join(tmpdir(),'loreviva-studio-'));
  t.after(()=>rm(root,{recursive:true,force:true}));
  // Hand-built input/output proof, independent of the production manifest writer.
  const inputs={
    'apps/studio/src/main.ts':'export const x=1;', 'apps/studio/tests/sample.ts':'export {};', 'apps/studio/index.html':'<div id="app"></div>',
    'apps/studio/vite.config.ts':'export default {};','apps/studio/tsconfig.json':'{}','apps/studio/package.json':'{}',
    'packages/sdk-ts/src/index.ts':'export {};','packages/sdk-ts/package.json':'{}','packages/sdk-ts/tsconfig.json':'{}',
    'contracts/source/openapi.yaml':'openapi: 3.1.0','contracts/generated/manifest.json':JSON.stringify({source_hash:'a'.repeat(64)}),
    'package.json':'{}','pnpm-lock.yaml':'lockfileVersion: 9','pnpm-workspace.yaml':'packages: []',
    'scripts/build-studio.mjs':'// builder','scripts/studio-artifact.mjs':'// provenance'
  };
  const outputs={'index.html':'<script src="/studio/assets/app.js"></script>','assets/app.js':'compiled bytes'};
  for(const [p,b] of Object.entries(inputs)){await mkdir(path.dirname(path.join(root,p)),{recursive:true});await writeFile(path.join(root,p),b);}
  const dir=path.join(root,'internal/app/studio_dist');
  for(const [p,b] of Object.entries(outputs)){await mkdir(path.dirname(path.join(dir,p)),{recursive:true});await writeFile(path.join(dir,p),b);}
  const rows=files=>Object.entries(files).sort(([a],[b])=>a<b?-1:a>b?1:0).map(([p,b])=>({path:p,size:Buffer.byteLength(b),sha256:digest(b)}));
  const manifest={format:'loreviva.studio-build.v1',source_hash:'a'.repeat(64),inputs:rows(inputs),files:rows(outputs)};
  await writeFile(path.join(dir,'manifest.json'),JSON.stringify(manifest));
  return {root,dir,manifest};
}

test('valid proof verifies exact inputs and output bytes without rewriting them',async t=>{
  const f=await fixture(t);const {verifyStudio}=await import('./studio-artifact.mjs');
  const before=await readFile(path.join(f.dir,'manifest.json'));
  await verifyStudio(f.root,f.dir);
  assert.deepEqual(await readFile(path.join(f.dir,'manifest.json')),before);
});
for(const [name,mutate] of Object.entries({
  'changed Studio source':f=>writeFile(path.join(f.root,'apps/studio/src/main.ts'),'different'),
  'changed SDK source':f=>writeFile(path.join(f.root,'packages/sdk-ts/src/index.ts'),'different'),
  'added input':f=>writeFile(path.join(f.root,'apps/studio/src/new.ts'),'new'),
  'removed input':f=>rm(path.join(f.root,'apps/studio/src/main.ts')),
  'changed contract':f=>writeFile(path.join(f.root,'contracts/generated/manifest.json'),JSON.stringify({source_hash:'b'.repeat(64)})),
  'tampered output':f=>writeFile(path.join(f.dir,'assets/app.js'),'tampered'),
  'missing output':f=>rm(path.join(f.dir,'assets/app.js')),
  'extra output':f=>writeFile(path.join(f.dir,'assets/old.js'),'old'),
  'unrecorded local build environment':f=>writeFile(path.join(f.root,'apps/studio/.env.local'),'VITE_LOCAL=1'),
  'alternative Vite configuration':f=>writeFile(path.join(f.root,'apps/studio/vite.config.js'),'export default {base:"/wrong/"}'),
  'old manifest format':f=>writeFile(path.join(f.dir,'manifest.json'),JSON.stringify({source_hash:f.manifest.source_hash,files:f.manifest.files})),
  'forged traversal output':f=>writeFile(path.join(f.dir,'manifest.json'),JSON.stringify({...f.manifest,files:[...f.manifest.files,{path:'../outside',sha256:'a'.repeat(64),size:1}]}))
}))test('rejects '+name,async t=>{
  const f=await fixture(t);await mutate(f);
  const {verifyStudio}=await import('./studio-artifact.mjs');await assert.rejects(verifyStudio(f.root,f.dir));
});

test('source changes during compilation prevent creation of a provenance receipt',async t=>{
  const f=await fixture(t);const {snapshotInputs,writeStudioManifest}=await import('./studio-artifact.mjs');
  const before=await snapshotInputs(f.root);await writeFile(path.join(f.root,'apps/studio/src/main.ts'),'changed during build');
  const original=await readFile(path.join(f.dir,'manifest.json'));
  await assert.rejects(writeStudioManifest(f.root,f.dir,before),/inputs changed/);
  assert.deepEqual(await readFile(path.join(f.dir,'manifest.json')),original);
});

test('rejects output links before following their content',async t=>{
  const f=await fixture(t);
  // A directory junction is available on supported Windows hosts without developer mode.
  await symlink(path.join(f.root,'packages'),path.join(f.dir,'linked'),process.platform==='win32'?'junction':'dir');
  const {verifyStudio}=await import('./studio-artifact.mjs');await assert.rejects(verifyStudio(f.root,f.dir),/link/);
});
