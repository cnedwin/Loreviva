import {spawn,type ChildProcessWithoutNullStreams} from 'node:child_process';
import {createHash,createCipheriv,createDecipheriv,randomBytes,timingSafeEqual} from 'node:crypto';
import {open,lstat,readFile,mkdir,unlink,type FileHandle} from 'node:fs/promises';
import {dirname,join,parse,resolve} from 'node:path';
import {createInterface} from 'node:readline';
import validators from '../../packages/sdk-ts/src/generated/validators.cjs';
import {transportDescriptors} from '../../packages/sdk-ts/src/generated/transport.ts';
import type {PreparedRequestSnapshot} from '../../packages/sdk-ts/src/client.ts';

export interface JournalOptions {
 readonly root:string;readonly installation:string;
 readonly windows:Readonly<{executable:string;sha256:string;bytes:number;volume:string;fileId:string;userSID:string}>;
}
interface NativeFile {volume:string;nodeDevice:string;fileId:string;length:string;directory:boolean}
interface NativePhase {
 files:Record<string,NativeFile>;ownerCreation:string;childPID:number;childCreation:string;
 finish():Promise<void>;
}
const HEADER=Buffer.from('LVJ1\n','ascii'),MAX_TOTAL=64*1024*1024;
const nativeScript=String.raw`
$ErrorActionPreference='Stop'
[Console]::InputEncoding=New-Object System.Text.UTF8Encoding($false)
[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false)
$OutputEncoding=[Console]::OutputEncoding
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using Microsoft.Win32.SafeHandles;
public static class JournalNative {
 [StructLayout(LayoutKind.Sequential)] public struct FileId { public ulong Volume; public ulong Low; public ulong High; }
 [StructLayout(LayoutKind.Sequential)] public struct AttributeTag { public uint Attributes; public uint Tag; }
 [StructLayout(LayoutKind.Sequential)] public struct FileTime { public uint Low; public uint High; }
 [StructLayout(LayoutKind.Sequential)] public struct LegacyInfo { public uint Attributes; public FileTime Creation; public FileTime Access; public FileTime Write; public uint Volume; public uint SizeHigh; public uint SizeLow; public uint Links; public uint IndexHigh; public uint IndexLow; }
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] public static extern SafeFileHandle CreateFile(string p,uint a,uint s,IntPtr z,uint d,uint f,IntPtr t);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool GetFileInformationByHandleEx(SafeFileHandle h,int c,out FileId v,uint n);
 [DllImport("kernel32.dll",SetLastError=true,EntryPoint="GetFileInformationByHandleEx")] public static extern bool Attributes(SafeFileHandle h,int c,out AttributeTag v,uint n);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool GetFileSizeEx(SafeFileHandle h,out long v);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool GetFileInformationByHandle(SafeFileHandle h,out LegacyInfo v);
 [DllImport("advapi32.dll",SetLastError=true)] public static extern uint GetSecurityInfo(SafeFileHandle h,int t,uint i,out IntPtr o,out IntPtr g,out IntPtr d,out IntPtr s,out IntPtr sd);
 [DllImport("advapi32.dll")] public static extern uint GetSecurityDescriptorLength(IntPtr p);
 [DllImport("advapi32.dll",SetLastError=true)] public static extern bool GetSecurityDescriptorDacl(IntPtr p,out bool present,out IntPtr dacl,out bool defaulted);
 [DllImport("advapi32.dll",CharSet=CharSet.Unicode,SetLastError=true)] public static extern uint SetNamedSecurityInfo(string p,int t,uint i,IntPtr o,IntPtr g,IntPtr d,IntPtr s);
 [DllImport("kernel32.dll")] public static extern IntPtr LocalFree(IntPtr p);
 public static SafeFileHandle Open(string p) {
  var h=CreateFile(p,0x20080,3,IntPtr.Zero,3,0x02200000,IntPtr.Zero);
  if(h.IsInvalid)throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
  AttributeTag a;if(!Attributes(h,9,out a,8)){h.Dispose();throw new Exception("attributes");}
  if((a.Attributes&0x400)!=0){h.Dispose();throw new Exception("reparse");} return h;
 }
 public static RawSecurityDescriptor Security(SafeFileHandle h) {
  IntPtr o,g,d,s,sd;uint e=GetSecurityInfo(h,1,5,out o,out g,out d,out s,out sd);
  if(e!=0)throw new Exception("security");
  try { var bytes=new byte[GetSecurityDescriptorLength(sd)];Marshal.Copy(sd,bytes,0,bytes.Length);return new RawSecurityDescriptor(bytes,0); }
  finally {LocalFree(sd);}
 }
 public static void SetOwnedDacl(string path,byte[] descriptor,string owner) {
  using(var h=Open(path)){if(Security(h).Owner.Value!=owner)throw new Exception("init-owner");}
  IntPtr memory=Marshal.AllocHGlobal(descriptor.Length);
  try {
   Marshal.Copy(descriptor,0,memory,descriptor.Length);bool present,def;IntPtr dacl;
   if(!GetSecurityDescriptorDacl(memory,out present,out dacl,out def)||!present||dacl==IntPtr.Zero)throw new Exception("init-dacl");
   uint result=SetNamedSecurityInfo(path,1,0x80000004,IntPtr.Zero,IntPtr.Zero,dacl,IntPtr.Zero);
   if(result!=0)throw new System.ComponentModel.Win32Exception((int)result);
  }finally{Marshal.FreeHGlobal(memory);}
 }
}
'@
$v=([Console]::In.ReadLine()|ConvertFrom-Json)
if(@($v.PSObject.Properties.Name).Count -ne 8 -or $v.format -ne 'lv-native-journal-phase-v1' -or $v.challenge -notmatch '^[0-9a-f]{64}$'){throw 'input'}
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
if($sid -cne $v.sid){throw 'user'}
$root=[IO.Path]::GetFullPath([string]$v.root)
if($root -cne [string]$v.root -or -not [IO.Path]::IsPathRooted($root)){throw 'root'}
$handles=New-Object 'System.Collections.Generic.List[Microsoft.Win32.SafeHandles.SafeFileHandle]'
function OpenHeld([string]$p) {
 $h=[JournalNative]::Open(('\\?\'+$p));$handles.Add($h);return $h
}
function CheckSecurity($h,[bool]$strict) {
 $sd=[JournalNative]::Security($h)
 $trusted=@($sid,'S-1-5-18','S-1-5-32-544')
 if($null -eq $sd.DiscretionaryAcl -or $null -eq $sd.Owner){throw 'null-security'}
 if($strict -and ($sd.Owner.Value -cne $sid -or ($sd.ControlFlags -band [Security.AccessControl.ControlFlags]::DiscretionaryAclProtected) -eq 0)){throw 'owner-protection'}
 if(-not $strict -and $trusted -cnotcontains $sd.Owner.Value){throw 'parent-owner'}
 [long]$ownAllow=0
 foreach($ace in $sd.DiscretionaryAcl){
  if($ace -isnot [Security.AccessControl.CommonAce] -or $ace.IsCallback){throw 'unknown-ace'}
  if($strict -and $ace.AceFlags -ne [Security.AccessControl.AceFlags]::None){throw 'inheritance'}
  $who=$ace.SecurityIdentifier.Value
  if($ace.AceQualifier -eq [Security.AccessControl.AceQualifier]::AccessAllowed){
   if($trusted -cnotcontains $who -and ($strict -or ($ace.AccessMask -band 0xD0046) -ne 0)){throw 'untrusted-allow'}
   if($who -ceq $sid){$ownAllow=$ownAllow -bor $ace.AccessMask}
  }elseif($ace.AceQualifier -eq [Security.AccessControl.AceQualifier]::AccessDenied){
   if(@($sid,'S-1-1-0','S-1-5-11','S-1-5-32-545') -ccontains $who -and ($ace.AccessMask -band 0x1F01FF) -ne 0){throw 'restrictive-deny'}
  }else{throw 'unknown-qualifier'}
 }
 if($strict -and ($ownAllow -band 0x1F01FF) -ne 0x1F01FF){throw 'owner-rights'}
}
function SetOwnedAcl([string]$p,[bool]$directory) {
 if($directory){$acl=New-Object Security.AccessControl.DirectorySecurity}else{$acl=New-Object Security.AccessControl.FileSecurity}
 $owner=New-Object Security.Principal.SecurityIdentifier($sid)
 $acl.SetOwner($owner);$acl.SetAccessRuleProtection($true,$false)
 foreach($who in @($sid,'S-1-5-18','S-1-5-32-544')){
  $id=New-Object Security.Principal.SecurityIdentifier($who)
  $rule=New-Object Security.AccessControl.FileSystemAccessRule($id,[Security.AccessControl.FileSystemRights]::FullControl,[Security.AccessControl.AccessControlType]::Allow)
  $acl.AddAccessRule($rule)
 }
 [JournalNative]::SetOwnedDacl(('\\?\'+$p),$acl.GetSecurityDescriptorBinaryForm(),$sid)
}
function NativeValue($h) {
 $id=New-Object JournalNative+FileId
 if(-not [JournalNative]::GetFileInformationByHandleEx($h,18,[ref]$id,24) -or $id.High -ne 0){throw 'file-id'}
 $attr=New-Object JournalNative+AttributeTag
 if(-not [JournalNative]::Attributes($h,9,[ref]$attr,8)){throw 'attributes'}
 $directory=($attr.Attributes -band 16) -ne 0;[long]$length=0
 if(-not $directory -and -not [JournalNative]::GetFileSizeEx($h,[ref]$length)){throw 'size'}
 $legacy=New-Object JournalNative+LegacyInfo
 if(-not [JournalNative]::GetFileInformationByHandle($h,[ref]$legacy)){throw 'legacy-file-id'}
 [UInt64]$oldId=([UInt64]$legacy.IndexHigh -shl 32) -bor [UInt64]$legacy.IndexLow
 if($oldId -ne $id.Low){throw 'file-id-views'}
 return @{volume=$id.Volume.ToString();nodeDevice=$legacy.Volume.ToString();fileId=$id.Low.ToString();length=$length.ToString();directory=$directory}
}
try {
 $self=[Diagnostics.Process]::GetCurrentProcess()
 $executable=$self.MainModule.FileName
 if($executable -cne [IO.Path]::GetFullPath([string]$v.tool.executable)){throw 'actual-tool-path'}
 $toolHandle=OpenHeld $executable;$tool=NativeValue $toolHandle
 if($tool.directory -or $tool.volume -cne $v.tool.volume -or $tool.fileId -cne $v.tool.fileId -or $tool.length -cne ([string]$v.tool.bytes)){throw 'actual-tool-identity'}
 $parent=[IO.Directory]::GetParent($root).FullName;$p=$parent
 while($p){$h=OpenHeld $p;if($p -ceq $parent){CheckSecurity $h $false};$next=[IO.Directory]::GetParent($p);if($null -eq $next){break};$p=$next.FullName}
 if($v.mode -eq 'initialize'){
  if([IO.Directory]::Exists($root) -or [IO.File]::Exists($root)){throw 'existing-root'}
  [IO.Directory]::CreateDirectory($root)|Out-Null;SetOwnedAcl $root $true
 }elseif(@('verify','protect-created') -cnotcontains $v.mode){throw 'mode'}
 $h=OpenHeld $root;CheckSecurity $h $true;$files=@{root=(NativeValue $h)}
 foreach($name in @($v.names)){
  if(@('key.json','journal.bin','owner.lock') -cnotcontains $name -or $files.ContainsKey($name)){throw 'file-name'}
  $p=[IO.Path]::Combine($root,$name)
  $h=OpenHeld $p
  if($v.mode -eq 'protect-created'){SetOwnedAcl $p $false}
  CheckSecurity $h $true;$entry=NativeValue $h;if($entry.directory){throw 'file-type'};$files[$name]=$entry
 }
 $owner=[Diagnostics.Process]::GetProcessById([int]$v.pid);$self=[Diagnostics.Process]::GetCurrentProcess()
 @{format='lv-native-journal-ready-v1';challenge=$v.challenge;sid=$sid;files=$files;tool=$tool;owner_creation=$owner.StartTime.ToFileTimeUtc().ToString();pid=$self.Id;creation=$self.StartTime.ToFileTimeUtc().ToString()}|ConvertTo-Json -Depth 6 -Compress|%{[Console]::Out.WriteLine($_)}
 $done=([Console]::In.ReadLine()|ConvertFrom-Json)
 if(@($done.PSObject.Properties.Name).Count -ne 2 -or $done.format -ne 'lv-native-journal-done-v1' -or $done.challenge -cne $v.challenge){throw 'done'}
}finally {foreach($h in $handles){$h.Dispose()}}
`;

