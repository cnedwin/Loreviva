package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"sync"
	"time"
)

const MaxNodes = 1000
const MaxSources = 1000
const MaxAcquisitions = 2000
const MaxGrants = 2000
const MaxReferences = 64
const MaxAggregateBytes = 16 << 20

var ErrInvalid = errors.New("memory scope, evidence, or bounds invalid")

type Namespace struct {
	domain.Scope
	DataSubjectID string
}
type MemoryService struct {
	governanceMu sync.Mutex
	governanceBinding *store.IntegrationGovernanceOwnerBinding
	governanceLedger *DeletionLedger
	governanceFiles *files.Store
	Store  *store.Store
	Ledger *DeletionLedger
	Files  *files.Store
}
type ContextBundle struct{ Records []MemoryRecord }

func New(s *store.Store, l *DeletionLedger) *MemoryService {
	return &MemoryService{Store: s, Ledger: l}
}
func (n Namespace) Validate() error {
	if n.Scope.Validate() != nil || domain.ValidateID(n.DataSubjectID) != nil {
		return ErrInvalid
	}
	return nil
}
func (n Namespace) Key() store.LockKey {
	return store.LockKey{Rank: 90, Kind: "memory", IDs: []string{n.TenantID, n.ProductID, n.WorldInstanceID, content.Identity("MEMORY_SUBJECT", n.WorldInstanceID, n.DataSubjectID)}}
}
func (n Namespace) Locks() []store.LockKey {
	return []store.LockKey{{Rank: 30, IDs: []string{n.TenantID, n.ProductID, n.DataSubjectID}}, n.Key()}
}
func emptyAggregate(n Namespace) Aggregate {
	return Aggregate{Sources: map[string]Source{}, Nodes: map[string]MemoryRecord{}, Epoch: content.Identity("source-epoch", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID), PolicyRevision: 1}
}
func loadTx(ctx context.Context, tx store.Tx, n Namespace) (Aggregate, error) {
	if e := tx.RequireLock(n.Key()); e != nil {
		return Aggregate{}, e
	}
	var b []byte
	e := tx.QueryRowContext(ctx, "SELECT document FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return emptyAggregate(n), nil
	}
	if e != nil {
		return Aggregate{}, e
	}
	var a Aggregate
	e = json.Unmarshal(b, &a)
	return a, e
}
func saveTx(ctx context.Context, tx store.Tx, n Namespace, a Aggregate) error {
	if len(a.Nodes) > MaxNodes || len(a.Sources) > MaxSources || len(a.Grants) > MaxGrants || len(a.Acquisitions) > MaxAcquisitions {
		return ErrInvalid
	}
	b, e := json.Marshal(a)
	if e != nil || len(b) > MaxAggregateBytes {
		return ErrInvalid
	}
	if e = tx.RequireLock(n.Key()); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO memory_aggregates(tenant_id,product_id,world_instance_id,data_subject_id,aggregate_id,revision,document) VALUES(?,?,?,?,?,1,?) ON DUPLICATE KEY UPDATE revision=revision+1,document=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.Key().IDs[3], b, b)
	return e
}
func authorizeTx(ctx context.Context, tx store.Tx, n Namespace, action string) error {
	if n.Validate() != nil {
		return ErrInvalid
	}
	var current bool
	if handled, e := store.IntegrationGovernanceCallerTx(ctx, tx, n.Scope, action); handled {
		if e != nil || n.SubjectID != n.DataSubjectID { return store.ErrDenied }
		var permitted bool
		e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action=? AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", n.TenantID, n.ProductID, n.SubjectID, n.DataSubjectID, action).Scan(&permitted)
		if e != nil || !permitted { return store.ErrDenied }; return nil
	}
	_, integration, e := store.IntegrationTurnInitializationTx(ctx, tx, n.Scope)
	if e != nil {
		return e
	}
	if integration {
		current = n.SubjectID == n.DataSubjectID
	} else {
		e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_instances i JOIN subject_governance g ON g.tenant_id=i.tenant_id AND g.product_id=i.product_id AND g.subject_id=? WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=? AND i.owner_subject_id=? AND g.enabled=1)", n.DataSubjectID, n.TenantID, n.ProductID, n.WorldInstanceID, n.SubjectID).Scan(&current)
	}
	if e != nil {
		return e
	}
	if !current {
		return store.ErrDenied
	}
	if n.SubjectID == n.DataSubjectID && action == "memory.intake" {
		return nil
	}
	e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action=? AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", n.TenantID, n.ProductID, n.SubjectID, n.DataSubjectID, action).Scan(&current)
	if e != nil {
		return e
	}
	if !current {
		return store.ErrDenied
	}
	return nil
}
func authorizeUserTx(ctx context.Context, tx store.Tx, n Namespace, action string) error {
	if e := store.CheckMemoryCallerTx(ctx, tx, n.ProductScope, action); e != nil {
		return e
	}
	return authorizeTx(ctx, tx, n, action)
}
func (s *MemoryService) mutate(ctx context.Context, n Namespace, key, operation string, payload any, fn func(store.Tx, *Aggregate) error) error {
	return s.mutateLocks(ctx, n, key, operation, payload, nil, fn)
}
func (s *MemoryService) mutateLocks(ctx context.Context, n Namespace, key, operation string, payload any, extra []store.LockKey, fn func(store.Tx, *Aggregate) error) error {
	if n.Validate() != nil {
		return ErrInvalid
	}
	g, e := content.Gate(ctx, s.Store, n.ProductScope)
	if e != nil {
		return e
	}
	var v domain.Versions
	e = s.Store.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", n.TenantID, n.ProductID, n.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(payload)
	_, e = s.Store.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: n.ProductScope, WorldScope: &n.Scope, Versions: &v, OwnerSubjectID: n.SubjectID, Operation: operation, CommandKey: key, CommitKey: content.Identity(n.TenantID, n.ProductID, n.SubjectID, n.WorldInstanceID, operation, key), PayloadHash: sha256.Sum256(b), GateVersions: g, Locks: append(n.Locks(), extra...)}, func(tx store.Tx) error {
		if e := authorizeUserTx(ctx, tx, n, "memory.intake"); e != nil {
			return e
		}
		a, e := loadTx(ctx, tx, n)
		if e != nil {
			return e
		}
		if e = fn(tx, &a); e != nil {
			return e
		}
		return saveTx(ctx, tx, n, a)
	})
	return e
}
func actorExists(ctx context.Context, tx store.Tx, n Namespace, id string) error {
	var exists bool
	if domain.ValidateID(id) != nil {
		return ErrInvalid
	}
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?)", n.TenantID, n.ProductID, n.WorldInstanceID, id).Scan(&exists)
	if e != nil {
		return e
	}
	if !exists {
		return store.ErrDenied
	}
	return nil
}
func validUses(uses []Use) bool {
	if len(uses) == 0 || len(uses) > 4 {
		return false
	}
	seen := map[Use]bool{}
	for _, u := range uses {
		if seen[u] {
			return false
		}
		seen[u] = true
		switch u {
		case PrivateResponse, PublicResponse, Relationship, Research:
		default:
			return false
		}
	}
	return true
}

