<script setup lang="ts">
import {computed,onBeforeUnmount,onMounted,ref,watch} from 'vue';
import {createReferenceState} from './state.ts';
import type {MediaRef,RuntimeMediaRef} from '../../../packages/sdk-ts/src/generated/contracts.ts';

const owner=createReferenceState(),d=owner.data;
const login=ref(''),credential=ref(''),actor=ref(''),message=ref(''),reason=ref(''),replacement=ref('');
const product=ref(''),release=ref(''),originalCreateKey=ref(''),knownTask=ref(''),knownTurn=ref('');
const audio=ref<HTMLDivElement>(),alert=ref<HTMLElement>(),loginField=ref<HTMLInputElement>();
const selectedMemory=ref(''),connected=computed(()=>Boolean(d.session?.client)),working=computed(()=>d.busy>0);
type Item=Record<string,unknown>;
const text=(value:unknown)=>typeof value==='string'?value:'';
const rows=(value:unknown):Item[]=>Array.isArray(value)?value.filter(x=>x&&typeof x==='object'):[];
const actors=computed(()=>rows(d.snapshot?.actors));
const memory=computed(()=>d.memories.find(m=>m.memory_id===selectedMemory.value));
const capabilities=computed(()=>d.capabilities?.operations as Record<string,string>|undefined);
const enabled=(op:string)=>connected.value&&!working.value&&capabilities.value?.[op]==='AVAILABLE';
const packetText=(packet:Item)=>rows(packet.text_segments).map(s=>text(s.text)).join('\n');
const mediaRefs=(packet:Item)=>rows(packet.media) as unknown as (MediaRef|RuntimeMediaRef)[];
async function invoke(action:()=>Promise<unknown>){
 try{await action()}catch{d.error='操作未完成，请重新检查当前状态。'}
}
async function signIn(){
 const secret=credential.value;credential.value='';
 await invoke(()=>owner.login(login.value,secret));
 if(d.session)await invoke(()=>owner.capabilities());
}
async function connect(){
 await invoke(()=>owner.exchange());
 if(d.session?.client){await invoke(()=>owner.capabilities());await invoke(()=>owner.selfConsent())}
}
async function signOut(){await invoke(()=>owner.logout());loginField.value?.focus()}
function clearForm(){
 actor.value='';message.value='';reason.value='';replacement.value='';selectedMemory.value='';
 originalCreateKey.value='';knownTask.value='';knownTurn.value='';credential.value='';
}
watch(()=>d.session,clearForm);
watch(()=>d.viewGeneration,clearForm);
watch(()=>d.error,async value=>{if(value){await Promise.resolve();alert.value?.focus()}});
watch(actors,value=>{if(!value.some(a=>a.actor_instance_id===actor.value))actor.value=''});
const hide=()=>{if(document.visibilityState==='hidden')void owner.stopMedia()};
const leave=()=>{void owner.close()};
onMounted(()=>{if(audio.value)owner.attachMedia(()=>{const sink=document.createElement('audio');sink.preload='none';audio.value!.appendChild(sink);return sink});document.addEventListener('visibilitychange',hide);window.addEventListener('pagehide',leave)});
onBeforeUnmount(()=>{document.removeEventListener('visibilitychange',hide);window.removeEventListener('pagehide',leave);clearForm();void owner.close()});
</script>

