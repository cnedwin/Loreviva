import test from 'node:test';
import assert from 'node:assert/strict';
import {createLorevivaClient} from '../packages/sdk-ts/src/index.ts';
import {createAdminClient,AdminTransportError} from '../packages/sdk-ts/src/admin.ts';
import {readFile} from 'node:fs/promises';
test('SDK config requires HTTP origin and an explicit session token',()=>{
 for(const baseUrl of ['invalid','file:///private','http://user:pass@localhost']) assert.throws(()=>createLorevivaClient({baseUrl,sessionToken:'synthetic-token'}));
 assert.throws(()=>createLorevivaClient({baseUrl:'http://127.0.0.1:8080',sessionToken:''}));
 const client=createLorevivaClient({baseUrl:'http://127.0.0.1:8080/',sessionToken:'synthetic-token'});
 assert.equal(client.baseUrl,'http://127.0.0.1:8080');assert.equal(client.sessionToken,'synthetic-token');assert.ok(Object.isFrozen(client));
});
test('import transport preserves original recovery after ambiguous responses',async t=>{
 const source=JSON.parse(await readFile(new URL('../contracts/source/openapi.yaml',import.meta.url),'utf8'));
 const options=source.components.schemas.PackageImportOptions.examples[0];
 const refusal={code:'FORBIDDEN',correlation_id:'synthetic-import',message:'Not granted',recovery_action:'FIX_REQUEST',retryable:false};
 for(const [name,status,body,expected] of [['broken success',201,'{',0],['invalid success schema',201,'{}',0],['server failure',500,JSON.stringify(refusal),0],['proxy failure',502,'<html>failure</html>',0],['unverified refusal',403,'{}',0],['verified refusal',403,JSON.stringify(refusal),403]]){
  await t.test(name,async()=>{let count=0;const client=createAdminClient({getOperatorBearer:()=> 'synthetic-sdk-operator',fetch:async(_url,init)=>{count++;assert.equal(init.headers['Idempotency-Key'],'original-import-key');assert.equal(init.body.get('archive').size,3);return new Response(body,{status})}});
   await assert.rejects(client.importPackage('coffee',new Blob(['zip']),options,'original-import-key'),e=>e instanceof AdminTransportError&&e.status===expected&&(expected!==0||e.body.code==='EXTERNAL_UNKNOWN'));assert.equal(count,1,'transport must not retry with another request');
  });
 }
});
