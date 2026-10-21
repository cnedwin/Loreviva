import {createServer,type IncomingMessage,type ServerResponse} from 'node:http';
import {createHash,randomBytes,timingSafeEqual} from 'node:crypto';
import {once} from 'node:events';
import {lstat,open} from 'node:fs/promises';
import {dirname,isAbsolute,parse,resolve} from 'node:path';
import {fileURLToPath,pathToFileURL} from 'node:url';
import {spawn} from 'node:child_process';
import {initializeReferenceJournal,openReferenceJournal,JournalOriginalUnavailable,type JournalOptions} from './command-journal.ts';
import {createPreparedIntegrationOwner,type Profile,type Input,type Operation,type BffSessionView} from '../../packages/sdk-ts/src/client.ts';
import {transportDescriptors} from '../../packages/sdk-ts/src/generated/transport.ts';
import type {ClientSession,ClientSessionRequest,PlayerSelfConsentSnapshot} from '../../packages/sdk-ts/src/generated/contracts.ts';
import type {AsyncDisposableEventStream} from '../../packages/sdk-ts/src/sse.ts';
import type {AuthorizedMediaBody} from '../../packages/sdk-ts/src/media.ts';

export interface ReferenceUser {
 readonly login:string;readonly credentialSHA256:string;
 readonly tenant:string;readonly product:string;readonly customer:string;readonly serviceSubject:string;
 readonly player:string;readonly externalSubject:string;readonly entitlement:string;readonly world:string;
 readonly profile:Profile;readonly requestedActions:ClientSessionRequest['requested_actions'];readonly serviceKey:string;
}
export interface ReferenceBffOptions {
 readonly platformOrigin:string;readonly port:number;readonly journal:JournalOptions;
 readonly users:readonly ReferenceUser[];
 readonly customer?:Readonly<{
  executable:Readonly<{path:string;bytes:number;sha256:string}>;
  databases:readonly Readonly<{customer:string;world:string;dsn:string}>[];
 }>;
}
type JsonRecord=Record<string,unknown>;
type Identity=readonly [string,string,string,string,string,string,Profile];
type SourceResult={status:string;protocol:string;headers:Readonly<Record<string,string>>;body:unknown};
const durable=['createInstance','controlInstance','createQuote','submitTurn','commandPlayerSelfConsent','governMemories','cancelTask'] as const;
const reads=['getIntegrationCapabilities','getPlayerSelfConsent','getSnapshot','getEvents','streamEvents','getTurn','getOriginalTurnIntakeReceipt','getTask','readRuntimeMedia','listMemories','getMemory'] as const;
const COOKIE='lv_reference_session',MAX_JSON=1048576;
const sha=(value:Uint8Array|string)=>createHash('sha256').update(value).digest('hex');
function record(value:unknown):JsonRecord{
 if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Object required');
 return value as JsonRecord;
}
function closed(value:unknown,keys:readonly string[]):JsonRecord{
 const r=record(value);if(Object.keys(r).length!==keys.length||keys.some(k=>!Object.hasOwn(r,k)))throw Error('Closed local fields required');return r;
}
function text(value:unknown,max=128):string{
 if(typeof value!=='string'||!value||value.length>max||/[\u0000-\u001f]/.test(value))throw Error('Invalid local text');return value;
}
function id(value:unknown){const s=text(value,64);if(!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(s))throw Error('Invalid identity');return s}
function equalSecret(a:string,b:string){const aa=Buffer.from(a),bb=Buffer.from(b);return aa.length===bb.length&&timingSafeEqual(aa,bb)}
class LocalError extends Error{
 readonly status:number;readonly code:string;readonly recovery:string;
 constructor(status:number,code:string,recovery='NONE'){super('The reference request could not be completed');this.status=status;this.code=code;this.recovery=recovery}
}
// JSON.parse alone loses duplicate fields. Check the original bounded grammar
// first; no caller object, getter, prototype or serialization callback is used.
function strictJSON(raw:string):unknown{
 let at=0;const white=()=>{while(/[ \t\r\n]/.test(raw[at]??'x'))at++};
 function string(){const begin=at;if(raw[at++]!=='"')throw Error('JSON string');
  for(;;){const c=raw[at++];if(c===undefined)throw Error('JSON end');if(c==='\\'){at++;continue}if(c==='"')return JSON.parse(raw.slice(begin,at)) as string}}
 function value(depth:number):void{
  if(depth>64)throw Error('JSON depth');white();const c=raw[at];
  if(c==='{'){at++;white();const seen=new Set<string>();if(raw[at]==='}'){at++;return}
   for(;;){white();const key=string();if(seen.has(key)||key==='__proto__'||key==='constructor')throw Error('Duplicate or unsafe JSON field');seen.add(key);
    white();if(raw[at++]!==':')throw Error('JSON colon');value(depth+1);white();const end=raw[at++];if(end==='}')return;if(end!==',')throw Error('JSON object')}
  }
  if(c==='['){at++;white();if(raw[at]===']'){at++;return}for(;;){value(depth+1);white();const end=raw[at++];if(end===']')return;if(end!==',')throw Error('JSON array')}}
  if(c==='"'){string();return}
  const found=/^(?:true|false|null|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)/.exec(raw.slice(at));if(!found)throw Error('JSON value');at+=found[0].length;
 }
 value(0);white();if(at!==raw.length)throw Error('Trailing JSON');return JSON.parse(raw);
}
async function localBody(req:IncomingMessage,maximum=MAX_JSON){
 if(req.headers['content-type']?.split(';')[0]?.trim().toLowerCase()!=='application/json')throw new LocalError(400,'LOCAL_INPUT');
 const chunks:Buffer[]=[];let size=0;
 for await(const chunk of req){size+=chunk.length;if(size>maximum)throw new LocalError(413,'LOCAL_INPUT');chunks.push(Buffer.from(chunk))}
 return strictJSON(new TextDecoder('utf-8',{fatal:true}).decode(Buffer.concat(chunks)));
}
function pairs(value:unknown){
 if(!Array.isArray(value)||value.length>16)throw new LocalError(400,'LOCAL_INPUT');
 const result:Record<string,string|number>=Object.create(null);
 for(const pair of value){if(!Array.isArray(pair)||pair.length!==2)throw new LocalError(400,'LOCAL_INPUT');
  const key=text(pair[0],64);if(Object.hasOwn(result,key))throw new LocalError(400,'LOCAL_INPUT');
  const value=pair[1];if(typeof value==='number'){if(!Number.isSafeInteger(value))throw new LocalError(400,'LOCAL_INPUT');result[key]=value}else result[key]=text(value,16384)}
 return result;
}
function userCopy(input:ReferenceUser):ReferenceUser{
 const u=closed(input,['login','credentialSHA256','tenant','product','customer','serviceSubject','player','externalSubject','entitlement','world','profile','requestedActions','serviceKey']);
 for(const key of ['tenant','product','customer','serviceSubject','player','externalSubject','world'])id(u[key]);
 text(u.login);text(u.entitlement,512);text(u.serviceKey,4096);
 if(typeof u.credentialSHA256!=='string'||!/^[a-f0-9]{64}$/.test(u.credentialSHA256)||!['public_v1','integration_r0_v1'].includes(String(u.profile)))throw Error('Invalid startup mapping');
 if(!Array.isArray(u.requestedActions)||!u.requestedActions.length||u.requestedActions.some(v=>!['READ','TURN','CONTROL_PAUSE','CONTROL_RESUME','MEMORY_VIEW','MEMORY_GOVERN'].includes(v))||new Set(u.requestedActions).size!==u.requestedActions.length)throw Error('Invalid startup actions');
 return Object.freeze({...input,requestedActions:Object.freeze([...input.requestedActions]) as unknown as ClientSessionRequest['requested_actions']});
}
function sameScope(value:unknown,u:ReferenceUser,world=true){
 const scope=record(value);if(scope.tenant_id!==u.tenant||scope.product_id!==u.product||(world&&scope.world_instance_id!==u.world))throw new LocalError(403,'CURRENT_IDENTITY');
}
function safeResponse(result:SourceResult,u:ReferenceUser,credentials:readonly string[],operation:string){
 if(result.protocol!=='JSON')return;
 const body=record(result.body);
 if(Number(result.status)<300){
  if(body.scope) sameScope(body.scope,u,operation!=='createInstance'&&operation!=='getPlayerSelfConsent');
  if(operation==='getPlayerSelfConsent'&&body.subject_id!==u.player)throw new LocalError(403,'CURRENT_IDENTITY');
  if(body.snapshot){const snapshot=record(body.snapshot);sameScope(snapshot.scope,u,false);if(snapshot.subject_id!==u.player)throw new LocalError(403,'CURRENT_IDENTITY')}
  if(body.task){const task=record(body.task);sameScope(task.scope,u)}
 }
 const bytes=JSON.stringify(body);if(credentials.some(value=>value.length>=8&&bytes.includes(value)))throw new LocalError(502,'UPSTREAM_PROTOCOL');
}

