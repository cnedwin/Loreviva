package mysql

import (
	"bytes"
	"context"
 "crypto/sha256"
 "encoding/hex"
	"database/sql"
	"encoding/json"
 "fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"time"
)

// IntegrationMemoryRead is Store-issued and retains its exact CLIENT, world,
// memory and publication locks until Finish or Close. Getters are copies only.
type IntegrationMemoryRead struct {
 self *IntegrationMemoryRead
	store         *Store
	tx            *sql.Tx
	principal     currentIntegration
	record        sessionRecord
	eligibility   sessionEligibility
	scope         domain.Scope
	catalog       []creationPublication
	keys          []LockKey
	initial, body []byte
	lineage       *IntegrationInitialLineage
	epoch         uint64
 aggregateID string
 aggregateRevision uint64
	now           time.Time
	release       func()
	closed        bool
	metadataOnly  bool
 proofPhase string
 proofState []byte
 proofValid, finalBodyReadable bool
 bodyCandidate, componentsSealed, selectionSealed bool
 provenItems map[string]bool
 selectedItems []string
 births []integrationBirthReadStamp
 history []IntegrationMemoryHistory
}

func (r *IntegrationMemoryRead) Scope() domain.Scope { return r.scope }
func (r *IntegrationMemoryRead) Body() []byte {
 if r==nil||r.self!=r||r.closed{return nil}
 if r.metadataOnly{return r.MetadataBody()}
 if !r.selectionSealed{return nil}
 var document map[string]json.RawMessage
 var nodes map[string]json.RawMessage
 var sources map[string]json.RawMessage
 if json.Unmarshal(r.body,&document)!=nil||json.Unmarshal(document["Nodes"],&nodes)!=nil||json.Unmarshal(document["Sources"],&sources)!=nil{return nil}
 selected:=map[string]json.RawMessage{};visibleSources:=map[string]json.RawMessage{}
 for _,id:=range r.selectedItems{if !r.provenItems[id]{return nil};raw,ok:=nodes[id];if !ok{return nil};selected[id]=raw;var n struct{Sources []string};if json.Unmarshal(raw,&n)!=nil{return nil};for _,sid:=range n.Sources{visibleSources[sid]=sources[sid]}}
 document["Nodes"],_=json.Marshal(selected);document["Sources"],_=json.Marshal(visibleSources)
 body,_:=json.Marshal(document);return body
}
// Visibility data only; it cannot reveal source/node bodies from an unproved component.
func (r *IntegrationMemoryRead) MetadataBody() []byte {
 if r==nil||r.self!=r||r.closed{return nil}
	// This copy retains only visibility inputs and required source identity.
	// Body, source text, epochs and occurrence metadata never leave the handle.
	var safe struct {
		Nodes map[string]struct {
			ID, Kind, ActorID, TargetID, Audience string
			Revision                              uint64
			Sources, Parents, Uses                []string
			Blocked                               bool
			ExpiresAt                             time.Time
		}
		Sources map[string]struct {
			ID, Domain, Audience string
			Revision             uint64
			Uses                 []string
			Blocked              bool
		}
		Grants []struct {
			ActorID, SourceID string
			Uses              []string
			Revoked           bool
			ExpiresAt         time.Time
		}
		Acquisitions []struct{ ActorID, SourceID, Kind, EvidenceID string }
		Disabled     []string
	}
	if json.Unmarshal(r.body, &safe) != nil {
		return nil
	}
	body, _ := json.Marshal(safe)
	return body
}
func (r *IntegrationMemoryRead) Lineage() *IntegrationInitialLineage {
	if r.metadataOnly {
		return nil
	}
	return creationCopy(r.lineage)
}
func (r *IntegrationMemoryRead) Epoch() uint64  { return r.epoch }
func (r *IntegrationMemoryRead) Now() time.Time { return r.now }
func (r *IntegrationMemoryRead) Publications() []generated.Release {
	// A metadata handle does not grant access to published source bodies.
	if r.metadataOnly {
		return nil
	}
	out := []generated.Release{}
	for _, p := range r.catalog {
		out = append(out, creationCopy(p.Release))
	}
	return out
}
func (r *IntegrationMemoryRead) Close() {
	if r == nil || r.self!=r || r.closed {
		return
	}
	r.closed = true
	if r.tx != nil { _ = r.tx.Rollback(); r.tx=nil }
 r.proofPhase="CLOSED";r.proofState=nil;r.history=nil;r.provenItems=nil;r.selectedItems=nil
	r.release()
}
func (s *Store) CheckIntegrationMemoryRead(ctx context.Context, r *IntegrationMemoryRead) error {
 if r==nil{return ErrDenied};return s.checkIntegrationMemoryReadMode(ctx,r,r.metadataOnly)
}
func(s *Store) checkIntegrationMemoryReadMode(ctx context.Context,r *IntegrationMemoryRead,metadataOnly bool)error{
	if r == nil || r.self!=r || r.store != s || r.closed || r.tx == nil {
		return ErrDenied
	}
	current, eligible, e := s.currentClient(ctx, r.tx, r.principal)
	if e != nil || !reflect.DeepEqual(current, r.record) || !reflect.DeepEqual(eligible, r.eligibility) {
		return ErrDenied
	}
	found := false
	for _, a := range current.Actions {
		found = found || a == "MEMORY_VIEW"
	}
	if !found {
		return ErrDenied
	}
	if metadataOnly {
		// Source visibility cannot replace the caller's current metadata permission.
		// Begin and Finish share this exact check under the subject lock and Tx.
		var permitted bool
		e = r.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.metadata.read' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))`, current.Tenant, current.Product, current.Player, current.Player).Scan(&permitted)
		if e != nil || !permitted {
			return ErrDenied
		}
	} else {
		snapshot, _, e := s.consentSnapshot(ctx, r.tx, current, eligible)
		if e != nil || !snapshot.CurrentUseEnabled {
			return ErrDenied
		}
		var permitted bool
		e = r.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.private_body.read' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))`, current.Tenant, current.Product, current.Player, current.Player).Scan(&permitted)
		if e != nil || !permitted {
			return ErrDenied
		}
	}
	u := &unit{tx: r.tx, scope: r.scope.ProductScope, plan: map[string]bool{}}
	for _, k := range r.keys {
		u.plan[string(lockBytes(k))] = true
	}
	for _, pub := range r.catalog {
		if e = s.checkCreationPublicationTx(ctx, u, pub); e != nil {
			return e
		}
	}
	var initial, body []byte
	var epoch,aggregateRevision uint64
 var aggregateID string
	e = r.tx.QueryRowContext(ctx, `SELECT i.initialization,m.document,g.privacy_epoch,m.aggregate_id,m.revision FROM runtime_instances i JOIN memory_aggregates m USING(tenant_id,product_id,world_instance_id) JOIN subject_governance g ON g.tenant_id=m.tenant_id AND g.product_id=m.product_id AND g.subject_id=m.data_subject_id WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=? AND i.owner_subject_id=? AND i.mode='PRIVATE' AND m.data_subject_id=?`, current.Tenant, current.Product, current.World, current.ServiceSubject, current.Player).Scan(&initial, &body, &epoch, &aggregateID, &aggregateRevision)
	if e != nil || !bytes.Equal(initial, r.initial) || !bytes.Equal(body, r.body) || epoch != r.epoch || aggregateID!=r.aggregateID || aggregateRevision!=r.aggregateRevision {
		return ErrDenied
	}

 if r.proofValid {
  rows,e:=s.integrationMemoryHistory(ctx,r.tx,r);if e!=nil||!reflect.DeepEqual(rows,r.history){return ErrDenied}
 births,_,e:=s.integrationMemoryBirths(ctx,r.tx,r);if e!=nil||!reflect.DeepEqual(births,r.births){return ErrDenied}
 }
 return nil
}
func (r *IntegrationMemoryRead) Finish(ctx context.Context) error {
	if r == nil || r.self!=r || r.closed {
		return ErrDenied
	}
	defer r.Close()
	if r.store.BeforeReadFinish != nil {
		if e := r.store.BeforeReadFinish(ctx, r.tx); e != nil {
			return e
		}
	}
	return r.store.finishIntegrationMemoryProof(ctx,r)
}
func (s *Store) BeginIntegrationMemoryRead(ctx context.Context, world string) (*IntegrationMemoryRead, error) {
	return s.beginIntegrationMemoryRead(ctx, world, false)
}

