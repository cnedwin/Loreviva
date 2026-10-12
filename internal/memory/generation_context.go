package memory

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

type ActorGenerationRequest struct {
	Scope                                                domain.Scope
	TaskID, ActorID, DataSubjectID, TargetKind, TargetID string
	Audience                                             Audience
	Use                                                  Use
}
type ActorCore struct {
	ActorID                                       string
	Values, Boundaries, Interests, InnerConflicts []string
	RelationshipPolicy                            string
}

// GenerationContextProof is produced only by this module inside a WORLD UoW.
// It contains no body and is not an independent authority to read or generate.
type GenerationContextProof struct{ hash string }
type ActorGenerationContext struct {
	Core                  ActorCore
	Initial, Relationship []MemoryRecord
	Proof                 *GenerationContextProof
}

func GenerationContextLocks(r ActorGenerationRequest) []store.LockKey {
	keys := (Namespace{Scope: r.Scope, DataSubjectID: r.Scope.SubjectID}).Locks()
	keys = append(keys, (Namespace{Scope: r.Scope, DataSubjectID: r.DataSubjectID}).Locks()...)
	actor := func(id string) store.LockKey {
		return store.LockKey{Rank: 90, Kind: "actor", IDs: []string{r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, id}}
	}
	keys = append(keys, actor(r.ActorID))
	if r.TargetKind == "ACTOR" {
		keys = append(keys, actor(r.TargetID))
	}
	return keys
}

// AssembleGenerationContextTx keeps stable character core, operational INITIAL,
// and directional relationship memory separate. Retained authoring goals and
// knowledge references are never expanded to fill absent or deleted memories.
// R0's caller is the real creator/instance owner; Service/Client enrollment must
// supply its own authority adapter before that deliberate boundary is widened.
func AssembleGenerationContextTx(ctx context.Context, tx store.Tx, r ActorGenerationRequest) (ActorGenerationContext, error) {
	return assembleGenerationContextTx(ctx, tx, r, nil)
}

