import validators from './generated/validators.cjs';
import {transportDescriptors, type OperationRoles, type ResponseProtocols, type MultipartBodies} from './generated/transport.ts';
import type {ProfileOperations, ProjectionResponses} from './generated/contracts.ts';
import {createEventStream, type AsyncDisposableEventStream} from './sse.ts';
import {createAuthorizedMediaBody, type AuthorizedMediaBody} from './media.ts';

export type Profile = keyof ProfileOperations;
export type Operation<P extends Profile> = keyof ProfileOperations[P] & keyof OperationRoles;
export type CredentialRole = OperationRoles[keyof OperationRoles];
type SourceInput<P extends Profile,O extends Operation<P>> = ProfileOperations[P][O] extends {input:infer I}?I:never;
export type Input<P extends Profile,O extends Operation<P>> = O extends keyof MultipartBodies
 ? Omit<SourceInput<P,O>,'body'> & {body:MultipartBodies[O]} : SourceInput<P,O>;
type Responses<P extends Profile,O extends Operation<P>> = ProfileOperations[P][O] extends {responses:infer V}?V:never;
type RoleOperation<R extends CredentialRole> = {
 [O in keyof OperationRoles]:[R] extends [OperationRoles[O]]?O:never
}[keyof OperationRoles];
export type OperationsForRole<P extends Profile,R extends CredentialRole> = Operation<P> & RoleOperation<R>;
type WithoutProjection<I> = I extends unknown ? Omit<I,'query'> & {query?:I extends {query?:infer Q}?Omit<Q,'include'> & {include?:never}:never}:never;
type DefaultInput<P extends Profile,O extends Operation<P>> = O extends keyof ProjectionResponses?WithoutProjection<Input<P,O>>:Input<P,O>;
type Protocol<P extends Profile,O extends Operation<P>,S> = O extends keyof ResponseProtocols[P]
 ? S extends keyof ResponseProtocols[P][O]?ResponseProtocols[P][O][S]:never:never;
