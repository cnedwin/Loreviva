import test from 'node:test';
import assert from 'node:assert/strict';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import {loadSources} from './generate-contracts.mjs';
import validators from '../packages/sdk-ts/src/generated/validators.cjs';
test('browser contract validation needs no dynamic code under strict CSP',async()=>{
 const native=globalThis.Function;globalThis.Function=function(){throw Error('dynamic evaluation forbidden by Studio CSP')};
 try{const {validateContract}=await import('../packages/sdk-ts/src/index.ts');assert.doesNotThrow(()=>validateContract('GenerationSettings',{mode:'NONE',required_capabilities:[],allowed_external_uses:['NONE']}));assert.throws(()=>validateContract('GenerationSettings',{mode:'NONE'}))}finally{globalThis.Function=native}
});
test('standalone validators preserve all 189 source schemas and conditional closed-field semantics without mutation',async()=>{
 const {openapi}=await loadSources();const base='https://schemas.loreviva.invalid/parity';const ajv=new Ajv2020({strict:false,allErrors:true,validateFormats:true});addFormats(ajv);ajv.addSchema({...openapi,$id:base});
 assert.equal(Object.keys(validators).filter(k=>k.startsWith('schema_')).length,189);
 const values=[null,false,0,1,'','9007199254740993',[],{}, {mode:'NONE',required_capabilities:[],allowed_external_uses:['NONE']},{scope_type:'TENANT',tenant_id:'demo-local'},{scope_type:'TENANT',tenant_id:'demo-local',product_id:'coffee'},{scope_type:'PRODUCT',tenant_id:'demo-local',product_id:'coffee'},{fields:{description:'🦊',example_lines:['one','two']}},{fields:{description:'ok'},unknown:true}];
 let comparisons=0;
 for(const [name,schema]of Object.entries(openapi.components.schemas)){
  const dynamic=ajv.compile({$ref:base+'#/components/schemas/'+name});const compiled=validators['schema_'+name];
  const examples=schema.examples??[];for(const value of [...values,...examples,...examples.filter(v=>v&&typeof v==='object'&&!Array.isArray(v)).map(v=>({...v,unregistered_property:true}))]){const before=JSON.stringify(value);assert.equal(compiled(value),dynamic(value),name+' parity');assert.equal(JSON.stringify(value),before,name+' mutation');comparisons++}
 }
 assert(comparisons>=2184);
});
