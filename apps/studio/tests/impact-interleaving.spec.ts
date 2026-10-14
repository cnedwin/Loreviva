import {beforeEach,afterEach,expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {mount,flushPromises} from '@vue/test-utils';
import ImpactCommands from '../src/components/ImpactCommands.vue';
import {useProjectStore,clone} from '../src/stores/project';
import {useTasksStore} from '../src/stores/tasks';
import type {ContentDraft} from '@loreviva/sdk-ts';
const original={draft_id:'d',draft_revision:'1',state:'DRAFT',input:{name:'A',description:'',tags:[],root_object_id:'o',objects:[{object_id:'o',kind:'KNOWLEDGE',name:'A',version:'1',description:'',tags:[],notes:[]}],module_agent_bindings:[{binding_id:'binding-1'}],skill_bindings:[],dependency_locks:[]}} as unknown as ContentDraft;
const removed={...original,draft_revision:'2',input:{...original.input,module_agent_bindings:[]}};
const receipt={draft_id:'d',draft_revision:'2',action:'REMOVE_BINDING',state:'DRAFT'};
function deferred(){let resolve!:(x:unknown)=>void;const promise=new Promise(r=>resolve=r);return {promise,resolve}}
let pinia:ReturnType<typeof createPinia>;
beforeEach(()=>{pinia=createPinia();setActivePinia(pinia);vi.spyOn(window,'confirm').mockReturnValue(true)});
afterEach(()=>vi.restoreAllMocks());
function button(view:ReturnType<typeof mount>,text:string){return view.findAll('button').find(b=>b.text()===text)!}
async function setup(command:()=>Promise<unknown>,get:()=>Promise<unknown>=async()=>removed,action='REMOVE_BINDING'){
 const s=useProjectStore();s.acceptDraft(clone(original));const calls:{op:string;input:any}[]=[];
 const call=async(op:string,input:any)=>{calls.push({op,input:clone(input)});if(op==='checkDraftImpact')return {draft_id:'d',draft_revision:'1',action:input.body.action,report_id:'report-1',blocking_findings:[],selected_targets:[],incoming_refs:[],duty_coverage:[],affected_release_ids:[],expires_at:'2099-01-01T00:00:00Z'};if(op==='commandContentDraft')return command();if(op==='getContentDraft')return get();throw Error('Unexpected operation '+op)};
 s.setClient({call} as never);const view=mount(ImpactCommands,{global:{plugins:[pinia]}});
 await view.get('select').setValue(action);if(action==='REMOVE_BINDING')await view.get('input').setValue('binding-1');await button(view,'检查当前影响').trigger('click');await flushPromises();return {s,view,calls};
}
it('late REMOVE_BINDING preserves B and blocks saving until the server change is explicitly reconciled',async()=>{
 const gate=deferred();const {s,view,calls}=await setup(()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');s.editable!.name='B';gate.resolve(receipt);await flushPromises();
 expect(s.editable!.name).toBe('B');expect(s.basis!.input.module_agent_bindings).toEqual([]);expect(s.dirty).toBe(true);
 await expect(s.saveDraft()).rejects.toThrow();expect(calls.filter(x=>x.op==='saveContentDraft')).toHaveLength(0);view.unmount();
});
it.each(['command','readback'])('identity change isolates a late %s response',async phase=>{
 const gate=deferred();const {s,view}=await setup(phase==='command'?()=>gate.promise:async()=>receipt,phase==='readback'?()=>gate.promise:async()=>removed);
 await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();s.bearer='new-identity';s.newDraft('KNOWLEDGE');s.editable!.name='New identity';gate.resolve(phase==='command'?receipt:removed);await flushPromises();expect(s.impact).toBeUndefined();expect(s.basis).toBeUndefined();expect(s.editable!.name).toBe('New identity');view.unmount();
});
it('delayed readback cannot replace C opened after COMMIT',async()=>{
 const gate=deferred();const {s,view}=await setup(async()=>receipt,()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();s.acceptDraft({...original,draft_id:'c',input:{...original.input,name:'C'}});gate.resolve(removed);await flushPromises();expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');expect(s.impact!.server!.draft_id).toBe('d');view.unmount();
});
it.each(['COPY','DELETE'])('%s preserves B without pretending it belongs to the operation result',async action=>{
 const gate=deferred();const result=action==='COPY'?{draft:{...original,draft_id:'copy',input:{...original.input,name:'Copy'}},source_draft_id:'d',excluded_categories:[]}:{...receipt,action:'DELETE',state:'DELETED'};
 const {s,view,calls}=await setup(()=>gate.promise,async()=>{throw Error('No GET for copy/delete')},action);await button(view,'确认执行 '+action).trigger('click');s.editable!.name='B';gate.resolve(result);await flushPromises();expect(s.editable!.name).toBe('B');expect(calls.filter(x=>x.op==='getContentDraft')).toHaveLength(0);
 if(action==='COPY'){expect(s.basis!.draft_id).toBe('d');expect(s.impact!.server!.draft_id).toBe('copy');await button(view,'放弃当前本地修改并打开操作结果').trigger('click');expect(s.basis!.draft_id).toBe('copy')}
 else {expect(s.basis).toBeUndefined();expect(s.newKind).toBeUndefined();await expect(s.saveDraft()).rejects.toThrow();await expect(s.createDraft()).rejects.toThrow();await button(view,'放弃已删除草稿的本地恢复内容').trigger('click');expect(s.editable).toBeUndefined()};view.unmount();
});
it.each(['COPY','DELETE'])('%s cannot replace or clear a later draft C',async action=>{
 const gate=deferred();const {s,view}=await setup(()=>gate.promise,async()=>removed,action);await button(view,'确认执行 '+action).trigger('click');s.acceptDraft({...original,draft_id:'c',input:{...original.input,name:'C'}});gate.resolve(action==='COPY'?{draft:{...original,draft_id:'copy'}}:{...receipt,action:'DELETE',state:'DELETED'});await flushPromises();expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');view.unmount();
});
it('explicit reconciliation gates save/candidate/rehearsal and allows a conscious local choice',async()=>{
 const gate=deferred();const {s,view}=await setup(()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');s.editable!.name='B';gate.resolve(receipt);await flushPromises();const tasks=useTasksStore();expect(s.impactBlocked).toBe(true);await expect(tasks.createCandidate({} as never)).rejects.toThrow();await expect(tasks.rehearse({} as never)).rejects.toThrow();expect(()=>s.discardUnsaved()).toThrow();await button(view,'已对照差异，明确保留本地版本').trigger('click');expect(s.impactBlocked).toBe(false);expect(s.basis!.draft_revision).toBe('2');expect(s.editable!.name).toBe('B');expect(s.editable!.module_agent_bindings).toHaveLength(1);expect(s.dirty).toBe(true);view.unmount();
});
it('explicit server choice drops B and keeps the removed binding absent',async()=>{
 const gate=deferred();const {s,view}=await setup(()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');s.editable!.name='B';gate.resolve(receipt);await flushPromises();await button(view,'放弃本地修改并采用操作后的服务器内容').trigger('click');expect(s.impactBlocked).toBe(false);expect(s.editable!.module_agent_bindings).toEqual([]);expect(s.dirty).toBe(false);view.unmount();
});
it('a later server revision is not silently accepted as the receipt revision',async()=>{
 const {s,view}=await setup(async()=>receipt,async()=>({...removed,draft_revision:'3',input:{...removed.input,name:'Another writer'}}));await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();expect(s.basis!.draft_revision).toBe('1');expect(s.editable!.name).toBe('A');expect(s.impactBlocked).toBe(true);await button(view,'放弃本地修改并采用操作后的服务器内容').trigger('click');expect(s.basis!.draft_revision).toBe('3');view.unmount();
});
it('late impact checks cannot authorize changed controls or a different session',async()=>{
 const s=useProjectStore();s.acceptDraft(original);const gate=deferred();s.setClient({call:()=>gate.promise} as never);const view=mount(ImpactCommands,{global:{plugins:[pinia]}});await button(view,'检查当前影响').trigger('click');await view.get('select').setValue('DELETE');gate.resolve({draft_id:'d',draft_revision:'1',action:'COPY',report_id:'r',blocking_findings:[],affected_release_ids:[]});await flushPromises();expect(button(view,'确认执行 COPY')).toBeUndefined();expect(s.impactReportCurrent).toBe(false);view.unmount();
});
it('UNKNOWN check uses its exact check request and remains inapplicable after switching drafts',async()=>{
 const s=useProjectStore();s.acceptDraft(original);const calls:any[]=[];s.setClient({call:async(op:string,input:unknown)=>{calls.push({op,input:clone(input)});if(calls.length===1)throw TypeError('offline check');return {draft_id:'d',draft_revision:'1',action:'COPY',report_id:'r',blocking_findings:[],affected_release_ids:[]}}} as never);let view=mount(ImpactCommands,{global:{plugins:[pinia]}});await button(view,'检查当前影响').trigger('click');await flushPromises();view.unmount();s.newDraft('KNOWLEDGE');view=mount(ImpactCommands,{global:{plugins:[pinia]}});await button(view,'恢复原影响操作').trigger('click');await flushPromises();expect(calls[1]).toEqual(calls[0]);expect(s.impactReportCurrent).toBe(false);expect(s.basis).toBeUndefined();view.unmount();
});
it('a deleted recovery buffer can be explicitly discarded after switching to C without clearing C',async()=>{
 const gate=deferred();const {s,view}=await setup(()=>gate.promise,async()=>removed,'DELETE');await button(view,'确认执行 DELETE').trigger('click');s.editable!.name='B';gate.resolve({...receipt,action:'DELETE',state:'DELETED'});await flushPromises();s.acceptDraft({...original,draft_id:'c',input:{...original.input,name:'C'}});await flushPromises();const discard=button(view,'放弃已删除草稿的本地恢复内容');expect(discard).toBeDefined();await discard.trigger('click');expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');await view.get('select').setValue('COPY');await button(view,'检查当前影响').trigger('click');await flushPromises();expect(s.impact!.draft.draft_id).toBe('c');view.unmount();
});
it('late command acknowledgement cannot replace draft C',async()=>{
 const gate=deferred();const {s,view}=await setup(()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');s.acceptDraft({...original,draft_id:'c',input:{...original.input,name:'C'}});gate.resolve(receipt);await flushPromises();expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');view.unmount();
});
it('a late readback cannot overwrite B entered after the command already committed',async()=>{
 const gate=deferred();const {s,view}=await setup(async()=>receipt,()=>gate.promise);await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();s.editable!.name='B';gate.resolve(removed);await flushPromises();expect(s.editable!.name).toBe('B');view.unmount();
});
it('UNKNOWN survives unmount and a new draft, and resumes the exact original key and payload',async()=>{
 let count=0;const {s,view,calls}=await setup(async()=>{if(count++===0)throw new TypeError('offline');return receipt});await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();const sent=calls.find(x=>x.op==='commandContentDraft')!.input;view.unmount();s.newDraft('KNOWLEDGE');s.editable!.name='C';
 const next=mount(ImpactCommands,{global:{plugins:[pinia]}});const recovery=button(next,'恢复原影响操作');expect(recovery).toBeDefined();await recovery.trigger('click');await flushPromises();expect(calls.filter(x=>x.op==='commandContentDraft').map(x=>x.input)).toEqual([sent,sent]);expect(s.editable!.name).toBe('C');expect(s.basis).toBeUndefined();next.unmount();
});
it('COMMITTED readback failure has a GET-only recovery entry',async()=>{
 let count=0;const {view,calls}=await setup(async()=>receipt,async()=>{if(count++===0)throw new TypeError('offline GET');return removed});await button(view,'确认执行 REMOVE_BINDING').trigger('click');await flushPromises();const recovery=button(view,'重新读取已提交内容');expect(recovery).toBeDefined();await recovery.trigger('click');await flushPromises();expect(calls.filter(x=>x.op==='commandContentDraft')).toHaveLength(1);expect(calls.filter(x=>x.op==='getContentDraft')).toHaveLength(2);view.unmount();
});
