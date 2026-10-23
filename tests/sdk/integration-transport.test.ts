import {test, expect, vi} from 'vitest';
import {createEventStream, type SSEFrame} from '../../packages/sdk-ts/src/sse.ts';
import {createServer} from 'node:http';
import {randomBytes, createHash} from 'node:crypto';
import {appendFileSync, readFileSync} from 'node:fs';
import {transportDescriptors} from '../../packages/sdk-ts/src/generated/transport.ts';
import {operationRoutes} from '../../packages/sdk-ts/src/generated/contracts.ts';
import type {Input,Profile} from '../../packages/sdk-ts/src/client.ts';

const encode = (s:string) => new TextEncoder().encode(s);
function stream(chunks:Uint8Array[]) {
 let cancelled=0;
 const body=new ReadableStream<Uint8Array>({
  start(c){for(const chunk of chunks)c.enqueue(chunk);c.close()},
  cancel(){cancelled++}
 });
 return {body,get cancelled(){return cancelled}};
}
function validate(frame:SSEFrame) {
 const data=frame.data as Record<string,unknown>;
 if(!data || typeof data!=='object' || data['ok']!==true)throw Error('Invalid test payload');
}

test('SDKSSEFramingAndReset', async()=>{
 const line='event: update\r\nid: cursor-1\r\ndata: {"ok":true,"text":"界"}\r\n\r\n';
 const bytes=encode(line);
 const split=stream(Array.from(bytes,b=>new Uint8Array([b])));
 const resource=createEventStream(split.body,validate,()=>true);
 const iterator=resource[Symbol.asyncIterator]();
 const first=await iterator.next();
 expect(first.done).toBe(false);expect(resource.cursor).toBeNull();
 expect(first.value!.data).toEqual({ok:true,text:'界'});
 expect(()=>resource.accept({...first.value!})).toThrow();
 resource.accept(first.value!);expect(resource.cursor).toBe('cursor-1');
 expect((await iterator.next()).done).toBe(true);expect(split.body.locked).toBe(false);

 const cr=stream([encode('event: update\rid: cr-id\rdata: {"ok":true}\r\r')]);
 const crResource=createEventStream(cr.body,validate,()=>true);const accepted:string[]=[];
 for await(const f of crResource){crResource.accept(f);accepted.push(f.id)}
 expect(accepted).toEqual(['cr-id']);expect(cr.body.locked).toBe(false);
 const multi=stream([encode(': keepalive\nevent: update\nid: multiline\ndata: {"ok":\ndata: true}\n\n')]);
 const mr=createEventStream(multi.body,validate,()=>true);let multiline=0;
 for await(const frame of mr){mr.accept(frame);multiline++}expect(multiline).toBe(1);expect(mr.cursor).toBe('multiline');

 for(const invalid of [
  encode('event: update\nid: x\ndata: {"ok":true}'),
  encode('event: update\nid: x\ndata: {"ok":false}\n\n'),
  encode('event: update\nid: x\u0000\ndata: {"ok":true}\n\n'),
  new Uint8Array([0xff]),encode('data: '+'x'.repeat(256*1024))
 ]){
  const s=stream([invalid]);const r=createEventStream(s.body,validate,()=>true);
  await expect(async()=>{for await(const _ of r){/* No invalid frame may escape. */}}).rejects.toThrow();
  expect(r.cursor).toBeNull();expect(s.body.locked).toBe(false);
 }
 let current=true;
 const changed=stream([encode(line)]);const stale=createEventStream(changed.body,validate,()=>current);
 const it=stale[Symbol.asyncIterator]();const f=await it.next();current=false;
 expect(()=>stale.accept(f.value!)).toThrow();await expect(it.next()).rejects.toThrow();
 expect(stale.cursor).toBeNull();expect(changed.body.locked).toBe(false);

 const reset=stream([encode('event: view_reset\nid: reset-id\ndata: {"ok":true}\n\n'+line)]);
 const rr=createEventStream(reset.body,validate,()=>true);let count=0;
 for await(const f of rr){count++;rr.accept(f)}
 expect(count).toBe(1);expect(rr.cursor).toBe('reset-id');expect(reset.body.locked).toBe(false);
 const controller=new AbortController();let cancelCount=0;
 const waiting=new ReadableStream<Uint8Array>({cancel(){cancelCount++}});
 const aborted=createEventStream(waiting,validate,()=>true,controller.signal);
 const next=aborted[Symbol.asyncIterator]().next();controller.abort();
 await next.catch(()=>{});await aborted.close();
 expect(cancelCount).toBe(1);expect(waiting.locked).toBe(false);expect(aborted.cursor).toBeNull();
});

