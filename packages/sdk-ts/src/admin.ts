import validators from './generated/validators.cjs';
import {operationRoutes} from './generated/contracts.ts';
import type {Operations,AssetUploadMetadata,FileRecord} from './generated/contracts.ts';

function validate(name:string,value:unknown):void {if(!Object.hasOwn(validators,name))throw Error('未知契约');const validator=validators[name as keyof typeof validators];if(!validator(value))throw new AdminTransportError(422,{message:'字段格式不符合契约',field_errors:validator.errors?.map(e=>({pointer:e.instancePath,code:e.keyword,message:e.message??'字段无效'}))})}
export function validateContract(name:string,value:unknown):void {validate('schema_'+name,value)}
export class AdminTransportError extends Error {readonly status:number;readonly body:unknown;constructor(status:number,body:unknown){super((body as {message?:string})?.message??'请求失败');this.status=status;this.body=body;this.name='AdminTransportError'}}
type Keys={ [K in keyof Operations]: (typeof operationRoutes)[K]['path'] extends `/world/v1/admin/${string}` ? K:never}[keyof Operations];
export type AdminOperation=Exclude<Keys,'uploadContentAsset'>;
export type AdminSuccess<K extends keyof Operations>=Operations[K]['responses'][Extract<keyof Operations[K]['responses'],`2${string}`>];
interface Input {path?:Record<string,string>;query?:Record<string,unknown>;headers?:Record<string,string>;body?:unknown}
const MAX_BYTES=16*1024*1024;
// Enforce the limit on streamed bytes even when Content-Length is absent; cancel a failed read and release its reader.
// A transport failure does not prove that a write was uncommitted.
async function bounded(response:Response,limit:number):Promise<Uint8Array> {const length=response.headers.get('content-length');if(length&&(!/^\d+$/.test(length)||BigInt(length)>BigInt(limit)))throw Error('响应超过大小限制');if(!response.body)return new Uint8Array();const reader=response.body.getReader();const chunks:Uint8Array[]=[];let size=0;try{for(;;){const {done,value}=await reader.read();if(done)break;size+=value.byteLength;if(size>limit)throw Error('响应超过大小限制');chunks.push(value)}}catch(e){await reader.cancel();throw e}finally{reader.releaseLock()}const out=new Uint8Array(size);let offset=0;for(const chunk of chunks){out.set(chunk,offset);offset+=chunk.length}return out}
// Read the current bearer for each request; never send cookies or follow redirects. Bound reads and validate successful JSON replies.
// The caller must recover an uncertain write with its original operation, payload and key; this client does not automatically recover it.
// Asset metadata supplies an expected size, not authority to read the bytes.
export function createAdminClient(options:{getOperatorBearer:()=>string|null;fetch?:typeof fetch}) {
 const request=options.fetch??globalThis.fetch.bind(globalThis);
 function headers(extra:Record<string,string>={}) {const bearer=options.getOperatorBearer();if(!bearer)throw Error('请填写操作员凭据');return {...extra,Authorization:'Bearer '+bearer}}
 async function decode(response:Response):Promise<unknown> {const bytes=await bounded(response,MAX_BYTES);let value:unknown;try{value=JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(bytes))}catch{throw new AdminTransportError(response.status,{message:'服务返回了无效数据'})}if(!response.ok)throw new AdminTransportError(response.status,value);return value}
 async function call<K extends AdminOperation>(operation:K,input:Operations[K]['input'],signal?:AbortSignal):Promise<AdminSuccess<K>> {
  const route=operationRoutes[operation];if(!route.path.startsWith('/world/v1/admin/'))throw Error('操作不属于管理接口');
  const auth=headers();const actual=input as Input;validate('input_'+operation,actual);
  const path=route.path.replace(/\{([^}]+)\}/g,(_,key:string)=>{const value=actual.path?.[key];if(value===undefined)throw Error('缺少路径字段');return encodeURIComponent(value)});
  const query=new URLSearchParams();for(const [k,v] of Object.entries(actual.query??{}))if(v!==undefined)query.set(k,String(v));
  const init:RequestInit={method:route.method,headers:{...actual.headers,...auth},credentials:'omit',redirect:'error',cache:'no-store'};if(signal)init.signal=signal;if(actual.body!==undefined){init.headers={...init.headers,'Content-Type':'application/json'};init.body=JSON.stringify(actual.body)}
  const response=await request(path+(query.size?'?'+query:''),init);const value=await decode(response);try{validate('response_'+operation+'_'+response.status,value)}catch{throw new AdminTransportError(0,{message:'响应无法验证；请用原请求键恢复，勿新建操作'})}return value as AdminSuccess<K>;
 }
 async function readAsset(assetId:string,productId:string,size:string,signal?:AbortSignal):Promise<Uint8Array> {if(!/^\d+$/.test(size)||BigInt(size)>BigInt(MAX_BYTES))throw Error('资产大小不受支持');const url=operationRoutes.readAuthorizedAsset.path.replace('{asset_id}',encodeURIComponent(assetId));const response=await request(url+'?'+new URLSearchParams({product_id:productId}),{headers:headers(),credentials:'omit',redirect:'error',cache:'no-store',...(signal?{signal}:{})});if(!response.ok){await decode(response);throw Error('读取失败')}const bytes=await bounded(response,Number(size));if(bytes.length!==Number(size))throw Error('资产大小不符');return bytes}
 async function upload(metadata:AssetUploadMetadata,file:Blob,key:string):Promise<FileRecord> {validateContract('AssetUploadMetadata',metadata);if(String(file.size)!==metadata.expected_size||file.size>MAX_BYTES)throw Error('上传大小不符');const form=new FormData();form.set('metadata',JSON.stringify(metadata));form.set('file',file,metadata.name);const response=await request(operationRoutes.uploadContentAsset.path,{method:'POST',headers:headers({'Idempotency-Key':key}),body:form,credentials:'omit',redirect:'error',cache:'no-store'});const value=await decode(response);validateContract('FileRecord',value);return value as FileRecord}
 return {call,readAsset,upload};
}
export type AdminClient=ReturnType<typeof createAdminClient>;
