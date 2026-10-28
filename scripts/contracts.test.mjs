import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,readFile,writeFile,rm} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {generateContracts, loadSources, validateExamples} from './generate-contracts.mjs';
import {checkGenerated} from './check-generated.mjs';
const repo = fileURLToPath(new URL('../', import.meta.url));
const sourceRoot = path.resolve(repo,'contracts/source');
test('wepkg backend artifacts derive from the sole source and reject missing or changed copies',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-wepkg-backend-'));
 try {
  const result=await generateContracts({sourceRoot,outputRoot:root});
  for(const file of ['internal/generated/wepkg.schema.json','internal/generated/wepkg_validate.go'])assert.ok(result.outputs.includes(file),file);
  const {wepkg}=await loadSources(sourceRoot);
  assert.deepEqual(JSON.parse(await readFile(path.join(root,'internal/generated/wepkg.schema.json'),'utf8')),wepkg);
  const target='internal/generated/wepkg_validate.go';
  await writeFile(path.join(root,target),'stale');
  assert.ok((await checkGenerated({sourceRoot,outputRoot:root})).includes(target));
 } finally {await rm(root,{recursive:true,force:true})}
});
test('original18 operations permit only explicit memory protocol deltas; original43 DTOs remain immutable',async()=>{
 const old=JSON.parse(await readFile(path.join(repo,'scripts/fixtures/original-public-contract.json'),'utf8'));
 const {openapi}=await loadSources(sourceRoot);
 for(const [route,item]of Object.entries(old.paths)){
  const current=structuredClone(openapi.paths[route]);
  if(route==='/world/v1/instances/{instance_id}/memories'||route==='/world/v1/instances/{instance_id}/memories/{memory_id}'){
   assert.deepEqual(current.get.parameters.pop(),{name:'include',in:'query',required:false,schema:{type:'string',enum:['provenance_v1']}});
   const legacy=route.endsWith('/memories')?'MemoryPage':'Memory';
   const projected=route.endsWith('/memories')?'MemoryProvenancePageV1':'MemoryProvenanceV1';
   assert.deepEqual(current.get.responses['200'].content['application/json'].schema,{oneOf:[{$ref:'#/components/schemas/'+legacy},{$ref:'#/components/schemas/'+projected}]});
   assert.deepEqual(current.get['x-loreviva-response-projection'],{parameter:'include',value:'provenance_v1',status:'200',default_schema:{$ref:'#/components/schemas/'+legacy},projection_schema:{$ref:'#/components/schemas/'+projected}});
   delete current.get['x-loreviva-response-projection'];
   current.get.responses['200'].content['application/json'].schema={$ref:'#/components/schemas/'+legacy};
  }
  if(route==='/world/v1/instances/{instance_id}/memory-governance'){
   assert.deepEqual(current.post.requestBody.content['application/json'].schema,{$ref:'#/components/schemas/MemoryGovernanceCommandV1'});
   current.post.requestBody.content['application/json'].schema={$ref:'#/components/schemas/MemoryGovernanceRequest'};
  }
  assert.deepEqual(current,item,route);
 }
 for(const [name,schema]of Object.entries(old.schemas))assert.deepEqual(openapi.components.schemas[name],schema,name);
});
test('1.5 retains approved 1.4 content/runtime/memory and fixed Operator reads with seven settings operations',async()=>{
 const {openapi}=await loadSources(sourceRoot);
 assert.equal(openapi.info.version,'1.6.0');
 const ids=Object.values(openapi.paths).flatMap(x=>Object.values(x).filter(v=>v.operationId).map(v=>v.operationId));
 assert.equal(ids.length,67);
 for(const id of ['createContentDraft','saveContentDraft','publishContentDraft','readAuthorizedAsset','previewInstanceMigration','executeInstanceMigration','createAdminInstance','listAdminInstances','getAdminInstance','controlAdminInstance','listAdminActorInstances'])assert.ok(ids.includes(id),id);
 for(const name of ['DraftInput','ContentDraft','Release','PublishCheckReport','AdminTaskReceipt','MigrationPlan','CharacterDefinition','InitialActorSetup'])assert.ok(openapi.components.schemas[name],name);
 assert.deepEqual(openapi.components.schemas.TaskReceipt.properties.task_kind.enum,['TURN','CONTROL','MEMORY_GOVERNANCE','MEDIA','MEMORY_MAINTENANCE']);
});
test('same canonical source generates identical bytes and hash',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-contract-'));
 try {
  const a=await generateContracts({sourceRoot,outputRoot:path.join(root,'a')});
  const b=await generateContracts({sourceRoot,outputRoot:path.join(root,'b')});
  assert.equal(a.source_hash,b.source_hash); assert.deepEqual(a.outputs,b.outputs);
  for(const output of a.outputs) assert.deepEqual(await readFile(path.join(root,'a',output)),await readFile(path.join(root,'b',output)));
  assert.deepEqual(await checkGenerated({sourceRoot,outputRoot:path.join(root,'a')}),[]);
  await writeFile(path.join(root,'a',a.outputs[0]),'tampered');
  assert.ok((await checkGenerated({sourceRoot,outputRoot:path.join(root,'a')})).includes(a.outputs[0]));
 } finally {await rm(root,{recursive:true,force:true})}
});
test('operation IDs unique and SDK covers every operation',async()=>{
 const {openapi}=await loadSources(sourceRoot);
 const ids=Object.values(openapi.paths).flatMap(x=>Object.values(x).filter(v=>v.operationId).map(v=>v.operationId));
 assert.equal(new Set(ids).size,ids.length);
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-ops-'));
 try {await generateContracts({sourceRoot,outputRoot:root}); const ts=await readFile(path.join(root,'packages/sdk-ts/src/generated/contracts.ts'),'utf8'); for(const id of ids) assert.ok(ts.includes(JSON.stringify(id)+':'),id)} finally {await rm(root,{recursive:true,force:true})}
});
test('all canonical examples validate with JSON Schema 2020-12',async()=>{const result=await validateExamples(sourceRoot); assert.ok(result.count>0); assert.deepEqual(result.errors,[])});
test('duplicate operation IDs and changed exact numeric example are rejected',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-invalid-'));
 try {const {openapi,wepkg}=await loadSources(sourceRoot); const ops=Object.values(openapi.paths).flatMap(x=>Object.values(x).filter(v=>v.operationId)); const original=ops[1].operationId; ops[1].operationId=ops[0].operationId;
 await writeFile(path.join(root,'openapi.yaml'),JSON.stringify(openapi)); await writeFile(path.join(root,'wepkg.schema.json'),JSON.stringify(wepkg));
 await assert.rejects(()=>generateContracts({sourceRoot:root,outputRoot:path.join(root,'out')}),/duplicate/i);
 ops[1].operationId=original;
 openapi.components.schemas.Versions.examples=[{release_id:'release',world_revision:9007199254740992,control_version:'1',privacy_epoch:'1',view_epoch:'1'}];
 await writeFile(path.join(root,'openapi.yaml'),JSON.stringify(openapi)); assert.ok((await validateExamples(root)).errors.length>0);
 } finally {await rm(root,{recursive:true,force:true})}
});
