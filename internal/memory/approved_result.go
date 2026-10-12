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

// Terminal reads use current selected source authority and the real world receipt.
func (m *MemoryService) AuthorizeApprovedTurn(ctx context.Context, s domain.Scope, task string) (domain.CandidateCommit, error) {
	return m.Store.InspectApprovedTurn(ctx, s, task)
}
func (m *MemoryService) InspectApprovedTurnTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, v domain.Versions) (domain.CandidateCommit, error) {
	var out domain.CandidateCommit
	var actor string
	if e := tx.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=? AND owner_subject_id=?", s.TenantID, s.ProductID, task, s.WorldInstanceID, s.SubjectID).Scan(&actor); e != nil {
		return out, e
	}

	var raw []byte
	e := tx.QueryRowContext(ctx, "SELECT c.document FROM harness_turn_commits c JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) JOIN approved_results a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.record_id=c.result_id AND a.task_id=c.task_id JOIN commit_receipts r ON r.commit_key=c.receipt_key AND r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.task_id=c.task_id WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND t.owner_subject_id=? AND t.state='COMMITTED' AND h.fsm_state='COMMITTED' AND a.status='COMMITTED' AND r.owner_subject_id=? AND r.world_instance_id=? AND r.operation='HARNESS_TURN_COMMIT' AND r.phase='COMMIT'", s.TenantID, s.ProductID, task, s.WorldInstanceID, s.SubjectID, s.SubjectID, s.SubjectID, s.WorldInstanceID).Scan(&raw)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	if json.Unmarshal(raw, &out) != nil || out.Metadata.Identity.Scope != s || out.Metadata.Identity.TaskID != task {
		return domain.CandidateCommit{}, store.ErrConflict
	}
	retired, e := tx.AssertCommittedHistory(ctx, s, task)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	var active bool
	if e = tx.QueryRowContext(ctx, "SELECT state='COMMITTED' AND narrative_generation=? FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", out.NarrativeGeneration, s.TenantID, s.ProductID, task).Scan(&active); e != nil {
		return domain.CandidateCommit{}, e
	}
	if (!active && !retired) || out.NarrativeGeneration == 0 {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	expected := out.Versions
	expected.WorldRevision = v.WorldRevision
	if retired {
		expected.ReleaseID = v.ReleaseID
		expected.ControlVersion = v.ControlVersion
		expected.ViewEpoch = v.ViewEpoch
	}
	if expected != v {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	init, e := world.InitializationEvidence(ctx, tx, s)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	var current world.ActorState
	if e = tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, actor).Scan(&raw); e != nil {
		return domain.CandidateCommit{}, e
	}
	if json.Unmarshal(raw, &current) != nil {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	same := false
	for _, original := range init.Actors {
		if original.ID == actor && digest(original) == digest(current) {
			same = true
		}
	}
	if !same {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	var allowed bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", s.TenantID, s.ProductID, s.SubjectID, s.SubjectID).Scan(&allowed); e != nil {
		return domain.CandidateCommit{}, e
	}
	if !allowed {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	var intent bool
	if e = tx.QueryRowContext(ctx, "SELECT r.document,l.delete_intent FROM harness_result_sources r JOIN memory_asset_lineage l USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND l.world_instance_id=? AND l.data_subject_id=? AND l.actor_id=?", s.TenantID, s.ProductID, task, out.Metadata.Identity.AttemptID, out.Metadata.BodyStorage, s.WorldInstanceID, s.SubjectID, actor).Scan(&raw, &intent); e != nil {
		return domain.CandidateCommit{}, e
	}
	var refs domain.ResultSourceBinding
	if intent || json.Unmarshal(raw, &refs) != nil || refs.BodyHash != out.Metadata.BodyHash || refs.Storage != out.Metadata.BodyStorage || refs.SubjectID != s.SubjectID {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return domain.CandidateCommit{}, e
	}
	if !resultSourcesAllowed(raw, a, actor, now) {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	var original domain.OriginalContextSnapshot
	if e = tx.QueryRowContext(ctx, "SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, task, out.Metadata.Identity.AttemptID).Scan(&raw); e != nil {
		return domain.CandidateCommit{}, e
	}
	if json.Unmarshal(raw, &original) != nil {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	currentWorld, e := world.PerceptionTx(ctx, tx, s, actor)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	if !approvedPerceptionAllowed(original.World, currentWorld, retired) {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	if original.Recovery != nil {
		plan := *original.Recovery
		if out.OriginalDependency == nil || *out.OriginalDependency != plan.Original || plan.AttemptID != out.Metadata.Identity.AttemptID || plan.Binding != original.Binding {
			return domain.CandidateCommit{}, store.ErrDenied
		}
		if e = m.validateRecoveryPlanTx(ctx, tx, s, task, plan); e != nil {
			return domain.CandidateCommit{}, e
		}
		if e = m.inspectApprovedDependencyTx(ctx, tx, s, task, actor, plan.Original, a, currentWorld, now, retired); e != nil {
			return domain.CandidateCommit{}, e
		}
	} else if out.OriginalDependency != nil {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	route, e := world.CommittedGenerationRouteTx(ctx, tx, s, actor, task)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	if retired {
		route.ReleaseID = original.World.Versions.ReleaseID
	}
	routeHash, e := content.Hash(route)
	if e != nil || routeHash != original.Binding.RouteID {
		return domain.CandidateCommit{}, store.ErrDenied
	}
	return out, nil
}

func (m *MemoryService) inspectApprovedDependencyTx(ctx context.Context, tx store.Tx, s domain.Scope, task, actor string, meta domain.MockResultMetadata, a Aggregate, currentWorld domain.WorldPerception, now time.Time, retired bool) error {
	if meta.Identity.Scope != s || meta.Identity.TaskID != task {
		return store.ErrDenied
	}
	var raw []byte
	var intent bool
	if e := tx.QueryRowContext(ctx, "SELECT r.document,l.delete_intent FROM harness_result_sources r JOIN memory_asset_lineage l USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND l.world_instance_id=? AND l.data_subject_id=? AND l.actor_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID, meta.BodyStorage, s.WorldInstanceID, s.SubjectID, actor).Scan(&raw, &intent); e != nil {
		return e
	}
	var refs domain.ResultSourceBinding
	if intent || json.Unmarshal(raw, &refs) != nil || refs.BodyHash != meta.BodyHash || refs.Storage != meta.BodyStorage || refs.SubjectID != s.SubjectID || !resultSourcesAllowed(raw, a, actor, now) {
		return store.ErrDenied
	}
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID).Scan(&raw); e != nil {
		return e
	}
	var original domain.OriginalContextSnapshot
	if json.Unmarshal(raw, &original) != nil || original.Recovery != nil || original.Binding.RequestHash != meta.Identity.RequestHash || !approvedPerceptionAllowed(original.World, currentWorld, retired) {
		return store.ErrDenied
	}
	route, e := world.CommittedGenerationRouteTx(ctx, tx, s, actor, task)
	if e != nil {
		return e
	}
	if retired {
		route.ReleaseID = original.World.Versions.ReleaseID
	}
	hash, e := content.Hash(route)
	if e != nil || hash != original.Binding.RouteID {
		return store.ErrDenied
	}
	return nil
}

// Used only after AssertCommittedHistory. Current selected identities and
// permissions remain mandatory; the recorded presentation migration does not
// retroactively alter the immutable original request.
func approvedPerceptionAllowed(original, current domain.WorldPerception, retired bool) bool {
	if retired {
		current.Versions.ReleaseID = original.Versions.ReleaseID
		current.Versions.ControlVersion = original.Versions.ControlVersion
		current.Versions.ViewEpoch = original.Versions.ViewEpoch
	}
	return world.OriginalPerceptionStillAllowed(original, current)
}
