package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"reflect"
	"sort"
	"sync/atomic"
	"time"
)

// IntegrationInitialOwner is installed by the application, never selected by a
// request. Its input is a Store-issued creation seal bound to this exact unit.
type IntegrationInitialOwner interface {
	InspectIntegrationInitialSourcesTx(context.Context, Tx, *IntegrationCreationPlan) (*IntegrationInitialLineage, error)
}

// IntegrationCreationOwner is fixed once by app construction. No request can
// supply a callback, SQL executor, replacement owner, or additional lock keys.
type IntegrationCreationOwner interface {
	IntegrationCreationLocks(*IntegrationCreationPlan) ([]LockKey, error)
	MaterializeIntegrationCreation(context.Context, Tx, *IntegrationCreationPlan) (generated.Instance, error)
	ValidateIntegrationCreation(context.Context, Tx, *IntegrationCreationPlan) error
}

func (s *Store) BindIntegrationCreationOwners(initial IntegrationInitialOwner, material IntegrationCreationOwner) error {
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if initial == nil || material == nil || s.integrationInitialOwner != nil || s.integrationMaterialOwner != nil || s.integrationClosed.Load() || s.Guard == nil {
		return ErrDenied
	}
	_, release, e := s.EnterOperation(context.Background())
	if e != nil {
		return e
	}
	defer release()
	s.integrationInitialOwner = initial
	s.integrationMaterialOwner = material
	return nil
}

type IntegrationSourceOrigin struct {
	Release, Object, Version, Hash, Pointer string
	Rights                                  generated.SourceRights
	Audience                                string
	SourceRefs                              []string
	AssetAbsent                             bool
}
type IntegrationInitialSource struct {
	Actor, SourceID, Acquisition, BodyHash, Kind string
	Origins                                      []IntegrationSourceOrigin
}
type IntegrationInitialLineage struct {
	Policy, Tenant, Product, World, Customer, Player, CreationReceipt string
	Sources                                                           []IntegrationInitialSource
}
type creationPublication struct {
	Release                generated.Release
	Document               []byte
	Result, Command, Owner string
}
type creationEntitlement struct {
	ID, Player, Release, World, Actions, Policy, Receipt string
	Revision                                             uint64
	Expiry                                               time.Time
	Eligible                                             bool
}
type creationIdentityState struct {
	Gates        GateVersions
	Installation string
	Generation   uint64
}
type integrationCreationState struct {
	started                          atomic.Bool
	store                            *Store
	principal                        currentIntegration
	request                          generated.CreateInstanceRequest
	key, id                          string
	hash                             [32]byte
	identity                         creationIdentityState
	mapping                          *registeredPlayerSnapshot
	entitlement                      creationEntitlement
	entitlementOwner, entitlementKey string
	catalog                          []creationPublication
	keys                             []LockKey
	replay                           bool
	originalAtEntry                  bool
	unit                             *unit
	phase                            string
	lineage                          *IntegrationInitialLineage
	materialized                     []byte
}
type IntegrationCreationPlan struct{ state *integrationCreationState }

