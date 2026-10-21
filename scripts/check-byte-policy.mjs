import {readFile,lstat,readdir} from 'node:fs/promises';
import {lstatSync} from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import {spawnSync} from 'node:child_process';

const textExtensions=new Set('.go .mod .sum .json .yaml .yml .md .txt .js .mjs .cjs .ts .mts .cts .tsx .vue .css .html .sql .sh .ps1 .svg .xml .toml'.split(' '));
const binaryExtensions=new Set('.png .jpg .jpeg .gif .webp .ico .woff .woff2 .ttf .otf .pdf .zip .wasm .exe .dll'.split(' '));
const textNames=new Set(['.env.example','.gitattributes','.gitignore','.editorconfig','LICENSE','NOTICE']);
const rawLicense={path:'LICENSES/typescript-5.9.3-LICENSE.txt',bytes:9197,sha256:'a7d00bfd54525bc694b6e32f64c7ebcf5e6b7ae3657be5cc12767bce74654a47'};
const sha=b=>createHash('sha256').update(b).digest('hex');
// Public paths keep exact POSIX spelling; private Windows path equivalence never applies here.
export function validName(name){
 if(typeof name!=='string'||!name||name.includes('\\')||name.includes(':')||name.includes('\0')||name.startsWith('/')||name.split('/').some(p=>!p||p==='.'||p==='..'||/[. ]$/.test(p)||/^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\.|$)/i.test(p)))throw Error('Unsafe relative path');
 return name;
}
// Classify original bytes without repairing them; upstream licenses and binaries retain raw identity.
export function verifyBytes(name,body){
 validName(name);if(!Buffer.isBuffer(body))throw Error('Buffer required');
 if(name===rawLicense.path&&(body.length!==rawLicense.bytes||sha(body)!==rawLicense.sha256))throw Error('Raw license drift');
 if(name.startsWith('LICENSES/'))return 'UPSTREAM_RAW';
 const extension=path.posix.extname(name);
 if(binaryExtensions.has(extension))return 'BINARY_RAW';
 if(!textExtensions.has(extension)&&!textNames.has(name))throw Error('Unclassified bytes: '+name);
 new TextDecoder('utf-8',{fatal:true}).decode(body);
 if(body.subarray(0,3).equals(Buffer.from([239,187,191]))||body.includes(13)||body.includes(0))throw Error('Text must be UTF-8 LF without BOM: '+name);
 return 'TEXT_LF';
}
// Inspect unresolved ancestors from the volume root before any manifest, child or Git read.
export function guardRoot(root){
 if(typeof root!=='string'||!path.isAbsolute(root)||root.split(/[\\/]/).some(p=>p==='.'||p==='..'||/[. ]$/.test(p)))throw Error('Unsafe root path');
 if(process.platform==='win32'&&!/^[A-Za-z]:[\\/]/.test(root))throw Error('Unsupported root namespace');
 const base=path.parse(root).root;let current=base;
 for(const part of ['',...root.slice(base.length).split(path.sep).flatMap(p=>p.split('/')).filter(Boolean)]){
  if(part)current=path.join(current,part);const metadata=lstatSync(current);
  if(metadata.isSymbolicLink())throw Error('Alias root refused');
  if(!metadata.isDirectory())throw Error('Directory root required');
 }
 return current;
}
// Git returns real immutable blob bytes; no clean/smudge normalization is applied by this reader.
function gitRead(root,args,git='git'){
 const result=spawnSync(git,['-C',root,...args],{encoding:null,maxBuffer:128*1024*1024});
 if(result.error||result.status!==0)throw Error('Git byte observation failed: '+args[0]);
 return result.stdout;
}
function nulEntries(raw){if(!raw.length)return [];if(raw.at(-1)!==0)throw Error('Truncated Git inventory');return raw.subarray(0,-1).toString('utf8').split('\0');}
// A closed regular-file inventory rejects unmerged entries, duplicate names and unsupported modes.
export function gitInventory(root,{revision=null,index=false,git='git'}={}){
 root=guardRoot(root);
 if(index===Boolean(revision))throw Error('Choose exactly one Git byte source');
 const rows=nulEntries(gitRead(root,index?['ls-files','--stage','-z']:['ls-tree','-r','-z',revision],git));
 const result=[];const seen=new Set();
 for(const row of rows){const tab=row.indexOf('\t');if(tab<0)throw Error('Invalid Git row');const fields=row.slice(0,tab).split(' '),name=validName(row.slice(tab+1));
  const [mode,second,third]=fields,oid=index?second:third;
  if(fields.length!==3||!['100644','100755'].includes(mode)||(index?third!=='0':second!=='blob')||!(/^[0-9a-f]{40}$/.test(oid))||seen.has(name)||seen.has(name.toLowerCase()))throw Error('Nonregular/duplicate/unmerged Git entry');
  seen.add(name);seen.add(name.toLowerCase());const body=gitRead(root,['cat-file','blob',oid],git);result.push({name,mode,oid,body});
 }
 return result;
}
// Exact child spelling and each ancestor type are checked before reading working bytes.
export async function workingBody(root,name){
 let current=path.resolve(root);
 for(const part of validName(name).split('/')){if(!(await readdir(current)).includes(part))throw Error('Exact path spelling missing: '+name);current=path.join(current,part);const st=await lstat(current);if(st.isSymbolicLink())throw Error('Alias refused: '+name);}
 const st=await lstat(current);if(!st.isFile())throw Error('Regular file required');return readFile(current);
}
export const inventoryName='scripts/source-byte-inventory.json';
export const sourceDomains=['.github','apps','cmd','contracts','docs','examples','internal','migrations','packages','scripts','tests'];
const excludedDirectories=new Set(['apps/reference-client/dist','apps/reference-client/node_modules','apps/studio/dist','apps/studio/node_modules','packages/sdk-ts/dist','packages/sdk-ts/node_modules']);
// These are exact root domains, never basename exclusions at arbitrary depths.
const excludedRootDirectories=new Set(['.git','.cache','node_modules','evidence','.sdd','.superpowers','test-results']);
const historicalRootFiles=new Set([
 'phase32-admin-detail-binding-diagnostic.txt','phase32-admin-detail-binding-diagnostic.txt.command.json',
 'phase32-admin-detail-first.txt','phase32-admin-detail-first.txt.command.json',
 'phase32-admin-detail-green.txt','phase32-admin-detail-green.txt.command.json',
 'phase32-admin-detail-red.txt','phase32-admin-detail-red.txt.command.json',
 'phase32-admin-legacy-hash-green.txt','phase32-admin-legacy-hash-green.txt.command.json',
 'phase32-admin-legacy-hash-red.txt','phase32-admin-legacy-hash-red.txt.command.json',
 'phase32-admin-legacy-proof-green.txt','phase32-admin-legacy-proof-green.txt.command.json',
 'phase32-admin-legacy-proof-red.txt','phase32-admin-legacy-proof-red.txt.command.json',
 'phase32-detail-final-proof-green.txt','phase32-detail-final-proof-green.txt.command.json',
 'phase32-detail-malformed-proof-red.txt','phase32-detail-malformed-proof-red.txt.command.json',
 'phase32-detail-missing-original-green.txt','phase32-detail-missing-original-green.txt.command.json',
 'phase32-detail-proof-accounting.txt','phase32-detail-proof-accounting.txt.command.json',
 'phase32-final-affected.txt','phase32-final-affected.txt.command.json',
 'phase32-final-affected2.txt','phase32-final-affected2.txt.command.json',
 'phase32-list-late-green.txt','phase32-list-late-green.txt.command.json'
]);
export const compareNames=(a,b)=>Buffer.compare(Buffer.from(a),Buffer.from(b));
export function closedObject(value,keys){if(!value||typeof value!=='object'||Array.isArray(value)||Object.keys(value).sort().join('\0')!==[...keys].sort().join('\0'))throw Error('Closed object required');}
// Inventories are exact data, not authority. The caller separately pins the reviewed seed/revision.
export function orderedNames(names){
 if(!Array.isArray(names)||!names.length)throw Error('Nonempty source inventory required');
 for(let i=0;i<names.length;i++){validName(names[i]);if(i&&compareNames(names[i-1],names[i])>=0)throw Error('Source inventory order or duplicate');}
 return names;
}
export function parseInventory(body){
 verifyBytes(inventoryName,body);const data=JSON.parse(body);closedObject(data,['format','paths']);
 if(data.format!=='loreviva.source-byte-inventory.v1')throw Error('Inventory format');
 orderedNames(data.paths);if(!data.paths.includes(inventoryName))throw Error('Inventory must include itself');return data;
}
// Only these exact dependency/build domains are skipped; aliases elsewhere fail before traversal.
export async function discoverSource(root){
 root=guardRoot(root);const found=[];
 async function walk(relative){
  if(excludedDirectories.has(relative))return;
  const target=path.join(root,relative);let st;try{st=await lstat(target);}catch(e){if(e.code==='ENOENT')return;throw e;}
  if(st.isSymbolicLink())throw Error('Alias refused: '+relative);
  if(st.isDirectory()){for(const child of await readdir(target))await walk(relative+'/'+child);}
  else if(st.isFile()){validName(relative);found.push(relative);}else throw Error('Nonregular source');
 }
 for(const name of await readdir(root)){
  if(excludedRootDirectories.has(name)||historicalRootFiles.has(name))continue;
  const metadata=await lstat(path.join(root,name));
  if(metadata.isSymbolicLink())throw Error('Alias refused: '+name);
  if(metadata.isDirectory()){if(!sourceDomains.includes(name))throw Error('Unknown source directory: '+name);await walk(name);}
  else if(metadata.isFile()){validName(name);found.push(name);}else throw Error('Nonregular source');
 }
 return found.sort(compareNames);
}
// Partial library data is explicitly opt-in; the engineering CLI always requires the complete inventory.
export async function checkSourceBytes(root,scopeFile,{partial=false}={}){
 if(typeof partial!=='boolean')throw Error('Partial mode must be boolean');
 root=guardRoot(root);const scopePath=path.resolve(scopeFile);guardRoot(path.dirname(scopePath));
 const raw=await workingBody(path.dirname(scopePath),path.basename(scopePath));verifyBytes('scope.json',raw);
 const scope=JSON.parse(raw);closedObject(scope,['format','files']);if(scope.format!=='loreviva.source-byte-scope.v1'||!Array.isArray(scope.files))throw Error('Source scope format');
 const names=orderedNames(scope.files.map(x=>x?.path));const output=[];
 if(!partial&&!names.includes(inventoryName))throw Error('Project inventory required');
 for(const entry of scope.files){
  closedObject(entry,['path','bytes','sha256']);
  if(!Number.isSafeInteger(entry.bytes)||entry.bytes<0||typeof entry.sha256!=='string'||!(/^[0-9a-f]{64}$/.test(entry.sha256)))throw Error('Source fingerprint type');
  const body=await workingBody(root,entry.path),kind=verifyBytes(entry.path,body);
  if(body.length!==entry.bytes||sha(body)!==entry.sha256)throw Error('Source fingerprint stale: '+entry.path);
  output.push({...entry,kind});
 }
 if(names.includes(inventoryName)){
  const inventory=parseInventory(await workingBody(root,inventoryName));
  if(JSON.stringify(inventory.paths)!==JSON.stringify(names))throw Error('Scope/inventory differs');
  for(const found of await discoverSource(root))if(!names.includes(found))throw Error('Uninventoried source: '+found);
 }
 return output;
}
export async function checkBytes(root,{revision=null,index=false,git='git'}={}){
 root=guardRoot(root);const output=[];
 if(revision||index){for(const row of gitInventory(root,{revision,index,git}))output.push({path:row.name,kind:verifyBytes(row.name,row.body),bytes:row.body.length,sha256:sha(row.body)});return output;}
 let names,hasManifest=true;
 // Only initial absence selects Git fallback. An existing manifest must pass the exact leaf guard before any body read.
 try{await lstat(path.join(root,'PUBLICATION.json'));}catch(error){if(error.code!=='ENOENT')throw error;hasManifest=false;}
 if(hasManifest){const raw=await workingBody(root,'PUBLICATION.json');verifyBytes('PUBLICATION.json',raw);const manifest=JSON.parse(raw);names=[...Object.keys(manifest.files),'PUBLICATION.json'];}
 else names=nulEntries(gitRead(root,['ls-files','-z'],git));
 if(!names.length||new Set(names).size!==names.length)throw Error('Empty or duplicate inventory');
 for(const name of names){const body=await workingBody(root,name);output.push({path:name,kind:verifyBytes(name,body),bytes:body.length,sha256:sha(body)});}
 return output;
}
// Classification alone cannot prove identity: this gate binds one managed set across all three sources.
export async function checkGitBridge(root,{revision='HEAD',git='git'}={}){
 root=guardRoot(root);
 const staged=gitInventory(root,{index:true,git}),committed=gitInventory(root,{revision,git});
 const names=nulEntries(gitRead(root,['ls-files','--cached','--others','--exclude-standard','-z'],git));
 const expected=staged.map(x=>x.name).sort();
 if(!expected.length||new Set(names).size!==names.length||JSON.stringify([...names].sort())!==JSON.stringify(expected)||JSON.stringify(committed.map(x=>x.name).sort())!==JSON.stringify(expected))throw Error('Working/index/commit inventory differs');
 const byName=new Map(committed.map(x=>[x.name,x]));const output=[];
 // Windows Git represents executable mode in the index when core.filemode=false; native regular type is still mandatory.
 const windowsIndexMode=process.platform==='win32';
 if(windowsIndexMode&&gitRead(root,['config','--bool','core.filemode'],git).toString('utf8').trim()!=='false')throw Error('Unsupported Windows working mode policy');
 for(const row of staged){
  const head=byName.get(row.name),body=await workingBody(root,row.name),metadata=await lstat(path.join(root,row.name));
  const workingMode=windowsIndexMode?row.mode:((metadata.mode&0o111)?'100755':'100644');
  if(row.mode!==head.mode||workingMode!==row.mode)throw Error('Working/index/commit mode differs: '+row.name);
  verifyBytes(row.name,body);verifyBytes(row.name,row.body);verifyBytes(row.name,head.body);
  if(!body.equals(row.body)||!row.body.equals(head.body))throw Error('Working/index/commit byte identity differs: '+row.name);
  output.push({path:row.name,mode:row.mode,bytes:body.length,sha256:sha(body)});
 }
 return output;
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url)){
 const args=process.argv.slice(2);let options={};
 if(args.length===2&&args[0]==='--source-scope'){const rows=await checkSourceBytes(process.cwd(),args[1]);console.log('Explicit source bytes: '+rows.length+' files (not a Git or approval proof)');}
 else if(args.length===1&&args[0]==='--bridge'){const rows=await checkGitBridge(process.cwd());console.log('Exact working/index/HEAD bridge: '+rows.length+' files');}
 else{
  if(args.length===2&&args[0]==='--git')options={revision:args[1]};
  else if(args.length===1&&args[0]==='--git-index')options={index:true};
  else if(args.length)throw Error('Usage: check-byte-policy.mjs [--bridge|--git REV|--git-index]');
  const rows=await checkBytes(process.cwd(),options);console.log('Byte policy: '+rows.length+' exact files checked');
 }
}
