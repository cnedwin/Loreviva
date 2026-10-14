import {expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {mount,flushPromises} from '@vue/test-utils';
import ImpactCommands from '../src/components/ImpactCommands.vue';
import {useProjectStore} from '../src/stores/project';
it('committed deletion uses its receipt without reading a deleted draft',async()=>{
 const pinia=createPinia();setActivePinia(pinia);const s=useProjectStore();s.acceptDraft({draft_id:'d',draft_revision:'1',input:{root_object_id:'o',objects:[{object_id:'o'}]}} as never);
 const receipt={draft_id:'d',draft_revision:'2',action:'DELETE',state:'DELETED'};
 const call=vi.fn().mockResolvedValueOnce({draft_id:'d',draft_revision:'1',action:'DELETE',report_id:'r',blocking_findings:[],affected_release_ids:[]}).mockResolvedValueOnce(receipt).mockRejectedValue(Error('Deleted draft is not readable'));s.setClient({call} as never);
 const confirm=vi.spyOn(window,'confirm').mockReturnValue(true);const view=mount(ImpactCommands,{global:{plugins:[pinia]}});await view.get('select').setValue('DELETE');await view.findAll('button').find(b=>b.text()==='检查当前影响')!.trigger('click');await flushPromises();await view.findAll('button').find(b=>b.text()==='确认执行 DELETE')!.trigger('click');await flushPromises();
 expect(call).toHaveBeenCalledTimes(2);expect(s.impact?.result).toEqual(receipt);expect(s.basis).toBeUndefined();expect(s.editable).toBeUndefined();confirm.mockRestore();view.unmount();
});
