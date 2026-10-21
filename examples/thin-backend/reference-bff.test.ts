import {test,expect,vi} from 'vitest';
import {mkdtemp,readFile,writeFile,rename,open,lstat,type FileHandle} from 'node:fs/promises';
import {appendFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawn} from 'node:child_process';
import {createServer,request as httpRequest} from 'node:http';
import {createHash,createCipheriv,createDecipheriv,randomBytes} from 'node:crypto';
import validators from '../../packages/sdk-ts/src/generated/validators.cjs';
import {createPreparedIntegrationOwner} from '../../packages/sdk-ts/src/client.ts';

// Test-only mutation of one already-owned synthetic file. The production verifier
// receives no callback or replacement security result.
async function withUnsafeAcl(executable:string,path:string,mode:'foreign-read'|'inherit',assertDenied:()=>Promise<void>){
 const script=String.raw`
$ErrorActionPreference='Stop'
[Console]::InputEncoding=New-Object Text.UTF8Encoding($false)
[Console]::OutputEncoding=New-Object Text.UTF8Encoding($false)
Add-Type -TypeDefinition @'
using System;using System.Runtime.InteropServices;
public static class AclFixture {
 [DllImport("advapi32.dll",SetLastError=true)] static extern bool GetSecurityDescriptorDacl(IntPtr p,out bool present,out IntPtr d,out bool def);
 [DllImport("advapi32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern uint SetNamedSecurityInfo(string p,int t,uint i,IntPtr o,IntPtr g,IntPtr d,IntPtr s);
 public static void Set(string path,byte[] bytes,bool protect){
  IntPtr p=Marshal.AllocHGlobal(bytes.Length);
  try{Marshal.Copy(bytes,0,p,bytes.Length);bool present,def;IntPtr d;
   if(!GetSecurityDescriptorDacl(p,out present,out d,out def)||!present||d==IntPtr.Zero)throw new Exception("fixture-dacl");
   uint e=SetNamedSecurityInfo(path,1,protect?0x80000004u:0x20000004u,IntPtr.Zero,IntPtr.Zero,d,IntPtr.Zero);
   if(e!=0)throw new System.ComponentModel.Win32Exception((int)e);
  }finally{Marshal.FreeHGlobal(p);}
 }
}
'@
$v=[Console]::In.ReadLine()|ConvertFrom-Json
$path=[IO.Path]::GetFullPath([string]$v.path)
if($path -cne $v.path -or [IO.Path]::GetFileName($path) -cne 'key.json'){throw 'fixture-target'}
$old=[IO.File]::GetAccessControl($path,([Security.AccessControl.AccessControlSections]::Owner -bor [Security.AccessControl.AccessControlSections]::Access))
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
if($old.GetOwner([Security.Principal.SecurityIdentifier]).Value -cne $sid){throw 'fixture-owner'}
$original=$old.GetSecurityDescriptorBinaryForm()
$changed=New-Object Security.AccessControl.FileSecurity
$changed.SetSecurityDescriptorBinaryForm($original)
if($v.mode -ceq 'foreign-read'){
 $everyone=New-Object Security.Principal.SecurityIdentifier('S-1-1-0')
 $rule=New-Object Security.AccessControl.FileSystemAccessRule($everyone,[Security.AccessControl.FileSystemRights]::ReadData,[Security.AccessControl.AccessControlType]::Allow)
 $changed.AddAccessRule($rule);$protect=$true
}elseif($v.mode -ceq 'inherit'){$changed.SetAccessRuleProtection($false,$true);$protect=$false}
else{throw 'fixture-mode'}
try{
 [AclFixture]::Set($path,$changed.GetSecurityDescriptorBinaryForm(),$protect)
 [Console]::Out.WriteLine('MUTATED')
 if([Console]::In.ReadLine() -cne 'RESTORE'){throw 'fixture-return'}
}finally{[AclFixture]::Set($path,$original,$old.AreAccessRulesProtected)}
`;
 const child=spawn(executable,['-NoLogo','-NoProfile','-NonInteractive','-Command',script],{stdio:['pipe','pipe','pipe'],windowsHide:true});
 let output='',errors='';let readyResolve!:()=>void,readyReject!:(e:Error)=>void;
 const ready=new Promise<void>((resolve,reject)=>{readyResolve=resolve;readyReject=reject});
 child.stdout.on('data',b=>{output+=b;if(output==='MUTATED\r\n'||output==='MUTATED\n')readyResolve();if(output.length>4096)readyReject(Error('Fixture output cap'))});
 child.stderr.on('data',b=>{errors+=b;if(errors.length>16384)readyReject(Error('Fixture diagnostics cap'))});
 const returned=new Promise<number|null>((resolve,reject)=>{child.on('error',reject);child.on('close',code=>{readyReject(Error('ACL fixture ended before mutation: '+errors));resolve(code)})});
 const timeout=setTimeout(()=>{child.kill();readyReject(Error('ACL fixture budget'))},10000);
 child.stdin.write(JSON.stringify({path,mode})+'\n');
 try{await ready;await assertDenied()}finally{
  child.stdin.end('RESTORE\n');
  try{expect(await returned,errors).toBe(0)}finally{clearTimeout(timeout)}
 }
}

