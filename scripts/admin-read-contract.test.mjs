import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {loadSources} from './generate-contracts.mjs';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
const names=['DraftGenerationResolutionRequest','DraftGenerationSupport','DraftGenerationResolution','AdminTaskAccountingStatus','AdminTaskListAdminItem','AdminTaskListWorldItem','AdminTaskListItem','AdminTaskPage'].sort();
test('1.3 adds exactly2 operations/8 schemas and preserves every1.2 path/schema',async()=>{
 const old=JSON.parse(await readFile(new URL('../tests/fixtures/contract12-before-task06.json',import.meta.url),'utf8'));
 const {openapi:o}=await loadSources();
 assert.equal(old.info.version,'1.2.0');assert.equal(o.info.version,'1.3.0');
 for(const [p,v] of Object.entries(old.paths))assert.deepEqual(o.paths[p],v,p);
 for(const [n,v] of Object.entries(old.components.schemas))assert.deepEqual(o.components.schemas[n],v,n);
 assert.deepEqual(Object.keys(o.components.schemas).filter(n=>!old.components.schemas[n]).sort(),names);
 const additions=Object.keys(o.paths).filter(p=>!old.paths[p]);assert.deepEqual(additions.sort(),['/world/v1/admin/content-drafts/{draft_id}/generation-resolution','/world/v1/admin/tasks']);
 for(const p of additions){const [op]=Object.values(o.paths[p]);assert.deepEqual(op.security,[{OperatorBearer:[]}]);assert.equal(op['x-owner-task'],6)}
 assert.equal(o.paths[additions[0]].post.operationId,'resolveDraftGeneration');assert.equal(o.paths[additions[1]].get.operationId,'listAdminTasks');
});
test('new closed schemas enforce read configuration and independent safe metadata',async()=>{
 const {openapi:o}=await loadSources();const ajv=new Ajv2020({strict:false,allErrors:true});addFormats(ajv);ajv.addSchema({...o,$id:'https://schemas.loreviva.invalid/contract13',$schema:o.jsonSchemaDialect});
 const valid=(n,v)=>ajv.validate({$ref:'https://schemas.loreviva.invalid/contract13#/components/schemas/'+n},v);
 const scope={tenant_id:'demo-local',product_id:'coffee'};
 const item={family:'WORLD',scope:{...scope,world_instance_id:'actual-world'},task_id:'original-turn',task_kind:'TURN',task_state:'COMMITTED',query_url:'/world/v1/admin/tasks?product_id=coffee&task_id=original-turn',accounting_status:{service_settlement_state:'PENDING_VERIFICATION',supplier_reconciliation_state:'PENDING_VERIFICATION',platform_risk_state:'RESERVED'}};
 assert.equal(valid('AdminTaskPage',{items:[item]}),true,JSON.stringify(ajv.errors));assert.equal(valid('AdminTaskPage',{items:[],next_cursor:'opaque'}),true);
 assert.equal(valid('AdminTaskListItem',{...item,packet:'private'}),false);assert.equal(valid('AdminTaskPage',{items:[],total:5}),false);
 const response={scope,draft_id:'draft',object_id:'object',draft_revision:'1',draft_hash:'a'.repeat(64),module_id:'AUTHORING',owner_kind:'WORLD',state:'NONE',effective_settings:{mode:'NONE',required_capabilities:[],allowed_external_uses:['NONE']},support:{supported_fields:[],input_upper_bound:'16000',output_upper_bound:'4000',max_model_calls:2,mock_only:true}};
 assert.equal(valid('DraftGenerationResolution',response),true,JSON.stringify(ajv.errors));assert.equal(valid('DraftGenerationResolution',{...response,route_hash:'b'.repeat(64)}),false);
 assert.equal(valid('DraftGenerationResolution',{...response,state:'RESOLVED'}),false);
 assert.equal(valid('DraftGenerationResolutionRequest',{product_id:'coffee',expected_draft_revision:'1',object_id:'object',module_id:'AUTHORING',prompt:'hidden'}),false);
});
