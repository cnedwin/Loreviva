package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

func (m *MemoryService) AuthorizeOriginalResult(ctx context.Context, lease domain.TaskLease, worker string) (domain.MockResultMetadata, error) {
	r, e := m.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	return m.Store.InspectOriginalResult(ctx, r)
}

func (m *MemoryService) AuthorizeAttemptResult(ctx context.Context, lease domain.TaskLease, worker, attempt string) (domain.MockResultMetadata, error) {
	r, e := m.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	return m.Store.InspectAttemptResult(ctx, r, attempt)
}

func (m *MemoryService) AuthorizeAttemptResultWithOriginal(ctx context.Context, lease domain.TaskLease, worker, attempt string, evidence store.OriginalResultEvidence) (domain.MockResultMetadata, error) {
	r, e := m.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	return m.Store.InspectAttemptResultWithOriginal(ctx, r, attempt, evidence)
}

func (m *MemoryService) OriginalResultBarrier(ctx context.Context, lease domain.TaskLease, worker string) (store.BarrierRequest, error) {
	var zero store.BarrierRequest
	if r, e := m.Store.RehearsalResultBarrier(ctx, lease, worker); e != nil {
		return zero, e
	} else if r != nil {
		return *r, nil
	}
	if lease.WorldInstanceID == nil || lease.OwnerSubjectID != lease.SubjectID {
		return zero, store.ErrDenied
	}
	s := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	var actor string
	var raw []byte
	if e := m.Store.DB.QueryRowContext(ctx, "SELECT actor_id,versions FROM harness_turns WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=? AND task_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID, lease.TaskID).Scan(&actor, &raw); e != nil {
		return zero, e
	}
	var versions domain.Versions
	if e := json.Unmarshal(raw, &versions); e != nil {
		return zero, e
	}
	g, e := content.Gate(ctx, m.Store, s.ProductScope)
	if e != nil {
		return zero, e
	}
	r := store.BarrierRequest{ProductScope: s.ProductScope, WorldScope: &s, Versions: &versions, OwnerSubjectID: s.SubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, GateVersions: g, Locks: GenerationContextLocks(ActorGenerationRequest{Scope: s, ActorID: actor, DataSubjectID: s.SubjectID, TargetKind: "PLAYER", TargetID: s.SubjectID})}
	return r, nil
}

func (m *MemoryService) ValidateResultSourcesTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, meta domain.MockResultMetadata, evidence store.OriginalResultEvidence) error {
	b, e := m.ValidateOriginalResultContextTx(ctx, tx, s, task, meta.Identity.AttemptID, evidence)
	if e != nil {
		return e
	}
	var raw []byte
	var intent bool
	if e = tx.QueryRowContext(ctx, "SELECT r.document,l.delete_intent FROM harness_result_sources r JOIN memory_asset_lineage l USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND l.world_instance_id=? AND l.data_subject_id=? AND l.actor_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID, meta.BodyStorage, s.WorldInstanceID, s.SubjectID, b.ActorID).Scan(&raw, &intent); e != nil {
		return e
	}
	var refs domain.ResultSourceBinding
	if intent || json.Unmarshal(raw, &refs) != nil || refs.Storage != meta.BodyStorage || refs.BodyHash != meta.BodyHash || refs.SubjectID != s.SubjectID {
		return store.ErrDenied
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	if !resultSourcesAllowed(raw, a, b.ActorID, now) {
		return store.ErrDenied
	}
	return nil
}

func (m *MemoryService) BindResultSourcesTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt, storage, hash string, evidence store.OriginalResultEvidence) error {
	binding, e := m.ValidateOriginalResultContextTx(ctx, tx, s, task, attempt, evidence)
	if e != nil {
		return e
	}
	c, e := AssembleGenerationContextTx(ctx, tx, ActorGenerationRequest{Scope: s, TaskID: task, ActorID: binding.ActorID, DataSubjectID: binding.DataSubjectID, TargetKind: binding.TargetKind, TargetID: binding.TargetID, Audience: Private, Use: PrivateResponse})
	if e != nil {
		return e
	}
	if binding.DataSubjectID != s.SubjectID {
		return store.ErrDenied
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return e
	}
	refs := domain.ResultSourceBinding{ActorID: binding.ActorID, SubjectID: s.SubjectID, Storage: storage, BodyHash: hash, Epoch: a.Epoch, Nodes: map[string]string{}}
	for _, n := range append(c.Initial, c.Relationship...) {
		refs.Nodes[n.ID] = lineageHash(a, n)
	}
	return tx.BindHarnessResult(ctx, s, task, attempt, refs)
}

// The pending result is not a context node. Every original selected node must
// remain eligible at the exact original source revisions; it cannot self-feed.
func resultSourcesAllowed(raw []byte, a Aggregate, actor string, now time.Time) bool {
	var b domain.ResultSourceBinding
	if json.Unmarshal(raw, &b) != nil || b.ActorID != actor || b.Epoch != a.Epoch || len(b.Nodes) == 0 {
		return false
	}
	permitted := map[string]string{}
	for _, n := range a.Allowed(actor, Private, PrivateResponse, now) {
		permitted[n.ID] = lineageHash(a, n)
	}
	for node, hash := range b.Nodes {
		if permitted[node] != hash {
			return false
		}
	}
	return true
}
func resultLineageTx(ctx context.Context, tx store.Tx, n Namespace, storage, actor string, a Aggregate) (bool, bool, error) {
	var raw []byte
	e := tx.QueryRowContext(ctx, "SELECT document FROM harness_result_sources WHERE tenant_id=? AND product_id=? AND storage_key=?", n.TenantID, n.ProductID, storage).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return false, false, nil
	}
	if e != nil {
		return false, false, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return true, false, e
	}
	return true, resultSourcesAllowed(raw, a, actor, now), nil
}

// Fixed Files owning current-governance check. No body permission is minted.
// Files calls this before/after IO, never from a held SQL transaction.
func (m *MemoryService) CheckResultGovernance(ctx context.Context) error {
	if m == nil || m.Store == nil || m.Ledger == nil {
		return ErrPreservation
	}
	ctx, close, e := m.Store.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer close()
	if e = m.Ledger.ReadyDatabase(ctx, m.Store); e != nil {
		return e
	}
	return maintenance.GovernanceReady(ctx, m.Store)
}
