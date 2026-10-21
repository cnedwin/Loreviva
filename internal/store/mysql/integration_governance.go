package mysql

import (
 "bytes"
 "context"
 "crypto/aes"
 "crypto/cipher"
 "crypto/hmac"
 "crypto/rand"
 "crypto/sha256"
 "encoding/base64"
 "encoding/hex"
 "encoding/json"
 "database/sql"
 "errors"
 "strconv"
 "loreviva.local/loreviva/internal/generated"
 "loreviva.local/loreviva/internal/domain"
 "loreviva.local/loreviva/internal/wire"
 "reflect"
 "time"
)

// Eligibility is calculated inside the existing read transaction. It exposes
// neither body nor publication material and never treats a metadata label as use.
func (s *Store) MemoryBodyEligibility(ctx context.Context, r *IntegrationMemoryRead) (bool,error) {
 if e:=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{return false,e}
 snapshot,_,e:=s.consentSnapshot(ctx,r.tx,r.record,r.eligibility);if e!=nil{return false,e}
 if !snapshot.CurrentUseEnabled{return false,nil}
 var allowed bool
 e=r.tx.QueryRowContext(ctx,`SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.private_body.read' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))`,r.record.Tenant,r.record.Product,r.record.Player,r.record.Player).Scan(&allowed)
 if e!=nil{return false,e};if !allowed{return false,nil}
 if e=s.checkIntegrationMemoryReadMode(ctx,r,false);e!=nil{return false,e}
 return true,nil
}

// The binding is minted only for the already-installed consent/memory owner.
// Interface satisfaction, a copied scope, or a zero-value plan is not admission.
type IntegrationGovernanceOwner interface {
 PrepareIntegrationMemoryProof(context.Context,*IntegrationMemoryRead)([]byte,error)
 FinishIntegrationMemoryProof(context.Context,*IntegrationMemoryRead,[]byte)error
 ExecuteIntegrationGovernance(context.Context,*IntegrationMemoryGovernancePlan)(generated.TaskReceipt,error)
 InspectIntegrationGovernance(context.Context,*IntegrationGovernanceRead)(generated.TaskView,error)
 ApplyIntegrationGovernance(context.Context,*IntegrationMemoryGovernancePlan,Tx)(IntegrationGovernanceWritten,error)
}
type IntegrationGovernanceOwnerBinding struct { store *Store; owner IntegrationGovernanceOwner }
type IntegrationGovernanceRead struct { plan *IntegrationMemoryGovernancePlan }
type IntegrationGovernanceWritten struct { Document,Binding []byte; Task,Actor,Hash string; NextRun time.Time }
type IntegrationGovernanceAsset struct { ID,Storage string }
type IntegrationGovernanceFacts struct {
 Scope domain.Scope
 Actor,Task,Key,Action,Replacement string
 Raw,Document []byte
 Expected map[string]uint64
 IDs []string
}
type integrationGovernanceCommand struct {
 Operation string `json:"operation"`
 IDs []string `json:"memory_ids"`
 Expected map[string]string `json:"expected_revisions"`
 Replacement string `json:"replacement"`
 Reason string `json:"reason"`
 Uses []string `json:"uses"`
 Selector *struct {Kind string `json:"kind"`;Target string `json:"target_kind"`;IDs []string `json:"ids"`;Expected map[string]string `json:"expected_revisions"`} `json:"selector"`
}
type IntegrationMemoryGovernancePlan struct {
 self *IntegrationMemoryGovernancePlan
 store *Store; owner *IntegrationGovernanceOwnerBinding
 principal currentIntegration; record sessionRecord; eligibility sessionEligibility
 facts IntegrationGovernanceFacts; request BarrierRequest; keys []LockKey
 active, executing, replay bool; unit *unit
 payload []byte; hash [32]byte; expected *IntegrationGovernanceWritten
 aggregateRevision uint64
 assets []IntegrationGovernanceAsset
}
// Copies and a closed plan cannot recreate the issuing Store's live unit.
func (p *IntegrationMemoryGovernancePlan) Close(){if p!=nil&&p.self==p&&!p.executing{p.active=false}}
type integrationGovernanceContextKey struct{}

