import {beforeEach,expect,it,vi} from 'vitest';
import {createPinia,setActivePinia} from 'pinia';
import {useProjectStore} from '../src/stores/project';
import {useTasksStore} from '../src/stores/tasks';
beforeEach(()=>setActivePinia(createPinia()));
it('changing actual credential clears private task bodies and frozen candidate context',()=>{const p=useProjectStore();p.bearer='first';const t=useTasksStore();t.candidates.old={fields:{description:'private'}};t.candidateContexts.old={} as never;t.reports.old={} as never;p.signOut();expect(t.candidates).toEqual({});expect(t.candidateContexts).toEqual({});expect(t.reports).toEqual({})});
it('late response from previous credential cannot repopulate a draft',async()=>{const p=useProjectStore();p.bearer='first';let finish:(v:unknown)=>void=()=>{};p.setClient({call:vi.fn().mockImplementation(()=>new Promise(resolve=>{finish=resolve}))} as never);const work=p.loadDraft('original');p.signOut();p.bearer='second';finish({draft_id:'old',input:{objects:[],root_object_id:'old'}});await expect(work).rejects.toThrow();expect(p.basis).toBeUndefined()});
