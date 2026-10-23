import {readFile, readdir, lstat, writeFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import path from 'node:path';

const format='loreviva.studio-build.v1';
const directories=['apps/studio/src','apps/studio/tests','packages/sdk-ts/src','contracts/source','contracts/generated'];
const files=['apps/studio/index.html','apps/studio/vite.config.ts','apps/studio/tsconfig.json','apps/studio/package.json',
  'packages/sdk-ts/package.json','packages/sdk-ts/tsconfig.json','package.json','pnpm-lock.yaml','pnpm-workspace.yaml',
  'scripts/build-studio.mjs','scripts/studio-artifact.mjs'];
const compare=(a,b)=>a<b?-1:a>b?1:0;
const hash=b=>createHash('sha256').update(b).digest('hex');
const equal=(a,b)=>JSON.stringify(a)===JSON.stringify(b);

async function regular(root,name){
  // Check every component: following an intermediate link also escapes the proof.
  let current=root;
  for(const part of name.split('/')){
    current=path.join(current,part);
    if((await lstat(current)).isSymbolicLink())throw Error('Studio build link forbidden: '+name);
  }
  if(!(await lstat(current)).isFile())throw Error('Studio build file required: '+name);
  return readFile(current);
}
async function walk(root,name=''){
  const dir=path.join(root,name),stat=await lstat(dir);
  if(stat.isSymbolicLink())throw Error('Studio build link forbidden: '+name);
  if(!stat.isDirectory())throw Error('Studio directory required: '+name);
  const result=[];
  for(const entry of await readdir(dir,{withFileTypes:true})){
    const p=name?name+'/'+entry.name:entry.name;
    if(entry.isSymbolicLink())throw Error('Studio build link forbidden: '+p);
    if(entry.isDirectory())result.push(...await walk(root,p));
    else if(entry.isFile())result.push(p);
    else throw Error('Studio special file forbidden: '+p);
  }
  return result.sort(compare);
}
async function rows(root,names){
  const result=[];
  for(const name of [...names].sort(compare)){
    const bytes=await regular(root,name);
    result.push({path:name,size:bytes.length,sha256:hash(bytes)});
  }
  return result;
}
export async function snapshotInputs(root){
  const entries=await readdir(path.join(root,'apps/studio'));
  if(entries.some(n=>n.startsWith('vite.config.')&&n!=='vite.config.ts'))throw Error('Alternative Studio Vite configuration forbidden');
  // Local Vite env files could silently compile secrets or unrecorded settings.
  if(entries.some(n=>n==='.env'||n.startsWith('.env.')))
    throw Error('Studio build environment files are not supported');
  const names=[...files];
  for(const dir of directories)names.push(...await walk(root,dir));
  try{names.push(...await walk(root,'apps/studio/public'));}catch(e){if(e.code!=='ENOENT')throw e;}
  return rows(root,names);
}
async function outputs(dir){
  const names=(await walk(dir)).filter(n=>n!=='manifest.json');
  if(!names.includes('index.html')||names.some(n=>n!=='index.html'&&!n.startsWith('assets/')))
    throw Error('Unexpected Studio output inventory');
  if(!(await regular(dir,'index.html')).includes('/studio/assets/'))throw Error('Real Studio entry required');
  return rows(dir,names);
}
async function sourceHash(root){
  const value=JSON.parse(await regular(root,'contracts/generated/manifest.json')).source_hash;
  if(!/^[a-f0-9]{64}$/.test(value))throw Error('Invalid contract source hash');
  return value;
}
export async function writeStudioManifest(root,dir,before){
  const after=await snapshotInputs(root);
  if(!equal(before,after))throw Error('Studio inputs changed during build');
  const manifest={format,source_hash:await sourceHash(root),inputs:before,files:await outputs(dir)};
  await writeFile(path.join(dir,'manifest.json'),JSON.stringify(manifest,null,2)+'\n');
  return manifest;
}
export async function verifyStudio(root,dir){
  const manifest=JSON.parse(await regular(dir,'manifest.json'));
  if(manifest.format!==format||manifest.source_hash!==await sourceHash(root))throw Error('Stale Studio contract or manifest; run pnpm build:studio');
  if(!equal(manifest.inputs,await snapshotInputs(root)))throw Error('Studio inputs changed; run pnpm build:studio');
  if(!equal(manifest.files,await outputs(dir)))throw Error('Studio output bytes or inventory changed; run pnpm build:studio');
  return manifest;
}
