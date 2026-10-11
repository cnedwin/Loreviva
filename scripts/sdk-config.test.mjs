import test from 'node:test';
import assert from 'node:assert/strict';
import {createLorevivaClient} from '../packages/sdk-ts/src/index.ts';
test('SDK config requires HTTP origin and an explicit session token',()=>{
 for(const baseUrl of ['invalid','file:///private','http://user:pass@localhost']) assert.throws(()=>createLorevivaClient({baseUrl,sessionToken:'synthetic-token'}));
 assert.throws(()=>createLorevivaClient({baseUrl:'http://127.0.0.1:8080',sessionToken:''}));
 const client=createLorevivaClient({baseUrl:'http://127.0.0.1:8080/',sessionToken:'synthetic-token'});
 assert.equal(client.baseUrl,'http://127.0.0.1:8080');assert.equal(client.sessionToken,'synthetic-token');assert.ok(Object.isFrozen(client));
});
