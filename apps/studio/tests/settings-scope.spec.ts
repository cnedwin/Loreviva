import {expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {shallowMount,flushPromises} from '@vue/test-utils';
import SettingsView from '../src/views/SettingsView.vue';
import {useProjectStore} from '../src/stores/project';
it('late product read cannot replace the selected tenant settings',async()=>{
 const pinia=createPinia();setActivePinia(pinia);const s=useProjectStore();let product!:(v:unknown)=>void;let tenant!:(v:unknown)=>void;
 s.setClient({call:vi.fn().mockImplementationOnce(()=>new Promise(resolve=>product=resolve)).mockImplementationOnce(()=>new Promise(resolve=>tenant=resolve))} as never);
 const view=shallowMount(SettingsView,{global:{plugins:[pinia]}});await view.find('select').setValue('TENANT');
 const snapshot=(scope_type:string)=>({basis:{scope:{scope_type}},installation_storage:{}});
 tenant(snapshot('TENANT'));await flushPromises();product(snapshot('PRODUCT'));await flushPromises();
 expect(view.findComponent({name:'SettingsSections'}).props('snapshot').basis.scope.scope_type).toBe('TENANT');view.unmount();
});
