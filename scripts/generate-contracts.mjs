import {readFile,writeFile,mkdir} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import standaloneCode from 'ajv/dist/standalone/index.js';
import {resolveIntegrationProfiles} from './integration-profiles.mjs';

export const GENERATOR_VERSIONS=Object.freeze({loreviva:'1.6.0',ajv:'8.18.0','ajv-formats':'3.0.1','go-jsonschema':'6.0.2'});
const repo=fileURLToPath(new URL('../',import.meta.url));
export const canonicalSourceRoot=path.resolve(repo,'contracts/source');
const methods=new Set(['get','put','post','delete','options','head','patch','trace']);
const json=x=>JSON.stringify(x,null,2)+'\n';
const sorted=x=>Object.fromEntries(Object.entries(x??{}).sort(([a],[b])=>a<b?-1:a>b?1:0));
const pointer=x=>x.replaceAll('~','~0').replaceAll('/','~1');

// Bind generated output to the exact named source bytes, including file boundaries; generated DTOs and validators remain derived views, not an alternate contract authority.
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
 if(s.$ref){
  // JSON Schema siblings constrain the reference; preserve finite scalar consts.
  const name=refName(s),literal=s.const;
  return literal===null||['string','number','boolean'].includes(typeof literal)?'('+name+' & '+JSON.stringify(literal)+')':name;
 }
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