// RecordStatement is the current subject's explicit private delivery to one real
// actor. It preserves PLAYER_STATEMENT and never establishes a world fact.
func (s *MemoryService) RecordStatement(ctx context.Context, n Namespace, id, key, actor, body string, uses []Use) error {
	if domain.ValidateID(id) != nil || len(body) == 0 || len(body) > 8192 || !validUses(uses) || contains(uses, PublicResponse) {
		return ErrInvalid
	}
	return s.mutate(ctx, n, key, "MEMORY_STATEMENT", []any{n, id, actor, body, uses}, func(tx store.Tx, a *Aggregate) error {
		if e := actorExists(ctx, tx, n, actor); e != nil {
			return e
		}
		if _, ok := a.Sources[id]; ok {
			return store.ErrConflict
		}
		if a.Intake == ^uint64(0) {
			return ErrInvalid
		}
		a.Intake++
		a.Sources[id] = Source{ID: id, Body: body, Domain: PlayerStatement, Audience: Private, Uses: uses, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: time.Now().UTC(), EstimatedTime: true}
		a.Grants = append(a.Grants, Grant{ActorID: actor, SourceID: id, Uses: uses})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor, SourceID: id, Kind: Told, EvidenceID: content.Identity("statement", key)})
		node := content.Identity(id, actor, "statement")
		a.Nodes[node] = MemoryRecord{ID: node, ActorID: actor, TargetKind: "PLAYER", TargetID: n.DataSubjectID, Kind: "PREFERENCE", Body: body, Domain: PlayerStatement, Audience: Private, Uses: uses, Sources: []string{id}, Revision: 1}
		return nil
	})
}
func (s *MemoryService) GrantSource(ctx context.Context, n Namespace, id, actor string, uses []Use, key string) error {
	if !validUses(uses) {
		return ErrInvalid
	}
	return s.mutate(ctx, n, key, "MEMORY_GRANT", []any{n, id, actor, uses}, func(tx store.Tx, a *Aggregate) error {
		if e := actorExists(ctx, tx, n, actor); e != nil {
			return e
		}
		source, ok := a.Sources[id]
		if !ok || source.Blocked {
			return store.ErrDenied
		}
		for _, u := range uses {
			if !contains(source.Uses, u) {
				return store.ErrDenied
			}
		}
		a.Grants = append(a.Grants, Grant{ActorID: actor, SourceID: id, Uses: uses})
		return nil
	})
}
func (s *MemoryService) Tell(ctx context.Context, n Namespace, id, from, to, key string) error {
	return s.mutate(ctx, n, key, "MEMORY_TELL", []any{n, id, from, to}, func(tx store.Tx, a *Aggregate) error {
		if e := actorExists(ctx, tx, n, to); e != nil {
			return e
		}
		if !a.sourceAllowed(id, from, Private, PrivateResponse, time.Now()) || contains(a.Disabled, PrivateResponse) {
			return store.ErrDenied
		}
		granted := false
		for _, g := range a.Grants {
			if g.ActorID == to && g.SourceID == id && !g.Revoked && (g.ExpiresAt.IsZero() || time.Now().Before(g.ExpiresAt)) && contains(g.Uses, PrivateResponse) {
				granted = true
			}
		}
		if !granted {
			return store.ErrDenied
		}
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: to, SourceID: id, Kind: Told, EvidenceID: content.Identity("telling", from, to, key)})
		source := a.Sources[id]
		node := content.Identity(id, to, "statement")
		if _, ok := a.Nodes[node]; ok {
			return store.ErrConflict
		}
		a.Nodes[node] = MemoryRecord{ID: node, ActorID: to, TargetKind: "PLAYER", TargetID: n.DataSubjectID, Kind: "PREFERENCE", Domain: source.Domain, Audience: source.Audience, Uses: source.Uses, Body: source.Body, Sources: []string{id}, Revision: 1}
		return nil
	})
}
// Current body-read authority is only the outer gate: return records for this relationship only when the actor has acquired every source and still has the required audience/use permissions.
func (s *MemoryService) AllowedContext(ctx context.Context, scope domain.RelationshipScope, actor string, audience Audience) (ContextBundle, error) {
	out := ContextBundle{Records: []MemoryRecord{}}
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if scope.Validate() != nil || actor != scope.ActorInstanceID || scope.PlayerID != scope.SubjectID {
		return out, store.ErrDenied
	}
	n := Namespace{Scope: scope.Scope, DataSubjectID: scope.PlayerID}
	if _, e := content.Gate(ctx, s.Store, n.ProductScope); e != nil {
		return out, e
	}
	read, e := s.Store.BeginMemoryRead(ctx, n.ProductScope, "memory.private_body.read", n.DataSubjectID)
	if e != nil {
		return out, e
	}
	defer read.Close()
	var b []byte
	e = read.QueryRowContext(ctx, "SELECT m.document FROM memory_aggregates m JOIN runtime_instances i ON i.tenant_id=m.tenant_id AND i.product_id=m.product_id AND i.world_instance_id=m.world_instance_id JOIN world_controls w ON w.tenant_id=i.tenant_id AND w.product_id=i.product_id AND w.world_instance_id=i.world_instance_id JOIN releases r ON r.tenant_id=w.tenant_id AND r.product_id=w.product_id AND r.release_id=w.release_id JOIN subject_governance g ON g.tenant_id=m.tenant_id AND g.product_id=m.product_id AND g.subject_id=m.data_subject_id WHERE m.tenant_id=? AND m.product_id=? AND m.world_instance_id=? AND m.data_subject_id=? AND i.owner_subject_id=? AND r.available=1 AND g.enabled=1", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return out, store.ErrDenied
	}
	if e != nil {
		return out, e
	}
	if e = read.Finish(ctx); e != nil {
		return out, e
	}
	var a Aggregate
	if e = json.Unmarshal(b, &a); e != nil {
		return out, e
	}
	use := PrivateResponse
	if audience == Public {
		use = PublicResponse
	}
	for _, record := range a.Allowed(actor, audience, use, time.Now()) {
		if record.ActorID == actor && record.TargetKind == "PLAYER" && record.TargetID == scope.PlayerID {
			out.Records = append(out.Records, record)
		}
	}
	return out, nil
}

