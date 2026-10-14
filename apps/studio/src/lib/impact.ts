import {computed,shallowRef,shallowReactive} from 'vue';
import type {AdminClient,ContentDraft,DraftInput,DraftAction,ImpactReport,DraftCommandResult,Operations} from '@loreviva/sdk-ts';
import {createCommandRunner} from './commands';
type CheckInput=Operations['checkDraftImpact']['input'];
type CommandInput=Operations['commandContentDraft']['input'];
type Stage='CHECKING'|'CHECK_UNKNOWN'|'CHECKED'|'COMMANDING'|'COMMAND_UNKNOWN'|'READBACK_PENDING'|'DONE'|'DELETED_BUFFER'|'REJECTED';
type Current={client:AdminClient;product:string;session:number;basis?:ContentDraft;input?:DraftInput};
type Context={client:AdminClient;product:string;session:number;draft:ContentDraft;sent:DraftInput;check:CheckInput;command?:CommandInput;stage:Stage;report?:ImpactReport;result?:DraftCommandResult;server?:ContentDraft;recoveryInput?:DraftInput;invalid:boolean;review:boolean;error?:unknown;busy:boolean};
const copy=<T>(v:T):T=>JSON.parse(JSON.stringify(v));
const equal=(a:unknown,b:unknown)=>JSON.stringify(a)===JSON.stringify(b);
export function createImpactFlow(host:{current:()=>Current;adopt:(draft:ContentDraft)=>void;beforeExplicitChoice:()=>void;baseline:(draft:ContentDraft)=>void;deleted:(keepInput:boolean)=>void}){
 const context=shallowRef<Context>();
 const alive=(c:Context)=>context.value===c&&host.current().client===c.client&&host.current().product===c.product;
 const sameSession=(c:Context)=>alive(c)&&host.current().session===c.session;
 const unchanged=(c:Context)=>sameSession(c)&&equal(host.current().input,c.sent);
 const applies=(c:Context)=>alive(c)&&(sameSession(c)||host.current().basis?.draft_id===c.draft.draft_id);
 const blocked=computed(()=>{const c=context.value;return !!c&&applies(c)&&(c.review||['COMMANDING','COMMAND_UNKNOWN','READBACK_PENDING','DELETED_BUFFER'].includes(c.stage))});
 const reportCurrent=computed(()=>{const c=context.value;return !!c&&c.stage==='CHECKED'&&!c.invalid&&unchanged(c)&&host.current().basis?.draft_revision===c.draft.draft_revision});
 function assertReady(){if(blocked.value)throw Error('请先恢复原影响操作或明确重整本地与服务器内容')}
 function reset(){context.value=undefined}
 function invalidateCheck(){const c=context.value;if(c&&['CHECKING','CHECK_UNKNOWN','CHECKED'].includes(c.stage))c.invalid=true}
 function vacant(){const c=context.value;if(c&&(c.busy||!['CHECKED','DONE','REJECTED'].includes(c.stage)||c.review))throw Error('先完成原影响操作及内容重整');if(createCommandRunner(host.current().client).pending.value)throw Error('先恢复原操作，不能替换其请求键')}
 async function check(action:DraftAction,objectIds:string[]){
  assertReady();vacant();const now=host.current();if(!now.basis||!now.input||!equal(now.input,now.basis.input))throw Error('请先保存或取消本地修改');
  const c=shallowReactive<Context>({client:now.client,product:now.product,session:now.session,draft:copy(now.basis),sent:copy(now.input),stage:'CHECKING',invalid:false,review:false,busy:false,check:{path:{draft_id:now.basis.draft_id},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:now.product,expected_draft_revision:now.basis.draft_revision,action,object_ids:[...objectIds],...(action==='COPY'?{copy_mode:'INDEPENDENT_COPY'}:{})}}});context.value=c;await readCheck(c);
 }
 async function readCheck(c:Context){
  c.busy=true;c.error=undefined;
  try{const report=await createCommandRunner(c.client).run('checkDraftImpact',c.check);if(!alive(c))return;c.report=report;c.stage='CHECKED'}
  catch(e){if(alive(c)){c.error=e;c.stage=createCommandRunner(c.client).pending.value?'CHECK_UNKNOWN':'REJECTED'}}finally{c.busy=false}
 }
 async function execute(options:{copyName:string;restoreRevision:string}){
  const c=context.value;if(!c||!reportCurrent.value||!c.report||c.report.blocking_findings.length)throw Error('需要当前已保存修订的影响报告');
  if(createCommandRunner(c.client).pending.value)throw Error('先恢复原操作');
  c.command={path:{draft_id:c.report.draft_id},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:c.product,expected_draft_revision:c.report.draft_revision,action:c.report.action,impact_report_id:c.report.report_id,...(c.report.action==='COPY'?{copy_name:options.copyName}:{}),...(c.report.action==='RESTORE'?{restore_revision:options.restoreRevision}:{})}};
  await sendCommand(c);
 }
 async function sendCommand(c:Context){
  if(!c.command)throw Error('缺少原影响请求');c.busy=true;c.stage='COMMANDING';c.error=undefined;
  try{
   const result=await createCommandRunner(c.client).run('commandContentDraft',c.command);if(!alive(c))return;c.result=result;
   // A known commit never re-enters the command runner, even when its GET fails.
   if('draft' in result){c.server=result.draft;c.stage='DONE';if(unchanged(c))host.adopt(result.draft)}
   else if(result.state==='DELETED'){c.stage='DONE';if(sameSession(c)){const keep=!unchanged(c);if(keep)c.recoveryInput=copy(host.current().input!);host.deleted(keep);if(keep)c.stage='DELETED_BUFFER'}}
   else {c.stage='READBACK_PENDING';await readBack(c)}
  }catch(e){if(alive(c)){c.error=e;c.stage=c.result?'READBACK_PENDING':createCommandRunner(c.client).pending.value?'COMMAND_UNKNOWN':'REJECTED'}}finally{c.busy=false}
 }
 async function readBack(c:Context){
  if(!c.result||'draft' in c.result||c.result.state==='DELETED')throw Error('没有可读回的提交');
  const value=await c.client.call('getContentDraft',{path:{draft_id:c.draft.draft_id},query:{product_id:c.product}});if(!alive(c))return;
  c.server=value;c.stage='DONE';
  // The dispatch session owns acceptance. A later revision requires an explicit choice.
  if(sameSession(c)){
   if(value.draft_revision===c.result.draft_revision){if(unchanged(c))host.adopt(value);else{host.baseline(value);c.review=true}}
   else c.review=true;
  }
 }
 // Unknown outcomes retry the captured check or command with its original payload and key.
 // Once a commit is known, recovery only repeats its GET and never resends the command.
 async function recover(){
  const c=context.value;if(!c||!alive(c)||c.busy)throw Error('没有可恢复的原影响操作');
  if(c.stage==='CHECK_UNKNOWN')return readCheck(c);
  if(c.stage==='COMMAND_UNKNOWN')return sendCommand(c);
  if(c.stage==='READBACK_PENDING'){c.busy=true;c.error=undefined;try{await readBack(c)}catch(e){if(alive(c))c.error=e}finally{c.busy=false};return}
  throw Error('原影响操作不需要恢复');
 }
 // Explicit choices supersede pending reads; automatic commit handling above does not.
 function useServer(){const c=context.value;if(!c||!alive(c)||!c.server)throw Error('没有已读取的服务器内容');host.beforeExplicitChoice();c.review=false;host.adopt(c.server)}
 function keepLocal(){const c=context.value;if(!c||!sameSession(c)||!c.review||!c.server||!host.current().input)throw Error('当前会话没有待重整内容');host.beforeExplicitChoice();host.baseline(c.server);c.review=false}
 function preserveRecovery(){const c=context.value;if(c&&sameSession(c)&&c.stage==='DELETED_BUFFER'&&host.current().input)c.recoveryInput=copy(host.current().input!)}
 function discardDeleted(){const c=context.value;if(!c||!alive(c)||c.stage!=='DELETED_BUFFER')throw Error('没有删除恢复内容');host.beforeExplicitChoice();c.stage='DONE';c.recoveryInput=undefined;if(sameSession(c))host.deleted(false)}
 return {context,blocked,reportCurrent,assertReady,reset,invalidateCheck,check,execute,recover,useServer,keepLocal,discardDeleted,preserveRecovery};
}