func (s *Store) BindIntegrationGovernanceOwner(owner IntegrationGovernanceOwner)(*IntegrationGovernanceOwnerBinding,error){
 s.integrationOwnerMu.Lock();defer s.integrationOwnerMu.Unlock()
 if owner==nil||s.integrationConsentOwner==nil||reflect.ValueOf(owner).Kind()!=reflect.Pointer||reflect.ValueOf(owner).Pointer()!=reflect.ValueOf(s.integrationConsentOwner).Pointer()||s.integrationClosed.Load()||s.Guard==nil{return nil,ErrDenied}
 if s.integrationGovernanceOwner!=nil {if s.integrationGovernanceOwner.owner!=owner{return nil,ErrDenied};return s.integrationGovernanceOwner,nil}
 s.integrationGovernanceOwner=&IntegrationGovernanceOwnerBinding{s,owner};return s.integrationGovernanceOwner,nil
}
func (s *Store) governancePlanCurrent(ctx context.Context,q creationRows,p *IntegrationMemoryGovernancePlan) error {
 if p==nil||p.self!=p||!p.active||p.store!=s||p.owner==nil||p.owner!=s.integrationGovernanceOwner||s.integrationClosed.Load(){return ErrDenied}
 r,eligible,e:=s.currentClient(ctx,q,p.principal)
 expectedEligibility:=p.eligibility
 if p.expected!=nil {if expectedEligibility.mapping==nil||expectedEligibility.mapping.Privacy==^uint64(0)||expectedEligibility.mapping.View==^uint64(0){return ErrDenied};mapping:=*expectedEligibility.mapping;mapping.Privacy++;mapping.View++;expectedEligibility.mapping=&mapping}
 if e!=nil||!reflect.DeepEqual(r,p.record)||!reflect.DeepEqual(eligible,expectedEligibility){return ErrDenied}
 found:=false;for _,a:=range r.Actions {found=found||a=="MEMORY_GOVERN"};if !found{return ErrDenied}
 for _,right:=range []string{"memory.govern",func()string{if p.facts.Action=="CORRECT"{return "memory.correct"};return "memory.govern"}()} {
  var permitted bool
  e=q.QueryRowContext(ctx,`SELECT EXISTS(SELECT 1 FROM memory_subject_permissions p JOIN subject_governance g ON g.tenant_id=p.tenant_id AND g.product_id=p.product_id AND g.subject_id=p.data_subject_id WHERE p.tenant_id=? AND p.product_id=? AND p.caller_subject_id=? AND p.data_subject_id=? AND p.action=? AND p.allowed_use='PRIVATE_RESPONSE' AND p.enabled=1 AND p.expires_at>UTC_TIMESTAMP(6) AND g.enabled=1 AND g.can_govern=1)`,r.Tenant,r.Product,r.Player,r.Player,right).Scan(&permitted)
  if e!=nil||!permitted{return ErrDenied}
 }
 return nil
}

