<script setup lang="ts">
import {ref} from 'vue';
import {useProjectStore} from '../stores/project';
import {createCommandRunner} from '../lib/commands';
import type {PublishCheckReport,Release} from '@loreviva/sdk-ts';
import FieldErrors from './FieldErrors.vue';
const s=useProjectStore();const report=ref<PublishCheckReport>();const release=ref<Release>();const error=ref<unknown>();const note=ref('');const runner=createCommandRunner(s.client);
// A check report is not a release. Publish sends the saved revision and report identity for server validation; creating the immutable content release neither creates nor starts a runtime instance.
async function check(){try{s.assertImpactReady();if(!s.basis||s.dirty)throw Error('请先保存或取消修改');report.value=await runner.run('checkDraftForPublish',{path:{draft_id:s.basis.draft_id},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:s.productId,expected_draft_revision:s.basis.draft_revision,requested_support_labels:['MOCK_DEMO']}});error.value=undefined}catch(e){error.value=e}}
async function publish(){try{s.assertImpactReady();if(!s.basis||s.dirty||!report.value?.passed)throw Error('需要当前已保存修订的通过检查');release.value=await runner.run('publishContentDraft',{path:{draft_id:s.basis.draft_id},headers:{'Idempotency-Key':crypto.randomUUID()},body:{product_id:s.productId,expected_draft_revision:s.basis.draft_revision,check_report_id:report.value.report_id,approval_note:note.value,requested_support_labels:report.value.support_labels,rehearsal_report_ids:[]}});error.value=undefined}catch(e){error.value=e}}
</script>
<template><section class="panel"><h2>发布检查</h2><p>发布会创建不可变内容版本；不会创建或启动运行实例。试演报告内部结果标识不能代替发布引用。</p><FieldErrors :error="error"/><button :disabled="s.impactBlocked||s.dirty||!s.basis" @click="check">检查当前修订</button><section v-if="report"><p>{{report.passed?'检查通过':'检查未通过'}} · 修订 {{report.draft_revision}} · 有效至 {{report.expires_at}}</p><FieldErrors :findings="report.findings"/><label>发布说明<textarea v-model="note" rows="2"/></label><button class="primary" :disabled="s.impactBlocked||!report.passed||s.dirty" @click="publish">发布不可变版本</button></section><section v-if="release" role="status"><h3>发布已成立</h3><p>{{release.release_id}} · 修订 {{release.draft_revision}}</p><RouterLink to="/runtime">到运行中心单独创建与启动实例 →</RouterLink></section></section></template>
