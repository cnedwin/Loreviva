import {shallowReactive} from 'vue';
import {createReferenceBffClient,BffResponseError,type Profile,type DurableOperation,type ClientGetOperation,type BffSessionView,type BffCustomerResult} from '../../../packages/sdk-ts/src/client.ts';
import {ViewOwner,type ViewToken} from '../../../packages/sdk-ts/src/cache.ts';
import {BffRecoveryController,type BffOriginalReference} from '../../../packages/sdk-ts/src/recovery.ts';
import {MediaController,type MediaSinkFactory} from '../../../packages/sdk-ts/src/media.ts';
import type {AsyncDisposableEventStream} from '../../../packages/sdk-ts/src/sse.ts';
import type {MediaRef,RuntimeMediaRef} from '../../../packages/sdk-ts/src/generated/contracts.ts';

type Data=Record<string,unknown>;
type SourceResult={status:string;protocol:string;body:unknown};
type DisplaySession=Readonly<{identity:NonNullable<BffSessionView['identity']>;client:BffSessionView['client']}>;
interface Original {id:string;operation:DurableOperation<Profile>;state:'PREPARED'|'UNKNOWN'|'OBSERVED';queries:number}
function object(value:unknown):Data{if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Object missing');return value as Data}
function list(value:unknown):Data[]{return Array.isArray(value)?value.map(object):[]}
function string(value:unknown):string{return typeof value==='string'?value:''}

/** Local presentation ownership only. Every access still crosses the current BFF/platform gates. */
export function createReferenceState(profile:Profile='integration_r0_v1'){
 const adapter=createReferenceBffClient(profile),view=new ViewOwner(),recovery=new BffRecoveryController(adapter,view);
 const data=shallowReactive({
  profile,session:null as DisplaySession|null,busy:0,error:'',notice:'尚未登录',capabilities:null as Data|null,
  consent:null as Data|null,snapshot:null as Data|null,quote:null as Data|null,task:null as Data|null,taskAccess:'UNKNOWN' as 'CLIENT'|'SERVICE'|'UNKNOWN',
  packets:[] as Data[],deliveries:[] as Data[],memories:[] as Data[],memoryBody:null as Data|null,
  cursor:'',streaming:false,viewGeneration:0,originals:[] as Original[],customer:null as BffCustomerResult|null,
 });
 const locators=new Map<string,{reference:BffOriginalReference;token:ViewToken;command?:string}>();
 const controllers=new Set<AbortController>();let generation=0,closed=false,stream:AsyncDisposableEventStream|null=null,streamAbort:AbortController|null=null,media:MediaController|null=null,sink:MediaSinkFactory|null=null;
 async function stopMedia(){const old=media;media=null;await old?.close()}
 function mediaOwner(){
  if(!sink)throw Error('音频播放器尚未就绪。');
  if(!media)media=new MediaController({async fetchRuntimeMedia(input,signal){
   const result=await adapter.read('readRuntimeMedia',input as never,signal);
   if((result.status==='200'||result.status==='206')&&result.protocol==='BINARY')return result.body;
   throw Error('当前音频不可播放。');
  }},view,sink);
  return media;
 }
 function clear(){
  view.invalidate();data.viewGeneration++;for(const c of controllers)c.abort();streamAbort?.abort();streamAbort=null;void stream?.close();stream=null;
  locators.clear();data.originals=[];data.quote=null;data.task=null;data.taskAccess='UNKNOWN';data.snapshot=null;data.cursor='';
  data.packets=[];data.deliveries=[];data.memories=[];data.memoryBody=null;data.customer=null;data.streaming=false;
 }
 function enter(session:DisplaySession){
  const [customer,player,world]=session.identity,client=session.client;
  return view.enter({loginGeneration:String(generation),customer,player,world,profile,
   sessionIdentity:JSON.stringify(client),...(client?{viewEpoch:client.view_epoch}:{})});
 }
 function current(token:ViewToken){if(closed||!view.current(token))throw Error('View changed')}
 function token(){if(!data.session)throw Error('请先登录');return view.capture()}
 function world(){if(!data.session)throw Error('请先登录');return data.session.identity[2]}
 function key(){return crypto.randomUUID().replaceAll('-','')}
 function headers(originalKey?:string){return {'Loreviva-Client-Contract':profile,...(originalKey?{'Idempotency-Key':originalKey}:{})}}
 function reset(message:string){clear();if(data.session)enter(data.session);data.notice=message}
 function source(result:SourceResult):Data{
  if(result.protocol!=='JSON')throw Error('Expected structured response');
  const body=object(result.body);
  if(Number(result.status)>=400){
   if(result.status==='409'&&(body.recovery_action==='FETCH_NEW_SNAPSHOT'||body.code==='VIEW_RESET'))reset('视图已变化，请重新读取当前场景。');
   const message=result.status==='401'?'会话已失效，请重新连接。':result.status==='403'?'当前身份没有这项访问权限。':'请求未完成，请检查当前状态或查询原请求。';
   if(result.status==='401'||result.status==='403'){reset('访问资格已变化，私人视图和媒体已清除。');data.error=message}
   throw Error(message);
  }
  return body;
 }
 function epochs(body:Data){
  const versions=body.versions?object(body.versions):body,old=view.identity;
  const nextView=string(versions.view_epoch),nextPrivacy=string(versions.privacy_epoch);
  if(old&&((nextView&&nextView!==old.viewEpoch)||(nextPrivacy&&nextPrivacy!==old.privacyEpoch))){
   clear();
   view.enter({...old,...(nextView?{viewEpoch:nextView}:{}),...(nextPrivacy?{privacyEpoch:nextPrivacy}:{})});
  }
 }
 async function run<T>(action:(t:ViewToken,signal:AbortSignal)=>Promise<T>,blocking=true):Promise<T|undefined>{
  const t=token(),c=new AbortController(),unregister=view.register(t,()=>c.abort());controllers.add(c);if(blocking)data.busy++;data.error='';
  try{return await action(t,c.signal)}
  catch(error){
   if(view.current(t)&&!closed&&error instanceof BffResponseError&&(error.status===401||error.status===403)){
    reset('当前本地身份不再允许访问，私人视图已清除。');data.error='会话或访问资格已变化，请重新连接。';
   }
   if(view.current(t)&&!closed)data.error=error instanceof BffResponseError?
    (error.status===401?'会话已失效，请重新连接。':'原请求结果尚未确认，请使用只读查询。'):
    error instanceof Error?error.message:'请求未完成。';
   return undefined;
  }finally{unregister();controllers.delete(c);if(blocking)data.busy--}
 }
 async function read(operation:ClientGetOperation<Profile>,parts:Data,t:ViewToken,signal:AbortSignal){
  const result=await adapter.read(operation,{...parts,headers:headers()} as never,signal) as SourceResult;
  current(t);return source(result);
 }
 function taskFrom(body:Data,operation?:string,command?:string){
  const value=body.acceptance?object(body.acceptance):body;
  const task=value.task?object(value.task):value;
  if(typeof task.task_id==='string'){
   data.task=task;data.taskAccess=operation==='controlInstance'&&task.task_kind==='CONTROL'?
    (command==='START'||command==='END'?'SERVICE':command==='PAUSE'||command==='RESUME'?'CLIENT':'UNKNOWN'):
    ((operation==='submitTurn'||operation==='cancelTask')&&task.task_kind==='TURN'||operation==='governMemories'&&task.task_kind==='MEMORY_GOVERNANCE'?'CLIENT':'UNKNOWN');
  }
 }
 async function mutate(operation:DurableOperation<Profile>,body:Data,path:Data,originalKey=key()){
  return run(async(t,signal)=>{
   if(data.originals.some(o=>o.operation===operation&&o.state!=='OBSERVED'))throw Error('该操作有结果未明的原请求，请先查询。');
   const session=data.session!,id=key(),reference:BffOriginalReference=Object.freeze({operation,key:originalKey,profile,customer:session.identity[0],player:session.identity[1],world:session.identity[2]});
   locators.set(id,{reference,token:t,...(operation==='controlInstance'?{command:string(body.command)}:{})});
   data.originals=[...data.originals,{id,operation,state:'PREPARED',queries:0}];
   try{
    const result=await adapter.command(operation,{...(Object.keys(path).length?{path}:{}),body,headers:headers(originalKey)} as never,signal) as SourceResult;
    current(t);data.originals=data.originals.map(o=>o.id===id?{...o,state:'OBSERVED'}:o);
    const value=source(result);taskFrom(value,operation,string(body.command));return value;
   }catch(error){
    if(view.current(t))data.originals=data.originals.map(o=>o.id===id&&o.state==='PREPARED'?{...o,state:'UNKNOWN'}:o);
    throw error;
   }
  });
 }
 async function sessionChange(action:()=>Promise<BffSessionView>){
  generation++;const expected=generation;clear();data.session=null;data.consent=null;data.capabilities=null;data.error='';
  try{
   const result=await action();if(closed||expected!==generation)return;
   if(!result.authenticated||!result.identity)throw Error('登录未建立。');
   const session={identity:result.identity,client:result.client};data.session=session;enter(session);
   data.notice=result.client?'已连接当前本人会话。':'已登录，请连接本人会话。';
  }catch{if(!closed&&expected===generation)data.error='登录或会话连接未完成。'}
 }
 function page(body:Data){
  epochs(body);data.deliveries=list(body.deliveries);
  data.packets=data.deliveries.flatMap(d=>d.available_packet?[object(d.available_packet)]:[]);
  if(typeof body.next_cursor==='string')data.cursor=body.next_cursor;
 }
 return Object.freeze({
  data,
  login(login:string,credential:string){return sessionChange(()=>adapter.login({login,credential}))},
  exchange(){return sessionChange(()=>adapter.exchange({originalKey:key()}))},
  async logout(){generation++;clear();data.session=null;data.consent=null;data.capabilities=null;data.notice='已退出，私人视图和媒体已清除。';try{await adapter.logout()}catch{data.error='本地视图已清除；退出确认未收到。'}},
  capabilities(){return run(async(t,s)=>{data.capabilities=await read('getIntegrationCapabilities',{},t,s)})},
  selfConsent(){return run(async(t,s)=>{data.consent=await read('getPlayerSelfConsent',{},t,s)})},
  async consent(command:'INITIAL'|'RENEW'|'REVOKE'){
   if(!data.consent){data.error='请先读取当前本人许可。';return}
   const value=await mutate('commandPlayerSelfConsent',{command,expected_revision:data.consent.consent_revision,policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'},{});
   if(value){data.consent=value.snapshot?object(value.snapshot):value;reset('许可已更新。请重新读取场景和记忆。')}
  },
  snapshot(){return run(async(t,s)=>{const result=await read('getSnapshot',{path:{instance_id:world()}},t,s);epochs(result);data.snapshot=result;data.cursor=string(result.cursor);data.notice='已读取当前场景。'})},
  async control(command:'START'|'PAUSE'|'RESUME'|'END'){
   if(!data.snapshot){data.error='请先读取当前场景版本。';return}
   if(command==='PAUSE'||command==='END')await stopMedia();
   await mutate('controlInstance',{command,expected_control_version:object(data.snapshot.versions).control_version},{instance_id:world()});
  },
  async create(product:string,release:string,originalKey?:string){
   if(!data.session)return;
   const result=await mutate('createInstance',{product_id:product,release_id:release,mode:'PRIVATE',initial_participants:[data.session.identity[1]],client_capabilities:['TEXT','AUDIO']},{},originalKey);
   if(result)data.notice='创建请求已返回。后续会话仍绑定本次登录配置的世界。';
  },
  async quote(actor:string){
   if(!data.snapshot){data.error='请先读取当前场景。';return}
   const result=await mutate('createQuote',{scope:data.snapshot.scope,service_kind:'TURN_TEXT',primary_actor_id:actor,input_upper_bound:'16000',output_upper_bound:'1000',purchase_responsibility:'MOCK'},{});
   if(result)data.quote=result;
  },
  async turn(actor:string,text:string){
   if(!data.snapshot||!data.quote){data.error='请先读取场景并取得本次报价。';return}
   const versions=object(data.snapshot.versions);
   await mutate('submitTurn',{expected_versions:{world_revision:versions.world_revision,control_version:versions.control_version},primary_actor_id:actor,
    input:{kind:'TEXT',text},quote_id:data.quote.quote_id,requested_audience:'PRIVATE_RELATIONSHIP'},{instance_id:world()});
  },
  task(taskId?:string){return run(async(t,s)=>{const id=taskId||string(data.task?.task_id);if(!id)throw Error('尚无已知任务，不能推测原请求结果。');if(data.taskAccess!=='CLIENT'||id!==data.task?.task_id)throw Error('此原任务没有本人查询权限，请只读查询原请求；场景快照仅表示当前实例状态。');const result=await read('getTask',{path:{task_id:id}},t,s);data.task=object(result.task)})},
  async cancelTask(){if(data.task?.task_kind!=='TURN'){data.error='此任务不提供取消操作；必要清理仍由原流程完成。';return}await mutate('cancelTask',{reason:'本人请求取消'},{task_id:data.task.task_id})},
  turnResult(turnId:string){return run(async(t,s)=>{const result=await read('getTurn',{path:{turn_id:turnId}},t,s);epochs(result);data.task=object(result.task);data.taskAccess='CLIENT';data.packets=list(result.approved_packets);data.deliveries=list(result.deliveries)})},
  memories(){return run(async(t,s)=>{const result=await read('listMemories',{path:{instance_id:world()},query:{limit:20}},t,s);epochs(result);data.memories=list(result.memories)})},
  memory(memoryId:string,provenance=false){return run(async(t,s)=>{
   const result=await adapter.read('getMemory',{path:{instance_id:world(),memory_id:memoryId},headers:headers(),...(provenance?{query:{include:'provenance_v1'}}:{})} as never,s) as SourceResult;
   current(t);const value=source(result);data.memoryBody=provenance?object(value.memory):value;
  })},
  async govern(memory:Data,operation:'DELETE'|'CORRECT',reason:string,replacement?:string){
   if(!data.session||memory.target_subject_id!==data.session.identity[1]){data.error='只能治理当前本人的记忆。';return}
   const memoryId=string(memory.memory_id);
   const body={operation,memory_ids:[memoryId],expected_revisions:{[memoryId]:memory.revision},reason,...(operation==='CORRECT'?{replacement:replacement??''}:{})};
   const result=await mutate('governMemories',body,{instance_id:world()});
   if(result){data.memoryBody=null;data.notice='治理已受理；请按原任务查看结果，受理不表示清理已经完成。'}
  },
  events(){return run(async(t,s)=>{if(!data.cursor)throw Error('请先读取当前场景。');page(await read('getEvents',{path:{instance_id:world()},query:{cursor:data.cursor,limit:50}},t,s))})},
  stream(){return run(async(t,s)=>{
   if(!data.cursor||streamAbort)throw Error('请先读取场景，或停止现有更新。');
   const cancel=new AbortController(),abort=()=>cancel.abort(s.reason);streamAbort=cancel;data.streaming=true;s.addEventListener('abort',abort,{once:true});if(s.aborted)abort();
   let resource:AsyncDisposableEventStream|null=null;
   try{
    const result=await adapter.read('streamEvents',{path:{instance_id:world()},query:{cursor:data.cursor},headers:headers()} as never,cancel.signal);
    current(t);if(result.status!=='200'||result.protocol!=='SSE'){source(result as SourceResult);return}
    resource=result.body;stream=resource;
    for await(const frame of resource){current(t);if(frame.event!=='update'){reset('更新视图失效，请重新读取场景。');break}page(object(frame.data));if(!view.current(t))break;resource.accept(frame);data.cursor=frame.id}
   }catch(error){if(!cancel.signal.aborted)throw error}
   finally{s.removeEventListener('abort',abort);await resource?.close();if(stream===resource)stream=null;if(streamAbort===cancel){streamAbort=null;data.streaming=false}}
  },false)},
  async stopStream(){streamAbort?.abort();await stream?.close();stream=null;streamAbort=null;data.streaming=false},
  async recover(id:string){
   const saved=locators.get(id);if(!saved||!view.current(saved.token)){data.error='本视图没有该原请求的定位信息。';return}
   data.busy++;
   try{const result=await recovery.queryOriginal(saved.reference,saved.token);current(saved.token);
    data.originals=data.originals.map(o=>o.id===id?{...o,state:result.state,queries:result.queries}:o);
    if(result.state==='OBSERVED'&&result.original.observation&&result.original.observation.status<300)taskFrom(object(result.original.observation.body),saved.reference.operation,saved.command);
    data.notice=result.state==='UNKNOWN'?'原请求结果仍未明确；未重新发送。':'已读取原请求的观察结果。';
   }catch{if(view.current(saved.token))data.error='原请求读取未完成；未重新发送。'}finally{data.busy--}
  },
  attachMedia(value:MediaSinkFactory){
   if(sink)throw Error('Media sink already bound');sink=value;
  },
  play(resource:MediaRef|RuntimeMediaRef){return run(async(t)=>{await mediaOwner().play(resource,t)})},
  stopMedia,
  consumeCustomer(){return run(async(t,s)=>{const result=await adapter.consumeCustomer(s);current(t);data.customer=result})},
  async close(){if(closed)return;closed=true;generation++;clear();data.session=null;data.consent=null;data.capabilities=null;await Promise.allSettled([media?.close(),recovery.close(),adapter.close()])},
 });
}