func (s *Store) ReadIntegrationGovernanceTask(ctx context.Context,id string)(generated.TaskView,error){
 var out generated.TaskView
 if domain.ValidateID(id)!=nil||s.integrationGovernanceOwner==nil{return out,ErrDenied}
 ctx,done,e:=s.EnterOperation(ctx);if e!=nil{return out,e};defer done()
 v,ok:=ctx.Value(integrationContextKey{}).(currentIntegration);if !ok||v.store!=s{return out,ErrDenied}
 r,eligible,e:=s.currentClient(ctx,s.DB,v);if e!=nil{return out,e}
 p:=&IntegrationMemoryGovernancePlan{store:s,owner:s.integrationGovernanceOwner,principal:v,record:r,eligibility:eligible,active:true,executing:true}
 p.self=p
 p.facts=IntegrationGovernanceFacts{Scope:domain.Scope{ProductScope:domain.ProductScope{TenantID:r.Tenant,ProductID:r.Product,SubjectID:r.Player},WorldInstanceID:r.World},Task:id,Action:"DELETE"}
 var hash []byte
 e=s.DB.QueryRowContext(ctx,`SELECT c.command_key,c.payload_hash FROM commit_receipts c JOIN memory_governance m ON m.tenant_id=c.tenant_id AND m.product_id=c.product_id AND m.task_id=? WHERE c.commit_key=? AND c.tenant_id=? AND c.product_id=? AND c.owner_subject_id=? AND c.world_instance_id=? AND c.operation='MEMORY_GOVERNANCE_ACCEPT' AND c.phase='COMMIT' AND m.owner_subject_id=? AND m.data_subject_id=? AND m.world_instance_id=? AND m.request_hash=c.payload_hash`,id,creationID(id,"accept"),r.Tenant,r.Product,r.Player,r.World,r.Player,r.Player,r.World).Scan(&p.facts.Key,&hash)
 if e!=nil||len(hash)!=32||creationID(r.Tenant,r.Product,r.Player,r.World,"MEMORY_GOVERNANCE",p.facts.Key)!=id{return out,ErrDenied};copy(p.hash[:],hash)
 scope:=p.facts.Scope;p.request=BarrierRequest{ProductScope:scope.ProductScope,WorldScope:&scope,OwnerSubjectID:r.Player,Operation:"MEMORY_GOVERNANCE_ACCEPT",CommandKey:p.facts.Key,CommitKey:creationID(id,"accept"),PayloadHash:p.hash,commandOnly:true}
 p.keys=append(consentKeys(r,eligible,""),LockKey{Rank:40,IDs:[]string{r.Tenant,r.Product,r.World}},LockKey{Rank:50,IDs:[]string{r.Tenant,r.Product,id}},LockKey{Rank:90,Kind:"memory",IDs:[]string{r.Tenant,r.Product,r.World,creationID("MEMORY_SUBJECT",r.World,r.Player)}},LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{p.request.CommitKey}})
 p.keys=append(p.keys,LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{creationID(id,"preserved-clean")}})
 defer func(){p.active=false;p.executing=false}()
 return p.owner.owner.InspectIntegrationGovernance(ctx,&IntegrationGovernanceRead{plan:p})
}
func (s *Store) IntegrationGovernanceReadFacts(owner *IntegrationGovernanceOwnerBinding,r *IntegrationGovernanceRead)(IntegrationGovernanceFacts,error){if r==nil{return IntegrationGovernanceFacts{},ErrDenied};return s.IntegrationGovernanceFacts(owner,r.plan)}
// A known accepted task may recover the original preservation binding. This
// short SQL phase ends before the memory owner touches its independent Ledger.
// A missing reservation/hash/acceptance never manufactures a new task or POST.
func (s *Store) ReadIntegrationGovernanceRecovery(ctx context.Context,owner *IntegrationGovernanceOwnerBinding,r *IntegrationGovernanceRead,ledgerHash string)([]byte,error){
 if _,e:=s.IntegrationGovernanceReadFacts(owner,r);e!=nil{return nil,e};p:=r.plan
 if ledgerHash!=hex.EncodeToString(p.hash[:]){return nil,ErrDenied}
 tx,e:=s.integrationMemoryReadTx(ctx,p.keys);if e!=nil{return nil,e};defer tx.Rollback()
 read:=func()([]byte,error){
  if e:=s.governancePlanCurrent(ctx,tx,p);e!=nil{return nil,e};if e:=s.checkGovernanceAcceptedTx(ctx,tx,p,false);e!=nil{return nil,e}
  var binding []byte;if e:=tx.QueryRowContext(ctx,"SELECT binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?",p.record.Tenant,p.record.Product,p.facts.Task).Scan(&binding);e!=nil{return nil,e};return binding,nil
 }
 before,e:=read();if e!=nil{return nil,e};stateBefore,e:=s.integrationGovernanceReadState(ctx,tx,p);if e!=nil{return nil,e};if s.BeforeReadFinish!=nil{if e=s.BeforeReadFinish(ctx,tx);e!=nil{return nil,e}}
 after,e:=read();if e!=nil||!bytes.Equal(before,after){return nil,ErrDenied};stateAfter,e:=s.integrationGovernanceReadState(ctx,tx,p);if e!=nil||stateBefore!=stateAfter{return nil,ErrDenied};return append([]byte(nil),before...),nil
}
func (s *Store) FinishIntegrationGovernanceTask(ctx context.Context,owner *IntegrationGovernanceOwnerBinding,r *IntegrationGovernanceRead,ledgerHash string,binding []byte)(generated.TaskView,error){
 var out generated.TaskView
 if _,e:=s.IntegrationGovernanceReadFacts(owner,r);e!=nil{return out,e};p:=r.plan
 if ledgerHash!=hex.EncodeToString(p.hash[:])||len(binding)==0{return out,ErrDenied}
 tx,e:=s.integrationMemoryReadTx(ctx,p.keys);if e!=nil{return out,e};defer tx.Rollback()
 check:=func()error{if e:=s.governancePlanCurrent(ctx,tx,p);e!=nil{return e};if e:=s.checkGovernanceAcceptedTx(ctx,tx,p,false);e!=nil{return e};var stored []byte;if e:=tx.QueryRowContext(ctx,`SELECT binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?`,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&stored);e!=nil||!jsonEquivalent(stored,binding){return ErrDenied};return nil}
 if e=check();e!=nil{return out,e}
 projection:=func()(generated.TaskView,error){
 var out generated.TaskView
 var state string;var versions domain.Versions
 if e=tx.QueryRowContext(ctx,`SELECT state FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?`,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&state);e!=nil{return out,e}
 if e=tx.QueryRowContext(ctx,`SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?`,p.record.Tenant,p.record.Product,p.record.World).Scan(&versions.ReleaseID,&versions.WorldRevision,&versions.ControlVersion,&versions.PrivacyEpoch,&versions.ViewEpoch);e!=nil{return out,e}
 var count int
 for _,table:=range []string{"reservations","attempts","harness_envelopes","harness_turns","harness_turn_steps","service_events","postings"} {
  // Closed internal table inventory; no caller SQL or schema is accepted.
  if e=tx.QueryRowContext(ctx,"SELECT COUNT(*) FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=?",p.record.Tenant,p.record.Product,p.facts.Task).Scan(&count);e!=nil{return out,e};if count!=0{return out,ErrDenied}
 }
 out.Task=generated.TaskReceipt{TaskID:p.facts.Task,TaskKind:"MEMORY_GOVERNANCE",TaskState:state,CorrelationID:p.facts.Task,Scope:turnScope(p.facts.Scope)}
 out.Versions=generated.Versions{ReleaseID:versions.ReleaseID,WorldRevision:strconv.FormatUint(versions.WorldRevision,10),ControlVersion:strconv.FormatUint(versions.ControlVersion,10),PrivacyEpoch:strconv.FormatUint(versions.PrivacyEpoch,10),ViewEpoch:strconv.FormatUint(versions.ViewEpoch,10)}
 out.Attempts=[]generated.AttemptSummary{};out.CheckpointState="NONE"
 out.Billing=generated.BillingSummary{CustomerConsumedUnits:"0",CustomerReservedUnits:"0",RiskBearer:"SELF_HOSTED",ServiceSettlementState:"NOT_APPLICABLE",ServiceUnit:"WE_CREDIT",SupplierCostCurrency:"USD",SupplierSafetyReservedUnits:"0"}
 return out,nil
 }
 out,e=projection();if e!=nil{return generated.TaskView{},e};stateBefore,e:=s.integrationGovernanceReadState(ctx,tx,p);if e!=nil{return generated.TaskView{},e}
 if s.BeforeReadFinish!=nil {if e=s.BeforeReadFinish(ctx,tx);e!=nil{return generated.TaskView{},e}}
 if e=check();e!=nil{return generated.TaskView{},e};stateAfter,e:=s.integrationGovernanceReadState(ctx,tx,p);if e!=nil||stateBefore!=stateAfter{return generated.TaskView{},ErrDenied};after,e:=projection();if e!=nil||!reflect.DeepEqual(after,out){return generated.TaskView{},ErrDenied};if e=typedJSON("TaskView",out);e!=nil{return generated.TaskView{},e};return out,nil
}
func (s *Store) PlanIntegrationMemoryGovernance(ctx context.Context,world,key string,raw []byte)(*IntegrationMemoryGovernancePlan,error){
 if s.integrationGovernanceOwner==nil||domain.ValidateID(world)!=nil||domain.ValidateIdempotencyKey(key)!=nil||generated.Validate("MemoryGovernanceCommandV1",raw)!=nil{return nil,ErrDenied}
 if _,e:=wire.Parse(raw);e!=nil{return nil,e}
 var c integrationGovernanceCommand;if json.Unmarshal(raw,&c)!=nil||len(c.Uses)!=0||(c.Operation!="DELETE"&&c.Operation!="CORRECT"){return nil,ErrDenied}
 if c.Selector!=nil {if c.Selector.Kind!="IDS"||c.Selector.Target!="MEMORY_NODE"||len(c.IDs)!=0{return nil,ErrDenied};c.IDs=c.Selector.IDs;c.Expected=c.Selector.Expected}
 if len(c.IDs)<1||len(c.IDs)>100||len(c.Expected)!=len(c.IDs)||(c.Operation=="CORRECT"&&(len(c.IDs)!=1||c.Replacement=="")){return nil,ErrDenied}
 v,ok:=ctx.Value(integrationContextKey{}).(currentIntegration);if !ok||v.store!=s{return nil,ErrDenied}
 r,eligible,e:=s.currentClient(ctx,s.DB,v);if e!=nil||r.World!=world{return nil,ErrDenied}
 p:=&IntegrationMemoryGovernancePlan{store:s,owner:s.integrationGovernanceOwner,principal:v,record:r,eligibility:eligible,active:true}
 p.self=p
 p.facts=IntegrationGovernanceFacts{Scope:domain.Scope{ProductScope:domain.ProductScope{TenantID:r.Tenant,ProductID:r.Product,SubjectID:r.Player},WorldInstanceID:world},Task:creationID(r.Tenant,r.Product,r.Player,world,"MEMORY_GOVERNANCE",key),Key:key,Action:c.Operation,Replacement:c.Replacement,Raw:append([]byte(nil),raw...),Expected:map[string]uint64{},IDs:append([]string(nil),c.IDs...)}
 for _,id:=range c.IDs {n,e:=strconv.ParseUint(c.Expected[id],10,64);if e!=nil||n==0||strconv.FormatUint(n,10)!=c.Expected[id]||domain.ValidateID(id)!=nil||p.facts.Expected[id]!=0{return nil,ErrDenied};p.facts.Expected[id]=n}
 p.keys=append(consentKeys(r,eligible,""),LockKey{Rank:30,IDs:[]string{r.Tenant,r.Product,r.Player}},LockKey{Rank:40,IDs:[]string{r.Tenant,r.Product,world}},LockKey{Rank:50,IDs:[]string{r.Tenant,r.Product,p.facts.Task}},LockKey{Rank:90,Kind:"memory",IDs:[]string{r.Tenant,r.Product,world,creationID("MEMORY_SUBJECT",world,r.Player)}},LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{creationID(p.facts.Task,"accept")}})
 tx,e:=s.integrationMemoryReadTx(ctx,p.keys);if e!=nil{return nil,e};defer tx.Rollback()
 if e=s.governancePlanCurrent(ctx,tx,p);e!=nil{return nil,e}
 if e=tx.QueryRowContext(ctx,`SELECT document,revision FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?`,r.Tenant,r.Product,world,r.Player).Scan(&p.facts.Document,&p.aggregateRevision);e!=nil{return nil,e}
 var a struct{Nodes map[string]struct{ID,ActorID,TargetID,Domain string;Revision uint64}}
 if json.Unmarshal(p.facts.Document,&a)!=nil||len(a.Nodes)>1000{return nil,ErrDenied}
 var prior []byte
 e=tx.QueryRowContext(ctx,`SELECT request_hash FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?`,r.Tenant,r.Product,p.facts.Task).Scan(&prior)
 if e!=nil&&!errors.Is(e,sql.ErrNoRows){return nil,e};p.replay=e==nil
 for _,id:=range c.IDs {node,ok:=a.Nodes[id];if !ok||node.ID!=id||node.TargetID!=r.Player||domain.ValidateID(node.ActorID)!=nil||(!p.replay&&node.Revision!=p.facts.Expected[id])||(c.Operation=="CORRECT"&&node.Domain=="WORLD_FACT"){return nil,ErrDenied};if p.facts.Actor!=""&&p.facts.Actor!=node.ActorID{return nil,ErrDenied};p.facts.Actor=node.ActorID}
 var actor bool;if e=tx.QueryRowContext(ctx,`SELECT EXISTS(SELECT 1 FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?)`,r.Tenant,r.Product,world,p.facts.Actor).Scan(&actor);e!=nil||!actor{return nil,ErrDenied}
 var versions domain.Versions;var gates GateVersions;var status string
 if e=tx.QueryRowContext(ctx,`SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?`,r.Tenant,r.Product,world).Scan(&versions.ReleaseID,&versions.WorldRevision,&versions.ControlVersion,&versions.PrivacyEpoch,&versions.ViewEpoch,&status);e!=nil{return nil,e}
 if e=tx.QueryRowContext(ctx,`SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?`,r.Tenant,r.Product,r.Player).Scan(&gates.Tenant,&gates.Product,&gates.Subject,&gates.Privacy,&gates.View);e!=nil{return nil,e}
 scope:=p.facts.Scope;p.request=BarrierRequest{ProductScope:scope.ProductScope,WorldScope:&scope,Versions:&versions,Purpose:GovernancePurpose,OwnerSubjectID:r.Player,ExpectedWorldStatus:status,Operation:"MEMORY_GOVERNANCE_ACCEPT",CommandKey:key,CommitKey:creationID(p.facts.Task,"accept"),GateVersions:gates,commandOnly:true,integrationGovernance:p}
 if e=s.governancePlanCurrent(ctx,tx,p);e!=nil{return nil,e};return p,nil
}