export type TransportResult<P extends Profile,O extends Operation<P>,I=Input<P,O>> = {
 [S in keyof Responses<P,O>]:Readonly<{status:S;protocol:Protocol<P,O,S>;headers:Readonly<Record<string,string>>;
 body:Protocol<P,O,S> extends 'BINARY'?AuthorizedMediaBody:
 Protocol<P,O,S> extends 'SSE'?AsyncDisposableEventStream:
 I extends {query:{include:'provenance_v1'}}?S extends '200'?O extends keyof ProjectionResponses
 ?ProjectionResponses[O]['provenance_v1']:Responses<P,O>[S]:Responses<P,O>[S]:Responses<P,O>[S]}>
}[keyof Responses<P,O>];
export interface TransportOptions<P extends Profile,R extends CredentialRole> {
 baseUrl:string;profile:P;role:R;getBearer:()=>string|null;
 /** Local invalidation only; the server still performs every current permission check. */
 current?:()=>boolean;
}
declare const preparedBrand:unique symbol;
export interface PreparedIntegrationRequest<P extends Profile,O extends Operation<P>> {
 readonly [preparedBrand]:{profile:P;operation:O};
}
export interface PreparedRequestSnapshot {
 readonly operation:string;readonly profile:Profile;readonly role:CredentialRole;
 readonly method:string;readonly path:string;readonly idempotencyKey:string|null;
 readonly contentType:string|null;readonly body:Uint8Array;readonly bodySHA256:string;
}
export class IntegrationResponseError extends Error {
 readonly status:string;readonly body:unknown;readonly headers:Readonly<Record<string,string>>;
 constructor(status:string,body:unknown,headers:Readonly<Record<string,string>>){
  super('The authorized resource was not returned');this.name='IntegrationResponseError';this.status=status;this.body=body;this.headers=headers;
 }
}
interface ResponseDescriptor {mime:string;protocol:string;validator:string|null;sseData:string|null;sseFrame:string|null}
interface Descriptor {
 method:string;path:string;roles:readonly string[];
 parameters:readonly {name:string;in:string;required:boolean}[];
 requestMime:string|null;bodyRequired:boolean;inputValidator:string;
 responses:Readonly<Record<string,ResponseDescriptor>>;projection?:{validator?:string}|null;
}
interface Parts {path?:Record<string,string>;query?:Record<string,unknown>;headers?:Record<string,string>;body?:unknown}
interface Sealed {
 operation:string;descriptor:Descriptor;parts:Parts;method:string;path:string;
 body:Uint8Array<ArrayBuffer>;bodySHA256:string;contentType:string|null;used:boolean;
}
const encoder=new TextEncoder();
const MAX_JSON=1024*1024;
const validation=validators as unknown as Record<string,((value:unknown)=>boolean)|undefined>;
function validate(name:string,value:unknown):void {
 if(!Object.hasOwn(validation,name)||!validation[name]?.(value))throw Error('Contract validation failed');
}
// Only inert values are serializable input. Accessors and toJSON cannot mutate a checked request.
function copy(value:unknown,seen=new Set<object>()):unknown {
 if(value===null||typeof value==='string'||typeof value==='boolean')return value;
 if(typeof value==='number'&&Number.isFinite(value))return value;
 if(value instanceof Blob)return value;
 if(typeof value!=='object'||!value||seen.has(value))throw Error('Non-data request input');
 const proto=Object.getPrototypeOf(value);
 if(!Array.isArray(value)&&proto!==Object.prototype&&proto!==null)throw Error('Non-data request prototype');
 seen.add(value);
 const out:Record<string,unknown>|unknown[]=Array.isArray(value)?[]:Object.create(null);
 for(const key of Reflect.ownKeys(value)){
  if(Array.isArray(value)&&key==='length')continue;
  if(typeof key!=='string'||key==='__proto__'||key==='constructor'||key==='toJSON')throw Error('Invalid request property');
  const d=Object.getOwnPropertyDescriptor(value,key)!;
  if(!d.enumerable||!('value' in d))throw Error('Request accessors are forbidden');
  (out as Record<string,unknown>)[key]=copy(d.value,seen);
 }
 seen.delete(value);return out;
}
async function sha(bytes:Uint8Array<ArrayBuffer>):Promise<string> {
 return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',bytes)),x=>x.toString(16).padStart(2,'0')).join('');
}
async function jsonBody(response:Response,signal:AbortSignal):Promise<unknown> {
 const length=response.headers.get('content-length');
 if(length!==null&&(!/^(0|[1-9][0-9]*)$/.test(length)||BigInt(length)>BigInt(MAX_JSON)))throw Error('Invalid JSON length');
 if(!response.body)throw Error('Missing JSON body');
 const reader=response.body.getReader();const chunks:Uint8Array[]=[];let size=0;
 try{
  for(;;){if(signal.aborted)throw Error('Request cancelled');const part=await reader.read();if(part.done)break;
   size+=part.value.length;if(size>MAX_JSON)throw Error('JSON response too large');chunks.push(part.value)}
  if(length!==null&&size!==Number(length))throw Error('Truncated JSON response');
  const bytes=new Uint8Array(size);let offset=0;for(const c of chunks){bytes.set(c,offset);offset+=c.length}
  return JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(bytes));
 }finally{try{await reader.cancel()}finally{reader.releaseLock()}}
}
function responseHeaders(h:Headers):Readonly<Record<string,string>> {
 const out:Record<string,string>={};
 for(const key of ['content-type','content-length','content-range','accept-ranges','retry-after','loreviva-client-contract']){
  const value=h.get(key);
  if(key==='accept-ranges'&&value!==null&&value!=='bytes'&&value!=='none')throw Error('Invalid binary range capability');
  if(value!==null)out[key]=value;
 }
 return Object.freeze(out);
}

