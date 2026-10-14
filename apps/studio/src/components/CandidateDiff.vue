<script setup lang="ts">
import {computed,ref,watch} from 'vue';
import {useProjectStore} from '../stores/project';
import {useTasksStore} from '../stores/tasks';
import FieldErrors from './FieldErrors.vue';
const p=defineProps<{taskId:string}>();const s=useProjectStore();const tasks=useTasksStore();const selected=ref<string[]>([]);const error=ref<unknown>();const message=ref('');
const original=computed(()=>tasks.candidateContexts[p.taskId]);const artifact=computed(()=>tasks.candidates[p.taskId]);const current=computed(()=>s.editable?.objects.find(o=>o.object_id===original.value?.request.object_id));
watch(artifact,()=>{selected.value=Object.keys(artifact.value?.fields??{})},{immediate:true});
function adopt(){try{s.applyCandidate(p.taskId,selected.value);message.value='已采用到本地草稿，仍需保存。';error.value=undefined}catch(e){error.value=e}}
</script>
<template><section class="panel"><h3>Mock 候选对比</h3><p>当前合成 Mock 回显原字段，不代表真实模型质量改善。采用只改本地草稿。</p><FieldErrors :error="error"/><p v-if="message" role="status">{{message}}</p><table v-if="artifact&&original"><thead><tr><th>采用</th><th>字段</th><th>原已保存值</th><th>候选值</th><th>当前本地值</th></tr></thead><tbody><tr v-for="(value,field) in artifact.fields" :key="field"><td><input v-model="selected" type="checkbox" :value="field" :aria-label="'采用 '+field"></td><th>{{field}}</th><td><pre>{{(original.object as unknown as Record<string,unknown>)[field]}}</pre></td><td><pre>{{value}}</pre></td><td><pre>{{(current as unknown as Record<string,unknown>)?.[field]}}</pre></td></tr></tbody></table><p v-else>没有已验证的候选正文或可信原受理上下文，不能采用。</p><button class="primary" :disabled="!artifact||!original||!selected.length" @click="adopt">采用选中字段</button></section></template>
