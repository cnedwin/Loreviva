import validators from './generated/validators.cjs';
import {ViewOwner,type ViewToken} from './cache.ts';
import type {MediaRef,RuntimeMediaRef} from './generated/contracts.ts';
export interface AuthorizedMediaBody {
 readonly status:200|206;readonly mime:string;readonly contentRange:string|null;
 read():Promise<Uint8Array>;cancel(reason?:unknown):Promise<void>;close():Promise<void>;
}
export interface RuntimeMediaPort {
 fetchRuntimeMedia(input:{path:{asset_id:string};headers:{'Loreviva-Client-Contract':'public_v1'|'integration_r0_v1'}},signal?:AbortSignal):Promise<AuthorizedMediaBody>;
}
export interface MediaSink extends EventTarget {src:string;play():Promise<void>;pause():void;removeAttribute(name:string):void;load():void;remove():void}
export type MediaSinkFactory=()=>MediaSink;
interface Playback {sink:MediaSink;listener:EventListener;controller:AbortController;url:string|null;resource:AuthorizedMediaBody|null;unregister:()=>void;released:boolean}
/** Each play fetches again. Blob URLs carry no bearer and never survive this view. */
export class MediaController {
 #port:RuntimeMediaPort;#view:ViewOwner;#createSink:MediaSinkFactory;#issued=new WeakSet<MediaSink>();#active:Playback|null=null;#closed=false;#pending=new Set<Promise<void>>();
 constructor(port:RuntimeMediaPort,view:ViewOwner,createSink:MediaSinkFactory){this.#port={fetchRuntimeMedia:port.fetchRuntimeMedia.bind(port)};this.#view=view;this.#createSink=createSink}
 #dispose(slot:Playback){
  if(slot.released)return;slot.released=true;slot.controller.abort();slot.unregister();
  for(const kind of ['ended','pause','error'])slot.sink.removeEventListener(kind,slot.listener);
  if(this.#active===slot)this.#active=null;
  slot.sink.pause();slot.sink.removeAttribute('src');slot.sink.load();slot.sink.remove();
  if(slot.url!==null){URL.revokeObjectURL(slot.url);slot.url=null}
  void slot.resource?.close().catch(()=>{});
 }
 play(raw:MediaRef|RuntimeMediaRef,token:ViewToken):Promise<void>{
  if(this.#closed||!this.#view.current(token))return Promise.reject(Error('Playback view is closed or stale'));
  if(this.#active)this.#dispose(this.#active);
  const profile=this.#view.identity?.profile;
  const ref=JSON.parse(JSON.stringify(raw)) as RuntimeMediaRef;
  if((profile!=='public_v1'&&profile!=='integration_r0_v1')||!(profile==='public_v1'?validators.schema_MediaRef(ref):validators.schema_RuntimeMediaRef(ref))
   ||ref.media_state!=='READY'||ref.media_kind!=='AUDIO'||!ref.asset_id)return Promise.reject(Error('No current playable audio descriptor'));
  if(profile==='integration_r0_v1'&&(ref.mime_type!=='audio/wav'||ref.byte_length!=='160044'||ref.content_path!==`/world/v1/runtime-media/${ref.asset_id}/content`))return Promise.reject(Error('Runtime media identity differs'));
  const sink=this.#createSink();if(!(sink instanceof EventTarget)||this.#issued.has(sink))return Promise.reject(Error('Playback requires a fresh native target'));
  this.#issued.add(sink);
  const slot:Playback={sink,listener:()=>{},controller:new AbortController(),url:null,resource:null,unregister:()=>{},released:false};this.#active=slot;
  slot.listener=(event)=>{if(event.currentTarget===slot.sink)this.#dispose(slot)};
  for(const kind of ['ended','pause','error'])slot.sink.addEventListener(kind,slot.listener);
  slot.unregister=this.#view.register(token,()=>this.#dispose(slot));
  const current=()=>!this.#closed&&!slot.released&&this.#active===slot&&this.#view.current(token);
  const run=async()=>{
   try{
    slot.resource=await this.#port.fetchRuntimeMedia({path:{asset_id:ref.asset_id!},headers:{'Loreviva-Client-Contract':profile}},slot.controller.signal);
    if(!current())throw Error('Playback view changed');
    if(slot.resource.status!==200||slot.resource.mime!=='audio/wav')throw Error('Whole WAV required for playback');
    const bytes=await slot.resource.read();if(!current())throw Error('Playback view changed');
    if(bytes.length!==160044)throw Error('Unsupported runtime WAV length');
    const d=new DataView(bytes.buffer,bytes.byteOffset,bytes.byteLength),text=(at:number,n:number)=>new TextDecoder().decode(bytes.subarray(at,at+n));
    if(text(0,4)!=='RIFF'||d.getUint32(4,true)!==160036||text(8,4)!=='WAVE'||text(12,4)!=='fmt '||d.getUint32(16,true)!==16
     ||d.getUint16(20,true)!==1||d.getUint16(22,true)!==1||d.getUint32(24,true)!==16000||d.getUint32(28,true)!==32000
     ||d.getUint16(32,true)!==2||d.getUint16(34,true)!==16||text(36,4)!=='data'||d.getUint32(40,true)!==160000)throw Error('Unsupported runtime WAV format');
    if(profile==='integration_r0_v1'){
     const digest=Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',new Uint8Array(bytes))),x=>x.toString(16).padStart(2,'0')).join('');
     if(digest!==ref.content_sha256)throw Error('Runtime WAV hash differs');
    }
    if(!current())throw Error('Playback view changed');
    slot.url=URL.createObjectURL(new Blob([new Uint8Array(bytes)],{type:'audio/wav'}));
    if(!current())throw Error('Playback view changed');slot.sink.src=slot.url;
    if(!current())throw Error('Playback view changed');await slot.sink.play();
    if(!current())throw Error('Playback view changed');
   }catch(e){this.#dispose(slot);throw e}
   finally{await slot.resource?.close()}
  };
  const flight=run();this.#pending.add(flight);void flight.finally(()=>this.#pending.delete(flight)).catch(()=>{});return flight;
 }
 async close(){this.#closed=true;if(this.#active)this.#dispose(this.#active);await Promise.allSettled([...this.#pending])}
}
export function createAuthorizedMediaBody(response:Response,current:()=>boolean,signal?:AbortSignal):AuthorizedMediaBody {
 if(response.status!==200&&response.status!==206)throw Error('Invalid binary success status');
 const mime=response.headers.get('content-type')?.split(';')[0]?.trim().toLowerCase()??'';
 if(!['audio/wav','application/octet-stream','image/png','image/jpeg','image/webp'].includes(mime))throw Error('Unsupported binary MIME');
 const range=response.headers.get('content-range');const length=response.headers.get('content-length');
 let expected:number|undefined;
 if(length!==null){if(!/^(0|[1-9][0-9]*)$/.test(length)||BigInt(length)>16777216n)throw Error('Invalid binary length');expected=Number(length)}
 if(response.status===206){const m=/^bytes ([0-9]+)-([0-9]+)\/([0-9]+)$/.exec(range??'');if(!m)throw Error('Missing partial range');const first=BigInt(m[1]!),last=BigInt(m[2]!),size=BigInt(m[3]!);if(last<first||last>=size||size>16777216n)throw Error('Invalid partial range');const count=Number(last-first+1n);if(expected!==undefined&&expected!==count)throw Error('Partial length differs');expected=count}
 else if(range!==null)throw Error('Unexpected whole-body range');
 const reader=response.body?.getReader();let closed=false,started=false,closePromise:Promise<void>|undefined;
 async function close(reason?:unknown){if(closePromise)return closePromise;closed=true;closePromise=(async()=>{try{await reader?.cancel(reason)}finally{reader?.releaseLock();signal?.removeEventListener('abort',abort)}})();return closePromise}
 const abort=()=>{void close(signal?.reason).catch(()=>{})};signal?.addEventListener('abort',abort,{once:true});if(signal?.aborted)abort();
 return Object.freeze({status:response.status as 200|206,mime,contentRange:range,cancel:close,close:()=>close(),async read(){
  if(started||closed||!current())throw Error('Media is closed, consumed or stale');started=true;const chunks:Uint8Array[]=[];let size=0;
  try{if(!reader)throw Error('Missing binary body');for(;;){if(!current()||signal?.aborted)throw Error('Media authorization view changed');const x=await reader.read();if(x.done)break;size+=x.value.byteLength;if(size>16777216||(expected!==undefined&&size>expected))throw Error('Binary exceeds bound');for(let at=0;at<x.value.byteLength;at+=16384){
    if(closed||!current()||signal?.aborted)throw Error('Media authorization view changed');
    chunks.push(x.value.slice(at,Math.min(at+16384,x.value.byteLength)));
   }}
   if(expected!==undefined&&size!==expected)throw Error('Truncated binary');if(!current())throw Error('Media view changed');const out=new Uint8Array(size);let at=0;for(const c of chunks){if(closed||!current()||signal?.aborted)throw Error('Media authorization view changed');out.set(c,at);at+=c.length}return out;
  }finally{await close()}
 } });
}
