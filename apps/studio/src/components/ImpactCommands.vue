<script setup lang="ts">
import {computed,ref,watch} from 'vue';
import {useProjectStore} from '../stores/project';
import type {DraftAction} from '@loreviva/sdk-ts';
import FieldErrors from './FieldErrors.vue';
import DraftDiff from './DraftDiff.vue';
const s=useProjectStore();const action=ref<DraftAction>(s.impact?.check.body.action??'COPY');const name=ref('副本');const restoreRevision=ref('');const bindingId=ref(s.impact?.check.body.action==='REMOVE_BINDING'?s.impact.check.body.object_ids[0]??'':'');const error=ref<unknown>();
const report=computed(()=>s.impactReportCurrent?s.impact?.report:undefined);
const stageLabel=computed(()=>s.impact?({CHECKING:'正在检查',CHECK_UNKNOWN:'检查结果未知',CHECKED:'检查已返回',COMMANDING:'正在提交',COMMAND_UNKNOWN:'提交结果未知',READBACK_PENDING:'已提交，等待读回',DONE:'已完成',DELETED_BUFFER:'已删除，保留未保存内容',REJECTED:'请求被拒绝'}[s.impact.stage]):'');
watch([action,bindingId,()=>s.selectedId],()=>s.invalidateImpactCheck(),{flush:'sync'});
watch(()=>s.client,()=>{error.value=undefined;name.value='副本';restoreRevision.value='';bindingId.value=''});
// Execute only the current impact report after explicit confirmation. Recovery stays in the existing impact flow; choosing server or local content is a separate, explicit reconciliation decision.
async function check(){try{await s.checkImpact(action.value,action.value==='REMOVE_BINDING'?[bindingId.value]:[s.selectedId]);error.value=undefined}catch(e){error.value=e}}
async function execute(){try{if(!report.value)throw Error('需要当前已保存修订的影响报告');if(!window.confirm('执行 '+report.value.action+'？请确认已阅读影响与排除范围。'))return;await s.executeImpact({copyName:name.value,restoreRevision:restoreRevision.value});error.value=undefined}catch(e){error.value=e}}
async function recover(){try{await s.recoverImpact();error.value=undefined}catch(e){error.value=e}}
function server(){if(window.confirm('放弃当前本地修改并打开此操作的服务器内容？'))s.useImpactServer()}
function local(){if(window.confirm('保留全部本地内容可能重新加入刚删除的绑定，或覆盖刚提交的内容。确认已对照差异并选择本地版本？'))s.keepImpactLocal()}
</script>
<template>
 <section v-if="s.impact" class="panel" aria-label="原影响操作状态">
  <p class="break">原操作 {{s.impact.check.body.action}} · 草稿 {{s.impact.draft.draft_id}} · {{stageLabel}}</p>
  <FieldErrors :error="s.impact.error"/>
  <button v-if="['CHECK_UNKNOWN','COMMAND_UNKNOWN'].includes(s.impact.stage)" :disabled="s.impact.busy" @click="recover">恢复原影响操作</button>
  <button v-if="s.impact.stage==='READBACK_PENDING'" :disabled="s.impact.busy" @click="recover">重新读取已提交内容</button>
  <p v-if="s.impact.result" role="status">操作已提交。<span v-if="'draft'in s.impact.result">副本 {{s.impact.result.draft.draft_id}}</span><span v-else>修订 {{s.impact.result.draft_revision}}</span></p>
  <template v-if="s.impactBlocked"><p role="alert">请先恢复原影响操作或明确重整内容；保存、候选、试演与发布暂不可用。</p>
   <template v-if="s.impact.review&&s.impact.server&&s.editable"><h3>服务器已提交，本地仍有不同内容</h3><p>保留本地版本可能重新加入已删除的绑定。请对照差异后明确选择，不会自动合并数组。</p><DraftDiff :before="s.impact.server.input" :after="s.editable"/><button @click="server">放弃本地修改并采用操作后的服务器内容</button><button @click="local">已对照差异，明确保留本地版本</button></template>
  </template>
   <template v-if="s.impact.stage==='DELETED_BUFFER'"><p>原草稿已删除。这里只保留未保存的恢复内容，不能保存为原草稿，也不会自动创建。</p><DraftDiff v-if="s.impact.recoveryInput" :before="s.impact.sent" :after="s.impact.recoveryInput"/><button @click="s.discardDeletedImpact()">放弃已删除草稿的本地恢复内容</button></template>
  <button v-if="s.impact.server&&!s.impactBlocked&&s.impact.server.draft_id!==s.basis?.draft_id" @click="server">放弃当前本地修改并打开操作结果</button>
 </section>
 <details class="panel" aria-label="内容影响操作"><summary>复制、归档与删除影响</summary><p>复制只包含内容；运行身份、玩家状态、私人记忆、费用与凭据不会复制。删除内容不等于删除运行或私人资料。</p><div class="toolbar"><label>操作<select v-model="action"><option v-for="value in ['COPY','ARCHIVE','RESTORE','DELETE','REMOVE_BINDING']" :key="value">{{value}}</option></select></label><label v-if="action==='COPY'">副本名称<input v-model="name"></label><label v-if="action==='RESTORE'">目标修订<input v-model="restoreRevision" inputmode="numeric"></label><label v-if="action==='REMOVE_BINDING'">绑定标识<input v-model="bindingId"></label><button :disabled="s.dirty||!s.basis||s.impactBlocked||s.impact?.busy" @click="check">检查当前影响</button></div><FieldErrors :error="error"/><section v-if="report"><h3>{{report.action}} · 修订 {{report.draft_revision}}</h3><p>有效至 {{report.expires_at}}</p><ul><li v-for="target in report.selected_targets" :key="JSON.stringify(target)">{{target}}</li></ul><h4>入引用</h4><ul><li v-for="(incoming,i) in report.incoming_refs" :key="i">{{incoming}}</li></ul><h4>职责覆盖</h4><ul><li v-for="d in report.duty_coverage" :key="JSON.stringify(d)">{{d}}</li></ul><p>涉及发布：{{report.affected_release_ids.join('、')||'无'}}</p><FieldErrors :findings="report.blocking_findings"/><button :disabled="!!report.blocking_findings.length||s.dirty||s.impact?.busy" @click="execute">确认执行 {{report.action}}</button></section></details>
</template>