func assembleGenerationContextTx(ctx context.Context, tx store.Tx, r ActorGenerationRequest, original *domain.Versions) (ActorGenerationContext, error) {
	out := ActorGenerationContext{Initial: []MemoryRecord{}, Relationship: []MemoryRecord{}}
	if r.Scope.Validate() != nil || domain.ValidateID(r.ActorID) != nil || domain.ValidateID(r.DataSubjectID) != nil || domain.ValidateID(r.TargetID) != nil || domain.ValidateID(r.TaskID) != nil {
		return out, ErrInvalid
	}
	if r.Audience != Private || r.Use != PrivateResponse {
		return out, store.ErrDenied
	}
	switch r.TargetKind {
	case "PLAYER":
		if r.TargetID != r.DataSubjectID {
			return out, store.ErrDenied
		}
	case "ACTOR":
		if r.ActorID == r.TargetID {
			return out, store.ErrDenied
		}
	default:
		return out, ErrInvalid
	}
	if e := tx.AssertWorldTask(r.Scope, r.TaskID); e != nil {
		return out, e
	}
	for _, key := range GenerationContextLocks(r) {
		if e := tx.RequireLock(key); e != nil {
			return out, e
		}
	}
	init, e := world.InitializationEvidence(ctx, tx, r.Scope)
	if e != nil {
		return out, e
	}
	var actor world.ActorState
	for _, a := range init.Actors {
		if a.ID == r.ActorID {
			actor = a
		}
	}
	if actor.ID == "" {
		return out, store.ErrDenied
	}
	var raw []byte
	if e = tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.ActorID).Scan(&raw); e != nil {
		return out, e
	}
	var current world.ActorState
	if e = json.Unmarshal(raw, &current); e != nil {
		return out, e
	}
	// Immutable origin must still match the actual materialized actor. Mutable
	// presentation is deliberately absent from this stable core whitelist.
	if digest(current) != digest(actor) {
		return out, store.ErrConflict
	}
	if r.TargetKind == "ACTOR" {
		if e = actorExists(ctx, tx, Namespace{Scope: r.Scope, DataSubjectID: r.DataSubjectID}, r.TargetID); e != nil {
			return out, e
		}
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, e
	}
	initialNS := Namespace{Scope: r.Scope, DataSubjectID: r.Scope.SubjectID}
	relationshipNS := Namespace{Scope: r.Scope, DataSubjectID: r.DataSubjectID}
	synthetic, e := tx.RehearsalBasis(r.Scope)
	if e != nil {
		return out, e
	}
	if synthetic != nil && (synthetic.ActorID != r.ActorID || r.DataSubjectID != r.Scope.SubjectID || r.TargetKind != "PLAYER" || r.TargetID != r.Scope.SubjectID) {
		return out, store.ErrDenied
	}
	// Both use permissions are checked even if one side has no eligible records.
	// Admin private_body.read, governance, and ordinary execute are insufficient.
	for _, n := range []Namespace{initialNS, relationshipNS} {
		if synthetic != nil {
			continue
		}
		var allowed bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use=? AND enabled=1 AND expires_at>?)", n.TenantID, n.ProductID, n.SubjectID, n.DataSubjectID, string(r.Use), now).Scan(&allowed); e != nil {
			return out, e
		}
		if !allowed {
			return out, store.ErrDenied
		}
	}
	initial, e := loadTx(ctx, tx, initialNS)
	if e != nil {
		return out, e
	}
	if synthetic != nil {
		for _, src := range initial.Sources {
			if src.Domain != Sandbox {
				return out, store.ErrDenied
			}
		}
		for _, node := range initial.Nodes {
			if node.Domain != Sandbox {
				return out, store.ErrDenied
			}
		}
	}
	relation := initial
	if initialNS.DataSubjectID != relationshipNS.DataSubjectID {
		relation, e = loadTx(ctx, tx, relationshipNS)
		if e != nil {
			return out, e
		}
	}
	isInitial := func(a Aggregate, n MemoryRecord) bool {
		if n.ActorID != r.ActorID || len(n.Sources) != 1 || len(n.Parents) != 0 || n.ID != content.Identity(n.Sources[0], r.ActorID, "initial") {
			return false
		}
		for _, ac := range a.Acquisitions {
			if ac.ActorID == r.ActorID && ac.SourceID == n.Sources[0] && ac.Kind == Initial && ac.EvidenceID == init.CreationCommandID {
				return true
			}
		}
		return false
	}
	for _, n := range initial.Allowed(r.ActorID, r.Audience, r.Use, now) {
		if isInitial(initial, n) {
			out.Initial = append(out.Initial, n)
		}
	}
	for _, n := range relation.Allowed(r.ActorID, r.Audience, r.Use, now) {
		if !isInitial(relation, n) && n.TargetKind == r.TargetKind && n.TargetID == r.TargetID {
			out.Relationship = append(out.Relationship, n)
		}
	}
	c := actor.OriginalCharacter
	out.Core = ActorCore{ActorID: actor.ID, Values: append([]string{}, c.Values...), Boundaries: append([]string{}, c.Boundaries...), Interests: append([]string{}, c.Interests...), InnerConflicts: append([]string{}, c.InnerConflicts...), RelationshipPolicy: c.RelationshipPolicy}
	var versions domain.Versions
	if e = tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID).Scan(&versions.ReleaseID, &versions.WorldRevision, &versions.ControlVersion, &versions.PrivacyEpoch, &versions.ViewEpoch); e != nil {
		return out, e
	}
	if original != nil {
		// Only the private original-result evidence path may normalize resource
		// revision. Every control/privacy/release field remains exact.
		versions.WorldRevision = original.WorldRevision
		if versions != *original {
			return out, store.ErrConflict
		}
	}
	// Include the entire governing snapshot as hashes, not prompt material. Source,
	// grant, acquisition, policy and ancestor changes invalidate an accepted proof;
	// eligibility and expiry are also freshly evaluated using DB time on each use.
	out.Proof = &GenerationContextProof{hash: digest(struct {
		Request                         ActorGenerationRequest
		Versions                        domain.Versions
		Core                            ActorCore
		Initial, Relationship           []MemoryRecord
		InitialGraph, RelationshipGraph string
	}{r, versions, out.Core, out.Initial, out.Relationship, digest(initial), digest(relation)})}
	return out, nil
}
func ValidateGenerationContextTx(ctx context.Context, tx store.Tx, r ActorGenerationRequest, p *GenerationContextProof) error {
	if p == nil || p.hash == "" {
		return store.ErrDenied
	}
	current, e := AssembleGenerationContextTx(ctx, tx, r)
	if e != nil {
		return e
	}
	if current.Proof.hash != p.hash {
		return store.ErrConflict
	}
	return nil
}