function browserValidators(openapi,ops,profiles,header) {
 const base='https://schemas.loreviva.invalid/generated/browser';
 const ajv=new Ajv2020({strict:false,allErrors:true,validateFormats:true,inlineRefs:false,code:{source:true}});addFormats(ajv);ajv.addSchema({...openapi,$id:base});
 const exports={};
 function anchor(value){if(Array.isArray(value))return value.map(anchor);if(value&&typeof value==='object')return Object.fromEntries(Object.entries(value).map(([key,v])=>[key,key==='$ref'&&typeof v==='string'&&v.startsWith('#')?base+v:anchor(v)]));return value}
 function add(name,schema){const id=base+'/'+name;ajv.addSchema({...anchor(schema),$id:id});exports[name]=id}
 for(const name of Object.keys(openapi.components.schemas).sort())add('schema_'+name,{$ref:base+'#/components/schemas/'+name});
 for(const [prefix,doc,list] of [['',openapi,ops],...Object.entries(profiles).map(([p,d])=>['profile_'+p+'_',d,operations(d)])])for(const op of list){
  const input={type:'object',properties:{},required:[],additionalProperties:false};
  for(const raw of op.parameters){const p=resolve(openapi,raw);const group=p.in==='header'?'headers':p.in;const shape=input.properties[group]??={type:'object',properties:{},required:[],additionalProperties:false};shape.properties[p.name]=p.schema;if(p.required){shape.required.push(p.name);if(!input.required.includes(group))input.required.push(group)}}
  const body=resolve(openapi,op.requestBody);if(body){const media=body.content['application/json']??Object.values(body.content)[0];input.properties.body=media.schema;if(body.required)input.required.push('body')}
  add(prefix+'input_'+op.operationId,input);
  for(const [status,raw]of Object.entries(op.responses)){
   const response=resolve(doc,raw);const projection=op['x-loreviva-response-projection'];
   const schema=prefix&&projection?.status===status?projection.default_schema:response.content?.['application/json']?.schema;
   if(schema)add(prefix+'response_'+op.operationId+'_'+status,schema);
   if(prefix&&projection?.status===status)add(prefix+'provenance_'+op.operationId+'_'+status,projection.projection_schema);
   if(response['x-sse-data-schema'])add(prefix+'sse_data_'+op.operationId,response['x-sse-data-schema']);
   if(response['x-sse-frame-schema'])add(prefix+'sse_frame_'+op.operationId,response['x-sse-frame-schema']);
  }
 }
 const code='// '+header+'\n'+standaloneCode(ajv,exports)+'\n';
 const types='// '+header+'\ninterface ValidationError {instancePath:string;keyword:string;message?:string}\ninterface Validator {(value:unknown):boolean;errors?:ValidationError[]|null}\ndeclare const validators: {\n'+Object.keys(exports).map(name=>'  '+JSON.stringify(name)+': Validator;').join('\n')+'\n};\nexport = validators;\n';
 return {code,types};
}
// Runtime routing and authority roles are derived from the same source as DTOs.
function transportDescriptors(openapi,profiles,header){
 const roleNames={ClientSessionBearer:'CLIENT',ServiceBearer:'SERVICE',OperatorBearer:'OPERATOR'};
 const out={};
 for(const [profile,doc]of Object.entries(profiles)){
  out[profile]={};
  for(const op of operations(doc)){
   const roles=[...new Set((op.security??doc.security??[]).flatMap(s=>{
    const names=Object.keys(s);if(names.length!==1||!roleNames[names[0]])throw Error('unsupported source security alternative');return roleNames[names[0]];
   }))];
   if(!roles.length)throw Error('source operation has no explicit credential role');
   const parameters=op.parameters.map(p=>resolve(doc,p));
   const body=resolve(doc,op.requestBody);const requestMime=body?Object.keys(body.content)[0]:null;
   const responses=Object.fromEntries(Object.entries(op.responses).map(([status,raw])=>{
    const response=resolve(doc,raw);const contents=Object.keys(response.content??{});
    if(contents.length>1)throw Error('ambiguous response content protocol');
    const mime=contents[0]??null;
    const protocol=mime===null?'EMPTY':mime==='application/json'?'JSON':mime==='text/event-stream'?'SSE':'BINARY';
    return [status,{mime,protocol,validator:protocol==='JSON'?'profile_'+profile+'_response_'+op.operationId+'_'+status:null,sseData:response['x-sse-data-schema']?'profile_'+profile+'_sse_data_'+op.operationId:null,sseFrame:response['x-sse-frame-schema']?'profile_'+profile+'_sse_frame_'+op.operationId:null}];
   }));
   out[profile][op.operationId]={method:op.method.toUpperCase(),path:op.route,roles,parameters:parameters.map(p=>({name:p.name,in:p.in,required:p.required===true,schema:p.schema})),requestMime,bodyRequired:body?.required===true,inputValidator:'profile_'+profile+'_input_'+op.operationId,responses,projection:op['x-loreviva-response-projection']??null};
  }
 }
 // A profile may legitimately omit an operation, and a legacy source may declare
 // no profile at all; roles are per operation, so any profile carrying it wins.
 const roles=Object.fromEntries(operations(openapi).map(op=>{const entry=Object.values(out).map(p=>p[op.operationId]).find(Boolean);return [op.operationId,entry?entry.roles:[]]}));
 const protocols=Object.fromEntries(Object.entries(out).map(([p,ops])=>[p,Object.fromEntries(Object.entries(ops).map(([id,op])=>[id,Object.fromEntries(Object.entries(op.responses).map(([status,r])=>[status,r.protocol]))]))]));
 const multipart=operations(openapi).filter(op=>resolve(openapi,op.requestBody)?.content?.['multipart/form-data']);
 const overlay=multipart.map(op=>{
  const schema=resolve(openapi,op.requestBody).content['multipart/form-data'].schema;
  const fields=Object.entries(schema.properties??{}).map(([k,v])=>JSON.stringify(k)+(schema.required?.includes(k)?'':'?')+': '+(v.type==='string'&&v.format==='binary'?'Blob':tsType(v))+';').join(' ');
  return JSON.stringify(op.operationId)+': { '+fields+' };';
 }).join('\n');
 return '// '+header+'\nimport type * as Contracts from "./contracts.ts";\n'+
 'export const transportDescriptors = '+JSON.stringify(out,null,2)+' as const;\n'+
 'export const operationRoles = '+JSON.stringify(roles,null,2)+' as const;\n'+
 'export const responseProtocols = '+JSON.stringify(protocols,null,2)+' as const;\n'+
 'export type OperationRoles = { [O in keyof typeof operationRoles]: (typeof operationRoles)[O][number] };\n'+
 'export type ResponseProtocols = typeof responseProtocols;\n'+
 'export interface MultipartBodies { '+overlay.replace(/(?<![\w.])AssetUploadMetadata/g,'Contracts.AssetUploadMetadata')+' }\n';
}
// Validate the entire prepared output set before the first mkdir/write; never repair CR while hashing or writing.
export function assertGeneratedLF(files){
 for(const [name,body] of Object.entries(files))if(typeof body!=='string'||body.includes('\r'))throw Error('Generated output must be LF without CR: '+name);
}
function manifestBackendValidator(header) {
 return `// ${header}
package generated

import (
 _ "embed"
 "encoding/json"
 "errors"
 "sync"
 "time"
 "github.com/dlclark/regexp2"
 "github.com/santhosh-tekuri/jsonschema/v6"
 "loreviva.local/loreviva/internal/wire"
)

//go:embed wepkg.schema.json
var manifestSchemaBytes []byte
var manifestOnce sync.Once
var manifestSchema *jsonschema.Schema
var manifestCompileError error

type manifestRegexp struct { expression *regexp2.Regexp }
func (r *manifestRegexp) MatchString(s string) bool {
 matched,err:=r.expression.MatchString(s)
 return err==nil && matched
}
func (r *manifestRegexp) String() string { return r.expression.String() }

// CompileManifestPattern preserves source ECMAScript semantics, including
// lookaheads, with finite work. A timeout or engine error never grants a match.
func CompileManifestPattern(pattern string) (jsonschema.Regexp,error) {
 expression,err:=regexp2.Compile(pattern,regexp2.ECMAScript)
 if err!=nil { return nil,errors.New("invalid manifest pattern") }
 expression.MatchTimeout=100*time.Millisecond
 return &manifestRegexp{expression:expression},nil
}

type manifestNoLoader struct{}
func (manifestNoLoader) Load(string) (any,error) { return nil,errors.New("external schema loading disabled") }

// ValidateManifest validates original bytes against the sole generated schema;
// it never reads runtime schema files or fetches remote resources.
func ValidateManifest(data []byte,maxBytes int64) error {
 value,err:=wire.ParseBounded(data,maxBytes)
 if err!=nil { return wire.ErrJSON }
 manifestOnce.Do(func(){
  var document any
  if err:=json.Unmarshal(manifestSchemaBytes,&document);err!=nil {manifestCompileError=err;return}
  compiler:=jsonschema.NewCompiler()
  compiler.AssertFormat()
  compiler.UseRegexpEngine(CompileManifestPattern)
  compiler.UseLoader(manifestNoLoader{})
  const resource="https://schemas.loreviva.invalid/generated/wepkg"
  if err:=compiler.AddResource(resource,document);err!=nil {manifestCompileError=err;return}
  manifestSchema,manifestCompileError=compiler.Compile(resource)
 })
 if manifestCompileError!=nil || manifestSchema==nil {return errors.New("manifest schema initialization failed")}
 return manifestSchema.Validate(value)
}
`;
}

