<script setup lang="ts">
import {ref,onMounted} from 'vue';
import {useProjectStore} from '../stores/project';
import type {StudioSettingsSnapshot} from '@loreviva/sdk-ts';
import FieldErrors from '../components/FieldErrors.vue';
import SettingsSections from '../components/SettingsSections.vue';
import MemberGrants from '../components/MemberGrants.vue';
import SelfConsent from '../components/SelfConsent.vue';
const s=useProjectStore();const settings=ref<StudioSettingsSnapshot>();const kind=ref<'TENANT'|'PRODUCT'>(s.settingsScope);const error=ref<unknown>();
let readVersion=0;
// Clear the previous snapshot before reading the selected scope; TENANT sends no product ID. Accept success or failure only while this read, client and scope are still current.
async function refresh(){const version=++readVersion;const active=s.client;const scope=kind.value;settings.value=undefined;error.value=undefined;try{const value=await active.call('getAdminSettings',{query:{scope_type:scope,tenant_id:s.tenantId,...(scope==='PRODUCT'?{product_id:s.productId}:{})}});if(version!==readVersion||active!==s.client||scope!==kind.value)return;settings.value=value}catch(e){if(version===readVersion&&active===s.client&&scope===kind.value)error.value=e}}
onMounted(refresh);
</script>
<template><div class="page-heading"><div><p class="eyebrow">明确范围，独立保存</p><h1>设置</h1><p>租户与产品配置分别生效；私人生成许可属于本人。</p></div><label>范围<select v-model="kind" @change="refresh"><option value="PRODUCT">当前产品</option><option value="TENANT">当前租户</option></select></label></div><FieldErrors :error="error"/><button @click="refresh">重新读取（丢弃未保存设置）</button><template v-if="settings"><SettingsSections :key="JSON.stringify(settings)" :snapshot="settings"/><MemberGrants :key="JSON.stringify(settings.basis.scope)" :scope="settings.basis.scope"/><section class="panel"><h2>安装与存储</h2><dl><template v-for="(value,key) in settings.installation_storage" :key="key"><dt>{{key}}</dt><dd>{{value??'未知'}}</dd></template></dl><p>未知容量不显示为无限可用。</p></section></template><SelfConsent/><section class="panel future"><h2>后续维护能力</h2><p>备份、恢复、完整迁移及运维入口由 Task11 接通；本页不提供预制成功结果。</p></section></template>
