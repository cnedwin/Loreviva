<script setup lang="ts">
import {computed,ref,watch,onBeforeUnmount} from 'vue';
import type {Release,DependencyLock,PackagePreviewOptions,PackageImportPreview,PackagePreviewRequirements,GenerationSettings} from '@loreviva/sdk-ts';
import {useProjectStore} from '../stores/project';
import {definitionHash} from '../lib/presets';
import FieldErrors from './FieldErrors.vue';
const props=defineProps<{releases:Release[]}>();const s=useProjectStore();const file=ref<File>();const entry=ref('');const discovery=ref<PackageImportPreview>();const result=ref<PackageImportPreview>();const error=ref<unknown>();const busy=ref(false);const attested=ref(false);const inputKey=ref(0);
const dependencyChoices=ref<Record<string,string>>({});const modelChoices=ref<Record<string,string>>({});const capabilityChoices=ref<Record<string,string>>({});const assetChoices=ref<Record<string,string>>({});let epoch=0;let controller:AbortController|undefined;
const requirements=computed<PackagePreviewRequirements|undefined>(()=>discovery.value?.status==='NEEDS_MAPPING'?discovery.value.requirements:undefined);
const choices=computed(()=>props.releases.flatMap(release=>release.input.objects.map(object=>({key:release.release_id+'/'+object.object_id,label:release.input.name+' / '+object.name+' · '+object.kind,release,object}))));
const generating=computed(()=>requirements.value?.capabilities.filter(c=>c.family==='MODEL'&&c.usages.some(use=>use==='GENERATION'))??[]);
const localCapabilities=computed(()=>requirements.value?.capabilities.filter(c=>c.usages.some(use=>use==='SKILL')||(c.family==='PRESENTATION'&&c.required)||(c.family==='MODEL'&&c.required&&!c.usages.some(use=>use==='GENERATION')))??[]);
function invalidate(){epoch++;controller?.abort();controller=undefined;result.value=undefined;error.value=undefined;busy.value=false}
function reset(){invalidate();file.value=undefined;discovery.value=undefined;entry.value='';attested.value=false;dependencyChoices.value={};modelChoices.value={};capabilityChoices.value={};assetChoices.value={};inputKey.value++}
function chooseFile(event:Event){const selected=(event.target as HTMLInputElement).files?.[0];reset();file.value=selected}
function codeLabel(code:string){return ({SOURCE_REPRESENTATION:'来源信息超出本地格式限制，请调整内容包后再检查',SOURCE_USE_REQUIRED:'请核对每个对象的编辑与复用许可',LOCAL_DEPENDENCY_UNAVAILABLE:'所选依赖当前不可用或不符合来源许可',LOCAL_DEFINITION_BLOCKED:'当前定义、职责或模型配置仍有缺口',MODEL_MAPPING_REQUIRED:'请明确选择符合要求的本地模型与参数',CAPABILITY_MAPPING_REQUIRED:'所需能力尚未完成本地匹配',MAPPING_LIMIT:'依赖数量超出当前预览范围',UNUSED_MAPPING:'存在多余映射，请重新读取并选择',PORTABLE_PACKAGE_INVALID:'内容包未通过完整检查',PORTABLE_ENTRY_INVALID:'所选入口不可用'} as Record<string,string>)[code]??'请检查此项映射与当前可用范围'}
async function send(options:PackagePreviewOptions,stage:'DISCOVER'|'RESOLVE'){
 const selected=file.value;if(!selected)throw Error('请先选择内容包');invalidate();const current=++epoch;const product=s.productId;const tenant=s.tenantId;const bearer=s.bearer;const client=s.client;const local=new AbortController();controller=local;busy.value=true;if(stage==='DISCOVER'){discovery.value=undefined;attested.value=false;dependencyChoices.value={};modelChoices.value={};capabilityChoices.value={};assetChoices.value={}}
 try{const response=await client.previewPackage(product,selected,options,crypto.randomUUID(),local.signal);if(current!==epoch||product!==s.productId||tenant!==s.tenantId||bearer!==s.bearer||client!==s.client||file.value!==selected)return;
  if(stage==='DISCOVER'&&response.status==='NEEDS_MAPPING'){discovery.value=response;entry.value=response.entrypoint;dependencyChoices.value={};modelChoices.value={};capabilityChoices.value={};assetChoices.value={};attested.value=false;result.value=undefined}else result.value=response;
 }catch(e){if(current===epoch&&!local.signal.aborted)error.value=e}finally{if(current===epoch){busy.value=false;controller=undefined}}
}
async function discover(){try{await send({stage:'DISCOVER',...(entry.value?{entrypoint:entry.value}:{})},'DISCOVER')}catch(e){error.value=e}}
async function resolve(){
 const current=epoch,product=s.productId,tenant=s.tenantId,bearer=s.bearer,client=s.client,selectedFile=file.value;
 try{const req=requirements.value;const initial=discovery.value;if(!req||initial?.status!=='NEEDS_MAPPING'||!attested.value)throw Error('请读取内容包并核对来源许可');busy.value=true;
  const mappings:Array<{pointer:string;target:DependencyLock}>=[];const additional=new Map<string,DependencyLock>();const selectedRoots:Array<{release:Release;objectId:string}>=[];
  for(const reference of req.references){const selected=choices.value.find(c=>c.key===dependencyChoices.value[reference.pointer]);if(!selected)throw Error('请为每项外部依赖选择本地内容');if(reference.expected_kinds.length&&!reference.expected_kinds.includes(selected.object.kind))throw Error('所选依赖类型不匹配');mappings.push({pointer:reference.pointer,target:{release_id:selected.release.release_id,object_id:selected.object.object_id,version:selected.object.version,content_hash:await definitionHash(selected.object)}});selectedRoots.push({release:selected.release,objectId:selected.object.object_id})}
  // Follow only references actually reachable from the selected immutable
  // objects. These hints remain untrusted until the server's owning checks.
  const visited=new Set<string>();function collect(release:Release,id:string){const key=release.release_id+'/'+id;if(visited.has(key))return;visited.add(key);if(visited.size>10000)throw Error('依赖范围过大');const object=release.input.objects.find(o=>o.object_id===id);if(!object)throw Error('所选版本缺少依赖对象');function walk(v:unknown){if(Array.isArray(v)){v.forEach(walk);return}if(!v||typeof v!=='object')return;const ref=v as Record<string,unknown>;if(ref.kind==='LOCAL_DRAFT'){collect(release,String(ref.object_id));return}if(ref.kind==='PINNED_DEFINITION'){const lock=release.dependency_locks.find(d=>d.release_id===ref.release_id&&d.object_id===ref.object_id&&d.version===ref.version&&d.content_hash===ref.content_hash);if(!lock)throw Error('所选版本缺少固定依赖');additional.set(JSON.stringify(lock),lock);const next=props.releases.find(r=>r.release_id===lock.release_id);if(next)collect(next,lock.object_id);return}Object.values(ref).forEach(walk)}walk(object)}selectedRoots.forEach(root=>collect(root.release,root.objectId));for(const mapping of mappings)additional.delete(JSON.stringify(mapping.target));
  const models=generating.value.map(cap=>{if(modelChoices.value[cap.capability_id]!=='MOCK')throw Error('请明确选择本地模型');const settings:GenerationSettings={mode:'OVERRIDE',connection_ref:'mock-local',model_id:'synthetic-text-v1',required_capabilities:['TEXT'],allowed_external_uses:['NONE'],...('parameters'in cap?{parameters:cap.parameters}:{})};return {capability_id:cap.capability_id,settings}});
  const capabilities:Record<string,'TEXT'|'SIGN_CUSTODY'>={};for(const cap of localCapabilities.value){const value=capabilityChoices.value[cap.capability_id];if(value!=='TEXT'&&value!=='SIGN_CUSTODY')throw Error('请为所需能力选择本地对应项');capabilities[cap.capability_id]=value}
  const assets=req.files.map(asset=>{const id=assetChoices.value[asset.file_path];if(!id)throw Error('请填写已上传并有权使用的本地资产标识');return {file_path:asset.file_path,asset_id:id}});
  if(current!==epoch||product!==s.productId||tenant!==s.tenantId||bearer!==s.bearer||client!==s.client||file.value!==selectedFile)return;
  await send({stage:'RESOLVE',entrypoint:initial.entrypoint,namespace:initial.namespace,dependency_mappings:mappings,additional_locks:[...additional.values()],model_mappings:models,local_capabilities:capabilities,asset_mappings:assets,rights:req.objects.map(o=>({object_id:o.object_id,allowed_uses:['EDIT','COPY']})),rights_attested:true},'RESOLVE');
 }catch(e){if(current===epoch)error.value=e}finally{if(current===epoch)busy.value=false}
}
watch([()=>s.productId,()=>s.tenantId,()=>s.bearer,()=>s.client],reset);watch(()=>props.releases,()=>{if(result.value)invalidate()});onBeforeUnmount(()=>{epoch++;controller?.abort()});
</script>
<template>
<section class="panel" aria-labelledby="package-preview-title">
 <h2 id="package-preview-title">内容包导入预览</h2><p>读取世界、角色或其他创作材料，匹配本地依赖并检查编辑候选。本页不会导入或发布内容。</p>
 <FieldErrors :error="error"/>
 <label>选择内容包<input :key="inputKey" type="file" accept=".wepkg" @change="chooseFile"></label><p v-if="file" class="muted">已选择：{{file.name}}</p>
 <div class="actions"><button :disabled="!file||busy" @click="discover">{{discovery?'重新读取内容包':'读取内容包'}}</button><button :disabled="!file&&!busy" @click="reset">取消预览</button></div>
 <template v-if="requirements">
  <label>创作入口<select v-model="entry" :disabled="busy" @change="discover"><option v-for="item in requirements.entries" :key="item.logical_id" :value="item.logical_id">{{item.logical_id}} · {{item.kind}}</option></select></label>
  <h3>来源与使用范围</h3><ul><li v-for="object in requirements.objects" :key="object.object_id"><strong>{{object.object_id}}</strong> · {{object.kind}} · {{object.license}}<p class="muted break">{{object.source_reference}} · 原版本 {{object.source_version}}</p></li></ul>
  <label><input v-model="attested" type="checkbox" @change="invalidate">我已核对来源许可，允许在本项目编辑和复用以上内容。</label>
  <template v-if="requirements.references.length"><h3>匹配外部内容</h3><label v-for="reference in requirements.references" :key="reference.pointer">{{reference.logical_id}} · {{reference.expected_kinds.join(' / ')||'内容对象'}}<select v-model="dependencyChoices[reference.pointer]" :aria-label="reference.logical_id+' · '+(reference.expected_kinds.join(' / ')||'内容对象')" @change="invalidate"><option value="">请选择本地已发布内容</option><option v-for="choice in choices.filter(c=>!reference.expected_kinds.length||reference.expected_kinds.includes(c.object.kind))" :key="choice.key" :value="choice.key">{{choice.label}}</option></select></label></template>
  <template v-if="generating.length"><h3>选择本地模型</h3><label v-for="capability in generating" :key="capability.capability_id">{{capability.capability_id}}<select :aria-label="'模型 '+capability.capability_id" v-model="modelChoices[capability.capability_id]" @change="invalidate"><option value="">请选择模型</option><option value="MOCK">本地演示模型</option></select><span class="muted">模型当前状态将在检查时验证。</span></label></template>
  <template v-if="localCapabilities.length"><h3>匹配已登记能力</h3><label v-for="capability in localCapabilities" :key="capability.family+capability.capability_id">{{capability.capability_id}}<select :aria-label="'能力 '+capability.capability_id" v-model="capabilityChoices[capability.capability_id]" @change="invalidate"><option value="">请选择对应能力</option><option value="TEXT">文字</option><option v-if="capability.family!=='MODEL'&&capability.family!=='PRESENTATION'" value="SIGN_CUSTODY">道具保管规则</option></select></label></template>
  <template v-if="requirements.files.length"><h3>匹配已有资产</h3><label v-for="asset in requirements.files" :key="asset.file_path">{{asset.file_path}} · {{asset.media_type}}<input v-model="assetChoices[asset.file_path]" placeholder="已上传的本地资产标识" @input="invalidate"></label></template>
  <p v-if="requirements.capabilities.some(c=>c.family==='TOOL')" class="muted">此包声明了工具需求，当前版本的预览会明确报告支持范围。</p>
  <button class="primary" :disabled="busy||!attested" @click="resolve">检查本地匹配</button>
 </template>
 <p v-if="busy" role="status">正在检查…</p>
 <section v-if="result" aria-live="polite"><h3>{{result.status==='RESOLVED'?'本地匹配通过，尚未导入':'仍有需要处理的项目'}}</h3><ul><li v-for="(finding,index) in result.findings" :key="index">{{codeLabel(finding.code)}}</li></ul><template v-if="result.status==='RESOLVED'"><p>已生成 {{result.input.objects.length}} 个对象的编辑预览。后续导入仍需独立检查和确认。</p><ul><li v-for="object in result.input.objects" :key="object.object_id">{{object.name}} · {{object.kind}}</li></ul><details><summary>查看候选内容</summary><pre class="preview">{{JSON.stringify(result.input,null,2)}}</pre></details></template></section>
</section>
</template>
