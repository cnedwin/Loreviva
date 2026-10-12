import {readFile,writeFile,mkdir} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

export const GENERATOR_VERSIONS=Object.freeze({loreviva:'1.3.0',ajv:'8.17.1','ajv-formats':'3.0.1','go-jsonschema':'6.0.2'});
const repo=fileURLToPath(new URL('../',import.meta.url));
export const canonicalSourceRoot=path.resolve(repo,'contracts/source');
const methods=new Set(['get','put','post','delete','options','head','patch','trace']);
const json=x=>JSON.stringify(x,null,2)+'\n';
const sorted=x=>Object.fromEntries(Object.entries(x??{}).sort(([a],[b])=>a<b?-1:a>b?1:0));
const pointer=x=>x.replaceAll('~','~0').replaceAll('/','~1');

export async function loadSources(sourceRoot=canonicalSourceRoot) {
 const filenames=['openapi.yaml','wepkg.schema.json'];
 const bytes=await Promise.all(filenames.map(n=>readFile(path.join(sourceRoot,n))));
 const source_hash=createHash('sha256'); const source_hashes={};
 for(let i=0;i<filenames.length;i++){source_hash.update(filenames[i]+'\0').update(bytes[i]).update('\0');source_hashes[filenames[i]]=createHash('sha256').update(bytes[i]).digest('hex')}
 const [openapi,wepkg]=bytes.map(b=>JSON.parse(b.toString('utf8')));
 if(openapi.openapi!=='3.1.2'||openapi.jsonSchemaDialect!=='https://json-schema.org/draft/2020-12/schema'||wepkg.$schema!=='https://json-schema.org/draft/2020-12/schema') throw Error('unsupported canonical schema dialect');
 return {openapi,wepkg,source_hash:source_hash.digest('hex'),source_hashes};
}

