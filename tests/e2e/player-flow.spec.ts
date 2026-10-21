import assert from 'node:assert/strict';
import {test,type Browser} from '@playwright/test';
import {appendFile} from 'node:fs/promises';
import {dirname,resolve} from 'node:path';
import {validateContract} from '../../packages/sdk-ts/src/index.ts';

// The owned runner pins the already installed browser; no download or shared
// user profile is used. Playwright creates and closes its own fresh profile.
test.use({channel:'chrome',launchOptions:{args:['--enable-automation']}});

export interface ReferenceBrowserInput {
 readonly origin:string;readonly login:string;readonly credential:string;
 readonly world:string;readonly turn:string;readonly player:string;
 readonly fresh:Readonly<{login:string;player:string;world:string;release:string;actor:string;memory:string}>;
}

/** Real loopback scenario. Its caller owns the real Go platform and both DBs;
 * there is deliberately no page.route/response stub or SQL recovery here. */
export async function runReferencePlayerRealLoopback(browser:Browser,input:ReferenceBrowserInput,collect:(values:string[])=>void){
 assert.equal(input.origin,'http://127.0.0.1:27185');
 const native=await browser.newBrowserCDPSession();
 const command=await native.send('Browser.getBrowserCommandLine');
 const profileArgument=command.arguments.find((value:string)=>value.startsWith('--user-data-dir='));
 assert.ok(profileArgument,'fresh Playwright profile argument');
 const profile=resolve(profileArgument.slice('--user-data-dir='.length));
 assert.equal(dirname(profile),resolve(process.env.TMP??''));
 assert.match(profile,/playwright_chromiumdev_profile-/);
 const processes=await native.send('SystemInfo.getProcessInfo');
 const browserProcess=processes.processInfo.find(value=>value.type==='browser');assert.ok(browserProcess);
 console.log(JSON.stringify({format:'lv-reference-owned-browser-v1',pid:browserProcess.id,profile,browserVersion:browser.version(),newProfile:true}));
 await native.detach();
 const context=await browser.newContext({viewport:{width:390,height:844},serviceWorkers:'block'});
 const page=await context.newPage();page.setDefaultTimeout(15000);
 const failures:string[]=[];const responseJobs:Promise<void>[]=[];let currentCSRF='';
 page.on('pageerror',()=>failures.push('browser-script-error'));
 page.on('response',response=>{
  if(!response.url().startsWith(input.origin+'/bff/'))return;
  responseJobs.push((async()=>{
   if(response.headers()['content-type']?.includes('application/json')){
    const value=await response.json().catch(()=>null);
    if(value&&typeof value.csrf==='string'){currentCSRF=value.csrf;collect([value.csrf])}
   }
  })());
 });
 try{
  const navigation=await page.goto(input.origin,{waitUntil:'networkidle'});
  assert.equal(navigation?.status(),200);
  assert.equal(navigation?.headers()['cache-control'],'no-store');
  await page.getByLabel('登录名称',{exact:true}).fill(input.login);
  await page.getByLabel('登录凭据',{exact:true}).fill(input.credential);
  await page.getByRole('button',{name:'登录',exact:true}).click();
  await page.getByTestId('current-player').waitFor();
  assert.equal(await page.getByTestId('current-player').textContent(),input.player);
  collect((await context.cookies()).map(c=>c.value));
  const cookies=await context.cookies();
  assert.equal(cookies.length,1);assert.equal(cookies[0]?.httpOnly,true);assert.equal(cookies[0]?.sameSite,'Strict');
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();
  await page.getByText('本人会话已连接',{exact:false}).waitFor();
  await Promise.all(responseJobs);assert.ok(currentCSRF);
  const wrongCSRF=await page.evaluate(async()=>{
   const response=await fetch('/bff/customer-consume',{method:'POST',credentials:'same-origin',
    headers:{'Content-Type':'application/json','X-Loreviva-Csrf':'wrong-synthetic-csrf'},
    body:JSON.stringify({format:'lv-bff-consume-v1'})});
   return response.status;
  });
  assert.equal(wrongCSRF,403,'actual page cookie with wrong CSRF refuses');
  // APIRequestContext uses this isolated browser's cookie jar; it can set an
  // Origin that page Fetch deliberately forbids. This is a real HTTP negative.
  const wrongOrigin=await context.request.post(input.origin+'/bff/customer-consume',{
   headers:{Origin:'http://foreign.invalid','X-Loreviva-Csrf':currentCSRF},
   data:{format:'lv-bff-consume-v1'}});
  assert.equal(wrongOrigin.status(),403);await wrongOrigin.dispose();
  await page.getByRole('button',{name:'读取当前场景',exact:true}).click();
  await page.getByText('运行状态：',{exact:false}).waitFor();
  assert.equal(await page.locator('[role="alert"]').count(),0);
  const actors=await page.locator('#actor option:not([disabled])').count();
  assert.ok(actors>0,'real current public actor presence');
  await page.getByLabel('已知回合标识',{exact:true}).fill(input.turn);
  await page.getByRole('button',{name:'读取回合结果',exact:true}).click();
  await page.locator('.packet, [role="alert"]').first().waitFor();
  assert.equal(await page.locator('[role="alert"]').count(),0,'actual getTurn must reach approved packet display');
  await page.locator('.packet').first().waitFor();
  assert.ok((await page.locator('.packet .body-text').first().innerText()).length>0,'actual approved bytes reached Vue');
  await page.getByRole('button',{name:'执行一次客户接收',exact:true}).click();
  await page.getByTestId('customer-state').waitFor();
  assert.match(await page.getByTestId('customer-state').innerText(),/CONFIRMED/);
  const memoryResponsePromise=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/client/listMemories'));
  await page.getByRole('button',{name:'读取记忆列表',exact:true}).click();
  const memoryResponse=await memoryResponsePromise;
  const memoryPage=await memoryResponse.json();
  console.log(JSON.stringify({stage:'real-memory-list',status:memoryResponse.status(),code:typeof memoryPage.code==='string'?memoryPage.code:undefined,count:Array.isArray(memoryPage.memories)?memoryPage.memories.length:null}));
  assert.equal(memoryResponse.status(),200,'real metadata-only list must succeed');
  assert.ok(Array.isArray(memoryPage.memories)&&memoryPage.memories.length>0,'real CREATE initial memory prerequisite');
  await page.waitForFunction(()=>document.querySelectorAll('#memory option').length>1);
  const memoryID=await page.locator('#memory option').nth(1).getAttribute('value');assert.ok(memoryID);
  await page.getByLabel('选择记忆',{exact:true}).selectOption(memoryID);
  await page.getByRole('button',{name:'读取安全信息',exact:true}).click();
  await page.getByTestId('memory-body').waitFor();
  assert.match(await page.getByTestId('memory-body').innerText(),/未返回正文/);
  assert.equal(await page.evaluate(()=>document.cookie),'','HttpOnly authentication never exposed to script');
  assert.equal(await page.evaluate(()=>localStorage.length+sessionStorage.length),0);
  const overflow=await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1);
  if(overflow)console.log(JSON.stringify({stage:'layout-overflow',elements:await page.evaluate(()=>Array.from(document.querySelectorAll('main *')).map(e=>{const r=e.getBoundingClientRect();return {tag:e.tagName,id:e.id,class:e.className,width:r.width,left:r.left,right:r.right,scrollWidth:e.scrollWidth,clientWidth:e.clientWidth}}).filter(e=>e.right>innerWidth+1||e.left<0||e.scrollWidth>e.clientWidth+1).slice(0,20))}));
  assert.equal(overflow,false,'390px real layout must not overflow');
  await page.getByRole('button',{name:'读取当前许可',exact:true}).focus();
  await page.keyboard.press('Tab');
  assert.equal(await page.evaluate(()=>document.activeElement?.tagName),'BUTTON','actual keyboard focus');
  await page.getByRole('button',{name:'退出并清除私人视图',exact:true}).click();
  await page.getByLabel('登录名称',{exact:true}).waitFor();
  assert.equal(await page.locator('.packet').count(),0);
  assert.equal(await page.getByTestId('memory-body').count(),0);
  assert.equal(await page.evaluate(()=>document.activeElement?.id),'login');
  // A distinct, genuinely registered person/world has no prior positive use.
  // Browser actions use the ordinary page and original BFF/platform ports.
  await page.getByLabel('登录名称',{exact:true}).fill(input.fresh.login);
  await page.getByLabel('登录凭据',{exact:true}).fill(input.credential);
  await page.getByRole('button',{name:'登录',exact:true}).click();
  await page.getByTestId('current-player').waitFor();
  assert.equal(await page.getByTestId('current-player').innerText(),input.fresh.player);
  assert.equal(await page.locator('.packet').count(),0,'switch cannot retain former player content');
  collect((await context.cookies()).map(c=>c.value));
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();
  await page.getByText('本人会话已连接',{exact:false}).waitFor();
  const readUI=async(label:string,operation:string)=>{
   const response=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/client/'+operation));
   await page.getByRole('button',{name:label,exact:true}).click();
   const r=await response;assert.equal(r.status(),200,'real '+operation);const body=await r.json();
   await page.waitForFunction(()=>!Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='连接本人会话')?.disabled);return body;
  };
  const commandUI=async(label:string,operation:string)=>{
   const response=page.waitForResponse(r=>r.url()===input.origin+'/bff/commands'&&r.request().postDataJSON()?.operation===operation);
   await page.getByRole('button',{name:label,exact:true}).click();
   const r=await response;assert.ok(r.status()>=200&&r.status()<300,'real '+operation+' status '+r.status());
   const body=await r.json();await page.waitForFunction(()=>!Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='连接本人会话')?.disabled);return body;
  };
  const finishTask=async(id:string)=>{
   let last:any;
   for(let attempt=0;attempt<15;attempt++){
    await page.getByLabel('已知原任务标识',{exact:true}).fill(id);last=await readUI('查询原任务','getTask');
    if(last.task.task_state==='COMMITTED')return last;
    assert.ok(['ACCEPTED','RUNNING'].includes(last.task.task_state),'real task terminal '+last.task.task_state);
    await page.waitForTimeout(1000);
   }throw Error('Owned task did not commit within bounded UI reads');
  };
  const prior=await readUI('读取当前许可','getPlayerSelfConsent');assert.equal(prior.state,'NOT_GRANTED');
  await commandUI('首次同意','commandPlayerSelfConsent');
  let snapshot=await readUI('读取当前场景','getSnapshot');assert.equal(snapshot.instance_state,'CREATED');
  // The Go transport wrapper discards only a genuine successful 202.
  // A snapshot observes the instance; it cannot resolve this original.
  const lostControl=async(label:string)=>{
   const before=await page.locator('.originals li').filter({hasText:'controlInstance'}).count();
   const response=page.waitForResponse(r=>r.url()===input.origin+'/bff/commands'&&r.request().postDataJSON()?.operation==='controlInstance');
   await page.getByRole('button',{name:label,exact:true}).click();assert.equal((await response).status(),502);
   await page.waitForFunction(()=>!Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='连接本人会话')?.disabled);
   const rows=page.locator('.originals li').filter({hasText:'controlInstance'});
   assert.equal(await rows.count(),before+1);const original=rows.last();assert.match(await original.innerText(),/UNKNOWN/);
   assert.equal(await page.getByRole('button',{name:'查询原任务',exact:true}).isDisabled(),true);
   const query=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/query-original/')&&r.request().method()==='POST');
   await original.getByRole('button',{name:'只读查询原请求',exact:true}).click();
   const result=await query;assert.equal(result.status(),200);assert.equal((await result.json()).state,'UNKNOWN');
   await page.waitForFunction(()=>!Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='连接本人会话')?.disabled,null,{timeout:30000});
   assert.match(await original.innerText(),/UNKNOWN/);return original;
  };
  const lostStart=await lostControl('开始世界');
  // START belongs to SERVICE. The CLIENT observes its world transition,
  // without borrowing SERVICE task-read authority.
  for(let attempt=0;attempt<15;attempt++){
   snapshot=await readUI('读取当前场景','getSnapshot');
   if(snapshot.instance_state==='RUNNING')break;
   assert.equal(snapshot.instance_state,'CREATED');await page.waitForTimeout(1000);
  }
  assert.equal(snapshot.instance_state,'RUNNING');
  assert.match(await lostStart.innerText(),/UNKNOWN/,'snapshot cannot claim command completion');
  // An explicit new current Session resets the local view, not the durable
  // original journal. No original command is replayed or marked resolved.
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();await page.getByText('本人会话已连接',{exact:false}).waitFor();
  snapshot=await readUI('读取当前场景','getSnapshot');
  snapshot=await readUI('读取当前场景','getSnapshot');
  await page.getByLabel('当前可见角色',{exact:true}).selectOption(input.fresh.actor);
  await page.getByLabel('对角色说',{exact:true}).fill('Pass the sign and remember my request.');
  await commandUI('取得本次报价','createQuote');
  const intake=await commandUI('发送一次','submitTurn');
  const turnTask=intake.acceptance?.task??intake.task;assert.ok(turnTask?.task_id,'real intake task identity');
  await finishTask(turnTask.task_id);
  // A committed TURN cannot be cancelled. This is the seventh real broker
  // write, with its original conflict outcome rather than a fabricated success.
  const cancelled=page.waitForResponse(r=>r.url()===input.origin+'/bff/commands'&&r.request().postDataJSON()?.operation==='cancelTask');
  await page.getByRole('button',{name:'请求取消该任务',exact:true}).click();
  assert.equal((await cancelled).status(),409);
  const original=page.locator('.originals li').filter({hasText:'submitTurn'});assert.equal(await original.count(),1);
  const recovered=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/query-original/')&&r.request().method()==='POST');
  await original.getByRole('button',{name:'只读查询原请求',exact:true}).click();
  const recoveredResponse=await recovered;assert.equal(recoveredResponse.status(),200);
  const recoveredOriginal=await recoveredResponse.json();assert.match(recoveredOriginal.original_id,/^[a-f0-9]{32}$/);
  await page.getByLabel('已知回合标识',{exact:true}).fill(turnTask.task_id);
  let turn=await readUI('读取回合结果','getTurn');assert.equal(turn.approved_packets.length,1);
  await page.locator('.packet').first().waitFor();
  for(let attempt=0;attempt<15&&!turn.approved_packets[0].media.some((m:any)=>m.media_state==='READY');attempt++){
   await page.waitForTimeout(1000);turn=await readUI('读取回合结果','getTurn');
  }
  assert.ok(turn.approved_packets[0].media.some((m:any)=>m.media_state==='READY'),'actual synthetic media producer reached READY');
  const wavResponse=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/client/readRuntimeMedia'));
  await page.getByRole('button',{name:'重新授权并播放',exact:true}).first().click();
  const wav=await wavResponse;assert.equal(wav.status(),200);assert.equal((await wav.body()).length,160044);
  await page.waitForFunction(()=>document.querySelector('audio')?.src.startsWith('blob:'));
  // Retain A's actual native target, install B, then dispatch A's queued
  // native event. It must never release B's private Blob URL.
  await page.evaluate(()=>{(window as any).__ownedOldAudio=document.querySelector('audio')});
  const secondWav=page.waitForResponse(r=>r.url().startsWith(input.origin+'/bff/client/readRuntimeMedia'));
  await page.getByRole('button',{name:'重新授权并播放',exact:true}).first().click();assert.equal((await secondWav).status(),200);
  await page.waitForFunction(()=>document.querySelector('audio')?.src.startsWith('blob:')&&document.querySelector('audio')!==(window as any).__ownedOldAudio);
  const currentUrl=await page.locator('audio').getAttribute('src');assert.ok(currentUrl);
  await page.evaluate(()=>{for(const kind of ['ended','pause','error'])(window as any).__ownedOldAudio.dispatchEvent(new Event(kind));delete (window as any).__ownedOldAudio});
  assert.equal(await page.locator('audio').getAttribute('src'),currentUrl);
  await page.evaluate(()=>document.querySelector('audio')!.dispatchEvent(new Event('error')));
  await page.waitForFunction(()=>!document.querySelector('audio')?.getAttribute('src'));
  await page.getByRole('button',{name:'停止播放并释放音频',exact:true}).click();

  snapshot=await readUI('读取当前场景','getSnapshot');
  await page.getByRole('button',{name:'接收持续更新',exact:true}).click();
  await page.getByText('正在接收更新',{exact:false}).waitFor();
  const pause=await commandUI('暂停世界','controlInstance');assert.equal(await page.getByRole('button',{name:'查询原任务',exact:true}).isDisabled(),false);await finishTask(pause.task_id);
  await page.getByRole('button',{name:'停止更新',exact:true}).click();
  await page.getByText('持续更新已停止',{exact:false}).waitFor();
  snapshot=await readUI('读取当前场景','getSnapshot');assert.equal(snapshot.instance_state,'PAUSED');
  const resumed=await commandUI('恢复世界','controlInstance');assert.equal(await page.getByRole('button',{name:'查询原任务',exact:true}).isDisabled(),false);await finishTask(resumed.task_id);
  snapshot=await readUI('读取当前场景','getSnapshot');assert.equal(snapshot.instance_state,'RUNNING');
  await page.getByText('创建新的私人实例',{exact:true}).click();
  await page.getByLabel('产品标识',{exact:true}).fill('coffee');
  await page.getByLabel('已发布版本',{exact:true}).fill(input.fresh.release);
  await page.getByLabel('本次创建原请求键',{exact:true}).fill('browser-explicit-create');
  const created=await commandUI('创建一次','createInstance');assert.notEqual(created.scope.world_instance_id,input.fresh.world);
  assert.equal(await page.getByTestId('current-player').innerText(),input.fresh.player);
  // The original current-use qualification is still valid at this point.
  const consumed=page.waitForResponse(r=>r.url()===input.origin+'/bff/customer-consume');
  await page.getByRole('button',{name:'执行一次客户接收',exact:true}).click();
  const consumedBody=await (await consumed).json();assert.equal(consumedBody.State,'CONFIRMED');
  assert.equal(consumedBody.Persisted,4);assert.equal(consumedBody.Confirmed,4);
  // Actual committed TURN input and Mock ActorBelief remain visible under
  // their original receipt/producer proof and current permissions.
  const afterTurn=await readUI('读取记忆列表','listMemories');validateContract('PlayerMemoryMetadataPage',afterTurn);
  assert.ok(afterTurn.memories.length>0);assert.ok(afterTurn.memories.every((item:any)=>item.body_readable===true));
  let actualInput=false,actualDerived=false;
  for(const item of afterTurn.memories.filter((item:any)=>item.validity==='ACTIVE')){
   await page.getByLabel('选择记忆',{exact:true}).selectOption(item.memory_id);
   const value=await readUI('申请读取正文与来源','getMemory');validateContract('MemoryProvenanceV1',value);
   assert.ok(value.memory.content.length>0);
   if(value.memory.content===JSON.stringify({kind:'TEXT',text:'Pass the sign and remember my request.'}))actualInput=true;
   if(value.evidence_domain==='ACTOR_BELIEF'&&value.parent_memory_ids.length>0)actualDerived=true;
  }
  assert.equal(actualInput,true,'real TURN input body present');
  assert.equal(actualDerived,true,'real committed Mock derived memory present');
  // After the real TURN, current initial/input/derived proof supports CORRECT
  // and a new Session plus genuine RENEW without inventing retained history.
  await commandUI('续期许可','commandPlayerSelfConsent');
  await readUI('读取记忆列表','listMemories');
  await page.getByLabel('选择记忆',{exact:true}).selectOption(input.fresh.memory);
  await page.getByLabel('纠正或删除原因',{exact:true}).fill('本人更正');
  await page.getByLabel('纠正后的内容',{exact:true}).fill('The player prefers a quiet table.');
  const corrected=await commandUI('纠正本人记忆','governMemories');await finishTask(corrected.task_id);
  assert.equal(await page.getByRole('button',{name:'请求取消该任务',exact:true}).count(),0);
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();await page.getByText('本人会话已连接',{exact:false}).waitFor();
  await readUI('读取当前许可','getPlayerSelfConsent');
  await commandUI('续期许可','commandPlayerSelfConsent');
  const correctedPage=await readUI('读取记忆列表','listMemories');validateContract('PlayerMemoryMetadataPage',correctedPage);
  assert.ok(correctedPage.memories.every((item:any)=>item.body_readable===true),'each source-defined metadata item carries the page eligibility');
  const active=correctedPage.memories.filter((m:any)=>m.validity==='ACTIVE');assert.ok(active.length>1);
  let replacementSeen=false,unchangedSeen=false;
  for(const item of active){
   await page.getByLabel('选择记忆',{exact:true}).selectOption(item.memory_id);
   const value=await readUI('申请读取正文与来源','getMemory');validateContract('MemoryProvenanceV1',value);assert.equal(value.projection,'provenance_v1');
   assert.ok(value.memory.content.length>0);
   if(value.memory.content==='The player prefers a quiet table.')replacementSeen=true;else unchangedSeen=true;
  }
  assert.equal(replacementSeen&&unchangedSeen,true,'real corrected and unchanged bodies remain readable after RENEW');
  await readUI('读取当前许可','getPlayerSelfConsent');
  // END's snapshot prerequisite still requires a current positive choice.
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();await page.getByText('本人会话已连接',{exact:false}).waitFor();
  await readUI('读取当前场景','getSnapshot');
  await lostControl('结束世界');
  await commandUI('撤回使用许可','commandPlayerSelfConsent');
  const remaining=await readUI('读取记忆列表','listMemories');assert.ok(remaining.memories.length>0);
  const ownMemory=remaining.memories.find((m:any)=>m.target_subject_id===input.fresh.player&&m.validity==='ACTIVE');assert.ok(ownMemory);
  await page.getByLabel('选择记忆',{exact:true}).selectOption(ownMemory.memory_id);
  await page.getByLabel('纠正或删除原因',{exact:true}).fill('本人删除');
  const removed=await commandUI('删除本人记忆','governMemories');await finishTask(removed.task_id);
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();await page.getByText('本人会话已连接',{exact:false}).waitFor();
  const denied=page.waitForResponse(r=>r.url()===input.origin+'/bff/customer-consume');
  await page.getByRole('button',{name:'执行一次客户接收',exact:true}).click();
  const deniedBody=await (await denied).json();
  assert.equal(deniedBody.State,'REJECTED');assert.equal(deniedBody.Persisted,0);assert.equal(deniedBody.Confirmed,0);
  await page.getByRole('button',{name:'退出并清除私人视图',exact:true}).click();await page.getByLabel('登录名称',{exact:true}).waitFor();
  assert.equal(await page.locator('.packet').count(),0);assert.equal(await page.getByTestId('memory-body').count(),0);
  await page.getByLabel('登录名称',{exact:true}).fill(input.login);
  await page.getByLabel('登录凭据',{exact:true}).fill(input.credential);
  await page.getByRole('button',{name:'登录',exact:true}).click();await page.getByTestId('current-player').waitFor();
  assert.equal(await page.getByTestId('current-player').innerText(),input.player);
  assert.equal(await page.locator('.originals li').count(),0);
  await page.getByRole('button',{name:'连接本人会话',exact:true}).click();await page.getByText('本人会话已连接',{exact:false}).waitFor();
  const foreignOriginal=await context.request.get(input.origin+'/bff/originals/'+recoveredOriginal.original_id);
  const foreignBody=await foreignOriginal.json();
  // The journal's foreign-identity exception is deliberately opaque at this
  // HTTP boundary. It is not a platform success or a recovered observation.
  assert.equal(foreignOriginal.status(),404);
  assert.equal(foreignBody.code,'ORIGINAL_NOT_FOUND');
  assert.equal(JSON.stringify(foreignBody).includes(recoveredOriginal.original_id),false);
  assert.equal('observation'in foreignBody,false);
  await foreignOriginal.dispose();
  const foreignQuery=await context.request.post(input.origin+'/bff/query-original/'+recoveredOriginal.original_id,{headers:{Origin:input.origin,'X-Loreviva-Csrf':currentCSRF},data:{format:'lv-bff-query-v1'}});
  assert.equal(foreignQuery.status(),404);assert.equal((await foreignQuery.json()).code,'ORIGINAL_NOT_FOUND');await foreignQuery.dispose();
  await page.getByRole('button',{name:'退出并清除私人视图',exact:true}).click();await page.getByLabel('登录名称',{exact:true}).waitFor();
  assert.deepEqual(failures,[]);
  return {target:'reference-player-real-loopback',browserVersion:browser.version(),realBrowser:true,realPlatform:true,realCustomer:true,
   narrowWidth:390,keyboard:true,privateViewCleared:true,networkStub:false};
 }finally{
  // Close cancels outstanding browser response-body readers on both success
  // and assertion failure; do not await them while keeping the context alive.
  await context.close();await Promise.allSettled(responseJobs);
 }
}

test('reference-player-real-loopback',async({browser})=>{
 const raw=process.env.LOREVIVA_REFERENCE_BROWSER_INPUT;
 assert.ok(raw,'owning real Go platform fixture must supply the local browser input');
 delete process.env.LOREVIVA_REFERENCE_BROWSER_INPUT;
 const input=JSON.parse(raw) as ReferenceBrowserInput;
 assert.deepEqual(Object.keys(input).sort(),['credential','fresh','login','origin','player','turn','world']);
 const custody=process.env.LOREVIVA_TEST_CUSTODY_FILE;
 assert.ok(custody&&dirname(resolve(custody))===resolve(process.env.TMP??''));
 const materials=[input.credential];
 try{
  const result=await runReferencePlayerRealLoopback(browser,input,values=>materials.push(...values));
  console.log(JSON.stringify(result));
 }finally{await appendFile(custody,JSON.stringify(materials.filter(Boolean))+'\n')}
});
