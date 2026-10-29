import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile,writeFile,mkdtemp,rm} from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import vm from 'node:vm';
import {createRequire} from 'node:module';
import {createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import ts from 'typescript';
import * as generator from '../../scripts/generate-contracts.mjs';
const names='PackageAssetMapping PackageDependencyMapping PackageImportPreview PackageModelMapping PackagePreviewID PackagePreviewOptions PackagePreviewRequirements PackageRightsMapping ContentPackageCapabilities IntegrationApprovedPacket IntegrationCapabilities IntegrationChallenge IntegrationContractProfile IntegrationDeliveryUpdate IntegrationEventPage IntegrationNotification IntegrationNotificationPage IntegrationProjectionRule IntegrationReceipt IntegrationReceiptRequest IntegrationSSEUpdate IntegrationTurnView OriginalTurnIntakeReceipt PackagePreflightFinding PackagePreflightLimits PackagePreflightReport PlayerMemoryMetadata PlayerMemoryMetadataPage PlayerSelfConsentCommandRequest PlayerSelfConsentReceipt PlayerSelfConsentSnapshot PlayerSelfConsentState RuntimeMediaRef'.split(' ').sort();
const ids='previewContentPackage commandPlayerSelfConsent getContentPackageCapabilities getCustomerNotifications getIntegrationCapabilities getOriginalTurnIntakeReceipt getPlayerSelfConsent preflightContentPackage readRuntimeMedia recordCustomerNotificationReceipt'.split(' ').sort();
const operations=o=>Object.values(o.paths).flatMap(p=>Object.values(p).filter(x=>x.operationId));
const ref=n=>({$ref:'#/components/schemas/'+n});
async function temp(t){const p=await mkdtemp(path.join(os.tmpdir(),'loreviva-integration-contract-'));t.after(()=>rm(p,{recursive:true,force:true}));return p;}
function validator(o){const a=new Ajv2020({strict:false,allErrors:true});addFormats(a);a.addSchema({...o,$id:'https://test.loreviva.invalid/profile'});return (n,v)=>a.validate({$ref:'https://test.loreviva.invalid/profile#/components/schemas/'+n},v);}
test('integration additions preserve every approved baseline object',async()=>{
 const old=JSON.parse(await readFile(new URL('./fixtures/contract14-approved-baseline.json',import.meta.url),'utf8'));
 const {openapi:o,source_hashes}=await generator.loadSources();
 assert.equal(o.info.version,'1.7.0');
 for(const [p,v]of Object.entries(old.paths))assert.deepEqual(o.paths[p],v,p);
 for(const [n,v]of Object.entries(old.components.schemas))assert.deepEqual(o.components.schemas[n],v,n);
 assert.deepEqual(Object.keys(o.components.schemas).filter(n=>!old.components.schemas[n]).sort(),names);
 assert.deepEqual(operations(o).filter(x=>!operations(old).some(y=>y.operationId===x.operationId)).map(x=>x.operationId).sort(),ids);
 assert.equal(operations(o).length,68);assert.equal(Object.keys(o.components.schemas).length,189);
 // Only the adopted portable-body descriptor is additive. The remaining
 // exact canonical bytes must still match the original approved baseline.
 const packageRaw=await readFile(new URL('../../contracts/source/wepkg.schema.json',import.meta.url));
 assert.equal(source_hashes['wepkg.schema.json'],createHash('sha256').update(packageRaw).digest('hex'));
 const legacyPackage=JSON.parse(packageRaw.toString('utf8'));
 assert.deepEqual(packageRaw,Buffer.from(JSON.stringify(legacyPackage,null,2)+'\n'));
 assert.ok(Object.hasOwn(legacyPackage,'x-loreviva-portable-body'));
 delete legacyPackage['x-loreviva-portable-body'];
 assert.equal(createHash('sha256').update(JSON.stringify(legacyPackage,null,2)+'\n').digest('hex'),'d12efa2de15d1ad4ae5ca9dc1fc093343451a059713326ceccca38e31a075d03');
 for(const op of operations(o).filter(x=>ids.includes(x.operationId))){const declared=op['x-implementation-status'];assert.ok(declared==='design_not_implemented'||declared==='implemented',op.operationId+' status '+declared);if(op.operationId==='preflightContentPackage'||op.operationId==='getContentPackageCapabilities'||op.operationId==='previewContentPackage')assert.equal(declared,'implemented',op.operationId);else assert.equal(declared,'design_not_implemented',op.operationId);}
});
test('new schemas reject authority injection, cross-tuples and unavailable media fields',async()=>{
 const {openapi:o}=await generator.loadSources();assert.ok(o.components.schemas.RuntimeMediaRef,'runtime media schema missing');const v=validator(o);
 for(const n of names){assert.ok(o.components.schemas[n].examples?.length,n+' examples');for(const example of o.components.schemas[n].examples)assert.equal(v(n,example),true,n);}
 const pending={media_id:'audio',media_kind:'AUDIO',media_state:'PENDING',resource_revision:'1'};
 const ready={...pending,media_state:'READY',asset_id:'asset',mime_type:'audio/wav',byte_length:'160044',content_sha256:'a'.repeat(64),content_path:'/world/v1/runtime-media/asset/content'};
 assert.equal(v('RuntimeMediaRef',ready),true);
 for(const bad of [{...pending,asset_id:'asset'},{...ready,content_path:'https://remote/private'},{...ready,mime_type:'audio/mpeg'},{...ready,byte_length:160044},{...ready,body:'private'}])assert.equal(v('RuntimeMediaRef',bad),false);
 for(const key of ['asset_id','mime_type','byte_length','content_sha256','content_path']){const bad={...ready};delete bad[key];assert.equal(v('RuntimeMediaRef',bad),false,key);}
 const self={command:'INITIAL',expected_revision:'0',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'};
 assert.equal(v('PlayerSelfConsentCommandRequest',self),true);
 for(const extra of [{subject_id:'other'},{expires_at:'2099-01-01T00:00:00Z'},{expected_revision:'1'},{use_authority_generation:'1'}])assert.equal(v('PlayerSelfConsentCommandRequest',{...self,...extra}),false);
 const rules=o.components.schemas.IntegrationProjectionRule.examples;
 for(const rule of rules){assert.equal(v('IntegrationProjectionRule',{...rule,operation_id:'getSnapshot'}),false);assert.equal(v('IntegrationProjectionRule',{...rule,value:'provenance_v1'}),false);}
 const metadata=o.components.schemas.PlayerMemoryMetadata.examples[0];assert.equal(v('PlayerMemoryMetadata',{...metadata,content:'private'}),false);
 const intake=o.components.schemas.OriginalTurnIntakeReceipt.examples[0];assert.equal(v('OriginalTurnIntakeReceipt',{...intake,body:'private'}),false);
 const delivery=o.components.schemas.IntegrationDeliveryUpdate.examples[0];assert.equal(v('IntegrationDeliveryUpdate',{...delivery,delivery_state:'WITHDRAWN'}),false);const noPacket={...delivery};delete noPacket.available_packet;assert.equal(v('IntegrationDeliveryUpdate',noPacket),false);
 const frame=o.components.schemas.IntegrationSSEUpdate.examples[0];assert.equal(v('IntegrationSSEUpdate',frame),true);assert.equal(v('IntegrationSSEUpdate',{...frame,data:{raw_sequence:'1'}}),false);
});
test('capability profiles cannot duplicate a selector or advertise the other profile document',async()=>{
 const {openapi:o}=await generator.loadSources();const v=validator(o);const caps=structuredClone(o.components.schemas.IntegrationCapabilities.examples[0]);
 assert.equal(v('IntegrationCapabilities',caps),true);
 const bad=structuredClone(caps);bad.profiles[1]={...bad.profiles[0],openapi_sha256:'b'.repeat(64)};assert.equal(v('IntegrationCapabilities',bad),false);
 const integration=structuredClone(caps.profiles[1]);integration.projections=Array(5).fill(integration.projections[0]);assert.equal(v('IntegrationContractProfile',integration),false);
 assert.equal(v('IntegrationContractProfile',{...caps.profiles[0],openapi_ref:'/contracts/profiles/integration_r0_v1.openapi.json'}),false);
});
test('resolved profiles expose standard exact schemas, explicit provenance and string SSE without mutating source',async t=>{
 const root=await temp(t);const {openapi:o}=await generator.loadSources();const before=structuredClone(o);
 const result=await generator.generateContracts({outputRoot:root});
 assert.ok(result.outputs.includes('contracts/generated/profiles/integration_r0_v1.openapi.json'),'resolved profile output missing');
 for(const profile of ['public_v1','integration_r0_v1']){
  const p=JSON.parse(await readFile(path.join(root,'contracts/generated/profiles/'+profile+'.openapi.json'),'utf8'));const ops=operations(p);const op=n=>ops.find(x=>x.operationId===n);
  assert.equal(ops.length,68);assert.deepEqual(op('streamEvents').responses['200'].content['text/event-stream'].schema,{type:'string'});
  assert.deepEqual(op('getTurn').responses['200'].content['application/json'].schema,ref(profile==='public_v1'?'TurnView':'IntegrationTurnView'));
  assert.deepEqual(op('getMemory').responses['200'].content['application/json'].schema,{oneOf:[ref(profile==='public_v1'?'Memory':'PlayerMemoryMetadata'),ref('MemoryProvenanceV1')]});
  const header=op('getMemory').parameters.find(p=>p.name==='Loreviva-Client-Contract');assert.equal(header.schema.const,profile);assert.equal(header.required,profile!=='public_v1');
  assert.equal(op('getMemory').parameters.filter(p=>p.name==='include').length,1);
  const wire=op('streamEvents').responses['200'].content['text/event-stream'].example;
  const frame=Object.fromEntries(wire.trim().split('\n').map(line=>{const colon=line.indexOf(':');return [line.slice(0,colon),line.slice(colon+1).trimStart()];}));frame.data=JSON.parse(frame.data);
  assert.equal(validator(p)(profile==='public_v1'?'EventPage':'IntegrationSSEUpdate',profile==='public_v1'?frame.data:frame),true);
  function refs(x){if(!x||typeof x!=='object')return;if(x.$ref){assert.ok(x.$ref.startsWith('#/'));let value=p;for(const part of x.$ref.slice(2).split('/'))value=value?.[part.replaceAll('~1','/').replaceAll('~0','~')];assert.notEqual(value,undefined,x.$ref);}for(const v of Object.values(x))refs(v);}refs(p);
  const a=new Ajv2020({strict:false,allErrors:true});addFormats(a);const base='https://test.loreviva.invalid/'+profile;a.addSchema({...p,$id:base});
  for(const operation of ops)for(const response of Object.values(operation.responses))for(const media of Object.values(response.content??{})){
   const schema=JSON.parse(JSON.stringify(media.schema).replaceAll('"#/','"'+base+'#/'));const validate=a.compile(schema);
   for(const example of [media.example,...Object.values(media.examples??{}).map(x=>x.value)].filter(x=>x!==undefined))assert.equal(validate(example),true,operation.operationId+JSON.stringify(validate.errors));
  }
 }
 assert.deepEqual(o,before);
 const manifest=JSON.parse(await readFile(path.join(root,'contracts/generated/manifest.json'),'utf8'));
 for(const [p,h]of Object.entries(manifest.profile_hashes))assert.equal(createHash('sha256').update(await readFile(path.join(root,p))).digest('hex'),h);
});
test('generator rejects unknown profile keys, arbitrary projections, duplicate profiles and provenance conflicts',async t=>{
 const root=await temp(t);const {openapi:o,wepkg}=await generator.loadSources();assert.ok(o['x-integration-contract-profiles'],'closed profile metadata missing');
 const mutations=[m=>m.extra=true,m=>m.selector_header='Other',m=>m.profiles.push(structuredClone(m.profiles[0])),m=>m.profiles[1].response_projections[0].schema=ref('Error'),m=>m.profiles[1].response_projections[0].operation_id='getSnapshot',m=>m.profiles[1].response_projections[0].expression='any',m=>m.profiles[1].response_projections.pop()];
 await writeFile(path.join(root,'wepkg.schema.json'),JSON.stringify(wepkg));
 for(const mutate of mutations){const bad=structuredClone(o);mutate(bad['x-integration-contract-profiles']);await writeFile(path.join(root,'openapi.yaml'),JSON.stringify(bad));await assert.rejects(()=>generator.generateContracts({sourceRoot:root,outputRoot:path.join(root,'out')}),/profile|projection/i);}
 const bad=structuredClone(o);const mem=operations(bad).find(x=>x.operationId==='getMemory');mem['x-loreviva-response-projection'].projection_schema=ref('PlayerMemoryMetadata');await writeFile(path.join(root,'openapi.yaml'),JSON.stringify(bad));await assert.rejects(()=>generator.generateContracts({sourceRoot:root,outputRoot:path.join(root,'out')}),/projection/i);
});
test('profile operations retain old DTO declarations and type wire SSE separately from parsed frames',async t=>{
 const root=await temp(t);await generator.generateContracts({outputRoot:root});const file=path.join(root,'packages/sdk-ts/src/generated/contracts.ts');const code=await readFile(file,'utf8');assert.ok(code.includes('export interface ProfileOperations'),'typed profile operations missing');
 const fixture=path.join(root,'types.ts');await writeFile(fixture,`import type {ProfileOperations,ProfileResponses,ParsedSSEFrame,Operations,PlayerSelfConsentCommandRequest} from './packages/sdk-ts/src/generated/contracts';
const initial: PlayerSelfConsentCommandRequest={command:'INITIAL',expected_revision:'0',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'};
const renew: PlayerSelfConsentCommandRequest={command:'RENEW',expected_revision:'1',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'};
// @ts-expect-error INITIAL must retain its source const through the ExactUnsigned reference
const invalidInitial: PlayerSelfConsentCommandRequest={command:'INITIAL',expected_revision:'1',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'};
declare const frame: ParsedSSEFrame; const event: 'update'|'view_reset'=frame.event;
const wire: ProfileResponses['integration_r0_v1']['streamEvents']['200']='event: update\\n\\n';
// @ts-expect-error parsed frame is not the wire string
const badWire: ProfileResponses['integration_r0_v1']['streamEvents']['200']=frame;
const input: ProfileOperations['integration_r0_v1']['getMemory']['input']={path:{instance_id:'w',memory_id:'m'},headers:{'Loreviva-Client-Contract':'integration_r0_v1'}};
// @ts-expect-error selected profile cannot be silently downgraded
const bad: typeof input={...input,headers:{'Loreviva-Client-Contract':'public_v1'}};
declare const original: Operations['streamEvents']['responses']['200']; const oldWire:string=original;
`);
 const program=ts.createProgram([fixture],{strict:true,noEmit:true,target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext,moduleResolution:ts.ModuleResolutionKind.Bundler,skipLibCheck:true,types:[]});assert.deepEqual(ts.getPreEmitDiagnostics(program).map(x=>ts.flattenDiagnosticMessageText(x.messageText,'\n')),[]);
 const old=JSON.parse(await readFile(new URL('./fixtures/contract14-approved-baseline.json',import.meta.url),'utf8'));
 const priorRoot=await temp(t);await writeFile(path.join(priorRoot,'openapi.yaml'),JSON.stringify(old));const {wepkg}=await generator.loadSources();await writeFile(path.join(priorRoot,'wepkg.schema.json'),JSON.stringify(wepkg));await generator.generateContracts({sourceRoot:priorRoot,outputRoot:path.join(priorRoot,'out')});
 const prior=await readFile(path.join(priorRoot,'out/packages/sdk-ts/src/generated/contracts.ts'),'utf8');for(const line of prior.split('\n').filter(x=>x.startsWith('export type ')))assert.ok(code.split('\n').includes(line),line.slice(0,100));
});
test('standalone integration validators run with CSP code generation disabled and preserve conditional rejection',async t=>{
 const root=await temp(t);await generator.generateContracts({outputRoot:root});const file=path.join(root,'packages/sdk-ts/src/generated/validators.cjs');const code=await readFile(file,'utf8');const exported={};vm.runInNewContext(code,{exports:exported,require:createRequire(import.meta.url)},{codeGeneration:{strings:false,wasm:false},timeout:5000});
 assert.equal(typeof exported.schema_IntegrationSSEUpdate,'function','standalone typed SSE validator missing');
 const {openapi:o}=await generator.loadSources();for(const n of names)for(const sample of o.components.schemas[n].examples)assert.equal(exported['schema_'+n](sample),true,n);
 assert.equal(exported.schema_RuntimeMediaRef({media_id:'x',media_kind:'AUDIO',resource_revision:'1',media_state:'READY'}),false);
 assert.equal(exported.schema_PlayerSelfConsentCommandRequest({command:'INITIAL',expected_revision:'0',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D',subject_id:'other'}),false);
});

// Exercise the actual writer, including a corrupt last output; rejection must precede every filesystem mutation.
test('generator rejects late CR output before any mkdir or write',async t=>{
 const fs=await import('node:fs/promises');const {syncBuiltinESMExports}=await import('node:module');
 const base=await temp(t);const outputRoot=path.join(base,'must-remain-absent');
 const originalStringify=JSON.stringify,originalMkdir=fs.default.mkdir,originalWrite=fs.default.writeFile;
 let injected=0,mkdirCalls=0,writeCalls=0,failure;
 fs.default.mkdir=async(...args)=>{mkdirCalls++;return originalMkdir(...args);};
 fs.default.writeFile=async(...args)=>{writeCalls++;return originalWrite(...args);};syncBuiltinESMExports();
 JSON.stringify=function(value,...args){const body=originalStringify.call(this,value,...args);if(value&&value.source_hash&&value.profile_hashes&&value.generator_versions&&value.operation_ids&&value.limitations){injected++;return body+'\r';}return body;};
 try{await generator.generateContracts({outputRoot});}catch(error){failure=error;}
 finally{JSON.stringify=originalStringify;fs.default.mkdir=originalMkdir;fs.default.writeFile=originalWrite;syncBuiltinESMExports();}
 console.log(JSON.stringify({lateOutputInjected:injected,mkdirCalls,writeCalls,rejected:Boolean(failure)}));
 assert.equal(injected,1,'PRECONDITION: exact late manifest reached');
 assert.ok(failure,'late CR must be rejected by actual generator');assert.match(failure.message,/CR|LF/);
 assert.equal(mkdirCalls,0);assert.equal(writeCalls,0);await assert.rejects(fs.access(outputRoot),{code:'ENOENT'});
});
