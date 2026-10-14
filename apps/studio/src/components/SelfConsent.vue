<script setup lang="ts">
import {onMounted,ref} from 'vue';
import {useProjectStore} from '../stores/project';
import {createCommandRunner} from '../lib/commands';
import {recoverSettingsCommand} from '../lib/settings';
import type {StudioSelfConsentSnapshot,StudioSelfConsentCommandRequest} from '@loreviva/sdk-ts';
import FieldErrors from './FieldErrors.vue';
const s=useProjectStore();const state=ref<StudioSelfConsentSnapshot>();const accepted=ref(false);const error=ref<unknown>();const message=ref('');const runner=createCommandRunner(s.client);
async function refresh(){try{state.value=await s.client.call('getAdminSelfConsent',{query:{scope_type:'PRODUCT',tenant_id:s.tenantId,product_id:s.productId}});accepted.value=false;error.value=undefined}catch(e){error.value=e}}
// Require the separate confirmation for INITIAL or RENEW, but not REVOKE. Submit the displayed self-consent revision and recover the original receipt rather than treating recovery as another renewal.
async function command(kind:'INITIAL'|'RENEW'|'REVOKE'){try{if(!state.value)throw Error('先读取本人许可');if(kind!=='REVOKE'&&!accepted.value)throw Error('请独立确认本人许可');const body:StudioSelfConsentCommandRequest=kind==='REVOKE'?{kind,scope:state.value.scope,expected_revision:state.value.consent_revision,policy_id:state.value.policy_id}:{kind,scope:state.value.scope,expected_revision:state.value.consent_revision,policy_id:state.value.policy_id,accepted:true};const receipt=await runner.run('commandAdminSelfConsent',{headers:{'Idempotency-Key':crypto.randomUUID()},body});message.value='本人许可操作已提交，修订 '+receipt.result_revision;await refresh()}catch(e){error.value=e}}
async function recover(){try{await recoverSettingsCommand(s.client);message.value='已恢复原回执';await refresh()}catch(e){error.value=e}}
onMounted(refresh);
</script>
<template><section class="panel"><h2>本人的私人生成许可</h2><p>仅对当前本人、当前产品生效，不可由成员授权代替。启用会允许本人的 PRIVATE_RESPONSE 生成用途，最长期限为服务器当前时间起 30 天；已有来源与主体许可仍分别校验。</p><FieldErrors :error="error"/><p v-if="message" role="status">{{message}}</p><button @click="refresh">读取本人当前许可</button><template v-if="state"><dl><dt>本人</dt><dd>{{state.subject_id}}</dd><dt>状态</dt><dd>{{state.state}}</dd><dt>当前实际用途</dt><dd>{{state.current_use_enabled?'启用':'未启用'}}</dd><dt>到期</dt><dd>{{state.expires_at??'尚未同意'}}</dd><dt>首次/最近同意</dt><dd>{{state.accepted_at??'无'}}</dd><dt>修订</dt><dd>{{state.consent_revision}}</dd></dl><label><input v-model="accepted" type="checkbox">我确认上述本人、用途与期限；当前 Mock 不代表真实模型或收费已启用。</label><div class="actions"><button class="primary" :disabled="!accepted" @click="command(state.state==='NOT_GRANTED'?'INITIAL':'RENEW')">{{state.state==='NOT_GRANTED'?'首次同意':'续期本人许可'}}</button><button @click="command('REVOKE')">撤回本人许可</button><button @click="accepted=false">取消本地勾选</button></div></template><button v-if="runner.pending.value?.operation==='commandAdminSelfConsent'" @click="recover">查原本人许可回执</button></section></template>