/** Tokens belong to one local request owner. They are not platform credentials or journal permits. */
export function createPreparedIntegrationOwner<P extends Profile,R extends CredentialRole>(options:TransportOptions<P,R>) {
 const base=new URL(options.baseUrl),profile=options.profile,role=options.role,getBearer=options.getBearer,current=options.current??(()=>true);
 if(!['http:','https:'].includes(base.protocol)||base.username||base.password||base.search||base.hash||base.pathname!=='/')throw Error('Platform base must be an HTTP(S) origin');
 if(!Object.hasOwn(transportDescriptors,profile)||typeof getBearer!=='function')throw Error('Invalid transport configuration');
 const descriptors=transportDescriptors[profile] as unknown as Record<string,Descriptor|undefined>;
 const owned=new WeakMap<object,Sealed>();const active=new Map<AbortController,Promise<unknown>>();
 const resources=new Set<{close():Promise<void>}>();let closed=false;
 function live(){return !closed&&current()}
 function member(token:object){const v=owned.get(token);if(!v||!live()||v.used)throw Error('Foreign, stale, consumed or closed request');return v}
 async function prepare<O extends OperationsForRole<P,R>>(operation:O,input:Input<P,O>):Promise<PreparedIntegrationRequest<P,O>> {
  if(!live()||!Object.hasOwn(descriptors,operation))throw Error('Unknown or closed operation');
  const descriptor=descriptors[operation]!;
  if(!descriptor.roles.includes(role))throw Error('Configured credential role does not own this operation');
  const parts=copy(input) as Parts;let contentType:string|null=null,body=new Uint8Array(0);
  const supplied=parts.headers?.['Loreviva-Client-Contract'];
  if(supplied!==undefined&&supplied!==profile)throw Error('Profile mismatch');
  const allowed=new Set(descriptor.parameters.filter(x=>x.in==='header').map(x=>x.name));
  for(const name of Object.keys(parts.headers??{}))if(!allowed.has(name))throw Error('Unknown request header');
  if(descriptor.requestMime==='multipart/form-data'){
   const multipart=parts.body as MultipartBodies['uploadContentAsset'];
   if(!multipart||!(multipart.file instanceof Blob)||multipart.file.size>16777216)throw Error('Invalid multipart file');
   validate(descriptor.inputValidator,{...parts,body:{...multipart,file:''}});
   if(multipart.metadata.expected_size!==String(multipart.file.size))throw Error('Multipart size mismatch');
   const form=new FormData();form.set('metadata',JSON.stringify(multipart.metadata));form.set('file',multipart.file,multipart.metadata.name);
   const serialized=new Response(form);contentType=serialized.headers.get('content-type');
   body=new Uint8Array(await serialized.arrayBuffer());
  }else{
   validate(descriptor.inputValidator,parts);
   if(parts.body!==undefined){if(descriptor.requestMime!=='application/json')throw Error('Unsupported request encoding');
    body=encoder.encode(JSON.stringify(parts.body));contentType='application/json';if(body.length>MAX_JSON)throw Error('Request too large')}
  }
  const path=descriptor.path.replace(/\{([^}]+)\}/g,(_,name:string)=>{
   const value=parts.path?.[name];if(value===undefined)throw Error('Missing path value');return encodeURIComponent(value)});
  const query=new URLSearchParams();for(const [key,value] of Object.entries(parts.query??{}))query.set(key,String(value));
  const target=path+(query.size?'?'+query.toString():'');
  const bodySHA256=await sha(body);if(!live())throw Error('Request owner changed during preparation');
  const token=Object.freeze({}) as PreparedIntegrationRequest<P,O>;
  owned.set(token,{operation,descriptor,parts,method:descriptor.method,path:target,body,bodySHA256,contentType,used:false});return token;
 }
 function inspect<O extends Operation<P>>(token:PreparedIntegrationRequest<P,O>):PreparedRequestSnapshot {
  const v=member(token);return Object.freeze({operation:v.operation,profile,role,method:v.method,path:v.path,
   idempotencyKey:v.parts.headers?.['Idempotency-Key']??null,body:v.body.slice(),bodySHA256:v.bodySHA256,contentType:v.contentType});
 }
 // Consumption and fetch invocation happen synchronously, before the first network await.
 function sendPreparedOnce<O extends Operation<P>>(token:PreparedIntegrationRequest<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>> {
  let v:Sealed;
  try{v=member(token);if(signal?.aborted)throw Error('Request cancelled');
   const bearer=getBearer();if(!bearer||/[\r\n]/.test(bearer))throw Error('Configured credential is unavailable');
   const headers=new Headers(v.parts.headers);headers.set('Authorization','Bearer '+bearer);
   if(v.contentType)headers.set('Content-Type',v.contentType);
   const controller=new AbortController();const abort=()=>controller.abort(signal?.reason);
   signal?.addEventListener('abort',abort,{once:true});
   const init:RequestInit={method:v.method,headers,credentials:'omit',redirect:'error',cache:'no-store',signal:controller.signal};
   if(v.contentType)init.body=v.body;
   v.used=true;
   const flight=fetch(base.origin+v.path,init).then(async response=>{
    let transferred=false;
    try{
     if(!live()||controller.signal.aborted)throw Error('Response owner changed');
     const descriptor=v.descriptor.responses[String(response.status)];if(!descriptor)throw Error('Unknown response status');
     const mime=response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase();
     if(mime!==descriptor.mime)throw Error('Response media type differs from contract');
     const responseProfile=response.headers.get('loreviva-client-contract');
     if(responseProfile!==null&&responseProfile!==profile)throw Error('Response profile mismatch');
     let body:unknown;
     const finish=()=>{active.delete(controller);signal?.removeEventListener('abort',abort)};
     if(descriptor.protocol==='SSE'){
      if(!response.body)throw Error('Missing event stream');
      const resource=createEventStream(response.body,frame=>{
       if(frame.event==='update'){validate(descriptor.sseData!,frame.data);if(descriptor.sseFrame)validate(descriptor.sseFrame,frame)}
       else {validate('schema_Error',frame.data);if(descriptor.sseFrame)validate(descriptor.sseFrame,{event:frame.event,data:frame.data})}
      },live,controller.signal);
      const close=async()=>{try{await resource.close()}finally{resources.delete(wrapped);finish()}};
      const wrapped:AsyncDisposableEventStream=Object.freeze({...resource,get cursor(){return resource.cursor},close,cancel:close,
       async *[Symbol.asyncIterator](){try{yield*resource}finally{await close()}}});
      resources.add(wrapped);body=wrapped;transferred=true;
     }else if(descriptor.protocol==='BINARY'){
      const resource=createAuthorizedMediaBody(response,live,controller.signal);
      const close=async()=>{try{await resource.close()}finally{resources.delete(wrapped);finish()}};
      const wrapped:AuthorizedMediaBody=Object.freeze({...resource,close,cancel:close,
       async read(){try{return await resource.read()}finally{await close()}}});
      resources.add(wrapped);body=wrapped;transferred=true;
     }else{
      body=await jsonBody(response,controller.signal);
      const name=v.parts.query?.['include']==='provenance_v1'&&response.status===200
       ?`profile_${profile}_provenance_${v.operation}_200`:descriptor.validator;
      if(!name)throw Error('Missing response validator');validate(name,body);if(!live()||controller.signal.aborted)throw Error('Response owner changed');finish();
     }
     return Object.freeze({status:String(response.status),protocol:descriptor.protocol,headers:responseHeaders(response.headers),body}) as TransportResult<P,O>;
    }catch(e){if(!transferred)await response.body?.cancel().catch(()=>{});throw e}
   }).catch(e=>{active.delete(controller);signal?.removeEventListener('abort',abort);throw e});
   active.set(controller,flight);return flight;
  }catch(e){return Promise.reject(e)}
 }
 async function close(){
  closed=true;for(const controller of active.keys())controller.abort();
  await Promise.allSettled([...resources].map(r=>r.close()));await Promise.allSettled([...active.values()]);
  resources.clear();active.clear();
 }
 return Object.freeze({prepare,inspect,sendPreparedOnce,close});
}