// Initialize imports only knowledge references and goals in the original
// materialized actors. Repeated calls cannot revive tombstones or add knowledge
// from a later immutable release.
func (s *MemoryService) Initialize(ctx context.Context, n Namespace, key string) error {
	return s.mutate(ctx, n, key, "MEMORY_INITIALIZE", n, func(tx store.Tx, a *Aggregate) error {
		init, e := world.InitializationEvidence(ctx, tx, n.Scope)
		if e != nil {
			return e
		}
		return populateInitial(ctx, tx, n, a, init)
	})
}

func (s *MemoryService) CreationLocks(scope domain.Scope) []store.LockKey {
	return (Namespace{Scope: scope, DataSubjectID: scope.SubjectID}).Locks()
}
func (s *MemoryService) CreateInitial(ctx context.Context, tx store.Tx, scope domain.Scope, init world.Initialization) error {
	n := Namespace{Scope: scope, DataSubjectID: scope.SubjectID}
	if init.CreationCommandID != scope.WorldInstanceID || len(init.Actors) == 0 {
		return ErrInvalid
	}
	a, e := loadTx(ctx, tx, n)
	if e != nil {
		return e
	}
	if len(a.Sources) != 0 || len(a.Nodes) != 0 {
		return store.ErrConflict
	}
	if e = populateInitial(ctx, tx, n, &a, init); e != nil {
		return e
	}
	return saveTx(ctx, tx, n, a)
}
func populateInitial(ctx context.Context, tx store.Tx, n Namespace, a *Aggregate, init world.Initialization) error {
	var e error
	for _, actor := range init.Actors {
		for index, goal := range append(append([]string{}, actor.OriginalSetup.InitialGoals...), actor.OriginalCharacter.LongTermGoals...) {
			id := content.Identity(init.CreationCommandID, actor.ID, "goal", strconv.Itoa(index))
			if _, exists := a.Sources[id]; exists {
				continue
			}
			a.Intake++
			a.Sources[id] = Source{ID: id, Body: goal, Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: time.Now().UTC(), EstimatedTime: true}
			a.Grants = append(a.Grants, Grant{ActorID: actor.ID, SourceID: id, Uses: []Use{PrivateResponse, Relationship}})
			a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor.ID, SourceID: id, Kind: Initial, EvidenceID: init.CreationCommandID})
			node := content.Identity(id, actor.ID, "initial")
			a.Nodes[node] = MemoryRecord{ID: node, ActorID: actor.ID, TargetKind: "PLAYER", TargetID: n.DataSubjectID, Kind: "GOAL", Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Sources: []string{id}, Body: goal, Revision: 1}
		}
		// Setup-local references belong to the world release; character-local
		// references belong to the qualified character owner's immutable release.
		characterRelease := init.ReleaseID
		var owner struct {
			Kind      string `json:"kind"`
			ReleaseID string `json:"release_id"`
		}
		if e = json.Unmarshal(actor.OwnerReference, &owner); e != nil {
			return e
		}
		if owner.Kind == "PINNED_DEFINITION" {
			characterRelease = owner.ReleaseID
		} else if owner.Kind != "LOCAL_DRAFT" {
			return ErrInvalid
		}
		type initialReference struct {
			reference    generated.DefinitionRef
			localRelease string
		}
		refs := []initialReference{}
		for _, ref := range actor.OriginalSetup.KnowledgeRefs {
			refs = append(refs, initialReference{ref, init.ReleaseID})
		}
		for _, ref := range actor.OriginalCharacter.InitialKnowledgeRefs {
			refs = append(refs, initialReference{ref, characterRelease})
		}
		for _, origin := range refs {
			ref := origin.reference
			var qualified struct {
				Kind      string `json:"kind"`
				ReleaseID string `json:"release_id"`
				ObjectID  string `json:"object_id"`
				Version   string `json:"version"`
				Hash      string `json:"content_hash"`
			}
			if e = json.Unmarshal(ref, &qualified); e != nil {
				return e
			}
			release := origin.localRelease
			if qualified.Kind == "PINNED_DEFINITION" {
				release = qualified.ReleaseID
			} else if qualified.Kind != "LOCAL_DRAFT" {
				return ErrInvalid
			}
			var raw []byte
			if e = tx.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", n.TenantID, n.ProductID, release).Scan(&raw); e != nil {
				return e
			}
			var rel generated.Release
			if e = generated.Decode("Release", raw, &rel); e != nil {
				return e
			}
			found := false
			for _, object := range rel.Input.Objects {
				var head struct {
					ObjectID string `json:"object_id"`
					Version  string `json:"version"`
				}
				if e = json.Unmarshal(object, &head); e != nil {
					return e
				}
				if head.ObjectID != qualified.ObjectID {
					continue
				}
				var knowledge generated.KnowledgeDefinition
				if e = generated.Decode("KnowledgeDefinition", object, &knowledge); e != nil {
					return e
				}
				if qualified.Kind == "PINNED_DEFINITION" {
					var v any
					if e = json.Unmarshal(object, &v); e != nil {
						return e
					}
					h, e := content.Hash(v)
					if e != nil || h != qualified.Hash || head.Version != qualified.Version {
						return ErrInvalid
					}
				}
				found = true
				id := content.Identity("initial-knowledge", release, head.ObjectID, head.Version)
				if _, ok := a.Sources[id]; !ok {
					a.Intake++
					a.Sources[id] = Source{ID: id, Body: knowledge.Text, Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: time.Now().UTC(), EstimatedTime: true}
				}
				node := content.Identity(id, actor.ID, "initial")
				if _, exists := a.Nodes[node]; exists {
					continue
				}
				source := a.Sources[id]
				if source.Blocked {
					return store.ErrDenied
				}
				a.Grants = append(a.Grants, Grant{ActorID: actor.ID, SourceID: id, Uses: source.Uses})
				a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor.ID, SourceID: id, Kind: Initial, EvidenceID: init.CreationCommandID})
				a.Nodes[node] = MemoryRecord{ID: node, ActorID: actor.ID, TargetKind: "PLAYER", TargetID: n.DataSubjectID, Kind: "BELIEF", Domain: ActorBelief, Audience: Private, Uses: source.Uses, Sources: []string{id}, Body: knowledge.Text, Revision: 1}
			}
			if !found {
				return ErrInvalid
			}
		}
	}
	return nil
}

