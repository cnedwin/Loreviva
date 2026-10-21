package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"sort"
	"strconv"
	"time"
)

type DeliverySnapshotOwner interface {
	DeliverySnapshotStore() *Store
	PlanIntegrationSnapshot(context.Context, *IntegrationSnapshotPlan) ([]LockKey, error)
	SnapshotTx(context.Context, *IntegrationSnapshotUnit) (IntegrationSnapshotProjection, error)
}
type IntegrationSnapshotProjection struct {
	State         map[string]json.RawMessage
	Actors        []generated.ActorView
	Relationships []generated.RelationshipView
}
type SnapshotPlanFacts struct {
	Scope                         domain.Scope
	WorldRevision                 uint64
	ActorIDs, ItemIDs, ReleaseIDs []string
}
type SnapshotRecord struct {
	ID   string
	Body json.RawMessage
}
type SnapshotPublicationFact struct {
	ReleaseID, PublisherSubjectID, ContentHash, DocumentSHA256         string
	ResultID, ResultDocumentSHA256, CheckReportID, CheckDocumentSHA256 string
	CommitKey, CommitPayloadHash                                       string
	DependencyLocks                                                    []generated.DependencyLock
	ProofStamp                                                         [32]byte
}
type SnapshotSceneFact struct {
	ResourceKind, ResourceID                         string
	OriginalResourceHash, CurrentResourceHash        string
	OriginalSceneRef                                 generated.DefinitionRef
	OwnerReleaseID, SourceReleaseID                  string
	CanonicalSceneRef                                generated.DefinitionRef
	SceneObjectID, SceneVersion, SceneDefinitionHash string
	SceneIdentityHash, Visibility                    string
	PublicationIDs                                   []string
}
type SnapshotReadFacts struct {
	Scope          domain.Scope
	WorldRevision  uint64
	Initialization json.RawMessage
	Actors, Items  []SnapshotRecord
	Scenes         []SnapshotSceneFact
	Publications   []SnapshotPublicationFact
}
type IntegrationSnapshotPlan struct {
	store            *Store
	owner            DeliverySnapshotOwner
	read             *IntegrationRead
	planning, closed bool
	facts            SnapshotPlanFacts
}
type IntegrationSnapshotUnit struct {
	store  *Store
	plan   *IntegrationSnapshotPlan
	tx     *sql.Tx
	owner  DeliverySnapshotOwner
	active bool
	facts  SnapshotReadFacts
}
type deliveryReadState struct {
	record                         sessionRecord
	eligible                       sessionEligibility
	choice                         *integrationChoice
	scope                          domain.Scope
	versions                       domain.Versions
	subjectPrivacy, subjectView, h uint64
	status                         string
	initial                        []byte
	catalog                        []withdrawalPublication
	keys                           []LockKey
}
type IntegrationRead struct {
	store      *Store
	ctx        context.Context
	principal  currentIntegration
	purpose    string
	state      deliveryReadState
	plan       *IntegrationSnapshotPlan
	tx         *sql.Tx
	unit       *IntegrationSnapshotUnit
	release    func()
	closed     bool
	facts      SnapshotReadFacts
	projection IntegrationSnapshotProjection
	now        time.Time
	cursorKey  [32]byte
	page       *deliveryPagePlan
	finished   bool
}