// This separate Store-issued read never grants body or provenance authority.
func (s *Store) BeginIntegrationMemoryMetadata(ctx context.Context, world string) (*IntegrationMemoryRead, error) {
	return s.beginIntegrationMemoryRead(ctx, world, true)
}
func (s *Store) beginIntegrationMemoryRead(ctx context.Context, world string, metadataOnly bool) (*IntegrationMemoryRead, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		cancel()
		return nil, e
	}
	release := func() { done(); cancel() }
	good := false
	defer func() {
		if !good {
			release()
		}
	}()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || domain.ValidateID(world) != nil {
		return nil, ErrDenied
	}
	record, eligible, e := s.currentClient(ctx, s.DB, v)
	if e != nil || record.World != world {
		return nil, ErrDenied
	}
	scope := domain.Scope{ProductScope: domain.ProductScope{TenantID: record.Tenant, ProductID: record.Product, SubjectID: record.Player}, WorldInstanceID: world}
	r := &IntegrationMemoryRead{store: s, principal: v, record: record, eligibility: eligible, scope: scope, release: release, metadataOnly: metadataOnly};r.self=r
	r.keys = append(consentKeys(record, eligible, ""), LockKey{Rank: 90, Kind: "memory", IDs: []string{record.Tenant, record.Product, world, creationID("MEMORY_SUBJECT", world, record.Player)}})
	queue := []string{eligible.entitlement.Release}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		if len(seen) >= 200 {
			return nil, ErrDenied
		}
		seen[id] = true
		pub, e := s.creationPublication(ctx, scope.ProductScope, id)
		if e != nil {
			return nil, e
		}
		r.catalog = append(r.catalog, pub)
		r.keys = append(r.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{record.Tenant, record.Product, id}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{record.Tenant, record.Product, pub.Result}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{pub.Result}})
		for _, dep := range pub.Release.DependencyLocks {
			queue = append(queue, string(dep.ReleaseID))
		}
	}
	r.tx, e = s.integrationMemoryReadTx(ctx, r.keys)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !good {
			if r.tx!=nil {_ = r.tx.Rollback()}
		}
	}()
	e = r.tx.QueryRowContext(ctx, `SELECT i.initialization,m.document,g.privacy_epoch,m.aggregate_id,m.revision,UTC_TIMESTAMP(6) FROM runtime_instances i JOIN memory_aggregates m USING(tenant_id,product_id,world_instance_id) JOIN subject_governance g ON g.tenant_id=m.tenant_id AND g.product_id=m.product_id AND g.subject_id=m.data_subject_id WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=? AND i.owner_subject_id=? AND i.mode='PRIVATE' AND m.data_subject_id=?`, record.Tenant, record.Product, world, record.ServiceSubject, record.Player).Scan(&r.initial, &r.body, &r.epoch, &r.aggregateID, &r.aggregateRevision, &r.now)
	if e != nil {
		return nil, ErrDenied
	}
 if r.aggregateID!=creationID("MEMORY_SUBJECT",world,record.Player)||r.aggregateRevision==0{return nil,ErrDenied}
	var init struct {
		IntegrationInitialLineage *IntegrationInitialLineage
		CreationCommandID         string
		Participants              []string
	}
	if json.Unmarshal(r.initial, &init) != nil || init.CreationCommandID != world || !reflect.DeepEqual(init.Participants, []string{record.Player}) || init.IntegrationInitialLineage == nil {
		return nil, ErrDenied
	}
	r.lineage = init.IntegrationInitialLineage
	if r.lineage.Tenant != record.Tenant || r.lineage.Product != record.Product || r.lineage.Customer != record.Customer || r.lineage.Player != record.Player || r.lineage.World != world || r.lineage.CreationReceipt != world {
		return nil, ErrDenied
	}
	if e = s.CheckIntegrationMemoryRead(ctx, r); e != nil {
		return nil, e
	}

 candidate,e:=s.MemoryBodyEligibility(ctx,r);if e!=nil{return nil,e};r.bodyCandidate=candidate
 if candidate && s.integrationGovernanceOwner!=nil {
  // The initial SQL phase ends before the bound owner opens its Ledger.
  _=r.tx.Rollback();r.tx=nil;r.proofPhase="PREPARE"
  proof,pe:=s.integrationGovernanceOwner.owner.PrepareIntegrationMemoryProof(ctx,r)
  r.proofPhase="SEALED";r.proofValid=pe==nil&&r.componentsSealed
  if pe==nil {r.proofState=append([]byte(nil),proof...)} else {r.history=nil}
  if !metadataOnly && pe!=nil{return nil,fmt.Errorf("%w: memory proof %v",ErrDenied,pe)}
  r.tx,e=s.integrationMemoryReadTx(ctx,r.keys);if e!=nil{return nil,e}
  if e=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{return nil,e}
 } else if !metadataOnly {return nil,ErrDenied}
 good = true
 return r, nil
}

