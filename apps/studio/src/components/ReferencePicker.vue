<script setup lang="ts">
import type {ContentObject,DefinitionRef} from '@loreviva/sdk-ts';
const props=defineProps<{modelValue:DefinitionRef;objects:ContentObject[];label:string;id:string}>();
const emit=defineEmits<{'update:modelValue':[DefinitionRef]}>();
function patch(key:string,value:string){emit('update:modelValue',{...props.modelValue,[key]:value} as DefinitionRef)}
// Switching reference kind starts a fresh shape so local and pinned fields are not mixed. These events edit local form data; they do not resolve or save a definition.
function branch(kind:string){emit('update:modelValue',kind==='LOCAL_DRAFT'?{kind,object_id:''}:{kind:'PINNED_DEFINITION',release_id:'',object_id:'',version:'',content_hash:''})}
</script>
<template><fieldset class="reference"><legend>{{label}}</legend><label :for="id+'-kind'">引用来源</label><select :id="id+'-kind'" :value="modelValue.kind" @change="branch(($event.target as HTMLSelectElement).value)"><option value="LOCAL_DRAFT">本地草稿对象</option><option value="PINNED_DEFINITION">固定发布版本</option></select><template v-if="modelValue.kind==='LOCAL_DRAFT'"><label :for="id">对象</label><select :id="id" :value="modelValue.object_id" @change="patch('object_id',($event.target as HTMLSelectElement).value)"><option value="">请选择</option><option v-for="o in objects" :key="o.object_id" :value="o.object_id">{{o.name}} · {{o.kind}}</option></select></template><template v-else><label v-for="field in ['release_id','object_id','version','content_hash']" :key="field">{{field}}<input :id="id+'-'+field" :value="(modelValue as unknown as Record<string,string>)[field]" @input="patch(field,($event.target as HTMLInputElement).value)"></label></template></fieldset></template>