func (s *Store) BindDeliverySnapshotOwner(owner DeliverySnapshotOwner) error {
	if owner == nil || reflect.ValueOf(owner).Kind() != reflect.Pointer || reflect.ValueOf(owner).IsNil() || owner.DeliverySnapshotStore() != s || s.integrationInitialOwner == nil || s.integrationMaterialOwner == nil {
		return ErrDenied
	}
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if s.integrationSnapshotOwner != nil {
		return ErrDenied
	}
	s.integrationSnapshotOwner = owner
	return nil
}
func (s *Store) CheckDeliverySnapshotOwner(owner DeliverySnapshotOwner) error {
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if owner == nil || s.integrationSnapshotOwner != owner || owner.DeliverySnapshotStore() != s || s.integrationClosed.Load() {
		return ErrDenied
	}
	return nil
}
func (p *IntegrationSnapshotPlan) Facts(owner DeliverySnapshotOwner) (SnapshotPlanFacts, error) {
	if p == nil || p.closed || !p.planning || p.owner != owner || p.store.CheckDeliverySnapshotOwner(owner) != nil {
		return SnapshotPlanFacts{}, ErrDenied
	}
	return creationCopy(p.facts), nil
}
func (s *Store) CheckIntegrationSnapshotUnit(ctx context.Context, u *IntegrationSnapshotUnit, owner DeliverySnapshotOwner) error {
	if u == nil || u.store != s || u.tx == nil || !u.active || u.owner != owner || u.plan == nil || u.plan.closed || u.plan.owner != owner || u.plan.read == nil || u.plan.read.closed || u.plan.read.tx != u.tx || u.plan.read.unit != u || ctx != u.plan.read.ctx {
		return ErrDenied
	}
	if e := s.CheckDeliverySnapshotOwner(owner); e != nil {
		return e
	}
	return s.checkDeliveryAdmission(ctx)
}
func (u *IntegrationSnapshotUnit) Facts(ctx context.Context, owner DeliverySnapshotOwner) (SnapshotReadFacts, error) {
	if u == nil || u.store == nil {
		return SnapshotReadFacts{}, ErrDenied
	}
	if e := u.store.CheckIntegrationSnapshotUnit(ctx, u, owner); e != nil {
		return SnapshotReadFacts{}, e
	}
	return creationCopy(u.facts), nil
}
func deliveryRawHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Planning and final verification use the same current CLIENT and original
// issuance checks. A READ action never borrows TURN or a model actor identity.
func (s *Store) inspectDeliveryRead(ctx context.Context, q withdrawalQuerier, v currentIntegration, world string) (deliveryReadState, error) {
	var out deliveryReadState
	if v.store != s || v.principal.Kind != domain.IntegrationClient || domain.ValidateID(world) != nil {
		return out, ErrDenied
	}
	rec, elig, e := s.currentClient(ctx, q, v)
	if e != nil || rec.World != world {
		return out, ErrDenied
	}
	allowed := false
	for _, a := range rec.Actions {
		allowed = allowed || a == "READ"
	}
	if !allowed {
		return out, ErrDenied
	}
	out.record = rec
	out.eligible = elig
	out.scope = domain.Scope{ProductScope: domain.ProductScope{TenantID: rec.Tenant, ProductID: rec.Product, SubjectID: rec.Player}, WorldInstanceID: world}
	service, e := s.integrationServiceProof(ctx, q, rec.Service)
	if e != nil {
		return out, e
	}
	choiceOwner := &integrationTurnPlan{store: s, principal: v, session: rec, eligibility: elig, service: service, scope: out.scope}
	out.choice, e = choiceOwner.inspectCurrentChoice(ctx, q, nil)
	if e != nil {
		return out, e
	}
	choiceOwner.choice = out.choice
	out.keys = append(consentKeys(rec, elig, ""), choiceOwner.choiceLocks()...)
	var permitted bool
	e = q.QueryRowContext(ctx, `SELECT g.privacy_epoch,g.view_epoch,EXISTS(SELECT 1 FROM memory_subject_permissions p WHERE p.tenant_id=g.tenant_id AND p.product_id=g.product_id AND p.caller_subject_id=g.subject_id AND p.data_subject_id=g.subject_id AND p.action='memory.private_body.read' AND p.allowed_use='PRIVATE_RESPONSE' AND p.enabled=1 AND p.expires_at>UTC_TIMESTAMP(6)) FROM subject_governance g WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=? AND g.enabled=1`, rec.Tenant, rec.Product, rec.Player).Scan(&out.subjectPrivacy, &out.subjectView, &permitted)
	if e != nil || !permitted {
		return out, ErrDenied
	}
	e = q.QueryRowContext(ctx, `SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,i.initialization,d.update_seq FROM world_controls w JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) JOIN delivery_streams d USING(tenant_id,product_id,world_instance_id) WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? AND i.owner_subject_id=? AND i.mode='PRIVATE'`, rec.Tenant, rec.Product, world, rec.ServiceSubject).Scan(&out.versions.ReleaseID, &out.versions.WorldRevision, &out.versions.ControlVersion, &out.versions.PrivacyEpoch, &out.versions.ViewEpoch, &out.status, &out.initial, &out.h)
	if e != nil {
		return out, e
	}
	queue := []string{out.versions.ReleaseID}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		if len(seen) >= 200 {
			return out, ErrDenied
		}
		seen[id] = true
		// The publisher is the real original subject, not the current player.
		var raw []byte
		var rel generated.Release
		if e = q.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", rec.Tenant, rec.Product, id).Scan(&raw); e != nil {
			return out, e
		}
		if generated.Decode("Release", raw, &rel) != nil {
			return out, ErrDenied
		}
		ps := out.scope.ProductScope
		ps.SubjectID = string(rel.PublishedBySubjectID)
		pub, live, e := readWithdrawalPublication(ctx, q, ps, id)
		if e != nil || !live {
			return out, ErrDenied
		}
		out.catalog = append(out.catalog, pub)
		out.keys = append(out.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{rec.Tenant, rec.Product, id}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{rec.Tenant, rec.Product, pub.result}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{rec.Tenant, rec.Product, string(pub.release.CheckReportID)}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{pub.result}})
		for _, dep := range pub.release.DependencyLocks {
			queue = append(queue, string(dep.ReleaseID))
		}
	}
	return out, nil
}

func deliveryPlanFacts(state deliveryReadState) (SnapshotPlanFacts, error) {
	out := SnapshotPlanFacts{Scope: state.scope, WorldRevision: state.versions.WorldRevision}
	var init struct {
		Actors, Items          []struct{ ID string }
		ReleaseID, ReleaseHash string
	}
	if json.Unmarshal(state.initial, &init) != nil || len(init.Actors) > 32 || len(init.Items) > 32 || init.ReleaseID != state.versions.ReleaseID || len(state.catalog) == 0 || init.ReleaseHash != state.catalog[0].release.ContentHash {
		return out, ErrDenied
	}
	seen := map[string]bool{}
	for _, a := range init.Actors {
		if domain.ValidateID(a.ID) != nil || seen["a/"+a.ID] {
			return out, ErrDenied
		}
		seen["a/"+a.ID] = true
		out.ActorIDs = append(out.ActorIDs, a.ID)
	}
	for _, a := range init.Items {
		if domain.ValidateID(a.ID) != nil || seen["i/"+a.ID] {
			return out, ErrDenied
		}
		seen["i/"+a.ID] = true
		out.ItemIDs = append(out.ItemIDs, a.ID)
	}
	for _, p := range state.catalog {
		out.ReleaseIDs = append(out.ReleaseIDs, string(p.release.ReleaseID))
	}
	sort.Strings(out.ActorIDs)
	sort.Strings(out.ItemIDs)
	sort.Strings(out.ReleaseIDs)
	return out, nil
}

