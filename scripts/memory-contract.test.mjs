import test from 'node:test';
import assert from 'node:assert/strict';
import {loadSources} from './generate-contracts.mjs';
import path from 'node:path';
test('memory protocol requires explicit provenance and separate Operator actions',async()=>{
 const {openapi:d}=await loadSources(path.resolve('contracts/source'));
 assert.equal(d.info.version,'1.10.0');
 assert.deepEqual(d.paths['/world/v1/instances/{instance_id}/memories'].get.parameters.at(-1),{name:'include',in:'query',required:false,schema:{type:'string',enum:['provenance_v1']}});
 for(const [url,item]of Object.entries(d.paths).filter(([u])=>u.includes('/admin/')&&u.includes('memor'))){for(const op of Object.values(item)){if(op.operationId)assert.deepEqual(op.security,[{OperatorBearer:[]}],url)}}
 assert.equal(d.components.schemas.MemoryProvenanceV1.additionalProperties,false);
 assert.deepEqual(d.components.schemas.MemorySelectorV1.oneOf[0].properties.target_kind.enum,['MEMORY_NODE','SOURCE']);
});
