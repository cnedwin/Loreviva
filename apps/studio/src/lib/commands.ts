import {shallowRef} from 'vue';
import {AdminTransportError,type AdminClient,type AdminOperation,type AdminSuccess,type Operations} from '@loreviva/sdk-ts';
const runners=new WeakMap<AdminClient,ReturnType<typeof makeRunner>>();
function makeRunner(client:AdminClient){
 const pending=shallowRef<{operation:AdminOperation;input:unknown}>();let busy=false;
 // Share one pending command per client and keep its captured input, including the original key. A retry of that operation uses this input; uncertain transport or recovery-required errors retain it, while definite client rejection clears it.
 async function run<K extends AdminOperation>(operation:K,input:Operations[K]['input']):Promise<AdminSuccess<K>>{
  if(busy)throw Error('原请求正在处理');if(pending.value&&pending.value.operation!==operation)throw Error('先恢复原操作：'+pending.value.operation);
  const original=pending.value??{operation,input:JSON.parse(JSON.stringify(input))};pending.value=original;busy=true;
  try{const result=await client.call(operation,original.input as Operations[K]['input']);pending.value=undefined;return result}
  catch(e){const body=e instanceof AdminTransportError?e.body as {code?:string;recovery_action?:string}:undefined;if(e instanceof AdminTransportError&&e.status>=400&&e.status<500&&body?.code!=='EXTERNAL_UNKNOWN'&&body?.recovery_action!=='USE_ORIGINAL_KEY')pending.value=undefined;throw e}finally{busy=false}
 }
 return {pending,run};
}
export function createCommandRunner(client:AdminClient){let runner=runners.get(client);if(!runner){runner=makeRunner(client);runners.set(client,runner)}return runner}
