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

// This is the fixed root-report read, not a generation context or ordinary
// committed-history permission. The Store owns its current root/child proof.
func (m *MemoryService) InspectRehearsalChildResultTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, v domain.Versions, c domain.CandidateCommit) error {
	return m.inspectRehearsalSourcesTx(ctx, tx, s, task, v, c.Versions, c.Metadata, c.OriginalDependency)
}
func (m *MemoryService) InspectRehearsalNegativeSourcesTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, v domain.Versions, n domain.RehearsalNegativeOutcome) error {
	return m.inspectRehearsalSourcesTx(ctx, tx, s, task, v, n.Versions, n.Metadata, n.OriginalDependency)
}
func (m *MemoryService) inspectRehearsalSourcesTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, v, original domain.Versions, meta domain.MockResultMetadata, dependency *domain.MockResultMetadata) error {
	basis, e := tx.RehearsalBasis(s)
	if e != nil {
		return e
	}
	if basis == nil {
		return store.ErrDenied
	}
	expected := original
	expected.WorldRevision = v.WorldRevision
	if expected != v {
		return store.ErrDenied
	}
	init, e := world.InitializationEvidence(ctx, tx, s)
	if e != nil {
		return e
	}
	var raw []byte
	if e = tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, basis.ActorID).Scan(&raw); e != nil {
		return e
	}
	var actor world.ActorState
	if json.Unmarshal(raw, &actor) != nil {
		return store.ErrDenied
	}
	same := false
	for _, a := range init.Actors {
		if a.ID == basis.ActorID && digest(a) == digest(actor) {
			same = true
		}
	}
	if !same {
		return store.ErrDenied
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return e
	}
	for _, src := range a.Sources {
		if src.Domain != Sandbox {
			return store.ErrDenied
		}
	}
	for _, node := range a.Nodes {
		if node.Domain != Sandbox {
			return store.ErrDenied
		}
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	current, e := world.PerceptionTx(ctx, tx, s, basis.ActorID)
	if e != nil {
		return e
	}
	route, e := world.GenerationRouteTx(ctx, tx, s, basis.ActorID)
	if e != nil {
		return e
	}
	routeHash, e := content.Hash(route)
	if e != nil {
		return e
	}
	metas := []domain.MockResultMetadata{meta}
	if dependency != nil {
		metas = append([]domain.MockResultMetadata{*dependency}, metas...)
	}
	for i, meta := range metas {
		var intent bool
		if e = tx.QueryRowContext(ctx, "SELECT r.document,l.delete_intent FROM harness_result_sources r JOIN memory_asset_lineage l USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND l.world_instance_id=? AND l.data_subject_id=? AND l.actor_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID, meta.BodyStorage, s.WorldInstanceID, s.SubjectID, basis.ActorID).Scan(&raw, &intent); e != nil {
			return e
		}
		var refs domain.ResultSourceBinding
		if intent || json.Unmarshal(raw, &refs) != nil || refs.Storage != meta.BodyStorage || refs.BodyHash != meta.BodyHash || refs.SubjectID != s.SubjectID || !resultSourcesAllowed(raw, a, basis.ActorID, now) {
			return store.ErrDenied
		}
		if e = tx.QueryRowContext(ctx, "SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID).Scan(&raw); e != nil {
			return e
		}
		var snapshot domain.OriginalContextSnapshot
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Binding.ActorID != basis.ActorID || snapshot.Binding.RequestHash != meta.Identity.RequestHash || snapshot.Binding.RouteID != routeHash || !world.OriginalPerceptionStillAllowed(snapshot.World, current) {
			return store.ErrDenied
		}
		if i == 0 && snapshot.Recovery != nil {
			return store.ErrDenied
		}
		if i == 1 {
			if snapshot.Recovery == nil || dependency == nil || snapshot.Recovery.Original != *dependency || snapshot.Recovery.AttemptID != meta.Identity.AttemptID || snapshot.Recovery.Binding != snapshot.Binding {
				return store.ErrDenied
			}
			if e = m.validateRecoveryPlanTx(ctx, tx, s, task, *snapshot.Recovery); e != nil {
				return e
			}
		}
	}
	return nil
}