function operations(openapi) {
 const found=[];const ids=new Set();
 for(const [route,item] of Object.entries(sorted(openapi.paths))) for(const [method,op] of Object.entries(sorted(item))) {
  if(!methods.has(method))continue;
  if(!op.operationId || !/^[A-Za-z][A-Za-z0-9_]*$/.test(op.operationId))throw Error('missing or invalid operation ID');
  if(ids.has(op.operationId))throw Error('duplicate operation ID: '+op.operationId);ids.add(op.operationId);
  const projection=op['x-loreviva-response-projection'];
  if(projection){
   if(!['getMemory','listMemories','getAdminMemoryBody'].includes(op.operationId)||JSON.stringify(Object.keys(projection).sort())!==JSON.stringify(['default_schema','parameter','projection_schema','status','value'])||projection.parameter!=='include'||projection.value!=='provenance_v1'||projection.status!=='200')throw Error('invalid closed response projection metadata');
   for(const schema of [projection.default_schema,projection.projection_schema])if(!schema||Object.keys(schema).length!==1||!schema.$ref?.startsWith('#/components/schemas/')||!openapi.components.schemas[refName(schema)])throw Error('invalid projection schema reference');
   if(JSON.stringify(op.responses['200']?.content?.['application/json']?.schema)!==JSON.stringify({oneOf:[projection.default_schema,projection.projection_schema]}))throw Error('projection metadata disagrees with wire schema');
   const parameter=op.parameters?.find(p=>p.in==='query'&&p.name==='include');if(!parameter||parameter.required!==false||parameter.schema?.type!=='string'||JSON.stringify(parameter.schema.enum)!==JSON.stringify(['provenance_v1']))throw Error('projection opt-in must remain explicit');
  }
  found.push({route,method,...op,parameters:[...(item.parameters??[]),...(op.parameters??[])]});
 }
 return found;
}
const refName=s=>s.$ref?.split('/').at(-1);
// Intersect the object refinements used by canonical if/then branches. Do not
// replace sibling shape with a constraint that only specifies required/limits.
function refine(base,constraint) {
 if(constraint===false||base===false)return false;
 if(!constraint||constraint===true)return structuredClone(base);
 const result={...structuredClone(base),...structuredClone(constraint)};
 result.required=[...new Set([...(base.required??[]),...(constraint.required??[])])];
 result.properties={...structuredClone(base.properties??{})};
 for(const [key,value]of Object.entries(constraint.properties??{}))result.properties[key]=refine(result.properties[key]??{},value);
 for(const key of ['minItems','minLength','minimum'])if(base[key]!==undefined&&constraint[key]!==undefined)result[key]=Math.max(base[key],constraint[key]);
 for(const key of ['maxItems','maxLength','maximum'])if(base[key]!==undefined&&constraint[key]!==undefined)result[key]=Math.min(base[key],constraint[key]);
 if(base.additionalProperties===false)result.additionalProperties=false;
 // not(required:[one property]) means that property's presence is forbidden.
 if(constraint.not&&Object.keys(constraint.not).length===1&&constraint.not.required?.length===1){
  const forbidden=constraint.not.required[0];if(result.required.includes(forbidden))return false;
  result.properties[forbidden]=false;delete result.not;
 }
 return result;
}
function discriminatedBranches(s) {
 const conditional=(s.allOf??[]).filter(c=>c.if);
 if(!conditional.length||!s.properties)return null;
 const keys=conditional.map(c=>Object.keys(c.if.properties??{}));
 if(keys.some(k=>k.length!==1)||keys.some(k=>k[0]!==keys[0][0]))return null;
 const key=keys[0][0];const values=s.properties[key]?.enum;
 if(!values||!s.required?.includes(key))return null;
 const predicates=conditional.map(c=>c.if.properties[key]);
 // Only translate predicates whose finite discriminant truth is exact.
 if(conditional.some(c=>Object.keys(c.if).some(k=>!['properties','required'].includes(k))||(c.if.required??[]).some(k=>!s.required.includes(k)))||
    predicates.some(p=>Object.keys(p).some(k=>!['const','enum'].includes(k))||(!('const'in p)&&!p.enum)))return null;
 const base=structuredClone(s);base.allOf=(s.allOf??[]).filter(c=>!c.if);if(!base.allOf.length)delete base.allOf;
 return values.map(value=>{
  let branch=refine(base,{properties:{[key]:{const:value}}});
  for(const c of conditional){const p=c.if.properties[key];const matches='const'in p?value===p.const:p.enum.includes(value);branch=refine(branch,matches?c.then:c.else)}
  return branch;
 });
}
function tsType(s) {
 if(s===true||s===undefined)return 'unknown'; if(s===false)return 'never';
 if(s.$ref)return refName(s);
 if(s.const!==undefined)return JSON.stringify(s.const);
 if(s.enum)return s.enum.map(x=>JSON.stringify(x)).join(' | ');
 if(s.oneOf||s.anyOf)return '('+(s.oneOf??s.anyOf).map(tsType).join(' | ')+')';
 if(s.allOf){
  const branches=discriminatedBranches(s);if(branches)return '('+branches.map(tsType).join(' | ')+')';
  const {allOf,...base}=s;const parts=[tsType(base),...allOf.map(tsType)].filter(t=>t!=='unknown');return parts.length?'('+parts.join(' & ')+')':'unknown';
 }
 if(Array.isArray(s.type))return s.type.map(type=>tsType({...s,type})).join(' | ');
 if(s.type==='array'){
  const item=tsType(s.items);const min=s.minItems??0,max=s.maxItems;
  if(Number.isInteger(max)&&max<=16&&max>=min)return '('+Array.from({length:max-min+1},(_,i)=>'['+Array(min+i).fill(item).join(', ')+']').join(' | ')+')';
  if(min>0&&min<=16)return '['+Array(min).fill(item).join(', ')+', ...Array<'+item+'>]';
  return 'Array<'+item+'>';
 }
 if(s.type==='object'||s.properties){
  const required=new Set(s.required??[]);
  const props=Object.entries(sorted(s.properties)).map(([k,v])=>JSON.stringify(k)+(required.has(k)?'':'?')+': '+tsType(v)+';');
  if(s.additionalProperties!==false) props.push('[key: string]: '+(s.additionalProperties&&typeof s.additionalProperties==='object'?tsType(s.additionalProperties):'unknown')+';');
  return '{ '+props.join(' ')+' }';
 }
 return {string:'string',integer:'number',number:'number',boolean:'boolean',null:'null'}[s.type]??'unknown';
}
const acronyms=new Set(['id','ids','url','uri','api','json','sse','sku','ttl']);
const goName=x=>x.split(/[_\-]/).map(w=>acronyms.has(w.toLowerCase())?w.toUpperCase():w[0].toUpperCase()+w.slice(1)).join('');
function goType(s) {
 if(s?.$ref)return refName(s);
 if(!s||s===true||s===false||s.oneOf||s.anyOf||s.allOf||Array.isArray(s.type))return 'json.RawMessage';
 if(s.const!==undefined)return typeof s.const==='string'?'string':typeof s.const==='boolean'?'bool':'json.RawMessage';
 if(s.type==='array')return '[]'+goType(s.items);
 if(s.type==='object'||s.properties){
  if(!s.properties)return s.additionalProperties&&typeof s.additionalProperties==='object'?'map[string]'+goType(s.additionalProperties):'map[string]json.RawMessage';
  // Arbitrary additional properties cannot be represented faithfully by a struct.
  if(s.additionalProperties!==false)return 'json.RawMessage';
  const required=new Set(s.required??[]);
  const fields=Object.entries(sorted(s.properties)).map(([k,v])=>{
   const optional=!required.has(k); const type=goType(v);return '\t'+goName(k)+' '+(optional?'*':'')+type+' `json:"'+k+(optional?',omitempty':'')+'"`';
  });
  return 'struct {\n'+fields.join('\n')+'\n}';
 }
 return {string:'string',integer:'int64',number:'float64',boolean:'bool'}[s.type]??'json.RawMessage';
}
function resolve(openapi,x){if(!x?.$ref)return x;return x.$ref.slice(2).split('/').reduce((a,k)=>a[k],openapi)}
function tsOperation(openapi,op) {
 const groups={};
 for(const raw of op.parameters) {const p=resolve(openapi,raw);const group=p.in==='header'?'headers':p.in; (groups[group]??=[]).push(p)}
 const fields=Object.entries(groups).map(([group,params])=>group+(params.some(p=>p.required)?'':'?')+': { '+params.map(p=>JSON.stringify(p.name)+(p.required?'':'?')+': '+tsType(p.schema)+';').join(' ')+' };');
 const body=resolve(openapi,op.requestBody);if(body){const media=body.content['application/json']??Object.values(body.content)[0];fields.push('body'+(body.required?'':'?')+': '+tsType(media?.schema)+';')}
 const responses=Object.entries(sorted(op.responses)).map(([status,raw])=>{const response=resolve(openapi,raw);const media=response.content?.['application/json']??response.content?.['text/event-stream']??Object.values(response.content??{})[0];const projection=op['x-loreviva-response-projection'];const schema=projection?.status===status?projection.default_schema:media?.schema;return JSON.stringify(status)+': '+(schema?tsType(schema):'undefined')+';'});
 return JSON.stringify(op.operationId)+': { input: { '+fields.join(' ')+' }; responses: { '+responses.join(' ')+' } };';
}