func creationCopy[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func (p *IntegrationCreationPlan) Scope() domain.Scope {
	if p == nil || p.state == nil {
		return domain.Scope{}
	}
	v := p.state
	return domain.Scope{ProductScope: domain.ProductScope{TenantID: v.principal.principal.Tenant, ProductID: v.principal.principal.Product, SubjectID: v.principal.principal.StableSubject}, WorldInstanceID: v.id}
}
func (p *IntegrationCreationPlan) PlayerScope() domain.Scope {
	s := p.Scope()
	if p != nil && p.state != nil {
		s.SubjectID = p.state.entitlement.Player
	}
	return s
}
func (p *IntegrationCreationPlan) WorldID() string { return p.Scope().WorldInstanceID }
func (p *IntegrationCreationPlan) Release() generated.Release {
	if p == nil || p.state == nil || len(p.state.catalog) == 0 {
		return generated.Release{}
	}
	return creationCopy(p.state.catalog[0].Release)
}
func (p *IntegrationCreationPlan) Dependencies() []generated.Release {
	out := []generated.Release{}
	if p != nil && p.state != nil && len(p.state.catalog) > 0 {
		for _, v := range p.state.catalog[1:] {
			out = append(out, creationCopy(v.Release))
		}
	}
	return out
}
func (p *IntegrationCreationPlan) InitialLineage() *IntegrationInitialLineage {
	if p == nil || p.state == nil {
		return nil
	}
	return creationCopy(p.state.lineage)
}
func (p *IntegrationCreationPlan) CustomerID() string {
	if p == nil || p.state == nil {
		return ""
	}
	return p.state.principal.principal.Customer
}
func creationID(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) currentCreationIdentity(ctx context.Context, q rowQuerier, v currentIntegration) (creationIdentityState, error) {
	var out creationIdentityState
	actual, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || actual.store != s || v.store != s || !reflect.DeepEqual(actual, v) || v.principal.Kind != domain.IntegrationService || ctx.Value(operatorContextKey{}) != nil {
		return out, ErrDenied
	}
	p := v.principal
	var live bool
	e := q.QueryRowContext(ctx, `SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,i.installation_id,i.generation,
 (k.kind='SERVICE' AND k.revoked=0 AND k.expires_at>UTC_TIMESTAMP(6) AND k.token_hash=? AND k.tenant_id=? AND k.product_id=? AND k.customer_id=? AND k.subject_id=? AND c.service_subject_id=k.subject_id AND c.enabled=1 AND c.expires_at>UTC_TIMESTAMP(6) AND t.enabled=1 AND p.enabled=1 AND g.enabled=1 AND w.applied_watermark>=w.required_watermark AND (w.fresh_synthetic=1 OR w.required_watermark>0))
 FROM integration_principals k JOIN integration_credential_hashes h ON h.token_hash=k.token_hash AND h.kind=k.kind AND h.principal_id=k.principal_id JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id JOIN integration_customers c USING(tenant_id,product_id,customer_id) JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN subject_governance g ON g.tenant_id=k.tenant_id AND g.product_id=k.product_id AND g.subject_id=k.subject_id JOIN governance_watermark w ON w.singleton=1 WHERE k.principal_id=?`, v.hash[:], p.Tenant, p.Product, p.Customer, p.StableSubject, p.PrincipalID).Scan(&out.Gates.Tenant, &out.Gates.Product, &out.Gates.Subject, &out.Gates.Privacy, &out.Gates.View, &out.Installation, &out.Generation, &live)
	if e != nil || !live {
		return out, ErrDenied
	}
	return out, nil
}

type creationRows interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func creationCandidates(ctx context.Context, q creationRows, p domain.IntegrationPrincipal, player, release, world string) ([]creationEntitlement, error) {
	rows, e := q.QueryContext(ctx, `SELECT entitlement_id,player_subject_id,release_id,COALESCE(world_instance_id,''),CAST(allowed_actions AS CHAR),self_consent_eligible,eligibility_policy,expires_at,revision,registry_receipt FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND player_subject_id=? AND release_id=? AND enabled=1 AND expires_at>UTC_TIMESTAMP(6) AND ((?='' AND world_instance_id IS NULL) OR world_instance_id=?) ORDER BY entitlement_id`, p.Tenant, p.Product, p.Customer, player, release, world, world)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []creationEntitlement{}
	for rows.Next() {
		var v creationEntitlement
		if e = rows.Scan(&v.ID, &v.Player, &v.Release, &v.World, &v.Actions, &v.Eligible, &v.Policy, &v.Expiry, &v.Revision, &v.Receipt); e != nil {
			return nil, e
		}
		out = append(out, v)
		if len(out) > 200 {
			return nil, ErrDenied
		}
	}
	return out, rows.Err()
}
func (s *Store) PrepareIntegrationCreation(ctx context.Context, key string, request generated.CreateInstanceRequest) (*IntegrationCreationPlan, error) {
	var release func()
	var enterErr error
	ctx, release, enterErr = s.EnterOperation(ctx)
	if enterErr != nil {
		return nil, enterErr
	}
	defer release()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	raw, e := json.Marshal(request)
	if !ok || s.Guard == nil || s.integrationClosed.Load() || s.integrationInitialOwner == nil || s.integrationMaterialOwner == nil || domain.ValidateIdempotencyKey(key) != nil || e != nil || generated.Validate("CreateInstanceRequest", raw) != nil || request.Mode != "PRIVATE" || len(request.InitialParticipants) != 1 || request.ProductID != v.principal.Product {
		return nil, ErrDenied
	}
	a := &integrationCreationState{store: s, principal: v, request: creationCopy(request), key: key}
	a.identity, e = s.currentCreationIdentity(ctx, s.DB, v)
	if e != nil {
		return nil, e
	}
	p := v.principal
	a.id = creationID(p.Tenant, p.Product, p.StableSubject, "createInstance", key)
	plan := &IntegrationCreationPlan{a}
	scope := plan.Scope()
	payload, e := json.Marshal(struct {
		Scope             domain.ProductScope
		Operation, Target string
		Input             any
	}{scope.ProductScope, "createInstance", a.id, a.request})
	if e != nil {
		return nil, e
	}
	payload, e = wire.Canonical(payload)
	if e != nil {
		return nil, e
	}
	a.hash = sha256.Sum256(payload)
	var receiptHash []byte
	e = s.DB.QueryRowContext(ctx, "SELECT payload_hash FROM commit_receipts WHERE commit_key=? AND tenant_id=? AND product_id=? AND owner_subject_id=? AND operation='createInstance' AND phase='COMMIT' AND task_id IS NULL", a.id, p.Tenant, p.Product, p.StableSubject).Scan(&receiptHash)
	if e == nil {
		if !bytes.Equal(receiptHash, a.hash[:]) {
			return nil, ErrConflict
		}
		a.replay = true
	} else if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var external []byte
	e = s.DB.QueryRowContext(ctx, "SELECT external_id FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND player_subject_id=?", p.Tenant, p.Product, p.Customer, request.InitialParticipants[0]).Scan(&external)
	if e != nil {
		return nil, ErrDenied
	}
	w := ""
	if a.replay {
		w = a.id
	}
	candidates, e := creationCandidates(ctx, s.DB, p, request.InitialParticipants[0], request.ReleaseID, w)
	if e != nil {
		return nil, e
	}
	if len(candidates) == 0 {
		return nil, ErrDenied
	}
	if len(candidates) != 1 {
		return nil, ErrConflict
	}
	a.entitlement = candidates[0]
	var actions []string
	if json.Unmarshal([]byte(a.entitlement.Actions), &actions) != nil {
		return nil, ErrDenied
	}
	read := false
	for _, action := range actions {
		read = read || action == "READ"
	}
	if !read || !a.entitlement.Eligible || a.entitlement.Policy != "SELF_PRIVATE_R0" {
		return nil, ErrDenied
	}
	op := domain.OperatorPrincipal{ProductScope: scope.ProductScope}
	a.mapping, e = readRegisteredPlayer(ctx, s.DB, op, domain.RegisterPlayerAccess{CustomerID: p.Customer, ExternalIDBytes: external, PlayerSubjectID: a.entitlement.Player, ExpiresAt: a.entitlement.Expiry, Actions: []string{"READ"}})
	if e != nil || a.mapping == nil {
		return nil, ErrDenied
	}
	a.keys = []LockKey{{Rank: 15, Kind: "integration_principal", IDs: []string{p.PrincipalID}}, {Rank: 25, Kind: "integration_entitlement", IDs: []string{p.Tenant, p.Product, p.Customer, a.entitlement.ID}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, a.entitlement.Player}}, {Rank: 35, Kind: "integration_customer", IDs: []string{p.Tenant, p.Product, p.Customer}}, {Rank: 35, Kind: "integration_mapping", IDs: []string{p.Tenant, p.Product, p.Customer, creationExternalHash(external)}}, {Rank: 35, Kind: "integration_participant", IDs: []string{p.Tenant, p.Product, a.id}}, {Rank: 90, Kind: "content_check", IDs: []string{p.Tenant, p.Product, a.id}}}
	a.keys = append(a.keys, a.mapping.receiptKeys(op)...)
	er, eo, ek, e := registeredReceipt(ctx, s.DB, op, a.entitlement.Receipt, "REGISTER_PLAYER_ACCESS", a.entitlement.Player)
	if e != nil || !a.registrationMatches(er) {
		return nil, ErrDenied
	}
	a.entitlementOwner, a.entitlementKey = eo, ek
	a.keys = append(a.keys, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.Tenant, p.Product, eo, ek}})
	queue := []string{request.ReleaseID}
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
		a.catalog = append(a.catalog, pub)
		a.keys = append(a.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{p.Tenant, p.Product, id}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{p.Tenant, p.Product, pub.Result}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{pub.Result}})
		for _, dep := range pub.Release.DependencyLocks {
			queue = append(queue, string(dep.ReleaseID))
		}
	}
	var rootKind string
	for _, raw := range a.catalog[0].Release.Input.Objects {
		var h struct {
			Kind string `json:"kind"`
			ID   string `json:"object_id"`
		}
		if json.Unmarshal(raw, &h) != nil {
			return nil, ErrDenied
		}
		if h.ID == string(a.catalog[0].Release.Input.RootObjectID) {
			rootKind = h.Kind
		}
	}
	if rootKind != "WORLD" {
		return nil, ErrDenied
	}
	return plan, nil
}
func creationExternalHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Store) creationPublication(ctx context.Context, scope domain.ProductScope, id string) (creationPublication, error) {
	var out creationPublication
	var live bool
	if e := s.DB.QueryRowContext(ctx, "SELECT c.document,r.available FROM content_releases c JOIN releases r USING(tenant_id,product_id,release_id) WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=?", scope.TenantID, scope.ProductID, id).Scan(&out.Document, &live); e != nil || !live {
		return out, ErrDenied
	}
	if generated.Decode("Release", out.Document, &out.Release) != nil || out.Release.Scope.TenantID != scope.TenantID || out.Release.Scope.ProductID != scope.ProductID || string(out.Release.ReleaseID) != id {
		return out, ErrDenied
	}
	rows, e := s.DB.QueryContext(ctx, `SELECT a.result_id,c.command_key,c.owner_subject_id FROM admin_results a JOIN commit_receipts c ON c.commit_key=a.result_id AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.owner_subject_id=a.owner_subject_id WHERE a.tenant_id=? AND a.product_id=? AND a.schema_name='Release' AND c.operation='publishContentDraft' AND c.phase='COMMIT' AND c.task_id IS NULL AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.release_id'))=?`, scope.TenantID, scope.ProductID, id)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n > 1 {
			return out, ErrDenied
		}
		if e = rows.Scan(&out.Result, &out.Command, &out.Owner); e != nil {
			return out, e
		}
	}
	if rows.Err() != nil || n != 1 {
		return out, ErrDenied
	}
	if out.Result != creationID(scope.TenantID, scope.ProductID, out.Owner, "publishContentDraft", out.Command) || id != creationID(scope.TenantID, scope.ProductID, out.Owner, "publish", out.Command) || out.Owner != string(out.Release.PublishedBySubjectID) {
		return out, ErrDenied
	}
	return out, nil
}