func (s *Store) IntegrationGovernanceFacts(owner *IntegrationGovernanceOwnerBinding,p *IntegrationMemoryGovernancePlan)(IntegrationGovernanceFacts,error){
 if p==nil||p.self!=p||!p.active||!p.executing||p.store!=s||p.owner!=owner||owner!=s.integrationGovernanceOwner{return IntegrationGovernanceFacts{},ErrDenied}
 f:=p.facts;f.Raw=append([]byte(nil),f.Raw...);f.Document=append([]byte(nil),f.Document...);f.IDs=append([]string(nil),f.IDs...);f.Expected=map[string]uint64{};for k,v:=range p.facts.Expected{f.Expected[k]=v};return f,nil
}
func (s *Store) GovernIntegrationMemory(ctx context.Context,p *IntegrationMemoryGovernancePlan)(generated.TaskReceipt,error){
 var out generated.TaskReceipt
 if p==nil||p.self!=p||!p.active||p.executing||p.store!=s||p.owner!=s.integrationGovernanceOwner{return out,ErrDenied}
 ctx,done,e:=s.EnterOperation(ctx);if e!=nil{return out,e};defer done()
 if e=s.governancePlanCurrent(ctx,s.DB,p);e!=nil{return out,e}
 p.executing=true;defer func(){p.active=false;p.executing=false;p.unit=nil}()
 return p.owner.owner.ExecuteIntegrationGovernance(context.WithValue(ctx,integrationGovernanceContextKey{},p),p)
}

