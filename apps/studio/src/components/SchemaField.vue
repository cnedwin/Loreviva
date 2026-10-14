<script setup lang="ts">
import {computed} from 'vue';
import type {ContentObject,DefinitionRef} from '@loreviva/sdk-ts';
import {resolve,emptyValue,label as fieldLabel,type FormSchema} from '../lib/validation';
import ReferencePicker from './ReferencePicker.vue';
const props=withDefaults(defineProps<{modelValue:unknown;schema:FormSchema;field:string;path:string;objects?:ContentObject[];readonly?:boolean}>(),{objects:()=>[]});
const emit=defineEmits<{'update:modelValue':[unknown]}>();
const base=computed(()=>resolve(props.schema));
const variants=computed(()=>base.value.oneOf??base.value.anyOf??[]);
// Schema changes seed a new local value for the selected variant; field and item edits copy the current siblings. Emitting these values does not validate or persist a document.
const branchIndex=computed(()=>{const current=props.modelValue as Record<string,unknown>|undefined;const index=variants.value.findIndex(s=>{const r=resolve(s);return Object.entries(r.properties??{}).some(([k,v])=>v.const!==undefined&&v.const===current?.[k])});return index<0?0:index});
const s=computed(()=>variants.value.length?resolve(variants.value[branchIndex.value]):base.value);
const id=computed(()=>'field-'+props.path.replaceAll('/','-'));
const title=computed(()=>fieldLabel(props.field));
const record=computed(()=>props.modelValue as Record<string,unknown>??{});
const items=computed(()=>Array.isArray(props.modelValue)?props.modelValue:[]);
const multiline=computed(()=>['description','background','biography','instruction_template','text','reason','expression_style','effect'].includes(props.field)||((s.value.maxLength??0)>300));
function set(value:unknown){emit('update:modelValue',value)}
function child(key:string,value:unknown){const next={...record.value};if(value===undefined)delete next[key];else next[key]=value;set(next)}
function item(index:number,value:unknown){const next=[...items.value];next[index]=value;set(next)}
function remove(index:number){set(items.value.filter((_,i)=>i!==index))}
</script>
<template>
 <ReferencePicker v-if="schema.$ref?.endsWith('/DefinitionRef')&&modelValue" :id="id" :label="title" :model-value="modelValue as DefinitionRef" :objects="objects" @update:model-value="set"/>
 <fieldset v-else-if="s.type==='object'" :disabled="readonly" :data-pointer="path"><legend>{{title}}</legend>
  <label v-if="variants.length">类型<select :value="branchIndex" @change="set(emptyValue(variants[Number(($event.target as HTMLSelectElement).value)]))"><option v-for="(variant,index) in variants" :key="index" :value="index">{{Object.values(resolve(variant).properties??{}).find(v=>v.const!==undefined)?.const??index+1}}</option></select></label>
  <template v-for="(childSchema,key) in s.properties" :key="key"><SchemaField v-if="record[key]!==undefined||s.required?.includes(key)" :model-value="record[key]" :schema="childSchema" :field="key" :path="path+'/'+key" :objects="objects" :readonly="readonly||childSchema.const!==undefined" @update:model-value="child(key,$event)"/><button v-else type="button" class="quiet" @click="child(key,emptyValue(childSchema))">添加{{fieldLabel(key)}}</button><button v-if="record[key]!==undefined&&!s.required?.includes(key)&&!readonly" type="button" class="quiet" @click="child(key,undefined)">移除{{fieldLabel(key)}}</button></template>
 </fieldset>
 <fieldset v-else-if="s.type==='array'" :disabled="readonly" :data-pointer="path"><legend>{{title}}</legend><p v-if="field==='tags'" class="muted">分类标签用于整理内容，不授予权限。</p><div v-for="(value,index) in items" :key="index" class="array-item"><SchemaField :model-value="value" :schema="s.items??{}" :field="field+' '+(index+1)" :path="path+'/'+index" :objects="objects" :readonly="readonly" @update:model-value="item(index,$event)"/><button type="button" class="quiet" :aria-label="'移除'+title+'第'+(index+1)+'项'" @click="remove(index)">移除</button></div><button v-if="!readonly" type="button" class="quiet" :disabled="s.maxItems!==undefined&&items.length>=s.maxItems" @click="set([...items,emptyValue(s.items??{})])">添加{{title}}</button></fieldset>
 <label v-else class="field" :for="id" :data-pointer="path"><span>{{title}}</span><select v-if="s.enum" :id="id" :data-field="field" :value="modelValue" :disabled="readonly" @change="set(($event.target as HTMLSelectElement).value)"><option v-for="value in s.enum" :key="String(value)" :value="String(value)">{{value}}</option></select><input v-else-if="s.type==='boolean'" :id="id" type="checkbox" :checked="!!modelValue" :disabled="readonly" @change="set(($event.target as HTMLInputElement).checked)"><input v-else-if="s.type==='number'||s.type==='integer'" :id="id" :data-field="field" type="number" :step="s.type==='integer'?1:'any'" :min="s.minimum" :max="s.maximum" :value="modelValue" :readonly="readonly" @input="set(($event.target as HTMLInputElement).valueAsNumber)"><textarea v-else-if="multiline" :id="id" :data-field="field" :value="modelValue as string" :maxlength="s.maxLength" :readonly="readonly" rows="4" @input="set(($event.target as HTMLTextAreaElement).value)"/><input v-else :id="id" :data-field="field" :value="modelValue" :maxlength="s.maxLength" :readonly="readonly||s.const!==undefined" @input="set(($event.target as HTMLInputElement).value)"></label>
</template>
