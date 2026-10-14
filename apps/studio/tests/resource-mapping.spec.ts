import {afterEach,beforeEach,expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {shallowMount,flushPromises} from '@vue/test-utils';
import ResourcesView from '../src/views/ResourcesView.vue';
import {useProjectStore} from '../src/stores/project';
import type {ContentDraft} from '@loreviva/sdk-ts';
beforeEach(()=>setActivePinia(createPinia()));afterEach(()=>vi.restoreAllMocks());
it.each(['EDIT_ONLY','COPY_ONLY','EXPIRED','EDITORS_PRIVATE'])('hash preview cannot automatically widen %s text into a NOTE',async(mode)=>{
 const s=useProjectStore();s.acceptDraft({draft_id:'d',draft_revision:'1',input:{root_object_id:'note',name:'Draft',objects:[{object_id:'note',kind:'KNOWLEDGE',knowledge_type:'NOTE',text:'Original NOTE',audience_policy:'PRODUCT_READERS',rights:{allowed_uses:['EDIT','PUBLISH']}}]}} as unknown as ContentDraft);s.selectedId='note';
 const raw=new TextEncoder().encode('Uploaded restricted text');const hash=Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',raw))).map(b=>b.toString(16).padStart(2,'0')).join('');
 const record={asset_id:'asset',state:'READY',media_type:'text/plain',name:'Restricted',size_bytes:String(raw.length),sha256:hash,audience_policy:'PRODUCT_EDITORS',rights:{allowed_uses:mode==='COPY_ONLY'?['COPY']:['EDIT'],...(mode==='EXPIRED'?{expires_at:'2000-01-01T00:00:00Z'}:{})}};
 const call=vi.fn(async(op:string)=>op==='getCreatorCapabilities'?{upload_max_bytes:'10000'}:op==='listContentReleases'?{items:[]}:record);s.setClient({call,readAsset:vi.fn().mockResolvedValue(raw)} as never);vi.spyOn(window,'confirm').mockReturnValue(true);
 const view=shallowMount(ResourcesView);await flushPromises();await view.get('form input').setValue('asset');await view.get('form').trigger('submit');await flushPromises();await view.findAll('button').find(b=>b.text()==='读取有权文本')!.trigger('click');await flushPromises();await vi.waitFor(()=>expect(view.get('.preview').text()).toBe('Uploaded restricted text'));
 const adopt=view.findAll('button').find(b=>b.text().includes('采用到当前 NOTE'))!;await adopt.trigger('click');expect(s.selectedObject).toHaveProperty('text','Original NOTE');expect(adopt.attributes('disabled')).toBeDefined();expect(view.text()).toContain('安全映射尚未接通');expect(window.confirm).not.toHaveBeenCalled();view.unmount();
});