function digest(value:Uint8Array){return createHash('sha256').update(value).digest('hex')}
function closed(value:unknown,keys:readonly string[]):Record<string,unknown>{
 if(!value||typeof value!=='object'||Array.isArray(value))throw Error('Invalid journal record');
 const r=value as Record<string,unknown>;
 if(Object.keys(r).length!==keys.length||keys.some(k=>!Object.hasOwn(r,k)))throw Error('Invalid journal record fields');
 return r;
}
async function noAlias(path:string){
 const abs=resolve(path);if(abs!==path)throw Error('Canonical journal path required');
 let p=abs;
 for(;;){const s=await lstat(p);if(s.isSymbolicLink())throw Error('Alias in journal path');const parent=dirname(p);if(parent===p)break;p=parent}
}
function optionsCopy(options:JournalOptions):JournalOptions{
 if(process.platform!=='win32')throw Error('This reference custody implementation requires verified Windows');
 const r=resolve(options.root);
 if(r!==options.root||r===parse(r).root||!/^[A-Za-z0-9._-]{1,128}$/.test(options.installation))throw Error('Invalid journal configuration');
 const w=options.windows;
 if(!/^[a-f0-9]{64}$/.test(w.sha256)||!/^S-1-5-21-[0-9-]+$/.test(w.userSID)||!Number.isSafeInteger(w.bytes)||w.bytes<=0)throw Error('Invalid runtime identity');
 return Object.freeze({root:r,installation:options.installation,windows:Object.freeze({...w})});
}
async function verifyTool(options:JournalOptions){
 const w=options.windows,tool=resolve(w.executable);
 await noAlias(tool);
 const fd=await open(tool,'r');
 try{
  const before=await fd.stat({bigint:true});
  if(!before.isFile()||before.size!==BigInt(w.bytes)||before.ino!==BigInt(w.fileId)){
   throw Error('Native tool identity mismatch '+JSON.stringify({actual:{regular:before.isFile(),size:String(before.size),device:String(before.dev),inode:String(before.ino)},expected:{size:w.bytes,device:w.volume,inode:w.fileId}}));
  }
  if(digest(await fd.readFile())!==w.sha256)throw Error('Native tool bytes mismatch');
  const after=await fd.stat({bigint:true});if(after.ino!==before.ino||after.dev!==before.dev||after.size!==before.size||after.mtimeNs!==before.mtimeNs)throw Error('Native tool changed');
  return before;
 }finally{await fd.close()}
}
async function nativePhase(options:JournalOptions,mode:'verify'|'initialize'|'protect-created',names:readonly string[]):Promise<NativePhase>{
 const toolBefore=await verifyTool(options);await noAlias(dirname(options.root));
 const challenge=randomBytes(32).toString('hex');
 const child=spawn(options.windows.executable,['-NoLogo','-NoProfile','-NonInteractive','-Command',nativeScript],{
  windowsHide:true,stdio:['pipe','pipe','pipe'],env:{SystemRoot:process.env['SystemRoot'],PATH:'',TEMP:process.env['TEMP'],TMP:process.env['TMP']}
 });
 const lines=createInterface({input:child.stdout});let output=0,stderr=0,diagnostic='',settled=false,readyDone=false;
 let resolveReady!:(value:NativePhase)=>void,rejectReady!:(reason:Error)=>void;
 const ready=new Promise<NativePhase>((yes,no)=>{resolveReady=yes;rejectReady=no});
 let resolveExit!:(value:number|null)=>void;const exit=new Promise<number|null>(yes=>{resolveExit=yes});
 const timer=setTimeout(()=>{child.kill();rejectReady(Error('Native custody phase timed out'))},10000);
 const fail=(message:string)=>{rejectReady(Error(message));child.kill()};
 child.stderr.on('data',(data:Buffer)=>{stderr+=data.length;if(stderr>16384)fail('Native custody diagnostic limit');else diagnostic+=data.toString('utf8')});
 child.stdout.on('data',(data:Buffer)=>{output+=data.length;if(output>65536)fail('Native custody output limit')});
 child.on('error',()=>{fail('Native custody process unavailable')});
 child.on('close',code=>{clearTimeout(timer);lines.close();resolveExit(code);if(!readyDone)rejectReady(Error('Native custody refused: '+diagnostic))});
 lines.on('line',line=>{
  try{
   if(readyDone)throw Error('Unexpected native custody output');
   const r=closed(JSON.parse(line),['format','challenge','sid','files','tool','owner_creation','pid','creation']);
   if(r['format']!=='lv-native-journal-ready-v1'||r['challenge']!==challenge||r['sid']!==options.windows.userSID||
      r['pid']!==child.pid||typeof r['creation']!=='string'||!/^[0-9]+$/.test(r['creation'])||
      typeof r['owner_creation']!=='string'||!/^[0-9]+$/.test(r['owner_creation']))throw Error('Native challenge/identity mismatch');
   const files=closed(r['files'],['root',...names]) as unknown as Record<string,NativeFile>;
   const tool=r['tool'] as NativeFile;
   for(const [name,f] of Object.entries({...files,tool})){
    closed(f,['volume','nodeDevice','fileId','length','directory']);
    if(![f.volume,f.nodeDevice,f.fileId,f.length].every(v=>typeof v==='string'&&/^[0-9]+$/.test(v))||f.directory!==(name==='root'))throw Error('Native file proof shape');
   }
   if(tool.volume!==options.windows.volume||tool.fileId!==options.windows.fileId||tool.length!==String(options.windows.bytes)||
      tool.nodeDevice!==String(toolBefore.dev))throw Error('Native/Node tool identity bridge failed');
   readyDone=true;
   resolveReady({files,ownerCreation:r['owner_creation'],childPID:child.pid!,childCreation:r['creation'],async finish(){
    if(settled)throw Error('Native phase already released');settled=true;
    child.stdin.end(JSON.stringify({format:'lv-native-journal-done-v1',challenge})+'\n');
    const code=await exit;if(code!==0||stderr!==0)throw Error('Native custody phase did not close cleanly');
   }});
  }catch{fail('Native custody protocol rejected')}
 });
 child.stdin.on('error',()=>{fail('Native custody input closed')});
 child.stdin.write(JSON.stringify({format:'lv-native-journal-phase-v1',challenge,sid:options.windows.userSID,root:options.root,mode,names,pid:process.pid,
  tool:{executable:options.windows.executable,volume:options.windows.volume,fileId:options.windows.fileId,bytes:options.windows.bytes}})+'\n');
 try{return await ready}catch(error){child.kill();await exit;throw error}
}
async function sameFile(fd:FileHandle,proof:NativeFile){
 const s=await fd.stat({bigint:true});
 if(!s.isFile()||s.dev!==BigInt(proof.nodeDevice)||s.ino!==BigInt(proof.fileId)||s.size!==BigInt(proof.length))throw Error('Node/native file identity mismatch');
 return s;
}
async function writeAll(fd:FileHandle,bytes:Uint8Array){
 let offset=0;while(offset<bytes.length){const r=await fd.write(bytes,offset,bytes.length-offset,null);if(r.bytesWritten<=0)throw Error('Journal write made no progress');offset+=r.bytesWritten}
}
/** Explicit fresh synthetic initialization. Normal open never creates a key or fixes ACLs. */
export async function initializeReferenceJournal(input:JournalOptions):Promise<void>{
 const options=optionsCopy(input),phase=await nativePhase(options,'initialize',[]);
 try{
  const key={format:'lv-reference-journal-key-v1',installation:options.installation,key_id:randomBytes(16).toString('hex'),key_base64:randomBytes(32).toString('base64')};
  for(const [name,body] of [['key.json',Buffer.from(JSON.stringify(key)+'\n')],['journal.bin',HEADER]] as const){
   const fd=await open(join(options.root,name),'wx');
   try{await writeAll(fd,body);await fd.sync()}finally{await fd.close()}
  }
 }finally{await phase.finish()}
 const verified=await nativePhase(options,'protect-created',['key.json','journal.bin']);await verified.finish();
}
export class JournalOriginalUnavailable extends Error {constructor(){super("Original is unavailable")}}
interface Key {format:string;installation:string;key_id:string;key_base64:string}
const commands=['createInstance','controlInstance','createQuote','submitTurn','commandPlayerSelfConsent','governMemories','cancelTask'] as const;
const FORMAT='lv-reference-journal-v1';
type Identity=readonly [string,string,string,string,string,string,'public_v1'|'integration_r0_v1'];
interface Original {
 format:typeof FORMAT;kind:'ORIGINAL';seq:string;original_id:string;identity:Identity;
 operation:typeof commands[number];original_key:string;quote_id:string|null;
 request_target:readonly [string,string,string];request_base64:string;request_sha256:string;original_hash:string;
}
interface Outcome {
 format:typeof FORMAT;kind:'OUTCOME';seq:string;original_id:string;original_hash:string;outcome_no:number;
 observation_kind:'RECEIPT'|'TASK'|'UNKNOWN'|'QUERY';status:string|null;
 validated_response_base64:string|null;response_sha256:string|null;association_task_id:string|null;
}
export interface JournalObservation {
 readonly kind:Outcome['observation_kind'];readonly status:string|null;readonly body:unknown|null;readonly taskId:string|null;
}
declare const permitBrand:unique symbol;
export interface FirstSendPermit {readonly [permitBrand]:true}
function originalHash(r:Omit<Original,'original_hash'>|Original){
 return digest(Buffer.from(JSON.stringify([FORMAT===r.format?'lv-reference-original-v1':'',r.identity,r.operation,r.original_key,r.quote_id,r.request_target,r.request_base64,r.request_sha256])));
}
function exactBase64(value:string){
 if(typeof value!=='string'||Buffer.from(value,'base64').toString('base64')!==value)throw Error('Noncanonical record bytes');
 return Buffer.from(value,'base64');
}
function parseJSON(bytes:Uint8Array){return JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(bytes)) as unknown}
const validate=validators as unknown as Record<string,((value:unknown)=>boolean)|undefined>;
function buildOriginal(snapshot:PreparedRequestSnapshot,identity:Identity,installation:string,id:string,seq:string):Original{
 if(!Array.isArray(identity)||identity.length!==7||identity[0]!==installation||identity[6]!==snapshot.profile||
    !identity.every(v=>typeof v==='string'&&/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(v)))throw Error('Journal identity mismatch');
 if(!(commands as readonly string[]).includes(snapshot.operation)||!['public_v1','integration_r0_v1'].includes(snapshot.profile))throw Error('Non-durable operation');
 const desc=transportDescriptors[snapshot.profile][snapshot.operation as typeof commands[number]];
 if(snapshot.method!==desc.method||!(desc.roles as readonly string[]).includes(snapshot.role)||
    !snapshot.path.startsWith('/')||snapshot.path.startsWith('//')||/[#\\\u0000-\u0020]/.test(snapshot.path))throw Error('Prepared destination mismatch');
 if(!snapshot.idempotencyKey||!/^[A-Za-z0-9._-]{1,128}$/.test(snapshot.idempotencyKey)||
    snapshot.contentType!=='application/json'||snapshot.body.length>1048576||digest(snapshot.body)!==snapshot.bodySHA256)throw Error('Prepared bytes mismatch');
 const url=new URL(snapshot.path,'http://journal.invalid');
 if(url.origin!=='http://journal.invalid'||url.pathname+url.search!==snapshot.path)throw Error('Noncanonical destination');
 const expected=desc.path.split('/'),actual=url.pathname.split('/'),path:Record<string,string>={};
 if(expected.length!==actual.length)throw Error('Prepared path mismatch');
 for(let i=0;i<expected.length;i++){
  const name=/^\{([^}]+)\}$/.exec(expected[i]!);
  if(name){const value=decodeURIComponent(actual[i]!);if(encodeURIComponent(value)!==actual[i])throw Error('Noncanonical path component');path[name[1]!]=value}
  else if(expected[i]!==actual[i])throw Error('Prepared path mismatch');
 }
 const query:Record<string,string>={};
 for(const [name,value] of url.searchParams){if(Object.hasOwn(query,name))throw Error('Duplicate prepared query');query[name]=value}
 const body=parseJSON(snapshot.body),headers={'Idempotency-Key':snapshot.idempotencyKey,'Loreviva-Client-Contract':snapshot.profile};
 const input={...(Object.keys(path).length?{path}:{}),...(Object.keys(query).length?{query}:{}),headers,body};
 if(!validate[desc.inputValidator]?.(input))throw Error('Prepared source input rejected');
 const quote=body&&typeof body==='object'&&'quote_id'in body?body.quote_id:null;
 if(quote!==null&&typeof quote!=='string')throw Error('Invalid original quote');
 const result:Original={format:FORMAT,kind:'ORIGINAL',seq,original_id:id,identity:[identity[0],identity[1],identity[2],identity[3],identity[4],identity[5],identity[6]],operation:snapshot.operation as typeof commands[number],
  original_key:snapshot.idempotencyKey,quote_id:quote,request_target:[snapshot.method,snapshot.path,snapshot.profile],
  request_base64:Buffer.from(snapshot.body).toString('base64'),request_sha256:snapshot.bodySHA256,original_hash:''};
 result.original_hash=originalHash(result);return result;
}
function checkOriginal(r:unknown,key:Key):Original{
 const o=closed(r,['format','kind','seq','original_id','identity','operation','original_key','quote_id','request_target','request_base64','request_sha256','original_hash']) as unknown as Original;
 if(o.format!==FORMAT||o.kind!=='ORIGINAL'||!/^[a-f0-9]{32}$/.test(o.original_id)||!Array.isArray(o.request_target)||o.request_target.length!==3)throw Error('Original frame shape');
 const profile=o.request_target[2];if(profile!=='public_v1'&&profile!=='integration_r0_v1')throw Error('Original profile');
 const desc=transportDescriptors[profile][o.operation];if(!desc)throw Error('Original operation');
 const body=exactBase64(o.request_base64);
 const rebuilt=buildOriginal({operation:o.operation,profile,role:desc.roles[0],method:o.request_target[0],path:o.request_target[1],
  idempotencyKey:o.original_key,contentType:'application/json',body,bodySHA256:o.request_sha256},o.identity,key.installation,o.original_id,o.seq);
 if(JSON.stringify(rebuilt)!==JSON.stringify(o))throw Error('Original tuple/hash mismatch');
 return o;
}
function knownTask(rows:readonly Outcome[]):string|null{
 for(let i=rows.length-1;i>=0;i--)if(rows[i]!.association_task_id!==null)return rows[i]!.association_task_id;
 return null;
}
function checkOutcome(r:unknown,original:Original,count:number,establishedTask:string|null=null):Outcome{
 const o=closed(r,['format','kind','seq','original_id','original_hash','outcome_no','observation_kind','status','validated_response_base64','response_sha256','association_task_id']) as unknown as Outcome;
 if(o.format!==FORMAT||o.kind!=='OUTCOME'||o.original_id!==original.original_id||o.original_hash!==original.original_hash||
    o.outcome_no!==count+1||o.outcome_no>32||!['RECEIPT','TASK','UNKNOWN','QUERY'].includes(o.observation_kind)||
    (o.association_task_id!==null&&(typeof o.association_task_id!=='string'||!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(o.association_task_id))))throw Error('Outcome association mismatch');
 if(establishedTask!==null&&o.association_task_id!==null&&o.association_task_id!==establishedTask)throw Error('Original task association changed');
 if(o.validated_response_base64===null){
  if(o.status!==null||o.response_sha256!==null||o.association_task_id!==null||o.observation_kind!=='UNKNOWN')throw Error('Unproven outcome');
 }else{
  const bytes=exactBase64(o.validated_response_base64);
  if(digest(bytes)!==o.response_sha256||typeof o.status!=='string'||!/^[1-5][0-9]{2}$/.test(o.status))throw Error('Outcome bytes mismatch');
  const body=parseJSON(bytes);
  const desc=transportDescriptors[original.identity[6]][o.observation_kind==='QUERY'&&original.operation==='submitTurn'?'getOriginalTurnIntakeReceipt':original.operation];
  const response=(desc.responses as Record<string,{validator:string|null}>)[o.status];
  const validator=o.observation_kind==='TASK'?'schema_TaskView':response?.validator;
  if(!validator||!validate[validator]?.(body))throw Error('Unsafe or invalid outcome body');
  if(o.observation_kind==='QUERY'&&original.operation==='submitTurn'&&o.status==='200'){
   const value=body as {operation_id?:unknown;original_key?:unknown;customer_id?:unknown;caller_subject_id?:unknown;scope?:Record<string,unknown>;
    acceptance?:{quote_id?:unknown;task?:{task_id?:unknown;task_kind?:unknown;scope?:Record<string,unknown>}}};
   const task=value.acceptance?.task;
   if(value.operation_id!=='submitTurn'||value.original_key!==original.original_key||value.customer_id!==original.identity[3]||
      value.caller_subject_id!==original.identity[4]||value.scope?.tenant_id!==original.identity[1]||value.scope?.product_id!==original.identity[2]||
      value.scope?.world_instance_id!==original.identity[5]||value.acceptance?.quote_id!==original.quote_id||!task||
      task.task_id!==o.association_task_id||task.task_kind!=='TURN'||task.scope?.tenant_id!==original.identity[1]||
      task.scope?.product_id!==original.identity[2]||task.scope?.world_instance_id!==original.identity[5])throw Error('Original TURN receipt association mismatch');
  }
  if(o.observation_kind==='TASK'){
   const task=(body as {task?:{task_id?:unknown;task_kind?:unknown;scope?:Record<string,unknown>}}).task;
   const expectedKind=original.operation==='governMemories'?'MEMORY_GOVERNANCE':original.operation==='submitTurn'?'TURN':original.operation==='controlInstance'?'CONTROL':null;
   if(o.status!=='200'||!task||task.task_id!==o.association_task_id||!establishedTask||task.task_id!==establishedTask||
      (expectedKind!==null&&task.task_kind!==expectedKind)||task.scope?.tenant_id!==original.identity[1]||
      task.scope?.product_id!==original.identity[2]||task.scope?.world_instance_id!==original.identity[5])throw Error('Task observation mismatch');
  }
 }
 return o;
}
function decodeJournal(bytes:Buffer,key:Key){
 if(bytes.length<HEADER.length||bytes.length>MAX_TOTAL||!bytes.subarray(0,HEADER.length).equals(HEADER))throw Error('Journal header/limit rejected');
 const originals=new Map<string,Original>(),outcomes=new Map<string,Outcome[]>(),nonces=new Set<string>();
 let offset=HEADER.length,seq=1,previous='0'.repeat(64);
 while(offset<bytes.length){
  if(bytes.length-offset<32)throw Error('Truncated journal frame');
  const length=bytes.readUInt32BE(offset),end=offset+4+12+length+16;
  if(length===0||length>1048576||end>bytes.length)throw Error('Journal frame bound rejected');
  const frame=bytes.subarray(offset,end),nonce=frame.subarray(4,16),nonceText=nonce.toString('hex');
  if(nonces.has(nonceText))throw Error('Repeated journal nonce');
  const decipher=createDecipheriv('aes-256-gcm',Buffer.from(key.key_base64,'base64'),nonce);
  decipher.setAAD(Buffer.from(JSON.stringify([FORMAT,key.installation,key.key_id,String(seq),previous])));
  decipher.setAuthTag(frame.subarray(frame.length-16));
  const plain=Buffer.concat([decipher.update(frame.subarray(16,frame.length-16)),decipher.final()]);
  const value=parseJSON(plain);
  if(!plain.equals(Buffer.from(JSON.stringify(value)))||!value||typeof value!=='object'||!('seq'in value)||value.seq!==String(seq))throw Error('Noncanonical journal frame');
  if('kind'in value&&value.kind==='ORIGINAL'){
   if(originals.size>=128)throw Error('Original capacity exceeded');
   const original=checkOriginal(value,key);if(originals.has(original.original_id))throw Error('Duplicate original');
   if([...originals.values()].some(v=>JSON.stringify([v.identity,v.operation,v.original_key])===JSON.stringify([original.identity,original.operation,original.original_key])))throw Error('Duplicate original association');
   originals.set(original.original_id,original);outcomes.set(original.original_id,[]);
  }else{
   if(!('original_id'in value)||typeof value.original_id!=='string')throw Error('Missing outcome original');
   const original=originals.get(value.original_id);if(!original)throw Error('Unknown outcome original');
   const list=outcomes.get(original.original_id)!;list.push(checkOutcome(value,original,list.length,knownTask(list)));
  }
  nonces.add(nonceText);previous=digest(frame);offset=end;seq++;
 }
 return {originals,outcomes,nonces,seq,previous};
}
async function readOwned(options:JournalOptions,names:readonly string[]){
 const phase=await nativePhase(options,'verify',names),values=new Map<string,Buffer>();
 try{
  for(const name of names){
   const fd=await open(join(options.root,name),'r');
   try{
    const initial=await sameFile(fd,phase.files[name]!);
    if(initial.size>BigInt(name==='journal.bin'?MAX_TOTAL:4096))throw Error('Journal input exceeds cap');
    values.set(name,await fd.readFile());const final=await sameFile(fd,phase.files[name]!);
    if(final.mtimeNs!==initial.mtimeNs)throw Error('Journal input changed during read');
   }finally{await fd.close()}
  }
 }finally{await phase.finish()}
 return {values,phase};
}
export async function openReferenceJournal(input:JournalOptions){
 const options=optionsCopy(input),first=await readOwned(options,['key.json','journal.bin']);
 const key=closed(JSON.parse(first.values.get('key.json')!.toString('utf8')),['format','installation','key_id','key_base64']) as unknown as Key;
 if(key.format!=='lv-reference-journal-key-v1'||key.installation!==options.installation||!/^[a-f0-9]{32}$/.test(key.key_id)||
    typeof key.key_base64!=='string'||Buffer.from(key.key_base64,'base64').length!==32||Buffer.from(key.key_base64,'base64').toString('base64')!==key.key_base64)throw Error('Invalid journal key');
 decodeJournal(first.values.get('journal.bin')!,key);
 const owner=randomBytes(16).toString('hex'),lock=join(options.root,'owner.lock');
 const lockBody=Buffer.from(JSON.stringify({format:'lv-reference-journal-owner-v1',installation:options.installation,owner,pid:process.pid,creation:first.phase.ownerCreation}));
 const fd=await open(lock,'wx');let identity;
 try{await writeAll(fd,lockBody);await fd.sync();identity=await fd.stat({bigint:true})}finally{await fd.close()}
 const protectedLock=await nativePhase(options,'protect-created',['owner.lock']);await protectedLock.finish();
 const locked=await readOwned(options,['key.json','journal.bin','owner.lock']);
 if(!locked.values.get('key.json')!.equals(first.values.get('key.json')!)||!locked.values.get('owner.lock')!.equals(lockBody))throw Error('Journal changed while acquiring owner');
 let bytes=locked.values.get('journal.bin')!,state=decodeJournal(bytes,key),stopped=false,closing=false,poisoned=false,brokerBound=false,queue=Promise.resolve();
 const permits=new WeakMap<object,{original:Original;used:boolean}>();
 function assertOpen(){if(stopped||closing||poisoned)throw Error('Journal closed or uncertain')}
 function serial<T>(action:()=>Promise<T>):Promise<T>{
  assertOpen();const pending=queue.then(()=>{assertOpen();return action()});queue=pending.then(()=>{},()=>{});return pending;
 }
 function ownOriginal(id:string,who:Identity){
  const original=state.originals.get(id);if(!original||JSON.stringify(original.identity)!==JSON.stringify(who))throw new JournalOriginalUnavailable();
  return original;
 }
 async function append(value:Original|Outcome){
  assertOpen();const plain=Buffer.from(JSON.stringify(value));if(plain.length===0||plain.length>1048576)throw Error('Journal frame capacity exceeded');
  let nonce:Buffer;do{nonce=randomBytes(12)}while(state.nonces.has(nonce.toString('hex')));
  const cipher=createCipheriv('aes-256-gcm',Buffer.from(key.key_base64,'base64'),nonce);
  cipher.setAAD(Buffer.from(JSON.stringify([FORMAT,key.installation,key.key_id,String(state.seq),state.previous])));
  const encrypted=Buffer.concat([cipher.update(plain),cipher.final()]),prefix=Buffer.alloc(4);prefix.writeUInt32BE(encrypted.length);
  const frame=Buffer.concat([prefix,nonce,encrypted,cipher.getAuthTag()]);
  if(bytes.length+frame.length>MAX_TOTAL)throw Error('Journal total capacity exceeded');
  const current=await readOwned(options,['key.json','journal.bin','owner.lock']);
  if(!current.values.get('key.json')!.equals(first.values.get('key.json')!)||!current.values.get('journal.bin')!.equals(bytes)||!current.values.get('owner.lock')!.equals(lockBody))throw Error('Journal phase drift');
  const phase=await nativePhase(options,'verify',['key.json','journal.bin','owner.lock']);let handle:FileHandle|undefined;
  try{
   handle=await open(join(options.root,'journal.bin'),'a');await sameFile(handle,phase.files['journal.bin']!);
   if(phase.files['journal.bin']!.length!==String(bytes.length))throw Error('Journal append position changed');
   await writeAll(handle,frame);await handle.sync();
   const after=await handle.stat({bigint:true});
   if(after.dev!==BigInt(phase.files['journal.bin']!.nodeDevice)||after.ino!==BigInt(phase.files['journal.bin']!.fileId)||after.size!==BigInt(bytes.length+frame.length))throw Error('Journal append identity changed');
   await handle.close();handle=undefined;await phase.finish();
  }catch(error){poisoned=true;await handle?.close().catch(()=>{});await phase.finish().catch(()=>{});throw error}
  bytes=Buffer.concat([bytes,frame]);state=decodeJournal(bytes,key);
 }
 return Object.freeze({
  findOriginal(who:Identity,operation:string,originalKey:string):string|null{
   assertOpen();
   if(!Array.isArray(who)||who.length!==7||who[0]!==key.installation||
      !who.every(v=>typeof v==='string'&&/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(v))||
      !(commands as readonly string[]).includes(operation)||typeof originalKey!=='string'||!/^[A-Za-z0-9._-]{1,128}$/.test(originalKey))throw Error('Invalid original locator');
   const matches=[...state.originals.values()].filter(r=>JSON.stringify(r.identity)===JSON.stringify(who)&&r.operation===operation&&r.original_key===originalKey);
   if(matches.length>1)throw Error('Ambiguous original association');
   return matches[0]?.original_id??null;
  },
  list(who?:Identity){assertOpen();return [...state.originals.values()].filter(r=>who&&JSON.stringify(r.identity)===JSON.stringify(who)).map(r=>Object.freeze({original_id:r.original_id,operation:r.operation,original_hash:r.original_hash,outcomes:state.outcomes.get(r.original_id)!.length}))},
  bindBroker(){
   assertOpen();if(brokerBound)throw Error('Journal broker already bound');brokerBound=true;
   return Object.freeze({
    commitPrepared(snapshot:PreparedRequestSnapshot,who:Identity):Promise<{originalId:string;permit:FirstSendPermit}>{
     // Copy synchronously before queueing: later DTO/buffer mutations cannot change the durable request.
     const copy={...snapshot,body:snapshot.body.slice()},identityCopy=[...who] as Identity;
     return serial(async()=>{
      if(state.originals.size>=128)throw Error('Original capacity exceeded');
      const original=buildOriginal(copy,identityCopy,key.installation,randomBytes(16).toString('hex'),String(state.seq));
      if([...state.originals.values()].some(r=>JSON.stringify([r.identity,r.operation,r.original_key])===JSON.stringify([original.identity,original.operation,original.original_key])))throw Error('Original already exists; query only');
      await append(original);const permit=Object.freeze({}) as FirstSendPermit;permits.set(permit,{original,used:false});
      return Object.freeze({originalId:original.original_id,permit});
     });
    },
    consumeFirstSendPermit(permit:FirstSendPermit,snapshot:PreparedRequestSnapshot,who:Identity){
     assertOpen();const p=permits.get(permit);if(!p||p.used)throw Error('Foreign or consumed first-send permit');
     const expected=buildOriginal(snapshot,who,key.installation,p.original.original_id,p.original.seq);
     if(expected.original_hash!==p.original.original_hash)throw Error('First-send request mismatch');p.used=true;
    }
   });
  },
  observe(id:string,who:Identity,observation:JournalObservation):Promise<void>{
   const identityCopy=[...who] as Identity,raw=observation.body===null?null:Buffer.from(JSON.stringify(observation.body));
   const data={...observation};
   return serial(async()=>{
    const original=ownOriginal(id,identityCopy),count=state.outcomes.get(id)!.length;
    const value:Outcome={format:FORMAT,kind:'OUTCOME',seq:String(state.seq),original_id:id,original_hash:original.original_hash,outcome_no:count+1,
     observation_kind:data.kind,status:data.status,validated_response_base64:raw?.toString('base64')??null,response_sha256:raw?digest(raw):null,association_task_id:data.taskId};
    checkOutcome(value,original,count,knownTask(state.outcomes.get(id)!));await append(value);
   });
  },
  original(id:string,who:Identity){
   assertOpen();const r=ownOriginal(id,who),rows=state.outcomes.get(id)!,last=rows.at(-1);
   const request=parseJSON(exactBase64(r.request_base64)) as Record<string,unknown>;
   const taskAccess=r.operation==='controlInstance'?
    (request.command==='START'||request.command==='END'?'SERVICE':request.command==='PAUSE'||request.command==='RESUME'?'CLIENT':'UNKNOWN'):
    (r.operation==='submitTurn'||r.operation==='governMemories'||r.operation==='cancelTask'?'CLIENT':'UNKNOWN');
   return Object.freeze({task_access:taskAccess,original_id:id,operation:r.operation,original_hash:r.original_hash,original_key:r.original_key,
    request_target:Object.freeze([...r.request_target]),state:!last?'PREPARED' as const:last.observation_kind!=='UNKNOWN'?'OBSERVED' as const:'UNKNOWN' as const,
    task_id:knownTask(rows),observation:last?.validated_response_base64?{status:Number(last.status),body:parseJSON(exactBase64(last.validated_response_base64))}:null});
  },
  async close(){
   if(stopped)return;if(closing)throw Error('Journal Close already in progress');closing=true;await queue;stopped=true;if(poisoned)throw Error('Uncertain journal retains its owner lock');
   const current=await readOwned(options,['key.json','journal.bin','owner.lock']);
   if(!current.values.get('owner.lock')!.equals(lockBody))throw Error('Owned lock changed');
   const stat=await lstat(lock,{bigint:true});
   if(stat.isSymbolicLink()||stat.ino!==identity.ino||stat.dev!==identity.dev)throw Error('Owned lock replaced');
   // The next phase holds the parent/key/journal. The exact lock is unlinked only after its read handle closes.
   const phase=await nativePhase(options,'verify',['key.json','journal.bin']);
   try{const exact=await lstat(lock,{bigint:true});if(exact.ino!==identity.ino||exact.dev!==identity.dev)throw Error('Owned lock replaced');await unlink(lock)}
   finally{await phase.finish()}
  }
 });
}