// CheckIntegrationCreationTx proves an exact live unit/Tx. Getters and caller
// metadata cannot construct this seal, even if all visible IDs are copied.
func (s *Store) CheckIntegrationCreationTx(ctx context.Context, tx Tx, p *IntegrationCreationPlan) error {
	u, ok := tx.(*unit)
	if !ok || p == nil || p.state == nil {
		return ErrDenied
	}
	a := p.state
	if s.integrationClosed.Load() || a.store != s || a.unit != u || u.operatorStore != s || u.integrationCreation != a || u.tx == nil || u.operation != "createInstance" || !u.commandOnly || u.runtime == nil || u.runtime.CreationID != a.id || u.scope != p.Scope().ProductScope || a.phase != "PRE_BODY" && a.phase != "MATERIALIZED_BEFORE_RECEIPT" && a.phase != "PRE_COMMIT" {
		return ErrDenied
	}
	_, e := s.currentCreationIdentity(ctx, u.tx, a.principal)
	return e
}
func (s *Store) checkIntegrationCreationTx(ctx context.Context, u *unit, a *integrationCreationState, phase string) error {
	p := &IntegrationCreationPlan{a}
	a.phase = phase
	if phase == "ORIGINAL_RESULT" {
		if u == nil || a.store != s {
			return ErrDenied
		}
	} else if e := s.CheckIntegrationCreationTx(ctx, u, p); e != nil {
		return e
	}
	current, e := s.currentCreationIdentity(ctx, u.tx, a.principal)
	if e != nil {
		return e
	}
	if current != a.identity {
		return ErrDenied
	}
	// Classify only this sealed CREATE, after its complete lock set is held.
	// A second preplanned request may now have its own committed result; this
	// never changes the frozen selected entitlement or retries another family.
	if phase == "PRE_BODY" {
		if e = u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{a.id}}); e != nil {
			return e
		}
		receipt, err := queryReceipt(ctx, u.tx, a.id)
		if err == nil {
			request := BarrierRequest{ProductScope: p.Scope().ProductScope, OwnerSubjectID: p.Scope().SubjectID, Operation: "createInstance", CommandKey: a.key, CommitKey: a.id, PayloadHash: a.hash, commandOnly: true}
			if receipt.key != a.id || receipt.nullTask.Valid || !receipt.matches(request) {
				return ErrConflict
			}
			a.originalAtEntry = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if a.replay {
			return ErrDenied
		}
	}
	who := a.principal.principal
	op := domain.OperatorPrincipal{ProductScope: p.Scope().ProductScope}
	mapping, e := readRegisteredPlayer(ctx, u.tx, op, domain.RegisterPlayerAccess{CustomerID: who.Customer, ExternalIDBytes: []byte(a.mapping.External), PlayerSubjectID: a.entitlement.Player, ExpiresAt: a.entitlement.Expiry, Actions: []string{"READ"}})
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(mapping, a.mapping) {
		return ErrDenied
	}
	er, eo, ek, e := registeredReceipt(ctx, u.tx, op, a.entitlement.Receipt, "REGISTER_PLAYER_ACCESS", a.entitlement.Player)
	if e != nil || eo != a.entitlementOwner || ek != a.entitlementKey || !a.registrationMatches(er) {
		return ErrDenied
	}
	world := ""
	if a.replay || a.originalAtEntry || phase != "PRE_BODY" {
		world = a.id
	}
	rows, e := creationCandidates(ctx, u.tx, who, a.entitlement.Player, a.entitlement.Release, world)
	if e != nil {
		return e
	}
	if len(rows) != 1 {
		return ErrConflict
	}
	expected := a.entitlement
	if !a.replay && (a.originalAtEntry || phase != "PRE_BODY") {
		if expected.Revision == ^uint64(0) {
			return ErrDenied
		}
		expected.Revision++
		expected.World = a.id
	}
	if !reflect.DeepEqual(rows[0], expected) {
		return ErrDenied
	}
	for _, pub := range a.catalog {
		if e = s.checkCreationPublicationTx(ctx, u, pub); e != nil {
			return e
		}
	}
	if phase == "PRE_BODY" && !a.replay && !a.originalAtEntry {
		var absent bool
		e = u.tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?) AND NOT EXISTS(SELECT 1 FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=?) AND NOT EXISTS(SELECT 1 FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=?) AND NOT EXISTS(SELECT 1 FROM delivery_streams WHERE tenant_id=? AND product_id=? AND world_instance_id=?) AND NOT EXISTS(SELECT 1 FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=?)`, who.Tenant, who.Product, a.id, who.Tenant, who.Product, a.id, who.Tenant, who.Product, a.id, who.Tenant, who.Product, a.id, who.Tenant, who.Product, a.id).Scan(&absent)
		if e != nil {
			return e
		}
		if !absent {
			return ErrConflict
		}
		return nil
	}
	if phase == "PRE_COMMIT" {
		lineage, e := s.integrationInitialOwner.InspectIntegrationInitialSourcesTx(ctx, u, p)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(lineage, a.lineage) {
			return ErrDenied
		}
		if e = s.integrationMaterialOwner.ValidateIntegrationCreation(ctx, u, p); e != nil {
			return e
		}
	}
	if phase == "PRE_COMMIT" || phase == "ORIGINAL_RESULT" || a.replay || a.originalAtEntry {
		return s.checkIntegrationCreationRowsTx(ctx, u, a, phase == "PRE_COMMIT")
	}
	return nil
}
func (s *Store) checkCreationPublicationTx(ctx context.Context, u *unit, pub creationPublication) error {
	p := u.scope
	id := string(pub.Release.ReleaseID)
	for _, k := range []LockKey{{Rank: 90, Kind: "content_release", IDs: []string{p.TenantID, p.ProductID, id}}, {Rank: 90, Kind: "content_check", IDs: []string{p.TenantID, p.ProductID, pub.Result}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{pub.Result}}} {
		if e := u.RequireLock(k); e != nil {
			return e
		}
	}
	var raw, result, hash []byte
	var live bool
	var owner, command string
	e := u.tx.QueryRowContext(ctx, `SELECT c.document,c.content_hash,r.available,a.document,b.owner_subject_id,b.command_key FROM content_releases c JOIN releases r USING(tenant_id,product_id,release_id) JOIN admin_results a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.result_id=? AND a.schema_name='Release' JOIN commit_receipts b ON b.commit_key=a.result_id AND b.tenant_id=a.tenant_id AND b.product_id=a.product_id AND b.owner_subject_id=a.owner_subject_id AND b.operation='publishContentDraft' AND b.phase='COMMIT' AND b.task_id IS NULL WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=?`, pub.Result, p.TenantID, p.ProductID, id).Scan(&raw, &hash, &live, &result, &owner, &command)
	if e != nil || !live || !bytes.Equal(raw, pub.Document) || !bytes.Equal(result, pub.Document) || owner != pub.Owner || command != pub.Command || hex.EncodeToString(hash) != pub.Release.ContentHash {
		return ErrDenied
	}
	return nil
}

func (s *Store) RunIntegrationCreation(ctx context.Context, p *IntegrationCreationPlan) (generated.Instance, error) {
	var zero generated.Instance
	if p == nil || p.state == nil || p.state.store != s || s.integrationClosed.Load() || s.integrationInitialOwner == nil || s.integrationMaterialOwner == nil {
		return zero, ErrDenied
	}
	a := p.state
	if !a.started.CompareAndSwap(false, true) {
		return zero, ErrDenied
	}
	defer func() { a.phase = "CLOSED"; a.unit = nil }()
	locks, e := s.integrationMaterialOwner.IntegrationCreationLocks(p)
	if e != nil {
		return zero, e
	}
	keys := append(append([]LockKey{}, a.keys...), locks...)
	r := BarrierRequest{integrationCreation: a, ProductScope: p.Scope().ProductScope, OwnerSubjectID: p.Scope().SubjectID, Operation: "createInstance", CommandKey: a.key, CommitKey: a.id, PayloadHash: a.hash, GateVersions: a.identity.Gates, Runtime: &RuntimeBinding{CreationID: a.id, Action: "CREATE"}, commandOnly: true, Locks: keys}
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		lineage, e := s.integrationInitialOwner.InspectIntegrationInitialSourcesTx(ctx, tx, p)
		if e != nil {
			return e
		}
		a.lineage = creationCopy(lineage)
		if a.entitlement.Revision == ^uint64(0) {
			return ErrDenied
		}
		who := a.principal.principal
		changed, e := u.tx.ExecContext(ctx, `UPDATE integration_entitlements SET world_instance_id=?,revision=revision+1 WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=? AND player_subject_id=? AND release_id=? AND world_instance_id IS NULL AND revision=? AND enabled=1 AND expires_at>UTC_TIMESTAMP(6) AND expires_at=?`, a.id, who.Tenant, who.Product, who.Customer, a.entitlement.ID, a.entitlement.Player, a.entitlement.Release, a.entitlement.Revision, a.entitlement.Expiry)
		if e != nil {
			return e
		}
		if e = registryAffected(changed); e != nil {
			return e
		}
		out, e := s.integrationMaterialOwner.MaterializeIntegrationCreation(ctx, tx, p)
		if e != nil {
			return e
		}
		if out != a.instance() {
			return ErrDenied
		}
		a.phase = "MATERIALIZED_BEFORE_RECEIPT"
		if e = s.writeIntegrationCreationRowsTx(ctx, u.tx, u, a); e != nil {
			return e
		}
		raw, e := json.Marshal(out)
		if e != nil || generated.Validate("Instance", raw) != nil {
			return ErrDenied
		}
		if e = u.RequireLock(LockKey{Rank: 90, Kind: "content_check", IDs: []string{who.Tenant, who.Product, a.id}}); e != nil {
			return e
		}
		_, e = u.ExecContext(ctx, "INSERT INTO admin_results(tenant_id,product_id,result_id,owner_subject_id,schema_name,document) VALUES(?,?,?,?,'Instance',?)", who.Tenant, who.Product, a.id, who.StableSubject, raw)
		return e
	})
	if e != nil {
		return zero, e
	}
	return s.readIntegrationCreationResult(ctx, a, r)
}
func (a *integrationCreationState) instance() generated.Instance {
	return generated.Instance{Scope: generated.WorldScope{TenantID: a.principal.principal.Tenant, ProductID: a.principal.principal.Product, WorldInstanceID: a.id}, Mode: "PRIVATE", InstanceState: "CREATED", Versions: generated.Versions{ReleaseID: a.request.ReleaseID, WorldRevision: "0", ControlVersion: "0", PrivacyEpoch: "1", ViewEpoch: "1"}}
}
func (s *Store) writeIntegrationCreationRowsTx(ctx context.Context, tx *sql.Tx, u *unit, a *integrationCreationState) error {
	if a.phase != "MATERIALIZED_BEFORE_RECEIPT" || u.tx != tx || s.CheckIntegrationCreationTx(ctx, u, &IntegrationCreationPlan{a}) != nil {
		return ErrDenied
	}
	p := a.principal.principal
	for _, k := range []LockKey{{Rank: 35, Kind: "integration_participant", IDs: []string{p.Tenant, p.Product, a.id}}, {Rank: 40, IDs: []string{p.Tenant, p.Product, a.id}}} {
		if e := u.RequireLock(k); e != nil {
			return e
		}
	}
	if e := snapshotCreationMaterial(ctx, u, a); e != nil {
		return e
	}
	r, e := tx.ExecContext(ctx, `INSERT INTO integration_world_participants(tenant_id,product_id,world_instance_id,customer_id,service_owner_id,player_subject_id,release_id,creation_receipt,entitlement_id,external_id,enabled,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1,1)`, p.Tenant, p.Product, a.id, p.Customer, p.StableSubject, a.entitlement.Player, a.request.ReleaseID, a.id, a.entitlement.ID, []byte(a.mapping.External))
	if e != nil {
		return e
	}
	if e = registryAffected(r); e != nil {
		return e
	}
	r, e = tx.ExecContext(ctx, "INSERT INTO delivery_streams(tenant_id,product_id,world_instance_id,update_seq,initialization_receipt,projection_version) VALUES(?,?,?,0,?,1)", p.Tenant, p.Product, a.id, a.id)
	if e != nil {
		return e
	}
	return registryAffected(r)
}
func snapshotCreationMaterial(ctx context.Context, u *unit, a *integrationCreationState) error {
	var raw []byte
	var owner, mode, release string
	p := a.principal.principal
	e := u.tx.QueryRowContext(ctx, "SELECT owner_subject_id,mode,initial_release_id,initialization FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=?", p.Tenant, p.Product, a.id).Scan(&owner, &mode, &release, &raw)
	if e != nil || owner != p.StableSubject || mode != "PRIVATE" || release != a.request.ReleaseID {
		return ErrDenied
	}
	var init struct {
		ReleaseID, ReleaseHash, CreationCommandID string
		Participants                              []string
		IntegrationInitialLineage                 *IntegrationInitialLineage
	}
	if json.Unmarshal(raw, &init) != nil || init.ReleaseID != release || init.ReleaseHash != a.catalog[0].Release.ContentHash || init.CreationCommandID != a.id || !reflect.DeepEqual(init.Participants, a.request.InitialParticipants) || !reflect.DeepEqual(init.IntegrationInitialLineage, a.lineage) {
		return ErrDenied
	}
	a.materialized = append([]byte{}, raw...)
	return nil
}
func (s *Store) checkIntegrationCreationRowsTx(ctx context.Context, u *unit, a *integrationCreationState, fresh bool) error {
	p := a.principal.principal
	var good bool
	var raw []byte
	e := u.tx.QueryRowContext(ctx, `SELECT (w.customer_id=? AND w.service_owner_id=? AND w.player_subject_id=? AND w.release_id=? AND w.creation_receipt=? AND w.entitlement_id=? AND w.external_id=? AND w.enabled=1 AND w.revision=1 AND d.initialization_receipt=? AND d.projection_version=1 AND i.owner_subject_id=? AND i.mode='PRIVATE'),i.initialization FROM integration_world_participants w JOIN delivery_streams d USING(tenant_id,product_id,world_instance_id) JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=?`, p.Customer, p.StableSubject, a.entitlement.Player, a.request.ReleaseID, a.id, a.entitlement.ID, []byte(a.mapping.External), a.id, p.StableSubject, p.Tenant, p.Product, a.id).Scan(&good, &raw)
	if e != nil || !good {
		return ErrDenied
	}
	if fresh && !bytes.Equal(raw, a.materialized) {
		return ErrDenied
	}
	return nil
}
func (s *Store) readIntegrationCreationResult(ctx context.Context, a *integrationCreationState, r BarrierRequest) (generated.Instance, error) {
	var out generated.Instance
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	keys, e := PlanLocks(append(baseLocks(r), r.Locks...))
	if e != nil {
		return out, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	u := &unit{tx: tx, scope: r.ProductScope, operatorStore: s, operation: "createInstance", commandOnly: true, runtime: r.Runtime, integrationCreation: a, plan: map[string]bool{}}
	for _, k := range keys {
		if k.Rank == 45 {
			if e = marker(ctx, tx, k); e != nil {
				return out, e
			}
		} else if e = s.acquire(ctx, tx, r, k); e != nil {
			return out, e
		}
		u.plan[string(lockBytes(k))] = true
	}
	a.unit = u
	if e = s.checkIntegrationCreationTx(ctx, u, a, "ORIGINAL_RESULT"); e != nil {
		return out, e
	}
	receipt, e := queryReceipt(ctx, tx, a.id)
	if e != nil || !receipt.matches(r) {
		return out, ErrDenied
	}
	var raw []byte
	e = tx.QueryRowContext(ctx, "SELECT document FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? AND owner_subject_id=? AND schema_name='Instance'", r.ProductScope.TenantID, r.ProductScope.ProductID, a.id, r.OwnerSubjectID).Scan(&raw)
	if e != nil {
		return out, e
	}
	if generated.Decode("Instance", raw, &out) != nil || out != a.instance() {
		return out, ErrDenied
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = s.checkIntegrationCreationTx(ctx, u, a, "ORIGINAL_RESULT"); e != nil {
		return out, e
	}
	return out, nil
}

// Only low-rank keys actually frozen by this Store-issued creation may use
// integration row locks. Existing Operator/Studio dispatch is unchanged.
func (s *Store) acquireIntegrationCreationKey(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) (bool, error) {
	a := r.integrationCreation
	if a == nil {
		return false, nil
	}
	if a.store != s {
		return true, ErrDenied
	}
	if k.Rank != 15 && k.Rank != 25 && k.Rank != 35 && !(k.Rank == 110 && k.Kind == "integration_receipt") {
		return false, nil
	}
	found := false
	for _, want := range a.keys {
		if reflect.DeepEqual(k, want) {
			found = true
			break
		}
	}
	if !found {
		return true, ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return true, e
	}
	if k.Kind == "integration_participant" {
		return true, studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
	}
	return true, lockStudioRows(ctx, tx, k)
}

// Reconstruct the original closed registration hash from the full qualified
// tuple. A matching expiry or an opaque receipt reference alone is insufficient.
func (a *integrationCreationState) registrationMatches(r domain.RegistryReceipt) bool {
	x := r.RegistrationExpiry
	if x == nil || x.EntitlementExpiresAt == nil || x.MappingExpiresAt == nil || !x.EntitlementExpiresAt.Equal(a.entitlement.Expiry) || !x.MappingExpiresAt.Equal(a.mapping.MappingExpiry) || !x.CustomerExpiresAt.Equal(a.mapping.CustomerExpiry) {
		return false
	}
	var actions []string
	if json.Unmarshal([]byte(a.entitlement.Actions), &actions) != nil {
		return false
	}
	c := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: a.principal.principal.Product, TargetID: a.entitlement.Player, Player: &domain.RegisterPlayerAccess{CustomerID: a.principal.principal.Customer, ExternalIDBytes: []byte(a.mapping.External), PlayerSubjectID: a.entitlement.Player, EntitlementID: a.entitlement.ID, ReleaseID: a.entitlement.Release, Actions: actions, ExpiresAt: a.entitlement.Expiry}}
	return r.Revision == 1 && registryHistoricalRegistrationMatches(c, [32]byte{}, r.Hash)
}

// Kept deterministic for audit and exact lock-set comparisons.
func creationSortedKeys(keys []LockKey) []string {
	out := []string{}
	for _, k := range keys {
		out = append(out, string(lockBytes(k)))
	}
	sort.Strings(out)
	return out
}
