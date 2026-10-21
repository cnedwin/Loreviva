package app

import (
 "encoding/json"
 "errors"
 "loreviva.local/loreviva/internal/generated"
 "loreviva.local/loreviva/internal/memory"
 "loreviva.local/loreviva/internal/wire"
 "net/http"
 "net/url"
 "strconv"
)

func (r *Runtime) integrationMemoryRoutes(mux *http.ServeMux){
 mux.HandleFunc("GET /world/v1/instances/{instance_id}/memories",r.handleIntegrationMemory)
 mux.HandleFunc("POST /world/v1/instances/{instance_id}/memory-governance",func(w http.ResponseWriter,q *http.Request){
  ctx,ok:=r.integrationClient(w,q);if !ok{return};raw,e:=integrationBody(w,q,"MemoryGovernanceCommandV1");if e!=nil{failure(w,e);return}
  out,e:=r.Memory.GovernIntegration(ctx,q.PathValue("instance_id"),q.Header.Get("Idempotency-Key"),raw);if e!=nil{
   var pending *memory.PreservationPending
   if errors.As(e,&pending) {w.Header().Set("Operation-Location","/world/v1/tasks/"+url.PathEscape(pending.TaskID));response(w,503,map[string]any{"code":"DEPENDENCY_UNAVAILABLE","message":"Governance preservation unconfirmed; query the original task.","correlation_id":pending.TaskID,"retryable":false,"recovery_action":"QUERY_ORIGINAL_TASK"});return}
   failure(w,e);return
  };response(w,202,out)
 })
}

// The existing single-item registration delegates here; there is no second
// route owner or Admin fallback. Both profiles share one read and one Finish.
func (r *Runtime) handleIntegrationMemory(w http.ResponseWriter,q *http.Request){
 ctx,ok:=r.integrationClient(w,q);if !ok{return}
 profile,_:=integrationProfile(q);params:=q.URL.Query();id:=q.PathValue("memory_id")
 for k,v:=range params {if len(v)!=1||(k!="include"&&(id!=""||(k!="cursor"&&k!="limit"))){failure(w,wire.ErrJSON);return}}
 include:=params.Get("include");if _,exists:=params["include"];exists&&include!="provenance_v1" {failure(w,wire.ErrJSON);return}
 limit:=50
 if text,exists:=params["limit"];exists {n,e:=strconv.Atoi(text[0]);if e!=nil||n<1||n>100||strconv.Itoa(n)!=text[0]{failure(w,wire.ErrJSON);return};limit=n}
 if _,exists:=params["cursor"];exists&&params.Get("cursor")==""{failure(w,wire.ErrJSON);return}
 page,e:=r.Memory.ListIntegrationMetadata(ctx,q.PathValue("instance_id"),id,params.Get("cursor"),profile,limit,include!="");if e!=nil{failure(w,e);return}
 scope:=generated.WorldScope{TenantID:page.Scope.TenantID,ProductID:page.Scope.ProductID,WorldInstanceID:page.Scope.WorldInstanceID}
 n:=memory.Namespace{Scope:page.Scope,DataSubjectID:page.Scope.SubjectID}
 memories:=[]generated.Memory{};meta:=[]generated.PlayerMemoryMetadata{};provenance:=[]generated.MemoryProvenanceV1{}
 for _,node:=range page.Nodes {
  v:=memoryWire(n,page.Aggregate,node,page.Epoch);memories=append(memories,v)
  meta=append(meta,generated.PlayerMemoryMetadata{MemoryID:v.MemoryID,Scope:scope,ActorInstanceID:v.ActorInstanceID,TargetSubjectID:v.TargetSubjectID,MemoryKind:v.MemoryKind,Revision:v.Revision,PrivacyEpoch:v.PrivacyEpoch,Validity:v.Validity,AllowedUses:v.AllowedUses,BodyReadable:page.BodyReadable})
  if include!="" {p:=generated.MemoryProvenanceV1{Projection:"provenance_v1",Memory:v,EvidenceDomain:string(node.Domain),SourceEpochIDS:[]generated.Identity{},ParentMemoryIDS:identities(node.Parents)};for _,sid:=range node.Sources {p.SourceEpochIDS=append(p.SourceEpochIDS,generated.Identity(page.Aggregate.Sources[sid].Epoch));p.OccurredAtEstimated=p.OccurredAtEstimated||page.Aggregate.Sources[sid].EstimatedTime};provenance=append(provenance,p)}
 }
 var next *string;if page.Next!=""{next=&page.Next}
 var out any
 if id!="" {if include!=""{out=provenance[0]}else if profile=="integration_r0_v1"{out=meta[0]}else{out=memories[0]}} else if include!="" {out=generated.MemoryProvenancePageV1{Scope:scope,Memories:provenance,NextCursor:next}}else if profile=="integration_r0_v1" {raw,_:=json.Marshal(next);out=generated.PlayerMemoryMetadataPage{Scope:scope,Memories:meta,NextCursor:raw,ViewEpoch:generated.ExactUnsigned(strconv.FormatUint(page.View,10))}}else{out=generated.MemoryPage{Scope:scope,Memories:memories,NextCursor:next}}
 response(w,200,out)
}