// Project trusted canonical schemas through a closed, source-owned descriptor.
// Instance data and schema examples are never interpreted as projection commands.
export function buildPortableBodySchema(openapi,wepkg) {
 const spec=wepkg['x-loreviva-portable-body'];
 const keys=['body_version','body_bytes','plan_bytes','root_schema','root_kinds','object_components','operations','reference_targets','assembly_edge_limit','assembly_vertex_limit','diagnostic_limit'];
 const exact=(value,allowed)=>value&&typeof value==='object'&&!Array.isArray(value)&&Object.keys(value).every(k=>allowed.includes(k));
 if(!exact(spec,keys)||keys.some(k=>!(k in spec))||spec.body_version!=='1.0'||spec.body_bytes!==1048576||spec.plan_bytes!==8388608||spec.root_schema!=='DraftInput'||spec.assembly_edge_limit!==10000||spec.assembly_vertex_limit!==64||spec.diagnostic_limit!==32)throw Error('Invalid portable projection descriptor');
 const roots=['WORLD','CHARACTER','ROLE','SKILL','KNOWLEDGE','PRESENTATION_THEME'];
 if(JSON.stringify(spec.root_kinds)!==JSON.stringify(roots))throw Error('Unsupported portable root set');
 const source=openapi.components.schemas,components=source.ContentObject.oneOf.map(x=>x.$ref.split('/').at(-1));
 if(JSON.stringify(components)!==JSON.stringify(spec.object_components))throw Error('Portable content component set changed');
 const mapSchema=(value,visit)=>{
  if(typeof value==='boolean')return value;
  if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Schema object required');
  const out=structuredClone(value);visit(out);
  for(const key of ['properties','patternProperties','$defs','definitions','dependentSchemas'])if(out[key])for(const name of Object.keys(out[key]))out[key][name]=mapSchema(out[key][name],visit);
  for(const key of ['items','additionalProperties','unevaluatedProperties','contains','not','if','then','else','propertyNames','additionalItems','unevaluatedItems','contentSchema'])if(out[key]!==undefined)out[key]=Array.isArray(out[key])?out[key].map(x=>mapSchema(x,visit)):mapSchema(out[key],visit);
  for(const key of ['allOf','anyOf','oneOf','prefixItems'])if(out[key])out[key]=out[key].map(x=>mapSchema(x,visit));
  return out;
 };
 const needed=new Set();
 const collect=node=>mapSchema(node,n=>{
  if(n.$ref){if(!n.$ref.startsWith('#/components/schemas/'))throw Error('External source schema reference refused');const name=n.$ref.slice(21);if(!Object.hasOwn(source,name))throw Error('Missing source schema');if(!needed.has(name)){needed.add(name);collect(source[name]);}}
 });
 needed.add('DraftInput');collect(source.DraftInput);
 const rewrite=node=>mapSchema(node,n=>{delete n.example;delete n.examples;if(n.$ref?.startsWith('#/components/schemas/'))n.$ref='#/$defs/'+n.$ref.slice(21);else if(n.$ref&&!n.$ref.startsWith('#/$defs/'))throw Error('External projected reference refused');});
 const document={$schema:wepkg.$schema,$id:'https://schemas.loreviva.invalid/generated/portable',$ref:'#/$defs/DraftInput',$defs:{}};
 for(const name of [...needed].sort())document.$defs[name]=rewrite(source[name]);
 const get=(root,p)=>{
  if(typeof p!=='string'||!p.startsWith('/'))throw Error('Local schema pointer required');
  let at=root;for(const raw of p.slice(1).split('/')){const k=raw.replaceAll('~1','/').replaceAll('~0','~');if(['__proto__','prototype','constructor'].includes(k)||!at||!Object.hasOwn(at,k))throw Error('Missing or unsafe schema pointer');at=at[k];}return at;
 };
 const schemaFor=op=>{
  const literal=Object.hasOwn(op,'schema'),hasSource=Object.hasOwn(op,'source')||Object.hasOwn(op,'from_pointer')||(op.op==='clone'&&Object.hasOwn(op,'pointer'));
  if(literal&&hasSource)throw Error('Ambiguous schema origin');
  if(literal)return rewrite(op.schema);
  if(!Object.hasOwn(op,op.op==='clone'?'pointer':'from_pointer'))throw Error('Missing schema origin pointer');
  const origin=op.source==='openapi'?openapi:op.source==='wepkg'?wepkg:null;if(!origin)throw Error('Unknown schema source');return rewrite(get(origin,op.from_pointer??op.pointer));
 };
 for(const op of spec.operations){
  if(!op||typeof op!=='object')throw Error('Invalid projection operation');
  if(op.op==='clone'){
   if(!exact(op,['op','name','source','pointer','schema'])||!(/^[A-Za-z][A-Za-z0-9_]*$/.test(op.name))||['__proto__','prototype','constructor'].includes(op.name)||Object.hasOwn(document.$defs,op.name))throw Error('Invalid schema clone');
   document.$defs[op.name]=schemaFor(op);
  }else if(op.op==='replace-schema'){
   if(!exact(op,['op','pointer','schema','source','from_pointer'])||!op.pointer?.startsWith('/$defs/'))throw Error('Invalid schema replacement');
   get(document,op.pointer);const bits=op.pointer.split('/');const name=bits.pop().replaceAll('~1','/').replaceAll('~0','~');const parent=get(document,bits.join('/'));parent[name]=schemaFor(op);
  }else if(['remove-property','replace-property','add-property'].includes(op.op)){
   const allowed=op.op==='remove-property'?['op','component','property']:op.op==='replace-property'?['op','component','property','schema']:['op','component','property','schema','required'];
   if(!exact(op,allowed)||!(/^[a-z][a-z0-9_]*$/.test(op.property))||['__proto__','prototype','constructor'].includes(op.property)||!Object.hasOwn(document.$defs,op.component)||(op.op!=='remove-property'&&!Object.hasOwn(op,'schema')))throw Error('Invalid property projection');
   const node=document.$defs[op.component],present=Object.hasOwn(node.properties??{},op.property);
   if(!node.properties||!Array.isArray(node.required)||(op.op==='add-property'?present:!present))throw Error('Source property/required set changed');
   if(op.op==='remove-property'){delete node.properties[op.property];node.required=node.required.filter(x=>x!==op.property);}
   else {node.properties[op.property]=schemaFor(op);if(op.op==='add-property'){if(typeof op.required!=='boolean')throw Error('Explicit required policy missing');if(op.required)node.required.push(op.property);}}
  }else throw Error('Unknown portable projection operation');
 }
 mapSchema(document,n=>{if(n.$ref){if(!n.$ref.startsWith('#/$defs/'))throw Error('Nonlocal portable reference');get(document,n.$ref.slice(1));}});
 const references=[];const fields=new Set(),arrays=new Set(),usedTargets=new Set();
 mapSchema(document,n=>{for(const [name,value]of Object.entries(n.properties??{})){fields.add(name);if(value.type==='array'||value.items)arrays.add(name);}});
 const knownKinds=new Set(components.map(name=>source[name].properties.kind.const??source[name].properties.kind.enum?.[0]));
 const generalTargets=new Set(['origin_ref','dependency_refs','binding_refs']);
 if(!spec.reference_targets||typeof spec.reference_targets!=='object'||Array.isArray(spec.reference_targets))throw Error('Target policy object required');
 const scan=(node,component,path=[],field='',stack=[])=>{
  if(typeof node==='boolean')return;
  if(node.$ref){const target=node.$ref.slice(8);
   if(['DefinitionRef','PortableRef'].includes(target)){
    if(!Object.hasOwn(spec.reference_targets,field))throw Error('Missing reference target policy');
    const policy=spec.reference_targets[field];
    if(!Array.isArray(policy)||new Set(policy).size!==policy.length||policy.some(kind=>typeof kind!=='string'||!knownKinds.has(kind))||(generalTargets.has(field)?policy.length!==0:policy.length===0))throw Error('Invalid reference target policy');
    usedTargets.add(field);
    const sourceKind=source[component].properties?.kind;const objectKind=sourceKind?.const??sourceKind?.enum?.[0]??'';
    references.push({component,object_kind:objectKind,owner_domain:component==='ModuleAgentBinding'?'MODULE_BINDING':component==='SkillBinding'?'SKILL_BINDING':'OBJECT',path,expected_kinds:[...spec.reference_targets[field]].sort()});return;
   }
   if(stack.includes(target))throw Error('Recursive portable source shape');scan(get(document,node.$ref.slice(1)),component,path,field,[...stack,target]);return;
  }
  for(const [name,value]of Object.entries(node.properties??{})){fields.add(name);if(value.type==='array'||value.items)arrays.add(name);scan(value,component,[...path,name],name,stack);}
  if(node.items)scan(node.items,component,[...path,'*'],field,stack);
  for(const key of ['oneOf','anyOf','allOf'])for(const branch of node[key]??[])scan(branch,component,path,field,stack);
  for(const key of ['if','then','else','not'])if(node[key])scan(node[key],component,path,field,stack);
 };
 for(const name of [...components,'ModuleAgentBinding','SkillBinding'])scan(document.$defs[name],name);
 if(JSON.stringify(Object.keys(spec.reference_targets).sort())!==JSON.stringify([...usedTargets].sort()))throw Error('Unused or missing reference target policy');
 const unique=new Map(references.map(x=>[JSON.stringify([x.component,x.path]),x]));
 const refPaths=[...unique.values()].sort((a,b)=>JSON.stringify([a.component,a.path])<JSON.stringify([b.component,b.path])?-1:1);
 if(refPaths.length!==45)throw Error('Portable reference inventory changed; review every source position');
 document['x-loreviva-portable-metadata']={body_version:spec.body_version,body_bytes:spec.body_bytes,plan_bytes:spec.plan_bytes,root_kinds:roots,assembly_edge_limit:spec.assembly_edge_limit,assembly_vertex_limit:spec.assembly_vertex_limit,diagnostic_limit:spec.diagnostic_limit,reference_paths:refPaths,diagnostic_fields:[...fields].sort(),diagnostic_arrays:[...arrays].sort()};
 return {document,source_components:Object.fromEntries([...needed].sort().map(name=>[name,createHash('sha256').update(JSON.stringify(source[name])).digest('hex')]))};
}