export async function validateExamples(sourceRoot=canonicalSourceRoot) {
 const {openapi,wepkg}=await loadSources(sourceRoot);
 const ajv=new Ajv2020({strict:false,allErrors:true,validateFormats:true});addFormats(ajv);
 const id='https://schemas.loreviva.invalid/generated/openapi';
 ajv.addSchema({$id:id,$schema:openapi.jsonSchemaDialect,...openapi});ajv.addSchema(wepkg);
 let count=0; const errors=[];
 const check=(schema,value,where)=>{count++;const validate=ajv.compile(schema);if(!validate(value))errors.push({where,errors:validate.errors})};
 function schemaExamples(node,base,loc=''){
  if(!node||typeof node!=='object')return;
  if(Array.isArray(node.examples))for(const [i,value]of node.examples.entries())check({$ref:base+'#'+loc},value,loc+'/examples/'+i);
  for(const [k,v]of Object.entries(node))if(k!=='examples')schemaExamples(v,base,loc+'/'+pointer(k));
 }
 schemaExamples(openapi.components.schemas,id,'/components/schemas');schemaExamples(wepkg,wepkg.$id);
 for(const op of operations(openapi)) {
  const containers=[resolve(openapi,op.requestBody),...Object.values(op.responses).map(x=>resolve(openapi,x))].filter(Boolean);
  for(const container of containers)for(const [mediaType,media]of Object.entries(container.content??{})) {
   const schema={...media.schema};
   // Anchor component references in the canonical OpenAPI document.
   function anchor(node){if(!node||typeof node!=='object')return;if(node.$ref?.startsWith('#'))node.$ref=id+node.$ref;for(const v of Object.values(node))anchor(v)} anchor(schema);
   for(const [name,example]of Object.entries(media.examples??{}))if('value'in example)check(schema,example.value,op.operationId+'/'+mediaType+'/'+name);
   if('example'in media)check(schema,media.example,op.operationId+'/'+mediaType);
  }
 }
 return {count,errors};
}

