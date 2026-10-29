import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,readFile,writeFile,rm} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {generateContracts, loadSources, validateExamples, buildPortableBodySchema} from './generate-contracts.mjs';
import {checkGenerated} from './check-generated.mjs';
const repo = fileURLToPath(new URL('../', import.meta.url));
const sourceRoot = path.resolve(repo,'contracts/source');
test('portable projection omits inapplicable example annotations',async()=>{
 const {openapi,wepkg}=await loadSources(sourceRoot);
 assert.ok(openapi.components.schemas.DraftInput.examples.length>0,'source example precondition');
 const projected=buildPortableBodySchema(openapi,wepkg).document;
 assert.ok(!Object.hasOwn(projected.$defs.DraftInput,'examples'),'obsolete local examples retained');
 const changed=structuredClone(openapi);changed.components.schemas.WorldDefinition.properties.example={type:'string',minLength:7,examples:['annotation only']};
 const again=buildPortableBodySchema(changed,wepkg).document;
 assert.equal(again.$defs.WorldDefinition.properties.example.type,'string');
 assert.equal(again.$defs.WorldDefinition.properties.example.minLength,7);
 assert.ok(!Object.hasOwn(again.$defs.WorldDefinition.properties.example,'examples'),'annotation copied into field schema');
 assert.ok(openapi.components.schemas.DraftInput.examples.length>0,'original source mutated');
});

test('portable projection rejects unsafe and ambiguous operations',async()=>{
 const {openapi,wepkg}=await loadSources(sourceRoot);
 for(const property of ['__proto__','prototype','constructor']){
  const bad=structuredClone(wepkg);bad['x-loreviva-portable-body'].operations.push({op:'add-property',component:'DraftInput',property,schema:{type:'string'},required:false});
  assert.throws(()=>buildPortableBodySchema(openapi,bad),'unsafe property '+property);
 }
 const ignored=structuredClone(wepkg);ignored['x-loreviva-portable-body'].operations.push({op:'replace-property',component:'DraftInput',property:'name',schema:{type:'string'},required:false});assert.throws(()=>buildPortableBodySchema(openapi,ignored),'ignored required directive');
 const mixed=structuredClone(wepkg);mixed['x-loreviva-portable-body'].operations.push({op:'clone',name:'AmbiguousClone',schema:{type:'string'},source:'openapi',pointer:'/components/schemas/Identity'});assert.throws(()=>buildPortableBodySchema(openapi,mixed),'ambiguous schema origin');
});

test('portable projection rejects malformed target policies',async()=>{
 const {openapi,wepkg}=await loadSources(sourceRoot);
 for(const value of ['ROLE',['UNKNOWN_KIND'],['ROLE','ROLE'],null,[]]){
  const bad=structuredClone(wepkg);bad['x-loreviva-portable-body'].reference_targets.role_ref=value;assert.throws(()=>buildPortableBodySchema(openapi,bad),'invalid role_ref target policy');
 }
 const unused=structuredClone(wepkg);unused['x-loreviva-portable-body'].reference_targets.unused_policy=['ROLE'];assert.throws(()=>buildPortableBodySchema(openapi,unused),'unused target policy');
});

test('portable body artifacts follow the source projection',async()=>{
 const root=await mkdtemp(path.join(os.tmpdir(),'loreviva-portable-body-'));
 try{
  const result=await generateContracts({sourceRoot,outputRoot:root});
  for(const file of ['contracts/generated/wepkg-portable.schema.json','internal/generated/wepkg-portable.schema.json','internal/generated/wepkg_portable_validate.go'])assert.ok(result.outputs.includes(file),'portable output missing: '+file);
  assert.equal(await readFile(path.join(root,'contracts/generated/wepkg-portable.schema.json'),'utf8'),await readFile(path.join(root,'internal/generated/wepkg-portable.schema.json'),'utf8'));
  const schema=JSON.parse(await readFile(path.join(root,'contracts/generated/wepkg-portable.schema.json'),'utf8'));
  assert.equal(schema['x-loreviva-portable-metadata'].reference_paths.length,45);
  for(const name of ['objects','module_agent_bindings','skill_bindings']){assert.ok(schema['x-loreviva-portable-metadata'].diagnostic_fields.includes(name));assert.ok(schema['x-loreviva-portable-metadata'].diagnostic_arrays.includes(name));}
  assert.equal(schema.$defs.DraftInput.properties.body_version.const,'1.0');
  assert.equal(schema.$defs.DraftInput.properties.dependency_locks,undefined);
  assert.ok((await readFile(path.join(root,'internal/generated/wepkg_portable_validate.go'),'utf8')).includes('func ValidatePortableBody('));
 }finally{await rm(root,{recursive:true,force:true})}
});

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
 assert.equal(openapi.info.version,'1.8.0');
 const ids=Object.values(openapi.paths).flatMap(x=>Object.values(x).filter(v=>v.operationId).map(v=>v.operationId));
 assert.equal(ids.length,70);
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

