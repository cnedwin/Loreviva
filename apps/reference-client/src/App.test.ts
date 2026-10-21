import {test,expect,vi} from 'vitest';
import {readFileSync,appendFileSync} from 'node:fs';
import {randomBytes,createHash,webcrypto} from 'node:crypto';
import {resolve} from 'node:path';
import {mount,flushPromises} from '@vue/test-utils';
import {createReferenceState} from './state.ts';
import App from './App.vue';
import validators from '../../../packages/sdk-ts/src/generated/validators.cjs';

const schemas=JSON.parse(readFileSync(resolve(process.cwd(),'contracts/generated/openapi.json'),'utf8')).components.schemas;
function example(name:string):any{
 if(name==='MemoryProvenanceV1')return {projection:'provenance_v1',memory:example('Memory'),evidence_domain:'PLAYER_STATEMENT',source_epoch_ids:[],parent_memory_ids:[],occurred_at_estimated:false};
 const value=structuredClone(schemas[name].examples[0]);
 if(name==='IntegrationCapabilities'){value.selected_profile='integration_r0_v1';for(const op of ['getPlayerSelfConsent','getIntegrationCapabilities','getSnapshot','controlInstance','getTask','listMemories','getMemory','governMemories'])value.operations[op]='AVAILABLE'}
 return value;
}