// This closed read acquisition adds only the original resource/check rows to
// the existing total order. No caller callback or query escapes the Store.
func (s *Store) deliveryReadTx(ctx context.Context, keys []LockKey) (*sql.Tx, error) {
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
		case "events", "approved_results":
			e = studioLockRows(ctx, tx, "SELECT record_id FROM "+k.Kind+" WHERE tenant_id=? AND product_id=? AND record_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "integration_receipt":
			e = studioLockRows(ctx, tx, "SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
		case "delivery_media":
			e = studioLockRows(ctx, tx, "SELECT asset_id FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "integration_participant":
			e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "commit_receipts":
			e = studioLockRows(ctx, tx, "SELECT commit_key FROM commit_receipts WHERE commit_key=? FOR UPDATE", k.IDs[0])
		case "content_check":
			e = studioLockRows(ctx, tx, "SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT report_id FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		case "actor", "item":
			e = studioLockRows(ctx, tx, "SELECT "+k.Kind+"_id FROM "+k.Kind+"s WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND "+k.Kind+"_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
		default:
			if k.Rank == 40 {
				for _, table := range []string{"world_controls", "runtime_instances", "delivery_streams"} {
					e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
					if e != nil {
						break
					}
				}
			} else if k.Rank == 50 {
				for _, table := range []string{"tasks", "runtime_tasks", "harness_turns", "harness_turn_commits"} {
					e = studioLockRows(ctx, tx, "SELECT task_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
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

func (s *Store) BeginIntegrationRead(ctx context.Context, world, purpose string) (*IntegrationRead, error) {
	return s.beginIntegrationRead(ctx, world, purpose, "", 50, "")
}

// Page parameters affect precollection; they never select an authorization scope.
func (s *Store) BeginIntegrationPage(ctx context.Context, world, cursor string, limit uint16) (*IntegrationRead, error) {
	return s.beginIntegrationRead(ctx, world, "EVENTS", cursor, limit, "")
}
func (s *Store) BeginIntegrationPacket(ctx context.Context, world, task string) (*IntegrationRead, error) {
	if domain.ValidateID(task) != nil {
		return nil, ErrDenied
	}
	return s.beginIntegrationRead(ctx, world, "PACKET", "", 1, task)
}
func (s *Store) beginIntegrationRead(ctx context.Context, world, purpose, cursor string, limit uint16, packetTask string) (out *IntegrationRead, err error) {
	phase := "purpose/admission"
	defer func() {
		if err != nil {
			err = fmt.Errorf("delivery read %s: %w", phase, err)
		}
	}()
	if purpose != "SNAPSHOT" && purpose != "EVENTS" && purpose != "PACKET" {
		return nil, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		cancel()
		return nil, e
	}
	r := &IntegrationRead{store: s, ctx: ctx, purpose: purpose, release: func() { done(); cancel() }}
	good := false
	defer func() {
		if !good {
			_ = r.Close()
		}
	}()
	if e = s.checkDeliveryAdmission(ctx); e != nil {
		return nil, e
	}
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok {
		return nil, ErrDenied
	}
	r.principal = v
	phase = "current planning"
	state, e := s.inspectDeliveryRead(ctx, s.DB, v, world)
	if e != nil {
		return nil, e
	}
	r.state = state
	if e = r.loadCursorKey(); e != nil {
		return nil, e
	}
	s.integrationOwnerMu.Lock()
	owner := s.integrationSnapshotOwner
	s.integrationOwnerMu.Unlock()
	phase = "bound owner"
	if s.CheckDeliverySnapshotOwner(owner) != nil {
		return nil, ErrDenied
	}
	phase = "plan facts"
	facts, e := deliveryPlanFacts(state)
	if e != nil {
		return nil, e
	}
	r.plan = &IntegrationSnapshotPlan{store: s, owner: owner, read: r, planning: true, facts: facts}
	phase = "owner plan"
	extra, e := owner.PlanIntegrationSnapshot(ctx, r.plan)
	r.plan.planning = false
	if e != nil {
		return nil, e
	}
	// The owner can request exactly the resources in the Store's frozen inventory.
	expected := []LockKey{}
	for _, id := range facts.ActorIDs {
		expected = append(expected, LockKey{Rank: 90, Kind: "actor", IDs: []string{state.scope.TenantID, state.scope.ProductID, world, id}})
	}
	for _, id := range facts.ItemIDs {
		expected = append(expected, LockKey{Rank: 90, Kind: "item", IDs: []string{state.scope.TenantID, state.scope.ProductID, world, id}})
	}
	want, _ := PlanLocks(expected)
	got, e := PlanLocks(extra)
	if e != nil || !reflect.DeepEqual(want, got) {
		return nil, ErrDenied
	}
	r.state.keys = append(r.state.keys, extra...)
	if purpose == "EVENTS" || packetTask != "" {
		phase = "page planning"
		if e = s.DB.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&r.now); e != nil {
			return nil, e
		}
		if e = r.planPage(cursor, limit, packetTask); e != nil {
			return nil, e
		}
	}

	phase = "read locks"
	r.tx, e = s.deliveryReadTx(ctx, r.state.keys)
	if e != nil {
		return nil, e
	}
	phase = "current transaction"
	current, e := s.inspectDeliveryRead(ctx, r.tx, v, world)
	if e != nil {
		return nil, e
	}
	current.keys = r.state.keys
	if !reflect.DeepEqual(current, r.state) {
		return nil, ErrReplan
	}
	if e = r.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&r.now); e != nil {
		return nil, e
	}
	if r.page != nil {
		if e = r.verifyPage(); e != nil {
			return nil, e
		}
	}
	phase = "scene facts"
	r.facts, e = s.snapshotSceneFactsTx(ctx, r.tx, r.state)
	if e != nil {
		return nil, e
	}
	r.unit = &IntegrationSnapshotUnit{store: s, plan: r.plan, tx: r.tx, owner: owner, active: true, facts: r.facts}
	phase = "owner projection"
	r.projection, e = owner.SnapshotTx(ctx, r.unit)
	r.unit.active = false
	if e != nil {
		return nil, e
	}
	good = true
	return r, nil
}
func (r *IntegrationRead) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	clear(r.cursorKey[:])
	if r.plan != nil {
		r.plan.closed = true
	}
	if r.unit != nil {
		r.unit.active = false
	}
	var e error
	if r.tx != nil {
		e = r.tx.Rollback()
		if errors.Is(e, sql.ErrTxDone) {
			e = nil
		}
		r.tx = nil
	}
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return e
}
func (r *IntegrationRead) Finish(ctx context.Context) error {
	if r == nil || r.closed || r.tx == nil || !reflect.DeepEqual(ctx.Value(integrationContextKey{}), r.principal) {
		return ErrDenied
	}
	defer r.Close()
	if r.store.BeforeReadFinish != nil {
		if e := r.store.BeforeReadFinish(r.ctx, r.tx); e != nil {
			return e
		}
	}
	if e := r.store.checkDeliveryAdmission(r.ctx); e != nil {
		return e
	}
	current, e := r.store.inspectDeliveryRead(r.ctx, r.tx, r.principal, r.state.scope.WorldInstanceID)
	if e != nil {
		return e
	}
	current.keys = r.state.keys
	if !reflect.DeepEqual(current, r.state) {
		return ErrReplan
	}
	facts, e := r.store.snapshotSceneFactsTx(r.ctx, r.tx, current)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(facts, r.facts) {
		return ErrReplan
	}
	if r.page != nil {
		if e = r.verifyPage(); e != nil {
			return e
		}
	}
	r.finished = true
	return nil
}
func (r *IntegrationRead) Snapshot() (generated.Snapshot, error) {
	if r == nil || r.closed || r.purpose != "SNAPSHOT" {
		return generated.Snapshot{}, ErrDenied
	}
	cursor, e := r.initialCursor(r.state.h)
	if e != nil {
		return generated.Snapshot{}, e
	}
	v := r.state.versions
	return generated.Snapshot{Cursor: cursor, Scope: turnScope(r.state.scope), Versions: generated.Versions{ReleaseID: v.ReleaseID, WorldRevision: strconv.FormatUint(v.WorldRevision, 10), ControlVersion: strconv.FormatUint(v.ControlVersion, 10), PrivacyEpoch: strconv.FormatUint(v.PrivacyEpoch, 10), ViewEpoch: strconv.FormatUint(v.ViewEpoch, 10)}, InstanceState: r.state.status, State: creationCopy(r.projection.State), Actors: creationCopy(r.projection.Actors), Relationships: creationCopy(r.projection.Relationships)}, nil
}

// Qualification derives from the original namespace graph and real published
// source proof in this transaction. Visibility is never a caller boolean.
func (s *Store) snapshotSceneFactsTx(ctx context.Context, tx *sql.Tx, state deliveryReadState) (SnapshotReadFacts, error) {
	out := SnapshotReadFacts{Scope: state.scope, WorldRevision: state.versions.WorldRevision, Initialization: append([]byte(nil), state.initial...), Actors: []SnapshotRecord{}, Items: []SnapshotRecord{}, Scenes: []SnapshotSceneFact{}, Publications: []SnapshotPublicationFact{}}
	var init struct {
		ReleaseID, ReleaseHash string
		Actors, Items          []json.RawMessage
	}
	if json.Unmarshal(state.initial, &init) != nil {
		return out, ErrDenied
	}
	catalog := map[string]generated.Release{}
	allowed := map[string]bool{}
	for _, p := range state.catalog {
		id := string(p.release.ReleaseID)
		catalog[id] = p.release
		proof, e := withdrawalHash(struct {
			Draft                                                                   string
			Revision                                                                uint64
			Hash, Document, Result, Check                                           []byte
			ReceiptKey, Tenant, Product, Owner, Operation, Command, Phase, Instance string
			ReceiptHash                                                             []byte
			Committed                                                               time.Time
		}{p.draft, p.revision, p.hash, p.document, p.resultDocument, p.checkDocument, p.receipt.key, p.receipt.tenant, p.receipt.product, p.receipt.owner, p.receipt.operation, p.receipt.command, p.receipt.phase, p.instance, p.receipt.hash, p.committed})
		if e != nil {
			return out, e
		}
		out.Publications = append(out.Publications, SnapshotPublicationFact{ReleaseID: id, PublisherSubjectID: string(p.release.PublishedBySubjectID), ContentHash: p.release.ContentHash, DocumentSHA256: deliveryRawHash(p.document), ResultID: p.result, ResultDocumentSHA256: deliveryRawHash(p.resultDocument), CheckReportID: string(p.release.CheckReportID), CheckDocumentSHA256: deliveryRawHash(p.checkDocument), CommitKey: p.receipt.key, CommitPayloadHash: hex.EncodeToString(p.receipt.hash), DependencyLocks: creationCopy(p.release.DependencyLocks), ProofStamp: proof})
	}
	root, ok := catalog[init.ReleaseID]
	if !ok || root.ContentHash != init.ReleaseHash {
		return out, ErrDenied
	}
	for _, dep := range root.DependencyLocks {
		ref, e := json.Marshal(map[string]string{"kind": "PINNED_DEFINITION", "release_id": string(dep.ReleaseID), "object_id": string(dep.ObjectID), "version": string(dep.Version), "content_hash": dep.ContentHash})
		if e != nil {
			return out, e
		}
		h, e := withdrawalHex(json.RawMessage(ref))
		if e != nil {
			return out, e
		}
		allowed[h] = true
	}
	scene := func(ref generated.DefinitionRef, owner string) (SnapshotSceneFact, error) {
		f := SnapshotSceneFact{OriginalSceneRef: append([]byte(nil), ref...), OwnerReleaseID: owner}
		var r struct {
			Kind      string `json:"kind"`
			ObjectID  string `json:"object_id"`
			ReleaseID string `json:"release_id"`
			Version   string `json:"version"`
			Hash      string `json:"content_hash"`
		}
		if generated.Validate("DefinitionRef", ref) != nil || json.Unmarshal(ref, &r) != nil {
			return f, ErrDenied
		}
		switch r.Kind {
		case "PINNED_DEFINITION":
			h, e := withdrawalHex(ref)
			if e != nil || !allowed[h] {
				return f, ErrDenied
			}
			owner = r.ReleaseID
		case "LOCAL_DRAFT":
		default:
			return f, ErrDenied
		}
		rel, ok := catalog[owner]
		if !ok {
			return f, ErrDenied
		}
		found := 0
		for _, body := range rel.Input.Objects {
			var head struct {
				ID      string `json:"object_id"`
				Version string `json:"version"`
			}
			if json.Unmarshal(body, &head) != nil {
				return f, ErrDenied
			}
			if head.ID != r.ObjectID {
				continue
			}
			found++
			var definition generated.SceneDefinition
			if generated.Decode("SceneDefinition", body, &definition) != nil {
				return f, ErrDenied
			}
			hash, e := withdrawalHex(json.RawMessage(body))
			if e != nil {
				return f, e
			}
			if r.Kind == "PINNED_DEFINITION" && (head.Version != r.Version || hash != r.Hash) {
				return f, ErrDenied
			}
			canonical := map[string]string{"kind": "LOCAL_DRAFT", "object_id": r.ObjectID}
			if owner != init.ReleaseID {
				canonical = map[string]string{"kind": "PINNED_DEFINITION", "release_id": owner, "object_id": r.ObjectID, "version": head.Version, "content_hash": hash}
			}
			b, e := json.Marshal(canonical)
			if e != nil {
				return f, e
			}
			f.CanonicalSceneRef = b
			f.SourceReleaseID = owner
			f.SceneObjectID = head.ID
			f.SceneVersion = head.Version
			f.SceneDefinitionHash = hash
			f.Visibility = definition.Visibility
			f.SceneIdentityHash, e = withdrawalHex(struct {
				Release string
				Ref     generated.DefinitionRef
			}{owner, f.CanonicalSceneRef})
			if e != nil {
				return f, e
			}
			f.PublicationIDs = []string{init.ReleaseID}
			if owner != init.ReleaseID {
				f.PublicationIDs = append(f.PublicationIDs, owner)
			}
		}
		if found != 1 {
			return f, ErrDenied
		}
		return f, nil
	}
	for _, kind := range []string{"ACTOR", "ITEM"} {
		originals := init.Actors
		table, column := "actors", "actor_id"
		if kind == "ITEM" {
			originals = init.Items
			table, column = "items", "item_id"
		}
		for _, original := range originals {
			var o map[string]json.RawMessage
			if json.Unmarshal(original, &o) != nil {
				return out, ErrDenied
			}
			var id string
			if json.Unmarshal(o["ID"], &id) != nil || domain.ValidateID(id) != nil {
				return out, ErrDenied
			}
			var raw []byte
			if e := tx.QueryRowContext(ctx, "SELECT document FROM "+table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND "+column+"=?", state.scope.TenantID, state.scope.ProductID, state.scope.WorldInstanceID, id).Scan(&raw); e != nil {
				return out, e
			}
			var cur map[string]json.RawMessage
			if json.Unmarshal(raw, &cur) != nil {
				return out, ErrDenied
			}
			stable := creationCopy(cur)
			if kind == "ITEM" {
				for _, key := range []string{"State", "HolderID", "Note"} {
					stable[key] = o[key]
				}
			}
			a, e := withdrawalHex(stable)
			if e != nil {
				return out, e
			}
			b, e := withdrawalHex(o)
			if e != nil || a != b {
				return out, ErrConflict
			}
			var nested struct {
				SceneRef generated.DefinitionRef `json:"scene_ref"`
			}
			owner := init.ReleaseID
			if kind == "ACTOR" {
				if json.Unmarshal(cur["OriginalSetup"], &nested) != nil {
					return out, ErrDenied
				}
			} else {
				if json.Unmarshal(cur["Definition"], &nested) != nil || json.Unmarshal(cur["OriginalReleaseID"], &owner) != nil {
					return out, ErrDenied
				}
			}
			f, e := scene(nested.SceneRef, owner)
			if e != nil {
				return out, e
			}
			f.ResourceKind = kind
			f.ResourceID = id
			f.OriginalResourceHash = b
			f.CurrentResourceHash, e = withdrawalHex(cur)
			if e != nil {
				return out, e
			}
			out.Scenes = append(out.Scenes, f)
			record := SnapshotRecord{ID: id, Body: append([]byte(nil), raw...)}
			if kind == "ACTOR" {
				out.Actors = append(out.Actors, record)
			} else {
				out.Items = append(out.Items, record)
			}
		}
	}
	return out, nil
}

// Packet references are filtered candidates. Only the original approved reader
// can supply body, followed by a second whole-page current/stamp verification.
type DeliveryPacketReference struct {
	DeliveryID, TaskID, PacketID       string
	Revision                           uint64
	EventIDs                           []string
	Kind, MediaState, MediaID, AssetID string
	Available                          bool
}
type deliveryPageRow struct {
	Seq                           uint64
	ID, Kind, Result, Task, State string
	Packet                        sql.NullString
	Revision                      uint64
	Events, Audience, Lineage     []string
}
type deliveryRowProof struct {
	Receipt        receiptRow
	Source         []byte
	Time           time.Time
	Revision       uint64
	Events         []deliveryEventProof
	OriginalEvents []deliveryEventProof
}

// Original event identity and raw evidence stay private. Only the verified kind,
// revision and original COMMIT time are projected; Actions never leave this proof.
type deliveryEventProof struct {
	ID, Tenant, Product, Owner, Task, World, Status, Kind string
	Revision                                              uint64
	Raw                                                   []byte
}
type deliveryPagePlan struct {
	Task   string
	Cursor string
	Limit  uint16
	Record deliveryCursorRecord
	Rows   []deliveryPageRow
	Proofs map[string]deliveryRowProof
	Plans  map[string]*integrationTurnPlan
	Media  map[string]mediaRecord
}
type deliveryRows interface {
	withdrawalQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (r *IntegrationRead) readPageRows(q deliveryRows, position uint64) ([]deliveryPageRow, error) {
	s := r.state.scope
	query := `SELECT update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND update_seq>? AND update_seq<=?`
	args := []any{s.TenantID, s.ProductID, s.WorldInstanceID, position, r.state.h}
	if r.page.Task != "" {
		query += " AND task_ref=?"
		args = append(args, r.page.Task)
	}
	order := " ORDER BY update_seq LIMIT 100"
	if r.page.Task != "" {
		order = " ORDER BY update_seq DESC LIMIT 1"
	}
	rows, e := q.QueryContext(r.ctx, query+order, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []deliveryPageRow{}
	count := uint64(0)
	last := position
	for rows.Next() {
		var v deliveryPageRow
		var ev, au, li []byte
		if e = rows.Scan(&v.Seq, &v.ID, &v.Kind, &v.Result, &v.Task, &ev, &v.Packet, &v.State, &v.Revision, &au, &li); e != nil {
			return nil, e
		}
		if json.Unmarshal(ev, &v.Events) != nil || json.Unmarshal(au, &v.Audience) != nil || json.Unmarshal(li, &v.Lineage) != nil {
			return nil, ErrDenied
		}
		// EVENTS covers the complete committed stream. A task-scoped PACKET
		// query is intentionally a subset and may skip other tasks' sequences.
		if r.page.Task == "" && (v.Seq <= last || v.Seq-last != 1) {
			return nil, ErrDenied
		}
		last = v.Seq
		out = append(out, v)
		if r.rowVisible(v) {
			n := uint64(len(v.Events))
			if v.Packet.Valid {
				n++
			}
			if len(out) == 1 && r.page.Record.ItemOffset > 0 {
				if r.page.Record.ItemOffset >= n {
					return nil, ErrDenied
				}
				n -= r.page.Record.ItemOffset
			}
			count += n
			if count >= uint64(r.page.Limit) {
				break
			}
		}
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	// Only an exhausted query proves EOF. At an output/scan bound the next
	// page checks continuity again; below either bound a missing tail is denial.
	if r.page.Task == "" && count < uint64(r.page.Limit) && len(out) < 100 && last != r.state.h {
		return nil, ErrDenied
	}
	return out, nil
}
func (r *IntegrationRead) rowVisible(v deliveryPageRow) bool {
	own := false
	for _, id := range v.Audience {
		own = own || id == r.state.record.Player
	}
	if !own {
		return false
	}
	known := map[string]bool{r.state.scope.WorldInstanceID: true}
	for _, p := range r.state.catalog {
		known[string(p.release.ReleaseID)] = true
		known[string(p.release.ContentHash)] = true
	}
	for _, ref := range v.Lineage {
		if !known[ref] {
			return false
		}
	}
	return true
}
func (r *IntegrationRead) rowProof(q withdrawalQuerier, v deliveryPageRow, a *integrationTurnPlan) (deliveryRowProof, error) {
	var out deliveryRowProof
	if v.Kind == "MEDIA" {
		return r.mediaRowProof(q, v, a)
	}
	ctx := r.ctx
	if a.control != nil {
		ctx = context.WithValue(ctx, integrationContextKey{}, a.principal)
	}
	if e := a.currentTaskRead(ctx, q); e != nil {
		return out, e
	}
	s := r.state.scope
	if v.ID != turnIdentity(s.TenantID, s.ProductID, s.WorldInstanceID, v.Kind, v.Result, "1") || v.Result != turnIdentity(v.Task, "result") || v.State != "COMMITTED" || v.Revision != 1 || !reflect.DeepEqual(v.Audience, []string{r.state.record.Player}) {
		return out, ErrDenied
	}
	key, op, command := v.Result, "INSTANCE_CONTROL", v.Task
	if v.Kind == "TURN" {
		key = turnIdentity(v.Task, "world-commit")
		op = "HARNESS_TURN_COMMIT"
		command = key
	} else if v.Kind != "CONTROL" {
		return out, ErrDenied
	}
	receipt, e := queryReceipt(r.ctx, q, key)
	if e != nil {
		return out, e
	}
	if receipt.tenant != s.TenantID || receipt.product != s.ProductID || receipt.owner != a.scope.SubjectID || receipt.kind != "WORLD" || !receipt.world.Valid || receipt.world.String != s.WorldInstanceID || receipt.task != v.Task || receipt.phase != "COMMIT" || receipt.operation != op || receipt.command != command || len(receipt.hash) != 32 {
		return out, ErrDenied
	}
	out.Receipt = receipt
	if e = q.QueryRowContext(r.ctx, "SELECT committed_at FROM commit_receipts WHERE commit_key=?", key).Scan(&out.Time); e != nil {
		return out, e
	}
	var valid bool
	if e = q.QueryRowContext(r.ctx, "SELECT status='COMMITTED',revision FROM approved_results WHERE tenant_id=? AND product_id=? AND record_id=? AND task_id=? AND owner_subject_id=?", s.TenantID, s.ProductID, v.Result, v.Task, a.scope.SubjectID).Scan(&valid, &out.Revision); e != nil || !valid {
		return out, ErrDenied
	}
	if v.Kind == "CONTROL" {
		if a.control == nil || v.Packet.Valid || !reflect.DeepEqual(v.Events, []string{v.Result}) || !reflect.DeepEqual(v.Lineage, []string{s.WorldInstanceID}) {
			return out, ErrDenied
		}
		h := sha256.Sum256(a.control.raw)
		if !reflect.DeepEqual(receipt.hash, h[:]) {
			return out, ErrDenied
		}
		if e = q.QueryRowContext(r.ctx, "SELECT document FROM runtime_events WHERE tenant_id=? AND product_id=? AND record_id=?", s.TenantID, s.ProductID, v.Result).Scan(&out.Source); e != nil {
			return out, e
		}
		var ev struct{ Kind, Action string }
		if json.Unmarshal(out.Source, &ev) != nil || ev.Kind != "CONTROL" || ev.Action != a.control.document.Action {
			return out, ErrDenied
		}
	} else {
		if a.control != nil || !v.Packet.Valid || v.Packet.String != v.Result {
			return out, ErrDenied
		}
		if e = q.QueryRowContext(r.ctx, "SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND result_id=? AND receipt_key=?", s.TenantID, s.ProductID, v.Task, v.Result, key).Scan(&out.Source); e != nil {
			return out, e
		}
		var c domain.CandidateCommit
		if json.Unmarshal(out.Source, &c) != nil || c.ResultID != v.Result || c.ReceiptKey != key || !reflect.DeepEqual(c.EventIDs, v.Events) || !reflect.DeepEqual(v.Lineage, []string{c.Versions.ReleaseID}) {
			return out, ErrDenied
		}
		out.Revision = c.Versions.WorldRevision
	}
	seen := map[string]bool{}
	for _, id := range v.Events {
		if seen[id] {
			return out, ErrDenied
		}
		seen[id] = true
		var ev deliveryEventProof
		if e = q.QueryRowContext(r.ctx, `SELECT e.record_id,e.tenant_id,e.product_id,e.owner_subject_id,e.task_id,e.world_instance_id,e.status,e.revision,r.document FROM events e JOIN runtime_events r USING(tenant_id,product_id,record_id) WHERE e.tenant_id=? AND e.product_id=? AND e.record_id=?`, s.TenantID, s.ProductID, id).Scan(&ev.ID, &ev.Tenant, &ev.Product, &ev.Owner, &ev.Task, &ev.World, &ev.Status, &ev.Revision, &ev.Raw); e != nil {
			return out, e
		}
		var kind struct{ Kind string }
		want := "CONTROL"
		if v.Kind == "TURN" {
			want = "ACTION_BUNDLE"
		}
		if json.Unmarshal(ev.Raw, &kind) != nil || kind.Kind != want || ev.ID != id || ev.Tenant != s.TenantID || ev.Product != s.ProductID || ev.Owner != a.scope.SubjectID || ev.Task != v.Task || ev.World != s.WorldInstanceID || ev.Status != "COMMITTED" || ev.Revision != out.Revision {
			return out, ErrDenied
		}
		ev.Kind = kind.Kind
		out.Events = append(out.Events, ev)
	}
	return out, nil
}
func (r *IntegrationRead) planPage(cursor string, limit uint16, task string) error {
	if limit < 1 || limit > 100 {
		return ErrDenied
	}
	p := &deliveryPagePlan{Task: task, Cursor: cursor, Limit: limit, Proofs: map[string]deliveryRowProof{}, Plans: map[string]*integrationTurnPlan{}, Media: map[string]mediaRecord{}}
	r.page = p
	if cursor == "" {
		p.Record = deliveryCursorRecord{Version: 2, Binding: r.cursorBinding(), Issued: r.now, Expires: r.now.Add(15 * time.Minute)}
	} else {
		v, e := r.openCursor(cursor)
		if e != nil {
			return e
		}
		p.Record = v
	}
	rows, e := r.readPageRows(r.store.DB, p.Record.Position)
	if e != nil {
		return e
	}
	p.Rows = rows
	if p.Record.ItemOffset > 0 && (len(rows) == 0 || rows[0].ID != p.Record.PartialUpdateID || !r.rowVisible(rows[0])) {
		return ErrDenied
	}
	for _, v := range rows {
		if !r.rowVisible(v) {
			continue
		}
		ctx := r.ctx
		if v.Kind == "CONTROL" {
			// CONTROL may originate from the current player or its real SERVICE.
			// Resolve the persisted scoped owner before selecting the existing
			// sealed read proof; a PLAYER task must never borrow SERVICE identity.
			var owner string
			scope := r.state.scope
			if e := r.store.DB.QueryRowContext(ctx, "SELECT owner_subject_id FROM tasks WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND task_id=? AND task_kind='CONTROL'", scope.TenantID, scope.ProductID, scope.WorldInstanceID, v.Task).Scan(&owner); e != nil {
				return e
			}
			if owner != r.state.record.Player {
				proof, e := r.store.integrationServiceProof(ctx, r.store.DB, r.state.record.Service)
				if e != nil || owner != proof.principal.StableSubject {
					return ErrDenied
				}
				ctx = context.WithValue(ctx, integrationContextKey{}, proof)
			}
		}
		a, e := r.store.integrationTaskPlan(ctx, r.store.DB, v.Task)
		if e != nil {
			return e
		}
		if a.scope.WorldInstanceID != r.state.scope.WorldInstanceID {
			return ErrDenied
		}
		p.Plans[v.ID] = a
		proof, e := r.rowProof(r.store.DB, v, a)
		if e != nil {
			return e
		}
		p.Proofs[v.ID] = proof
		if v.Kind == "MEDIA" {
			asset := turnIdentity(r.state.scope.TenantID, r.state.scope.ProductID, r.state.scope.WorldInstanceID, v.Result, "AUDIO", "asset")
			m, e := readMedia(r.ctx, r.store.DB, &mediaSource{scope: r.state.scope, asset: asset})
			if e != nil {
				return e
			}
			if e = r.validateCurrentMedia(v, m); e != nil {
				return e
			}
			p.Media[v.ID] = m
			r.state.keys = append(r.state.keys, LockKey{Rank: 90, Kind: "delivery_media", IDs: []string{r.state.scope.TenantID, r.state.scope.ProductID, asset}})
		}
		r.state.keys = append(r.state.keys, a.keys...)
		r.state.keys = append(r.state.keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{proof.Receipt.key}}, LockKey{Rank: 110, Kind: "approved_results", IDs: []string{a.scope.TenantID, a.scope.ProductID, v.Result}}, LockKey{Rank: 110, Kind: "events", IDs: []string{a.scope.TenantID, a.scope.ProductID, v.Result}})
		for _, ev := range proof.Events {
			r.state.keys = append(r.state.keys, LockKey{Rank: 110, Kind: "events", IDs: []string{a.scope.TenantID, a.scope.ProductID, ev.ID}})
		}
		for _, ev := range proof.OriginalEvents {
			r.state.keys = append(r.state.keys, LockKey{Rank: 110, Kind: "events", IDs: []string{a.scope.TenantID, a.scope.ProductID, ev.ID}})
		}
	}
	if p.Record.ItemOffset > 0 && deliveryManifest(rows[0], p.Proofs[rows[0].ID]) != p.Record.PartialManifestHash {
		return ErrDenied
	}
	return nil
}
func (r *IntegrationRead) verifyPage() error {
	rows, e := r.readPageRows(r.tx, r.page.Record.Position)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(rows, r.page.Rows) {
		return ErrReplan
	}
	if r.page.Cursor != "" {
		if _, e = r.openCursor(r.page.Cursor); e != nil {
			return e
		}
	}
	for _, v := range rows {
		if !r.rowVisible(v) {
			continue
		}
		a := r.page.Plans[v.ID]
		if a == nil {
			return ErrReplan
		}
		proof, e := r.rowProof(r.tx, v, a)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(proof, r.page.Proofs[v.ID]) {
			return ErrReplan
		}
		if v.Kind == "MEDIA" {
			old := r.page.Media[v.ID]
			m, e := readMedia(r.ctx, r.tx, &mediaSource{scope: r.state.scope, asset: old.Asset})
			if e != nil || !reflect.DeepEqual(m, old) || r.validateCurrentMedia(v, m) != nil {
				return ErrReplan
			}
		}
	}
	return nil
}

// The digest covers the complete ordered manifest and original private proof.
// It excludes independently arriving media, never the approved core body hash
// (which is retained in the original CandidateCommit Source bytes).
func deliveryManifest(v deliveryPageRow, p deliveryRowProof) string {
	c := p.Receipt
	fields := []any{v, p.Source, p.Events, p.Time, p.Revision, []string{c.key, c.tenant, c.product, c.owner, c.kind, c.task, c.operation, c.command, c.phase, c.world.String}, c.world.Valid, c.nullTask.Valid, c.hash}
	if v.Kind == "MEDIA" {
		fields = append(fields, p.OriginalEvents)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// Selection retains ordinals in the original manifest. Position advances only
// after all items of a row are processed; hidden whole rows still count as scan
// progress. This pure selection is shared by events and packet file IO.
func (r *IntegrationRead) pageSelection() (deliveryCursorRecord, []generated.VisibleEvent, []DeliveryPacketReference, error) {
	record := r.page.Record
	record.HighWater = r.state.h
	events := []generated.VisibleEvent{}
	packets := []DeliveryPacketReference{}
	count := 0
	for i, v := range r.page.Rows {
		if count == int(r.page.Limit) {
			break
		}
		start := uint64(0)
		if i == 0 {
			start = record.ItemOffset
		}
		if !r.rowVisible(v) {
			if start != 0 {
				return record, nil, nil, ErrDenied
			}
			record.Position = v.Seq
			continue
		}
		proof, ok := r.page.Proofs[v.ID]
		if !ok {
			return record, nil, nil, ErrDenied
		}
		total := uint64(len(proof.Events))
		if v.Packet.Valid {
			total++
		}
		hash := deliveryManifest(v, proof)
		if hash == "" || (start > 0 && (start >= total || record.PartialUpdateID != v.ID || record.PartialManifestHash != hash)) {
			return record, nil, nil, ErrDenied
		}
		offset := start
		for offset < total && count < int(r.page.Limit) {
			if offset < uint64(len(proof.Events)) {
				original := proof.Events[offset]
				ev := generated.VisibleEvent{EventID: original.ID, EventKind: original.Kind, WorldRevision: strconv.FormatUint(original.Revision, 10), OccurredAt: proof.Time.UTC().Format(time.RFC3339Nano), Payload: map[string]json.RawMessage{}}
				if v.Kind == "TURN" {
					task := v.Task
					ev.TurnID = &task
				}
				events = append(events, ev)
			} else {
				packets = append(packets, r.packetReference(v))
			}
			offset++
			count++
		}
		if offset == total {
			record.Position = v.Seq
			record.ItemOffset = 0
			record.PartialUpdateID = ""
			record.PartialManifestHash = ""
		} else {
			record.ItemOffset = offset
			record.PartialUpdateID = v.ID
			record.PartialManifestHash = hash
			break
		}
	}
	return record, events, packets, nil
}
func (r *IntegrationRead) EventPage(cursor string, limit uint16) (generated.IntegrationEventPage, error) {
	var out generated.IntegrationEventPage
	if r == nil || r.closed || r.page == nil || r.purpose != "EVENTS" || r.page.Cursor != cursor || r.page.Limit != limit {
		return out, ErrDenied
	}
	record, events, _, e := r.pageSelection()
	if e != nil {
		return out, e
	}
	out = generated.IntegrationEventPage{Scope: turnScope(r.state.scope), ViewEpoch: strconv.FormatUint(r.state.versions.ViewEpoch, 10), Events: events, Deliveries: []generated.IntegrationDeliveryUpdate{}}
	// The page that reaches the tail must publish its progress. Only the next
	// truly empty, nonpartial read can echo exact input bytes at EOF.
	if len(r.page.Rows) == 0 && record.ItemOffset == 0 && cursor != "" {
		out.NextCursor = cursor
		return out, nil
	}
	out.NextCursor, e = r.sealCursor(record)
	return out, e
}
func (r *IntegrationRead) PacketReferences() ([]DeliveryPacketReference, error) {
	if r == nil || r.closed || r.page == nil {
		return nil, ErrDenied
	}
	if r.purpose == "EVENTS" {
		_, _, refs, e := r.pageSelection()
		return refs, e
	}
	out := []DeliveryPacketReference{}
	for _, v := range r.page.Rows {
		if r.rowVisible(v) && (v.Kind == "TURN" || v.Kind == "MEDIA") {
			out = append(out, r.packetReference(v))
		}
	}
	return out, nil
}

// A previous stage must have genuinely finished; its private immutable facts
// remain data for comparison and cannot reopen its transaction or capabilities.
func (r *IntegrationRead) MatchFinished(previous *IntegrationRead) error {
	if r == nil || r.closed || previous == nil || !previous.closed || !previous.finished || r.store != previous.store || r.purpose != previous.purpose || !reflect.DeepEqual(r.principal, previous.principal) || !reflect.DeepEqual(r.state, previous.state) || !reflect.DeepEqual(r.facts, previous.facts) {
		return ErrReplan
	}
	if (r.page == nil) != (previous.page == nil) {
		return ErrReplan
	}
	if r.page != nil && (r.page.Task != previous.page.Task || r.page.Cursor != previous.page.Cursor || r.page.Limit != previous.page.Limit || !reflect.DeepEqual(r.page.Rows, previous.page.Rows) || !reflect.DeepEqual(r.page.Proofs, previous.page.Proofs) || !reflect.DeepEqual(r.page.Media, previous.page.Media)) {
		return ErrReplan
	}
	return nil
}
