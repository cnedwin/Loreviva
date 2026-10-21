import {createIntegrationTransport,createReferenceBffClient, type Operation, type OperationsForRole} from '../../packages/sdk-ts/src/client.ts';
import type {MemoryProvenanceV1,PlayerMemoryMetadata,Error as APIError} from '../../packages/sdk-ts/src/generated/contracts.ts';
import type {AuthorizedMediaBody} from '../../packages/sdk-ts/src/media.ts';
import type {AsyncDisposableEventStream} from '../../packages/sdk-ts/src/sse.ts';

// This function is never invoked: the strict noEmit gate verifies both positive
// assignments and rejected source-role/profile/protocol combinations.
export async function compileOnlySourceContracts(){
 const client=createIntegrationTransport({baseUrl:'https://example.invalid',profile:'integration_r0_v1',role:'CLIENT',getBearer:()=>null});
 const headers={'Loreviva-Client-Contract':'integration_r0_v1' as const};
 const result=await client.call('getMemory',{path:{instance_id:'i',memory_id:'m'},headers});
 if(result.status==='200'){const metadata:PlayerMemoryMetadata=result.body;void metadata;
  // @ts-expect-error Default integration metadata never acquires a body by a cast-free call.
  const provenance:MemoryProvenanceV1=result.body;void provenance}
 const withProvenance=await client.call('getMemory',{path:{instance_id:'i',memory_id:'m'},headers,query:{include:'provenance_v1'}});
 if(withProvenance.status==='200'){const provenance:MemoryProvenanceV1=withProvenance.body;void provenance}
 const media=await client.call('readRuntimeMedia',{path:{asset_id:'a'},headers});
 if(media.status==='200'||media.status==='206'){const resource:AuthorizedMediaBody=media.body;void resource}
 if(media.status==='416'){const error:APIError=media.body;void error;
  // @ts-expect-error A JSON range error is not a binary reader.
  await media.body.read()}
 const events=await client.call('streamEvents',{path:{instance_id:'i'},headers,query:{cursor:'opaque_cursor_0001'}});
 if(events.status==='200'){const resource:AsyncDisposableEventStream=events.body;void resource}
 // @ts-expect-error CLIENT cannot use the SERVICE create operation.
 await client.call('createInstance',{});
 // @ts-expect-error Explicit integration profile header is mandatory.
 await client.call('getPlayerSelfConsent',{});
 // @ts-expect-error A different profile cannot be used through this owner.
 await client.call('getPlayerSelfConsent',{headers:{'Loreviva-Client-Contract':'public_v1'}});
 // @ts-expect-error A caller cannot replace Authorization through typed source headers.
 await client.call('getPlayerSelfConsent',{headers:{...headers,Authorization:'caller'}});
 const operator=createIntegrationTransport({baseUrl:'https://example.invalid',profile:'public_v1',role:'OPERATOR',getBearer:()=>null});
 // @ts-expect-error Multipart file is an actual Blob, not a JSON wire string.
 await operator.call('uploadContentAsset',{headers:{'Idempotency-Key':'k'},body:{metadata:{} as never,file:'not-a-blob'}});
 const known:Operation<'public_v1'>='getTurn';void known;
 // @ts-expect-error No handwritten unknown operation is admitted.
 const unknown:Operation<'public_v1'>='invented';void unknown;
 // @ts-expect-error Credential alternatives do not imply a role fallback.
 const forbidden:OperationsForRole<'public_v1','CLIENT'>='publishContentDraft';void forbidden;
 const browser=createReferenceBffClient('integration_r0_v1');
 const localProjection=await browser.read('getMemory',{path:{instance_id:'i',memory_id:'m'},headers,query:{include:'provenance_v1'}});
 if(localProjection.status==='200'){const value:MemoryProvenanceV1=localProjection.body;void value}
 // @ts-expect-error Local reads cannot become a mutation proxy.
 await browser.read('submitTurn',{});
 // @ts-expect-error The local factory accepts no caller base URL or credential provider.
 createReferenceBffClient('integration_r0_v1',{baseUrl:'https://example.invalid'});
}
