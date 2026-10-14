<script setup lang="ts">
import {computed} from 'vue';
import type {DraftInput} from '@loreviva/sdk-ts';
const p=defineProps<{before:DraftInput;after:DraftInput}>();
const changes=computed(()=>{const lines:Array<{field:string;before:unknown;after:unknown}>=[];function visit(a:unknown,b:unknown,path:string){if(JSON.stringify(a)===JSON.stringify(b))return;if(a&&b&&typeof a==='object'&&typeof b==='object'&&!Array.isArray(a)&&!Array.isArray(b)){for(const key of new Set([...Object.keys(a),...Object.keys(b)]))visit((a as Record<string,unknown>)[key],(b as Record<string,unknown>)[key],path+'/'+key)}else lines.push({field:path,before:a,after:b})}visit(p.before,p.after,'');return lines});
</script>
<template><details class="panel"><summary>未保存差异 · {{changes.length}} 项</summary><table><thead><tr><th>字段</th><th>保存时</th><th>当前本地</th></tr></thead><tbody><tr v-for="change in changes" :key="change.field"><th>{{change.field}}</th><td><pre>{{JSON.stringify(change.before,null,2)}}</pre></td><td><pre>{{JSON.stringify(change.after,null,2)}}</pre></td></tr></tbody></table></details></template>
