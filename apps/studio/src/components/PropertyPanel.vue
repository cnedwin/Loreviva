<script setup lang="ts">
import {useProjectStore} from '../stores/project';
import DefinitionForm from './DefinitionForm.vue';
const s=useProjectStore();
</script>
<template><section class="panel properties"><h2>对象属性</h2><DefinitionForm v-if="s.editable&&s.selectedObject" :model-value="s.selectedObject" :objects="s.editable.objects" :index="s.editable.objects.findIndex(o=>o.object_id===s.selectedId)" @update:model-value="s.editable!.objects[s.editable!.objects.findIndex(o=>o.object_id===s.selectedId)]=$event"/><p v-else>从对象树或舞台选择一个对象。</p></section></template>
