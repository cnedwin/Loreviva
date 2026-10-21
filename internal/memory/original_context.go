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

func generationSelection(s domain.Scope, task string, b domain.GenerationBinding) ActorGenerationRequest {
	return ActorGenerationRequest{Scope: s, TaskID: task, ActorID: b.ActorID, DataSubjectID: b.DataSubjectID, TargetKind: b.TargetKind, TargetID: b.TargetID, Audience: Audience(b.Audience), Use: Use(b.Use)}
}

func (m *MemoryService) SnapshotOriginalContextTx(ctx context.Context, tx store.Tx, s domain.Scope, task string) (domain.OriginalContextSnapshot, error) {
	var out domain.OriginalContextSnapshot
	b, e := m.ValidateHarnessContextTx(ctx, tx, s, task)
	if e != nil {
		return out, e
	}
	return m.snapshotContextTx(ctx, tx, s, task, b)
}

func (m *MemoryService) snapshotContextTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, b domain.GenerationBinding) (domain.OriginalContextSnapshot, error) {
	var out domain.OriginalContextSnapshot
	c, e := AssembleGenerationContextTx(ctx, tx, generationSelection(s, task, b))
	if e != nil {
		return out, e
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return out, e
	}
	out.Binding = b
	out.World, e = world.PerceptionTx(ctx, tx, s, b.ActorID)
	if e != nil {
		return out, e
	}
	worldHash, e := content.Hash(out.World)
	if e != nil || worldHash != b.WorldHash {
		return out, store.ErrConflict
	}
	if e = tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&out.Versions.ReleaseID, &out.Versions.WorldRevision, &out.Versions.ControlVersion, &out.Versions.PrivacyEpoch, &out.Versions.ViewEpoch); e != nil {
		return out, e
	}
	out.Sources = domain.ResultSourceBinding{ActorID: b.ActorID, SubjectID: s.SubjectID, Epoch: a.Epoch, Nodes: map[string]string{}}
	for _, n := range append(c.Initial, c.Relationship...) {
		out.Sources.Nodes[n.ID] = lineageHash(a, n)
	}
	return out, nil
}

func (m *MemoryService) ValidateOriginalResultContextTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt string, evidence store.OriginalResultEvidence) (domain.GenerationBinding, error) {
	var zero domain.GenerationBinding
	if e := tx.AssertWorldTask(s, task); e != nil {
		return zero, e
	}
	var raw []byte
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, task, attempt).Scan(&raw); e != nil {
		return zero, e
	}
	var original domain.OriginalContextSnapshot
	if json.Unmarshal(raw, &original) != nil || original.Versions.Validate() != nil || len(original.Sources.Nodes) == 0 {
		return zero, store.ErrDenied
	}
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&raw); e != nil {
		return zero, e
	}
	var taskBinding domain.GenerationBinding
	if json.Unmarshal(raw, &taskBinding) != nil {
		return zero, store.ErrConflict
	}
	base := original.Binding
	if original.Recovery != nil {
		plan := *original.Recovery
		if plan.AttemptID != attempt || plan.Binding != original.Binding || plan.Original.Identity.AttemptID == attempt {
			return zero, store.ErrDenied
		}
		if e := m.validateRecoveryPlanTx(ctx, tx, s, task, plan); e != nil {
			return zero, e
		}
		if _, e := m.recoveryOriginalTx(ctx, tx, s, task, plan, evidence); e != nil {
			return zero, e
		}
		base = plan.BaseBinding
	} else if taskBinding != original.Binding {
		return zero, store.ErrConflict
	}
	current, e := currentGenerationMaterialAtOriginalRevision(ctx, tx, generationSelection(s, task, base), &original.Versions, &original.World, original.Sources.Nodes)
	if e != nil {
		return zero, e
	}
	if !sameGenerationProof(current.Binding, base) {
		return zero, store.ErrConflict
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return zero, e
	}
	refs, e := json.Marshal(original.Sources)
	if e != nil {
		return zero, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return zero, e
	}
	if !resultSourcesAllowed(refs, a, original.Binding.ActorID, now) {
		return zero, store.ErrDenied
	}
	if original.Recovery != nil {
		body, e := m.recoveryOriginalTx(ctx, tx, s, task, *original.Recovery, evidence)
		if e != nil {
			return zero, e
		}
		current, e = recoveryMaterial(current, *original.Recovery, body)
		if e != nil {
			return zero, e
		}
		if !sameGenerationProof(current.Binding, original.Binding) {
			return zero, store.ErrConflict
		}
	}
	return current.Binding, nil
}

// sameGenerationProof compares an original attempt's recorded proof with the
// proof reconstructed now. Component digests let a world advance concurrently:
// only the attempt's own memories, core, skills and world must still match, so
// a memory another task committed is not treated as an identity conflict.
// Older bindings without components fall back to exact equality (fail closed).
func sameGenerationProof(current, base domain.GenerationBinding) bool {
	if current.MemoryGraphHash == "" || base.MemoryGraphHash == "" {
		return current == base
	}
	return current.SameOrigin(base) && current.CoreHash == base.CoreHash &&
		current.SkillsHash == base.SkillsHash && current.MemoryGraphHash == base.MemoryGraphHash &&
		current.WorldHash == base.WorldHash
}