export function createIntegrationTransport<P extends Profile,R extends CredentialRole>(options:TransportOptions<P,R>) {
 const owner=createPreparedIntegrationOwner<P,R>(options);
 function call<O extends OperationsForRole<P,R> & keyof ProjectionResponses>(operation:O,input:Input<P,O> & {query:{include:'provenance_v1'}},signal?:AbortSignal):Promise<TransportResult<P,O,{query:{include:'provenance_v1'}}>>;
 function call<O extends OperationsForRole<P,R>>(operation:O,input:DefaultInput<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>>;
 async function call(operation:string,input:unknown,signal?:AbortSignal):Promise<unknown> {
  return execute(operation,input,signal);
 }
 async function execute(operation:string,input:unknown,signal?:AbortSignal):Promise<unknown> {
  // Public overloads constrain source types; prepare independently validates
  // untrusted runtime callers before any credential or network access.
  return owner.sendPreparedOnce(await owner.prepare(operation as OperationsForRole<P,R>,input as Input<P,OperationsForRole<P,R>>),signal);
 }
 async function streamEvents(input:R extends 'CLIENT'?Input<P,'streamEvents'>:never,signal?:AbortSignal):Promise<AsyncDisposableEventStream>{
  const result=await execute('streamEvents',input,signal) as TransportResult<Profile,'streamEvents'>;
  if(result.status==='200')return result.body;throw new IntegrationResponseError(String(result.status),result.body,result.headers);
 }
 async function fetchRuntimeMedia(input:R extends 'CLIENT'?Input<P,'readRuntimeMedia'>:never,signal?:AbortSignal):Promise<AuthorizedMediaBody>{
  const result=await execute('readRuntimeMedia',input,signal) as TransportResult<Profile,'readRuntimeMedia'>;
  if(result.status==='200'||result.status==='206')return result.body;throw new IntegrationResponseError(String(result.status),result.body,result.headers);
 }
 return Object.freeze({call,streamEvents,fetchRuntimeMedia,close:owner.close});
}
export type IntegrationTransport<P extends Profile,R extends CredentialRole> = ReturnType<typeof createIntegrationTransport<P,R>>;

const clientGets=[
 'getIntegrationCapabilities','getPlayerSelfConsent','getSnapshot','getEvents','streamEvents','getTurn',
 'getOriginalTurnIntakeReceipt','getTask','readRuntimeMedia','listMemories','getMemory'
] as const;
const durableOperations=['createInstance','controlInstance','createQuote','submitTurn','commandPlayerSelfConsent','governMemories','cancelTask'] as const;
export type ClientGetOperation<P extends Profile> = Operation<P> & typeof clientGets[number];
export type DurableOperation<P extends Profile> = Operation<P> & typeof durableOperations[number];
export interface BffSessionView {
 readonly format:'lv-bff-session-v1';readonly authenticated:boolean;
 readonly identity:null|readonly [string,string,string,Profile];readonly csrf:string|null;
 readonly client:null|Readonly<{subject:string;world:string;profile:Profile;expires_at:string;view_epoch:string}>;
}
export interface BffOriginalObservation {
 readonly format:'lv-bff-original-v1';readonly original_id:string;readonly operation:typeof durableOperations[number];
 readonly state:'PREPARED'|'OBSERVED'|'UNKNOWN';readonly task_id:string|null;
 readonly observation:null|Readonly<{status:number;body:unknown}>;
}
export interface BffCustomerResult {
 readonly format:'lv-bff-customer-result-v1';
 readonly State:'INITIALIZED'|'CONFIRMED'|'DRAIN_LIMIT'|'REJECTED'|'UNKNOWN';
 readonly Persisted:number;readonly Confirmed:number;readonly Reconciled:boolean;
 readonly Retried:boolean;readonly OriginalActiveAtQuery:boolean;
}
export interface BrowserBffAdapter<P extends Profile> {
 login(input:{login:string;credential:string},signal?:AbortSignal):Promise<BffSessionView>;
 me(signal?:AbortSignal):Promise<BffSessionView>;
 logout(signal?:AbortSignal):Promise<void>;
 exchange(input:{originalKey:string},signal?:AbortSignal):Promise<BffSessionView>;
 read<O extends ClientGetOperation<P> & keyof ProjectionResponses>(operation:O,input:Input<P,O> & {query:{include:'provenance_v1'}},signal?:AbortSignal):Promise<TransportResult<P,O,{query:{include:'provenance_v1'}}>>;
 read<O extends ClientGetOperation<P>>(operation:O,input:DefaultInput<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>>;
 command<O extends DurableOperation<P>>(operation:O,input:Input<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>>;
 findOriginal<O extends DurableOperation<P>>(operation:O,originalKey:string,signal?:AbortSignal):Promise<BffOriginalObservation>;
 original(originalId:string,signal?:AbortSignal):Promise<BffOriginalObservation>;
 queryOriginal(originalId:string,signal?:AbortSignal):Promise<BffOriginalObservation>;
 consumeCustomer(signal?:AbortSignal):Promise<BffCustomerResult>;
 close():Promise<void>;
}
export class BffResponseError extends Error {
 readonly code:string;readonly recoveryAction:string;readonly status:number;readonly retryAfter:string|undefined;
 constructor(status:number,code:string,recoveryAction:string,retryAfter?:string){
  super('The local reference request was not completed');this.name='BffResponseError';
  if(retryAfter!==undefined&&!/^(0|[1-9][0-9]*)$/.test(retryAfter))throw Error('Invalid local Retry-After');
  this.status=status;this.code=code;this.recoveryAction=recoveryAction;
  this.retryAfter=retryAfter;
 }
}
function record(value:unknown,keys:readonly string[]):Record<string,unknown>{
 if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Invalid local response');
 const r=value as Record<string,unknown>;
 if(Object.keys(r).length!==keys.length||keys.some(k=>!Object.hasOwn(r,k)))throw Error('Invalid local response fields');
 return r;
}
function text(value:unknown,max=4096):value is string{return typeof value==='string'&&value.length>0&&value.length<=max&&!/[\u0000-\u001f\u007f]/.test(value)}
function freezeData<T>(value:T):T {
 if(value&&typeof value==='object'){for(const item of Object.values(value))freezeData(item);Object.freeze(value)}
 return value;
}
function sessionView(value:unknown,profile:Profile):BffSessionView {
 const r=record(value,['format','authenticated','identity','csrf','client']);
 if(r['format']!=='lv-bff-session-v1'||typeof r['authenticated']!=='boolean')throw Error('Invalid local session');
 if(!r['authenticated']){
  if(r['identity']!==null||r['csrf']!==null||r['client']!==null)throw Error('Anonymous session contains an association');
 }else{
  const identity=r['identity'];
  if(!Array.isArray(identity)||identity.length!==4||!identity.every(v=>text(v))||identity[3]!==profile||!text(r['csrf']))throw Error('Invalid local identity');
  if(r['client']!==null){
   const c=record(r['client'],['subject','world','profile','expires_at','view_epoch']);
   if(c['subject']!==identity[1]||c['world']!==identity[2]||c['profile']!==profile||!text(c['view_epoch'])||
      !text(c['expires_at'])||!Number.isFinite(Date.parse(c['expires_at'])))throw Error('Invalid local client association');
  }
 }
 return freezeData(value as BffSessionView);
}

/** Browser-local protocol only. This owner cannot accept a platform URL or bearer provider. */
export function createReferenceBffClient<P extends Profile>(profile:P):BrowserBffAdapter<P> {
 if(!Object.hasOwn(transportDescriptors,profile))throw Error('Unknown reference profile');
 const location=globalThis.location;
 if(!location||!['http:','https:'].includes(location.protocol)||location.origin==='null')throw Error('A browser origin is required');
 const origin=location.origin;
 const descriptors=transportDescriptors[profile] as unknown as Record<string,Descriptor|undefined>;
 const active=new Map<AbortController,Promise<unknown>>(),resources=new Set<{close():Promise<void>}>();
 let closed=false,generation=0,csrf:string|null=null,association:string|null=null,transition=false;
 function live(g:number){return !closed&&generation===g}
 async function releaseResources(){
  for(const controller of active.keys())controller.abort();
  await Promise.allSettled([...resources].map(r=>r.close()));
 }
 function invalidate(){generation++;for(const controller of active.keys())controller.abort();for(const r of resources)void r.close()}
 function partsFor(operation:string,input:unknown,write:boolean):{parts:Parts;descriptor:Descriptor}{
  const set:readonly string[]=write?durableOperations:clientGets;
  if(closed||transition||!set.includes(operation)||!Object.hasOwn(descriptors,operation))throw Error('Unknown, closed or transitioning reference operation');
  const descriptor=descriptors[operation]!;
  if(!write&&(descriptor.method!=='GET'||!descriptor.roles.includes('CLIENT')))throw Error('Not a reference CLIENT read');
  const parts=copy(input) as Parts;
  if(parts.headers?.['Loreviva-Client-Contract']!==undefined&&parts.headers['Loreviva-Client-Contract']!==profile)throw Error('Reference profile mismatch');
  const allowed=new Set(descriptor.parameters.filter(p=>p.in==='header').map(p=>p.name));
  for(const k of Object.keys(parts.headers??{}))if(!allowed.has(k))throw Error('Unknown reference header');
  validate(descriptor.inputValidator,parts);
  return {parts,descriptor};
 }
 async function request(path:string,body:unknown|undefined,signal:AbortSignal|undefined,
  decode:(response:Response,controller:AbortController,g:number,finish:()=>void)=>Promise<unknown>,login=false):Promise<unknown> {
  if(closed||signal?.aborted)throw Error('Reference owner closed or cancelled');
  const g=generation,controller=new AbortController(),abort=()=>controller.abort(signal?.reason);
  signal?.addEventListener('abort',abort,{once:true});
  const finish=()=>{active.delete(controller);signal?.removeEventListener('abort',abort)};
  let transferred=false;
  const flight=(async()=>{
   const headers=new Headers({Accept:'application/json, text/event-stream, audio/wav'});
   const init:RequestInit={method:body===undefined?'GET':'POST',headers,credentials:'same-origin',redirect:'error',cache:'no-store',signal:controller.signal};
   if(body!==undefined){
    if(!login){if(!csrf)throw Error('Current local CSRF association is required');headers.set('X-Loreviva-CSRF',csrf)}
    const bytes=encoder.encode(JSON.stringify(body));if(bytes.length>MAX_JSON)throw Error('Local request too large');
    headers.set('Content-Type','application/json');init.body=bytes;
   }
   const response=await fetch(origin+path,init);
   try{
    if(!live(g)||controller.signal.aborted)throw Error('Local association changed');
    const localError=response.headers.get('x-loreviva-bff-error');
    if(localError!==null){
     if(localError!=='1'||response.status<400||response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase()!=='application/json')throw Error('Invalid local error discriminator');
     const e=record(await jsonBody(response,controller.signal),['format','code','message','retryable','recovery_action']);
     if(e['format']!=='lv-bff-error-v1'||e['retryable']!==false||!text(e['code'])||!text(e['message'])||!text(e['recovery_action']))throw Error('Invalid local error');
     if(!live(g)||controller.signal.aborted)throw Error('Local association changed');
     throw new BffResponseError(response.status,e['code'],e['recovery_action'],response.headers.get('retry-after')??undefined);
    }
    const value=await decode(response,controller,g,finish);
    if(!live(g)||controller.signal.aborted){
     if(value&&typeof value==='object'&&'protocol' in value&&(value.protocol==='SSE'||value.protocol==='BINARY')&&
        'body' in value&&value.body&&typeof value.body==='object'&&'close' in value.body&&typeof value.body.close==='function'){
      await value.body.close();
     }
     throw Error('Local association changed');
    }
    transferred=Boolean(value&&typeof value==='object'&&'protocol' in value&&(value.protocol==='SSE'||value.protocol==='BINARY'));
    return value;
   }finally{if(!transferred)await response.body?.cancel().catch(()=>{})}
  })();
  active.set(controller,flight);
  try{return await flight}finally{if(!transferred)finish()}
 }
 async function local(path:string,body:unknown|undefined,signal?:AbortSignal,login=false):Promise<unknown>{
  return request(path,body,signal,async(response,c)=>{
   if(response.status!==200||response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase()!=='application/json')throw Error('Invalid local response protocol');
   return jsonBody(response,c.signal);
  },login);
 }
 async function source(operation:string,parts:Parts,descriptor:Descriptor,path:string,body:unknown|undefined,signal?:AbortSignal):Promise<unknown>{
  return request(path,body,signal,async(response,controller,g,finish)=>{
   const d=descriptor.responses[String(response.status)];
   if(!d||response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase()!==d.mime)throw Error('Invalid reference source response protocol');
   const rp=response.headers.get('loreviva-client-contract');if(rp!==null&&rp!==profile)throw Error('Invalid reference response profile');
   let result:unknown;
   if(d.protocol==='SSE'){
    if(!response.body)throw Error('Missing reference stream');
    const r=createEventStream(response.body,frame=>{
     if(frame.event==='update'){validate(d.sseData!,frame.data);if(d.sseFrame)validate(d.sseFrame,frame)}
     else{validate('schema_Error',frame.data);if(d.sseFrame)validate(d.sseFrame,{event:frame.event,data:frame.data})}
    },()=>live(g),controller.signal);
    const close=async()=>{try{await r.close()}finally{resources.delete(wrapped);finish()}};
    const wrapped:AsyncDisposableEventStream=Object.freeze({...r,get cursor(){return r.cursor},close,cancel:close,
     async *[Symbol.asyncIterator](){try{yield*r}finally{await close()}}});
    resources.add(wrapped);result=wrapped;
   }else if(d.protocol==='BINARY'){
    const r=createAuthorizedMediaBody(response,()=>live(g),controller.signal);
    const close=async()=>{try{await r.close()}finally{resources.delete(wrapped);finish()}};
    const wrapped:AuthorizedMediaBody=Object.freeze({...r,close,cancel:close,async read(){try{return await r.read()}finally{await close()}}});
    resources.add(wrapped);result=wrapped;
   }else{
    result=await jsonBody(response,controller.signal);
    const name=parts.query?.['include']==='provenance_v1'&&response.status===200?`profile_${profile}_provenance_${operation}_200`:d.validator;
    if(!name)throw Error('Missing reference source validator');validate(name,result);
   }
   return Object.freeze({status:String(response.status),protocol:d.protocol,headers:responseHeaders(response.headers),body:result});
  });
 }
 async function updateSession(path:string,body:unknown|undefined,signal?:AbortSignal,login=false):Promise<BffSessionView>{
  if(closed||transition)throw Error('Session transition already active or closed');
  transition=true;
  try{
   // Prevent responses issued before login/exchange from populating the new view.
   if(body!==undefined)invalidate();
   const result=sessionView(await local(path,body,signal,login),profile);
   const next=JSON.stringify([result.identity,result.client]);
   if(association!==next)invalidate();
   association=next;csrf=result.csrf;return result;
  }catch(error){csrf=null;association=null;invalidate();throw error}finally{transition=false}
 }
 function originalId(id:string){if(!/^[a-f0-9]{32}$/.test(id))throw Error('Invalid original identifier');return id}
 function observation(value:unknown,id:string):BffOriginalObservation{
  const r=record(value,['format','original_id','operation','state','task_id','observation']);
  if(r['format']!=='lv-bff-original-v1'||r['original_id']!==id||!(durableOperations as readonly unknown[]).includes(r['operation'])||
     !['PREPARED','OBSERVED','UNKNOWN'].includes(String(r['state']))||(r['task_id']!==null&&!text(r['task_id'])))throw Error('Invalid original observation');
  if(r['observation']!==null){
   const o=record(r['observation'],['status','body']),d=descriptors[r['operation'] as string]!;
   if(!Number.isInteger(o['status']))throw Error('Invalid observation status');
   const name=d.responses[String(o['status'])]?.validator;
   const valid=name&&validation[name]?.(o['body']);
   const originalTurn=r['operation']==='submitTurn'&&o['status']===200&&validation['schema_OriginalTurnIntakeReceipt']?.(o['body']);
   if(!valid&&!originalTurn&&!(o['status']===200&&validation['schema_TaskView']?.(o['body'])))throw Error('Invalid credential-free observation');
  }
  return freezeData(value as BffOriginalObservation);
 }
 function read<O extends ClientGetOperation<P> & keyof ProjectionResponses>(operation:O,input:Input<P,O> & {query:{include:'provenance_v1'}},signal?:AbortSignal):Promise<TransportResult<P,O,{query:{include:'provenance_v1'}}>>;
 function read<O extends ClientGetOperation<P>>(operation:O,input:DefaultInput<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>>;
 async function read(operation:string,input:unknown,signal?:AbortSignal):Promise<unknown>{
  const {parts,descriptor}=partsFor(operation,input,false);
  const envelope={format:'lv-bff-read-v1',profile,path:parts.path??{},query:Object.entries(parts.query??{}),
   range:operation==='readRuntimeMedia'?parts.headers?.['Range']??null:null,
   original_key:operation==='getOriginalTurnIntakeReceipt'?parts.headers?.['Idempotency-Key']??null:null};
  const encoded=encodeURIComponent(JSON.stringify(envelope));if(encoder.encode(encoded).length>16384)throw Error('Reference read envelope too large');
  return source(operation,parts,descriptor,`/bff/client/${operation}?args=${encoded}`,undefined,signal);
 }
 return Object.freeze({
  async login(input:{login:string;credential:string},signal?:AbortSignal){
   const r=record(copy(input),['login','credential']);if(!text(r['login'])||!text(r['credential']))throw Error('Invalid login input');
   return updateSession('/bff/login',{format:'lv-bff-login-v1',...r},signal,true);
  },
  me(signal?:AbortSignal){return updateSession('/bff/me',undefined,signal)},
  async logout(signal?:AbortSignal){
   if(closed||transition)throw Error('Session transition already active or closed');transition=true;invalidate();
   try{await request('/bff/logout',{format:'lv-bff-logout-v1'},signal,async response=>{
    if(response.status!==204)throw Error('Invalid logout response');return null;
   })}finally{csrf=null;association=null;transition=false}
  },
  async exchange(input:{originalKey:string},signal?:AbortSignal){
   const r=record(copy(input),['originalKey']);
   if(typeof r['originalKey']!=='string'||!/^[A-Za-z0-9._-]{1,128}$/.test(r['originalKey']))throw Error('Invalid original exchange key');
   return updateSession('/bff/exchange',{format:'lv-bff-exchange-v1',original_key:r['originalKey']},signal);
  },
  read,
  async command<O extends DurableOperation<P>>(operation:O,input:Input<P,O>,signal?:AbortSignal):Promise<TransportResult<P,O>>{
   const {parts,descriptor}=partsFor(operation,input,true);
   const envelope={format:'lv-bff-command-v1',operation,profile,original_key:parts.headers?.['Idempotency-Key']??null,
    path:parts.path??{},query:Object.entries(parts.query??{}),body:parts.body??null};
   return source(operation,parts,descriptor,'/bff/commands',envelope,signal) as Promise<TransportResult<P,O>>;
  },
  async findOriginal<O extends DurableOperation<P>>(operation:O,originalKey:string,signal?:AbortSignal){
   if(transition||!(durableOperations as readonly string[]).includes(operation)||!Object.hasOwn(descriptors,operation)||
      typeof originalKey!=='string'||!/^[A-Za-z0-9._-]{1,128}$/.test(originalKey))throw Error('Invalid original locator');
   const envelope={format:'lv-bff-find-original-v1',operation,profile,original_key:originalKey};
   if(encoder.encode(JSON.stringify(envelope)).length>16384)throw Error('Original locator too large');
   const raw=await local('/bff/find-original',envelope,signal),r=record(raw,['format','original_id','operation','state','task_id','observation']);
   if(typeof r['original_id']!=='string'||r['operation']!==operation)throw Error('Original locator association differs');
   return observation(raw,originalId(r['original_id']));
  },
  async original(id:string,signal?:AbortSignal){originalId(id);return observation(await local('/bff/originals/'+id,undefined,signal),id)},
  async queryOriginal(id:string,signal?:AbortSignal){
   originalId(id);
   return request('/bff/query-original/'+id,{format:'lv-bff-query-v1'},signal,async(response,c)=>{
    if(response.status!==200||response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase()!=='application/json')throw Error('Invalid local observation protocol');
    const value=observation(await jsonBody(response,c.signal),id),status=value.observation?.status;
    if(status===429||status===503)throw new BffResponseError(status,'ORIGINAL_QUERY_WAIT','WAIT_RETRY_AFTER',response.headers.get('retry-after')??undefined);
    return value;
   }) as Promise<BffOriginalObservation>;
  },
  async consumeCustomer(signal?:AbortSignal){
   const value=await local('/bff/customer-consume',{format:'lv-bff-consume-v1'},signal);
   const r=record(value,['format','State','Persisted','Confirmed','Reconciled','Retried','OriginalActiveAtQuery']);
   if(r['format']!=='lv-bff-customer-result-v1'||!['INITIALIZED','CONFIRMED','DRAIN_LIMIT','REJECTED','UNKNOWN'].includes(String(r['State']))||
      !['Persisted','Confirmed'].every(k=>Number.isSafeInteger(r[k])&&(r[k] as number)>=0)||
      !['Reconciled','Retried','OriginalActiveAtQuery'].every(k=>typeof r[k]==='boolean'))throw Error('Invalid customer result');
   return freezeData(value as BffCustomerResult);
  },
  async close(){if(closed)return;closed=true;invalidate();await releaseResources();await Promise.allSettled([...active.values()]);active.clear();resources.clear();csrf=null;association=null}
 });
}
