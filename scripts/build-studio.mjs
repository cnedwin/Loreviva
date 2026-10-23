import {readFile, mkdir, writeFile, rm, lstat} from 'node:fs/promises';
import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {snapshotInputs, writeStudioManifest, verifyStudio} from './studio-artifact.mjs';

const repo=fileURLToPath(new URL('../',import.meta.url));
const app=path.join(repo,'apps/studio'),source=path.join(app,'dist'),target=path.join(repo,'internal/app/studio_dist');
const args=process.argv.slice(2);
if(args.length===1&&args[0]==='--check'){
  const manifest=await verifyStudio(repo,target);
  console.log(`Studio: ${manifest.inputs.length} inputs and ${manifest.files.length} embedded files verified`);
}else{
  if(args.length)throw Error('Usage: node scripts/build-studio.mjs [--check]');
  const before=await snapshotInputs(repo);
  try{if((await lstat(source)).isSymbolicLink())throw Error('Studio output directory link forbidden');}catch(e){if(e.code!=='ENOENT')throw e;}
  // Only this entry point stamps provenance; an arbitrary old dist cannot be adopted.
  const env={...process.env,NODE_ENV:'production'};
  for(const key of Object.keys(env))if(key.startsWith('VITE_'))delete env[key];
  for(const [tool,toolArgs] of [['vue-tsc/bin/vue-tsc.js',['--noEmit']],['vite/bin/vite.js',['build','--config',path.join(app,'vite.config.ts')]]]){
    const result=spawnSync(process.execPath,[path.join(app,'node_modules',tool),...toolArgs],{cwd:app,env,stdio:'inherit',shell:false});
    if(result.error)throw result.error;
    if(result.status!==0||result.signal)throw Error('Studio compiler failed: '+tool);
  }
  await writeStudioManifest(repo,source,before);
  const manifest=await verifyStudio(repo,source);
  // Validate before touching the fixed destination, and do not follow a replaced directory.
  for(const dir of [path.join(repo,'internal'),path.join(repo,'internal/app'),target]){
    try{if((await lstat(dir)).isSymbolicLink())throw Error('Embedded Studio destination link forbidden');}catch(e){if(e.code!=='ENOENT')throw e;}
  }
  const payloads=await Promise.all([...manifest.files.map(f=>f.path),'manifest.json'].map(async name=>({name,bytes:await readFile(path.join(source,name))})));
  await verifyStudio(repo,source);
  if(target!==path.join(repo,'internal','app','studio_dist'))throw Error('Invalid embed destination');
  await rm(target,{recursive:true,force:true});
  // The manifest is last: an interrupted copy cannot present a complete build receipt.
  for(const {name,bytes} of payloads){
    const destination=path.join(target,name);await mkdir(path.dirname(destination),{recursive:true});await writeFile(destination,bytes);
  }
  await verifyStudio(repo,target);
  console.log(`Studio embedded: ${manifest.files.length} verified files`);
}