// Content receipts use their existing commit-key lock, not the unrelated
// Studio-settings rank-110 tuple. Marker ordering stays shared with writers.
func (s *Store) integrationMemoryReadTx(ctx context.Context, keys []LockKey) (*sql.Tx, error) {
	ordered, e := PlanLocks(keys)
	if e != nil {
		return nil, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	good := false
	defer func() {
		if !good {
			_ = tx.Rollback()
		}
	}()
	for _, k := range ordered {
		if e = marker(ctx, tx, k); e != nil {
			return nil, e
		}
		switch k.Kind {
		case "integration_receipt":
			e = studioLockRows(ctx, tx, "SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
		case "integration_participant":
			e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "commit_receipts":
			e = studioLockRows(ctx, tx, "SELECT commit_key FROM commit_receipts WHERE commit_key=? FOR UPDATE", k.IDs[0])
		case "content_check":
			e = studioLockRows(ctx, tx, "SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "memory":
			e = studioLockRows(ctx, tx, "SELECT aggregate_id FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND aggregate_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
		default:
			if k.Rank == 50 {
				// Task metadata reads acquire the same exact task/quote marker and rows.
				for _, table := range []string{"tasks", "harness_quotes"} {
					column := "task_id"
					if table == "harness_quotes" {
						column = "quote_id"
					}
					e = studioLockRows(ctx, tx, "SELECT "+column+" FROM "+table+" WHERE tenant_id=? AND product_id=? AND "+column+"=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
					if e != nil {
						break
					}
				}
			} else if k.Rank == 40 {
				for _, table := range []string{"world_controls", "runtime_instances", "delivery_streams"} {
					e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
					if e != nil {
						break
					}
				}
			} else {
				e = lockStudioRows(ctx, tx, k)
			}
		}
		if e != nil {
			return nil, e
		}
	}
	var ready bool
	if e = tx.QueryRowContext(ctx, "SELECT applied_watermark>=required_watermark AND (fresh_synthetic=1 OR required_watermark>0) FROM governance_watermark WHERE singleton=1").Scan(&ready); e != nil || !ready {
		return nil, ErrDenied
	}
	good = true
	return tx, nil
}


// These facts are available only during the named installed owner's invocation.
// Neither a caller-created handle nor a copied binding can open this channel.
type IntegrationMemoryHistory struct {
 Task,Hash,Preservation,Cleanup,State string
 Binding []byte
 Accept,Complete receiptRow
}
type IntegrationMemoryProofFacts struct {
 Scope domain.Scope
 Initial,Document []byte
 Publications []generated.Release
 Lineage *IntegrationInitialLineage
 History []IntegrationMemoryHistory
 Births []IntegrationMemoryBirthEvidence
 AggregateRevision uint64
}
func(s *Store) memoryProofOwner(owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead) error {
 if owner==nil||owner!=s.integrationGovernanceOwner||owner.store!=s||r==nil||r.self!=r||r.store!=s||r.closed||r.proofPhase!="PREPARE"||r.tx!=nil||s.integrationClosed.Load(){return ErrDenied};return nil
}
func(s *Store) IntegrationMemoryProofInput(owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead)(IntegrationMemoryProofFacts,error){
 if e:=s.memoryProofOwner(owner,r);e!=nil{return IntegrationMemoryProofFacts{},e}
 f:=IntegrationMemoryProofFacts{AggregateRevision:r.aggregateRevision,Scope:r.scope,Initial:append([]byte(nil),r.initial...),Document:append([]byte(nil),r.body...),Lineage:creationCopy(r.lineage)}
 for _,p:=range r.catalog{f.Publications=append(f.Publications,creationCopy(p.Release))};return f,nil
}
func(s *Store) integrationMemoryHistory(ctx context.Context,tx *sql.Tx,r *IntegrationMemoryRead)([]IntegrationMemoryHistory,error){
 rows,e:=tx.QueryContext(ctx,`SELECT task_id,request_hash,binding,preservation,cleanup,owner_subject_id FROM memory_governance WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? ORDER BY task_id`,r.record.Tenant,r.record.Product,r.record.World,r.record.Player)
 if e!=nil{return nil,e}
 out:=[]IntegrationMemoryHistory{}
 for rows.Next(){var x IntegrationMemoryHistory;var raw []byte;var owner string
  if e=rows.Scan(&x.Task,&raw,&x.Binding,&x.Preservation,&x.Cleanup,&owner);e!=nil{rows.Close();return nil,e}
  if owner!=r.record.Player||len(raw)!=32{rows.Close();return nil,ErrDenied};x.Hash=hex.EncodeToString(raw);out=append(out,x)
 }
 e=rows.Err();rows.Close();if e!=nil{return nil,e}
 for i:=range out {x:=&out[i];var ok bool
  e=tx.QueryRowContext(ctx,`SELECT t.state,(t.task_kind='GOVERNANCE' AND t.owner_subject_id=? AND t.world_instance_id=? AND j.operation='MEMORY_GOVERNANCE' AND j.issuer_provenance='PROGRAM_OWNED' AND j.issuer_credential_id IS NULL) FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?`,r.record.Player,r.record.World,r.record.Tenant,r.record.Product,x.Task).Scan(&x.State,&ok)
  if e!=nil||!ok{return nil,ErrDenied}
  x.Accept,e=queryReceipt(ctx,tx,creationID(x.Task,"accept"));if e!=nil{return nil,e}
  a:=x.Accept
  if domain.ValidateIdempotencyKey(a.command)!=nil||x.Task!=creationID(r.record.Tenant,r.record.Product,r.record.Player,r.record.World,"MEMORY_GOVERNANCE",a.command){return nil,ErrDenied}
  if a.tenant!=r.record.Tenant||a.product!=r.record.Product||a.owner!=r.record.Player||a.kind!="WORLD"||!a.world.Valid||a.world.String!=r.record.World||a.task!=""||a.operation!="MEMORY_GOVERNANCE_ACCEPT"||a.phase!="COMMIT"||hex.EncodeToString(a.hash)!=x.Hash{return nil,ErrDenied}
  if x.Preservation!="CONFIRMED"||x.Cleanup!="LOCAL_COMPLETE"||x.State!="COMMITTED"{return nil,ErrDenied}
  x.Complete,e=queryReceipt(ctx,tx,creationID(x.Task,"preserved-clean"));if e!=nil{return nil,e}
  b:=x.Complete;sum:=sha256.Sum256(x.Binding)
  if b.tenant!=a.tenant||b.product!=a.product||b.owner!=a.owner||b.kind!="WORLD"||!b.world.Valid||b.world.String!=a.world.String||b.task!=x.Task||b.operation!="MEMORY_GOVERNANCE"||b.command!=x.Task||b.phase!="COMMIT"||!bytes.Equal(b.hash,sum[:]){return nil,ErrDenied}
 }
 return out,nil
}
func(s *Store) PlanIntegrationMemoryHistory(ctx context.Context,owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead,reservationCount int)([]IntegrationMemoryHistory,error){
 if e:=s.memoryProofOwner(owner,r);e!=nil{return nil,e}
 tx,e:=s.integrationMemoryReadTx(ctx,r.keys);if e!=nil{return nil,e}
 r.tx=tx
 if e=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{tx.Rollback();r.tx=nil;return nil,e}
 history,e:=s.integrationMemoryHistory(ctx,tx,r);tx.Rollback();r.tx=nil
 if e!=nil{return nil,e};if len(history)>reservationCount{return nil,ErrDenied}
 r.history=history
 for _,row:=range history{
  r.keys=append(r.keys,LockKey{Rank:50,IDs:[]string{r.record.Tenant,r.record.Product,row.Task}},
   LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{creationID(row.Task,"accept")}},
   LockKey{Rank:110,Kind:"commit_receipts",IDs:[]string{creationID(row.Task,"preserved-clean")}})
 }
 // Return only typed body-free row copies; receiptRow's private fields stay
 // Store-owned and are checked again with all precollected locks.
 out:=make([]IntegrationMemoryHistory,len(history));copy(out,history)
 for i:=range out{out[i].Binding=append([]byte(nil),history[i].Binding...)}
 return out,nil
}
func(r *IntegrationMemoryRead) FinalBodyReadable()bool{return r!=nil&&r.closed&&r.finalBodyReadable}
func(s *Store) finishIntegrationMemoryProof(ctx context.Context,r *IntegrationMemoryRead)error{
 if e:=s.CheckIntegrationMemoryRead(ctx,r);e!=nil{return e}
 eligible,e:=s.MemoryBodyEligibility(ctx,r);if e!=nil{return e}
 r.finalBodyReadable=eligible&&r.bodyCandidate&&r.proofValid&&r.selectionSealed&&len(r.selectedItems)>0
 for _,id:=range r.selectedItems{r.finalBodyReadable=r.finalBodyReadable&&r.provenItems[id]}
 if !r.metadataOnly&&!r.finalBodyReadable{return ErrDenied}
 if r.proofValid{
  _=r.tx.Rollback();r.tx=nil;r.proofPhase="FINAL"
  if e=s.integrationGovernanceOwner.owner.FinishIntegrationMemoryProof(ctx,r,append([]byte(nil),r.proofState...));e!=nil{r.finalBodyReadable=false;return e}
 }
 return nil
}
func(s *Store) CheckIntegrationMemoryProofFinal(owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead)error{
 if owner==nil||owner!=s.integrationGovernanceOwner||r==nil||r.self!=r||r.store!=s||r.closed||r.tx!=nil||r.proofPhase!="FINAL"||!r.proofValid{return ErrDenied};return nil
}

// Installed-owner-only output of PREPARE, not a caller-supplied authorization.
func(s *Store) SealIntegrationMemoryComponents(owner *IntegrationGovernanceOwnerBinding,r *IntegrationMemoryRead,ids []string)error{
 if e:=s.memoryProofOwner(owner,r);e!=nil{return e};if r.componentsSealed{return ErrDenied}
 var a struct{Nodes map[string]json.RawMessage};if json.Unmarshal(r.body,&a)!=nil{return ErrDenied}
 proven:=map[string]bool{};for _,id:=range ids{if _,ok:=a.Nodes[id];!ok||proven[id]{return ErrDenied};proven[id]=true}
 r.provenItems=proven;r.componentsSealed=true;return nil
}
// A read caller can only narrow the owner's frozen proof to actual output IDs.
func(r *IntegrationMemoryRead) SelectItems(ids []string)error{
 if r==nil||r.self!=r||r.closed||r.tx==nil||r.selectionSealed{return ErrDenied}
 var a struct{Nodes map[string]json.RawMessage};if json.Unmarshal(r.body,&a)!=nil{return ErrDenied}
 seen:=map[string]bool{};for _,id:=range ids{if _,ok:=a.Nodes[id];!ok||seen[id]{return ErrDenied};seen[id]=true;if !r.metadataOnly&&(!r.proofValid||!r.provenItems[id]){return ErrDenied}}
 r.selectedItems=append([]string(nil),ids...);r.selectionSealed=true;return nil
}