test('SDKTransport65AndProfileValidation', async()=>{
 const schemas=JSON.parse(readFileSync(new URL('../../contracts/generated/openapi.json',import.meta.url),'utf8')).components.schemas;
 // Keep the historical test identity, but check the complete current source operation set.
 const source=JSON.parse(readFileSync(new URL('../../contracts/source/openapi.yaml',import.meta.url),'utf8')) as {paths:Record<string,Record<string,{operationId?:string}>>};
 const sourceOperations=Object.values(source.paths).flatMap(item=>Object.entries(item)
  .filter(([method])=>['get','put','post','delete','options','head','patch','trace'].includes(method))
  .map(([,operation])=>operation.operationId));
 expect(sourceOperations.every(id=>typeof id==='string'&&id.length>0)).toBe(true);
 expect(new Set(sourceOperations).size).toBe(sourceOperations.length);
 expect(sourceOperations).toEqual(expect.arrayContaining(['getContentPackageCapabilities','preflightContentPackage']));
 const example=schemas.PlayerSelfConsentSnapshot.examples[0];
 const error=schemas.Error.examples[0];
 const bearer=randomBytes(24).toString('hex');
 if(!process.env['LOREVIVA_TEST_CUSTODY_FILE'])throw Error('Capture custody required');
 appendFileSync(process.env['LOREVIVA_TEST_CUSTODY_FILE'],JSON.stringify([bearer])+'\n');
 let mode='consent',fixture:unknown=null;let calls=0;let receivedBody='',receivedURL='',receivedHeaders:Record<string,string|string[]|undefined>={};
 const server=createServer(async(req,res)=>{
  calls++;receivedURL=req.url??'';receivedHeaders=req.headers;
  const chunks:Buffer[]=[];for await(const b of req)chunks.push(Buffer.from(b));receivedBody=Buffer.concat(chunks).toString('utf8');
  res.setHeader('Loreviva-Client-Contract',req.headers['loreviva-client-contract']??'public_v1');
  if(mode==='wrongmime'){res.setHeader('Content-Type','text/plain');res.end(JSON.stringify(example));return}
  if(mode==='unknown'){res.statusCode=299;res.setHeader('Content-Type','application/json');res.end('{}');return}
  if(mode==='invalid'){res.setHeader('Content-Type','application/json');res.end('{"arbitrary":true}');return}
  if(mode==='media'){res.setHeader('Content-Type','audio/wav');res.setHeader('Content-Length','4');res.end('RIFF');return}
  if(mode==='partial'){res.statusCode=206;res.setHeader('Content-Type','audio/wav');res.setHeader('Content-Range','bytes 0-1/4');res.setHeader('Content-Length','2');res.end('RI');return}
  if(mode==='416'){res.statusCode=416;res.setHeader('Content-Type','application/json');res.end(JSON.stringify(error));return}
  if(mode==='events'){
   res.setHeader('Content-Type','text/event-stream');
   const page=schemas[req.headers['loreviva-client-contract']==='integration_r0_v1'?'IntegrationEventPage':'EventPage'].examples[0];
   res.end('event: update\nid: cursor_test_00000001\ndata: '+JSON.stringify(page)+'\n\n'+'event: view_reset\nid: cursor_test_00000002\ndata: '+JSON.stringify(error)+'\n\n');return;
  }
  res.setHeader('Content-Type','application/json');
  if(mode==='fixture'){res.end(JSON.stringify(fixture));return}
  if(mode==='provenance'){
   const projection={projection:'provenance_v1',memory:schemas.Memory.examples[0],evidence_domain:'PLAYER_STATEMENT',occurred_at_estimated:false,parent_memory_ids:[],source_epoch_ids:[]};
   res.end(JSON.stringify(req.url?.includes('/memories?')?{scope:schemas.Memory.examples[0].scope,memories:[projection]}:projection));return;
  }
  if(mode==='error'){res.statusCode=409;res.end(JSON.stringify(error));return}
  res.end(JSON.stringify(example));
 });
 await new Promise<void>(resolve=>server.listen(0,'127.0.0.1',resolve));
 const address=server.address();if(!address||typeof address==='string')throw Error('Owned loopback listener required');
 const baseUrl=`http://127.0.0.1:${address.port}`;
 try{
  // Missing SDK owner is a wiring gap, kept separate from a response-validation failure.
  const module=await import('../../packages/sdk-ts/src/client.ts').catch(()=>null);
  expect(module!==null,'Source transport owner must exist before its mechanism can be tested').toBe(true);
  if(!module)return;
  const {createIntegrationTransport,createPreparedIntegrationOwner}=module;
  for(const profile of ['public_v1','integration_r0_v1'] as const){
   const desc=transportDescriptors[profile];expect(Object.keys(desc).sort()).toEqual([...sourceOperations].sort());
   for(const key of Object.keys(desc) as (keyof typeof desc)[]){
    expect(desc[key].method).toBe(operationRoutes[key].method);expect(desc[key].path).toBe(operationRoutes[key].path);
   }
   const options={baseUrl,profile,role:'CLIENT' as const,getBearer:()=>bearer};
   const client=createIntegrationTransport(options);
   const input=profile==='public_v1'?{headers:{'Loreviva-Client-Contract':'public_v1' as const}}:{headers:{'Loreviva-Client-Contract':'integration_r0_v1' as const}};
   const result=await client.call('getPlayerSelfConsent',input);
   expect(result.status).toBe('200');expect(result.body).toEqual(example);
   expect(receivedHeaders['authorization']==='Bearer '+bearer).toBe(true);
   for(const k of ['cookie','x-loreviva-csrf','origin'])expect(receivedHeaders[k]).toBeUndefined();
   expect(receivedBody).toBe('');
   const before=calls;
   await expect(client.call('getPlayerSelfConsent',{...input,headers:{...input.headers,Authorization:'untrusted'}} as never)).rejects.toThrow();
   await expect(client.call('createInstance' as never,{} as never)).rejects.toThrow();
   await expect(client.call('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':profile==='public_v1'?'integration_r0_v1':'public_v1'}} as never)).rejects.toThrow();
   expect(calls).toBe(before);
   mode='wrongmime';await expect(client.call('getPlayerSelfConsent',input)).rejects.toThrow();
   mode='invalid';await expect(client.call('getPlayerSelfConsent',input)).rejects.toThrow();
   mode='unknown';await expect(client.call('getPlayerSelfConsent',input)).rejects.toThrow();
   mode='error';const reset=await client.call('getPlayerSelfConsent',input);
   expect(reset.status).toBe('409');expect(reset.body).toEqual(error);
   const owner=createPreparedIntegrationOwner(options),foreign=createPreparedIntegrationOwner(options);
   const body={operation:'DELETE' as const,memory_ids:['memory1'] as [string,...string[]],expected_revisions:{memory1:'1'},reason:'  exact reason  '};
   const command={path:{instance_id:'world1'},headers:{...input.headers,'Idempotency-Key':'original1'},body};
   const prepared=await owner.prepare('governMemories',command as Input<Profile,'governMemories'>);
   const snapshot=owner.inspect(prepared);const wire=JSON.stringify(body);
   expect(snapshot.bodySHA256).toBe(createHash('sha256').update(wire).digest('hex'));
   body.reason='mutated';snapshot.body.fill(0);
   await expect(foreign.sendPreparedOnce(prepared)).rejects.toThrow();
   await expect(owner.sendPreparedOnce({...prepared})).rejects.toThrow();
   await owner.sendPreparedOnce(prepared);
   expect(receivedBody).toBe(wire);expect(receivedURL).toBe('/world/v1/instances/world1/memory-governance');
   await expect(owner.sendPreparedOnce(prepared)).rejects.toThrow();
   const pending=await owner.prepare('getPlayerSelfConsent',input);await owner.close();
   await expect(owner.sendPreparedOnce(pending)).rejects.toThrow();
   await foreign.close();
   mode='media';const media=await client.call('readRuntimeMedia',{path:{asset_id:'media1'},headers:input.headers} as Input<Profile,'readRuntimeMedia'>);
   expect(media.status).toBe('200');if(media.status==='200')expect(Array.from(await media.body.read())).toEqual(Array.from(encode('RIFF')));
   mode='416';const badRange=await client.call('readRuntimeMedia',{path:{asset_id:'media1'},headers:{...input.headers,Range:'bytes=999-'}} as Input<Profile,'readRuntimeMedia'>);
   expect(badRange.status).toBe('416');expect(badRange.body).toEqual(error);
   mode='provenance';const provenance=await client.call('getMemory',{path:{instance_id:'world1',memory_id:'memory1'},query:{include:'provenance_v1'},headers:input.headers} as Input<Profile,'getMemory'> & {query:{include:'provenance_v1'}});
   expect(provenance.status).toBe('200');if(provenance.status==='200')expect(provenance.body.projection).toBe('provenance_v1');
   const provenanceList=await client.call('listMemories',{path:{instance_id:'world1'},query:{include:'provenance_v1'},...input});
   expect(provenanceList.status).toBe('200');if(provenanceList.status==='200')expect(provenanceList.body.memories[0]?.projection).toBe('provenance_v1');
   // A default metadata call must not accept an unsolicited provenance/body projection.
   const defaultMemory=profile==='public_v1'?{path:{instance_id:'world1',memory_id:'memory1'},headers:{'Loreviva-Client-Contract':'public_v1' as const}}:{path:{instance_id:'world1',memory_id:'memory1'},headers:{'Loreviva-Client-Contract':'integration_r0_v1' as const}};
   await expect(client.call('getMemory',defaultMemory)).rejects.toThrow();
   mode='events';const events=await client.call('streamEvents',{path:{instance_id:'world1'},query:{cursor:'cursor_test_00000000'},headers:input.headers} as Input<Profile,'streamEvents'>);
   expect(events.status).toBe('200');
   if(events.status==='200'){const seen:string[]=[];for await(const frame of events.body){events.body.accept(frame);seen.push(frame.event)}expect(seen).toEqual(['update','view_reset']);expect(events.body.cursor).toBe('cursor_test_00000002')}
   const named=await client.streamEvents({path:{instance_id:'world1'},query:{cursor:'cursor_test_00000000'},...input});
   await named.close();
   mode='fixture';fixture=schemas[profile==='public_v1'?'TurnView':'IntegrationTurnView'].examples[0];
   expect((await client.call('getTurn',{path:{turn_id:'turn1'},...input})).body).toEqual(fixture);
   fixture=schemas[profile==='public_v1'?'EventPage':'IntegrationEventPage'].examples[0];
   expect((await client.call('getEvents',{path:{instance_id:'world1'},query:{cursor:'cursor_test_00000000'},...input})).body).toEqual(fixture);
   fixture=schemas[profile==='public_v1'?'MemoryPage':'PlayerMemoryMetadataPage'].examples[0];
   expect((await client.call('listMemories',{path:{instance_id:'world1'},...input})).body).toEqual(fixture);
   fixture=schemas[profile==='public_v1'?'Memory':'PlayerMemoryMetadata'].examples[0];
   expect((await client.call('getMemory',{path:{instance_id:'world1',memory_id:'memory1'},...input})).body).toEqual(fixture);
   mode='partial';const partial=await client.fetchRuntimeMedia({path:{asset_id:'media1'},...input});
   expect(partial.status).toBe(206);expect(Array.from(await partial.read())).toEqual(Array.from(encode('RI')));
   mode='416';await expect(client.fetchRuntimeMedia({path:{asset_id:'media1'},...input})).rejects.toMatchObject({status:'416',body:error});
   await client.close();await expect(client.call('getPlayerSelfConsent',input)).rejects.toThrow();
   mode='consent';
  }
  expect('createReferenceBffClient' in module,'The browser local-cookie hop must be separate from the platform credential hop').toBe(true);
  if(!('createReferenceBffClient' in module))return;
  const csrf=randomBytes(24).toString('hex');appendFileSync(process.env['LOREVIVA_TEST_CUSTODY_FILE']!,JSON.stringify([csrf])+'\n');
  const localRequests:{url:string;init:RequestInit}[]=[];let localMode='normal';
  let delayedResolve:((response:Response)=>void)|null=null;
  vi.stubGlobal('location',new URL(baseUrl));
  vi.stubGlobal('fetch',async(url:RequestInfo|URL,init:RequestInit={})=>{
   localRequests.push({url:String(url),init});const path=new URL(String(url)).pathname;
   if(localMode==='delay'&&(path.startsWith('/bff/client/')||path==='/bff/find-original'))return new Promise<Response>(resolve=>{delayedResolve=resolve});
   if(localMode==='local-error')return Response.json({format:'lv-bff-error-v1',code:'UNKNOWN',message:'Query the original command.',retryable:false,recovery_action:'QUERY_ORIGINAL'},{status:503,headers:{'X-Loreviva-Bff-Error':'1'}});
   if(localMode==='fake-local-error')return Response.json(error,{status:503,headers:{'X-Loreviva-Bff-Error':'1'}});
   if(localMode==='bad-session')return Response.json({format:'lv-bff-session-v1',authenticated:true,identity:['c','p','w','integration_r0_v1'],csrf,client:null,token:'forbidden'});
   if(path==='/bff/logout')return new Response(null,{status:204});
   if(path==='/bff/login'||path==='/bff/me'||path==='/bff/exchange')return Response.json({format:'lv-bff-session-v1',authenticated:true,identity:['c','p','w','integration_r0_v1'],csrf,client:null});
   if(path==='/bff/commands')return Response.json(error,{status:409,headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
   if(path==='/bff/find-original'||path.startsWith('/bff/query-original/')){
    return Response.json({format:'lv-bff-original-v1',original_id:'a'.repeat(32),operation:'submitTurn',state:'OBSERVED',
     task_id:schemas.OriginalTurnIntakeReceipt.examples[0].acceptance.task.task_id,
     observation:{status:200,body:schemas.OriginalTurnIntakeReceipt.examples[0]}});
   }
   if(path==='/bff/client/streamEvents')return new Response('event: update\nid: browser_cursor_0001\ndata: '+JSON.stringify(schemas.IntegrationEventPage.examples[0])+'\n\n',{headers:{'Content-Type':'text/event-stream','Loreviva-Client-Contract':'integration_r0_v1'}});
   if(path==='/bff/client/readRuntimeMedia'){
    if(localMode==='range-error')return Response.json(error,{status:416});
    return new Response('RI',{status:206,headers:{'Content-Type':'audio/wav','Content-Length':'2','Content-Range':'bytes 0-1/4'}});
   }
   return Response.json(example,{headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
  });
  try{
   const browser=module.createReferenceBffClient('integration_r0_v1');
   await browser.login({login:'synthetic',credential:bearer});
   await browser.read('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
   const last=localRequests.at(-1)!;expect(last.url.startsWith(baseUrl+'/bff/client/getPlayerSelfConsent?args=')).toBe(true);
   expect(last.init.credentials).toBe('same-origin');expect(last.init.redirect).toBe('error');expect(last.init.cache).toBe('no-store');
   const h=new Headers(last.init.headers);expect(h.has('authorization')).toBe(false);expect(h.has('cookie')).toBe(false);expect(h.has('x-loreviva-csrf')).toBe(false);
   const before=localRequests.length;await expect(browser.read('submitTurn' as never,{} as never)).rejects.toThrow();expect(localRequests).toHaveLength(before);
   const bodies={
    createInstance:'CreateInstanceRequest',controlInstance:'ControlRequest',createQuote:'QuoteRequest',submitTurn:'TurnRequest',
    commandPlayerSelfConsent:'PlayerSelfConsentCommandRequest',governMemories:'MemoryGovernanceCommandV1',cancelTask:'CancelRequest'
   } as const;
   for(const [operation,schema] of Object.entries(bodies)){
    const descriptor=transportDescriptors.integration_r0_v1[operation as keyof typeof bodies];
    const path=Object.fromEntries(descriptor.parameters.filter(p=>p.in==='path').map(p=>[p.name,p.name==='task_id'?'task1':'world1']));
    const command={...(Object.keys(path).length?{path}:{}),headers:{'Loreviva-Client-Contract':'integration_r0_v1','Idempotency-Key':'original_browser_1'},
     body:operation==='governMemories'?{operation:'DELETE',memory_ids:['memory1'],expected_revisions:{memory1:'1'},reason:'  exact reason  '}:schemas[schema].examples[0]};
    const result=await browser.command(operation as keyof typeof bodies,command as never);
    expect(result.status).toBe('409');
    const request=localRequests.at(-1)!,envelope=JSON.parse(new TextDecoder().decode(request.init.body as Uint8Array));
    expect(request.url).toBe(baseUrl+'/bff/commands');expect(request.init.method).toBe('POST');
    expect(envelope).toEqual({format:'lv-bff-command-v1',operation,profile:'integration_r0_v1',original_key:'original_browser_1',path,query:[],body:command.body});
    const headers=new Headers(request.init.headers);expect(headers.get('x-loreviva-csrf')===csrf).toBe(true);
    expect(headers.has('authorization')).toBe(false);expect(headers.has('cookie')).toBe(false);
   }
   const located=await browser.findOriginal('submitTurn','original_browser_1');
   expect(located.original_id).toBe('a'.repeat(32));expect(located.observation?.body).toEqual(schemas.OriginalTurnIntakeReceipt.examples[0]);
   const lookup=localRequests.at(-1)!;
   expect(lookup.url).toBe(baseUrl+'/bff/find-original');expect(lookup.init.method).toBe('POST');expect(lookup.init.credentials).toBe('same-origin');
   expect(JSON.parse(new TextDecoder().decode(lookup.init.body as Uint8Array))).toEqual({format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'original_browser_1'});
   expect(new Headers(lookup.init.headers).has('authorization')).toBe(false);expect(new Headers(lookup.init.headers).get('x-loreviva-csrf')).toBe(csrf);
   expect((await browser.queryOriginal(located.original_id)).operation).toBe('submitTurn');
   const lookupCount=localRequests.length;
   await expect(browser.findOriginal('exchangeClientSession' as never,'original_browser_1')).rejects.toThrow();
   await expect(browser.findOriginal('submitTurn','bad key')).rejects.toThrow();expect(localRequests).toHaveLength(lookupCount);
   localMode='local-error';await expect(browser.read('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':'integration_r0_v1'}})).rejects.toMatchObject({name:'BffResponseError',code:'UNKNOWN',recoveryAction:'QUERY_ORIGINAL'});
   localMode='fake-local-error';await expect(browser.read('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':'integration_r0_v1'}})).rejects.toThrow();
   localMode='bad-session';await expect(browser.me()).rejects.toThrow();
   localMode='normal';await browser.me();await browser.exchange({originalKey:'exchange_original'});
   const stream=await browser.read('streamEvents',{path:{instance_id:'world1'},query:{cursor:'browser_cursor_0000'},headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
   expect(stream.status).toBe('200');
   if(stream.status==='200'){let frames=0;for await(const frame of stream.body){stream.body.accept(frame);frames++}expect(frames).toBe(1);expect(stream.body.cursor).toBe('browser_cursor_0001')}
   const binary=await browser.read('readRuntimeMedia',{path:{asset_id:'media1'},headers:{'Loreviva-Client-Contract':'integration_r0_v1',Range:'bytes=0-1'}});
   expect(binary.status).toBe('206');if(binary.status==='206')expect(Array.from(await binary.body.read())).toEqual([82,73]);
   const rangeRequest=localRequests.at(-1)!;expect(new Headers(rangeRequest.init.headers).has('range')).toBe(false);
   expect(JSON.parse(new URL(rangeRequest.url).searchParams.get('args')!).range).toBe('bytes=0-1');
   localMode='range-error';const rangeError=await browser.read('readRuntimeMedia',{path:{asset_id:'media1'},headers:{'Loreviva-Client-Contract':'integration_r0_v1',Range:'bytes=99-'}});
   expect(rangeError.status).toBe('416');expect(rangeError.body).toEqual(error);
   localMode='delay';const late=browser.read('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
   expect(delayedResolve).not.toBeNull();
   await browser.logout();expect(new Headers(localRequests.at(-1)!.init.headers).get('x-loreviva-csrf')===csrf).toBe(true);
   delayedResolve!(Response.json(example));await expect(late).rejects.toThrow();
   localMode='normal';await browser.login({login:'synthetic',credential:bearer});
   localMode='delay';const lateFind=browser.findOriginal('submitTurn','original_browser_1'),ignoredFind=lateFind.catch(()=>null);
   await browser.logout();
   delayedResolve!(Response.json({format:'lv-bff-original-v1',original_id:'a'.repeat(32),operation:'submitTurn',state:'UNKNOWN',task_id:null,observation:null}));
   expect(await ignoredFind).toBeNull();
   await browser.close();await expect(browser.me()).rejects.toThrow();
  }finally{vi.unstubAllGlobals()}
 }finally{server.closeAllConnections();await new Promise<void>((resolve,reject)=>server.close(e=>e?reject(e):resolve()))}
});