test('portable RULES body has a closed auxiliary schema',async()=>{
 const {openapi,wepkg}=await loadSources(sourceRoot);
 const schema=buildPortableBodySchema(openapi,wepkg).document.$defs.PortableRulesBody;
 assert.ok(schema,'RULES auxiliary schema missing');
 assert.equal(schema.additionalProperties,false);
 assert.deepEqual(schema.required,['primitives']);
 assert.equal(schema.properties.primitives.maxItems,8);
 assert.equal(schema.properties.primitives.uniqueItems,true);
 assert.deepEqual(schema.properties.primitives.items,{$ref:'#/$defs/PortableLogicalID'});
});

// Compare every unmodified source subtree after the declared projection holes
// are removed. This catches lost sibling constraints and nested source drift.
test('portable projection preserves every unchanged source constraint',async()=>{
 const {openapi,wepkg}=await loadSources(sourceRoot);const {document,source_components}=buildPortableBodySchema(openapi,wepkg);const spec=wepkg['x-loreviva-portable-body'];
 const normalize=value=>{
  if(typeof value==='boolean')return value;const node=structuredClone(value);delete node.example;delete node.examples;delete node.description;
  if(node.$ref?.startsWith('#/components/schemas/'))node.$ref='#/$defs/'+node.$ref.slice(21);
  for(const key of ['properties','patternProperties','$defs','definitions','dependentSchemas'])if(node[key])for(const name of Object.keys(node[key]))node[key][name]=normalize(node[key][name]);
  for(const key of ['items','additionalProperties','unevaluatedProperties','contains','not','if','then','else','propertyNames','additionalItems','unevaluatedItems','contentSchema'])if(node[key]!==undefined)node[key]=normalize(node[key]);
  for(const key of ['allOf','anyOf','oneOf','prefixItems'])if(node[key])node[key]=node[key].map(normalize);
  return node;
 };
 const omit=(root,pointer)=>{const bits=pointer.split('/').slice(1).map(s=>s.replaceAll('~1','/').replaceAll('~0','~'));let at=root;for(const key of bits.slice(0,-1)){if(!at||!Object.hasOwn(at,key))return;at=at[key]};if(at)delete at[bits.at(-1)];};
 for(const name of Object.keys(source_components)){
  const expected=normalize(openapi.components.schemas[name]),actual=normalize(document.$defs[name]);
  const prefix='/$defs/'+name;
  for(const op of spec.operations){
   if(op.op==='replace-schema'&&(op.pointer===prefix||op.pointer.startsWith(prefix+'/'))){const local=op.pointer.slice(prefix.length);if(local===''){Object.keys(expected).forEach(k=>delete expected[k]);Object.keys(actual).forEach(k=>delete actual[k]);}else {omit(expected,local);omit(actual,local)}}
   if(op.component===name){omit(expected,'/properties/'+op.property);omit(actual,'/properties/'+op.property);if(expected.required)expected.required=expected.required.filter(k=>k!==op.property);if(actual.required)actual.required=actual.required.filter(k=>k!==op.property);}
  }
  assert.deepEqual(actual,expected,'source constraints drifted in '+name);
 }
 const changed=structuredClone(openapi);changed.components.schemas.WorldDefinition.properties.description.maxLength=417;
 assert.equal(buildPortableBodySchema(changed,wepkg).document.$defs.WorldDefinition.properties.description.maxLength,417);
 for(const operation of [{op:'unknown'},{op:'remove-property',component:'DraftInput',property:'missing'},{op:'replace-schema',pointer:'/$defs/missing',schema:{type:'object'}}]){const bad=structuredClone(wepkg);bad['x-loreviva-portable-body'].operations.push(operation);assert.throws(()=>buildPortableBodySchema(openapi,bad));}
 const external=structuredClone(openapi);external.components.schemas.WorldDefinition.properties.name={$ref:'https://example.invalid/private-schema'};assert.throws(()=>buildPortableBodySchema(external,wepkg));
});