function portableBackendValidator(header) { return `// ${header}
package generated

import (
 _ "embed"
 "encoding/json"
 "errors"
 "sync"

 "github.com/santhosh-tekuri/jsonschema/v6"
 "loreviva.local/loreviva/internal/wire"
)

//go:embed wepkg-portable.schema.json
var portableSchemaDocument []byte
var portableOnce sync.Once
var portableCompiled *jsonschema.Schema
var portableRulesCompiled *jsonschema.Schema
var portableCompileError error
var portableMetadata PortableSchemaMetadata

type PortableReferencePath struct {
 Component string \`json:"component"\`
 ObjectKind string \`json:"object_kind"\`
 OwnerDomain string \`json:"owner_domain"\`
 Path []string \`json:"path"\`
 ExpectedKinds []string \`json:"expected_kinds"\`
}

type PortableSchemaMetadata struct {
 BodyVersion string \`json:"body_version"\`
 BodyBytes int64 \`json:"body_bytes"\`
 PlanBytes int64 \`json:"plan_bytes"\`
 RootKinds []string \`json:"root_kinds"\`
 ReferencePaths []PortableReferencePath \`json:"reference_paths"\`
 DiagnosticFields []string \`json:"diagnostic_fields"\`
 DiagnosticArrays []string \`json:"diagnostic_arrays"\`
 AssemblyEdgeLimit int \`json:"assembly_edge_limit"\`
 AssemblyVertexLimit int \`json:"assembly_vertex_limit"\`
 DiagnosticLimit int \`json:"diagnostic_limit"\`
}

func initializePortableSchema() {
 var document map[string]any
 if err:=json.Unmarshal(portableSchemaDocument,&document);err!=nil {portableCompileError=err;return}
 metadata,err:=json.Marshal(document["x-loreviva-portable-metadata"])
 if err!=nil {portableCompileError=err;return}
 if err=json.Unmarshal(metadata,&portableMetadata);err!=nil {portableCompileError=err;return}
 compiler:=jsonschema.NewCompiler()
 compiler.AssertFormat()
 compiler.UseRegexpEngine(CompileManifestPattern)
 compiler.UseLoader(manifestNoLoader{})
 const resource="https://schemas.loreviva.invalid/generated/portable"
 if err=compiler.AddResource(resource,document);err!=nil {portableCompileError=err;return}
 portableCompiled,portableCompileError=compiler.Compile(resource)
 if portableCompileError==nil {portableRulesCompiled,portableCompileError=compiler.Compile(resource+"#/$defs/PortableRulesBody")}
}

// ValidatePortableBody checks original bytes with the source-owned fixed bound.
// Passing structure grants no local permission, resource ownership, or import.
func ValidatePortableBody(kind string,data []byte) error {
 value,err:=wire.ParseBounded(data,1048576)
 if err!=nil {return wire.ErrJSON}
 portableOnce.Do(initializePortableSchema)
 if portableCompileError!=nil {return errors.New("portable schema initialization failed")}
 if kind=="RULES" {return portableRulesCompiled.Validate(value)}
 allowed:=false
 for _,root:=range portableMetadata.RootKinds {if root==kind {allowed=true;break}}
 if !allowed {return errors.New("unsupported portable body kind")}
 return portableCompiled.Validate(value)
}

// PortableMetadata returns copies of generated positions, never caller hints.
func PortableMetadata() (PortableSchemaMetadata,error) {
 portableOnce.Do(initializePortableSchema)
 if portableCompileError!=nil {return PortableSchemaMetadata{},errors.New("portable schema initialization failed")}
 out:=portableMetadata
 out.RootKinds=append([]string{},out.RootKinds...)
 out.DiagnosticFields=append([]string{},out.DiagnosticFields...)
 out.DiagnosticArrays=append([]string{},out.DiagnosticArrays...)
 out.ReferencePaths=append([]PortableReferencePath{},out.ReferencePaths...)
 for i:=range out.ReferencePaths {
  out.ReferencePaths[i].Path=append([]string{},out.ReferencePaths[i].Path...)
  out.ReferencePaths[i].ExpectedKinds=append([]string{},out.ReferencePaths[i].ExpectedKinds...)
 }
 return out,nil
}
`; }

