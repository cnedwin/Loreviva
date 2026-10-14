<script setup lang="ts">
import {computed,nextTick} from 'vue';
import type {FieldError} from '@loreviva/sdk-ts';
const props=defineProps<{error?:unknown;findings?:FieldError[]}>();
const body=computed(()=>((props.error as {body?:{message?:string;field_errors?:FieldError[]}})?.body));
const items=computed(()=>props.findings??body.value?.field_errors??[]);
const message=computed(()=>body.value?.message??(props.error instanceof Error?props.error.message:props.error?'请求失败，请保留本地修改并重试。':''));
async function focus(pointer:string){const path=pointer.replace(/^(?:\/body)?\/input(?=\/)/,'');window.dispatchEvent(new CustomEvent('studio-field-focus',{detail:path}));await nextTick();const target=[...document.querySelectorAll<HTMLElement>('[data-pointer]')].find(e=>e.dataset.pointer===path);if(!target)return;let parent=target.parentElement;while(parent){if(parent instanceof HTMLDetailsElement)parent.open=true;parent=parent.parentElement}const control=target.matches('input,textarea,select')?target:target.querySelector<HTMLElement>('input,textarea,select,button');control?.focus();target.scrollIntoView({block:'center',behavior:'smooth'})}
</script>
<template><div v-if="message||items.length" role="alert" class="error field-errors"><p v-if="message">{{message}}</p><p v-if="items.length">共 {{items.length}} 条检查结果。滚动查看，选择一项定位字段。</p><ul v-if="items.length" aria-label="字段检查结果"><li v-for="(f,i) in items" :key="i"><button type="button" class="quiet" @click="focus(f.pointer)">{{f.message}} · {{f.pointer}}</button></li></ul></div></template>
<style scoped>.field-errors ul{max-height:18rem;overflow:auto;overscroll-behavior:contain}.field-errors button{text-align:left;overflow-wrap:anywhere}</style>
