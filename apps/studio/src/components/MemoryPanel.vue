<script setup lang="ts">
import {ref} from 'vue';
import {useProjectStore} from '../stores/project';
import type {AdminActorMetadata,AdminMemoryMetadataV1,Memory} from '@loreviva/sdk-ts';
import FieldErrors from './FieldErrors.vue';
const p=defineProps<{instanceId:string}>();const s=useProjectStore();const actors=ref<AdminActorMetadata[]>([]);const actor=ref('');const subject=ref('');const metadata=ref<AdminMemoryMetadataV1[]>([]);const body=ref<Memory>();const error=ref<unknown>();
async function loadActors(){try{const result=await s.client.call('listAdminActorInstances',{path:{instance_id:p.instanceId},query:{product_id:s.productId}});actors.value=result.items;error.value=undefined}catch(e){error.value=e}}
// Metadata and body are separate requests. Clear the previous body before either selection/read path; a listed record does not grant body access, which the server checks for the requested subject and actor.
async function list(){try{body.value=undefined;metadata.value=(await s.client.call('listAdminMemoryMetadata',{path:{instance_id:p.instanceId},query:{product_id:s.productId,data_subject_id:subject.value,actor_instance_id:actor.value}})).items;error.value=undefined}catch(e){error.value=e}}
async function read(id:string){try{body.value=undefined;body.value=await s.client.call('getAdminMemoryBody',{path:{instance_id:p.instanceId,memory_id:id},query:{product_id:s.productId,data_subject_id:subject.value,actor_instance_id:actor.value}});error.value=undefined}catch(e){error.value=e}}
</script>
<template><details><summary>当前实例的记忆权限视图</summary><p>元数据与正文分开取权。世界参与、来源、本人用途和当前动作权限仍分别检查。</p><button @click="loadActors">读取已登记演员</button><label>演员<select v-model="actor"><option value="">请选择</option><option v-for="a in actors" :key="a.actor_instance_id" :value="a.actor_instance_id">{{a.display_name}} · {{a.initial_slot_id}}</option></select></label><label>资料主体标识<input v-model="subject"></label><button :disabled="!actor||!subject" @click="list">读取有权元数据</button><FieldErrors :error="error"/><ul><li v-for="item in metadata" :key="item.memory_id">{{item.memory_kind}} · 修订 {{item.revision}} · {{item.blocked?'已阻断':'未阻断'}} <button @click="read(item.memory_id)">独立请求正文</button></li></ul><article v-if="body" class="panel"><p>{{body.content}}</p><p class="muted">{{body.validity}} · {{body.allowed_uses.join('、')}}</p></article></details></template>
