<script setup lang="ts">
import {ref,watch,onMounted,onBeforeUnmount} from 'vue';
import {useRouter} from 'vue-router';
import {AdminTransportError} from '@loreviva/sdk-ts';
import {useProjectStore} from './stores/project';
import FieldErrors from './components/FieldErrors.vue';
import LocalNotifications from './components/LocalNotifications.vue';
const s=useProjectStore();const router=useRouter();const connected=ref(false);const error=ref<unknown>();let attempt=0;
watch(()=>s.client,()=>{attempt++;connected.value=false;error.value=undefined},{flush:'sync'});
// Try settings-only entry after a denied content-capability read. Accept a response only for the captured connection attempt, client, tenant and product; this UI routing does not grant server authority.
async function connect(){
 const version=++attempt;const active=s.client;const product=s.productId;const tenant=s.tenantId;connected.value=false;error.value=undefined;
 const current=()=>version===attempt&&active===s.client&&product===s.productId&&tenant===s.tenantId;
 const denied=(e:unknown)=>e instanceof AdminTransportError&&(e.status===401||e.status===403);
 try{
  try{await active.call('getCreatorCapabilities',{query:{product_id:product}});if(current())connected.value=true;return}catch(e){if(!denied(e))throw e}
  for(const scope of ['PRODUCT','TENANT'] as const){
   if(!current())return;
   try{await active.call('getAdminSettings',{query:{scope_type:scope,tenant_id:tenant,...(scope==='PRODUCT'?{product_id:product}:{})}});if(!current())return;s.settingsScope=scope;await router.replace('/settings');if(current())connected.value=true;return}catch(e){if(scope==='TENANT'||!denied(e))throw e}
  }
 }catch(e){if(current())error.value=e}
}
function unload(e:BeforeUnloadEvent){if(s.dirty){e.preventDefault();e.returnValue=''}}
onMounted(()=>window.addEventListener('beforeunload',unload));onBeforeUnmount(()=>window.removeEventListener('beforeunload',unload));
const navigation=[['/','工作空间','01'],['/editor','世界编辑','02'],['/resources','内容与资源','03'],['/runtime','运行中心','04'],['/settings','设置','05']];
</script>
<template><a class="skip-link" href="#main">跳至主要内容</a><div class="shell"><aside class="sidebar"><RouterLink class="brand" to="/">Loreviva<span>WORLD STUDIO</span></RouterLink><p class="sidebar-note">创造世界，<br>让它持续生长。</p><nav aria-label="Studio 导航"><RouterLink v-for="[path,title,n] in navigation" :key="path" :to="path"><span>{{n}}</span>{{title}}</RouterLink></nav><div class="sidebar-footer"><span class="status-dot"/> 本地创作 · R0<br><small>当前仅合成 Mock</small></div></aside><div class="main-shell"><header class="topbar"><span>{{s.productId||'尚未选择产品'}}</span><span v-if="s.dirty" class="badge warm">有未保存修改</span><button v-if="connected" class="quiet" @click="s.signOut();connected=false">退出凭据</button></header><main id="main"><section v-if="!connected" class="panel login"><p class="eyebrow">连接本地世界引擎</p><h1>进入你的创作空间</h1><p>使用已登记的操作员凭据。凭据仅保留在本页内存，刷新后重新输入。</p><form @submit.prevent="connect"><label>租户标识<input v-model="s.tenantId" required autocomplete="off"></label><label>产品标识<input v-model="s.productId" required autocomplete="off"></label><label>操作员凭据<input v-model="s.bearer" type="password" required autocomplete="off" spellcheck="false"></label><button class="primary">连接</button></form><FieldErrors :error="error"/></section><template v-else><RouterView/><LocalNotifications/></template></main></div></div></template>
