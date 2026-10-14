import type {ContentObject,DraftInput,Release,DefinitionRef,DependencyLock} from '@loreviva/sdk-ts';
function quote(v:string){return JSON.stringify(v).replace(/[<>&\u2028\u2029]/g,c=>'\\u'+c.charCodeAt(0).toString(16).padStart(4,'0'))}
// Hash the canonical definition, including normalized finite numbers, escaped strings and sorted object keys; ordinary JSON serialization is not the content-hash representation.
export function canonicalDefinition(value:unknown):string {
 if(value===null)return 'null';if(typeof value==='string')return quote(value);if(typeof value==='boolean')return String(value);
 if(typeof value==='number'){if(!Number.isFinite(value))throw Error('非有限数字');if(value===0)return '0';let text=String(value),sign='';if(text.startsWith('-')){sign='-';text=text.slice(1)}const parts=text.toLowerCase().split('e');let coefficient=parts[0],exponent=Number(parts[1]??0);const dot=coefficient.indexOf('.');if(dot>=0){exponent-=coefficient.length-dot-1;coefficient=coefficient.replace('.','')}coefficient=coefficient.replace(/^0+/,'');const trimmed=coefficient.replace(/0+$/,'');exponent+=coefficient.length-trimmed.length;return sign+trimmed+(exponent?'e'+exponent:'')}
 if(Array.isArray(value))return '['+value.map(canonicalDefinition).join(',')+']';
 if(value&&typeof value==='object')return '{'+Object.keys(value).sort().map(k=>quote(k)+':'+canonicalDefinition((value as Record<string,unknown>)[k])).join(',')+'}';throw Error('不支持的内容值');
}
export async function definitionHash(value:unknown){const bytes=new TextEncoder().encode(canonicalDefinition(value));return [...new Uint8Array(await crypto.subtle.digest('SHA-256',bytes))].map(b=>b.toString(16).padStart(2,'0')).join('')}
// Copy into a local draft with a new object identity and an origin reference. Rewrite release-local references to pinned definitions and retain their dependency locks; returning this draft does not persist or publish it.
export async function adoptPreset(input:DraftInput,release:Release,objectId:string,newId:string):Promise<DraftInput>{
 const source=release.input.objects.find(o=>o.object_id===objectId);if(!source)throw Error('预设对象不存在');if(input.objects.some(o=>o.object_id===newId))throw Error('新对象标识已存在');
 const result=JSON.parse(JSON.stringify(input)) as DraftInput;const pins=new Map<string,DefinitionRef>();const locks=new Map<string,DependencyLock>();
 for(const object of release.input.objects){const content_hash=await definitionHash(object);pins.set(object.object_id,{kind:'PINNED_DEFINITION',release_id:release.release_id,object_id:object.object_id,version:object.version,content_hash})}
 function retain(ref:DefinitionRef){if(ref.kind!=='PINNED_DEFINITION')throw Error('预设引用未冻结');const {kind,...lock}=ref;locks.set(JSON.stringify(lock),lock)}
 function rewrite(value:unknown):unknown {if(Array.isArray(value))return value.map(rewrite);if(!value||typeof value!=='object')return value;const object=value as Record<string,unknown>;if(object.kind==='LOCAL_DRAFT'){const pin=pins.get(String(object.object_id));if(!pin)throw Error('预设本地引用缺失');retain(pin);return pin}if(object.kind==='PINNED_DEFINITION'){retain(object as DefinitionRef);return {...object}}return Object.fromEntries(Object.entries(object).map(([k,v])=>[k,rewrite(v)]))}
 const adopted=rewrite(source) as ContentObject;adopted.object_id=newId;adopted.version='1';adopted.origin_ref=pins.get(objectId)!;retain(adopted.origin_ref);if('builtin_original'in adopted)adopted.builtin_original=false;result.objects.push(adopted);
 for(const lock of release.dependency_locks)locks.set(JSON.stringify(lock),lock);
 for(const lock of locks.values())if(!result.dependency_locks.some(x=>JSON.stringify(x)===JSON.stringify(lock)))result.dependency_locks.push(lock);
 return result;
}
