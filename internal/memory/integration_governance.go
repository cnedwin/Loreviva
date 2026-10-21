package memory

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "reflect"
 "time"
 "loreviva.local/loreviva/internal/generated"
 store "loreviva.local/loreviva/internal/store/mysql"
)

func (s *MemoryService) BindIntegrationGovernance() error {
 s.governanceMu.Lock();defer s.governanceMu.Unlock()
 if s.Store==nil||s.Files==nil||s.Files.DB!=s.Store||s.Ledger==nil||s.Ledger.participant==nil{return store.ErrDenied}
 if s.governanceBinding!=nil {if s.governanceFiles!=s.Files||s.governanceLedger!=s.Ledger{return store.ErrDenied};return nil}
 binding,e:=s.Store.BindIntegrationGovernanceOwner(s);if e!=nil{return e}
 s.governanceBinding=binding;s.governanceFiles=s.Files;s.governanceLedger=s.Ledger;return nil
}
func (s *MemoryService) integrationGovernanceOwner(ctx context.Context)(*store.IntegrationGovernanceOwnerBinding,error){
 binding,e:=s.integrationGovernanceBinding();if e!=nil{return nil,e}
 if _,e=s.Ledger.participant.GovernanceJournalIdentity(ctx);e!=nil{return nil,e};return binding,nil
}
func (s *MemoryService) integrationGovernanceBinding()(*store.IntegrationGovernanceOwnerBinding,error){
 s.governanceMu.Lock();defer s.governanceMu.Unlock()
 if s.governanceBinding==nil||s.governanceFiles!=s.Files||s.governanceLedger!=s.Ledger||s.Files.DB!=s.Store||s.Ledger.participant==nil{return nil,store.ErrDenied}
 return s.governanceBinding,nil
}
func (s *MemoryService) GovernIntegration(ctx context.Context,world,key string,raw []byte)(generated.TaskReceipt,error){
 var out generated.TaskReceipt
 if _,e:=s.integrationGovernanceOwner(ctx);e!=nil{return out,e}
 p,e:=s.Store.PlanIntegrationMemoryGovernance(ctx,world,key,raw);if e!=nil{return out,e}
 return s.Store.GovernIntegrationMemory(ctx,p)
}
func governanceCommand(f store.IntegrationGovernanceFacts) MemoryGovernanceCommand {
 return MemoryGovernanceCommand{Key:f.Key,Action:f.Action,Replacement:f.Replacement,Selector:Selector{Kind:"IDS",TargetKind:"MEMORY_NODE",IDs:append([]string(nil),f.IDs...),Expected:f.Expected}}
}
func (s *MemoryService) ExecuteIntegrationGovernance(ctx context.Context,p *store.IntegrationMemoryGovernancePlan)(generated.TaskReceipt,error){
 var out generated.TaskReceipt
 owner,e:=s.integrationGovernanceOwner(ctx);if e!=nil{return out,e};f,e:=s.Store.IntegrationGovernanceFacts(owner,p);if e!=nil{return out,e}
 n:=Namespace{Scope:f.Scope,DataSubjectID:f.Scope.SubjectID}
 // Resolve the same original graph in its actual memory owner. Store checks
 // every retained identity too; neither a caller actor nor a scope grants it.
 var a Aggregate;if json.Unmarshal(f.Document,&a)!=nil{return out,ErrInvalid}
 for _,id:=range f.IDs {node,ok:=a.Nodes[id];if !ok||node.ActorID!=f.Actor||node.TargetID!=n.DataSubjectID||(f.Action=="CORRECT"&&node.Domain==WorldFact){return out,store.ErrDenied}}
 intake,finish,e:=s.Ledger.beginIntake(ctx);if e!=nil{return out,e};defer finish()
 keys,assets,e:=s.assetLocks(ctx,n);if e!=nil{return out,e};refs:=make([]store.IntegrationGovernanceAsset,0,len(assets));for _,a:=range assets{refs=append(refs,store.IntegrationGovernanceAsset{ID:a.ID,Storage:a.Storage})}
 payload,e:=s.Store.PrepareIntegrationGovernanceAccept(owner,p,keys,refs);if e!=nil{return out,e};hash:=sha256.Sum256(payload)
 pending:=&PreservationPending{TaskID:f.Task,InstanceID:n.WorldInstanceID,ProductID:n.ProductID,DataSubjectID:n.DataSubjectID,ActorID:f.Actor}
 reservation:=Reservation{ID:f.Task,RequestHash:hex.EncodeToString(hash[:]),Scope:[]string{n.TenantID,n.ProductID,n.WorldInstanceID,n.DataSubjectID,n.SubjectID}}
 fresh,e:=s.Ledger.reserve(ctx,reservation);if e!=nil{
  // Distinguish an actual retained original with different exact wire bytes
  // from missing/corrupt preservation. Never turn an uncertain read into a
  // conflict or replace the original Ledger reservation.
  prior,readErr:=s.Ledger.Lookup(ctx,f.Task)
  if readErr==nil&&prior.Reservation.ID==reservation.ID&&reflect.DeepEqual(prior.Reservation.Scope,reservation.Scope)&&prior.Reservation.RequestHash!=reservation.RequestHash{return out,store.ErrConflict}
  return out,e
 }
 _,e=s.Store.AcceptIntegrationGovernance(ctx,owner,p)
 if e!=nil {
  if errors.Is(e,store.ErrCommitUnknown)||!fresh{return out,pending}
  originalError:=e;proof,proofError:=s.Store.ProveMemoryRejection(ctx,e,f.Task);if proofError!=nil{return out,errors.Join(pending,originalError,proofError)}
  if e=s.Ledger.SealRejectedOnce(ctx,intake,f.Task,proof);e!=nil{return out,pending}
  return out,originalError
 }
 var raw []byte
 if e=s.Store.DB.QueryRowContext(ctx,"SELECT binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?",n.TenantID,n.ProductID,f.Task,n.SubjectID).Scan(&raw);e!=nil{return out,pending}
 var binding Binding;if json.Unmarshal(raw,&binding)!=nil{return out,pending}
 if !sameGovernanceSelection(binding,f){return out,pending}
 if e=s.Ledger.AppendOnce(ctx,f.Task,binding);e!=nil{return out,pending}
 lease,e:=s.Store.ClaimRegistered(ctx,"memory-preserver",time.Now(),time.Minute,[]store.RegisteredOperation{{Kind:"GOVERNANCE",Operation:"MEMORY_GOVERNANCE",TaskID:f.Task}})
 if e==nil {if e=s.Process(ctx,lease,"memory-preserver");e!=nil{return out,pending}}else if !errors.Is(e,store.ErrNoTask){return out,pending}
 view,e:=s.Store.ReadIntegrationGovernanceTask(ctx,f.Task);if e!=nil{return out,pending};return view.Task,nil
}
func sameGovernanceSelection(b Binding,f store.IntegrationGovernanceFacts)bool{
 if b.Action!=f.Action||b.TargetKind!="MEMORY_NODE"||len(b.Targets)!=len(f.IDs)||!reflect.DeepEqual(b.Revisions,f.Expected){return false}
 seen:=map[string]bool{};for _,id:=range f.IDs{seen[id]=true};for _,id:=range b.Targets{if !seen[id]{return false};delete(seen,id)};return len(seen)==0
}
func (s *MemoryService) ApplyIntegrationGovernance(ctx context.Context,p *store.IntegrationMemoryGovernancePlan,tx store.Tx)(store.IntegrationGovernanceWritten,error){
 var out store.IntegrationGovernanceWritten
 owner,e:=s.integrationGovernanceBinding();if e!=nil{return out,e};f,e:=s.Store.IntegrationGovernanceFacts(owner,p);if e!=nil{return out,e}
 n:=Namespace{Scope:f.Scope,DataSubjectID:f.Scope.SubjectID}
 // Asset selection was frozen before the barrier; the common body must not
 // perform filesystem work or open an independent DB connection under SQL.
 ids,e:=s.Store.IntegrationGovernanceAssets(owner,p);if e!=nil{return out,e}
 refs:=make([]assetRef,0,len(ids));for _,a:=range ids{refs=append(refs,assetRef{ID:a.ID,Storage:a.Storage})}
 payload,e:=s.Store.IntegrationGovernancePayload(owner,p);if e!=nil{return out,e};hash:=sha256.Sum256(payload)
 out=store.IntegrationGovernanceWritten{Task:f.Task,Actor:f.Actor,Hash:hex.EncodeToString(hash[:])}
 e=s.applyGovernanceTx(ctx,tx,n,governanceCommand(f),f.Task,hash,refs,&out);return out,e
}
func (s *MemoryService) InspectIntegrationGovernance(ctx context.Context,read *store.IntegrationGovernanceRead)(generated.TaskView,error){
 var out generated.TaskView
 owner,e:=s.integrationGovernanceOwner(ctx);if e!=nil{return out,e};f,e:=s.Store.IntegrationGovernanceReadFacts(owner,read);if e!=nil{return out,e}
 entry,e:=s.Ledger.Lookup(ctx,f.Task);if e!=nil||(entry.Kind!="APPENDED"&&entry.Kind!="RESERVED")||!reflect.DeepEqual(entry.Reservation.Scope,[]string{f.Scope.TenantID,f.Scope.ProductID,f.Scope.WorldInstanceID,f.Scope.SubjectID,f.Scope.SubjectID}){return out,store.ErrDenied}
 // Only a genuine pre-existing reservation and original ACCEPT can enter the
 // original preservation worker. This read does not replay or mint a command.
 raw,e:=s.Store.ReadIntegrationGovernanceRecovery(ctx,owner,read,entry.Reservation.RequestHash);if e!=nil{return out,e}
 var accepted Binding;if json.Unmarshal(raw,&accepted)!=nil||accepted.TargetKind!="MEMORY_NODE"||(accepted.Action!="DELETE"&&accepted.Action!="CORRECT")||len(accepted.Targets)==0||len(accepted.Targets)!=len(accepted.Revisions){return out,store.ErrDenied}
 if entry.Kind=="APPENDED"&&(entry.Binding==nil||!reflect.DeepEqual(*entry.Binding,accepted)){return out,store.ErrDenied}
 // Recovery is an existing reservation, not a new intake. AppendOnce and the
 // original Process own their Ledger state locks and participant admissions;
 // reacquiring governance.lock here would deadlock a fresh caller's intake.
 if e=s.Ledger.AppendOnce(ctx,f.Task,accepted);e!=nil{return out,e}
 lease,e:=s.Store.ClaimRegistered(ctx,"memory-preserver",time.Now(),time.Minute,[]store.RegisteredOperation{{Kind:"GOVERNANCE",Operation:"MEMORY_GOVERNANCE",TaskID:f.Task}})
 if e==nil{if e=s.Process(ctx,lease,"memory-preserver");e!=nil{return out,e}}else if !errors.Is(e,store.ErrNoTask){return out,e}
 entry,e=s.Ledger.Lookup(ctx,f.Task);if e!=nil||entry.Kind!="APPENDED"||entry.Binding==nil||!reflect.DeepEqual(*entry.Binding,accepted){return out,store.ErrDenied}
 binding,e:=json.Marshal(entry.Binding);if e!=nil{return out,e}
 out,e=s.Store.FinishIntegrationGovernanceTask(ctx,owner,read,entry.Reservation.RequestHash,binding);if e!=nil{return out,e}
 final,e:=s.Ledger.Lookup(ctx,f.Task);if e!=nil||!reflect.DeepEqual(final,entry){return generated.TaskView{},store.ErrDenied};return out,nil
}