test('ReferenceBackendOriginalJournalAndIsolation',async()=>{
 const parent=await mkdtemp(join(tmpdir(),'reference-journal-'));
 const root=join(parent,'owned');
 const custody=process.env['LOREVIVA_TEST_CUSTODY_FILE'];if(!custody)throw Error('Owned custody is required');
 // Native ownership evidence is supplied by the test owner, outside the repository.
 const nativeFile=process.env['LOREVIVA_TEST_NATIVE_FIXTURE_FILE'];if(!nativeFile)throw Error('Native ownership fixture is required');
 const native=JSON.parse(await readFile(nativeFile,'utf8'));
 const module=await import('./command-journal.ts').catch(()=>null);
 expect(module!==null,'The durable journal owner must exist before native custody and IO behavior can be tested').toBe(true);
 if(!module)return;
 const options={root,installation:'synthetic-reference',windows:{executable:native.tool_private_fp.path,
  sha256:native.tool_private_fp.sha256,bytes:native.tool_private_fp.bytes,
  volume:native.tool_native_stat.device,fileId:native.tool_native_stat.inode,userSID:native.expected_user_sid}};
 try{await module.initializeReferenceJournal(options)}finally{
  const key=await readFile(join(root,'key.json'),'utf8').then(body=>JSON.parse(body)).catch(()=>null);
  if(key)appendFileSync(custody,JSON.stringify([key.key_base64,Buffer.from(key.key_base64,'base64').toString('hex')])+'\n');
 }
 const journal=await module.openReferenceJournal(options);
 const identity=['synthetic-reference','tenant1','product1','customer1','player1','world1','integration_r0_v1'] as const;
 const source=createPreparedIntegrationOwner({baseUrl:'http://127.0.0.1:1',profile:'integration_r0_v1',role:'CLIENT',getBearer:()=>null});
 const prepared=await source.prepare('governMemories',{path:{instance_id:'world1'},headers:{'Loreviva-Client-Contract':'integration_r0_v1','Idempotency-Key':'journal_original_1'},
  body:{operation:'DELETE',memory_ids:['memory1'],expected_revisions:{memory1:'1'},reason:'  retained exact reason  '}});
 const snapshot=source.inspect(prepared);let originalId='';
 try{
  expect(journal.list()).toEqual([]);
  await expect(module.openReferenceJournal(options)).rejects.toThrow();
  const child=spawn(process.execPath,['--input-type=module','--eval',
   `import {openReferenceJournal} from ${JSON.stringify(new URL('./command-journal.ts',import.meta.url).href)};
    let raw='';for await(const part of process.stdin)raw+=part;
    try{const j=await openReferenceJournal(JSON.parse(raw));await j.close();process.exitCode=3}
    catch(e){if(e.code==='EEXIST'){process.stdout.write('OWNED_LOCK_REFUSED\\n')}else{process.stderr.write(String(e));process.exitCode=4}}`
  ],{stdio:['pipe','pipe','pipe'],windowsHide:true,env:{...process.env,NODE_OPTIONS:''}});
  let childOut='',childErr='';child.stdout.on('data',b=>{childOut+=b;expect(childOut.length).toBeLessThan(4096)});child.stderr.on('data',b=>{childErr+=b;expect(childErr.length).toBeLessThan(4096)});
  const timeout=setTimeout(()=>child.kill(),15000);
  const childExit=new Promise<number|null>((resolve,reject)=>{child.on('error',reject);child.on('close',resolve)});
  child.stdin.end(JSON.stringify(options));
  try{expect(await childExit).toBe(0);expect(childOut).toBe('OWNED_LOCK_REFUSED\n');expect(childErr).toBe('')}finally{clearTimeout(timeout)}
  const broker=journal.bindBroker();expect(()=>journal.bindBroker()).toThrow();
  const stored=await broker.commitPrepared(snapshot,identity);originalId=stored.originalId;
  expect(journal.list(identity)).toHaveLength(1);
  expect(()=>broker.consumeFirstSendPermit({...stored.permit},snapshot,identity)).toThrow();
  expect(()=>broker.consumeFirstSendPermit(stored.permit,{...snapshot,path:snapshot.path+'x'},identity)).toThrow();
  broker.consumeFirstSendPermit(stored.permit,snapshot,identity);
  expect(()=>broker.consumeFirstSendPermit(stored.permit,snapshot,identity)).toThrow();
  await expect(broker.commitPrepared(snapshot,identity)).rejects.toThrow();
  await journal.observe(originalId,identity,{kind:'UNKNOWN',status:null,body:null,taskId:null});
  expect(journal.original(originalId,identity).state).toBe('UNKNOWN');
  expect(()=>journal.original(originalId,[...identity.slice(0,4),'another','world1','integration_r0_v1'] as never)).toThrow();
 }finally{await source.close();await journal.close()}
 const reopened=await module.openReferenceJournal(options);
 try{expect(reopened.list(identity)).toHaveLength(1);expect(reopened.original(originalId,identity).state).toBe('UNKNOWN');
  await expect(reopened.bindBroker().commitPrepared(snapshot,identity)).rejects.toThrow();
 }finally{await reopened.close()}
 const journalPath=join(root,'journal.bin'),originalBytes=await readFile(journalPath);
 const keyPath=join(root,'key.json'),keyBytes=await readFile(keyPath);
 await rename(keyPath,join(parent,'key.saved'));
 try{await expect(module.openReferenceJournal(options)).rejects.toThrow()}finally{await rename(join(parent,'key.saved'),keyPath)}
 expect((await readFile(keyPath)).equals(keyBytes)).toBe(true);
 const mutations=[
  originalBytes.subarray(0,originalBytes.length-1),
  Buffer.concat([Buffer.from('NOPE\n'),originalBytes.subarray(5)]),
  Buffer.from(originalBytes),Buffer.from(originalBytes),Buffer.from(originalBytes)
 ];
 mutations[2]![21]=mutations[2]![21]!^1; // Authenticated ciphertext.
 mutations[3]![9]=mutations[3]![9]!^1; // Nonce/AAD chain cannot be replaced.
 mutations[4]!.writeUInt32BE(1048577,5); // Reject before allocating an oversized frame.
 for(const bytes of mutations){
  await writeFile(journalPath,bytes);
  try{await expect(module.openReferenceJournal(options)).rejects.toThrow()}finally{await writeFile(journalPath,originalBytes)}
 }
 const final=await module.openReferenceJournal(options);try{expect(final.list(identity)).toHaveLength(1)}finally{await final.close()}
 for(const mode of ['foreign-read','inherit'] as const){
  await withUnsafeAcl(options.windows.executable,keyPath,mode,async()=>{
   await expect(module.openReferenceJournal(options)).rejects.toThrow();
   expect((await readFile(journalPath)).equals(originalBytes)).toBe(true);
  });
 }
 const aclRestored=await module.openReferenceJournal(options);await aclRestored.close();
 // Authenticated DATA fixtures exercise storage bounds without claiming that
 // 128 business requests were issued. They use only this owned synthetic key.
 const key=JSON.parse(keyBytes.toString('utf8'));
 const sha=(body:Uint8Array)=>createHash('sha256').update(body).digest('hex');
 const firstLength=originalBytes.readUInt32BE(5),firstFrame=originalBytes.subarray(5,5+32+firstLength);
 const decrypt=createDecipheriv('aes-256-gcm',Buffer.from(key.key_base64,'base64'),firstFrame.subarray(4,16));
 decrypt.setAAD(Buffer.from(JSON.stringify(['lv-reference-journal-v1',key.installation,key.key_id,'1','0'.repeat(64)])));
 decrypt.setAuthTag(firstFrame.subarray(firstFrame.length-16));
 const template=JSON.parse(Buffer.concat([decrypt.update(firstFrame.subarray(16,-16)),decrypt.final()]).toString('utf8'));
 function frames(records:Record<string,unknown>[],corrupt?:'seq'|'aad'|'previous'){
  const parts=[Buffer.from('LVJ1\n')];let previous='0'.repeat(64);
  records.forEach((record,index)=>{
   const value={...record,seq:String(index+1+(corrupt==='seq'?1:0))},nonce=randomBytes(12);
   const cipher=createCipheriv('aes-256-gcm',Buffer.from(key.key_base64,'base64'),nonce);
   cipher.setAAD(Buffer.from(JSON.stringify(['lv-reference-journal-v1',corrupt==='aad'?'another-installation':key.installation,key.key_id,String(index+1),corrupt==='previous'?'1'.repeat(64):previous])));
   const bytes=Buffer.concat([cipher.update(Buffer.from(JSON.stringify(value))),cipher.final()]),length=Buffer.alloc(4);length.writeUInt32BE(bytes.length);
   const frame=Buffer.concat([length,nonce,bytes,cipher.getAuthTag()]);parts.push(frame);previous=sha(frame);
  });return Buffer.concat(parts);
 }
 const capacity=Array.from({length:129},(_,i)=>{
  const row={...template,seq:String(i+1),original_id:i.toString(16).padStart(32,'0'),original_key:'capacity_'+i};
  row.original_hash=sha(Buffer.from(JSON.stringify(['lv-reference-original-v1',row.identity,row.operation,row.original_key,row.quote_id,row.request_target,row.request_base64,row.request_sha256])));
  return row;
 });
 const outcomes=Array.from({length:33},(_,i)=>({
   format:'lv-reference-journal-v1',kind:'OUTCOME',seq:String(i+2),original_id:template.original_id,
   original_hash:template.original_hash,outcome_no:i+1,observation_kind:'UNKNOWN',status:null,
   validated_response_base64:null,response_sha256:null,association_task_id:null
 }));
 for(const [label,bytes,valid] of [
  ['authenticated wrong sequence',frames([template],'seq'),false],
  ['foreign AAD identity',frames([template],'aad'),false],
  ['wrong previous-frame binding',frames([template],'previous'),false],
  ['authenticated wrong original hash',frames([{...template,original_hash:'1'.repeat(64)}]),false],
  ['authenticated wrong request hash',frames([{...template,request_sha256:'1'.repeat(64)}]),false],
  ['authenticated ambiguous locator',frames([template,{...template,original_id:'e'.repeat(32)}]),false],
  ['outcome references foreign original',frames([template,{...outcomes[0],original_id:'f'.repeat(32)}]),false],
  ['128 originals',frames(capacity.slice(0,128)),true],['129 originals',frames(capacity),false],
  ['32 outcomes',frames([template,...outcomes.slice(0,32)]),true],
  ['33 outcomes',frames([template,...outcomes]),false],
  ['64MiB overflow',Buffer.alloc(67108865),false]
 ] as const){
  await writeFile(journalPath,bytes);
  try{
   if(valid){
    const full=await module.openReferenceJournal(options);
    try{
     if(label==='128 originals'){
      expect(full.list(identity),label).toHaveLength(128);
      await expect(full.bindBroker().commitPrepared(snapshot,identity)).rejects.toThrow('capacity');
     }else{
      expect(full.list(identity)[0]?.outcomes).toBe(32);
      await expect(full.observe(template.original_id,identity,{kind:'UNKNOWN',status:null,body:null,taskId:null})).rejects.toThrow();
     }
     expect((await readFile(journalPath)).equals(bytes)).toBe(true);
    }finally{await full.close()}
   }else await expect(module.openReferenceJournal(options),label).rejects.toThrow();
  }finally{await writeFile(journalPath,originalBytes)}
 }
 // These are actual FileHandle failures at the owned append boundary, not a
 // production "success" callback. Each uncertain file/lock remains untouched.
 for(const fault of ['partial-write','sync','close'] as const){
  const faultRoot=join(parent,'fault-'+fault),faultOptions={...options,root:faultRoot};
  try{await module.initializeReferenceJournal(faultOptions)}finally{
   const key=await readFile(join(faultRoot,'key.json'),'utf8').then(body=>JSON.parse(body)).catch(()=>null);
   if(key)appendFileSync(custody,JSON.stringify([key.key_base64,Buffer.from(key.key_base64,'base64').toString('hex')])+'\n');
  }
  const target=await module.openReferenceJournal(faultOptions),broker=target.bindBroker();
  const targetFile=join(faultRoot,'journal.bin'),before=await lstat(targetFile,{bigint:true});
  const probe=await open(targetFile,'r'),prototype=Object.getPrototypeOf(probe);await probe.close();
  const method=fault==='sync'?'sync':'write';
  const original=prototype[method];let injected=0,sent=0;
  let restoreClose:(()=>void)|undefined;
  const spy=vi.spyOn(prototype,method).mockImplementation(async function(this:FileHandle,...args:unknown[]){
   const s=await this.stat({bigint:true}).catch(()=>null);
   if(s&&s.ino===before.ino&&s.dev===before.dev&&injected===0){
    if(fault==='close'){
     // Node owns close as an instance function; install only on the actual
     // append handle after its first real write, rather than a false prototype.
     if(!restoreClose){
      const actualClose=this.close.bind(this);
      const closeSpy=vi.spyOn(this,'close').mockImplementation(async()=>{
       await actualClose();if(injected===0){injected++;throw Error('CONTROLLED_OWNED_close')}
      });
      restoreClose=()=>closeSpy.mockRestore();
     }
     return original.apply(this,args);
    }
    injected++;
    if(fault==='partial-write')await original.call(this,(args[0] as Uint8Array).subarray(0,8));
    throw Error('CONTROLLED_OWNED_'+fault);
   }
   return original.apply(this,args);
  });
  try{
   await expect(broker.commitPrepared(snapshot,identity).then(value=>{
    broker.consumeFirstSendPermit(value.permit,snapshot,identity);sent++;
   })).rejects.toThrow('CONTROLLED_OWNED_'+fault);
   expect(injected).toBe(1);expect(sent).toBe(0);
  }finally{spy.mockRestore();restoreClose?.()}
  await expect(target.close()).rejects.toThrow('Uncertain journal');
  expect((await lstat(join(faultRoot,'owner.lock'))).isFile()).toBe(true);
  await expect(module.openReferenceJournal(faultOptions)).rejects.toThrow();
 }
 const backend=await import('./reference-bff.ts').catch(()=>null);
 expect(backend!==null,'Verified journal custody must be composed into the real loopback BFF owner').toBe(true);
 if(!backend)return;
 const bffRoot=join(parent,'backend'),bffJournal={...options,root:bffRoot};
 try{await module.initializeReferenceJournal(bffJournal)}finally{
  const key=await readFile(join(bffRoot,'key.json'),'utf8').then(body=>JSON.parse(body)).catch(()=>null);
  if(key)appendFileSync(custody,JSON.stringify([key.key_base64,Buffer.from(key.key_base64,'base64').toString('hex')])+'\n');
 }
 const bffKey=JSON.parse(await readFile(join(bffRoot,'key.json'),'utf8'));
 function decodeOwned(bytes:Buffer){
  let at=5,seq=1,previous='0'.repeat(64);const records:Record<string,any>[]=[];
  while(at<bytes.length){
   const length=bytes.readUInt32BE(at),frame=bytes.subarray(at,at+32+length);
   const decipher=createDecipheriv('aes-256-gcm',Buffer.from(bffKey.key_base64,'base64'),frame.subarray(4,16));
   decipher.setAAD(Buffer.from(JSON.stringify(['lv-reference-journal-v1',bffKey.installation,bffKey.key_id,String(seq),previous])));
   decipher.setAuthTag(frame.subarray(-16));records.push(JSON.parse(Buffer.concat([decipher.update(frame.subarray(16,-16)),decipher.final()]).toString('utf8')));
   previous=sha(frame);seq++;at+=frame.length;
  }return records;
 }
 const serviceKey=randomBytes(24).toString('hex'),clientToken=randomBytes(24).toString('hex'),loginCredential=randomBytes(24).toString('hex');
 appendFileSync(custody,JSON.stringify([serviceKey,clientToken,loginCredential])+'\n');
 const clientToken2=randomBytes(24).toString('hex');appendFileSync(custody,JSON.stringify([clientToken2])+'\n');
 const scope={tenant_id:'tenant1',product_id:'product1',world_instance_id:'world1'};
 const error={code:'STATE_CONFLICT',message:'Synthetic protocol observation',correlation_id:'corr1',retryable:false,recovery_action:'FIX_REQUEST'};
 let controlMode:'none'|'receipt'|'lost'='none',controlTaskQueries=0;
 let platformFailure=false,postCount=0,wrongCurrent=false,governReceipt=false,taskQueries=0,loseTurn=false,originalQueries=0,selfQueries=0;
 let streamMode:'normal'|'foreign'|'partial'|'hold'='normal',holdMedia=false,closedStreams=0,closedMedia=0;
 let mediaEntered:()=>void=()=>{};
 const page={scope,view_epoch:'1',events:[],deliveries:[],next_cursor:'bff_cursor_00000002'};
 const wav=Buffer.alloc(160044);
 wav.write('RIFF',0);wav.writeUInt32LE(160036,4);wav.write('WAVEfmt ',8);wav.writeUInt32LE(16,16);
 wav.writeUInt16LE(1,20);wav.writeUInt16LE(1,22);wav.writeUInt32LE(16000,24);wav.writeUInt32LE(32000,28);
 wav.writeUInt16LE(2,32);wav.writeUInt16LE(16,34);wav.write('data',36);wav.writeUInt32LE(160000,40);
 let savedTurn:Record<string,unknown>|null=null,browserAbortOnSend:AbortController|null=null;
 const receipt={task_id:'task_govern1',task_kind:'MEMORY_GOVERNANCE',task_state:'ACCEPTED',scope,correlation_id:'corr_govern1'};
 const taskView={task:{...receipt,task_state:'COMMITTED'},versions:{release_id:'release1',world_revision:'1',control_version:'1',privacy_epoch:'1',view_epoch:'1'},
  attempts:[],checkpoint_state:'NONE',billing:{service_settlement_state:'NOT_APPLICABLE',service_unit:'WE_CREDIT',customer_reserved_units:'0',customer_consumed_units:'0',supplier_safety_reserved_units:'0',supplier_cost_currency:'USD',risk_bearer:'SELF_HOSTED'}};
 const received:{path:string;body:Buffer;original:Record<string,any>}[]=[];
 const platform=createServer(async(req,res)=>{
  try{
   const chunks:Buffer[]=[];for await(const b of req)chunks.push(Buffer.from(b));const body=Buffer.concat(chunks);
   if(req.headers.cookie!==undefined||req.headers.origin!==undefined||req.headers['x-loreviva-csrf']!==undefined)throw Error('Cross-hop credential header');
   res.setHeader('content-type','application/json');res.setHeader('Loreviva-Client-Contract','integration_r0_v1');
   if(req.url==='/world/v1/client-sessions'){
    if(req.headers.authorization!=='Bearer '+serviceKey)throw Error('Exchange role');
    const request=JSON.parse(body.toString('utf8'));if(!['external1','external2'].includes(request.external_subject_id)||request.product_id!=='product1')throw Error('Exchange mapping');
    const second=request.external_subject_id==='external2';res.statusCode=201;res.end(JSON.stringify({scope,subject_id:second?'player2':'player1',access_token:second?clientToken2:clientToken,expires_at:new Date(Date.now()+900000).toISOString(),allowed_actions:['READ','MEMORY_GOVERN','CONTROL_PAUSE','CONTROL_RESUME','TURN','MEMORY_VIEW'],view_epoch:'1'}));return;
   }
   if(req.url==='/world/v1/self-consent'&&req.method==='GET'){
    selfQueries++;
    const second=req.headers.authorization==='Bearer '+clientToken2;if(!second&&req.headers.authorization!=='Bearer '+clientToken)throw Error('Current role');
    res.end(JSON.stringify({scope:{tenant_id:'tenant1',product_id:'product1'},subject_id:wrongCurrent?'other':second?'player2':'player1',state:'REVOKED',consent_revision:'2',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D',accepted_at:null,expires_at:null,current_use_enabled:false,eligible:true,server_now:new Date().toISOString()}));return;
   }
   if(req.url==='/world/v1/instances/world1/events?cursor=cursor_numeric_0001&limit=20'&&req.method==='GET'){if(req.headers.authorization!=='Bearer '+clientToken)throw Error('Page role');res.end(JSON.stringify(page));return;}
   if(req.url?.startsWith('/world/v1/instances/world1/events/stream?')&&req.method==='GET'){
    if(req.headers.authorization!=='Bearer '+clientToken)throw Error('Stream role');
    res.setHeader('content-type','text/event-stream');
    const data=streamMode==='foreign'?{...page,scope:{...scope,world_instance_id:'another_world'}}:page;
    const frame='event: update\nid: bff_cursor_00000001\ndata: '+JSON.stringify(data)+'\n\n';
    if(streamMode==='hold'){res.once('close',()=>{closedStreams++});res.write(frame);return}
    res.end(streamMode==='partial'?frame.slice(0,-2):frame);return;
   }
   if(req.url==='/world/v1/runtime-media/asset1/content'&&req.method==='GET'){
    if(req.headers.authorization!=='Bearer '+clientToken)throw Error('Binary role');
    if(req.headers.range==='bytes=999999-'){res.statusCode=416;res.end(JSON.stringify(error));return}
    res.setHeader('content-type','audio/wav');res.setHeader('accept-ranges','bytes');
    if(holdMedia){res.setHeader('content-length',String(wav.length));res.once('close',()=>{closedMedia++});res.write(wav.subarray(0,44));mediaEntered();return}
    if(req.headers.range==='bytes=0-1'){res.statusCode=206;res.setHeader('content-range','bytes 0-1/160044');res.setHeader('content-length','2');res.end(wav.subarray(0,2));return}
    res.setHeader('content-length',String(wav.length));res.end(wav);return;
   }
   if(/^\/world\/v1\/tasks\/task_control_(PAUSE|RESUME)$/.test(req.url??'')&&req.method==='GET'){
    if(req.headers.authorization!=='Bearer '+clientToken)throw Error('Control task role');controlTaskQueries++;
    const view={...taskView,task:{...receipt,task_id:req.url!.split('/').at(-1),task_kind:'CONTROL',task_state:'COMMITTED'}};
    if(!validators.schema_TaskView(view))throw Error('Control TaskView fixture');res.end(JSON.stringify(view));return;
   }
   if(req.url==='/world/v1/tasks/task_govern1'&&req.method==='GET'){
    if(req.headers.authorization!=='Bearer '+clientToken)throw Error('Task query role');
    taskQueries++;res.end(JSON.stringify(taskView));return;
   }
   if(req.url==='/world/v1/instances/world1/turn-intake-receipt'&&req.method==='GET'){
    const originalTurn=received.find(row=>row.original.operation==='submitTurn'&&row.original.original_key==='bff_original_11');
    if(req.headers.authorization!=='Bearer '+clientToken||!savedTurn||!originalTurn||req.headers['idempotency-key']!==originalTurn.original.original_key)throw Error('Original query binding');
    originalQueries++;res.end(JSON.stringify(savedTurn));return;
   }
   if(req.method==='POST'){
    postCount++;
    const rows=decodeOwned(await readFile(join(bffRoot,'journal.bin'))),original=rows.at(-1)!;
    if(original.kind!=='ORIGINAL'||original.request_target[0]!==req.method||original.request_target[1]!==req.url||
       original.original_key!==req.headers['idempotency-key']||original.request_sha256!==sha(body)||!Buffer.from(original.request_base64,'base64').equals(body))throw Error('Actual send preceded durable exact original');
    const isService=original.operation==='createInstance'||original.operation==='createQuote'||['START','END'].includes(JSON.parse(body.toString('utf8')).command);
    if(req.headers.authorization!=='Bearer '+(isService?serviceKey:clientToken))throw Error('Wrong source role');
    received.push({path:req.url!,body,original});
    if(browserAbortOnSend){const abort=browserAbortOnSend;browserAbortOnSend=null;abort.abort()}
    if(loseTurn&&original.operation==='submitTurn'){
     savedTurn={scope,customer_id:'customer1',caller_subject_id:'player1',operation_id:'submitTurn',original_key:original.original_key,intake_receipt_ref:'intake1',
      acceptance:{quote_id:'quote1',turn_id:'turn1',task:{...receipt,task_id:'task_turn1',task_kind:'TURN'}}};
     res.destroy();return;
    }
    if(controlMode!=='none'&&original.operation==='controlInstance'){
     if(controlMode==='lost'){res.destroy();return}
     const value={...receipt,task_id:'task_control_'+JSON.parse(body.toString('utf8')).command,task_kind:'CONTROL'};
     if(!validators.schema_TaskReceipt(value))throw Error('Control receipt fixture');res.statusCode=202;res.end(JSON.stringify(value));return;
    }
    if(governReceipt&&original.operation==='governMemories'){res.statusCode=202;res.end(JSON.stringify(receipt));return}
    res.statusCode=409;res.end(JSON.stringify(error));return;
   }
   res.statusCode=409;res.end(JSON.stringify(error));
  }catch{platformFailure=true;res.statusCode=500;res.end('{}')}
 });
 await new Promise<void>(resolve=>platform.listen(0,'127.0.0.1',resolve));
 const address=platform.address();if(!address||typeof address==='string')throw Error('Owned platform listener missing');
 const app=await backend.startReferenceBff({platformOrigin:`http://127.0.0.1:${address.port}`,port:0,journal:bffJournal,
  users:[{login:'synthetic1',credentialSHA256:sha(Buffer.from(loginCredential)),tenant:'tenant1',product:'product1',customer:'customer1',serviceSubject:'service1',player:'player1',externalSubject:'external1',entitlement:'entitlement1',world:'world1',profile:'integration_r0_v1',
   requestedActions:['READ','MEMORY_GOVERN','CONTROL_PAUSE','CONTROL_RESUME','TURN','MEMORY_VIEW'],serviceKey},
   {login:'synthetic2',credentialSHA256:sha(Buffer.from(loginCredential)),tenant:'tenant1',product:'product1',customer:'customer1',serviceSubject:'service1',player:'player2',externalSubject:'external2',entitlement:'entitlement2',world:'world1',profile:'integration_r0_v1',
   requestedActions:['READ','MEMORY_GOVERN','CONTROL_PAUSE','CONTROL_RESUME','TURN','MEMORY_VIEW'],serviceKey}]});
 let cookie='',csrf='';
 const local=async(path:string,body:unknown,extra:Record<string,string>={})=>fetch(app.origin+path,{
  method:'POST',headers:{'content-type':'application/json',Origin:app.origin,...(cookie?{Cookie:cookie,'X-Loreviva-CSRF':csrf}:{}),...extra},body:typeof body==='string'?body:JSON.stringify(body),redirect:'error'});
 const read=async(path:string)=>fetch(app.origin+path,{headers:{Cookie:cookie},redirect:'error'});
 try{
  const builtPage=await fetch(app.origin+'/',{redirect:'error'});
  expect(builtPage.status,'built reference UI is served by its same-origin BFF').toBe(200);
  const html=await builtPage.text();expect(html).toContain('<div id="app">');
  expect(builtPage.headers.get('cache-control')).toBe('no-store');
  expect(builtPage.headers.get('content-security-policy')).toContain("connect-src 'self'");
  const assetPaths=[...html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map(m=>m[1]!);
  expect(assetPaths.length).toBeGreaterThanOrEqual(2);
  for(const path of assetPaths){
   const resource=await fetch(app.origin+path,{redirect:'error'});expect(resource.status).toBe(200);
   expect(resource.headers.get('x-content-type-options')).toBe('nosniff');
   const content=await resource.text();expect(content.length).toBeGreaterThan(0);
   expect(content.includes(serviceKey)||content.includes(loginCredential)||content.includes(clientToken)).toBe(false);
  }
  for(const path of ['/command-journal.ts','/assets/unknown.js','/assets/%2e%2e%2fkey.json']){
   const resource=await fetch(app.origin+path,{redirect:'error'});expect(resource.status).toBe(404);await resource.arrayBuffer();
  }
  const rejected=await local('/bff/login',{format:'lv-bff-login-v1',login:'synthetic1',credential:loginCredential},{Origin:'http://foreign.invalid'});
  expect(rejected.status).toBe(403);await rejected.arrayBuffer();
  const logged=await local('/bff/login',{format:'lv-bff-login-v1',login:'synthetic1',credential:loginCredential});expect(logged.status).toBe(200);
  const setCookie=logged.headers.get('set-cookie')!;expect(setCookie.includes('HttpOnly')&&setCookie.includes('SameSite=Strict')&&setCookie.includes('Path=/bff')).toBe(true);
  cookie=setCookie.split(';')[0]!;const login=await logged.json();csrf=login.csrf;
  appendFileSync(custody,JSON.stringify([cookie.split('=')[1],csrf])+'\n');
  expect(JSON.stringify(login).includes(serviceKey)||JSON.stringify(login).includes(clientToken)).toBe(false);
  const exchange=await local('/bff/exchange',{format:'lv-bff-exchange-v1',original_key:'exchange_local_1'});expect(exchange.status).toBe(200);
  const associated=await exchange.json();expect(associated.client.subject).toBe('player1');expect(JSON.stringify(associated).includes(clientToken)).toBe(false);
  const selfArgs={format:'lv-bff-read-v1',profile:'integration_r0_v1',path:{},query:[],range:null,original_key:null};
  const self=await read('/bff/client/getPlayerSelfConsent?args='+encodeURIComponent(JSON.stringify(selfArgs)));
  expect(self.status,'self consent has tenant/product scope and its exact subject, without a world field').toBe(200);
  const selfBody=await self.json();expect(selfBody.scope).toEqual({tenant_id:'tenant1',product_id:'product1'});expect(selfBody.subject_id).toBe('player1');
  const numericPage={format:'lv-bff-read-v1',profile:'integration_r0_v1',path:{instance_id:'world1'},query:[['cursor','cursor_numeric_0001'],['limit',20]],range:null,original_key:null};
  const numeric=await read('/bff/client/getEvents?args='+encodeURIComponent(JSON.stringify(numericPage)));
  expect(numeric.status,'source integer query retains its type across both adapters').toBe(200);expect(await numeric.json()).toEqual(page);
  for(const limit of [0,101,1.5,'20',true]){
   const invalid=await read('/bff/client/getEvents?args='+encodeURIComponent(JSON.stringify({...numericPage,query:[['cursor','cursor_numeric_0001'],['limit',limit]]})));
   expect(invalid.status).toBeGreaterThanOrEqual(400);await invalid.arrayBuffer();
  }
  const unavailableCustomer=await local('/bff/customer-consume',{format:'lv-bff-consume-v1'});
  expect(unavailableCustomer.status,'current login reaches the bounded customer adapter without an installed customer runtime').toBe(503);
  expect((await unavailableCustomer.json()).code).toBe('LOCAL_CUSTOMER_UNAVAILABLE');
  const commands=[
   ['createInstance',{}, {product_id:'product1',release_id:'release1',mode:'PRIVATE',initial_participants:['player1'],client_capabilities:['TEXT']}],
   ['controlInstance',{instance_id:'world1'},{command:'PAUSE',expected_control_version:'1',reason:''}],
   ['createQuote',{}, {scope,service_kind:'TURN_TEXT',input_upper_bound:'8',output_upper_bound:'8',purchase_responsibility:'MOCK'}],
   ['submitTurn',{instance_id:'world1'},{expected_versions:{world_revision:'1',control_version:'1'},primary_actor_id:'actor1',input:{kind:'TEXT',text:'Synthetic input'},quote_id:'quote1',requested_audience:'PRIVATE_RELATIONSHIP'}],
   ['commandPlayerSelfConsent',{}, {command:'REVOKE',expected_revision:'2',policy_id:'R0_PLAYER_PRIVATE_GENERATION_30D'}],
   ['governMemories',{instance_id:'world1'}, {operation:'DELETE',memory_ids:['memory1'],expected_revisions:{memory1:'1'},reason:' exact  reason '}],
   ['cancelTask',{task_id:'task1'}, {reason:'cancel owned task'}]
  ] as const;
  const envelopeFor=(row:typeof commands[number],index:number)=>({format:'lv-bff-command-v1',operation:row[0],profile:'integration_r0_v1',original_key:'bff_original_'+index,path:row[1],query:[],body:row[2]});
  for(const [index,row] of commands.entries()){
   const response=await local('/bff/commands',envelopeFor(row,index));expect(response.status,row[0]).toBe(409);
   expect(response.headers.get('x-loreviva-bff-error')).toBeNull();expect(await response.json()).toEqual(error);
  }
  expect(postCount).toBe(7);expect(received).toHaveLength(7);expect(platformFailure).toBe(false);
  const first=received[0]!.original,originalId=first.original_id;
  const lookup=await read('/bff/originals/'+originalId);expect(lookup.status).toBe(200);
  const observation=await lookup.json();expect(observation.state).toBe('OBSERVED');expect(observation.original_key).toBeUndefined();expect(observation.request_base64).toBeUndefined();
  const originalAgain=await local('/bff/commands',envelopeFor(commands[0],0));expect(originalAgain.status).toBeGreaterThanOrEqual(400);await originalAgain.arrayBuffer();expect(postCount).toBe(7);
  const noCsrf=await local('/bff/commands',envelopeFor(commands[0],8),{'X-Loreviva-CSRF':'bad'});expect(noCsrf.status).toBe(403);await noCsrf.arrayBuffer();
  const duplicate=await local('/bff/commands','{"format":"lv-bff-command-v1","format":"lv-bff-command-v1"}');expect(duplicate.status).toBeGreaterThanOrEqual(400);await duplicate.arrayBuffer();
  const wrongPerson=await local('/bff/commands',{...envelopeFor(commands[5],9),path:{instance_id:'other'}});expect(wrongPerson.status).toBe(403);await wrongPerson.arrayBuffer();
  wrongCurrent=true;const stale=await read('/bff/originals/'+originalId);expect(stale.status).toBe(403);await stale.arrayBuffer();wrongCurrent=false;
  const bypass=await read('/bff/client/governMemories?args=%7B%7D');expect(bypass.status).toBe(404);await bypass.arrayBuffer();
  expect(postCount).toBe(7);
  governReceipt=true;
  const accepted=await local('/bff/commands',envelopeFor(commands[5],10));expect(accepted.status).toBe(202);expect(await accepted.json()).toEqual(receipt);
  const governanceId=received.at(-1)!.original.original_id;
  const query=await local('/bff/query-original/'+governanceId,{format:'lv-bff-query-v1'});
  expect(query.status,'A known original governance task must be queryable without repeating its POST').toBe(200);
  const queried=await query.json();expect(queried.task_id).toBe(receipt.task_id);expect(queried.observation.body).toEqual(taskView);
  expect(postCount).toBe(8);expect(taskQueries).toBe(1);
  loseTurn=true;
  const uncertain=await local('/bff/commands',envelopeFor(commands[3],11));expect(uncertain.status).toBe(502);await uncertain.arrayBuffer();
  const uncertainId=received.at(-1)!.original.original_id;
  const recovered=await local('/bff/query-original/'+uncertainId,{format:'lv-bff-query-v1'});
  expect(recovered.status,'A sent TURN with lost reply must query its exact original receipt, never POST again').toBe(200);
  const recoveredBody=await recovered.json();expect(recoveredBody.task_id).toBe('task_turn1');expect(recoveredBody.observation.body).toEqual(savedTurn);
  expect(postCount).toBe(9);expect(originalQueries).toBe(1);
  const intactTurn=structuredClone(recordForTest(savedTurn)),intactJournal=await readFile(join(bffRoot,'journal.bin'));
  for(const altered of [
   {...intactTurn,original_key:'another_original'},
   {...intactTurn,customer_id:'another_customer'},
   {...intactTurn,caller_subject_id:'another_player'},
   {...intactTurn,acceptance:{...recordForTest(intactTurn!.acceptance),quote_id:'another_quote'}}
  ]){
   savedTurn=altered;
   try{
    const rejected=await local('/bff/query-original/'+uncertainId,{format:'lv-bff-query-v1'});
    expect(rejected.status).toBe(502);await rejected.arrayBuffer();
    expect((await readFile(join(bffRoot,'journal.bin'))).equals(intactJournal)).toBe(true);
   }finally{savedTurn=intactTurn}
  }
  expect(postCount).toBe(9);
  const beforeFind=await readFile(join(bffRoot,'journal.bin')),queryCounts=[postCount,taskQueries,originalQueries],beforeAuth=selfQueries;
  const located=await local('/bff/find-original',{format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'bff_original_11'});
  expect(located.status,'Current person must find the retained original association by exact known operation/key without new send').toBe(200);
  const found=await located.json();expect(found.original_id).toBe(uncertainId);expect(found.operation).toBe('submitTurn');
  expect(found.original_key).toBeUndefined();expect(found.request_base64).toBeUndefined();
  expect([postCount,taskQueries,originalQueries]).toEqual(queryCounts);expect(selfQueries-beforeAuth).toBe(1);
  for(const [extra,status] of [
   [{original_key:'never_recorded'},404],[{operation:'cancelTask'},404],
   [{profile:'public_v1'},403],[{operation:'exchangeClientSession'},400],
   [{original_key:'key with spaces'},400],[{original_key:'a'.repeat(129)},400],
   [{request_base64:'e30='},400],[{identity:['other']},400],[{url:'https://invalid.example'},400],
  ] as const){
   const bad=await local('/bff/find-original',Object.assign({format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'bff_original_11'},extra));
   expect(bad.status).toBe(status);
   const body=await bad.json();expect(body.format).toBe('lv-bff-error-v1');
   if(status===404)expect(body.code).toBe('ORIGINAL_NOT_FOUND');
  }
  const duplicateFind=await fetch(app.origin+'/bff/find-original',{method:'POST',headers:{'content-type':'application/json',origin:app.origin,cookie,'x-loreviva-csrf':csrf},
   body:'{"format":"lv-bff-find-original-v1","operation":"submitTurn","profile":"integration_r0_v1","original_key":"bff_original_11","original_key":"never_recorded"}'});
  expect(duplicateFind.status).toBe(400);await duplicateFind.arrayBuffer();
  expect([postCount,taskQueries,originalQueries]).toEqual(queryCounts);expect(selfQueries-beforeAuth).toBe(3);
  expect((await readFile(join(bffRoot,'journal.bin'))).equals(beforeFind)).toBe(true);
  for(const extra of [{Origin:'http://127.0.0.1:1'},{'X-Loreviva-CSRF':'incorrect'}]){
   const denied=await local('/bff/find-original',{format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'bff_original_11'},extra);
   expect(denied.status,JSON.stringify(Object.keys(extra))).toBe(403);await denied.arrayBuffer();
  }
  const wrongHost=await new Promise<number|undefined>((resolve,reject)=>{
   const request=httpRequest(app.origin+'/bff/find-original',{method:'POST',headers:{Host:'127.0.0.1:1',Origin:app.origin,'content-type':'application/json',Cookie:cookie,'X-Loreviva-CSRF':csrf}},response=>{
    response.resume();response.once('end',()=>resolve(response.statusCode));response.once('error',reject);
   });
   request.setTimeout(3000,()=>request.destroy(Error('Owned Host fixture timeout')));request.once('error',reject);
   request.end(JSON.stringify({format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'bff_original_11'}));
  });
  expect(wrongHost,'Actual raw HTTP Host must remain independently checked').toBe(403);
  const hugeLookup=await local('/bff/find-original',' '.repeat(16385)+'{}');expect(hugeLookup.status).toBe(413);await hugeLookup.arrayBuffer();
  wrongCurrent=true;
  try{
   const denied=await local('/bff/find-original',{format:'lv-bff-find-original-v1',operation:'submitTurn',profile:'integration_r0_v1',original_key:'bff_original_11'});
   expect(denied.status).toBe(403);expect((await denied.text()).includes(uncertainId)).toBe(false);
  }finally{wrongCurrent=false}
  expect((await readFile(join(bffRoot,'journal.bin'))).equals(beforeFind)).toBe(true);
  const readEnvelope=(operation:string,path:Record<string,string>,query:[string,string][]=[],range:string|null=null)=>
   '/bff/client/'+operation+'?args='+encodeURIComponent(JSON.stringify({format:'lv-bff-read-v1',profile:'integration_r0_v1',path,query,range,original_key:null}));
  const media=await read(readEnvelope('readRuntimeMedia',{asset_id:'asset1'}));
  expect(media.status).toBe(200);expect(media.headers.get('content-type')).toBe('audio/wav');
  expect(Buffer.from(await media.arrayBuffer()).equals(wav)).toBe(true);
  const partial=await read(readEnvelope('readRuntimeMedia',{asset_id:'asset1'},[],'bytes=0-1'));
  expect(partial.status).toBe(206);expect(partial.headers.get('content-range')).toBe('bytes 0-1/160044');expect(Buffer.from(await partial.arrayBuffer()).equals(wav.subarray(0,2))).toBe(true);
  const rangeError=await read(readEnvelope('readRuntimeMedia',{asset_id:'asset1'},[],'bytes=999999-'));
  expect(rangeError.status).toBe(416);expect(rangeError.headers.get('content-type')).toBe('application/json');expect(await rangeError.json()).toEqual(error);
  const streamPath=readEnvelope('streamEvents',{instance_id:'world1'},[['cursor','bff_cursor_00000000']]);
  const stream=await read(streamPath);expect(stream.status).toBe(200);expect(stream.headers.get('content-type')).toBe('text/event-stream');
  expect(await stream.text()).toBe('event: update\nid: bff_cursor_00000001\ndata: '+JSON.stringify(page)+'\n\n');
  streamMode='partial';
  const incomplete=await read(streamPath);expect(incomplete.status).toBe(502);expect((await incomplete.text()).includes('bff_cursor_00000001')).toBe(false);
  streamMode='foreign';
  const foreignFrame=await read(streamPath);
  expect(foreignFrame.status,'A valid source schema with another world cannot cross the local current-identity stream boundary').toBe(403);
  expect((await foreignFrame.text()).includes('another_world')).toBe(false);streamMode='normal';
  expect(media.headers.get('accept-ranges'),'Validated permitted binary headers survive both hops').toBe('bytes');
  streamMode='hold';
  const heldStream=await read(streamPath),heldReader=heldStream.body!.getReader();
  expect((await heldReader.read()).value?.length).toBeGreaterThan(0);
  await heldReader.cancel();heldReader.releaseLock();
  let drainDeadline=Date.now()+3000;
  while((closedStreams!==1||app.accounting().activeRequests!==0)&&Date.now()<drainDeadline)await new Promise(resolve=>setTimeout(resolve,20));
  expect(closedStreams).toBe(1);expect(app.accounting().activeRequests).toBe(0);streamMode='normal';
  holdMedia=true;const binaryAbort=new AbortController(),atMedia=new Promise<void>(resolve=>{mediaEntered=resolve});
  const heldBinary=fetch(app.origin+readEnvelope('readRuntimeMedia',{asset_id:'asset1'}),{headers:{Cookie:cookie},signal:binaryAbort.signal}).catch(()=>null);
  let mediaTimer:ReturnType<typeof setTimeout>|undefined;
  try{await Promise.race([atMedia,new Promise<never>((_,reject)=>{mediaTimer=setTimeout(()=>reject(Error('Owned media read not reached')),3000)})])}
  finally{if(mediaTimer)clearTimeout(mediaTimer);binaryAbort.abort()}
  expect(await heldBinary).toBeNull();
  drainDeadline=Date.now()+3000;
  while((closedMedia!==1||app.accounting().activeRequests!==0)&&Date.now()<drainDeadline)await new Promise(resolve=>setTimeout(resolve,20));
  expect(closedMedia).toBe(1);expect(app.accounting().activeRequests).toBe(0);holdMedia=false;
  // Actual platform send remains zero when each real journal append fails.
  // These BFF instances own separate synthetic journals; no callback can mint
  // a successful permit, and their uncertain lock files remain preserved.
  for(const fault of ['partial-write','sync','close'] as const){
   const faultRoot=join(parent,'bff-fault-'+fault),faultOptions={...options,root:faultRoot};
   try{await module.initializeReferenceJournal(faultOptions)}finally{
    const key=await readFile(join(faultRoot,'key.json'),'utf8').then(body=>JSON.parse(body)).catch(()=>null);
    if(key)appendFileSync(custody,JSON.stringify([key.key_base64,Buffer.from(key.key_base64,'base64').toString('hex')])+'\n');
   }
   const failedApp=await backend.startReferenceBff({platformOrigin:`http://127.0.0.1:${address.port}`,port:0,journal:faultOptions,
    users:[{login:'synthetic1',credentialSHA256:sha(Buffer.from(loginCredential)),tenant:'tenant1',product:'product1',customer:'customer1',serviceSubject:'service1',player:'player1',externalSubject:'external1',entitlement:'entitlement1',world:'world1',profile:'integration_r0_v1',
     requestedActions:['READ'],serviceKey}]});
   const signed=await fetch(failedApp.origin+'/bff/login',{method:'POST',headers:{Origin:failedApp.origin,'content-type':'application/json'},
    body:JSON.stringify({format:'lv-bff-login-v1',login:'synthetic1',credential:loginCredential})});
   expect(signed.status).toBe(200);
   const faultCookie=signed.headers.get('set-cookie')!.split(';')[0]!,faultSession=await signed.json();
   appendFileSync(custody,JSON.stringify([faultCookie.split('=')[1],faultSession.csrf])+'\n');
   const targetFile=join(faultRoot,'journal.bin'),before=await lstat(targetFile,{bigint:true});
   const probe=await open(targetFile,'r'),prototype=Object.getPrototypeOf(probe);await probe.close();
   const method=fault==='sync'?'sync':'write',original=prototype[method];let injected=0,restoreClose:(()=>void)|undefined;
   const spy=vi.spyOn(prototype,method).mockImplementation(async function(this:FileHandle,...args:unknown[]){
    const stat=await this.stat({bigint:true}).catch(()=>null);
    if(stat&&stat.ino===before.ino&&stat.dev===before.dev&&injected===0){
     if(fault==='close'){
      if(!restoreClose){const actual=this.close.bind(this),closeSpy=vi.spyOn(this,'close').mockImplementation(async()=>{
       await actual();if(injected===0){injected++;throw Error('BFF_OWNED_close')}
      });restoreClose=()=>closeSpy.mockRestore()}
      return original.apply(this,args);
     }
     injected++;if(fault==='partial-write')await original.call(this,(args[0] as Uint8Array).subarray(0,8));
     throw Error('BFF_OWNED_'+fault);
    }
    return original.apply(this,args);
   });
   try{
    const failed=await fetch(failedApp.origin+'/bff/commands',{method:'POST',
     headers:{Origin:failedApp.origin,'content-type':'application/json',Cookie:faultCookie,'X-Loreviva-CSRF':faultSession.csrf},
     body:JSON.stringify(envelopeFor(commands[0],20))});
    expect(failed.status).toBe(502);await failed.arrayBuffer();
    expect(injected).toBe(1);expect(postCount).toBe(9);
   }finally{
    spy.mockRestore();restoreClose?.();
    await expect(failedApp.close()).rejects.toThrow('Uncertain journal');
   }
   expect(failedApp.accounting()).toEqual({activeRequests:0,sessions:0,stopped:true});
   expect((await lstat(join(faultRoot,'owner.lock'))).isFile()).toBe(true);
  }
  // Local unavailable identity is not a platform query or a business UNKNOWN.
  const untouched=await readFile(join(bffRoot,'journal.bin')),queriesBeforeMissing=[postCount,taskQueries,originalQueries,controlTaskQueries];
  for(const get of [true,false]){const response=get?await read('/bff/originals/'+'f'.repeat(32)):await local('/bff/query-original/'+'f'.repeat(32),{format:'lv-bff-query-v1'});expect(response.status).toBe(404);expect((await response.json()).code).toBe('ORIGINAL_NOT_FOUND')}
  expect(await readFile(join(bffRoot,'journal.bin'))).toEqual(untouched);expect([postCount,taskQueries,originalQueries,controlTaskQueries]).toEqual(queriesBeforeMissing);
  controlMode='receipt';
  for(const [i,command] of ['START','END','PAUSE','RESUME'].entries()){
   const row=['controlInstance',{instance_id:'world1'},{command,expected_control_version:'1',reason:''}] as typeof commands[number];
   const sent=await local('/bff/commands',envelopeFor(row,200+i));expect(sent.status).toBe(202);await sent.arrayBuffer();const id=received.at(-1)!.original.original_id;
   const counts=[postCount,taskQueries,originalQueries,controlTaskQueries],journalBefore=await readFile(join(bffRoot,'journal.bin'));
   const recovery=await local('/bff/query-original/'+id,{format:'lv-bff-query-v1'});expect(recovery.status).toBe(200);const observed=await recovery.json();
   expect([postCount,taskQueries,originalQueries]).toEqual(counts.slice(0,3));
   if(command==='PAUSE'||command==='RESUME'){expect(controlTaskQueries).toBe(counts[3]!+1);expect(observed.observation.body.task.task_state).toBe('COMMITTED')}
   else{expect(controlTaskQueries).toBe(counts[3]);expect(await readFile(join(bffRoot,'journal.bin'))).toEqual(journalBefore);expect(observed.observation.body.task_state).toBe('ACCEPTED')}
  }
  controlMode='lost';
  for(const [i,command] of ['START','END'].entries()){
   const row=['controlInstance',{instance_id:'world1'},{command,expected_control_version:'1',reason:''}] as typeof commands[number];
   const sent=await local('/bff/commands',envelopeFor(row,210+i));expect(sent.status).toBe(502);await sent.arrayBuffer();const id=received.at(-1)!.original.original_id;
   const counts=[postCount,taskQueries,originalQueries,controlTaskQueries],journalBefore=await readFile(join(bffRoot,'journal.bin'));
   const recovery=await local('/bff/query-original/'+id,{format:'lv-bff-query-v1'});expect(recovery.status).toBe(200);expect((await recovery.json()).state).toBe('UNKNOWN');
   expect([postCount,taskQueries,originalQueries,controlTaskQueries]).toEqual(counts);expect(await readFile(join(bffRoot,'journal.bin'))).toEqual(journalBefore);
  }
  controlMode='none';
  const browserLost=new AbortController();browserAbortOnSend=browserLost;
  const lostKey='bff_original_101',lostInput=envelopeFor(commands[5],101),beforeLostSend=postCount;
  await expect(fetch(app.origin+'/bff/commands',{method:'POST',headers:{Origin:app.origin,'content-type':'application/json',Cookie:cookie,'X-Loreviva-CSRF':csrf},
   body:JSON.stringify(lostInput),signal:browserLost.signal})).rejects.toThrow();
  const completedBefore=Date.now()+10000;
  while(app.accounting().activeRequests!==0&&Date.now()<completedBefore)await new Promise(resolve=>setTimeout(resolve,20));
  expect(app.accounting().activeRequests).toBe(0);expect(postCount).toBe(beforeLostSend+1);
  const exactLost=received.find(row=>row.original.original_key===lostKey)!;expect(exactLost).toBeDefined();
  const beforeLostLookup=await readFile(join(bffRoot,'journal.bin')),lostQueries=[postCount,taskQueries,originalQueries];
  const rediscovered=await local('/bff/find-original',{format:'lv-bff-find-original-v1',operation:'governMemories',profile:'integration_r0_v1',original_key:lostKey});
  expect(rediscovered.status).toBe(200);const recoveredLocal=await rediscovered.json();
  expect(recoveredLocal.original_id).toBe(exactLost.original.original_id);
  expect([postCount,taskQueries,originalQueries]).toEqual(lostQueries);
  expect((await readFile(join(bffRoot,'journal.bin'))).equals(beforeLostLookup)).toBe(true);
  const queriedLost=await local('/bff/query-original/'+recoveredLocal.original_id,{format:'lv-bff-query-v1'});
  expect(queriedLost.status).toBe(200);expect((await queriedLost.json()).original_id).toBe(recoveredLocal.original_id);
  expect(postCount).toBe(beforeLostSend+1);
  const appendIdentity=await lstat(join(bffRoot,'journal.bin'),{bigint:true}),probe=await open(join(bffRoot,'journal.bin'),'r');
  const prototype=Object.getPrototypeOf(probe);await probe.close();
  const realSync=prototype.sync;let enteredSync!:()=>void,releaseSync!:()=>void;
  const atSync=new Promise<void>(resolve=>{enteredSync=resolve}),continueSync=new Promise<void>(resolve=>{releaseSync=resolve});
  let blocked=false;
  const syncBoundary=vi.spyOn(prototype,'sync').mockImplementation(async function(this:FileHandle){
   const value=await realSync.call(this),stat=await this.stat({bigint:true});
   if(!blocked&&stat.ino===appendIdentity.ino&&stat.dev===appendIdentity.dev){blocked=true;enteredSync();await continueSync}
   return value;
  });
  const pendingCommand=local('/bff/commands',envelopeFor(commands[5],99));
  let boundaryTimer:ReturnType<typeof setTimeout>|undefined;
  try{
   await Promise.race([atSync,new Promise<never>((_,reject)=>{boundaryTimer=setTimeout(()=>reject(Error('Owned append boundary not reached')),3000)})]);
   const logout=await local('/bff/logout',{format:'lv-bff-logout-v1'});expect(logout.status).toBe(204);await logout.arrayBuffer();
  }finally{if(boundaryTimer)clearTimeout(boundaryTimer);releaseSync();syncBoundary.mockRestore()}
  const blockedCommand=await pendingCommand;expect(blockedCommand.status).toBe(401);await blockedCommand.arrayBuffer();
  expect(blocked).toBe(true);expect(postCount,'Current loss after real sync cannot grant a first-send permit').toBe(beforeLostSend+1);
  const persistedOnly=decodeOwned(await readFile(join(bffRoot,'journal.bin'))).at(-1)!;
  expect(persistedOnly.kind).toBe('ORIGINAL');expect(persistedOnly.original_key).toBe('bff_original_99');
  const revoked=await read('/bff/originals/'+originalId);expect(revoked.status).toBe(401);await revoked.arrayBuffer();
  // Another authenticated current local player cannot distinguish this
  // existing original from a missing one, nor cause a platform query/send.
  const logged2=await local('/bff/login',{format:'lv-bff-login-v1',login:'synthetic2',credential:loginCredential});
  expect(logged2.status).toBe(200);cookie=logged2.headers.get('set-cookie')!.split(';')[0]!;csrf=(await logged2.json()).csrf;
  appendFileSync(custody,JSON.stringify([cookie.split('=')[1],csrf])+'\n');
  const exchanged2=await local('/bff/exchange',{format:'lv-bff-exchange-v1',original_key:'exchange_local_2'});
  expect(exchanged2.status).toBe(200);expect((await exchanged2.json()).client.subject).toBe('player2');
  const foreignBefore=await readFile(join(bffRoot,'journal.bin')),foreignCounts=[postCount,taskQueries,originalQueries,controlTaskQueries];
  for(const get of [true,false]){
   const response=get?await read('/bff/originals/'+recoveredLocal.original_id):await local('/bff/query-original/'+recoveredLocal.original_id,{format:'lv-bff-query-v1'});
   expect(response.status).toBe(404);const value=await response.json();expect(value.code).toBe('ORIGINAL_NOT_FOUND');expect(JSON.stringify(value)).not.toContain(recoveredLocal.original_id);
  }
  expect(await readFile(join(bffRoot,'journal.bin'))).toEqual(foreignBefore);expect([postCount,taskQueries,originalQueries,controlTaskQueries]).toEqual(foreignCounts);
  const logout2=await local('/bff/logout',{format:'lv-bff-logout-v1'});expect(logout2.status).toBe(204);await logout2.arrayBuffer();
 }finally{
  await app.close();platform.closeAllConnections();await new Promise<void>((resolve,reject)=>platform.close(e=>e?reject(e):resolve()));
 }
 expect(app.accounting()).toEqual({activeRequests:0,sessions:0,stopped:true});expect(platformFailure).toBe(false);
 const cleanChild=spawn(process.execPath,['--input-type=module','--eval',
  `import {openReferenceJournal} from ${JSON.stringify(new URL('./command-journal.ts',import.meta.url).href)};
   let raw='';for await(const part of process.stdin)raw+=part;const v=JSON.parse(raw);
   const journal=await openReferenceJournal(v.options);
   try{if(journal.original(v.id,v.identity).state!=='UNKNOWN'||journal.list(v.identity).length!==1)throw Error('Reopen identity');
    if(journal.findOriginal(v.identity,'governMemories','journal_original_1')!==v.id)throw Error('Reopen known locator');
    if(journal.findOriginal(v.identity,'cancelTask','journal_original_1')!==null)throw Error('Reopen operation mismatch');
    if(journal.findOriginal([...v.identity.slice(0,4),'other',...v.identity.slice(5)],'governMemories','journal_original_1')!==null)throw Error('Reopen foreign identity');
    process.stdout.write('CLEAN_REOPEN_QUERY_ONLY\\n')}finally{await journal.close()}`
 ],{stdio:['pipe','pipe','pipe'],windowsHide:true,env:{...process.env,NODE_OPTIONS:''}});
 let cleanOut='',cleanErr='';cleanChild.stdout.on('data',b=>{cleanOut+=b});cleanChild.stderr.on('data',b=>{cleanErr+=b});
 const cleanTimeout=setTimeout(()=>cleanChild.kill(),15000);
 const cleanExit=new Promise<number|null>((resolve,reject)=>{cleanChild.on('error',reject);cleanChild.on('close',resolve)});
 cleanChild.stdin.end(JSON.stringify({options,id:originalId,identity}));
 try{expect(await cleanExit).toBe(0);expect(cleanOut).toBe('CLEAN_REOPEN_QUERY_ONLY\n');expect(cleanErr).toBe('')}finally{clearTimeout(cleanTimeout)}
 // Synthetic files remain in the bounded private capture for failure/reopen evidence.
},300000);

function recordForTest(value:unknown){if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Fixture object missing');return value as Record<string,unknown>}
