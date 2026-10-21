package mysql

import (
 "bytes"
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "io"
 "loreviva.local/loreviva/internal/domain"
 "reflect"
 "sort"
 "time"
 "unicode/utf8"
)

const integrationBirthSchema = "IntegrationMemoryBirthV1"
const integrationBirthOperation = "INTEGRATION_MEMORY_BIRTH_V1"
const integrationBirthFormat = "loreviva.integration-memory-birth.v1"
const integrationBirthMaxBytes = 16<<20
const integrationBirthTotalBytes = 32<<20

// IntegrationMemoryBirthBudget is pure byte accounting, not a proof or an
// authority mint. All callers retain their canonical, owner and member checks.
// Check the remaining budget before addition, including adversarial uint64
// inputs; neither wrapping nor clamping may turn an oversized history into one
// that fits. The same arithmetic bounds writer, decoder and complete readers.
func IntegrationMemoryBirthBudget(total, documentBytes uint64) (uint64, error) {
	if documentBytes == 0 || documentBytes > integrationBirthMaxBytes || total > integrationBirthTotalBytes || documentBytes > integrationBirthTotalBytes-total {
		return 0, ErrDenied
	}
	return total + documentBytes, nil
}

type IntegrationBirthSourceFields struct {
 ID, Domain, Audience string
 Uses []string
 Revision uint64
 Blocked bool
 Epoch string
 Intake uint64
 OccurredAt time.Time
 EstimatedTime bool
}
type IntegrationBirthSource struct {
 IntegrationBirthSourceFields
 BodyRawSHA256 string
}
type IntegrationBirthNodeFields struct {
 ID, Kind, ActorID, TargetKind, TargetID, Corrects, Domain, Audience string
 Uses, Sources, Parents, MediaRefs []string
 Revision uint64
 Blocked bool
 ExpiresAt time.Time
}
type IntegrationBirthNode struct {
 IntegrationBirthNodeFields
 BodyStringJSONSHA256 string
}
type IntegrationBirthGrant struct { ActorID, SourceID string; Uses []string; Revoked bool; ExpiresAt time.Time }
type IntegrationBirthAcquisition struct { ActorID, SourceID, Kind, EvidenceID string }
type integrationBirthRawSource struct { IntegrationBirthSourceFields; Body string }
type integrationBirthRawNode struct { IntegrationBirthNodeFields; Body string }
type integrationBirthAggregate struct {
 Sources map[string]integrationBirthRawSource
 Nodes map[string]integrationBirthRawNode
 Grants []IntegrationBirthGrant
 Acquisitions []IntegrationBirthAcquisition
 Disabled []string
 Epoch string
 Intake, PolicyRevision uint64
}
type IntegrationBirthReceipt struct {
 Key,Tenant,Product,Owner,Scope,World,Task,Operation,Command,Phase,Hash string
 TaskNullable bool
}
type IntegrationMemoryBirthDocument struct {
 Format,Kind,ProofID,TenantID,ProductID,WorldID,DataSubjectID,AggregateID,InstallationID,CreationID,ReleaseID string
 TaskID,ActorID string
 Original IntegrationBirthReceipt
 BeforeAggregateRevision,AfterAggregateRevision,BeforeIntake,AfterIntake uint64
 Epoch string
 PolicyRevision uint64
 Disabled []string
 ObservedAt time.Time
 BeforeCommitment,AfterCommitment string
 InputSourceID,InputHash,QuoteID,SessionID,ServiceID string
 Registration integrationRegistrationProof
 UseGeneration,ConsentRevision uint64
 UseExpires,SessionExpires time.Time
 AttemptID,ResultID string
 Metadata *domain.MockResultMetadata
 OriginalDependency *domain.MockResultMetadata
 MemoryIDs []string
 SourcesBorn []IntegrationBirthSource
 NodesBorn []IntegrationBirthNode
 AddedGrants []IntegrationBirthGrant
 AddedAcquisitions []IntegrationBirthAcquisition
 ParentNodes []IntegrationBirthNode
 ParentSources []IntegrationBirthSource
}
type IntegrationMemoryBirthEvidence struct { Document []byte; DocumentSHA256 [32]byte; Original,Companion CommitReceipt }
type integrationMemoryBirthPlan struct {
 store *Store
 unit *unit
 kind,proofID,task,actor string
 request BarrierRequest
 beforeID string
 beforeRevision,afterRevision uint64
 beforeRaw,afterRaw,document []byte
 observed, pendingObserved time.Time
 observations int
 active,started,sealed,closed bool
 input domain.TurnInput
 commit *domain.CandidateCommit
}
func birthHash(raw []byte)string{h:=sha256.Sum256(raw);return hex.EncodeToString(h[:])}
func birthJSONHash(v any)string{raw,_:=json.Marshal(v);return birthHash(raw)}
func birthReceipt(r BarrierRequest)IntegrationBirthReceipt{
 k,_,w:=scopeOf(r); task:=r.TaskID;if r.commandOnly{task=""}
 return IntegrationBirthReceipt{r.CommitKey,r.ProductScope.TenantID,r.ProductScope.ProductID,r.OwnerSubjectID,k,w,task,r.Operation,r.CommandKey,receiptPhase(r),hex.EncodeToString(r.PayloadHash[:]),r.commandOnly}
}
func birthStoredReceipt(r receiptRow)IntegrationBirthReceipt{return IntegrationBirthReceipt{r.key,r.tenant,r.product,r.owner,r.kind,r.world.String,r.task,r.operation,r.command,r.phase,hex.EncodeToString(r.hash),!r.nullTask.Valid}}
func birthSource(v integrationBirthRawSource)IntegrationBirthSource{return IntegrationBirthSource{v.IntegrationBirthSourceFields,birthHash([]byte(v.Body))}}
func birthNode(v integrationBirthRawNode)IntegrationBirthNode{return IntegrationBirthNode{v.IntegrationBirthNodeFields,birthJSONHash(v.Body)}}
func birthDecode(raw []byte,v any)error{
 if _,e:=IntegrationMemoryBirthBudget(0,uint64(len(raw)));e!=nil{return e};if !utf8.Valid(raw){return ErrDenied}
 // A canonical round trip rejects duplicate members, unknown members and
 // alternate encodings in the retained private document, not aggregate JSON.
 d:=json.NewDecoder(bytes.NewReader(raw));d.DisallowUnknownFields();if d.Decode(v)!=nil{return ErrDenied}
 var extra any;if d.Decode(&extra)!=io.EOF{return ErrDenied};return nil
}
func birthAggregate(raw []byte)(integrationBirthAggregate,error){var a integrationBirthAggregate;e:=birthDecode(raw,&a);if e!=nil||a.Epoch==""||a.Sources==nil||a.Nodes==nil{return a,ErrDenied};return a,nil}
func birthStructural(a integrationBirthAggregate)string{
 type projection struct{Sources map[string]IntegrationBirthSource;Nodes map[string]IntegrationBirthNode;Grants []IntegrationBirthGrant;Acquisitions []IntegrationBirthAcquisition;Disabled []string;Epoch string;Intake,PolicyRevision uint64}
 p:=projection{map[string]IntegrationBirthSource{},map[string]IntegrationBirthNode{},a.Grants,a.Acquisitions,a.Disabled,a.Epoch,a.Intake,a.PolicyRevision}
 for id,v:=range a.Sources{p.Sources[id]=birthSource(v)};for id,v:=range a.Nodes{p.Nodes[id]=birthNode(v)};return birthJSONHash(p)
}
func birthMemoryKey(s domain.Scope)LockKey{return LockKey{Rank:90,Kind:"memory",IDs:[]string{s.TenantID,s.ProductID,s.WorldInstanceID,creationID("MEMORY_SUBJECT",s.WorldInstanceID,s.SubjectID)}}}
func(p *integrationMemoryBirthPlan)key()LockKey{return LockKey{Rank:90,Kind:"content_check",IDs:[]string{p.request.ProductScope.TenantID,p.request.ProductScope.ProductID,p.proofID}}}
func(s *Store)planIntegrationMemoryBirth(r *BarrierRequest)error{
 if r.integrationMemoryBirth!=nil{return ErrDenied}
 kind:="";if r.Operation=="HARNESS_INTAKE"{kind="INPUT"};if r.Operation=="HARNESS_TURN_COMMIT"{kind="DERIVED"};if kind==""{return nil}
 a:=r.integrationTurn
 if a==nil{return nil} // Legacy/rehearsal remains on its original typed route.
 if a.store!=s||r.WorldScope==nil||*r.WorldScope!=a.scope||r.OwnerSubjectID!=a.scope.SubjectID||r.rehearsalExecution!=nil{return ErrDenied}
 if kind=="INPUT"&&(!r.commandOnly||a.lease!=nil||s.TurnOwner==nil){return ErrDenied}
 if kind=="DERIVED"&&(r.commandOnly||a.lease==nil||s.CandidateOwner==nil||r.TaskID!=a.task||r.localCandidateRound==nil){return ErrDenied}
 task:=a.task;if kind=="INPUT"{task=turnIdentity(a.scope.TenantID,a.scope.ProductID,a.scope.SubjectID,a.scope.WorldInstanceID,"TURN",a.key);if a.action!="TURN"||r.CommitKey!=turnIdentity(task,"accepted"){return ErrDenied}}
 p:=&integrationMemoryBirthPlan{store:s,kind:kind,task:task,request:*r}
 p.proofID=turnIdentity(r.ProductScope.TenantID,r.ProductScope.ProductID,r.CommitKey,"integration-memory-birth-v1",kind)
 r.integrationMemoryBirth=p;r.Locks=append(r.Locks,p.key(),LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{p.proofID}})
 return nil
}
func(s *Store)acquireIntegrationMemoryBirth(ctx context.Context,tx *sql.Tx,r BarrierRequest,k LockKey)(bool,error){
 p:=r.integrationMemoryBirth;if p==nil||!reflect.DeepEqual(k,p.key()){return false,nil}
 if p.store!=s||p.unit==nil||p.unit.tx!=tx{return true,ErrDenied};if e:=marker(ctx,tx,k);e!=nil{return true,e}
 return true,studioLockRows(ctx,tx,"SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE",k.IDs[0],k.IDs[1],k.IDs[2])
}
func(p *integrationMemoryBirthPlan)load(ctx context.Context)(string,uint64,[]byte,error){
 var id string;var rev uint64;var raw []byte;r:=p.request
 e:=p.unit.tx.QueryRowContext(ctx,"SELECT aggregate_id,revision,document FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?",r.ProductScope.TenantID,r.ProductScope.ProductID,r.WorldScope.WorldInstanceID,r.OwnerSubjectID).Scan(&id,&rev,&raw)
 if e!=nil{return id,rev,raw,e};if id!=birthMemoryKey(*r.WorldScope).IDs[3]||rev==0||len(raw)>integrationBirthMaxBytes{return id,rev,raw,ErrDenied};return id,rev,raw,nil
}
func(u *unit)beginIntegrationMemoryBirth(ctx context.Context,kind string,input domain.TurnInput)error{
 p:=u.integrationMemoryBirth;if p==nil{return nil}
 if p.closed||p.started||p.active||p.unit!=u||p.store!=u.operatorStore||p.kind!=kind||p.request.Operation!=u.operation||p.request.integrationTurn!=u.integrationTurn{return ErrDenied}
 if kind=="INPUT"&&(u.acceptedTurnID!=p.task||input.TaskID!=p.task||input.Scope!=*p.request.WorldScope){return ErrDenied}
 if kind=="DERIVED"&&(u.commandOnly||u.taskID!=p.task){return ErrDenied}
 if e:=u.RequireLock(birthMemoryKey(*p.request.WorldScope));e!=nil{return e}
 var e error;p.beforeID,p.beforeRevision,p.beforeRaw,e=p.load(ctx);if e!=nil{return e};p.input=input;p.active=true;p.started=true;return nil
}
// Only the actual producer's successful UTC Scan, while the same unit's owner
// invocation is active, can populate this timestamp. Evaluate is outside it.
func(u *unit)observeIntegrationBirthUTC(query string,args []any)error{
 p:=u.integrationMemoryBirth;if p==nil||!p.active||query!="select utc_timestamp(6)"{return nil}
 if p.unit!=u||p.closed||p.sealed||len(args)!=1{return ErrDenied};v,ok:=args[0].(*time.Time);if !ok||v.IsZero(){return ErrDenied}
 p.pendingObserved=*v;return nil
}
// The validation call inside ApplyOriginalCandidate also samples UTC. Only
// the immediately preceding successful Scan consumed by the actual memory save
// is latched, so Evaluate never mints an observed producer time.
func(u *unit)recordIntegrationBirthSave(table string)error{
 p:=u.integrationMemoryBirth;if p==nil||!p.active||table!="memory_aggregates"{return nil};if p.unit!=u||p.pendingObserved.IsZero()||p.observations!=0{return ErrDenied};p.observed=p.pendingObserved;p.observations=1;return nil
}
func(u *unit)sealIntegrationMemoryBirth(ctx context.Context,committed *domain.CandidateCommit)error{
 p:=u.integrationMemoryBirth;if p==nil{return nil};if p.unit!=u||!p.active||p.sealed||p.closed{return ErrDenied};p.active=false
 id,rev,raw,e:=p.load(ctx);if e!=nil{return e};if id!=p.beforeID{return ErrDenied};p.afterRevision=rev;p.afterRaw=append([]byte(nil),raw...)
 before,e:=birthAggregate(p.beforeRaw);if e!=nil{return e};after,e:=birthAggregate(raw);if e!=nil{return e}
 a:=p.request.integrationTurn
 d:=IntegrationMemoryBirthDocument{Format:integrationBirthFormat,Kind:p.kind,ProofID:p.proofID,TenantID:a.scope.TenantID,ProductID:a.scope.ProductID,WorldID:a.scope.WorldInstanceID,DataSubjectID:a.scope.SubjectID,AggregateID:id,InstallationID:a.eligibility.identity.Installation,CreationID:a.scope.WorldInstanceID,ReleaseID:a.eligibility.entitlement.Release,TaskID:p.task,Original:birthReceipt(p.request),BeforeAggregateRevision:p.beforeRevision,AfterAggregateRevision:rev,BeforeIntake:before.Intake,AfterIntake:after.Intake,Epoch:before.Epoch,PolicyRevision:before.PolicyRevision,Disabled:append([]string(nil),before.Disabled...),ObservedAt:p.observed,BeforeCommitment:birthStructural(before),AfterCommitment:birthStructural(after),QuoteID:a.quote,SessionID:a.session.ID,ServiceID:a.service.principal.PrincipalID,Registration:integrationRegistrationProof{a.session.CustomerRevision,a.session.MappingRevision,a.session.EntitlementRevision,a.session.Entitlement,a.session.Installation,a.scope.WorldInstanceID},UseGeneration:a.use.Generation,ConsentRevision:a.use.Revision,UseExpires:a.use.Expires,SessionExpires:a.session.Expires}
 if before.Epoch!=after.Epoch||before.PolicyRevision!=after.PolicyRevision||!reflect.DeepEqual(before.Disabled,after.Disabled){return ErrDenied}
 for id,v:=range before.Sources{if !reflect.DeepEqual(v,after.Sources[id]){return ErrDenied}}
 for id,v:=range before.Nodes{if !reflect.DeepEqual(v,after.Nodes[id]){return ErrDenied}}
 if len(after.Sources)<len(before.Sources)||len(after.Nodes)<len(before.Nodes)||len(after.Grants)<len(before.Grants)||len(after.Acquisitions)<len(before.Acquisitions){return ErrDenied}
 if !reflect.DeepEqual(before.Grants,after.Grants[:len(before.Grants)])||!reflect.DeepEqual(before.Acquisitions,after.Acquisitions[:len(before.Acquisitions)]){return ErrDenied}
 d.AddedGrants=append([]IntegrationBirthGrant{},after.Grants[len(before.Grants):]...);d.AddedAcquisitions=append([]IntegrationBirthAcquisition{},after.Acquisitions[len(before.Acquisitions):]...)
 if p.kind=="INPUT"{
  in:=p.input;d.ActorID=in.ActorID;d.InputSourceID=in.SourceID;d.InputHash=birthHash([]byte(in.Body));nodeID:=creationID(in.SourceID,in.ActorID,"turn-input")
  source,ok:=after.Sources[in.SourceID];node,nok:=after.Nodes[nodeID]
  if !ok||!nok||before.Intake==^uint64(0)||after.Intake!=before.Intake+1||p.beforeRevision==^uint64(0)||rev!=p.beforeRevision+1||len(after.Sources)!=len(before.Sources)+1||len(after.Nodes)!=len(before.Nodes)+1||p.observations!=1{return ErrDenied}
  wantSource:=integrationBirthRawSource{IntegrationBirthSourceFields:IntegrationBirthSourceFields{ID:in.SourceID,Domain:"PLAYER_STATEMENT",Audience:"PRIVATE",Uses:[]string{"PRIVATE_RESPONSE"},Revision:1,Epoch:before.Epoch,Intake:after.Intake,OccurredAt:p.observed},Body:in.Body}
  wantNode:=integrationBirthRawNode{IntegrationBirthNodeFields:IntegrationBirthNodeFields{ID:nodeID,Kind:"EXPERIENCE",ActorID:in.ActorID,TargetKind:"PLAYER",TargetID:in.Scope.SubjectID,Domain:"PLAYER_STATEMENT",Audience:"PRIVATE",Uses:[]string{"PRIVATE_RESPONSE"},Sources:[]string{in.SourceID},Revision:1},Body:in.Body}
  if !reflect.DeepEqual(source,wantSource)||!reflect.DeepEqual(node,wantNode)||!reflect.DeepEqual(d.AddedGrants,[]IntegrationBirthGrant{{ActorID:in.ActorID,SourceID:in.SourceID,Uses:[]string{"PRIVATE_RESPONSE"}}})||!reflect.DeepEqual(d.AddedAcquisitions,[]IntegrationBirthAcquisition{{in.ActorID,in.SourceID,"TOLD",p.task}}){return ErrDenied}
  d.SourcesBorn=[]IntegrationBirthSource{birthSource(source)};d.NodesBorn=[]IntegrationBirthNode{birthNode(node)};d.MemoryIDs=[]string{nodeID}
 }else{
  if committed==nil||committed.Metadata.Identity.TaskID!=p.task||committed.Metadata.Identity.AttemptID==""||committed.ReceiptKey!=p.request.CommitKey||after.Intake!=before.Intake||len(after.Sources)!=len(before.Sources)||len(d.AddedGrants)!=0{return ErrDenied}
  p.commit=creationCopy(committed);d.Metadata=creationCopy(&committed.Metadata);d.OriginalDependency=creationCopy(committed.OriginalDependency);d.AttemptID=committed.Metadata.Identity.AttemptID;d.ResultID=committed.ResultID;d.MemoryIDs=append([]string{},committed.MemoryIDs...)
  if len(d.MemoryIDs)==0{if rev!=p.beforeRevision||len(after.Nodes)!=len(before.Nodes)||p.observations!=0||len(d.AddedAcquisitions)!=0{return ErrDenied}}else if p.beforeRevision==^uint64(0)||rev!=p.beforeRevision+1||p.observations!=1||len(after.Nodes)!=len(before.Nodes)+len(d.MemoryIDs){return ErrDenied}
  seen:=map[string]bool{};parents:=map[string]bool{};sources:=map[string]bool{};expectedAcq:=[]IntegrationBirthAcquisition{}
  for _,id:=range d.MemoryIDs{
   node,ok:=after.Nodes[id];_,old:=before.Nodes[id];if !ok||old||seen[id]||node.ID!=id||node.Domain!="ACTOR_BELIEF"||node.Revision!=1||len(node.Parents)==0||len(node.MediaRefs)!=0||node.Corrects!=""||node.Blocked{return ErrDenied};seen[id]=true
   for _,parent:=range node.Parents{v,exists:=before.Nodes[parent];if exists{parents[parent]=true;for _,source:=range v.Sources{sources[source]=true}}else if !seen[parent]||parent==id{return ErrDenied}}
   for _,source:=range node.Sources{if _,ok:=before.Sources[source];!ok{return ErrDenied};sources[source]=true;expectedAcq=append(expectedAcq,IntegrationBirthAcquisition{node.ActorID,source,"INFERRED",id})}
   d.NodesBorn=append(d.NodesBorn,birthNode(node))
  }
  if !reflect.DeepEqual(expectedAcq,d.AddedAcquisitions){return ErrDenied}
  ids:=[]string{};for id:=range parents{ids=append(ids,id)};sort.Strings(ids);for _,id:=range ids{d.ParentNodes=append(d.ParentNodes,birthNode(before.Nodes[id]))}
  ids=nil;for id:=range sources{ids=append(ids,id)};sort.Strings(ids);for _,id:=range ids{d.ParentSources=append(d.ParentSources,birthSource(before.Sources[id]))}
 }
 if p.kind=="DERIVED" { if e=u.tx.QueryRowContext(ctx,"SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?",d.TenantID,d.ProductID,d.TaskID).Scan(&d.ActorID);e!=nil{return e} };p.actor=d.ActorID;p.document,e=json.Marshal(d);if e!=nil{return ErrDenied};if _,e=IntegrationMemoryBirthBudget(0,uint64(len(p.document)));e!=nil{return e}
 if e=u.RequireLock(p.key());e!=nil{return e}
 if _,e=u.tx.ExecContext(ctx,"INSERT INTO admin_results(tenant_id,product_id,result_id,owner_subject_id,schema_name,document) VALUES(?,?,?,?,?,?)",d.TenantID,d.ProductID,p.proofID,d.DataSubjectID,integrationBirthSchema,p.document);e!=nil{return e}
 h:=sha256.Sum256(p.document)
 if e=u.RequireLock(LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{p.proofID}});e!=nil{return e}
 _,e=u.tx.ExecContext(ctx,"INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,'WORLD',?,?,?,?,?,'COMMIT')",p.proofID,d.TenantID,d.ProductID,d.DataSubjectID,d.WorldID,p.task,integrationBirthOperation,p.proofID,h[:]);if e!=nil{return e};p.sealed=true;return nil
}
func(s *Store)checkIntegrationMemoryBirthFinal(ctx context.Context,u *unit,r BarrierRequest)error{
 p:=r.integrationMemoryBirth;if p==nil{return nil}
 if p.store!=s||p.unit!=u||u.integrationMemoryBirth!=p||u.tx==nil||p.active||!p.started||!p.sealed||p.closed||u.operation!=r.Operation||p.request.CommitKey!=r.CommitKey||p.request.PayloadHash!=r.PayloadHash{return ErrDenied}
 id,rev,raw,e:=p.load(ctx);if e!=nil||id!=p.beforeID||rev!=p.afterRevision||!bytes.Equal(raw,p.afterRaw){return ErrDenied}
 original,e:=queryReceipt(ctx,u.tx,r.CommitKey);if e!=nil||!original.matches(r){return ErrDenied}
 var document []byte;var owner,schema string
 e=u.tx.QueryRowContext(ctx,"SELECT owner_subject_id,schema_name,document FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=?",r.ProductScope.TenantID,r.ProductScope.ProductID,p.proofID).Scan(&owner,&schema,&document)
 if e!=nil||owner!=r.OwnerSubjectID||schema!=integrationBirthSchema||!bytes.Equal(document,p.document){return ErrDenied}
 companion,e:=queryReceipt(ctx,u.tx,p.proofID);h:=sha256.Sum256(document)
 if e!=nil||companion.key!=p.proofID||companion.tenant!=r.ProductScope.TenantID||companion.product!=r.ProductScope.ProductID||companion.owner!=r.OwnerSubjectID||companion.kind!="WORLD"||!companion.world.Valid||companion.world.String!=r.WorldScope.WorldInstanceID||!companion.nullTask.Valid||companion.task!=p.task||companion.operation!=integrationBirthOperation||companion.command!=p.proofID||companion.phase!="COMMIT"||!bytes.Equal(companion.hash,h[:]){return ErrDenied}
 if p.kind=="INPUT"{var source,actor string;var hash []byte;e=u.tx.QueryRowContext(ctx,"SELECT input_source_id,actor_id,input_hash FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?",r.ProductScope.TenantID,r.ProductScope.ProductID,p.task).Scan(&source,&actor,&hash);sum:=sha256.Sum256([]byte(p.input.Body));if e!=nil||source!=p.input.SourceID||actor!=p.input.ActorID||!bytes.Equal(hash,sum[:]){return ErrDenied}}else{var stored []byte;var got domain.CandidateCommit;e=u.tx.QueryRowContext(ctx,"SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?",r.ProductScope.TenantID,r.ProductScope.ProductID,p.task).Scan(&stored);if e!=nil||json.Unmarshal(stored,&got)!=nil||!reflect.DeepEqual(&got,p.commit){return ErrDenied}}
 return nil
}