<template>
 <main>
  <header><p class="eyebrow">LOREVIVA · REFERENCE PLAYER</p><h1>进入一个持续生长的世界</h1>
   <p>当前参考流程使用合成内容与 Mock 服务。任务受理、内容交付、音频和客户入库分别显示。</p>
  </header>
  <p role="status" aria-live="polite" class="notice">{{d.notice}}</p>
  <p v-if="d.error" ref="alert" role="alert" tabindex="-1" class="error">{{d.error}}</p>
  <section aria-labelledby="identity-title">
   <h2 id="identity-title">身份与会话</h2>
   <form v-if="!d.session" @submit.prevent="signIn">
    <label for="login">登录名称</label><input id="login" ref="loginField" v-model="login" autocomplete="username" required maxlength="128">
    <label for="credential">登录凭据</label><input id="credential" v-model="credential" type="password" autocomplete="current-password" required maxlength="4096">
    <button type="submit" :disabled="working">登录</button>
   </form>
   <div v-else>
    <p>当前本人：<strong data-testid="current-player">{{d.session.identity[1]}}</strong> · 世界：{{d.session.identity[2]}}</p>
    <p>{{connected?'本人会话已连接':'尚未连接本人会话'}} · 合同视图：{{d.profile}}</p>
    <div class="actions"><button :disabled="working" @click="connect">连接本人会话</button><button @click="signOut">退出并清除私人视图</button></div>
   </div>
  </section>
  <template v-if="d.session">
   <section aria-labelledby="consent-title">
    <h2 id="consent-title">本人的许可</h2>
    <button :disabled="!enabled('getPlayerSelfConsent')" @click="invoke(()=>owner.selfConsent())">读取当前许可</button>
    <p v-if="d.consent">状态：{{d.consent.state}} · 当前使用：{{d.consent.current_use_enabled?'已启用':'未启用'}} · 修订：{{d.consent.consent_revision}}</p>
    <p>仅处理本人的许可。撤回使用后，必要的纠正与删除流程仍需独立核验。</p>
    <div class="actions">
     <button :disabled="!enabled('commandPlayerSelfConsent')||d.consent?.state!=='NOT_GRANTED'" @click="invoke(()=>owner.consent('INITIAL'))">首次同意</button>
     <button :disabled="!enabled('commandPlayerSelfConsent')||!d.consent" @click="invoke(()=>owner.consent('RENEW'))">续期许可</button>
     <button :disabled="!enabled('commandPlayerSelfConsent')||!d.consent" @click="invoke(()=>owner.consent('REVOKE'))">撤回使用许可</button>
    </div>
   </section>
   <section aria-labelledby="world-title">
    <h2 id="world-title">当前世界</h2>
    <button :disabled="!enabled('getSnapshot')" @click="invoke(()=>owner.snapshot())">读取当前场景</button>
    <p v-if="d.snapshot">运行状态：{{d.snapshot.instance_state}} · 世界修订：{{(d.snapshot.versions as Item).world_revision}}</p>
    <div class="actions">
     <button :disabled="!enabled('controlInstance')||!d.snapshot" @click="invoke(()=>owner.control('START'))">开始世界</button>
     <button :disabled="!enabled('controlInstance')||!d.snapshot" @click="invoke(()=>owner.control('PAUSE'))">暂停世界</button>
     <button :disabled="!enabled('controlInstance')||!d.snapshot" @click="invoke(()=>owner.control('RESUME'))">恢复世界</button>
     <button :disabled="!enabled('controlInstance')||!d.snapshot" @click="invoke(()=>owner.control('END'))">结束世界</button>
    </div>
    <form @submit.prevent="invoke(()=>owner.turn(actor,message))">
     <label for="actor">当前可见角色</label>
     <select id="actor" v-model="actor" required><option value="" disabled>请选择</option><option v-for="a in actors" :key="text(a.actor_instance_id)" :value="text(a.actor_instance_id)">{{a.actor_instance_id}}</option></select>
     <label for="message">对角色说</label><textarea id="message" v-model="message" required maxlength="8000" rows="3"></textarea>
     <div class="actions"><button type="button" :disabled="!enabled('createQuote')||!actor" @click="invoke(()=>owner.quote(actor))">取得本次报价</button>
      <button type="submit" :disabled="!enabled('submitTurn')||!d.quote||!actor">发送一次</button></div>
     <p v-if="d.quote">本次报价：{{d.quote.quote_id}}。结果未知时只查询原请求，不自动重新发送。</p>
    </form>
    <details><summary>创建新的私人实例</summary>
     <form @submit.prevent="invoke(()=>owner.create(product,release,originalCreateKey))">
      <label for="product">产品标识</label><input id="product" v-model="product" required>
      <label for="release">已发布版本</label><input id="release" v-model="release" required>
      <label for="create-key">本次创建原请求键</label><input id="create-key" v-model="originalCreateKey" required pattern="[A-Za-z0-9._-]{1,128}">
      <button :disabled="!enabled('createInstance')">创建一次</button>
     </form><p>新实例不会改变当前登录配置所绑定的世界。</p>
    </details>
   </section>
   <section aria-labelledby="tasks-title">
    <h2 id="tasks-title">任务与原请求</h2>
    <p v-if="d.task" data-testid="task-state">任务 {{d.task.task_id}} · {{d.task.task_kind}} · {{d.task.task_state}}</p>
    <p v-if="d.task?.task_kind==='MEMORY_GOVERNANCE'">治理已受理不等于清理完成。必要隐私清理继续，不提供取消按钮。</p>
    <button v-if="d.task?.task_kind==='TURN'" :disabled="!enabled('cancelTask')" @click="invoke(()=>owner.cancelTask())">请求取消该任务</button>
    <form @submit.prevent="invoke(()=>owner.task(knownTask))"><label for="task">已知原任务标识</label><input id="task" v-model="knownTask" :placeholder="text(d.task?.task_id)">
     <button :disabled="!enabled('getTask')||d.taskAccess!=='CLIENT'||(!knownTask&&!d.task)">查询原任务</button></form>
    <p v-if="d.taskAccess!=='CLIENT'&&d.task">该原任务只能读取已保留的原请求观察；实例快照不证明该命令已完成。</p>
    <button v-if="d.taskAccess!=='CLIENT'&&d.task" :disabled="!enabled('getSnapshot')" @click="invoke(()=>owner.snapshot())">观察当前实例</button>
    <form @submit.prevent="invoke(()=>owner.turnResult(knownTurn))"><label for="turn">已知回合标识</label><input id="turn" v-model="knownTurn" required><button :disabled="!enabled('getTurn')">读取回合结果</button></form>
    <ul class="originals"><li v-for="o in d.originals" :key="o.id">{{o.operation}} · <strong>{{o.state}}</strong> · 已读 {{o.queries}} 次
      <button :disabled="working" @click="invoke(()=>owner.recover(o.id))">只读查询原请求</button></li></ul>
    <p>每轮最多读取 20 次，间隔至少 1 秒。没有可信原关联时保持 UNKNOWN，页面不会重发业务请求。</p>
   </section>
   <section aria-labelledby="delivery-title">
    <h2 id="delivery-title">内容交付与音频</h2>
    <div class="actions">
     <button :disabled="!enabled('getEvents')||!d.cursor" @click="invoke(()=>owner.events())">读取下一页</button>
     <button :disabled="!enabled('streamEvents')||!d.cursor||d.streaming" @click="invoke(()=>owner.stream())">接收持续更新</button>
     <button :disabled="!d.streaming" @click="invoke(()=>owner.stopStream())">停止更新</button>
    </div>
    <p data-testid="delivery-state">{{d.streaming?'正在接收更新':'持续更新已停止'}} · 本页交付 {{d.deliveries.length}} 项</p>
    <article v-for="p in d.packets" :key="text(p.packet_id)" class="packet"><h3>内容 {{p.packet_id}}</h3><p class="body-text">{{packetText(p)}}</p>
     <div v-for="(m,i) in mediaRefs(p)" :key="i"><span>音频状态：{{m.media_state}}</span>
      <button :disabled="!enabled('readRuntimeMedia')||m.media_state!=='READY'" @click="invoke(()=>owner.play(m))">重新授权并播放</button></div>
    </article>
    <button @click="invoke(()=>owner.stopMedia())">停止播放并释放音频</button>
    <p>每次播放重新取得当前授权。可播放不代表任务完成；已下载到客户设备的内容不能保证远程抹除。</p>
   </section>
   <section aria-labelledby="memory-title">
    <h2 id="memory-title">本人的记忆</h2>
    <button :disabled="!enabled('listMemories')" @click="invoke(()=>owner.memories())">读取记忆列表</button>
    <label for="memory">选择记忆</label><select id="memory" v-model="selectedMemory"><option value="">请选择</option><option v-for="m in d.memories" :key="text(m.memory_id)" :value="text(m.memory_id)">{{m.memory_id}} · {{m.validity}}</option></select>
    <div class="actions"><button :disabled="!enabled('getMemory')||!memory" @click="invoke(()=>owner.memory(selectedMemory))">读取安全信息</button>
     <button :disabled="!enabled('getMemory')||!memory" @click="invoke(()=>owner.memory(selectedMemory,true))">申请读取正文与来源</button></div>
    <p v-if="d.memoryBody" class="body-text" data-testid="memory-body">{{d.memoryBody.content||'未返回正文；标签不授予正文使用权限。'}}</p>
    <label for="reason">纠正或删除原因</label><input id="reason" v-model="reason" maxlength="500">
    <label for="replacement">纠正后的内容</label><textarea id="replacement" v-model="replacement" maxlength="8000" rows="3"></textarea>
    <div class="actions"><button :disabled="!enabled('governMemories')||!memory||memory.target_subject_id!==d.session.identity[1]||!replacement" @click="invoke(()=>owner.govern(memory!,'CORRECT',reason,replacement))">纠正本人记忆</button>
     <button :disabled="!enabled('governMemories')||!memory||memory.target_subject_id!==d.session.identity[1]" @click="invoke(()=>owner.govern(memory!,'DELETE',reason))">删除本人记忆</button></div>
   </section>
   <section aria-labelledby="customer-title">
    <h2 id="customer-title">客户入库与平台确认</h2>
    <button :disabled="!connected||working" @click="invoke(()=>owner.consumeCustomer())">执行一次客户接收</button>
    <p v-if="d.customer" data-testid="customer-state">实际返回：{{d.customer.State}} · 持久化 {{d.customer.Persisted}} · 已确认 {{d.customer.Confirmed}}</p>
    <p>只显示客户接收程序返回的状态与数量，不据数量构造消息或宣称业务成功。</p>
   </section>
  </template>
  <div ref="audio" data-testid="media-host"></div>
 </main>
</template>
