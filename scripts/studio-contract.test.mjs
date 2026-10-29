import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import {loadSources} from './generate-contracts.mjs';
const names=('StudioSettingsScope StudioSettingsCommandKind StudioSettingsBasis StudioModuleDefault StudioDefaults StudioQuotaPolicyBinding StudioConnectionMetadata StudioLocalNotificationPreference StudioInstallationStorageMetadata StudioSettingsReferenceCatalog StudioGrant StudioMemberMetadata StudioDelegableGrant StudioSelfConsentState StudioSelfConsentSnapshot StudioSettingsSnapshot StudioMemberPage StudioSettingsCommandRequest StudioMemberCommandRequest StudioSelfConsentCommandRequest StudioCommandTarget StudioLocalCommandReceipt StudioLocalCommandError DraftCandidateArtifact RehearsalReportArtifact').split(' ').sort();
const ids=['getAdminSettings','commandAdminSettings','listAdminMembers','commandAdminMembers','getAdminSelfConsent','commandAdminSelfConsent','getAdminSettingsCommand'].sort();
test('studio_contract_exact_delta',async()=>{
 const old=JSON.parse(await readFile(new URL('./fixtures/contract13-approved-baseline.json',import.meta.url),'utf8'));
 const approved=JSON.parse(await readFile(new URL('../tests/contracts/fixtures/contract14-approved-baseline.json',import.meta.url),'utf8'));
 const {openapi:o}=await loadSources(); assert.equal(o.info.version,'1.7.0');
 for(const [p,v]of Object.entries(old.paths))assert.deepEqual(o.paths[p],v,p);
 for(const [n,v]of Object.entries(old.components.schemas))assert.deepEqual(o.components.schemas[n],v,n);
 assert.deepEqual(Object.keys(o.components.schemas).filter(n=>approved.components.schemas[n]&&!old.components.schemas[n]).sort(),names);
 const ops=Object.values(o.paths).flatMap(p=>Object.values(p).filter(v=>v.operationId)); assert.equal(ops.length,68); assert.equal(Object.keys(o.components.schemas).length,189);
 const added=ops.filter(op=>ids.includes(op.operationId)); assert.deepEqual(added.map(o=>o.operationId).sort(),ids);
 for(const op of added)assert.deepEqual(op.security,[{OperatorBearer:[]}]);
 // Only the adopted portable-body descriptor is additive. The remaining
 // exact canonical bytes must still match the original approved baseline.
 const packageRaw=await readFile(new URL('../contracts/source/wepkg.schema.json',import.meta.url));
 const legacyPackage=JSON.parse(packageRaw.toString('utf8'));
 assert.deepEqual(packageRaw,Buffer.from(JSON.stringify(legacyPackage,null,2)+'\n'));
 assert.ok(Object.hasOwn(legacyPackage,'x-loreviva-portable-body'));
 delete legacyPackage['x-loreviva-portable-body'];
 assert.equal(createHash('sha256').update(JSON.stringify(legacyPackage,null,2)+'\n').digest('hex'),'d12efa2de15d1ad4ae5ca9dc1fc093343451a059713326ceccca38e31a075d03');
});
async function validator(){const {openapi:o}=await loadSources();const a=new Ajv2020({strict:false,allErrors:true});addFormats(a);a.addSchema({...o,$id:'https://schemas.loreviva.invalid/studio',$schema:o.jsonSchemaDialect});return (n,v)=>a.validate({$ref:'https://schemas.loreviva.invalid/studio#/components/schemas/'+n},v);}
test('studio_contract_closed_branches',async()=>{
 const v=await validator(),tenant={scope_type:'TENANT',tenant_id:'demo-local'},product={scope_type:'PRODUCT',tenant_id:'demo-local',product_id:'coffee'};
 assert.equal(v('StudioSettingsScope',tenant),true);assert.equal(v('StudioSettingsScope',product),true);
 assert.equal(v('StudioSettingsScope',{...tenant,product_id:'fake'}),false);assert.equal(v('StudioSettingsScope',{scope_type:'PRODUCT',tenant_id:'demo-local'}),false);
 const req={scope:product,kind:'INITIAL',expected_revision:'0',policy_id:'R0_OWNER_PRIVATE_GENERATION_30D',accepted:true};
 assert.equal(v('StudioSelfConsentCommandRequest',req),true);for(const extra of [{duration_seconds:1},{caller:'owner'},{action:'anything'},{use:'PRIVATE_RESPONSE'},{scope:tenant},{accepted:false}])assert.equal(v('StudioSelfConsentCommandRequest',{...req,...extra}),false);
 const notices={revision:'1',channel:'LOCAL_DISPLAY',enabled:true,events:['TASK_STATUS','CONSENT_EXPIRY'],frequency:'ON_OPEN'};
 assert.equal(v('StudioLocalNotificationPreference',notices),true);assert.equal(v('StudioLocalNotificationPreference',{...notices,events:['TASK_STATUS','TASK_STATUS']}),false);
 const mock={connection_id:'mock-local',revision:'1',name:'Local Mock',provider_ref:'r0-mock-provider',model_id:'synthetic-text-v1',endpoint_alias_ref:null,secret_alias_ref:null,status:'UNVERIFIED'};
 assert.equal(v('StudioConnectionMetadata',mock),true);for(const extra of [{status:'ACTIVE'},{secret:'secret'},{endpoint_alias_ref:'http://remote'},{model_id:'other'}])assert.equal(v('StudioConnectionMetadata',{...mock,...extra}),false);
 assert.equal(v('StudioDefaults',{revision:'0',product_pack_ref:null,module_defaults:[]}),true);assert.equal(v('StudioDefaults',{revision:'0',product_pack_ref:'not-registered',module_defaults:[]}),false);
});
test('studio_artifacts_preserve_types_and_historical_assertions',async()=>{
 const v=await validator();assert.equal(v('DraftCandidateArtifact',{fields:{example_lines:['一','two'],description:'🦊'}}),true);
 for(const bad of [{fields:{}},{fields:{example_lines:'string'}},{fields:{object_id:'evil'}},{fields:{description:'ok'},command:'save'}])assert.equal(v('DraftCandidateArtifact',bad),false);
 const report={program:'R0_REHEARSAL_REPORT_V1',inputs:[{index:0,result_id:'result-one',state:'COMMITTED',supplier_reconciliation_state:'PENDING_VERIFICATION'}],assertions:[{kind:'RIGHTS',state:'FAIL'},{kind:'RIGHTS',state:'NOT_RUN'}]};
 assert.equal(v('RehearsalReportArtifact',report),true);assert.equal(v('RehearsalReportArtifact',{...report,inputs:Array(21).fill(report.inputs[0])}),false);assert.equal(v('RehearsalReportArtifact',{...report,assertions:Array(5).fill(report.assertions[0])}),false);assert.equal(v('RehearsalReportArtifact',{...report,dialogue:'hidden'}),false);
});
test('studio_collection_limits_and_registered_pairs',async()=>{
 const v=await validator(),p={scope_type:'PRODUCT',tenant_id:'demo-local',product_id:'coffee'},t={scope_type:'TENANT',tenant_id:'demo-local'};
 const setting={mode:'NONE',required_capabilities:[],allowed_external_uses:['NONE']};
 const defaults={revision:'0',product_pack_ref:null,module_defaults:['INTERACTION','PROACTIVE','AUTHORING','KNOWLEDGE_EXTRACTION','MEDIA','REHEARSAL','EVALUATION'].map(module_id=>({module_id,generation_settings:setting}))};
 assert.equal(v('StudioDefaults',defaults),true);assert.equal(v('StudioDefaults',{...defaults,module_defaults:[defaults.module_defaults[0],{...defaults.module_defaults[0],generation_settings:{...setting,required_capabilities:['TEXT']}}]}),false);
 const grant={scope:p,action:'asset.upload',use:'PRODUCT_ADMIN',effect:'ALLOW',enabled:true,expires_at:null,revision:'1'};assert.equal(v('StudioGrant',grant),true);
 for(const extra of [{scope:t},{action:'memory.generation.consent'},{action:'memory.generation.use'},{use:'PRIVATE_RESPONSE'},{use:'TENANT_SETTINGS'}])assert.equal(v('StudioGrant',{...grant,...extra}),false);
 const member={credential_id:'second',subject_id:'creator',home_product_id:'coffee',enabled:true,expires_at:null,credential_revision:'1',grant_set_revision:'1',grants:Array(128).fill(grant)};
 assert.equal(v('StudioMemberMetadata',member),true);assert.equal(v('StudioMemberMetadata',{...member,grants:Array(129).fill(grant)}),false);
 const catalog={providers:[],endpoint_aliases:[],secret_aliases:[],quota_policies:[],product_packs:[]},entry={ref_id:'safe-ref',label:'Registered',enabled:true,revision:'1'};
 assert.equal(v('StudioSettingsReferenceCatalog',{...catalog,providers:Array(128).fill(entry)}),true);assert.equal(v('StudioSettingsReferenceCatalog',{...catalog,providers:Array(129).fill(entry)}),false);
 const basis={scope:p,credential_id:'owner',server_now:'2026-10-12T00:00:00Z',allowed_commands:[]};
 assert.equal(v('StudioMemberPage',{basis,items:Array(100).fill(member),delegable_pairs:[]}),true);assert.equal(v('StudioMemberPage',{basis,items:Array(101).fill(member),delegable_pairs:[]}),false);
});
