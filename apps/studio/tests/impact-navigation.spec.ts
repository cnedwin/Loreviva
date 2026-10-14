import {beforeEach,afterEach,expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {mount,flushPromises} from '@vue/test-utils';
import ImpactCommands from '../src/components/ImpactCommands.vue';
import {useProjectStore,clone} from '../src/stores/project';
import type {ContentDraft} from '@loreviva/sdk-ts';
const original={draft_id:'a',draft_revision:'1',state:'DRAFT',input:{name:'A',description:'',tags:[],root_object_id:'o',objects:[{object_id:'o',kind:'KNOWLEDGE',name:'A',version:'1',description:'',tags:[],notes:[]}],module_agent_bindings:[{binding_id:'binding-1'}],skill_bindings:[],dependency_locks:[]}} as unknown as ContentDraft;
const removed={...original,draft_revision:'2',input:{...original.input,module_agent_bindings:[]}};
const receipt={draft_id:'a',draft_revision:'2',action:'REMOVE_BINDING',state:'DRAFT'};
function deferred(){let resolve!:(x:unknown)=>void;let reject!:(x:unknown)=>void;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return {promise,resolve,reject}}
let pinia:ReturnType<typeof createPinia>;
beforeEach(()=>{pinia=createPinia();setActivePinia(pinia);vi.spyOn(window,'confirm').mockReturnValue(true)});
afterEach(()=>vi.restoreAllMocks());
function button(view:ReturnType<typeof mount>,text:string){return view.findAll('button').find(b=>b.text()===text)!}
async function setup(action:string,get:(id:string)=>Promise<unknown>){
 const s=useProjectStore();s.acceptDraft(clone(original));const command=deferred();const calls:{op:string;input:any}[]=[];
 s.setClient({call:async(op:string,input:any)=>{calls.push({op,input:clone(input)});if(op==='getContentDraft')return get(input.path.draft_id);if(op==='commandContentDraft')return command.promise;if(op==='checkDraftImpact')return {draft_id:'a',draft_revision:'1',action,report_id:'r',blocking_findings:[],selected_targets:[],incoming_refs:[],duty_coverage:[],affected_release_ids:[],expires_at:'2099-01-01T00:00:00Z'};throw Error('Unexpected '+op)}} as never);
 const view=mount(ImpactCommands,{global:{plugins:[pinia]}});await view.get('select').setValue(action);if(action==='REMOVE_BINDING')await view.get('input').setValue('binding-1');await button(view,'检查当前影响').trigger('click');await flushPromises();await button(view,'确认执行 '+action).trigger('click');return {s,view,command,calls};
}
it.each(['DELETE','REMOVE_BINDING'])('failed navigation retains owning cleanup for delayed %s',async action=>{
 const {s,view,command,calls}=await setup(action,async id=>{if(id==='c')throw Error('C unavailable');return removed});s.editable!.name='B';
 await expect(s.loadDraft('c')).rejects.toThrow('C unavailable');expect(s.basis!.draft_id).toBe('a');expect(s.editable!.name).toBe('B');
 command.resolve(action==='DELETE'?{...receipt,action,state:'DELETED'}:receipt);await flushPromises();
 expect(s.impactBlocked).toBe(true);expect(s.editable!.name).toBe('B');await expect(s.saveDraft()).rejects.toThrow();expect(calls.filter(c=>c.op==='saveContentDraft')).toHaveLength(0);
 if(action==='DELETE'){expect(s.basis).toBeUndefined();expect(s.newKind).toBeUndefined();await expect(s.createDraft()).rejects.toThrow();await button(view,'放弃已删除草稿的本地恢复内容').trigger('click');expect(s.editable).toBeUndefined()}
 else {expect(s.basis!.draft_revision).toBe('2');expect(s.basis!.input.module_agent_bindings).toEqual([]);expect(s.editable!.module_agent_bindings).toHaveLength(1);await button(view,'放弃本地修改并采用操作后的服务器内容').trigger('click');expect(s.editable!.module_agent_bindings).toEqual([]);expect(s.dirty).toBe(false)}
 view.unmount();
});
const draftC={...original,draft_id:'c',input:{...original.input,name:'C'}};
const draftD={...original,draft_id:'d',input:{...original.input,name:'D'}};
it('failed navigation during committed GET keeps B and requires reconciliation',async()=>{
 const readback=deferred();const {s,view,command,calls}=await setup('REMOVE_BINDING',id=>id==='a'?readback.promise:Promise.reject(Error('C unavailable')));
 command.resolve(receipt);await flushPromises();s.editable!.name='B';await expect(s.loadDraft('c')).rejects.toThrow('C unavailable');readback.resolve(removed);await flushPromises();
 expect(s.basis!.draft_revision).toBe('2');expect(s.editable!.name).toBe('B');expect(s.impactBlocked).toBe(true);expect(calls.filter(c=>c.op==='commandContentDraft')).toHaveLength(1);await expect(s.saveDraft()).rejects.toThrow();view.unmount();
});
it.each(['DELETE','REMOVE_BINDING'])('successful navigation isolates delayed %s from C',async action=>{
 const {s,view,command}=await setup(action,async id=>id==='c'?draftC:removed);s.editable!.name='B';await s.loadDraft('c');command.resolve(action==='DELETE'?{...receipt,action,state:'DELETED'}:receipt);await flushPromises();expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');expect(s.impactBlocked).toBe(false);view.unmount();
});
it.each(['DELETE','REMOVE_BINDING','COPY'])('automatic %s completion does not cancel pending user navigation',async action=>{
 const navigate=deferred();const {s,view,command}=await setup(action,id=>id==='c'?navigate.promise:Promise.resolve(removed));const opening=s.loadDraft('c');command.resolve(action==='DELETE'?{...receipt,action,state:'DELETED'}:action==='COPY'?{draft:{...original,draft_id:'copy'},source_draft_id:'a'}:receipt);await flushPromises();navigate.resolve(draftC);await opening;expect(s.basis!.draft_id).toBe('c');expect(s.editable!.name).toBe('C');view.unmount();
});
it.each(['resolve','reject'])('an older navigation %s cannot supersede the newer pending request',async finish=>{
 const c=deferred(),d=deferred();const {s,view}=await setup('REMOVE_BINDING',id=>id==='c'?c.promise:d.promise);const first=s.loadDraft('c');const firstOutcome=first.catch(e=>e);const second=s.loadDraft('d');if(finish==='resolve')c.resolve(draftC);else c.reject(Error('old failure'));await firstOutcome;expect(s.basis!.draft_id).toBe('a');d.resolve(draftD);await second;expect(s.basis!.draft_id).toBe('d');view.unmount();
});
it('a late C response cannot replace already accepted D',async()=>{
 const c=deferred();const {s,view}=await setup('DELETE',id=>id==='c'?c.promise:Promise.resolve(draftD));const first=s.loadDraft('c');await s.loadDraft('d');c.resolve(draftC);await first;expect(s.basis!.draft_id).toBe('d');view.unmount();
});
it.each(['new','accepted','identity','product'])('%s invalidates a pending read without reusing its attempt identity',async choice=>{
 const c=deferred();const {s,view}=await setup('DELETE',()=>c.promise);const opening=s.loadDraft('c');const outcome=opening.catch(e=>e);if(choice==='new')s.newDraft('KNOWLEDGE');else if(choice==='accepted')s.acceptDraft(draftD);else if(choice==='identity')s.bearer='different';else s.productId='another';c.resolve(draftC);const result=await outcome;expect(s.basis?.draft_id).not.toBe('c');if(choice==='accepted')expect(s.editable!.name).toBe('D');if(choice==='new')expect(s.newKind).toBe('KNOWLEDGE');if(choice==='identity'||choice==='product')expect(result).toBeInstanceOf(Error);view.unmount();
});
it.each(['server','local','discard'])('explicit %s choice supersedes a pending read',async choice=>{
 const c=deferred();const action=choice==='discard'?'DELETE':'REMOVE_BINDING';const {s,view,command}=await setup(action,id=>id==='c'?c.promise:Promise.resolve(removed));s.editable!.name='B';command.resolve(action==='DELETE'?{...receipt,action,state:'DELETED'}:receipt);await flushPromises();const opening=s.loadDraft('c');const text=choice==='server'?'放弃本地修改并采用操作后的服务器内容':choice==='local'?'已对照差异，明确保留本地版本':'放弃已删除草稿的本地恢复内容';await button(view,text).trigger('click');c.resolve(draftC);await opening;expect(s.basis?.draft_id).not.toBe('c');if(choice==='discard')expect(s.editable).toBeUndefined();else expect(s.editable!.name).toBe(choice==='local'?'B':'A');view.unmount();
});
