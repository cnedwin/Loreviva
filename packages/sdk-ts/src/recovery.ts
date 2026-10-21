import validators from './generated/validators.cjs';
import type {OriginalTurnIntakeReceipt} from './generated/contracts.ts';
import {BffResponseError,type Profile,type BffOriginalObservation,type DurableOperation} from './client.ts';
import {ViewOwner,type ViewToken} from './cache.ts';

export interface OriginalTurnReference {readonly world:string;readonly key:string;readonly customer:string;readonly player:string;readonly profile:Profile}
export interface OriginalQueryInput {readonly path:{readonly instance_id:string};readonly headers:{readonly 'Idempotency-Key':string;readonly 'Loreviva-Client-Contract':Profile}}
/** Read-only port: recovery has no mutation or credential-creation operation. */
export interface OriginalTurnQueryPort {query(input:OriginalQueryInput,signal?:AbortSignal):Promise<{status:string;body:unknown;headers:Readonly<Record<string,string>>}>}
export type OriginalObservation=Readonly<{state:'OBSERVED';receipt:OriginalTurnIntakeReceipt;queries:number}>|Readonly<{state:'UNKNOWN';queries:number;reason:string}>;
function delay(ms:number,signal:AbortSignal):Promise<void>{
 return new Promise((resolve,reject)=>{
  if(signal.aborted){reject(Error('Recovery cancelled'));return}
  const abort=()=>{clearTimeout(timer);signal.removeEventListener('abort',abort);reject(Error('Recovery cancelled'))};
  const timer=setTimeout(()=>{signal.removeEventListener('abort',abort);resolve()},ms);signal.addEventListener('abort',abort,{once:true});
 });
}
export class RecoveryController {
 #query:OriginalTurnQueryPort['query'];#view:ViewOwner;#closed=false;#active=new Map<AbortController,Promise<OriginalObservation>>();
 constructor(port:OriginalTurnQueryPort,view:ViewOwner){this.#query=port.query.bind(port);this.#view=view}
 queryOriginalTurn(reference:OriginalTurnReference,token:ViewToken,signal?:AbortSignal):Promise<OriginalObservation>{
  if(this.#closed||this.#active.size||!this.#view.current(token))return Promise.reject(Error('Recovery owner is unavailable'));
  const original=Object.freeze({...reference}),identity=this.#view.identity;
  if(!identity||identity.customer!==original.customer||identity.player!==original.player||identity.world!==original.world||identity.profile!==original.profile
   ||! /^[A-Za-z0-9._-]{1,128}$/.test(original.key))return Promise.reject(Error('Original association differs from current view'));
  const input=Object.freeze({path:Object.freeze({instance_id:original.world}),headers:Object.freeze({'Idempotency-Key':original.key,'Loreviva-Client-Contract':original.profile})});
  const controller=new AbortController(),abort=()=>controller.abort(signal?.reason);
  signal?.addEventListener('abort',abort,{once:true});if(signal?.aborted)abort();
  const unregister=this.#view.register(token,()=>controller.abort());
  const run=async():Promise<OriginalObservation>=>{
   try{
    for(let queries=1;queries<=20;queries++){
     if(this.#closed||controller.signal.aborted||!this.#view.current(token))throw Error('Recovery view changed');
     const began=performance.now();let response:Awaited<ReturnType<OriginalTurnQueryPort['query']>>;
     try{response=await this.#query(input,controller.signal)}catch{
      if(controller.signal.aborted||!this.#view.current(token))throw Error('Recovery cancelled');
      return Object.freeze({state:'UNKNOWN',queries,reason:'READ_UNCERTAIN'});
     }
     if(controller.signal.aborted||!this.#view.current(token))throw Error('Recovery view changed');
     if(response.status==='200'){
      if(!validators.schema_OriginalTurnIntakeReceipt(response.body))throw Error('Invalid original receipt');
      const receipt=JSON.parse(JSON.stringify(response.body)) as OriginalTurnIntakeReceipt;
      if(receipt.original_key!==original.key||receipt.customer_id!==original.customer||receipt.caller_subject_id!==original.player||receipt.scope.world_instance_id!==original.world
       ||receipt.acceptance.task.scope.world_instance_id!==original.world)throw Error('Original receipt association differs');
      return Object.freeze({state:'OBSERVED',receipt,queries});
     }
     if(!validators.schema_Error(response.body))throw Error('Invalid recovery error');
     if(!['404','429','503'].includes(response.status))return Object.freeze({state:'UNKNOWN',queries,reason:'CURRENT_GATE_OR_CONFLICT'});
     if(queries===20)return Object.freeze({state:'UNKNOWN',queries,reason:'READ_BUDGET_EXHAUSTED'});
     const retry=response.headers['retry-after'];let milliseconds=Math.max(0,1000-(performance.now()-began));
     if(retry!==undefined){
      if(!/^(0|[1-9][0-9]*)$/.test(retry))throw Error('Invalid Retry-After');
      if(BigInt(retry)>2147483n)return Object.freeze({state:'UNKNOWN',queries,reason:'RETRY_AFTER_OUTSIDE_TIMER_RANGE'});
      milliseconds=Math.max(milliseconds,Number(retry)*1000);
     }
     await delay(milliseconds,controller.signal);
    }
    throw Error('Unreachable recovery budget');
   }finally{unregister();signal?.removeEventListener('abort',abort);this.#active.delete(controller)}
  };
  const flight=run();this.#active.set(controller,flight);return flight;
 }
 async close(){this.#closed=true;for(const c of this.#active.keys())c.abort();await Promise.allSettled([...this.#active.values()]);this.#active.clear()}
}

export interface BffOriginalReference extends OriginalTurnReference {readonly operation:DurableOperation<Profile>}
/** The only capabilities used by this recovery cycle are local lookup and original read. */
export interface BffOriginalQueryPort {
 findOriginal(operation:DurableOperation<Profile>,key:string,signal?:AbortSignal):Promise<BffOriginalObservation>;
 queryOriginal(id:string,signal?:AbortSignal):Promise<BffOriginalObservation>;
}
export type BffRecoveryObservation=
 Readonly<{state:'OBSERVED';original:BffOriginalObservation;queries:number}>|
 Readonly<{state:'UNKNOWN';original:BffOriginalObservation|null;queries:number;reason:string}>;
export class BffRecoveryController {
 #port:BffOriginalQueryPort;#view:ViewOwner;#closed=false;#active=new Map<AbortController,Promise<BffRecoveryObservation>>();
 constructor(port:BffOriginalQueryPort,view:ViewOwner){
  this.#port=Object.freeze({findOriginal:port.findOriginal.bind(port),queryOriginal:port.queryOriginal.bind(port)});this.#view=view;
 }
 queryOriginal(reference:BffOriginalReference,token:ViewToken,signal?:AbortSignal):Promise<BffRecoveryObservation>{
  if(this.#closed||this.#active.size||!this.#view.current(token))return Promise.reject(Error('Recovery owner unavailable'));
  const original=Object.freeze({...reference}),identity=this.#view.identity;
  if(!identity||identity.customer!==original.customer||identity.player!==original.player||identity.world!==original.world||
     identity.profile!==original.profile||!['createInstance','controlInstance','createQuote','submitTurn','commandPlayerSelfConsent','governMemories','cancelTask'].includes(original.operation)||
     typeof original.key!=='string'||!/^[A-Za-z0-9._-]{1,128}$/.test(original.key))return Promise.reject(Error('Original locator differs from current view'));
  const controller=new AbortController(),abort=()=>controller.abort(signal?.reason);
  signal?.addEventListener('abort',abort,{once:true});if(signal?.aborted)abort();
  const unregister=this.#view.register(token,()=>controller.abort());
  const current=()=>{if(this.#closed||controller.signal.aborted||!this.#view.current(token))throw Error('Recovery view changed')};
  const run=async():Promise<BffRecoveryObservation>=>{
   let found:BffOriginalObservation|null=null;
   try{
    for(let queries=1;queries<=20;queries++){
     current();const began=performance.now();let retryAfter:string|undefined;
     const lookup:boolean=found===null;
     try{
      const value:BffOriginalObservation=lookup?await this.#port.findOriginal(original.operation,original.key,controller.signal):
       await this.#port.queryOriginal(found!.original_id,controller.signal);
      current();
      if(value.operation!==original.operation||(found!==null&&value.original_id!==found.original_id))throw Error('Original observation association differs');
      found=value;
      // A found PREPARED/UNKNOWN record is not evidence that a send did not happen.
      // Lookup and each subsequent query consume the same finite read budget.
      if(!lookup&&value.state==='OBSERVED'&&value.observation!==null&&!['404','429','503'].includes(String(value.observation.status)))
       return Object.freeze({state:'OBSERVED',original:value,queries});
     }catch(error){
      current();
      if(!(error instanceof BffResponseError))return Object.freeze({state:'UNKNOWN',original:found,queries,reason:'READ_UNCERTAIN'});
      if(![404,429,503].includes(error.status))return Object.freeze({state:'UNKNOWN',original:found,queries,reason:'CURRENT_GATE_OR_CONFLICT'});
      retryAfter=error.retryAfter;
     }
     if(queries===20)return Object.freeze({state:'UNKNOWN',original:found,queries,reason:'READ_BUDGET_EXHAUSTED'});
     let wait=Math.max(0,1000-(performance.now()-began));
     if(retryAfter!==undefined){
      if(!/^(0|[1-9][0-9]*)$/.test(retryAfter))throw Error('Invalid Retry-After');
      if(BigInt(retryAfter)>2147483n)return Object.freeze({state:'UNKNOWN',original:found,queries,reason:'RETRY_AFTER_OUTSIDE_TIMER_RANGE'});
      wait=Math.max(wait,Number(retryAfter)*1000);
     }
     await delay(wait,controller.signal);
    }
    throw Error('Unreachable recovery budget');
   }finally{unregister();signal?.removeEventListener('abort',abort);this.#active.delete(controller)}
  };
  const flight=run();this.#active.set(controller,flight);return flight;
 }
 async close(){this.#closed=true;for(const c of this.#active.keys())c.abort();await Promise.allSettled([...this.#active.values()]);this.#active.clear()}
}
