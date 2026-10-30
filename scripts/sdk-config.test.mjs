import test from 'node:test';
import assert from 'node:assert/strict';
import {createLorevivaClient} from '../packages/sdk-ts/src/index.ts';
import {createAdminClient,AdminTransportError} from '../packages/sdk-ts/src/admin.ts';
import {readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
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

test('approval and export keep the original key for ambiguous write replies',async t=>{
 const source=JSON.parse(await readFile(new URL('../contracts/source/openapi.yaml',import.meta.url),'utf8'));
 for(const operation of ['approvePackageDraft','exportContentPackage'])for(const [status,body]of [[201,'{'],[201,'{}'],[500,'{}'],[403,'{}']])await t.test(operation+' '+status+' '+body,async()=>{
  let calls=0;const client=createAdminClient({getOperatorBearer:()=> 'synthetic-transfer-operator',fetch:async(_url,init)=>{calls++;assert.equal(init.headers['Idempotency-Key'],'original-transfer');return new Response(body,{status})}});
  const input={headers:{'Idempotency-Key':'original-transfer'},body:source.components.schemas[operation==='approvePackageDraft'?'PackageApprovalRequest':'PackageExportRequest'].examples[0],...(operation==='approvePackageDraft'?{path:{draft_id:'draft'}}:{})};
  await assert.rejects(client.call(operation,input),e=>e instanceof AdminTransportError&&e.status===0&&e.body.code==='EXTERNAL_UNKNOWN');assert.equal(calls,1);
 });
});

test('package binary reads bind the current scope and exact metadata to bytes',async t=>{
 const source=JSON.parse(await readFile(new URL('../contracts/source/openapi.yaml',import.meta.url),'utf8'));const body=Buffer.from('abc'),hash=createHash('sha256').update(body).digest('hex');
 const receipt={...source.components.schemas.PackageExportReceipt.examples[0],size_bytes:'3',archive_sha256:hash};
 const info={...source.components.schemas.PackageFileInfo.examples[0],file_path:'files/note.txt',size_bytes:'3',sha256:hash};
 for(const tamper of [false,true])await t.test('export '+tamper,async()=>{let calls=0;const client=createAdminClient({getOperatorBearer:()=> 'synthetic-transfer-operator',fetch:async(url,init)=>{calls++;assert.ok(url.startsWith('/world/v1/admin/content-packages/exports/'));assert.equal(init.credentials,'omit');assert.equal(init.redirect,'error');return new Response(tamper?'abd':body)}});if(tamper)await assert.rejects(client.downloadPackage(receipt,receipt.scope.product_id));else assert.deepEqual(Buffer.from(await client.downloadPackage(receipt,receipt.scope.product_id)),body);assert.equal(calls,1)});
 for(const mode of ['valid','wrong path','wrong hash','missing metadata'])await t.test('file '+mode,async()=>{const metadata={...info,...(mode==='wrong path'?{file_path:'files/other.txt'}:{}),...(mode==='wrong hash'?{sha256:'a'.repeat(64)}:{})};const headers=mode==='missing metadata'?{}:{'Loreviva-Package-File':Buffer.from(JSON.stringify(metadata)).toString('base64')};const client=createAdminClient({getOperatorBearer:()=> 'synthetic-transfer-operator',fetch:async(url,init)=>{assert.ok(url.startsWith('/world/v1/admin/content-packages/file-bytes?'));assert.equal(init.body.get('archive').size,3);assert.equal(init.credentials,'omit');return new Response(body,{headers})}});if(mode==='valid'){const read=await client.readPackageFile('coffee',new Blob(['zip']),'files/note.txt');assert.deepEqual(Buffer.from(read.bytes),body);assert.equal(read.metadata.file_path,info.file_path)}else await assert.rejects(client.readPackageFile('coffee',new Blob(['zip']),'files/note.txt'))});
});
