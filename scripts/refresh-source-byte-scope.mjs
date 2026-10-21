import {readFile,writeFile,mkdir,lstat,rename,unlink} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash,randomUUID} from 'node:crypto';
import {guardRoot,workingBody,verifyBytes,parseInventory,orderedNames,closedObject,compareNames,discoverSource,inventoryName,sourceDomains} from './check-byte-policy.mjs';
const sha=b=>createHash('sha256').update(b).digest('hex');
const encode=v=>Buffer.from(JSON.stringify(v,null,2)+'\n');
const locator='.cache/source-byte-scope.json',pendingName='.cache/source-byte-pending.json',revisions='.cache/source-byte-scopes';
const activeName='.cache/source-byte-active-transition.json';
async function exists(p){try{await lstat(p);return true;}catch(e){if(e.code==='ENOENT')return false;throw e;}}
async function ensureDirectory(root,name){
 let current=root;for(const part of name.split('/')){current=path.join(current,part);if(!(await exists(current)))await mkdir(current);guardRoot(current);}
}
// Revision files are create-only. Equal bytes may be reused, never replaced.
async function immutable(root,name,body){
 await ensureDirectory(root,path.posix.dirname(name));
 if(await exists(path.join(root,name))){const old=await workingBody(root,name);if(!old.equals(body))throw Error('Immutable revision collision');return;}
 await writeFile(path.join(root,name),body,{flag:'wx'});
}
async function replaceCurrent(root,name,body){
 await ensureDirectory(root,path.posix.dirname(name));
 if(await exists(path.join(root,name)))await workingBody(root,name);
 const temp=name+'.'+randomUUID()+'.tmp';await writeFile(path.join(root,temp),body,{flag:'wx'});await rename(path.join(root,temp),path.join(root,name));
}
function parseScope(raw){
 verifyBytes('scope.json',raw);const s=JSON.parse(raw);closedObject(s,['format','files']);
 if(s.format!=='loreviva.source-byte-scope.v1'||!Array.isArray(s.files))throw Error('Scope format');orderedNames(s.files.map(x=>x.path));
 for(const x of s.files){closedObject(x,['path','bytes','sha256']);if(!Number.isSafeInteger(x.bytes)||x.bytes<0||!(/^[0-9a-f]{64}$/.test(x.sha256)))throw Error('Scope fingerprint');}return s;
}
function parseDelta(raw,oldScope,oldInventory){
 if(!raw)return null;verifyBytes('delta.json',raw);const d=JSON.parse(raw);closedObject(d,['format','previous_scope_sha256','previous_inventory_sha256','added','removed']);
 if(d.format!=='loreviva.source-byte-delta.v1'||d.previous_scope_sha256!==sha(oldScope)||d.previous_inventory_sha256!==sha(oldInventory))throw Error('Delta prior identity');
 for(const xs of [d.added,d.removed]){if(!Array.isArray(xs))throw Error('Delta list');if(xs.length)orderedNames(xs);}
 if(d.added.some(x=>d.removed.includes(x))||d.removed.includes(inventoryName))throw Error('Delta overlap or inventory removal');return d;
}
async function dataFile(filename){const p=path.resolve(filename);guardRoot(path.dirname(p));return workingBody(path.dirname(p),path.basename(p));}
async function prepareRows(root,inventoryBody){
 const inventory=parseInventory(inventoryBody),names=inventory.paths;
 for(const name of names){if(name.includes('/')&&!sourceDomains.includes(name.split('/')[0]))throw Error('Outside declared source domain');if(name.startsWith('phase32-'))throw Error('Historical file is not project source');}
 for(const name of await discoverSource(root))if(!names.includes(name))throw Error('Undeclared source requires explicit delta: '+name);
 const files=[];
 for(const name of names){
  // The inventory's own fingerprint binds its prepared NEW bytes, not the still-current old body.
  const body=name===inventoryName?inventoryBody:await workingBody(root,name);verifyBytes(name,body);files.push({path:name,bytes:body.length,sha256:sha(body)});
 }
 return {format:'loreviva.source-byte-scope.v1',files};
}
async function validatePrepared(root,transaction,deltaRaw){
 closedObject(transaction,['format','old_scope','old_inventory','new_scope','new_inventory','delta']);if(transaction.format!=='loreviva.source-byte-transition.v1')throw Error('Transition format');
 if((transaction.old_scope!==null&&typeof transaction.old_scope!=='string')||typeof transaction.old_inventory!=='string'||typeof transaction.new_scope!=='string'||typeof transaction.new_inventory!=='string'||(transaction.delta!==null&&typeof transaction.delta!=='string'))throw Error('Transition field type');
 if(transaction.delta!==(deltaRaw?deltaRaw.toString('utf8'):null))throw Error('Pending delta differs');
 const oldInventory=Buffer.from(transaction.old_inventory),previous=parseInventory(oldInventory),nextInventory=Buffer.from(transaction.new_inventory),nextScope=Buffer.from(transaction.new_scope);parseInventory(nextInventory);parseScope(nextScope);
 let expectedInventory=oldInventory;
 if(transaction.old_scope===null){if(deltaRaw)throw Error('Initial transition cannot have delta');}
 else{
  // Recovery rechecks the immutable prior chain before any write; mutable pending is not a baseline.
  const oldScope=Buffer.from(transaction.old_scope),old=parseScope(oldScope);
  const archived=await workingBody(root,revisions+'/'+sha(oldScope)+'.json');if(!archived.equals(oldScope))throw Error('Prior revision differs');
  const item=old.files.find(x=>x.path===inventoryName);if(!item||item.bytes!==oldInventory.length||item.sha256!==sha(oldInventory))throw Error('Prior inventory identity');
  const archivedInventory=await workingBody(root,revisions+'/inventories/'+item.sha256+'.json');if(!archivedInventory.equals(oldInventory))throw Error('Prior inventory differs');
  if(JSON.stringify(old.files.map(x=>x.path))!==JSON.stringify(previous.paths))throw Error('Prior scope inventory differs');
  const delta=parseDelta(deltaRaw,oldScope,oldInventory);
  if(delta){
   for(const name of delta.removed)if(!previous.paths.includes(name)||await exists(path.join(root,name)))throw Error('Removal must be prior and physically absent');
   for(const name of delta.added)if(previous.paths.includes(name))throw Error('Addition already inventoried');
   expectedInventory=encode({format:previous.format,paths:[...previous.paths.filter(x=>!delta.removed.includes(x)),...delta.added].sort(compareNames)});
  }
 }
 if(!nextInventory.equals(expectedInventory))throw Error('Pending inventory is not derived from prior delta');
 const currentInventory=await workingBody(root,inventoryName);
 if(!currentInventory.equals(nextInventory)&&currentInventory.toString('utf8')!==transaction.old_inventory)throw Error('Pending inventory changed');
 const hasLocator=await exists(path.join(root,locator));const oldCurrent=hasLocator?(await workingBody(root,locator)).toString('utf8'):null;
 if(oldCurrent!==transaction.old_scope&&oldCurrent!==transaction.new_scope)throw Error('Pending scope changed');
 if(!encode(await prepareRows(root,nextInventory)).equals(nextScope))throw Error('Pending source changed');
 return {nextInventory,nextScope,currentInventory};
}
async function finish(root,transaction,deltaRaw,{pendingExists=true}={}){
 // This fixed-location, create-only active body selects the plan; mutable pending cannot select a new hash archive.
 const anchored=await workingBody(root,activeName);if(!anchored.equals(encode(transaction)))throw Error('Active prepared anchor differs');
 const archiveName=revisions+'/transitions/'+sha(anchored)+'.json';
 if(pendingExists||await exists(path.join(root,archiveName))){const prepared=await workingBody(root,archiveName);if(!prepared.equals(anchored))throw Error('Prepared immutable transition differs');}
 const {nextInventory,nextScope,currentInventory}=await validatePrepared(root,transaction,deltaRaw);
 await immutable(root,archiveName,anchored);
 await immutable(root,revisions+'/'+sha(nextScope)+'.json',nextScope);
 await immutable(root,revisions+'/inventories/'+sha(nextInventory)+'.json',nextInventory);
 if(transaction.old_scope!==null)await immutable(root,revisions+'/'+sha(Buffer.from(transaction.old_scope))+'.json',Buffer.from(transaction.old_scope));
 if(transaction.old_inventory!==null)await immutable(root,revisions+'/inventories/'+sha(Buffer.from(transaction.old_inventory))+'.json',Buffer.from(transaction.old_inventory));
 await immutable(root,revisions+'/transitions/'+sha(encode(transaction))+'.json',encode(transaction));
 if(!currentInventory.equals(nextInventory))await replaceCurrent(root,inventoryName,nextInventory);
 await replaceCurrent(root,locator,nextScope);
 if(pendingExists)await unlink(path.join(root,pendingName));
 await unlink(path.join(root,activeName));
 return {revision:revisions+'/'+sha(nextScope)+'.json',sha256:sha(nextScope),files:JSON.parse(nextScope).files.length,approved:false};
}
// Explicit refresh establishes new byte expectations, never approval. Checks never call this writer.
export async function refreshSourceScope(root,{init=false,deltaFile=null}={}){
 root=guardRoot(root);const deltaRaw=deltaFile?await dataFile(deltaFile):null;
 const pendingExists=await exists(path.join(root,pendingName)),activeExists=await exists(path.join(root,activeName));
 if(activeExists){
  if(init)throw Error('Active update requires explicit refresh');
  const anchored=await workingBody(root,activeName);
  if(pendingExists&&!(await workingBody(root,pendingName)).equals(anchored))throw Error('Pending differs from active prepared anchor');
  return finish(root,JSON.parse(anchored),deltaRaw,{pendingExists});
 }
 if(pendingExists)throw Error('Pending requires original active anchor');
 const inventoryBody=await workingBody(root,inventoryName);parseInventory(inventoryBody);
 let oldScope=null,oldInventory=inventoryBody,nextInventory=inventoryBody;
 if(init){
  if(deltaRaw||await exists(path.join(root,locator))||await exists(path.join(root,revisions)))throw Error('Fresh initialization required');
 }else{
  oldScope=await workingBody(root,locator);const old=parseScope(oldScope);
  const archived=await workingBody(root,revisions+'/'+sha(oldScope)+'.json');if(!archived.equals(oldScope))throw Error('Revision differs');
  const item=old.files.find(x=>x.path===inventoryName);if(!item)throw Error('Prior inventory required');
  oldInventory=await workingBody(root,revisions+'/inventories/'+item.sha256+'.json');if(oldInventory.length!==item.bytes||sha(oldInventory)!==item.sha256||!oldInventory.equals(inventoryBody))throw Error('Inventory edited outside explicit delta');
  const previous=parseInventory(oldInventory),delta=parseDelta(deltaRaw,oldScope,oldInventory);
  if(delta){
   for(const name of delta.removed){if(!previous.paths.includes(name)||await exists(path.join(root,name)))throw Error('Removal must be prior and physically absent');}
   for(const name of delta.added)if(previous.paths.includes(name))throw Error('Addition already inventoried');
   nextInventory=encode({format:previous.format,paths:[...previous.paths.filter(x=>!delta.removed.includes(x)),...delta.added].sort(compareNames)});
  }
 }
 const nextScope=encode(await prepareRows(root,nextInventory));
 if(!encode(await prepareRows(root,nextInventory)).equals(nextScope)||!(await workingBody(root,inventoryName)).equals(inventoryBody))throw Error('Source drift during refresh');
 if(oldScope?.equals(nextScope))return {revision:revisions+'/'+sha(oldScope)+'.json',sha256:sha(oldScope),files:JSON.parse(oldScope).files.length,approved:false,unchanged:true};
 const transaction={format:'loreviva.source-byte-transition.v1',old_scope:oldScope?.toString('utf8')??null,old_inventory:oldInventory.toString('utf8'),new_scope:nextScope.toString('utf8'),new_inventory:nextInventory.toString('utf8'),delta:deltaRaw?.toString('utf8')??null};
 await validatePrepared(root,transaction,deltaRaw);
 // Create the independent active identity before archives or pending, preserving recovery after either later write fails.
 await ensureDirectory(root,'.cache');await writeFile(path.join(root,activeName),encode(transaction),{flag:'wx'});
 await immutable(root,revisions+'/transitions/'+sha(encode(transaction))+'.json',encode(transaction));
 await ensureDirectory(root,'.cache');await writeFile(path.join(root,pendingName),encode(transaction),{flag:'wx'});return finish(root,transaction,deltaRaw);
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url)){
 const args=process.argv.slice(2);let options;
 if(!args.length)options={};else if(args.length===1&&args[0]==='--init')options={init:true};else if(args.length===2&&args[0]==='--delta')options={deltaFile:args[1]};else throw Error('Usage: refresh-source-byte-scope.mjs [--init|--delta FILE]');
 console.log(JSON.stringify(await refreshSourceScope(process.cwd(),options)));
}