func (s *Store) PrepareIntegrationGovernanceAccept(owner *IntegrationGovernanceOwnerBinding,p *IntegrationMemoryGovernancePlan,assets []LockKey,refs []IntegrationGovernanceAsset)([]byte,error){
 if _,e:=s.IntegrationGovernanceFacts(owner,p);e!=nil{return nil,e};if p.payload!=nil{return nil,ErrDenied}
 for _,k:=range assets {if k.Rank!=90||k.Kind!="asset"||len(k.IDs)!=3||k.IDs[0]!=p.record.Tenant||k.IDs[1]!=p.record.Product{return nil,ErrDenied}}
 p.keys=append(p.keys,assets...)
 p.assets=append([]IntegrationGovernanceAsset(nil),refs...)
 namespace:=struct{domain.Scope;DataSubjectID string}{p.facts.Scope,p.record.Player}
 wireHash:=sha256.Sum256(p.facts.Raw)
 payload,e:=json.Marshal(struct{Format string;Namespace any;ActorID,Key,WireBase64,WireSHA256 string}{"loreviva.integration-memory-governance.v1",namespace,p.facts.Actor,p.facts.Key,base64.StdEncoding.EncodeToString(p.facts.Raw),hex.EncodeToString(wireHash[:])});if e!=nil{return nil,e}
 p.payload=payload;p.hash=sha256.Sum256(payload);p.request.PayloadHash=p.hash;p.request.Locks=append([]LockKey(nil),p.keys...)
 return append([]byte(nil),payload...),nil
}
func (s *Store) IntegrationGovernanceAssets(owner *IntegrationGovernanceOwnerBinding,p *IntegrationMemoryGovernancePlan)([]IntegrationGovernanceAsset,error){if _,e:=s.IntegrationGovernanceFacts(owner,p);e!=nil{return nil,e};return append([]IntegrationGovernanceAsset(nil),p.assets...),nil}
func (s *Store) IntegrationGovernancePayload(owner *IntegrationGovernanceOwnerBinding,p *IntegrationMemoryGovernancePlan)([]byte,error){if _,e:=s.IntegrationGovernanceFacts(owner,p);e!=nil{return nil,e};if p.payload==nil{return nil,ErrDenied};return append([]byte(nil),p.payload...),nil}
func (s *Store) AcceptIntegrationGovernance(ctx context.Context,owner *IntegrationGovernanceOwnerBinding,p *IntegrationMemoryGovernancePlan)(CommitReceipt,error){
 if _,e:=s.IntegrationGovernanceFacts(owner,p);e!=nil{return CommitReceipt{},e}
 if ctx.Value(integrationGovernanceContextKey{})!=p||p.payload==nil||p.unit!=nil{return CommitReceipt{},ErrDenied}
 receipt,_,e:=s.barrier(ctx,p.request,func(tx Tx)error{
  u,ok:=tx.(*unit);if !ok||u!=p.unit{return ErrDenied}
  written,e:=owner.owner.ApplyIntegrationGovernance(ctx,p,tx);if e!=nil{return e}
  if written.Task!=p.facts.Task||written.Actor!=p.facts.Actor||written.Hash!=hex.EncodeToString(p.hash[:])||len(written.Document)==0||len(written.Binding)==0{return ErrDenied}
  written.Document=append([]byte(nil),written.Document...);written.Binding=append([]byte(nil),written.Binding...);p.expected=&written
  return nil
 });p.unit=nil;return receipt,e
}
func (s *Store) checkIntegrationGovernance(ctx context.Context,u *unit,r BarrierRequest,final bool)error{
 p:=r.integrationGovernance;if p==nil{return nil}
 if p.unit!=u||u==nil||u.tx==nil||!p.executing||r.Operation!="MEMORY_GOVERNANCE_ACCEPT"||!r.commandOnly||r.CommitKey!=creationID(p.facts.Task,"accept")||r.CommandKey!=p.facts.Key||r.PayloadHash!=p.hash||r.ProductScope!=p.facts.Scope.ProductScope||worldKey(r)!=p.record.World||ctx.Value(integrationGovernanceContextKey{})!=p{return ErrDenied}
 if e:=s.governancePlanCurrent(ctx,u.tx,p);e!=nil{return e}
 var doc []byte;var revision uint64
 e:=u.tx.QueryRowContext(ctx,`SELECT document,revision FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?`,p.record.Tenant,p.record.Product,p.record.World,p.record.Player).Scan(&doc,&revision);if e!=nil{return e}
 if !final {
  if !bytes.Equal(doc,p.facts.Document)||revision!=p.aggregateRevision{return ErrDenied}
  if !p.replay{return nil}
  return s.checkGovernanceAcceptedTx(ctx,u.tx,p,false)
 }
 if p.expected==nil||!jsonEquivalent(doc,p.expected.Document)||revision!=p.aggregateRevision+1{return ErrDenied}
 if e=s.checkGovernanceAcceptedTx(ctx,u.tx,p,true);e!=nil{return e}
 var privacy,view uint64
 if e=u.tx.QueryRowContext(ctx,`SELECT privacy_epoch,view_epoch FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?`,p.record.Tenant,p.record.Product,p.record.Player).Scan(&privacy,&view);e!=nil||privacy!=r.GateVersions.Privacy+1||view!=r.GateVersions.View+1{return ErrDenied}
 var worldVersions domain.Versions;var worldStatus string
 if e=u.tx.QueryRowContext(ctx,`SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?`,p.record.Tenant,p.record.Product,p.record.World).Scan(&worldVersions.ReleaseID,&worldVersions.WorldRevision,&worldVersions.ControlVersion,&worldVersions.PrivacyEpoch,&worldVersions.ViewEpoch,&worldStatus);e!=nil{return e}
 expectedVersions:=*r.Versions;expectedVersions.PrivacyEpoch++;expectedVersions.ViewEpoch++
 if worldVersions!=expectedVersions||worldStatus!=r.ExpectedWorldStatus{return ErrDenied}
 return nil
}
func (s *Store) checkGovernanceAcceptedTx(ctx context.Context,q rowQuerier,p *IntegrationMemoryGovernancePlan,fresh bool)error{
 var hash,binding []byte;var world,subject,owner,preservation,cleanup string
 e:=q.QueryRowContext(ctx,`SELECT world_instance_id,data_subject_id,owner_subject_id,request_hash,binding,preservation,cleanup FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?`,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&world,&subject,&owner,&hash,&binding,&preservation,&cleanup)
 if e!=nil||world!=p.record.World||subject!=p.record.Player||owner!=p.record.Player||!bytes.Equal(hash,p.hash[:]){return ErrDenied}
 if fresh&&(p.expected==nil||!jsonEquivalent(binding,p.expected.Binding)||preservation!="PENDING"||cleanup!="PENDING"){return ErrDenied}
 var good bool
 e=q.QueryRowContext(ctx,`SELECT (t.task_kind='GOVERNANCE' AND t.owner_subject_id=? AND t.world_instance_id=? AND j.operation='MEMORY_GOVERNANCE' AND j.issuer_provenance='PROGRAM_OWNED' AND j.issuer_credential_id IS NULL) FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?`,p.record.Player,p.record.World,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&good)
 if e!=nil||!good{return ErrDenied}
 if fresh {
  var exact bool
  e=q.QueryRowContext(ctx,`SELECT (state='ACCEPTED' AND fence=0 AND scope_kind='WORLD' AND lease_owner IS NULL AND lease_expires_at IS NULL AND cancelled=0 AND calls_used=0 AND project_id IS NULL AND next_run_at=?) FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?`,p.expected.NextRun,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&exact)
  if e!=nil||!exact{return ErrDenied}
 }
 receipt,e:=queryReceipt(ctx,q,p.request.CommitKey);if e!=nil||!receipt.matches(p.request){return ErrDenied}
 return nil
}
func jsonEquivalent(a,b []byte)bool{var x,y any;da:=json.NewDecoder(bytes.NewReader(a));da.UseNumber();db:=json.NewDecoder(bytes.NewReader(b));db.UseNumber();return da.Decode(&x)==nil&&db.Decode(&y)==nil&&reflect.DeepEqual(x,y)}

