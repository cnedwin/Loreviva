<script setup lang="ts">
import {onMounted,ref} from 'vue';
import {useRouter} from 'vue-router';
import {useProjectStore} from '../stores/project';
import type {RootContentKind} from '@loreviva/sdk-ts';
import FieldErrors from '../components/FieldErrors.vue';
const s=useProjectStore();const router=useRouter();const error=ref<unknown>();const kind=ref<RootContentKind>('WORLD');const project=ref('');
async function refresh(){try{await s.loadProject(project.value||undefined);error.value=undefined}catch(e){error.value=e}}
// Confirm loss of dirty local edits before switching or creating drafts. Delegate navigation/session ownership to the project store; routing to the editor does not save a draft or recover an old command.
async function open(id:string){if(s.dirty&&!window.confirm('切换草稿会放弃尚未保存的修改，继续吗？'))return;try{await s.loadDraft(id);await router.push('/editor')}catch(e){error.value=e}}
function create(){if(s.dirty&&!window.confirm('创建草稿会放弃尚未保存的修改，继续吗？'))return;s.newDraft(kind.value);router.push('/editor')}
onMounted(refresh);
</script>
<template><div class="page-heading"><div><p class="eyebrow">你的世界，从一个设定开始</p><h1>工作空间</h1><p>整理世界、角色与岗位；普通编辑无需调用模型。</p></div><button class="quiet" @click="refresh">刷新</button></div><FieldErrors :error="error"/><section class="panel toolbar"><label>新建内容<select v-model="kind"><option v-for="k in ['WORLD','CHARACTER','ROLE','SKILL','KNOWLEDGE','PRESENTATION_THEME']" :key="k">{{k}}</option></select></label><button class="primary" @click="create">开始创作</button><label>项目筛选<input v-model="project" placeholder="全部已有项目"></label><button @click="refresh">筛选</button></section><div class="draft-grid"><article v-for="draft in s.drafts" :key="draft.draft_id" class="panel draft-card"><span class="badge">{{draft.root_kind}}</span><h2>{{draft.input.name}}</h2><p>{{draft.input.description||'还没有内容简介'}}</p><dl><dt>修订</dt><dd>{{draft.draft_revision}}</dd><dt>状态</dt><dd>{{draft.state}}</dd><dt>项目</dt><dd>{{draft.project_id||'独立内容'}}</dd></dl><button @click="open(draft.draft_id)">打开草稿 →</button></article></div><p v-if="!s.drafts.length&&!error" class="empty">此范围尚无草稿。可以创建内容，或检查已登记产品范围。</p></template>
