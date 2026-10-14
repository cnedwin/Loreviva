import {readFile,readdir,lstat,mkdir,writeFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const repo=fileURLToPath(new URL('../',import.meta.url));
const output=path.join(repo,'evidence/r0/task07/dependencies');await mkdir(path.join(output,'licenses'),{recursive:true});
const hash=b=>createHash('sha256').update(b).digest('hex');
const lock=await readFile(path.join(repo,'pnpm-lock.yaml'));const records=[];let section=false,current;
for(const line of lock.toString('utf8').split(/\r?\n/)){if(line==='packages:'){section=true;continue}if(line==='snapshots:'){section=false;continue}if(!section)continue;const match=/^  (\S.*):$/.exec(line);if(match){current={key:match[1].replace(/^'(.*)'$/,'$1'),record:line};records.push(current)}else if(current)current.record+='\n'+line}
const installed=[];const base=path.join(repo,'node_modules/.pnpm');
async function rightsFiles(dir,relative=''){const out=[];for(const item of await readdir(dir,{withFileTypes:true})){const p=path.join(dir,item.name),r=relative?relative+'/'+item.name:item.name;if(item.isDirectory())out.push(...await rightsFiles(p,r));else if(item.isFile())out.push({file:p,relative:r})}return out}
// This records the observed installed packages and retained rights files; lock entries and license declarations alone do not establish the embedded distribution or complete rights clearance.
for(const slot of await readdir(base,{withFileTypes:true})){if(!slot.isDirectory()||slot.name==='node_modules')continue;const modules=path.join(base,slot.name,'node_modules');let entries;try{entries=await readdir(modules,{withFileTypes:true})}catch{continue}const candidates=[];for(const entry of entries){const p=path.join(modules,entry.name);if(entry.name.startsWith('@')&&entry.isDirectory())for(const sub of await readdir(p))candidates.push(path.join(p,sub));else candidates.push(p)}
 for(const dir of candidates){if((await lstat(dir)).isSymbolicLink())continue;let bytes;try{bytes=await readFile(path.join(dir,'package.json'))}catch{continue}const pkg=JSON.parse(bytes);const item={name:pkg.name,version:pkg.version,license:pkg.license??null,manifest_sha256:hash(bytes),slot:slot.name,rights:[]};
  for(const entry of await readdir(dir,{withFileTypes:true})){if(!/^(licen[sc]e|copying|notice|third.?party)/i.test(entry.name))continue;const p=path.join(dir,entry.name);const files=entry.isDirectory()?await rightsFiles(p,entry.name):entry.isFile()?[{file:p,relative:entry.name}]:[];for(const file of files){const content=await readFile(file.file);const name=(pkg.name+'-'+pkg.version+'--'+file.relative).replace(/[@/\\:]/g,'_');await writeFile(path.join(output,'licenses',name),content);item.rights.push({package_file:file.relative,retained:'licenses/'+name,sha256:hash(content),bytes:content.length})}}
  installed.push(item);
 }
}
installed.sort((a,b)=>(a.name+'@'+a.version+' '+a.slot).localeCompare(b.name+'@'+b.version+' '+b.slot));
const locked=records.map(({key,record})=>{const at=key.lastIndexOf('@'),name=key.slice(0,at),version=key.slice(at+1);return{name,version,integrity:/integrity: ([^}\s]+)/.exec(record)?.[1]??null,installed:installed.some(p=>p.name===name&&p.version===version),lock_record:record}});
if(locked.some(p=>!p.integrity))throw Error('registry lock integrity missing');
const manifests={};for(const name of ['package.json','apps/studio/package.json','packages/sdk-ts/package.json','pnpm-workspace.yaml','go.mod','go.sum'])manifests[name]=hash(await readFile(path.join(repo,name)));
const result={scope:'Actual Task7 Windows amd64 installed dependency inventory and retained upstream rights; not a release or legal acceptance. Uninstalled lock records are not installed-platform proof.',node:process.version,pnpm:'11.25.0',lock_sha256:hash(lock),manifests,installed_instances:installed.length,installed_unique:new Set(installed.map(p=>p.name+'@'+p.version)).size,locked_records:locked.length,no_root_rights_file:installed.filter(p=>!p.rights.length).map(p=>p.name+'@'+p.version),installed,locked,go_dependency_change:'No Go dependency added by Task7; current go.mod/go.sum hashes and prior Task1/2 provenance retained.'};
await writeFile(path.join(output,'inventory.json'),JSON.stringify(result,null,2)+'\n');console.log(JSON.stringify({installed_instances:result.installed_instances,installed_unique:result.installed_unique,locked_records:result.locked_records,no_root_rights_file:result.no_root_rights_file,lock_sha256:result.lock_sha256}));