func IntegrationGovernanceCallerTx(ctx context.Context,tx Tx,scope domain.Scope,right string)(bool,error){
 p,ok:=ctx.Value(integrationGovernanceContextKey{}).(*IntegrationMemoryGovernancePlan);if !ok{return false,nil}
 u,ok:=tx.(*unit);if !ok||p.unit!=u||p.store!=u.operatorStore||!p.active||!p.executing||p.facts.Scope!=scope||u.operation!="MEMORY_GOVERNANCE_ACCEPT"||!u.commandOnly||(right!="memory.govern"&&right!="memory.correct")||(right=="memory.correct"&&p.facts.Action!="CORRECT"){return true,ErrDenied}
 return true,p.store.governancePlanCurrent(ctx,u.tx,p)
}
func (s *Store) acquireGovernanceIdentityKey(ctx context.Context,tx *sql.Tx,r BarrierRequest,k LockKey)(bool,error){
 p:=r.integrationGovernance;if p==nil{return false,nil}
 creationResource:=k.Rank==90&&(k.Kind=="content_release"||k.Kind=="content_check")
 if k.Rank!=15&&k.Rank!=25&&k.Rank!=35&&k.Kind!="integration_consent"&&k.Kind!="integration_receipt"&&!creationResource{return false,nil}
 if p.self!=p||p.store!=s||!p.active||!p.executing||p.owner!=s.integrationGovernanceOwner||ctx.Value(integrationGovernanceContextKey{})!=p||r.Operation!="MEMORY_GOVERNANCE_ACCEPT"{return true,ErrDenied}
 found:=false;for _,v:=range p.keys{found=found||reflect.DeepEqual(v,k)};if !found{return true,ErrDenied}
 if e:=marker(ctx,tx,k);e!=nil{return true,e}
 if k.Kind=="integration_participant"{return true,studioLockRows(ctx,tx,"SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE",k.IDs[0],k.IDs[1],k.IDs[2])}
 if k.Kind=="integration_receipt"{return true,studioLockRows(ctx,tx,"SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE",k.IDs[0],k.IDs[1],k.IDs[2],k.IDs[3])}
 return true,lockStudioRows(ctx,tx,k)
}