export async function generateContracts({sourceRoot=canonicalSourceRoot,outputRoot=repo}={}) {
 const {openapi,wepkg,source_hash,source_hashes}=await loadSources(sourceRoot);const ops=operations(openapi);
 const header='Code generated by Loreviva contracts '+GENERATOR_VERSIONS.loreviva+'; DO NOT EDIT. Source SHA-256: '+source_hash;
 const schemas=Object.entries(sorted(openapi.components.schemas));
 const ts='// '+header+'\nexport const sourceHash = '+JSON.stringify(source_hash)+' as const;\n'+schemas.map(([name,s])=>'export type '+name+' = '+tsType(s)+';').join('\n')+'\n\nexport interface Operations {\n'+ops.map(op=>'  '+tsOperation(openapi,op)).join('\n')+'\n}\n\nexport interface ProjectionResponses {\n'+ops.filter(op=>op['x-loreviva-response-projection']).map(op=>{const p=op['x-loreviva-response-projection'];return '  '+JSON.stringify(op.operationId)+': { '+JSON.stringify(p.value)+': '+tsType(p.projection_schema)+'; };'}).join('\n')+'\n}\n\nexport const operationRoutes = '+JSON.stringify(Object.fromEntries(ops.map(op=>[op.operationId,{method:op.method.toUpperCase(),path:op.route}])),null,2)+' as const;\n';
 const goTypes=schemas.map(([name,s])=>{const type=goType(s);return 'type '+name+(type==='json.RawMessage'?' = ':' ')+type}).join('\n\n');
 const go='// '+header+'\npackage generated\n\n'+(goTypes.includes('json.RawMessage')?'import "encoding/json"\n\n':'')+'const SourceHash = '+JSON.stringify(source_hash)+'\nconst GeneratorVersion = '+JSON.stringify(GENERATOR_VERSIONS.loreviva)+'\n\n'+goTypes+'\n';
 const validator=`// ${header}
package generated

import (
 _ "embed"
 "encoding/json"
 "errors"
 "fmt"
 "strings"
 "sync"
 "loreviva.local/loreviva/internal/wire"
 "github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas.json
var schemaDocument []byte
var once sync.Once
var compiled map[string]*jsonschema.Schema
var compileError error

// Validate validates original wire bytes before decoding DTOs. Do not validate
// a remarshal: that would discard unknown fields or coerce required zero values.
func Validate(name string, data []byte) error {
 once.Do(func(){
  var document map[string]any
  if err:=json.Unmarshal(schemaDocument,&document);err!=nil {compileError=err;return}
  compiler:=jsonschema.NewCompiler()
  compiler.AssertFormat()
  const resource="https://schemas.loreviva.invalid/generated/openapi"
  if err:=compiler.AddResource(resource,document);err!=nil {compileError=err;return}
  compiled=map[string]*jsonschema.Schema{}
  schemas:=document["components"].(map[string]any)["schemas"].(map[string]any)
  for key:=range schemas {schema,err:=compiler.Compile(resource+"#/components/schemas/"+strings.ReplaceAll(strings.ReplaceAll(key,"~","~0"),"/","~1"));if err!=nil {compileError=err;return};compiled[key]=schema}
 })
 if compileError!=nil {return errors.New("contract schema initialization failed")}
 schema,ok:=compiled[name];if !ok {return errors.New("unknown contract schema")}
 value,err:=wire.Parse(data)
 if err!=nil {return errors.New("invalid contract JSON")}
 if err:=schema.Validate(value);err!=nil {return fmt.Errorf("contract %s failed validation",name)}
 return nil
}

// Decode enforces full Schema validation including raw unions before decoding.
func Decode(name string,data []byte,target any) error {
 if err:=Validate(name,data);err!=nil {return err};return json.Unmarshal(data,target)
}
`;
 const files={
  'contracts/generated/openapi.json':json(openapi),
  'contracts/generated/wepkg.schema.json':json(wepkg),
  'internal/generated/types.go':go,
  'internal/generated/schemas.json':json({$schema:openapi.jsonSchemaDialect,components:openapi.components}),
  'internal/generated/validate.go':validator,
  'packages/sdk-ts/src/generated/contracts.ts':ts,
 };
 const manifest={source_hash,source_hashes,generator_versions:GENERATOR_VERSIONS,operation_ids:ops.map(op=>op.operationId),limitations:['Go top-level raw JSON DTOs alias json.RawMessage to preserve JSON methods; validate original wire JSON via generated.Decode before domain use.','TypeScript retains allOf sibling shapes and translates required finite enum discriminants with const/enum if predicates, then/else field requirements, single-field not(required), and small array bounds. Regex, formats, string/number ranges, uniqueness, propertyNames, array upper bounds above 16, arbitrary if predicates and general not/dependent constraints still require full JSON Schema validation.','OpenAPI source must use JSON syntax (valid YAML); no alternate authority or runtime timestamps.']};
 files['contracts/generated/manifest.json']=json(manifest);
 const outputs=Object.keys(files).sort();
 for(const output of outputs){const target=path.join(outputRoot,output);await mkdir(path.dirname(target),{recursive:true});await writeFile(target,files[output],'utf8')}
 return {source_hash,generator_versions:GENERATOR_VERSIONS,outputs};
}

if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url)){const result=await generateContracts();console.log(json(result))}
