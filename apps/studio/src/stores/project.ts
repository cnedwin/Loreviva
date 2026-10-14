import {computed,ref,shallowRef,toRaw,watch} from 'vue';
import {defineStore} from 'pinia';
import {createAdminClient,type AdminClient,type ContentDraft,type DraftInput,type Operations,type RootContentKind,type ContentObject} from '@loreviva/sdk-ts';
import {schemas,emptyValue,kindSchemas} from '../lib/validation';
import {createCommandRunner} from '../lib/commands';
import {createImpactFlow} from '../lib/impact';
import {useTasksStore} from './tasks';
export const clone=<T>(v:T):T=>JSON.parse(JSON.stringify(v));
export const useProjectStore=defineStore('project',()=>{
 const bearer=ref('');const productId=ref('coffee');const tenantId=ref('demo-local');
 const settingsScope=ref<'PRODUCT'|'TENANT'>('PRODUCT');
 const client=shallowRef<AdminClient>(createAdminClient({getOperatorBearer:()=>bearer.value||null}));
 const basis=ref<ContentDraft>();const editable=ref<DraftInput>();const selectedId=ref('');const error=shallowRef<unknown>();
 const pendingSave=ref<Operations['saveContentDraft']['input']>();const busy=ref(false);let editSession=0;let loadAttempt=0;let pendingSession=0;let activeSave:symbol|undefined;
 // Failed reads do not surrender the accepted editor's receipt ownership.
 // Automatic receipt acceptance must not cancel a user's pending navigation.
 const impact=createImpactFlow({current:()=>({client:client.value,product:productId.value,session:editSession,basis:basis.value,input:editable.value}),adopt:adoptDraft,beforeExplicitChoice:()=>{loadAttempt++},baseline:value=>{basis.value=clone(value)},deleted:keepInput=>{basis.value=undefined;newKind.value=undefined;pendingSave.value=undefined;if(!keepInput){editSession++;editable.value=undefined}}});
 watch([client,productId],()=>{loadAttempt++;impact.reset()},{flush:'sync'});
 watch(bearer,value=>{editSession++;activeSave=undefined;busy.value=false;client.value=createAdminClient({getOperatorBearer:()=>value||null});basis.value=undefined;editable.value=undefined;pendingSave.value=undefined;pendingCreate.value=undefined;drafts.value=[];newKind.value=undefined;error.value=undefined},{flush:'sync'});
 const newKind=ref<RootContentKind>();const pendingCreate=ref<Operations['createContentDraft']['input']>();const drafts=ref<ContentDraft[]>([]);
 const dirty=computed(()=>!!editable.value&&(!basis.value||JSON.stringify(editable.value)!==JSON.stringify(basis.value.input)));
 const selectedObject=computed(()=>editable.value?.objects.find(o=>o.object_id===selectedId.value));
 function setClient(value:AdminClient){client.value=value}
 function acceptDraft(value:ContentDraft){loadAttempt++;adoptDraft(value)}
 function adoptDraft(value:ContentDraft){impact.preserveRecovery();editSession++;basis.value=clone(value);editable.value=clone(value.input);if(!editable.value.objects.some(o=>o.object_id===selectedId.value))selectedId.value=editable.value.root_object_id;if(!createCommandRunner(client.value).pending.value){pendingSave.value=undefined;pendingCreate.value=undefined}newKind.value=undefined;error.value=undefined}
 // Only the accepting edit session may advance its basis; replace local input only when it still equals the submitted snapshot.
 function committedDraft(value:ContentDraft,sent:DraftInput,session:number){pendingSave.value=undefined;pendingCreate.value=undefined;if(session!==editSession)return false;const unchanged=JSON.stringify(editable.value)===JSON.stringify(sent);basis.value=clone(value);if(unchanged)editable.value=clone(value.input);pendingSave.value=undefined;pendingCreate.value=undefined;newKind.value=undefined;error.value=undefined;return true}
 function discardUnsaved(){impact.assertReady();if(basis.value)editable.value=clone(basis.value.input);else{editable.value=undefined;newKind.value=undefined}error.value=undefined}
 async function loadProject(projectId?:string){const active=client.value;const results:ContentDraft[]=[];let cursor:string|undefined;const seen=new Set<string>();do{const page=await active.call('listContentDrafts',{query:{product_id:productId.value,...(projectId?{project_id:projectId}:{}),...(cursor?{cursor}:{}),limit:100}});results.push(...page.items);cursor=page.next_cursor;if(cursor&&seen.has(cursor))throw Error('分页游标未前进');if(cursor)seen.add(cursor)}while(cursor);if(client.value!==active)throw Error('凭据已变化，请重新读取');drafts.value=results}
 function newDraft(kind:RootContentKind){loadAttempt++;impact.preserveRecovery();editSession++;const id=crypto.randomUUID();const object=emptyValue(schemas[kindSchemas[kind]]) as ContentObject;Object.assign(object,{object_id:id,version:'1',name:'未命名内容',kind});basis.value=undefined;newKind.value=kind;editable.value={name:'未命名内容',description:'',tags:[],root_object_id:id,objects:[object],module_agent_bindings:[],skill_bindings:[],dependency_locks:[]};selectedId.value=id;if(!createCommandRunner(client.value).pending.value){pendingSave.value=undefined;pendingCreate.value=undefined}error.value=undefined}
 async function createDraft(retry=false){
  impact.assertReady();
  if(busy.value)return false;if(retry&&!pendingCreate.value)throw Error('没有待恢复的原请求');if(!retry&&(!editable.value||!newKind.value))throw Error('没有新草稿');
  const input=retry?clone(pendingCreate.value!):{headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:productId.value,root_kind:newKind.value!,input:clone(editable.value!)}};
  const active=client.value;const runner=createCommandRunner(active);if(runner.pending.value&&!retry)throw Error('先用原请求键恢复保存');
  const session=retry?pendingSession:editSession;pendingSession=session;const token=Symbol();activeSave=token;pendingCreate.value=input;busy.value=true;
  try{const created=await runner.run('createContentDraft',input);if(client.value!==active)throw Error('凭据已变化，请重新读取');return committedDraft(created,input.body.input,session)}catch(e){if(client.value===active){if(!runner.pending.value)pendingCreate.value=undefined;if(session===editSession)error.value=e}throw e}finally{if(activeSave===token){activeSave=undefined;busy.value=false}}
 }
 async function loadDraft(id:string){impact.preserveRecovery();const active=client.value;const product=productId.value;const attempt=++loadAttempt;const value=await active.call('getContentDraft',{path:{draft_id:id},query:{product_id:product}});if(client.value!==active||productId.value!==product)throw Error('凭据或产品已变化，请重新读取');if(attempt===loadAttempt)adoptDraft(value)}
 // Keep the submitted payload, key and edit session for recovery; a retry must not adopt newer local edits.
 // Only that editor session may accept the returned basis, and only the active save may clear its busy state.
 async function saveDraft(expectedRevision:string|boolean=basis.value?.draft_revision??'',retry=false){
  impact.assertReady();
  if(busy.value)return false;const restoring=typeof expectedRevision==='boolean'?expectedRevision:retry;if(!restoring&&(!basis.value||!editable.value))return false;const revision=typeof expectedRevision==='string'?expectedRevision:basis.value?.draft_revision??'';
  if(!restoring&&revision!==basis.value?.draft_revision)throw Error('保存基准与本地原修订不同');if(restoring&&!pendingSave.value)throw Error('没有待恢复的原请求');
  const input=restoring?clone(pendingSave.value!):{path:{draft_id:basis.value!.draft_id},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:productId.value,expected_draft_revision:revision,input:clone(toRaw(editable.value!))}};
  const active=client.value;const runner=createCommandRunner(active);if(runner.pending.value&&!restoring)throw Error('先用原请求键恢复保存');
  const session=restoring?pendingSession:editSession;pendingSession=session;const token=Symbol();activeSave=token;pendingSave.value=input;busy.value=true;
  try{const saved=await runner.run('saveContentDraft',input);if(client.value!==active)throw Error('凭据已变化，请重新读取');return committedDraft(saved,input.body.input,session)}catch(e){if(client.value===active){if(!runner.pending.value)pendingSave.value=undefined;if(session===editSession)error.value=e}throw e}finally{if(activeSave===token){activeSave=undefined;busy.value=false}}
 }
 function applyCandidate(candidateId:string,selectedFields:string[]){useTasksStore().applyCandidate(candidateId,selectedFields)}
 function moveSelected(x:number,y:number){const object=selectedObject.value;if(object&&'position'in object)object.position={...object.position,x,y}}
 function signOut(){bearer.value='';basis.value=undefined;editable.value=undefined;pendingSave.value=undefined;error.value=undefined}
 return {bearer,productId,tenantId,settingsScope,client,basis,editable,selectedId,selectedObject,dirty,error,pendingSave,pendingCreate,newKind,drafts,busy,setClient,acceptDraft,discardUnsaved,loadDraft,loadProject,newDraft,createDraft,saveDraft,applyCandidate,moveSelected,signOut,impact:impact.context,impactBlocked:impact.blocked,impactReportCurrent:impact.reportCurrent,assertImpactReady:impact.assertReady,checkImpact:impact.check,executeImpact:impact.execute,recoverImpact:impact.recover,invalidateImpactCheck:impact.invalidateCheck,useImpactServer:impact.useServer,keepImpactLocal:impact.keepLocal,discardDeletedImpact:impact.discardDeleted};
});