// WitnessCommitted accepts only an actual committed ACTION_BUNDLE in this
// world, and only its named acting/target participant. Generic access never
// supplies witnessing, and no caller can label a statement as a world fact.
func (s *MemoryService) WitnessCommitted(ctx context.Context, n Namespace, event, actor, key string) error {
	return s.mutate(ctx, n, key, "MEMORY_WITNESS", []any{n, event, actor}, func(tx store.Tx, a *Aggregate) error {
		if e := actorExists(ctx, tx, n, actor); e != nil {
			return e
		}
		var raw []byte
		var revision uint64
		if e := tx.QueryRowContext(ctx, "SELECT r.document,e.revision FROM runtime_events r JOIN events e USING(tenant_id,product_id,record_id) JOIN tasks t ON t.tenant_id=e.tenant_id AND t.product_id=e.product_id AND t.task_id=e.task_id WHERE e.tenant_id=? AND e.product_id=? AND e.world_instance_id=? AND e.owner_subject_id=? AND e.record_id=? AND e.status='COMMITTED' AND t.state='COMMITTED'", n.TenantID, n.ProductID, n.WorldInstanceID, n.SubjectID, event).Scan(&raw, &revision); e != nil {
			return e
		}
		var committed struct {
			Kind    string
			Actions []world.ActionIntent
		}
		if json.Unmarshal(raw, &committed) != nil || committed.Kind != "ACTION_BUNDLE" {
			return store.ErrDenied
		}
		witness := false
		for _, action := range committed.Actions {
			if action.ActorID == actor || action.TargetID == actor {
				witness = true
			}
		}
		if !witness {
			return store.ErrDenied
		}
		id := content.Identity(n.TenantID, n.ProductID, n.WorldInstanceID, "committed-event", event)
		source, exists := a.Sources[id]
		if exists && source.Blocked {
			return store.ErrDenied
		}
		if !exists {
			a.Intake++
			source = Source{ID: id, Body: string(raw), Domain: WorldFact, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: time.Now().UTC(), EstimatedTime: true}
			a.Sources[id] = source
		}
		node := content.Identity(id, actor, "witness")
		if _, exists := a.Nodes[node]; exists {
			return store.ErrConflict
		}
		a.Grants = append(a.Grants, Grant{ActorID: actor, SourceID: id, Uses: source.Uses})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor, SourceID: id, Kind: Witnessed, EvidenceID: event})
		a.Nodes[node] = MemoryRecord{ID: node, ActorID: actor, TargetKind: "PLAYER", TargetID: n.DataSubjectID, Kind: "EPISODE", Body: source.Body, Domain: WorldFact, Audience: Private, Uses: source.Uses, Sources: []string{id}, Revision: 1}
		return nil
	})
}
func (s *MemoryService) AllowedActorContext(ctx context.Context, scope domain.ActorRelationshipScope, dataSubject string, audience Audience) (ContextBundle, error) {
	out := ContextBundle{Records: []MemoryRecord{}}
	if scope.Validate() != nil {
		return out, store.ErrDenied
	}
	n := Namespace{Scope: scope.Scope, DataSubjectID: dataSubject}
	a, _, e := s.AdminRead(ctx, n, scope.ActorInstanceID, "memory.private_body.read")
	if e != nil {
		return out, e
	}
	var pair bool
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*)=2 FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id IN (?,?)", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.ActorInstanceID, scope.TargetActorInstanceID).Scan(&pair); e != nil || !pair {
		return out, store.ErrDenied
	}
	use := PrivateResponse
	if audience == Public {
		use = PublicResponse
	}
	for _, v := range a.Allowed(scope.ActorInstanceID, audience, use, time.Now()) {
		if v.TargetKind == "ACTOR" && v.TargetID == scope.TargetActorInstanceID {
			out.Records = append(out.Records, v)
		}
	}
	return out, nil
}