// The fixed built application is public code, never a general filesystem proxy.
// Copy its bounded regular files at startup; requests can select only this map.
async function noAlias(path:string){
 let part=resolve(path);const root=parse(part).root;
 for(;;){const s=await lstat(part);if(s.isSymbolicLink())throw Error('Alias refused');if(part===root)break;part=dirname(part)}
}
async function regularBytes(path:string,maximum:number){
 await noAlias(path);
 const f=await open(path,'r');
 try{
  const before=await f.stat({bigint:true});
  if(!before.isFile()||before.size<=0n||before.size>BigInt(maximum))throw Error('Invalid bounded file');
  const bytes=await f.readFile(),after=await f.stat({bigint:true}),named=await lstat(path,{bigint:true});
  if(before.dev!==after.dev||before.ino!==after.ino||before.size!==after.size||before.mtimeNs!==after.mtimeNs||
     named.isSymbolicLink()||named.dev!==after.dev||named.ino!==after.ino||bytes.length!==Number(after.size))throw Error('Changed bounded file');
  return bytes;
 }finally{await f.close()}
}
async function builtApplication(){
 const root=fileURLToPath(new URL('../../apps/reference-client/dist/',import.meta.url));
 const html=await regularBytes(resolve(root,'index.html'),65536);
 const source=new TextDecoder('utf-8',{fatal:true}).decode(html);
 if(!source.includes('<div id="app">'))throw Error('Built application missing');
 const assets=[...source.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map(m=>m[1]!);
 if(assets.length<2||assets.length>32||new Set(assets).size!==assets.length||
    assets.some(p=>!/^\/assets\/[A-Za-z0-9_-]+\.(js|css)$/.test(p)))throw Error('Invalid built asset manifest');
 const files=new Map<string,{bytes:Buffer;type:string}>([['/',{bytes:html,type:'text/html; charset=utf-8'}]]);
 let total=html.length;
 for(const path of assets){
  const bytes=await regularBytes(resolve(root,path.slice(1)),16*1024*1024);total+=bytes.length;
  if(total>24*1024*1024)throw Error('Built application cap');
  files.set(path,{bytes,type:path.endsWith('.js')?'text/javascript; charset=utf-8':'text/css; charset=utf-8'});
 }
 return files;
}

function customerCopy(value:ReferenceBffOptions['customer'],users:readonly ReferenceUser[]){
 if(value===undefined)return null;
 const input=closed(value,['executable','databases']),pin=closed(input.executable,['path','bytes','sha256']);
 if(typeof pin.path!=='string'||!isAbsolute(pin.path)||!pin.path.endsWith('.exe')||
    !Number.isSafeInteger(pin.bytes)||Number(pin.bytes)<=0||Number(pin.bytes)>256*1024*1024||
    typeof pin.sha256!=='string'||!/^[a-f0-9]{64}$/.test(pin.sha256))throw Error('Invalid fixed customer executable');
 if(!Array.isArray(input.databases)||input.databases.length===0||input.databases.length>16)throw Error('Customer mappings required');
 const seen=new Set<string>();
 const databases=input.databases.map(item=>{
  const v=closed(item,['customer','world','dsn']),customer=id(v.customer),world=id(v.world),dsn=text(v.dsn,8192),key=JSON.stringify([customer,world]);
  if(seen.has(key)||!users.some(u=>u.customer===customer&&u.world===world)||
     !/^[^:\s]+:[^\r\n]*@tcp\(127\.0\.0\.1:33267\)\/loreviva_test_[a-f0-9]{24}(?:\?[^#\r\n]*)?$/.test(dsn)||
     /^root:/i.test(dsn))throw Error('Invalid owned customer mapping');
  seen.add(key);return Object.freeze({customer,world,dsn});
 });
 return Object.freeze({executable:Object.freeze({path:pin.path,bytes:Number(pin.bytes),sha256:pin.sha256}),databases:Object.freeze(databases)});
}
async function customerOnce(runtime:NonNullable<ReturnType<typeof customerCopy>>,configuration:Record<string,string>,signal:AbortSignal){
 const pin=runtime.executable;
 await noAlias(pin.path);
 const held=await open(pin.path,'r'),before=await held.stat({bigint:true});
 try{
  if(!before.isFile()||before.size!==BigInt(pin.bytes))throw new LocalError(503,'LOCAL_CUSTOMER_UNAVAILABLE');
  const bytes=await held.readFile(),hashed=await held.stat({bigint:true});
  const named=await lstat(pin.path,{bigint:true});
  if(bytes.length!==pin.bytes||sha(bytes)!==pin.sha256||before.size!==hashed.size||before.mtimeNs!==hashed.mtimeNs||
     named.isSymbolicLink()||named.ino!==before.ino||named.dev!==before.dev||signal.aborted)throw new LocalError(503,'LOCAL_CUSTOMER_UNAVAILABLE');
  const env:NodeJS.ProcessEnv={PATH:''};
  for(const key of ['SystemRoot','TEMP','TMP'])if(process.env[key]!==undefined)env[key]=process.env[key];
  // The only child is the original fixed Go executable. No shell, environment
  // credential or caller-selected command/argument participates in this hop.
  const child=spawn(pin.path,[],{env,stdio:['pipe','pipe','pipe'],windowsHide:true});
  const output:Buffer[]=[];let total=0,diagnostic=0,interrupted=false;
  const stop=()=>{interrupted=true;if(child.exitCode===null)child.kill()};
  const timeout=setTimeout(stop,55000);timeout.unref();
  signal.addEventListener('abort',stop,{once:true});if(signal.aborted)stop();
  child.stdout.on('data',(b:Buffer)=>{total+=b.length;if(total>4096)stop();else output.push(Buffer.from(b))});
  child.stderr.on('data',(b:Buffer)=>{diagnostic+=b.length;if(diagnostic>4096)stop()});
  child.stdin.on('error',stop);
  let code:number|null;
  try{
   const exited=new Promise<number|null>((resolve,reject)=>{child.once('error',reject);child.once('close',resolve)});
   child.stdin.end(JSON.stringify(configuration));code=await exited;
  }finally{clearTimeout(timeout);signal.removeEventListener('abort',stop);child.stdin.destroy();child.stdout.destroy();child.stderr.destroy()}
  const after=await held.stat({bigint:true}),final=await lstat(pin.path,{bigint:true});
  if(interrupted||signal.aborted||diagnostic!==0||before.dev!==after.dev||before.ino!==after.ino||
     before.size!==after.size||before.mtimeNs!==after.mtimeNs||final.isSymbolicLink()||final.dev!==after.dev||final.ino!==after.ino)
   throw new LocalError(502,'LOCAL_CUSTOMER_UNKNOWN','QUERY_ORIGINAL_TASK');
  const raw=new TextDecoder('utf-8',{fatal:true}).decode(Buffer.concat(output));
  const result=closed(strictJSON(raw),['State','Persisted','Confirmed','Reconciled','Retried','OriginalActiveAtQuery']);
  if(!['CONFIRMED','DRAIN_LIMIT','REJECTED','UNKNOWN'].includes(String(result.State))||
     !Number.isSafeInteger(result.Persisted)||Number(result.Persisted)<0||
     !Number.isSafeInteger(result.Confirmed)||Number(result.Confirmed)<0||
     ['Reconciled','Retried','OriginalActiveAtQuery'].some(k=>typeof result[k]!=='boolean')||
     code!==(['REJECTED','UNKNOWN'].includes(String(result.State))?1:0))throw new LocalError(502,'LOCAL_CUSTOMER_UNKNOWN','QUERY_ORIGINAL_TASK');
  return {format:'lv-bff-customer-result-v1',...result};
 }finally{await held.close()}
}

/** Root-owned composition. HTTP ingress cannot replace providers, journal or send callbacks. */
export async function startReferenceBff(input:ReferenceBffOptions){
 closed(input,['platformOrigin','port','journal','users',...(Object.hasOwn(input,'customer')?['customer']:[])]);
 const platform=new URL(input.platformOrigin);
 if(platform.protocol!=='http:'||platform.hostname!=='127.0.0.1'||platform.username||platform.password||platform.pathname!=='/'||platform.search||platform.hash)throw Error('The R0 reference platform must be the fixed loopback origin');
 if(!Number.isInteger(input.port)||input.port<0||input.port>65535)throw Error('Invalid listening port');
 if(!Array.isArray(input.users)||!input.users.length||input.users.length>16)throw Error('Bounded mappings required');
 const users=input.users.map(userCopy);if(new Set(users.map(u=>u.login)).size!==users.length)throw Error('Duplicate login');
 const customerRuntime=customerCopy(input.customer,users);let customerBusy=false;
 const application=await builtApplication();
 const installation=input.journal.installation,journal=await openReferenceJournal(input.journal),broker=journal.bindBroker();
 let stopped=false,origin='';
 interface Session{
  cookie:string;csrf:string;user:ReferenceUser;generation:number;live:boolean;client:ClientSession|null;
  exchanged:Set<string>;requests:Set<AbortController>;service:ReturnType<typeof createPreparedIntegrationOwner<Profile,'SERVICE'>>;
  clientOwner:ReturnType<typeof createPreparedIntegrationOwner<Profile,'CLIENT'>>;
 }
 const sessions=new Map<string,Session>(),operations=new Set<Promise<void>>(),allControllers=new Set<AbortController>();
 function live(s:Session){return !stopped&&s.live&&sessions.get(s.cookie)===s}
 function current(s:Session,generation:number,signal:AbortSignal){if(!live(s)||s.generation!==generation||signal.aborted)throw new LocalError(401,'LOCAL_SESSION','AUTHENTICATE')}
 function credentials(s:Session){return [s.user.serviceKey,s.cookie,s.csrf,s.client?.access_token??'']}
 function identity(s:Session):Identity{const u=s.user;return [installation,u.tenant,u.product,u.customer,u.player,u.world,u.profile]}
 async function invalidate(s:Session,except?:AbortController){
  s.live=false;s.generation++;sessions.delete(s.cookie);s.client=null;
  for(const c of s.requests)if(c!==except)c.abort();
  await Promise.allSettled([s.service.close(),s.clientOwner.close()]);
 }
 function makeSession(u:ReferenceUser){
  const s={} as Session;
  Object.assign(s,{cookie:randomBytes(32).toString('hex'),csrf:randomBytes(32).toString('hex'),user:u,generation:1,live:true,client:null,exchanged:new Set(),requests:new Set()});
  s.service=createPreparedIntegrationOwner({baseUrl:platform.origin,profile:u.profile,role:'SERVICE',getBearer:()=>live(s)?u.serviceKey:null,current:()=>live(s)});
  s.clientOwner=createPreparedIntegrationOwner({baseUrl:platform.origin,profile:u.profile,role:'CLIENT',getBearer:()=>live(s)&&s.client&&Date.parse(s.client.expires_at)>Date.now()?s.client.access_token:null,current:()=>live(s)&&!!s.client&&Date.parse(s.client.expires_at)>Date.now()});
  sessions.set(s.cookie,s);return s;
 }
 async function callClient(s:Session,operation:string,parts:unknown,signal:AbortSignal):Promise<SourceResult>{
  const token=await s.clientOwner.prepare(operation as never,parts as never);return await s.clientOwner.sendPreparedOnce(token,signal) as SourceResult;
 }
 async function currentSelf(s:Session,generation:number,signal:AbortSignal){
  current(s,generation,signal);
  if(!s.client||Date.parse(s.client.expires_at)<=Date.now())throw new LocalError(401,'LOCAL_SESSION','AUTHENTICATE');
  const result=await callClient(s,'getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':s.user.profile}},signal);
  current(s,generation,signal);
  if(result.status!=='200'||result.protocol!=='JSON')throw new LocalError(401,'CURRENT_IDENTITY','AUTHENTICATE');
  const body=result.body as PlayerSelfConsentSnapshot;sameScope(body.scope,s.user,false);
  if(body.subject_id!==s.user.player)throw new LocalError(403,'CURRENT_IDENTITY');
  // REVOKED/NOT_GRANTED is not converted into a positive-use requirement for
  // the owner's necessary correction/deletion or original observation.
 }
 function view(s:Session):BffSessionView{
  return {format:'lv-bff-session-v1',authenticated:true,identity:[s.user.customer,s.user.player,s.user.world,s.user.profile],csrf:s.csrf,
   client:s.client?{subject:s.client.subject_id,world:s.client.scope.world_instance_id,profile:s.user.profile,expires_at:s.client.expires_at,view_epoch:s.client.view_epoch}:null};
 }
 function authenticate(req:IncomingMessage):Session{
  const cookies=(req.headers.cookie??'').split(';').map(v=>v.trim()).filter(Boolean);
  if(cookies.length!==1||!cookies[0]!.startsWith(COOKIE+'='))throw new LocalError(401,'LOCAL_SESSION','AUTHENTICATE');
  const cookie=cookies[0]!.slice(COOKIE.length+1),s=sessions.get(cookie);
  if(!s||!live(s))throw new LocalError(401,'LOCAL_SESSION','AUTHENTICATE');return s;
 }
 function envelope(req:IncomingMessage){
  const checked=['host','origin','cookie','content-type','content-length','x-loreviva-csrf','authorization'];
  for(const key of checked)if(req.rawHeaders.filter((_,i)=>i%2===0).filter(k=>k.toLowerCase()===key).length>1)throw new LocalError(400,'LOCAL_INPUT');
  if(req.headers.host!==new URL(origin).host||req.headers.authorization!==undefined||req.headers['sec-fetch-site']==='cross-site')throw new LocalError(403,'LOCAL_ORIGIN');
  if(req.method!=='GET'&&req.headers.origin!==origin)throw new LocalError(403,'LOCAL_ORIGIN');
  if(req.headers.origin!==undefined&&req.headers.origin!==origin)throw new LocalError(403,'LOCAL_ORIGIN');
 }
 function json(res:ServerResponse,status:number,value:unknown,headers:Record<string,string>={}){
  const bytes=Buffer.from(JSON.stringify(value));if(bytes.length>MAX_JSON)throw new LocalError(502,'UPSTREAM_PROTOCOL');
  res.writeHead(status,{'content-type':'application/json','content-length':String(bytes.length),'cache-control':'no-store',...headers});res.end(bytes);
 }
 async function output(res:ServerResponse,result:SourceResult,s:Session,generation:number,signal:AbortSignal,operation:string){
  current(s,generation,signal);safeResponse(result,s.user,credentials(s),operation);
  const headers:Record<string,string>={'cache-control':'no-store'};
  for(const name of ['content-type','content-range','accept-ranges','retry-after','loreviva-client-contract']){
   const value=result.headers[name];if(value!==undefined)headers[name]=value;
  }
  if(result.protocol==='JSON'){json(res,Number(result.status),result.body,headers);return}
  if(result.protocol==='BINARY'){
   const resource=result.body as AuthorizedMediaBody;
   try{const bytes=await resource.read();current(s,generation,signal);
    res.writeHead(Number(result.status),{...headers,'content-length':String(bytes.length)});res.end(bytes);
   }finally{await resource.close()}return;
  }
  const stream=result.body as AsyncDisposableEventStream;let count=0;
  try{
   for await(const frame of stream){
    current(s,generation,signal);
    safeResponse({...result,protocol:'JSON',body:frame.data},s.user,credentials(s),operation);
    const payload=Buffer.from(`event: ${frame.event}\nid: ${frame.id}\ndata: ${JSON.stringify(frame.data)}\n\n`);
    count+=payload.length;if(count>8*1024*1024||payload.length>256*1024)throw new LocalError(502,'UPSTREAM_PROTOCOL');
    if(!res.headersSent)res.writeHead(Number(result.status),headers);
    if(!res.write(payload))await once(res,'drain',{signal});
    current(s,generation,signal);stream.accept(frame);
   }
   current(s,generation,signal);if(!res.headersSent)res.writeHead(Number(result.status),headers);res.end();
  }finally{await stream.close()}
 }
 function mappedInput(s:Session,v:JsonRecord,operation:string){
  const u=s.user,path=record(v.path),query=pairs(v.query),body=v.body;
  if(v.profile!==u.profile)throw new LocalError(403,'CURRENT_IDENTITY');
  if(path.instance_id!==undefined&&path.instance_id!==u.world)throw new LocalError(403,'CURRENT_IDENTITY');
  if(body!==undefined){
   const b=record(body);
   if(b.scope)sameScope(b.scope,u);
   if(b.product_id!==undefined&&b.product_id!==u.product)throw new LocalError(403,'CURRENT_IDENTITY');
   if(operation==='createInstance'&&(!Array.isArray(b.initial_participants)||b.initial_participants.length!==1||b.initial_participants[0]!==u.player||b.mode!=='PRIVATE'))throw new LocalError(403,'CURRENT_IDENTITY');
  }
  const headers:Record<string,string>={'Loreviva-Client-Contract':u.profile};
  if(v.original_key!==null&&v.original_key!==undefined)headers['Idempotency-Key']=text(v.original_key);
  if(v.range!==null&&v.range!==undefined){if(operation!=='readRuntimeMedia')throw new LocalError(400,'LOCAL_INPUT');headers.Range=text(v.range,128)}
  return {...(Object.keys(path).length?{path}:{}),...(Object.keys(query).length?{query}:{}),headers,...(body!==undefined?{body}:{})};
 }
 async function command(s:Session,v:JsonRecord,generation:number,signal:AbortSignal):Promise<SourceResult>{
  const operation=text(v.operation);if(!(durable as readonly string[]).includes(operation))throw new LocalError(400,'LOCAL_INPUT');
  if(v.format!=='lv-bff-command-v1')throw new LocalError(400,'LOCAL_INPUT');
  const body=record(v.body),role=operation==='createInstance'||operation==='createQuote'||(operation==='controlInstance'&&['START','END'].includes(String(body.command)))?'SERVICE':'CLIENT';
  if(role==='CLIENT')await currentSelf(s,generation,signal);else current(s,generation,signal);
  const parts=mappedInput(s,v,operation),owner=role==='SERVICE'?s.service:s.clientOwner;
  const prepared=await owner.prepare(operation as never,parts as never),snapshot=owner.inspect(prepared),who=identity(s);
  const stored=await broker.commitPrepared(snapshot,who);
  current(s,generation,signal);
  const final=owner.inspect(prepared);
  if(final.bodySHA256!==sha(final.body)||final.path!==snapshot.path||final.bodySHA256!==snapshot.bodySHA256||final.idempotencyKey!==snapshot.idempotencyKey)throw new LocalError(409,'LOCAL_ORIGINAL_UNKNOWN','QUERY_ORIGINAL_TASK');
  broker.consumeFirstSendPermit(stored.permit,final,who);
  // Both capabilities consume synchronously. There is no await, re-mapping or
  // second serialization between this permit check and the fixed source send.
  const sent=owner.sendPreparedOnce(prepared,signal);
  try{
   const result=await sent as SourceResult;current(s,generation,signal);
   safeResponse(result,s.user,credentials(s),operation);
   const response=record(result.body),task=response.task?record(response.task):response;
   const taskId=typeof task.task_id==='string'?id(task.task_id):null;
   await journal.observe(stored.originalId,who,{kind:'RECEIPT',status:result.status,body:result.body,taskId});
   current(s,generation,signal);return result;
  }catch{
   await journal.observe(stored.originalId,who,{kind:'UNKNOWN',status:null,body:null,taskId:null}).catch(()=>{});
   throw new LocalError(502,'LOCAL_ORIGINAL_UNKNOWN','QUERY_ORIGINAL_TASK');
  }
 }
 async function handle(req:IncomingMessage,res:ServerResponse,controller:AbortController){
  const signal=controller.signal;let session:Session|undefined;
  const disconnected=()=>controller.abort();req.once('aborted',disconnected);
  const closedResponse=()=>{if(!res.writableFinished)controller.abort()};res.once('close',closedResponse);
  const timer=setTimeout(()=>controller.abort(),60000);timer.unref();
  try{
   if(stopped)throw new LocalError(503,'LOCAL_CLOSED');envelope(req);
   const url=new URL(req.url??'/',origin);
   if(url.origin!==origin||url.hash)throw new LocalError(400,'LOCAL_INPUT');
   if(!url.pathname.startsWith('/bff/')){
    const file=application.get(url.pathname);
    if(req.method!=='GET'||url.search||!file)throw new LocalError(404,'LOCAL_ROUTE');
    res.writeHead(200,{'content-type':file.type,'content-length':String(file.bytes.length),'cache-control':'no-store',
     'x-content-type-options':'nosniff','referrer-policy':'no-referrer',
     'content-security-policy':"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; media-src blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"});
    res.end(file.bytes);return;
   }
   if(req.method==='POST'&&url.pathname==='/bff/login'){
    if(url.search)throw new LocalError(400,'LOCAL_INPUT');
    const v=closed(await localBody(req),['format','login','credential']);
    if(v.format!=='lv-bff-login-v1')throw new LocalError(400,'LOCAL_INPUT');
    const login=text(v.login),credential=text(v.credential,4096),u=users.find(x=>x.login===login);
    if(!u||!equalSecret(sha(credential),u.credentialSHA256))throw new LocalError(401,'LOCAL_SESSION','AUTHENTICATE');
    if(sessions.size>=16)throw new LocalError(429,'LOCAL_CAPACITY');
    try{const old=authenticate(req);await invalidate(old)}catch(e){if(!(e instanceof LocalError)||e.status!==401)throw e}
    const s=makeSession(u);json(res,200,view(s),{'set-cookie':`${COOKIE}=${s.cookie}; HttpOnly; SameSite=Strict; Path=/bff`});return;
   }
   session=authenticate(req);const s=session,generation=s.generation;s.requests.add(controller);
   if(req.method!=='GET'&&!equalSecret(String(req.headers['x-loreviva-csrf']??''),s.csrf))throw new LocalError(403,'LOCAL_CSRF');
   if(req.method==='GET'&&url.pathname==='/bff/me'){
    if(url.search)throw new LocalError(400,'LOCAL_INPUT');
    if(s.client)await currentSelf(s,generation,signal);json(res,200,view(s));return;
   }
   if(req.method==='POST'&&url.pathname==='/bff/logout'){
    const v=closed(await localBody(req),['format']);if(v.format!=='lv-bff-logout-v1'||url.search)throw new LocalError(400,'LOCAL_INPUT');
    await invalidate(s,controller);res.writeHead(204,{'cache-control':'no-store','set-cookie':`${COOKIE}=; HttpOnly; SameSite=Strict; Path=/bff; Max-Age=0`});res.end();return;
   }
   if(req.method==='POST'&&url.pathname==='/bff/exchange'){
    const v=closed(await localBody(req),['format','original_key']);if(v.format!=='lv-bff-exchange-v1'||url.search)throw new LocalError(400,'LOCAL_INPUT');
    const key=text(v.original_key);if(s.exchanged.has(key))throw new LocalError(409,'LOCAL_EXCHANGE_UNKNOWN','AUTHENTICATE');
    if(s.exchanged.size>=32)throw new LocalError(429,'LOCAL_CAPACITY');
    s.exchanged.add(key);
    const u=s.user,body:ClientSessionRequest={product_id:u.product,external_subject_id:u.externalSubject,entitlement_reference:u.entitlement,world_instance_id:u.world,requested_actions:u.requestedActions};
    const parts:Input<Profile,'exchangeClientSession'>=u.profile==='public_v1'
     ?{headers:{'Loreviva-Client-Contract':'public_v1','Idempotency-Key':key},body}
     :{headers:{'Loreviva-Client-Contract':'integration_r0_v1','Idempotency-Key':key},body};
    try{
     const prepared=await s.service.prepare('exchangeClientSession',parts),result=await s.service.sendPreparedOnce(prepared,signal);
     current(s,generation,signal);if(result.status!=='201')throw Error('Exchange refused');
     const client=result.body as ClientSession;sameScope(client.scope,u);
     if(client.subject_id!==u.player||!client.access_token||Date.parse(client.expires_at)<=Date.now()||client.allowed_actions.some(a=>!u.requestedActions.includes(a as never)))throw Error('Exchange identity');
     s.generation++;for(const c of s.requests)if(c!==controller)c.abort();
     s.client=Object.freeze({...client,scope:Object.freeze({...client.scope}),allowed_actions:[client.allowed_actions[0],...client.allowed_actions.slice(1)] as ClientSession['allowed_actions']});
     json(res,200,view(s));return;
    }catch{throw new LocalError(502,'LOCAL_EXCHANGE_UNKNOWN','AUTHENTICATE')}
   }
   if(req.method==='POST'&&url.pathname==='/bff/commands'){
    if(url.search)throw new LocalError(400,'LOCAL_INPUT');
    const v=closed(await localBody(req),['format','operation','profile','original_key','path','query','body']);
    const result=await command(s,v,generation,signal);await output(res,result,s,generation,signal,String(v.operation));return;
   }
   if(req.method==='POST'&&url.pathname==='/bff/customer-consume'){
    const v=closed(await localBody(req),['format']);if(v.format!=='lv-bff-consume-v1'||url.search)throw new LocalError(400,'LOCAL_INPUT');
    await currentSelf(s,generation,signal);
    const database=customerRuntime?.databases.find(d=>d.customer===s.user.customer&&d.world===s.user.world);
    if(!customerRuntime||!database)throw new LocalError(503,'LOCAL_CUSTOMER_UNAVAILABLE');
    if(customerBusy)throw new LocalError(409,'LOCAL_CUSTOMER_UNKNOWN','QUERY_ORIGINAL_TASK');
    customerBusy=true;
    try{
     const result=await customerOnce(customerRuntime,{DSN:database.dsn,BaseURL:platform.origin,ServiceToken:s.user.serviceKey,
      Installation:installation,Customer:s.user.customer,World:s.user.world,Mode:'once',Fault:''},signal);
     await currentSelf(s,generation,signal);current(s,generation,signal);json(res,200,result);
    }finally{customerBusy=false}
    return;
   }
   if(req.method==='GET'&&url.pathname.startsWith('/bff/client/')){
    const operation=url.pathname.slice('/bff/client/'.length);if(!(reads as readonly string[]).includes(operation))throw new LocalError(404,'LOCAL_ROUTE');
    if(url.search.length>16384||[...url.searchParams.keys()].join()!=='args')throw new LocalError(400,'LOCAL_INPUT');
    const v=closed(strictJSON(url.searchParams.get('args')??''),['format','profile','path','query','range','original_key']);
    if(v.format!=='lv-bff-read-v1')throw new LocalError(400,'LOCAL_INPUT');
    await currentSelf(s,generation,signal);
    const result=await callClient(s,operation,mappedInput(s,v,operation),signal);await output(res,result,s,generation,signal,operation);return;
   }
   if(req.method==='POST'&&url.pathname==='/bff/find-original'){
    if(url.search)throw new LocalError(400,'LOCAL_INPUT');
    let v:JsonRecord;
    try{
     v=closed(await localBody(req,16384),['format','operation','profile','original_key']);
     if(v.format!=='lv-bff-find-original-v1'||typeof v.operation!=='string'||!(durable as readonly string[]).includes(v.operation)||
        typeof v.original_key!=='string'||!/^[A-Za-z0-9._-]{1,128}$/.test(v.original_key))throw new LocalError(400,'LOCAL_INPUT');
    }catch(error){if(error instanceof LocalError)throw error;throw new LocalError(400,'LOCAL_INPUT')}
    if(v.profile!==s.user.profile)throw new LocalError(403,'CURRENT_IDENTITY');
    await currentSelf(s,generation,signal);
    const who=identity(s),originalId=journal.findOriginal(who,v.operation as string,v.original_key as string);
    if(originalId===null)throw new LocalError(404,'ORIGINAL_NOT_FOUND','QUERY_ORIGINAL_TASK');
    const final=journal.original(originalId,who);current(s,generation,signal);
    json(res,200,{format:'lv-bff-original-v1',original_id:originalId,operation:final.operation,state:final.state,task_id:final.task_id,observation:final.observation});return;
   }
   const originalMatch=/^\/bff\/(originals|query-original)\/([a-f0-9]{32})$/.exec(url.pathname);
   if(originalMatch&&((originalMatch[1]==='originals'&&req.method==='GET')||(originalMatch[1]==='query-original'&&req.method==='POST'))){
    if(url.search)throw new LocalError(400,'LOCAL_INPUT');
    if(req.method==='POST'){const v=closed(await localBody(req),['format']);if(v.format!=='lv-bff-query-v1')throw new LocalError(400,'LOCAL_INPUT')}
    await currentSelf(s,generation,signal);const who=identity(s),originalId=originalMatch[2]!,saved=journal.original(originalId,who);
    let retryAfter:string|undefined;
    if(req.method==='POST'){
     let result:SourceResult|null=null,observationKind:'QUERY'|'TASK'='QUERY';
     if(saved.operation==='submitTurn')result=await callClient(s,'getOriginalTurnIntakeReceipt',{path:{instance_id:s.user.world},headers:{'Idempotency-Key':saved.original_key,'Loreviva-Client-Contract':s.user.profile}},signal);
     else if(saved.task_id&&saved.task_access==='CLIENT'){result=await callClient(s,'getTask',{path:{task_id:saved.task_id},headers:{'Loreviva-Client-Contract':s.user.profile}},signal);observationKind=result.status==='200'?'TASK':'QUERY'}
     if(result){
      retryAfter=result.headers['retry-after'];
      current(s,generation,signal);safeResponse(result,s.user,credentials(s),saved.operation);
      const value=record(result.body),acceptance=value.acceptance?record(value.acceptance):value;
      const task=acceptance.task?record(acceptance.task):acceptance,taskId=typeof task.task_id==='string'?id(task.task_id):saved.task_id;
      if(saved.task_id&&taskId!==saved.task_id)throw new LocalError(502,'UPSTREAM_PROTOCOL');
      await journal.observe(originalId,who,{kind:observationKind,status:result.status,body:result.body,taskId});
     }
    }
    current(s,generation,signal);const final=journal.original(originalId,who);
    json(res,200,{format:'lv-bff-original-v1',original_id:originalId,operation:final.operation,state:final.state,task_id:final.task_id,observation:final.observation},retryAfter===undefined?{}:{'retry-after':retryAfter});return;
   }
   throw new LocalError(404,'LOCAL_ROUTE');
  }catch(error){
   if(res.headersSent){res.destroy();return}
   const e=error instanceof LocalError?error:error instanceof JournalOriginalUnavailable?new LocalError(404,'ORIGINAL_NOT_FOUND','QUERY_ORIGINAL_TASK'):new LocalError(502,'UPSTREAM_PROTOCOL');
   json(res,e.status,{format:'lv-bff-error-v1',code:e.code,message:e.message,retryable:false,recovery_action:e.recovery},{'x-loreviva-bff-error':'1'});
  }finally{clearTimeout(timer);req.off('aborted',disconnected);res.off('close',closedResponse);session?.requests.delete(controller);allControllers.delete(controller)}
 }
 const server=createServer((req,res)=>{
  const controller=new AbortController();allControllers.add(controller);
  const operation=handle(req,res,controller).catch(()=>{res.destroy()}).finally(()=>{operations.delete(operation)});operations.add(operation);
 });
 server.maxHeadersCount=32;server.headersTimeout=10000;server.requestTimeout=60000;server.keepAliveTimeout=1000;
 try{
  await new Promise<void>((resolve,reject)=>{server.once('error',reject);server.listen(input.port,'127.0.0.1',()=>{server.off('error',reject);resolve()})});
  const address=server.address();if(!address||typeof address==='string')throw Error('Loopback listener missing');origin=`http://127.0.0.1:${address.port}`;
 }catch(error){await journal.close();throw error}
 return Object.freeze({origin,
  accounting(){return Object.freeze({activeRequests:operations.size,sessions:sessions.size,stopped})},
  async close(){
   if(stopped)return;stopped=true;
   for(const c of allControllers)c.abort();
   await Promise.allSettled([...sessions.values()].map(s=>invalidate(s)));
   await Promise.allSettled([...operations]);
   server.closeAllConnections();await new Promise<void>((resolve,reject)=>server.close(e=>e?reject(e):resolve()));
   await journal.close();
  }
 });
}

async function main(){
 if(process.argv.length!==3||!['initialize','serve'].includes(process.argv[2]!))throw Error('Closed startup action');
 const input=process.stdin[Symbol.asyncIterator]();let pending=Buffer.alloc(0),first:unknown;
 const startupTimeout=setTimeout(()=>process.stdin.destroy(Error('Startup input timeout')),10000);
 try{
  for(;;){
   const line=pending.indexOf(10);
   if(line>=0){
    first=strictJSON(new TextDecoder('utf-8',{fatal:true}).decode(pending.subarray(0,line)));
    pending=pending.subarray(line+1);break;
   }
   const chunk=await input.next();if(chunk.done)throw Error('Startup line required');
   pending=Buffer.concat([pending,Buffer.from(chunk.value)]);if(pending.length>MAX_JSON)throw Error('Startup input cap');
  }
 }finally{clearTimeout(startupTimeout)}
 if(process.argv[2]==='initialize'){
  if(pending.length)throw Error('Unexpected initialization command');
  await initializeReferenceJournal(first as JournalOptions);
  process.stdout.write('{"format":"lv-reference-startup-v1","state":"INITIALIZED"}\n');
  process.stdin.destroy();return;
 }
 const options=first as ReferenceBffOptions;
 if(options?.port!==27185)throw Error('Primary BFF has a fixed loopback port');
 const app=await startReferenceBff(options);let stopping=false;
 const stop=()=>{stopping=true;process.stdin.destroy()};
 process.once('SIGINT',stop);process.once('SIGTERM',stop);
 try{
  process.stdout.write(JSON.stringify({format:'lv-reference-startup-v1',state:'READY',origin:app.origin,pid:process.pid})+'\n');
  for(;;){
   if(pending.length){
    if(pending.length>5||!Buffer.from('STOP\n').subarray(0,pending.length).equals(pending))throw Error('Closed lifetime command');
    if(pending.length===5)break;
   }
   const chunk=await input.next();if(chunk.done)break;
   pending=Buffer.concat([pending,Buffer.from(chunk.value)]);
  }
 }catch(error){if(!stopping)throw error}
 finally{
  await app.close();process.off('SIGINT',stop);process.off('SIGTERM',stop);process.stdin.destroy();
  process.stdout.write('{"format":"lv-reference-startup-v1","state":"CLOSED"}\n');
 }
}
if(process.argv[1]&&import.meta.url===pathToFileURL(resolve(process.argv[1])).href){
 main().catch(()=>{process.stderr.write('Reference backend stopped; inspect the owned private result.\n');process.exitCode=1;process.stdin.destroy()});
}