type memoryPageBinding struct {
 Format int `json:"fmt"`
 Domain string `json:"domain"`
 Installation string `json:"installation"`
 Tenant string `json:"tenant"`
 Product string `json:"product"`
 Customer string `json:"customer"`
 Player string `json:"player"`
 World string `json:"world"`
 Profile string `json:"profile"`
 Session string `json:"session"`
 Privacy uint64 `json:"privacy"`
 View uint64 `json:"view"`
 AggregateHash string `json:"aggregate_hash"`
}
type memoryPageCursor struct {
 Binding memoryPageBinding `json:"binding"`
 Issued time.Time `json:"issued"`
 Expires time.Time `json:"expires"`
 Last string `json:"last"`
}

type IntegrationMemoryPageRead struct { store *Store; read *IntegrationMemoryRead; codec cipher.AEAD }
func (r *IntegrationMemoryPageRead) Read() *IntegrationMemoryRead { return r.read }
func (r *IntegrationMemoryPageRead) Close() { if r!=nil&&r.read!=nil {r.read.Close()} }
func (s *Store) BeginIntegrationMemoryPage(ctx context.Context,world string,body bool)(*IntegrationMemoryPageRead,error){
 v,ok:=ctx.Value(integrationContextKey{}).(currentIntegration)
 if !ok||v.store!=s||s.integrationSessionRoot==nil{return nil,ErrDenied}
 // Key custody performs filesystem checks before the SQL read transaction opens.
 record,_,e:=s.currentClient(ctx,s.DB,v);if e!=nil{return nil,e}
 material,e:=s.integrationSessionRoot.SessionHMACKey(record.Installation,false);if e!=nil{return nil,e}
 h:=hmac.New(sha256.New,material[:]);h.Write([]byte("MEMORY_LIST_KEY_V1"));block,e:=aes.NewCipher(h.Sum(nil));if e!=nil{return nil,e};codec,e:=cipher.NewGCM(block);if e!=nil{return nil,e}
 read,e:=s.beginIntegrationMemoryRead(ctx,world,!body);if e!=nil{return nil,e}
 return &IntegrationMemoryPageRead{store:s,read:read,codec:codec},nil
}

func (s *Store) memoryPageCodec(ctx context.Context,page *IntegrationMemoryPageRead,profile string)(cipher.AEAD,memoryPageBinding,error){
 var b memoryPageBinding
 if page==nil||page.store!=s||page.read==nil||page.codec==nil{return nil,b,ErrDenied};r:=page.read
 if e:=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{return nil,b,e}
 if profile!=r.record.Profile || (profile!="public_v1"&&profile!="integration_r0_v1") || s.integrationSessionRoot==nil{return nil,b,ErrDenied}
 var view uint64
 if e:=r.tx.QueryRowContext(ctx,`SELECT view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?`,r.record.Tenant,r.record.Product,r.record.World).Scan(&view);e!=nil{return nil,b,e}
 digest:=sha256.Sum256(r.body)
 b=memoryPageBinding{1,"MEMORY_LIST",r.record.Installation,r.record.Tenant,r.record.Product,r.record.Customer,r.record.Player,r.record.World,profile,r.record.ID,r.epoch,view,hex.EncodeToString(digest[:])}
 return page.codec,b,nil
}