test('ReferencePlayerStateAndAccessibility',async()=>{
 const csrf=randomBytes(32).toString('hex'),credential=randomBytes(24).toString('hex');
 if(process.env['LOREVIVA_TEST_CUSTODY_FILE'])appendFileSync(process.env['LOREVIVA_TEST_CUSTODY_FILE'],JSON.stringify([csrf,credential])+'\n');
 const requests:{path:string;body:any;init:RequestInit}[]=[];
 expect(validators.schema_MemoryProvenanceV1(example('MemoryProvenanceV1'))).toBe(true);
 expect(validators.schema_IntegrationCapabilities(example('IntegrationCapabilities'))).toBe(true);
 let player='player_demo',delay=false,denyMemory=false,loseCommand=false,delayFind=false,resolveLate:((r:Response)=>void)|undefined,resolveFind:((r:Response)=>void)|undefined;
 let streamCancels=0,mediaReads=0;let controlCommand="",loseControl=false;
 const wav=new Uint8Array(160044),dv=new DataView(wav.buffer),ascii=(at:number,value:string)=>wav.set(new TextEncoder().encode(value),at);
 ascii(0,'RIFF');dv.setUint32(4,160036,true);ascii(8,'WAVE');ascii(12,'fmt ');dv.setUint32(16,16,true);dv.setUint16(20,1,true);dv.setUint16(22,1,true);dv.setUint32(24,16000,true);dv.setUint32(28,32000,true);dv.setUint16(32,2,true);dv.setUint16(34,16,true);ascii(36,'data');dv.setUint32(40,160000,true);
 const audioRef={...schemas.RuntimeMediaRef.examples[1],content_sha256:createHash('sha256').update(wav).digest('hex')};
 expect(validators.schema_RuntimeMediaRef(audioRef)).toBe(true);
 const original={format:'lv-bff-original-v1',original_id:'a'.repeat(32),operation:'governMemories',state:'OBSERVED',task_id:'task_control_demo',observation:{status:202,body:{...example('TaskReceipt'),task_kind:'MEMORY_GOVERNANCE'}}};
 const response=(value:unknown,status=200)=>Response.json(value,{status,headers:{'Loreviva-Client-Contract':'integration_r0_v1'}});
 vi.stubGlobal('fetch',vi.fn(async(url:string,init:RequestInit)=>{
  const path=new URL(url).pathname,body=init.body?JSON.parse(new TextDecoder().decode(init.body as Uint8Array)):null;
  requests.push({path,body,init});expect(init.credentials).toBe('same-origin');expect(new Headers(init.headers).has('Authorization')).toBe(false);
  if(path==='/bff/login'||path==='/bff/exchange')return response({format:'lv-bff-session-v1',authenticated:true,identity:['customer_demo',player,'instance_demo','integration_r0_v1'],csrf,client:{subject:player,world:'instance_demo',profile:'integration_r0_v1',expires_at:'2030-01-01T00:00:00Z',view_epoch:'1'}});
  if(path==='/bff/logout')return new Response(null,{status:204});
  if(path==='/bff/client/getSnapshot')return response(example('Snapshot'));
  if(path==='/bff/client/getTask'){
   if(controlCommand==='PAUSE'||controlCommand==='RESUME'){const task={...example('TaskView'),task:{...example('TaskReceipt'),task_kind:'CONTROL',task_state:'COMMITTED'}};expect(validators.schema_TaskView(task)).toBe(true);return response(task)}
   return response({...example('Error'),code:'FORBIDDEN',recovery_action:'AUTHENTICATE'},403);
  }
  if(path==='/bff/client/getTurn'){const args=JSON.parse(new URL(url).searchParams.get('args')!);expect(args.path).toEqual({turn_id:'turn_demo'});return response(example('IntegrationTurnView'))}
  if(path==='/bff/client/getMemory'&&delay)return new Promise<Response>(r=>{resolveLate=r});
  if(path==='/bff/client/getMemory'){
   if(denyMemory)return response({...example('Error'),code:'FORBIDDEN',recovery_action:'AUTHENTICATE'},403);
   const args=JSON.parse(new URL(url).searchParams.get('args')!);
   return response(args.query.some((x:string[])=>x[0]==='include')?example('MemoryProvenanceV1'):example('PlayerMemoryMetadata'));
  }
  if(path==='/bff/client/getIntegrationCapabilities')return response(example('IntegrationCapabilities'));
  if(path==='/bff/client/getPlayerSelfConsent')return response(example('PlayerSelfConsentSnapshot'));
  if(path==='/bff/client/listMemories')return response(example('PlayerMemoryMetadataPage'));
  if(path==='/bff/find-original'){
   if(delayFind)return new Promise<Response>(r=>{resolveFind=r});
   if(body.operation==='controlInstance')return response({...original,operation:'controlInstance',state:'UNKNOWN',task_id:null,observation:null});
   return response(original);
  }
  if(path==='/bff/query-original/'+original.original_id)return response(loseControl?{...original,operation:'controlInstance',state:'UNKNOWN',task_id:null,observation:null}:original);
  if(path==='/bff/client/readRuntimeMedia'){mediaReads++;return new Response(wav,{headers:{'Content-Type':'audio/wav','Content-Length':String(wav.length)}})}
  if(path==='/bff/client/streamEvents')return new Response(new ReadableStream<Uint8Array>({
   start(c){c.enqueue(new TextEncoder().encode('event: update\nid: opaque_synthetic_cursor_0002\ndata: '+JSON.stringify(example('IntegrationEventPage'))+'\n\n'))},
   cancel(){streamCancels++},
  }),{headers:{'Content-Type':'text/event-stream'}});
  if(path==='/bff/commands'){
   if(body.operation==='controlInstance'){controlCommand=body.body.command;if(loseControl)throw Error('Controlled CONTROL response lost');return response({...example('TaskReceipt'),task_kind:'CONTROL'},202);}
   expect(body.operation).toBe('governMemories');
   if(loseCommand)throw Error('Controlled response lost after command receipt');
   return response({...example('TaskReceipt'),task_kind:'MEMORY_GOVERNANCE'},202);
  }
  throw Error('Unexpected test route '+path);
 }));
 const state=createReferenceState();
 try{
  await state.login('synthetic',credential);expect(state.data.session?.identity[1]).toBe(player);
  await state.snapshot();expect(state.data.snapshot).not.toBeNull();
  await state.control('START');expect(state.data.task?.task_kind,'legal SERVICE CONTROL receipt prerequisite').toBe('CONTROL');
  const controlPosts=requests.filter(r=>r.path==='/bff/commands').length;
  const controlSnapshot=state.data.snapshot;
  await state.task();
  expect.soft(requests.filter(r=>r.path==='/bff/client/getTask'),'SERVICE CONTROL cannot be queried by CLIENT task reader').toHaveLength(0);
  expect.soft(state.data.snapshot,'unavailable original task action must not erase an otherwise-current private snapshot').toBe(controlSnapshot);
  expect(requests.filter(r=>r.path==='/bff/commands')).toHaveLength(controlPosts);
  for(const command of ['PAUSE','RESUME'] as const){
   await state.control(command);expect(state.data.taskAccess).toBe('CLIENT');const posts=requests.filter(r=>r.path==='/bff/commands').length;
   await state.task();expect(state.data.task?.task_state).toBe('COMMITTED');expect(state.data.snapshot).toBe(controlSnapshot);
   expect(requests.filter(r=>r.path==='/bff/commands')).toHaveLength(posts);
  }
  for(const command of ['START','END'] as const){
   await state.login('synthetic',credential);await state.snapshot();loseControl=true;await state.control(command);
   const pending=state.data.originals.at(-1)!;expect(pending.state).toBe('UNKNOWN');const posts=requests.filter(r=>r.path==='/bff/commands').length,queries=requests.filter(r=>r.path==='/bff/client/getTask').length;
   await state.task();await state.snapshot();expect(state.data.originals.at(-1)!.state,'snapshot is not original command proof').toBe('UNKNOWN');
   await state.recover(pending.id);expect(state.data.originals.at(-1)!.state).toBe('UNKNOWN');
   expect(requests.filter(r=>r.path==='/bff/client/getTask')).toHaveLength(queries);expect(requests.filter(r=>r.path==='/bff/commands')).toHaveLength(posts);loseControl=false;
  }
  await state.login('synthetic',credential);await state.snapshot();

  expect(validators.schema_IntegrationTurnView(example('IntegrationTurnView'))).toBe(true);
  await state.turnResult('turn_demo');expect(state.data.error).toBe('');expect(state.data.packets).toHaveLength(1);
  const own={...example('Memory'),target_subject_id:player};
  await state.govern(own,'CORRECT','本人更正','新的本人事实');
  const command=requests.find(r=>r.path==='/bff/commands'&&r.body.operation==='governMemories');
  expect(command,'A valid same-person correction must reach the typed local command').toBeDefined();
  expect(command!.body.body.replacement).toBe('新的本人事实');
  expect(command!.body.body.reason).toBe('本人更正');
  expect(state.data.task?.task_kind).toBe('MEMORY_GOVERNANCE');
  await state.memory(own.memory_id,true);
  expect.soft(state.data.memoryBody?.content,'Explicit validated provenance must expose only its approved memory body').toBe(example('MemoryProvenanceV1').memory.content);
  denyMemory=true;await state.memory(own.memory_id,true);
  expect(state.data.memoryBody,'A current denial must remove the previous private body').toBeNull();
  denyMemory=false;
  const count=requests.length;await state.govern({...own,target_subject_id:'other'},'DELETE','不是本人');expect(requests).toHaveLength(count);
  delay=true;const late=state.memory(own.memory_id);await vi.waitFor(()=>expect(resolveLate).toBeDefined());
  const signal=requests.at(-1)!.init.signal!;await state.logout();expect(signal.aborted).toBe(true);
  resolveLate!(response(own));await late;expect(state.data.memoryBody).toBeNull();expect(state.data.snapshot).toBeNull();expect(state.data.originals).toEqual([]);
  player='other';await state.login('other',credential);expect(state.data.memoryBody).toBeNull();
 }finally{await state.close()}
 delay=false;player='player_demo';
 const wrapper=mount(App,{attachTo:document.body});
 try{
  const mediaHost=wrapper.get('[data-testid="media-host"]').element;expect(wrapper.find('audio').exists()).toBe(false);
  await wrapper.get('#login').setValue('synthetic');await wrapper.get('#credential').setValue(credential);
  const loginElement=wrapper.get('#login').element as HTMLInputElement;loginElement.focus();expect(document.activeElement).toBe(loginElement);
  await wrapper.get('form').trigger('submit');await flushPromises();
  expect(wrapper.get('[data-testid="current-player"]').text()).toBe(player);
  expect(wrapper.get('[data-testid="media-host"]').element).toBe(mediaHost);expect(wrapper.find('audio').exists()).toBe(false);
  expect(wrapper.text()).not.toContain(credential);expect(wrapper.html()).not.toContain(csrf);
  const button=(label:string)=>{const b=wrapper.findAll('button').find(b=>b.text()===label);expect(b,label).toBeDefined();return b!};
  await button('读取记忆列表').trigger('click');await flushPromises();
  await wrapper.get('#memory').setValue(example('PlayerMemoryMetadataPage').memories[0].memory_id);
  await wrapper.get('#reason').setValue('本人纠正');await wrapper.get('#replacement').setValue('更新的内容');
  await button('纠正本人记忆').trigger('click');await flushPromises();
  expect(wrapper.get('[data-testid="task-state"]').text()).toContain('MEMORY_GOVERNANCE');
  expect(wrapper.text()).toContain('治理已受理不等于清理完成');
  expect(wrapper.findAll('button').some(b=>b.text()==='请求取消该任务')).toBe(false);
  expect(wrapper.get('[data-testid="delivery-state"]').text()).toContain('0 项');
  expect(wrapper.find('[data-testid="customer-state"]').exists()).toBe(false);
  await button('申请读取正文与来源').trigger('click');await flushPromises();
  expect(wrapper.get('[data-testid="memory-body"]').text()).toContain(example('MemoryProvenanceV1').memory.content);
  denyMemory=true;await button('申请读取正文与来源').trigger('click');await flushPromises();
  expect(wrapper.find('[data-testid="memory-body"]').exists()).toBe(false);
  expect(wrapper.get('[role="alert"]').attributes('tabindex')).toBe('-1');
  expect(document.activeElement).toBe(wrapper.get('[role="alert"]').element);
  await button('退出并清除私人视图').trigger('click');await flushPromises();
  expect(wrapper.find('[data-testid="task-state"]').exists()).toBe(false);
  expect(wrapper.find('[data-testid="current-player"]').exists()).toBe(false);
  expect(wrapper.get('[data-testid="media-host"]').element).toBe(mediaHost);expect(wrapper.find('audio').exists()).toBe(false);
  expect(document.activeElement).toBe(wrapper.get('#login').element);
 }finally{wrapper.unmount();await flushPromises()}
 denyMemory=false;player='player_demo';
 const state2=createReferenceState(),urls:string[]=[],revoked:string[]=[];
 const createDescriptor=Object.getOwnPropertyDescriptor(URL,'createObjectURL'),revokeDescriptor=Object.getOwnPropertyDescriptor(URL,'revokeObjectURL');
 Object.defineProperty(URL,'createObjectURL',{configurable:true,value:()=>{const url='blob:synthetic-'+urls.length;urls.push(url);return url}});
 Object.defineProperty(URL,'revokeObjectURL',{configurable:true,value:(url:string)=>{revoked.push(url)}});
 vi.stubGlobal('crypto',webcrypto);
 class TestSink extends EventTarget {src='';play=vi.fn(async()=>{});pause=vi.fn();removeAttribute(){this.src=''}load(){}remove(){}}
 let sink!:TestSink;const sinks:TestSink[]=[];const createSink=()=>{sink=new TestSink();sinks.push(sink);return sink};
 try{
  await state2.login('synthetic',credential);await state2.snapshot();state2.attachMedia(createSink);
  await state2.play(audioRef);expect(sink.src).toBe(urls[0]);await state2.play(audioRef);
  expect(mediaReads).toBe(2);expect(revoked).toEqual([urls[0]]);expect(sinks).toHaveLength(2);for(const target of sinks)expect(target.play).toHaveBeenCalledTimes(1);expect(sinks[0]!.src).toBe('');
  await state2.stopMedia();expect(revoked).toEqual(urls);expect(sink.src).toBe('');
  const streaming=state2.stream();await vi.waitFor(()=>expect(state2.data.deliveries).toHaveLength(1));
  expect(state2.data.busy,'a continuous read cannot block PAUSE or other foreground commands').toBe(0);
  await state2.stopStream();await streaming;expect(streamCancels).toBe(1);expect(state2.data.streaming).toBe(false);
  loseCommand=true;await state2.govern(example('Memory'),'DELETE','本人删除');
  expect(state2.data.originals.at(-1)?.state).toBe('UNKNOWN');
  const originalCount=requests.filter(r=>r.path==='/bff/commands').length;
  const id=state2.data.originals.at(-1)!.id;
  await state2.recover(id);
  expect(state2.data.originals.at(-1)?.state).toBe('OBSERVED');expect(state2.data.originals.at(-1)?.queries).toBe(2);
  expect(requests.filter(r=>r.path==='/bff/commands')).toHaveLength(originalCount);
  expect(state2.data.task?.task_kind).toBe('MEMORY_GOVERNANCE');
  delayFind=true;const pending=state2.recover(id);await vi.waitFor(()=>expect(resolveFind).toBeDefined());
  const findSignal=requests.at(-1)!.init.signal!;await state2.logout();expect(findSignal.aborted).toBe(true);
  resolveFind!(response(original));await pending;
  expect(state2.data.originals).toEqual([]);expect(state2.data.task).toBeNull();expect(sink.src).toBe('');
  expect(sessionStorage.length).toBe(0);expect(localStorage.length).toBe(0);
 }finally{
  await state2.close();
  if(createDescriptor)Object.defineProperty(URL,'createObjectURL',createDescriptor);else Reflect.deleteProperty(URL,'createObjectURL');
  if(revokeDescriptor)Object.defineProperty(URL,'revokeObjectURL',revokeDescriptor);else Reflect.deleteProperty(URL,'revokeObjectURL');
  vi.unstubAllGlobals();
 }
},60000);
