<script setup lang="ts">
import type {RehearsalReportArtifact,RehearsalRequest} from '@loreviva/sdk-ts';
defineProps<{report:RehearsalReportArtifact;original?:RehearsalRequest}>();
</script>
<template><section class="panel"><h3>隔离 Mock 试演报告</h3><p>这是原任务的历史报告，不是对白回放；其中费用状态不代表当前最新账务。</p><table><thead><tr><th>顺序</th><th>原输入</th><th>执行状态</th><th>供应商核对</th></tr></thead><tbody><tr v-for="row in report.inputs" :key="row.index"><td>{{row.index+1}}</td><td>{{original?.inputs[row.index]?.text??'原输入上下文未保留，仅展示安全结果'}}</td><td>{{row.state}}</td><td>{{row.supplier_reconciliation_state}}</td></tr></tbody></table><h4>断言（按原顺序）</h4><ol><li v-for="(assertion,index) in report.assertions" :key="index">{{assertion.kind}} · <strong>{{assertion.state}}</strong></li></ol></section></template>
