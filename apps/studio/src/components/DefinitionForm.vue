<script setup lang="ts">
import {computed} from 'vue';
import type {ContentObject} from '@loreviva/sdk-ts';
import {schemas,kindSchemas} from '../lib/validation';
import SchemaField from './SchemaField.vue';
const props=defineProps<{modelValue:ContentObject;objects:ContentObject[];index?:number}>();
const emit=defineEmits<{'update:modelValue':[ContentObject]}>();
const schema=computed(()=>schemas[kindSchemas[props.modelValue.kind]]);
const ordinary=new Set(['name','description','tags','background','biography','values','long_term_goals','interests','inner_conflicts','expression_style','example_lines','boundaries','relationship_policy','text','position','coordinate_system','visibility','reason','instruction_template']);
function change(key:string,value:unknown){emit('update:modelValue',{...props.modelValue,[key]:value} as ContentObject)}
</script>
<template><div class="definition-form"><p class="eyebrow">{{modelValue.kind}} · {{modelValue.object_id}}</p><template v-for="(field,key) in schema.properties" :key="key"><SchemaField v-if="ordinary.has(key)" :schema="field" :field="key" :path="'/objects/'+(index??0)+'/'+key" :model-value="(modelValue as unknown as Record<string,unknown>)[key]" :objects="objects" @update:model-value="change(key,$event)"/></template><details><summary>引用、岗位与高级设置</summary><template v-for="(field,key) in schema.properties" :key="key"><SchemaField v-if="!ordinary.has(key)" :schema="field" :field="key" :path="'/objects/'+(index??0)+'/'+key" :model-value="(modelValue as unknown as Record<string,unknown>)[key]" :objects="objects" :readonly="['object_id','version','kind','builtin_original'].includes(key)" @update:model-value="change(key,$event)"/></template></details></div></template>
