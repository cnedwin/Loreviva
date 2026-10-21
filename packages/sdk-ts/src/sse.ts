/** Completed, validated frames are observations; cursor acceptance is explicit. */
export interface SSEFrame { readonly event: 'update'|'view_reset'; readonly id: string; readonly data: unknown }
export interface AsyncDisposableEventStream extends AsyncIterable<SSEFrame> {
 readonly cursor: string|null;
 accept(frame:SSEFrame): void;
 cancel(reason?:unknown):Promise<void>;
 close():Promise<void>;
}
export function createEventStream(body:ReadableStream<Uint8Array>, validate:(frame:SSEFrame)=>void, current:()=>boolean, signal?:AbortSignal):AsyncDisposableEventStream {
 const reader=body.getReader();const decoder=new TextDecoder('utf-8',{fatal:true});
 const owned=new WeakSet<object>();let cursor:string|null=null,closed=false,iterated=false,total=0,buffer='',frameBytes=0;
 let event='',id='',data:string[]=[];let closePromise:Promise<void>|undefined;
 const encoder=new TextEncoder();
 async function close(reason?:unknown){if(closePromise)return closePromise;closed=true;closePromise=(async()=>{try{await reader.cancel(reason)}finally{reader.releaseLock();signal?.removeEventListener('abort',abort)}})();return closePromise}
 const abort=()=>{void close(signal?.reason).catch(()=>{})};signal?.addEventListener('abort',abort,{once:true});if(signal?.aborted)abort();
 function line(text:string):SSEFrame|null{
  if(text===''){
   if(data.length===0){event='';id='';frameBytes=0;return null}
   if(!['update','view_reset'].includes(event)||!id||id.includes('\0'))throw Error('Invalid SSE frame identity');
   const value:unknown=JSON.parse(data.join('\n'));
   const frame=Object.freeze({event:event as SSEFrame['event'],id,data:value});
   validate(frame);if(!current())throw Error('View changed');owned.add(frame);
   event='';id='';data=[];frameBytes=0;return frame;
  }
  frameBytes+=encoder.encode(text).byteLength+1;if(frameBytes>256*1024)throw Error('SSE frame exceeds limit');
  if(text.startsWith(':'))return null;
  const at=text.indexOf(':');const field=at<0?text:text.slice(0,at);let value=at<0?'':text.slice(at+1);if(value.startsWith(' '))value=value.slice(1);
  if(field==='event')event=value;else if(field==='id'){if(value.includes('\0'))throw Error('Invalid SSE ID');id=value}else if(field==='data')data.push(value);
  return null;
 }
 const resource:AsyncDisposableEventStream={
  get cursor(){return cursor},
  accept(frame){if(closed||!current()||!owned.has(frame))throw Error('Frame is not current and owned');owned.delete(frame);cursor=frame.id},
  cancel:close,close:()=>close(),
  async *[Symbol.asyncIterator](){
   if(iterated||closed)throw Error('Stream is closed or already consumed');iterated=true;
   try{
    for(;;){
     if(!current()||signal?.aborted)throw Error('View changed or aborted');
     const chunk=await reader.read();
     if(!current()||signal?.aborted)throw Error('View changed or aborted');
     if(chunk.done)buffer+=decoder.decode();
     else {total+=chunk.value.byteLength;if(total>8*1024*1024)throw Error('SSE connection exceeds limit');buffer+=decoder.decode(chunk.value,{stream:true})}
     // A final CR is a line ending; only a nonterminal chunk waits for a possible LF.
     for(;;){const index=buffer.search(/[\r\n]/);if(index<0)break;if(!chunk.done&&buffer[index]==='\r'&&index===buffer.length-1)break;
      const width=buffer[index]==='\r'&&buffer[index+1]==='\n'?2:1;const text=buffer.slice(0,index);buffer=buffer.slice(index+width);
      const frame=line(text);if(frame){if(!current())throw Error('View changed');yield frame;if(frame.event==='view_reset')return}
     }
     if(chunk.done){if(buffer||data.length||event||id)throw Error('Incomplete SSE frame');return}
     if(encoder.encode(buffer).byteLength+frameBytes>256*1024)throw Error('Unfinished SSE frame exceeds limit');
    }
   }finally{await close()}
  }
 };
 return Object.freeze(resource);
}