// Both cursor calls use the same Store-issued read and same snapshot. A token
// only selects a position; it never substitutes for Begin/current/Finish gates.
func (s *Store) IntegrationMemoryPageCursor(ctx context.Context,page *IntegrationMemoryPageRead,token,profile,last string)(after,next string,view uint64,err error){
 aead,b,e:=s.memoryPageCodec(ctx,page,profile);if e!=nil{return "","",0,e};r:=page.read
 c:=memoryPageCursor{Binding:b,Issued:r.now,Expires:r.now.Add(15*time.Minute)}
 if r.record.Expires.Before(c.Expires){c.Expires=r.record.Expires}
 if token!="" {
  raw,e:=base64.RawURLEncoding.DecodeString(token);if e!=nil||len(token)>4096||len(raw)<aead.NonceSize()+aead.Overhead(){return "","",0,ErrDenied}
  plain,e:=aead.Open(nil,raw[:aead.NonceSize()],raw[aead.NonceSize():],[]byte("MEMORY_LIST_V1"));if e!=nil{return "","",0,ErrDenied}
  if _,e=wire.Parse(plain);e!=nil{return "","",0,ErrDenied};decoder:=json.NewDecoder(bytes.NewReader(plain));decoder.DisallowUnknownFields();if decoder.Decode(&c)!=nil{return "","",0,ErrDenied}
  if !reflect.DeepEqual(c.Binding,b)||!c.Expires.After(r.now)||c.Issued.After(r.now)||!c.Expires.After(c.Issued)||c.Expires.After(c.Issued.Add(15*time.Minute))||c.Expires.After(r.record.Expires)||domain.ValidateID(c.Last)!=nil{return "","",0,ErrDenied}
 }
 after=c.Last
 if last=="" {return after,"",b.View,nil}
 if domain.ValidateID(last)!=nil||last<=after{return "","",0,ErrDenied}
 c.Last=last;plain,e:=json.Marshal(c);if e!=nil{return "","",0,e}
 nonce:=make([]byte,aead.NonceSize());if _,e=rand.Read(nonce);e!=nil{return "","",0,e}
 return after,base64.RawURLEncoding.EncodeToString(aead.Seal(nonce,nonce,plain,[]byte("MEMORY_LIST_V1"))),b.View,nil
}

// Finish rechecks the additional page watermark before closing the original
// transaction, including after the existing read-finish fault hook.
func (s *Store) FinishIntegrationMemoryPage(ctx context.Context,r *IntegrationMemoryRead,view uint64) error {
 if r==nil||r.store!=s||r.closed{return ErrDenied};defer r.Close()
 if s.BeforeReadFinish!=nil {if e:=s.BeforeReadFinish(ctx,r.tx);e!=nil{return e}}
 if e:=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{return e}
 var current uint64
 if e:=r.tx.QueryRowContext(ctx,`SELECT view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?`,r.record.Tenant,r.record.Product,r.record.World).Scan(&current);e!=nil{return e}
 if current!=view{return ErrDenied};return s.finishIntegrationMemoryProof(ctx,r)
}


type integrationGovernanceState struct{ Preservation,Cleanup,State string }
func(s *Store) integrationGovernanceReadState(ctx context.Context,q rowQuerier,p *IntegrationMemoryGovernancePlan)(integrationGovernanceState,error){
 var x integrationGovernanceState
 e:=q.QueryRowContext(ctx,`SELECT m.preservation,m.cleanup,t.state FROM memory_governance m JOIN tasks t USING(tenant_id,product_id,task_id) WHERE m.tenant_id=? AND m.product_id=? AND m.task_id=? AND m.owner_subject_id=? AND m.data_subject_id=? AND m.world_instance_id=?`,p.record.Tenant,p.record.Product,p.facts.Task,p.record.Player,p.record.Player,p.record.World).Scan(&x.Preservation,&x.Cleanup,&x.State)
 if e!=nil{return x,e}
 switch{
 case x.Preservation=="CONFIRMED"&&x.Cleanup=="LOCAL_COMPLETE":
  if x.State!="COMMITTED"{return x,ErrDenied}
  var raw []byte;if e=q.QueryRowContext(ctx,`SELECT binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?`,p.record.Tenant,p.record.Product,p.facts.Task).Scan(&raw);e!=nil{return x,e}
  receipt,e:=queryReceipt(ctx,q,creationID(p.facts.Task,"preserved-clean"));sum:=sha256.Sum256(raw)
  if e!=nil||receipt.tenant!=p.record.Tenant||receipt.product!=p.record.Product||receipt.owner!=p.record.Player||receipt.kind!="WORLD"||!receipt.world.Valid||receipt.world.String!=p.record.World||receipt.task!=p.facts.Task||receipt.operation!="MEMORY_GOVERNANCE"||receipt.command!=p.facts.Task||receipt.phase!="COMMIT"||!bytes.Equal(receipt.hash,sum[:]){return x,ErrDenied}
 case x.Preservation=="BLOCKED_NEEDS_REPLACEMENT"&&x.Cleanup=="PENDING":
  if x.State!="WAITING_EXTERNAL"{return x,ErrDenied}
 case x.Preservation=="PENDING"&&x.Cleanup=="PENDING":
  if x.State!="ACCEPTED"&&x.State!="RUNNING"&&x.State!="WAITING_EXTERNAL"{return x,ErrDenied}
 default:return x,ErrDenied
 };return x,nil
}