type integrationBirthReadStamp struct {
 ProofID string
 Document,Commit []byte
 History [32]byte
 Original,Companion IntegrationBirthReceipt
}
// Enumeration includes every original TURN and every private birth row in this
// namespace. Filtering response items cannot conceal a missing producer.
func(s *Store)integrationMemoryBirths(ctx context.Context,tx *sql.Tx,r *IntegrationMemoryRead)([]integrationBirthReadStamp,[]LockKey,error){
 t,p,w,self:=r.scope.TenantID,r.scope.ProductID,r.scope.WorldInstanceID,r.scope.SubjectID
 rows,e:=tx.QueryContext(ctx,"SELECT task_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=? ORDER BY task_id",t,p,w,self);if e!=nil{return nil,nil,e}
 tasks:=[]string{};for rows.Next(){var id string;if e=rows.Scan(&id);e!=nil{rows.Close();return nil,nil,e};tasks=append(tasks,id);if len(tasks)>1000{rows.Close();return nil,nil,ErrDenied}};e=rows.Err();rows.Close();if e!=nil{return nil,nil,e}
 var count int
 if e=tx.QueryRowContext(ctx,"SELECT COUNT(*) FROM integration_task_principals WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND player_subject_id=? AND task_kind='TURN'",t,p,w,self).Scan(&count);e!=nil||count!=len(tasks){return nil,nil,ErrDenied}
 docs:=map[string][]byte{};var total uint64
 rows,e=tx.QueryContext(ctx,"SELECT result_id,document FROM admin_results WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND schema_name=? ORDER BY result_id",t,p,self,integrationBirthSchema);if e!=nil{return nil,nil,e}
 for rows.Next(){if ctx.Err()!=nil{rows.Close();return nil,nil,ctx.Err()};var id string;var raw []byte;if e=rows.Scan(&id,&raw);e!=nil{rows.Close();return nil,nil,e}
  var d IntegrationMemoryBirthDocument;if birthDecode(raw,&d)!=nil{rows.Close();return nil,nil,ErrDenied};canonical,_:=json.Marshal(d);if !bytes.Equal(raw,canonical)||d.Format!=integrationBirthFormat||d.TenantID!=t||d.ProductID!=p||d.DataSubjectID!=self||d.ProofID!=id{rows.Close();return nil,nil,ErrDenied}
  if d.WorldID!=w{continue};total,e=IntegrationMemoryBirthBudget(total,uint64(len(raw)));if e!=nil{rows.Close();return nil,nil,e};docs[id]=append([]byte(nil),raw...)
 };e=rows.Err();rows.Close();if e!=nil{return nil,nil,e}
 out:=[]integrationBirthReadStamp{};keys:=[]LockKey{}
 for _,task:=range tasks{
  a,e:=s.integrationTaskPlan(ctx,tx,task);if e!=nil||a.control!=nil||a.scope!=r.scope{return nil,nil,ErrDenied};keys=append(keys,a.keys...)
  var inputSource,actor string;var inputHash []byte
  if e=tx.QueryRowContext(ctx,"SELECT input_source_id,actor_id,input_hash FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?",t,p,task).Scan(&inputSource,&actor,&inputHash);e!=nil||len(inputHash)!=32{return nil,nil,ErrDenied}
  var commitRaw []byte;var commit domain.CandidateCommit
  ce:=tx.QueryRowContext(ctx,"SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?",t,p,task).Scan(&commitRaw)
  if ce!=nil&&ce!=sql.ErrNoRows{return nil,nil,ce};hasCommit:=ce==nil
  if hasCommit&&(json.Unmarshal(commitRaw,&commit)!=nil||commit.ReceiptKey!=turnIdentity(task,"world-commit")||commit.Metadata.Identity.TaskID!=task||commit.Metadata.Identity.Scope!=r.scope||commit.Metadata.Identity.Kind!=""||commit.Metadata.Identity.AttemptID==""||commit.ResultID!=turnIdentity(task,"result")){return nil,nil,ErrDenied}
  kinds:=[]string{"INPUT"};if hasCommit{kinds=append(kinds,"DERIVED")}
  for _,kind:=range kinds{
   root:=turnIdentity(task,"accepted");if kind=="DERIVED"{root=commit.ReceiptKey}
   id:=turnIdentity(t,p,root,"integration-memory-birth-v1",kind);raw,ok:=docs[id];if !ok{return nil,nil,ErrDenied};delete(docs,id)
   var d IntegrationMemoryBirthDocument;if birthDecode(raw,&d)!=nil{return nil,nil,ErrDenied}
   original,e:=queryReceipt(ctx,tx,root);if e!=nil{return nil,nil,e};companion,e:=queryReceipt(ctx,tx,id);if e!=nil{return nil,nil,e}
   if d.Kind!=kind||d.ProofID!=id||d.WorldID!=w||d.TaskID!=task||d.ActorID!=actor||d.AggregateID!=r.aggregateID||d.CreationID!=w||d.InstallationID!=r.record.Installation||d.ReleaseID!=r.eligibility.entitlement.Release||d.Original!=birthStoredReceipt(original)||d.BeforeAggregateRevision==0||d.AfterAggregateRevision<d.BeforeAggregateRevision||d.Epoch==""||d.PolicyRevision==0{return nil,nil,ErrDenied}
   h:=sha256.Sum256(raw)
   want:=IntegrationBirthReceipt{id,t,p,self,"WORLD",w,task,integrationBirthOperation,id,"COMMIT",hex.EncodeToString(h[:]),false}
   if birthStoredReceipt(companion)!=want{return nil,nil,ErrDenied}
   if original.tenant!=t||original.product!=p||original.owner!=self||original.kind!="WORLD"||!original.world.Valid||original.world.String!=w||original.phase!="COMMIT"{return nil,nil,ErrDenied}
   // Resolve the original issuance, never replace it with today's Session.
   var exchange,consentKey string
   if e=tx.QueryRowContext(ctx,"SELECT x.original_key FROM integration_principals k JOIN integration_receipts x ON x.receipt_ref=k.issuance_receipt AND x.tenant_id=k.tenant_id AND x.product_id=k.product_id AND x.owner_id=k.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE k.principal_id=? AND k.kind='CLIENT' AND k.issuer_service_key=?",d.SessionID,d.ServiceID).Scan(&exchange);e!=nil{return nil,nil,ErrDenied}
   service:=a.service.principal;service.PrincipalID=d.ServiceID;originalSession,e:=sessionRead(ctx,tx,service,exchange)
   if e!=nil||originalSession.ID!=d.SessionID||originalSession.Player!=self||originalSession.World!=w||!originalSession.Expires.Equal(d.SessionExpires)||d.Registration!=(integrationRegistrationProof{originalSession.CustomerRevision,originalSession.MappingRevision,originalSession.EntitlementRevision,originalSession.Entitlement,originalSession.Installation,w}){return nil,nil,ErrDenied}
   if e=tx.QueryRowContext(ctx,"SELECT original_key FROM integration_self_consent_facts WHERE tenant_id=? AND product_id=? AND player_subject_id=? AND revision=? AND use_authority_generation=? AND expires_at=?",t,p,self,d.ConsentRevision,d.UseGeneration,d.UseExpires).Scan(&consentKey);e!=nil{return nil,nil,ErrDenied}
   if d.UseGeneration!=a.use.Generation||d.ConsentRevision!=a.use.Revision||!d.UseExpires.Equal(a.use.Expires)||d.QuoteID!=a.quote||d.ServiceID!=a.service.principal.PrincipalID{return nil,nil,ErrDenied}
   keys=append(keys,LockKey{Rank:15,Kind:"integration_principal",IDs:[]string{d.SessionID}},LockKey{Rank:15,Kind:"integration_principal",IDs:[]string{d.ServiceID}},LockKey{Rank:110,Kind:"integration_receipt",IDs:[]string{t,p,d.ServiceID,exchange}},LockKey{Rank:110,Kind:"integration_receipt",IDs:[]string{t,p,self,consentKey}},LockKey{Rank:90,Kind:"content_check",IDs:[]string{t,p,id}},LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{id}},LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{root}})
   var history [32]byte
   if kind=="INPUT"{
    if !original.matches(a.intakeRequest())||d.InputSourceID!=inputSource||d.InputHash!=hex.EncodeToString(inputHash)||len(d.SourcesBorn)!=1||d.SourcesBorn[0].BodyRawSHA256!=d.InputHash||d.Metadata!=nil||d.AttemptID!=""||d.ResultID!=""||d.ObservedAt.IsZero(){return nil,nil,ErrDenied}
   }else{
    if d.Metadata==nil||*d.Metadata!=commit.Metadata||d.ResultID!=commit.ResultID||d.AttemptID!=commit.Metadata.Identity.AttemptID||!reflect.DeepEqual(d.OriginalDependency,commit.OriginalDependency)||!reflect.DeepEqual(d.MemoryIDs,commit.MemoryIDs)||original.task!=task||!original.nullTask.Valid||original.operation!="HARNESS_TURN_COMMIT"||original.command!=root{return nil,nil,ErrDenied}
    _,hash,e:=harnessPayload(commit.Metadata);if e!=nil||!bytes.Equal(hash[:],original.hash){return nil,nil,ErrDenied}
    var approvalRaw []byte;var approval domain.CandidateApproval
    if e=tx.QueryRowContext(ctx,"SELECT document FROM harness_candidate_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,d.AttemptID).Scan(&approvalRaw);e!=nil||json.Unmarshal(approvalRaw,&approval)!=nil||approval.Metadata!=commit.Metadata||approval.ResultID!=commit.ResultID{return nil,nil,ErrDenied}
    if len(d.MemoryIDs)>0&&d.ObservedAt.IsZero()||len(d.MemoryIDs)==0&&!d.ObservedAt.IsZero(){return nil,nil,ErrDenied}
    var historyKeys []LockKey;history,historyKeys,e=s.integrationBirthCommittedHistory(ctx,tx,r,commit,d);if e!=nil{return nil,nil,e};keys=append(keys,historyKeys...)
   }
   stamp:=integrationBirthReadStamp{ProofID:id,Document:append([]byte(nil),raw...),History:history,Original:birthStoredReceipt(original),Companion:birthStoredReceipt(companion)};if kind=="DERIVED"{stamp.Commit=append([]byte(nil),commitRaw...)};out=append(out,stamp)
  }
 }
 if len(docs)!=0{return nil,nil,ErrDenied};return out,keys,nil
}
func(s *Store)PlanIntegrationMemoryBirths(ctx context.Context,owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead)([]IntegrationMemoryBirthEvidence,error){
 if e:=s.memoryProofOwner(owner,r);e!=nil{return nil,e};tx,e:=s.integrationMemoryReadTx(ctx,r.keys);if e!=nil{return nil,e};r.tx=tx
 if e=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{tx.Rollback();r.tx=nil;return nil,e}
 rows,keys,e:=s.integrationMemoryBirths(ctx,tx,r);tx.Rollback();r.tx=nil;if e!=nil{return nil,e};r.births=rows;r.keys=append(r.keys,keys...)
 out:=make([]IntegrationMemoryBirthEvidence,len(rows));for i,row:=range rows{out[i]=IntegrationMemoryBirthEvidence{append([]byte(nil),row.Document...),sha256.Sum256(row.Document),CommitReceipt{row.Original.Key,row.Original.Task,row.Original.Owner,true},CommitReceipt{row.Companion.Key,row.Companion.Task,row.Companion.Owner,true}}};return out,nil
}

