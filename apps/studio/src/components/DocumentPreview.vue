<script setup lang="ts">
import {computed,onBeforeUnmount,ref,shallowRef,watch} from 'vue';
import type {DocumentCapabilities,DocumentPreview,FileRecord} from '@loreviva/sdk-ts';
import {useProjectStore} from '../stores/project';
import FieldErrors from './FieldErrors.vue';
const props=defineProps<{asset?:FileRecord}>();const s=useProjectStore();
const caps=ref<DocumentCapabilities>();const result=ref<DocumentPreview>();const error=shallowRef<unknown>();const assetId=ref('');const busy=ref(false);let epoch=0;let pending:AbortController|undefined;let capabilityEpoch=0;let capabilityRequest:AbortController|undefined;
const labels={COMPLETE:'支持范围内文字已提取',PARTIAL:'部分提取：请检查缺失位置',OCR_REQUIRED:'需要OCR：当前未启用',UNSUPPORTED:'当前格式子集不支持'};
const formats=new Set(['text/plain','text/markdown','application/pdf','application/vnd.openxmlformats-officedocument.wordprocessingml.document']);
const canPreview=computed(()=>!!caps.value&&!!assetId.value&&!busy.value);
function cancel(){epoch++;pending?.abort();pending=undefined;busy.value=false}
watch(()=>props.asset,value=>{cancel();result.value=undefined;error.value=undefined;assetId.value=value?.asset_id??''},{flush:'sync'});
watch(assetId,()=>{cancel();result.value=undefined;error.value=undefined},{flush:'sync'});
watch(()=>[s.client,s.productId],async()=>{
 cancel();capabilityRequest?.abort();caps.value=undefined;result.value=undefined;error.value=undefined;const id=++capabilityEpoch;const client=s.client,product=s.productId;const controller=new AbortController();capabilityRequest=controller;
 try{const value=await client.call('getDocumentCapabilities',{query:{product_id:product}},controller.signal);if(capabilityEpoch===id&&client===s.client&&product===s.productId)caps.value=value}catch(e){if(capabilityEpoch===id&&!controller.signal.aborted)error.value=e}finally{if(capabilityEpoch===id)capabilityRequest=undefined}
},{immediate:true,flush:'sync'});
async function preview(){
 if(!canPreview.value)return;cancel();const id=++epoch;const client=s.client,product=s.productId,asset=assetId.value;const controller=new AbortController();pending=controller;busy.value=true;result.value=undefined;error.value=undefined;
 const live=()=>epoch===id&&s.client===client&&s.productId===product&&assetId.value===asset&&!controller.signal.aborted;
 try{const record=await client.call('getContentAsset',{path:{asset_id:asset},query:{product_id:product}},controller.signal);if(!live())return;if(record.state!=='READY'||record.purpose!=='KNOWLEDGE'||!formats.has(record.media_type))throw Error('请选择已上传完成的知识文件');if(BigInt(record.size_bytes)>BigInt(caps.value!.limits.input_bytes))throw Error('文档超过当前提取上限');
 const value=await client.call('previewContentDocument',{path:{asset_id:asset},query:{product_id:product},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:product,expected_sha256:record.sha256}},controller.signal);if(live())result.value=value
 }catch(e){if(live())error.value=e}finally{if(epoch===id){busy.value=false;pending=undefined}}
}
onBeforeUnmount(()=>{cancel();capabilityEpoch++;capabilityRequest?.abort()});
</script>

<template>
 <section id="document-preview" class="panel" aria-labelledby="document-title">
  <h2 id="document-title">文档提取预览</h2>
  <p>先上传知识文件，再检查文字、来源位置和缺失。文档内容仅作为资料显示。</p>
  <template v-if="caps"><p class="muted" data-testid="document-limits">TXT / Markdown / DOCX / 文字PDF；原文件最多 {{caps.limits.input_bytes/1024/1024}} MiB、PDF最多 {{caps.limits.pages}} 页、输出最多 {{caps.limits.output_bytes/1024}} KiB、完整响应编码后最多 {{caps.limits.encoded_response_bytes/1024/1024}} MiB、{{caps.limits.chunks}} 段。仅本地处理，最长 {{caps.limits.timeout_ms/1000}} 秒，不调用模型。</p><details><summary>格式支持范围</summary><ul><li v-for="line in caps.supported_subset" :key="line">{{line}}</li></ul><p>扫描件需要OCR，当前未启用。复杂字体、图片与其他未读取部件会明确列为缺失；文字预览不保证原版式。</p></details></template>
  <p v-else>请先取得当前格式能力与限额。</p>
  <form @submit.prevent="preview"><label>知识文件标识<input v-model="assetId" data-testid="document-asset-id" autocomplete="off" required/></label><button class="primary" :disabled="!canPreview" data-testid="document-preview">{{busy?'正在提取':'提取有权文档'}}</button><button v-if="busy" type="button" @click="cancel">取消预览</button></form>
  <FieldErrors :error="error"/>
  <article v-if="result" data-testid="document-result" aria-live="polite">
   <h3>{{labels[result.report.status]}}</h3><dl><dt>来源文件</dt><dd>{{result.asset.name}}</dd><dt>原始SHA-256</dt><dd class="break">{{result.report.source_sha256}}</dd><dt>解析版本</dt><dd>{{result.report.parser_version}}</dd><dt>可见范围</dt><dd>{{result.asset.audience_policy}}</dd></dl>
   <ul v-if="result.report.missing.length" data-testid="document-missing"><li v-for="(gap,index) in result.report.missing" :key="index">{{gap.location}} · {{gap.code}}</li></ul>
   <section v-for="(chunk,index) in result.report.chunks" :key="index"><h4>{{chunk.location}}<span v-if="chunk.section"> · {{chunk.section}}</span></h4><pre class="preview" style="white-space:pre-wrap;overflow-wrap:anywhere">{{chunk.text}}</pre></section>
   <p>预览尚未保存为知识。字段映射、来源确认和保存草稿将在后续接通；当前不会自动采用或发布。</p>
  </article>
 </section>
</template>
