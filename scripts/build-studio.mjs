import {readFile,readdir,mkdir,writeFile,rm} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const repo=fileURLToPath(new URL('../',import.meta.url));
const source=path.resolve(repo,'apps/studio/dist'),target=path.resolve(repo,'internal/app/studio_dist');
// Both roots are fixed project children; never delete a caller-supplied path.
if(target!==path.join(repo,'internal','app','studio_dist'))throw Error('invalid embed destination');
const files=[];
async function collect(dir,relative=''){for(const e of await readdir(dir,{withFileTypes:true})){const name=relative?relative+'/'+e.name:e.name;if(e.isSymbolicLink())throw Error('build symlink forbidden');if(e.isDirectory())await collect(path.join(dir,e.name),name);else {if(name!=='index.html'&&!name.startsWith('assets/'))throw Error('unexpected build output');const bytes=await readFile(path.join(dir,e.name));files.push({name,bytes,sha256:createHash('sha256').update(bytes).digest('hex')})}}}
await collect(source);if(!files.some(f=>f.name==='index.html'&&f.bytes.includes('/studio/assets/')))throw Error('build the real Studio first');
await rm(target,{recursive:true,force:true});
for(const f of files){const destination=path.join(target,f.name);await mkdir(path.dirname(destination),{recursive:true});await writeFile(destination,f.bytes)}
const manifest={source_hash:JSON.parse(await readFile(path.join(repo,'contracts/generated/manifest.json'),'utf8')).source_hash,files:files.sort((a,b)=>a.name.localeCompare(b.name)).map(({name,sha256,bytes})=>({path:name,sha256,size:bytes.length}))};
await writeFile(path.join(target,'manifest.json'),JSON.stringify(manifest,null,2)+'\n');console.log(JSON.stringify(manifest));