// Read only the stable owning history of an already committed result. This
// does not enter an execution lease, inspect a purged candidate file, query a
// supplier, or freeze later cost/query counters. All selected stable facts are
// included in the PREPARE/Check/Finish stamp under these exact original locks.
func(s *Store)integrationBirthCommittedHistory(ctx context.Context,tx *sql.Tx,r *IntegrationMemoryRead,commit domain.CandidateCommit,birth IntegrationMemoryBirthDocument)([32]byte,[]LockKey,error){
 var zero [32]byte;scope:=r.scope;t,p,task:=scope.TenantID,scope.ProductID,birth.TaskID
 keys:=[]LockKey{{Rank:50,IDs:[]string{t,p,task}}};facts:=[]any{}
 receipt:=func(key,operation,phase string,hash [32]byte)error{
  keys=append(keys,LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{key}})
  row,e:=queryReceipt(ctx,tx,key);if e!=nil{return e}
  want:=BarrierRequest{ProductScope:scope.ProductScope,WorldScope:&scope,OwnerSubjectID:scope.SubjectID,TaskID:task,Operation:operation,CommandKey:key,CommitKey:key,PayloadHash:hash,phase:phase}
  if !row.matches(want){return ErrDenied};facts=append(facts,birthStoredReceipt(row));return nil
 }
 metas:=[]domain.MockResultMetadata{commit.Metadata};if commit.OriginalDependency!=nil{metas=append([]domain.MockResultMetadata{*commit.OriginalDependency},metas...)}
 var project,owner,world,taskState,turnState,result,root string
 if e:=tx.QueryRowContext(ctx,"SELECT t.project_id,t.owner_subject_id,t.world_instance_id,t.state,h.fsm_state,c.result_id,c.receipt_key FROM tasks t JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN harness_turn_commits c USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?",t,p,task).Scan(&project,&owner,&world,&taskState,&turnState,&result,&root);e!=nil{return zero,nil,e}
 if owner!=scope.SubjectID||world!=scope.WorldInstanceID||taskState!="COMMITTED"||turnState!="COMMITTED"||result!=commit.ResultID||root!=commit.ReceiptKey{return zero,nil,ErrDenied}
 var count int;if e:=tx.QueryRowContext(ctx,"SELECT COUNT(*) FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=?",t,p,task).Scan(&count);e!=nil||count!=len(metas){return zero,nil,ErrDenied}
 var recovery domain.RecoveryPlan;var recoveryRaw []byte;var originalAttempt,extraAttempt,recoveryKey string
 e:=tx.QueryRowContext(ctx,"SELECT original_attempt_id,extra_attempt_id,receipt_key,document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?",t,p,task).Scan(&originalAttempt,&extraAttempt,&recoveryKey,&recoveryRaw)
 if commit.OriginalDependency==nil{if e!=sql.ErrNoRows{return zero,nil,ErrDenied}}else{
  if e!=nil||json.Unmarshal(recoveryRaw,&recovery)!=nil||recovery.Original!=*commit.OriginalDependency||originalAttempt!=metas[0].Identity.AttemptID||extraAttempt!=commit.Metadata.Identity.AttemptID||recovery.AttemptID!=extraAttempt||recovery.ApprovalID!=turnIdentity(task,"recovery")||recoveryKey!=recovery.ApprovalID||(recovery.Reason!="STRUCTURE_REPAIR"&&recovery.Reason!="STATE_CONFLICT_RECOMPUTE"){return zero,nil,ErrDenied}
  _,hash,e:=harnessPayload(recovery.Original);if e!=nil{return zero,nil,e};if e=receipt(recoveryKey,"HARNESS_RECOVERY_APPROVE","",hash);e!=nil{return zero,nil,e};facts=append(facts,sha256.Sum256(recoveryRaw))
 }
 var baseBinding domain.GenerationBinding
 for slot,meta:=range metas{
  id:=meta.Identity;attempt:=turnIdentity(task,"attempt",fmt.Sprint(slot));authorize:=turnIdentity(task,"authorize");send:=turnIdentity(task,"send");if slot==1{authorize=turnIdentity(attempt,"authorize");send=turnIdentity(attempt,"send")}
  hash,e:=hex.DecodeString(id.RequestHash);if e!=nil||len(hash)!=32||id.Scope!=scope||id.Kind!=""||id.TaskID!=task||id.AttemptID!=attempt||id.AuthorizationID!=turnIdentity(attempt,"authorization")||id.RequestKey!=turnIdentity(attempt,"request")||id.MarkerKey!=send||id.QuoteID!=birth.QuoteID||meta.State!="SUCCEEDED"||len(meta.BodyHash)!=64||meta.BodyStorage==""{return zero,nil,ErrDenied}
  d:=DispatchRequest{BarrierRequest:BarrierRequest{ProductScope:scope.ProductScope,WorldScope:&scope,TaskID:task,OwnerSubjectID:scope.SubjectID},Slot:uint8(slot),AttemptID:attempt,AuthorizationID:id.AuthorizationID,RequestKey:id.RequestKey,Route:id.RouteID,HarnessQuoteID:id.QuoteID};copy(d.RequestHash[:],hash)
  a,e:=readAttempt(ctx,tx,d);if e!=nil||!a.matches(d)||!a.started.Valid||!a.marker.Valid||a.marker.String!=id.MarkerKey{return zero,nil,ErrDenied}
  if e=tx.QueryRowContext(ctx,"SELECT owner_subject_id FROM dispatch_authorizations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND authorization_id=?",t,p,task,attempt,id.AuthorizationID).Scan(&owner);e!=nil||owner!=scope.SubjectID{return zero,nil,ErrDenied}
  keys=append(keys,LockKey{Rank:60,IDs:[]string{t,p,task,attempt,id.AuthorizationID}})
  var amount int64;if e=tx.QueryRowContext(ctx,"SELECT supplier_atoms FROM harness_attempt_allocations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,attempt).Scan(&amount);e!=nil||amount<0{return zero,nil,ErrDenied}
  for _,hold:=range r0Accounts(scope,project){if hold.Kind!="supplier_global"&&hold.Kind!="supplier_tenant"{continue};hold.ReservationID=turnIdentity(attempt,"supplier",hold.Kind);hold.Amount=amount;var actual int64;var purpose string
   if e=tx.QueryRowContext(ctx,"SELECT amount,purpose FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind=? AND scope_key=? AND account_id=? AND reservation_id=?",t,p,task,hold.Kind,hold.ScopeKey,hold.AccountID,hold.ReservationID).Scan(&actual,&purpose);e!=nil||actual!=amount||purpose!=attempt{return zero,nil,ErrDenied}
   d.Reservations=append(d.Reservations,hold);keys=append(keys,LockKey{Rank:80,Kind:hold.Kind,IDs:[]string{hold.ScopeKey,hold.AccountID,hold.ReservationID}})
  }
  dispatchHash,e:=dispatchFingerprint(d);if e!=nil{return zero,nil,e};if e=receipt(authorize,"TURN_DISPATCH","AUTHORIZE",dispatchHash);e!=nil{return zero,nil,e};if e=receipt(send,"TURN_DISPATCH","SEND",dispatchHash);e!=nil{return zero,nil,e}
  var identityRaw,metadataRaw,snapshotRaw []byte;var install,service,policy,marker,state string
  if e=tx.QueryRowContext(ctx,"SELECT request_identity,metadata,installation_id,service_id,policy,marker_key,provider_state FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,attempt).Scan(&identityRaw,&metadataRaw,&install,&service,&policy,&marker,&state);e!=nil{return zero,nil,e}
  var originalID domain.MockRequestIdentity;var saved domain.MockResultMetadata
  if json.Unmarshal(identityRaw,&originalID)!=nil||originalID!=id||json.Unmarshal(metadataRaw,&saved)!=nil||saved.Identity!=id||saved.State!=meta.State||saved.BodyStorage!=meta.BodyStorage||saved.BodyHash!=meta.BodyHash||install!=birth.InstallationID||service!="r0-mock-original-query"||policy!="R0_ORIGINAL_MOCK_COST_V1"||marker!=id.MarkerKey||state!="SUCCEEDED"{return zero,nil,ErrDenied}
  // Cost knowledge and BodyAvailable can progress independently. Only the
  // immutable returned body address/hash/state and request tuple bind birth.
  facts=append(facts,struct{Identity domain.MockRequestIdentity;Started time.Time;Install,Service,Policy,State,Storage,Hash string}{id,a.started.Time,install,service,policy,state,saved.BodyStorage,saved.BodyHash})
  var validationRaw []byte;var code,validator,validationKey,bodyHash string
  if e=tx.QueryRowContext(ctx,"SELECT metadata,body_hash,validation_code,validator_version,receipt_key FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,attempt).Scan(&validationRaw,&bodyHash,&code,&validator,&validationKey);e!=nil{return zero,nil,e}
  if json.Unmarshal(validationRaw,&saved)!=nil||saved!=meta||bodyHash!=meta.BodyHash||validator!="R0_CANDIDATE_STRUCTURE_V1"||validationKey!=turnIdentity(attempt,"validation")||(slot==len(metas)-1&&code!="STRUCTURE_VALID")||(code!="STRUCTURE_VALID"&&code!="STRUCTURE_INVALID"){return zero,nil,ErrDenied}
  _,validationHash,e:=harnessPayload(struct{Metadata domain.MockResultMetadata;Code string}{meta,code});if e!=nil{return zero,nil,e};if e=receipt(validationKey,"HARNESS_RESULT_VALIDATE","",validationHash);e!=nil{return zero,nil,e};facts=append(facts,sha256.Sum256(validationRaw),bodyHash,code,validator)
  if e=tx.QueryRowContext(ctx,"SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,attempt).Scan(&snapshotRaw);e!=nil{return zero,nil,e}
  var snapshot domain.OriginalContextSnapshot;if json.Unmarshal(snapshotRaw,&snapshot)!=nil||snapshot.Binding.RequestHash!=id.RequestHash||snapshot.Binding.RouteID!=id.RouteID||snapshot.Binding.ActorID!=birth.ActorID||snapshot.Binding.DataSubjectID!=scope.SubjectID||snapshot.Binding.InputSourceID==""||snapshot.World.ActorID!=birth.ActorID||snapshot.World.CreationID!=birth.CreationID{return zero,nil,ErrDenied}
  if slot==0{if snapshot.Recovery!=nil{return zero,nil,ErrDenied};baseBinding=snapshot.Binding}else if snapshot.Recovery==nil||*snapshot.Recovery!=recovery||snapshot.Binding!=recovery.Binding||!recovery.BaseBinding.SameOrigin(baseBinding){return zero,nil,ErrDenied};facts=append(facts,sha256.Sum256(snapshotRaw))
  if len(metas)==2&&slot==0{
   if recovery.Reason=="STRUCTURE_REPAIR"{if code!="STRUCTURE_INVALID"||recovery.OriginalPhase!=""||recovery.OriginalRound!=0{return zero,nil,ErrDenied}}else{
    if code!="STRUCTURE_VALID"||recovery.OriginalRound==0||(recovery.OriginalPhase!="APPROVAL"&&recovery.OriginalPhase!="WORLD_COMMIT"){return zero,nil,ErrDenied}
    var phaseState,reason,roundHash,roundKey string;var conflict []byte
    if e=tx.QueryRowContext(ctx,"SELECT state,reason,body_hash,original_receipt_key,conflict_document FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? AND round_number=?",t,p,task,attempt,recovery.OriginalPhase,recovery.OriginalRound).Scan(&phaseState,&reason,&roundHash,&roundKey,&conflict);e!=nil||phaseState!="ROLLED_BACK"||reason!="RESOURCE_CONFLICT"||roundHash!=meta.BodyHash{return zero,nil,ErrDenied}
    expected:=turnIdentity(attempt,"approval");if recovery.OriginalPhase=="WORLD_COMMIT"{expected=turnIdentity(task,"world-commit")};if roundKey!=expected{return zero,nil,ErrDenied}
    // A later successful COMMIT may reuse the world key. The failed round is
    // retained evidence, not a demand that today's valid COMMIT be absent.
    var old CandidateResourceConflict;if json.Unmarshal(conflict,&old)!=nil{return zero,nil,ErrDenied};facts=append(facts,phaseState,reason,roundHash,roundKey,sha256.Sum256(conflict))
   }
  }
 }
 var approvalRaw []byte;var approvalKey string;var approval domain.CandidateApproval
 if e=tx.QueryRowContext(ctx,"SELECT document,receipt_key FROM harness_candidate_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?",t,p,task,commit.Metadata.Identity.AttemptID).Scan(&approvalRaw,&approvalKey);e!=nil||json.Unmarshal(approvalRaw,&approval)!=nil||approval.Metadata!=commit.Metadata||approval.ResultID!=commit.ResultID||approvalKey!=turnIdentity(commit.Metadata.Identity.AttemptID,"approval"){return zero,nil,ErrDenied}
 _,approvalHash,e:=harnessPayload(commit.Metadata);if e!=nil{return zero,nil,e};if e=receipt(approvalKey,"HARNESS_CANDIDATE_APPROVE","",approvalHash);e!=nil{return zero,nil,e};facts=append(facts,sha256.Sum256(approvalRaw))
 raw,e:=json.Marshal(facts);if e!=nil{return zero,nil,e};return sha256.Sum256(raw),keys,nil
}
