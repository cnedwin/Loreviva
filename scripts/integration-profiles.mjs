// Resolve the closed, source-owned allocation into ordinary OpenAPI profiles.
// This whitelist is not a caller-supplied projection language or authority.
const ref=name=>({$ref:'#/components/schemas/'+name});
const same=(a,b)=>JSON.stringify(a)===JSON.stringify(b);
const keys=(object,wanted)=>object&&typeof object==='object'&&!Array.isArray(object)&&same(Object.keys(object).sort(),[...wanted].sort());
const tuples=[['getTurn','IntegrationTurnView'],['getEvents','IntegrationEventPage'],['streamEvents','IntegrationSSEUpdate'],['listMemories','PlayerMemoryMetadataPage'],['getMemory','PlayerMemoryMetadata']];
const ops=doc=>Object.values(doc.paths).flatMap(item=>Object.values(item).filter(op=>op?.operationId));
export function resolveIntegrationProfiles(source) {
 const meta=source['x-integration-contract-profiles'];
 if(meta===undefined){if(source.info.version==='1.6.0')throw Error('missing integration profile metadata');return {};}
 const fail=()=>{throw Error('invalid closed integration profile projection');};
 if(!keys(meta,['selector_header','default_profile','profiles'])||meta.selector_header!=='Loreviva-Client-Contract'||meta.default_profile!=='public_v1'||!Array.isArray(meta.profiles)||meta.profiles.length!==2)fail();
 const [legacy,integration]=meta.profiles;
 for(const p of meta.profiles)if(!keys(p,['value','response_projections'])||!Array.isArray(p.response_projections))fail();
 if(legacy.value!=='public_v1'||legacy.response_projections.length||integration.value!=='integration_r0_v1'||integration.response_projections.length!==5)fail();
 const sourceOps=ops(source);
 for(const [index,[id,name]]of tuples.entries()){
  const projection=integration.response_projections[index];const stream=id==='streamEvents';
  if(!keys(projection,['operation_id','status','media_type','schema',...(stream?['sse_data']:[])])||projection.operation_id!==id||projection.status!=='200'||projection.media_type!==(stream?'text/event-stream':'application/json')||!same(projection.schema,stream?{type:'string'}:ref(name))||(stream&&!same(projection.sse_data,ref(name)))||!source.components.schemas[name])fail();
  const op=sourceOps.find(x=>x.operationId===id);if(!op||!op.responses['200']?.content?.[projection.media_type])fail();
  if(id==='getMemory'||id==='listMemories'){
   const previous=id==='getMemory'?'Memory':'MemoryPage';const provenance=id==='getMemory'?'MemoryProvenanceV1':'MemoryProvenancePageV1';const p=op['x-loreviva-response-projection'];
   if(!keys(p,['parameter','value','status','default_schema','projection_schema'])||p.parameter!=='include'||p.value!=='provenance_v1'||p.status!=='200'||!same(p.default_schema,ref(previous))||!same(p.projection_schema,ref(provenance))||!same(op.responses['200'].content['application/json'].schema,{oneOf:[ref(previous),ref(provenance)]}))fail();
  }
 }
 const output={};
 for(const profile of meta.profiles){
  const doc=structuredClone(source);delete doc['x-integration-contract-profiles'];
  for(const op of ops(doc)){
   // Profile selection never changes bearer rights or the Admin SDK surface.
   if(op.security?.some(s=>Object.hasOwn(s,'OperatorBearer')))continue;
   if((op.parameters??[]).some(p=>p.name===meta.selector_header))fail();
   (op.parameters??=[]).push({name:meta.selector_header,in:'header',required:profile.value!=='public_v1',schema:{type:'string',const:profile.value,...(profile.value==='public_v1'?{default:'public_v1'}:{})}});
  }
  for(const projection of profile.response_projections){
   const op=ops(doc).find(x=>x.operationId===projection.operation_id);const response=op.responses[projection.status];const media=response.content[projection.media_type];
   // Keep canonical examples immutable. Derived examples use their actual
   // projected shape rather than relabelling a private memory body.
   delete media.example;delete media.examples;
   if(projection.sse_data){
    media.schema={type:'string'};
    const frame=source.components.schemas.IntegrationSSEUpdate.examples.find(x=>x.event==='update');
    media.example='event: update\nid: '+frame.id+'\ndata: '+JSON.stringify(frame.data)+'\n\n';
    response['x-sse-data-schema']=ref('IntegrationEventPage');response['x-sse-frame-schema']=structuredClone(projection.sse_data);
   }else{
    const p=op['x-loreviva-response-projection'];
    media.schema=p?{oneOf:[structuredClone(projection.schema),structuredClone(p.projection_schema)]}:structuredClone(projection.schema);
    if(p)p.default_schema=structuredClone(projection.schema);
    const name=projection.schema.$ref.split('/').at(-1);media.examples={integration_default:{value:structuredClone(source.components.schemas[name].examples[0])}};
    if(p){const name=p.projection_schema.$ref.split('/').at(-1);const example=source.components.schemas[name].examples?.[0];if(example)media.examples.provenance_v1={value:structuredClone(example)};}
   }
  }
  output[profile.value]=doc;
 }
 return output;
}