export async function generateContracts({sourceRoot=canonicalSourceRoot,outputRoot=repo}={}) {
 const {openapi,wepkg,source_hash,source_hashes}=await loadSources(sourceRoot);const ops=operations(openapi);
 const portable=buildPortableBodySchema(openapi,wepkg);
 const profiles=resolveIntegrationProfiles(openapi);
 const header='Code generated by Loreviva contracts '+GENERATOR_VERSIONS.loreviva+'; DO NOT EDIT. Source SHA-256: '+source_hash;
 const browser=browserValidators(openapi,ops,profiles,header);
 const schemas=Object.entries(sorted(openapi.components.schemas));
 const profileTypes=Object.keys(profiles).length?'\nexport interface ProfileOperations {\n'+Object.entries(profiles).map(([name,doc])=>'  '+JSON.stringify(name)+': {\n'+operations(doc).map(op=>'    '+tsOperation(doc,op)).join('\n')+'\n  };').join('\n')+'\n}\nexport type ProfileResponses = { [P in keyof ProfileOperations]: { [O in keyof ProfileOperations[P]]: ProfileOperations[P][O] extends { responses: infer R } ? R : never } };\nexport type ParsedSSEFrame = IntegrationSSEUpdate;\n':'';
 const ts='// '+header+'\nexport const sourceHash = '+JSON.stringify(source_hash)+' as const;\n'+schemas.map(([name,s])=>'export type '+name+' = '+tsType(s)+';').join('\n')+'\n\nexport interface Operations {\n'+ops.map(op=>'  '+tsOperation(openapi,op)).join('\n')+'\n}\n\nexport interface ProjectionResponses {\n'+ops.filter(op=>op['x-loreviva-response-projection']).map(op=>{const p=op['x-loreviva-response-projection'];return '  '+JSON.stringify(op.operationId)+': { '+JSON.stringify(p.value)+': '+tsType(p.projection_schema)+'; };'}).join('\n')+'\n}\n'+profileTypes+'\nexport const operationRoutes = '+JSON.stringify(Object.fromEntries(ops.map(op=>[op.operationId,{method:op.method.toUpperCase(),path:op.route}])),null,2)+' as const;\n';
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
  'contracts/generated/wepkg-portable.schema.json':json(portable.document),
  'internal/generated/wepkg-portable.schema.json':json(portable.document),
  'internal/generated/wepkg_portable_validate.go':portableBackendValidator(header),
  'internal/generated/wepkg.schema.json':json(wepkg),
  'internal/generated/wepkg_validate.go':manifestBackendValidator(header),
  'contracts/generated/openapi.json':json(openapi),
  'contracts/generated/wepkg.schema.json':json(wepkg),
  'internal/generated/types.go':go,
  'internal/generated/schemas.json':json({$schema:openapi.jsonSchemaDialect,components:openapi.components}),
  'internal/generated/validate.go':validator,
  'packages/sdk-ts/src/generated/contracts.ts':ts,
  'packages/sdk-ts/src/generated/validators.cjs':browser.code,
  'packages/sdk-ts/src/generated/validators.d.cts':browser.types,
  'packages/sdk-ts/src/generated/transport.ts':transportDescriptors(openapi,profiles,header),
 };
 const profile_hashes={};
 for(const [name,doc]of Object.entries(profiles)){const output='contracts/generated/profiles/'+name+'.openapi.json';files[output]=json(doc);profile_hashes[output]=createHash('sha256').update(files[output]).digest('hex');}
 const manifest={source_hash,source_hashes,profile_hashes,generator_versions:GENERATOR_VERSIONS,operation_ids:ops.map(op=>op.operationId),limitations:['Go top-level raw JSON DTOs alias json.RawMessage to preserve JSON methods; validate original wire JSON via generated.Decode before domain use.','TypeScript retains allOf sibling shapes and translates required finite enum discriminants with const/enum if predicates, then/else field requirements, single-field not(required), and small array bounds. Regex, formats, string/number ranges, uniqueness, propertyNames, array upper bounds above 16, arbitrary if predicates and general not/dependent constraints still require full JSON Schema validation.','OpenAPI source must use JSON syntax (valid YAML); no alternate authority or runtime timestamps.']};
 manifest.transport_artifacts=Object.fromEntries(['packages/sdk-ts/src/generated/transport.ts','packages/sdk-ts/src/generated/validators.cjs','packages/sdk-ts/src/generated/validators.d.cts'].map(p=>[p,{bytes:Buffer.byteLength(files[p]),sha256:createHash('sha256').update(files[p]).digest('hex')}]));
 manifest.portable_projection={source_components:portable.source_components,artifacts:Object.fromEntries(['contracts/generated/wepkg-portable.schema.json','internal/generated/wepkg-portable.schema.json','internal/generated/wepkg_portable_validate.go'].map(p=>[p,{bytes:Buffer.byteLength(files[p]),sha256:createHash('sha256').update(files[p]).digest('hex')}]))};
 files['contracts/generated/manifest.json']=json(manifest);
 assertGeneratedLF(files);
 const outputs=Object.keys(files).sort();
 for(const output of outputs){const target=path.join(outputRoot,output);await mkdir(path.dirname(target),{recursive:true});await writeFile(target,files[output],'utf8')}
 return {source_hash,generator_versions:GENERATOR_VERSIONS,outputs};
}

if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url)){const result=await generateContracts();console.log(json(result))}
