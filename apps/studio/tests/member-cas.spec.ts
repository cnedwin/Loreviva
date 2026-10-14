import {expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {mount,flushPromises} from '@vue/test-utils';
import MemberGrants from '../src/components/MemberGrants.vue';
import {useProjectStore} from '../src/stores/project';
it('member command CAS uses the member grant-set revision, not one action revision',async()=>{
 const pinia=createPinia();setActivePinia(pinia);const scope={scope_type:'PRODUCT',tenant_id:'t',product_id:'p'} as const;
 const grant={scope,action:'content.edit',use:'PRODUCT_ADMIN',effect:'ALLOW',enabled:true,expires_at:null,revision:'2'};
 const member={credential_id:'c',subject_id:'s',enabled:true,credential_revision:'1',grant_set_revision:'5',expires_at:null,grants:[grant]};
 const call=vi.fn().mockResolvedValueOnce({items:[member],delegable_pairs:[{...grant,max_expires_at:null}]}).mockRejectedValue(Error('stop after captured request'));useProjectStore().setClient({call} as never);
 const view=mount(MemberGrants,{props:{scope},global:{plugins:[pinia]}});await flushPromises();await view.findAll('button').find(b=>b.text()==='查看与编辑')!.trigger('click');await view.findAll('button').find(b=>b.text()==='保存这个动作')!.trigger('click');await flushPromises();expect(call.mock.calls[1][1].body.expected_revision).toBe('5');view.unmount();
});
