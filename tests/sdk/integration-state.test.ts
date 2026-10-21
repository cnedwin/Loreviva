import {test,expect,vi} from 'vitest';
import {createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {ViewOwner,compareWireRevision} from '../../packages/sdk-ts/src/cache.ts';
import {createAuthorizedMediaBody} from '../../packages/sdk-ts/src/media.ts';
import {BffResponseError,type BffOriginalObservation} from '../../packages/sdk-ts/src/client.ts';

const schemas=JSON.parse(readFileSync(new URL('../../contracts/generated/openapi.json',import.meta.url),'utf8')).components.schemas;
const identity={loginGeneration:'1',customer:'customer_demo',player:'player_demo',world:'instance_demo',profile:'integration_r0_v1',sessionIdentity:'local-observed-session',viewEpoch:'1',privacyEpoch:'1'};
const deferred=<T>()=>{let resolve!:(value:T)=>void;const promise=new Promise<T>(r=>{resolve=r});return {promise,resolve}};

test('SDKIdentityEpochAndLateMedia',async()=>{
 const owner=new ViewOwner();const first=owner.enter(identity);
 expect(owner.current({...first})).toBe(false);expect(owner.mayReuseBody(first)).toBe(true);
 let disposed=0;owner.register(first,()=>{disposed++;expect(owner.current(first)).toBe(false)});
 const wait=deferred<string>();let cached:string|null=null;
 const late=wait.promise.then(value=>{if(owner.current(first))cached=value});
 const second=owner.enter({...identity,player:'other'});
 wait.resolve('private-old-body');await late;expect(cached).toBeNull();expect(disposed).toBe(1);
 expect(owner.current(first)).toBe(false);expect(owner.current(second)).toBe(true);
 for(const field of ['loginGeneration','customer','player','world','profile','sessionIdentity','viewEpoch','privacyEpoch'] as const){
  const previous=owner.capture();owner.enter({...identity,[field]:'changed'});expect(owner.current(previous)).toBe(false);
 }
 const noEpoch=owner.enter({loginGeneration:'2',customer:'c',player:'p',world:'w',profile:'public_v1',sessionIdentity:'s'});
 expect(owner.mayReuseBody(noEpoch)).toBe(false);expect(owner.identity?.viewEpoch).toBeUndefined();
 expect(compareWireRevision('9007199254740992','9007199254740993')).toBe(-1);
 expect(compareWireRevision('18446744073709551615','18446744073709551614')).toBe(1);
 for(const value of ['-1','01','18446744073709551616','1.0'])expect(()=>compareWireRevision(value,'1')).toThrow();
 owner.close();expect(owner.current(noEpoch)).toBe(false);expect(()=>owner.capture()).toThrow();
});

test('SDKOriginalTurnRecoveryBudget',async()=>{
 const module=await import('../../packages/sdk-ts/src/recovery.ts').catch(()=>null);
 expect(module!==null,'Original-key recovery owner is required').toBe(true);if(!module)return;
 vi.useFakeTimers();
 try{
  const view=new ViewOwner();const token=view.enter(identity);const calls:{time:number;input:unknown}[]=[];
  let mode='missing',mutations=0;
  const originalMutation=()=>{mutations++;throw Error('Controlled lost acknowledgment after first send')};
  expect(originalMutation).toThrow();expect(mutations).toBe(1);
  const port={async query(input:unknown){calls.push({time:Date.now(),input});
   if(mode==='accepted')return {status:'200',body:schemas.OriginalTurnIntakeReceipt.examples[0],headers:{}};
   if(mode==='retry')return {status:'429',body:{...schemas.Error.examples[0],retryable:true,recovery_action:'WAIT_RETRY_AFTER'},headers:{'retry-after':'3'}};
   return {status:'404',body:schemas.Error.examples[0],headers:{}};}};
  const controller=new module.RecoveryController(port,view);
  const original={world:'instance_demo',key:'turn-demo-0001',customer:'customer_demo',player:'player_demo',profile:'integration_r0_v1' as const};
  const unknown=controller.queryOriginalTurn(original,token);
  await vi.advanceTimersByTimeAsync(19000);
  expect((await unknown).state).toBe('UNKNOWN');expect(calls).toHaveLength(20);
  for(let i=1;i<calls.length;i++)expect(calls[i]!.time-calls[i-1]!.time).toBeGreaterThanOrEqual(1000);
  expect(mutations).toBe(1);expect(new Set(calls.map(c=>JSON.stringify(c.input))).size).toBe(1);
  mode='retry';calls.length=0;const retry=controller.queryOriginalTurn(original,token);
  await vi.advanceTimersByTimeAsync(2999);expect(calls).toHaveLength(1);
  mode='accepted';await vi.advanceTimersByTimeAsync(1);
  expect((await retry).state).toBe('OBSERVED');expect(calls).toHaveLength(2);expect(mutations).toBe(1);
  mode='missing';const stale=controller.queryOriginalTurn(original,token);const observed=stale.catch(()=>null);
  view.enter({...identity,sessionIdentity:'new-session'});await vi.advanceTimersByTimeAsync(1000);
  expect(await observed).toBeNull();await controller.close();
  await expect(controller.queryOriginalTurn(original,token)).rejects.toThrow();
  const localView=new ViewOwner(),localToken=localView.enter(identity);
  const reads:{kind:string;time:number}[]=[];let localMode='missing',localFind=0,localQuery=0;
  const held=deferred<BffOriginalObservation>();
  const found:BffOriginalObservation={format:'lv-bff-original-v1',original_id:'a'.repeat(32),operation:'submitTurn',state:'UNKNOWN',task_id:null,observation:null};
  const localPort={
   async findOriginal(){reads.push({kind:'find',time:Date.now()});localFind++;
    if(localMode==='late')return held.promise;
    if(localMode==='missing'||localFind<3)throw new BffResponseError(404,'ORIGINAL_NOT_FOUND','QUERY_ORIGINAL_TASK');
    return found;},
   async queryOriginal(id:string){expect(id).toBe(found.original_id);reads.push({kind:'query',time:Date.now()});localQuery++;
    if(localMode==='wait')throw new BffResponseError(429,'ORIGINAL_QUERY_WAIT','WAIT_RETRY_AFTER','3');
    if(localMode==='accepted')return {...found,state:'OBSERVED' as const,observation:{status:200,body:schemas.OriginalTurnIntakeReceipt.examples[0]}};
    return found;}
  };
  const localController=new module.BffRecoveryController(localPort,localView),locator={...original,operation:'submitTurn' as const};
  const noRecord=localController.queryOriginal(locator,localToken);await vi.advanceTimersByTimeAsync(19000);
  expect(await noRecord).toMatchObject({state:'UNKNOWN',queries:20,reason:'READ_BUDGET_EXHAUSTED'});
  expect(reads).toHaveLength(20);expect(localQuery).toBe(0);expect(mutations).toBe(1);
  localMode='unknown';localFind=0;localQuery=0;reads.length=0;
  const mixed=localController.queryOriginal(locator,localToken);await vi.advanceTimersByTimeAsync(19000);
  expect(await mixed).toMatchObject({state:'UNKNOWN',queries:20});expect(localFind).toBe(3);expect(localQuery).toBe(17);
  for(let i=1;i<reads.length;i++)expect(reads[i]!.time-reads[i-1]!.time).toBeGreaterThanOrEqual(1000);
  localMode='wait';reads.length=0;
  const waited=localController.queryOriginal(locator,localToken);await vi.advanceTimersByTimeAsync(1000);
  expect(reads.map(r=>r.kind)).toEqual(['find','query']);
  await vi.advanceTimersByTimeAsync(2999);expect(reads).toHaveLength(2);
  localMode='accepted';await vi.advanceTimersByTimeAsync(1);expect(await waited).toMatchObject({state:'OBSERVED',queries:3});
  localMode='late';const lateFind=localController.queryOriginal(locator,localToken),lateOutcome=lateFind.catch(()=>null);
  localView.enter({...identity,player:'other'});held.resolve(found);await vi.advanceTimersByTimeAsync(0);
  expect(await lateOutcome).toBeNull();expect(mutations).toBe(1);await localController.close();
 }finally{vi.useRealTimers()}
});

function wav(){
 const b=new Uint8Array(160044),v=new DataView(b.buffer),text=(at:number,s:string)=>b.set(new TextEncoder().encode(s),at);
 text(0,'RIFF');v.setUint32(4,160036,true);text(8,'WAVE');text(12,'fmt ');v.setUint32(16,16,true);
 v.setUint16(20,1,true);v.setUint16(22,1,true);v.setUint32(24,16000,true);v.setUint32(28,32000,true);
 v.setUint16(32,2,true);v.setUint16(34,16,true);text(36,'data');v.setUint32(40,160000,true);return b;
}
class AudioSink extends EventTarget {src='';plays=0;pauses=0;removed=false;remove(){this.removed=true}async play(){this.plays++}pause(){this.pauses++}removeAttribute(name:string){if(name==='src')this.src=''}load(){}}
test('SDKMediaAuthorizationAndDisposal',async()=>{
 const module=await import('../../packages/sdk-ts/src/media.ts');
 expect('MediaController' in module,'Current authorized playback owner is required').toBe(true);if(!('MediaController' in module))return;
 const view=new ViewOwner();const token=view.enter(identity);let sink!:AudioSink;const sinks:AudioSink[]=[];const createSink=()=>{sink=new AudioSink();sinks.push(sink);return sink};const bytes=wav();let calls=0;
 const sha=createHash('sha256').update(bytes).digest('hex');
 let mode='good';const held=deferred<Uint8Array>();let slow=false;
 const port={async fetchRuntimeMedia(_input:unknown,signal?:AbortSignal){
  calls++;let source:ReadableStream<Uint8Array>;
  if(slow)source=new ReadableStream({async start(c){const b=await held.promise;try{c.enqueue(b);c.close()}catch{/* An invalidated reader was cancelled. */}}});
  else source=new ReadableStream({start(c){c.enqueue(mode==='bad'?new Uint8Array(bytes.length):bytes);c.close()}});
  return createAuthorizedMediaBody(new Response(source,{headers:{'content-type':'audio/wav'}}),()=>view.current(token),signal);
 }};
 const controller=new module.MediaController(port,view,createSink);
 const resource={...schemas.RuntimeMediaRef.examples[1],content_sha256:sha};
 const revoked:string[]=[];const real=URL.revokeObjectURL.bind(URL);
 const spy=vi.spyOn(URL,'revokeObjectURL').mockImplementation(url=>{revoked.push(url);real(url)});
 try{
  await controller.play(resource,token);const first=sink.src;expect(first.startsWith('blob:')).toBe(true);expect(calls).toBe(1);expect(sink.plays).toBe(1);
  await controller.play(resource,token);const second=sink.src;expect(second).not.toBe(first);expect(revoked).toContain(first);expect(calls).toBe(2);
  mode='bad';await expect(controller.play(resource,token)).rejects.toThrow();expect(sink.src).toBe('');mode='good';
  slow=true;const old=controller.play(resource,token);const oldResult=old.catch(()=>null);
  slow=false;await controller.play(resource,token);const currentURL=sink.src;
  held.resolve(bytes);await oldResult;expect(sink.src).toBe(currentURL);expect(revoked).not.toContain(currentURL);

  // Queue a native event for A, then create and attach B's listeners before
  // dispatch. The queued event still targets A, never the new playback.
  for(const kind of ['ended','pause','error']){
   const oldTarget=sink;
   const queued=new Promise<void>(resolve=>setTimeout(()=>{oldTarget.dispatchEvent(new Event(kind));resolve()},0));
   await controller.play(resource,token);const newTarget=sink,newURL=sink.src;
   await queued;
   expect(newTarget).not.toBe(oldTarget);expect(oldTarget.removed).toBe(true);
   expect(newTarget.src).toBe(newURL);expect(revoked).not.toContain(newURL);
   newTarget.dispatchEvent(new Event(kind));
   expect(newTarget.src).toBe('');expect(newTarget.removed).toBe(true);expect(revoked).toContain(newURL);
   await controller.play(resource,token);
  }
  const finalURL=sink.src;view.invalidate();expect(sink.src).toBe('');expect(revoked).toContain(finalURL);

  const publicToken=view.enter({...identity,profile:'public_v1'});const publicMedia={media_id:'audio_demo',asset_id:'asset_demo',media_kind:'AUDIO' as const,media_state:'READY' as const,resource_revision:'1'};
  // A public descriptor has no invented hash; authorization and actual WAV checks still apply.
  const publicPort={async fetchRuntimeMedia(_input:unknown,signal?:AbortSignal){calls++;return createAuthorizedMediaBody(new Response(bytes,{headers:{'content-type':'audio/wav'}}),()=>view.current(publicToken),signal)}};
  const publicController=new module.MediaController(publicPort,view,createSink);
  await publicController.play(publicMedia,publicToken);expect(sink.src.startsWith('blob:')).toBe(true);
  await publicController.close();await controller.close();expect(sink.src).toBe('');
 }finally{await controller.close();spy.mockRestore()}

 // A legal large native upstream chunk is consumed in bounded 16-KiB
 // copies, not rejected or confused with the network's own chunk size.
 const sizes:number[]=[];const native=wav();const originalSlice=native.slice.bind(native);
 native.slice=((start?:number,end?:number)=>{const out=originalSlice(start,end);sizes.push(out.length);return out}) as typeof native.slice;
 const bounded=createAuthorizedMediaBody(new Response(new ReadableStream<Uint8Array>({start(c){c.enqueue(native);c.close()}}),{headers:{'content-type':'audio/wav'}}),()=>true);
 expect(await bounded.read()).toEqual(wav());expect(sizes.length).toBeGreaterThan(1);
 expect(Math.max(...sizes)).toBeLessThanOrEqual(16384);expect(sizes.reduce((a,b)=>a+b,0)).toBe(160044);
 // Current loss and abort between bounded copies must stop consumption.
 // The upstream emits one legal native chunk; no network chunk limit is assumed.
 for(const mode of ['current','abort'] as const){
  const upstream=wav(),aborter=new AbortController();let valid=true,copies=0,cancelled=0;
  const nativeSlice=upstream.slice.bind(upstream);
  upstream.slice=((start?:number,end?:number)=>{const part=nativeSlice(start,end);copies++;if(copies===1){if(mode==='current')valid=false;else aborter.abort()}return part}) as typeof upstream.slice;
  const body=createAuthorizedMediaBody(new Response(new ReadableStream<Uint8Array>({start(c){c.enqueue(upstream)},cancel(){cancelled++}}),{headers:{'content-type':'audio/wav'}}),()=>valid,aborter.signal);
  await expect(body.read()).rejects.toThrow();await body.close();
  expect(copies).toBe(1);expect(cancelled).toBe(1);
 }
 const partial=createAuthorizedMediaBody(new Response(new Uint8Array([1,2]),{status:206,headers:{'content-type':'audio/wav','content-range':'bytes 0-1/160044'}}),()=>true);
 expect(Array.from(await partial.read())).toEqual([1,2]);
 const oversized=createAuthorizedMediaBody(new Response(new ReadableStream({start(c){c.enqueue(new Uint8Array(16777216));c.enqueue(new Uint8Array(1));c.close()}}),{headers:{'content-type':'audio/wav'}}),()=>true);
 await expect(oversized.read()).rejects.toThrow();
});
