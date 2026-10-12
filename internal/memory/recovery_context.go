package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"unicode/utf8"
)

func (m *MemoryService) validateRecoveryPlanTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, plan domain.RecoveryPlan) error {
	var raw []byte
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&raw); e != nil {
		return e
	}
	var saved domain.RecoveryPlan
	if json.Unmarshal(raw, &saved) != nil || saved != plan {
		return store.ErrConflict
	}
	return nil
}

// Only the installed Files instance can unwrap the live locked original bytes.
func (m *MemoryService) recoveryOriginalTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, plan domain.RecoveryPlan, evidence store.OriginalResultEvidence) ([]byte, error) {
	if m.Files == nil || plan.Original.Identity.Scope != s || plan.Original.Identity.TaskID != task {
		return nil, store.ErrDenied
	}
	if e := tx.RequireLock(store.LockKey{Rank: 60, IDs: []string{s.TenantID, s.ProductID, task, plan.Original.Identity.AttemptID, plan.Original.Identity.AuthorizationID}}); e != nil {
		return nil, e
	}
	body, meta, e := m.Files.RecoveryOriginalBytes(evidence, s, task, plan.Original.Identity.AttemptID)
	if e != nil {
		return nil, e
	}
	if meta != plan.Original || !utf8.Valid(body) {
		return nil, store.ErrDenied
	}
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != meta.BodyHash {
		return nil, store.ErrConflict
	}
	if e = m.ValidateResultSourcesTx(ctx, tx, s, task, meta, nil); e != nil {
		return nil, e
	}
	return body, nil
}

func recoveryMaterial(base domain.GenerationMaterial, plan domain.RecoveryPlan, body []byte) (domain.GenerationMaterial, error) {
	if !utf8.Valid(body) || len(body) == 0 || (plan.Reason != "STRUCTURE_REPAIR" && plan.Reason != "STATE_CONFLICT_RECOMPUTE") {
		return domain.GenerationMaterial{}, store.ErrDenied
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(base.Canonical, &doc) != nil {
		return domain.GenerationMaterial{}, store.ErrDenied
	}
	raw, e := json.Marshal(struct{ Reason, OriginalAttempt, OriginalBody string }{plan.Reason, plan.Original.Identity.AttemptID, string(body)})
	if e != nil {
		return domain.GenerationMaterial{}, e
	}
	doc["Recovery"] = raw
	raw, e = json.Marshal(doc)
	if e != nil {
		return domain.GenerationMaterial{}, e
	}
	raw, e = wire.Canonical(raw)
	if e != nil {
		return domain.GenerationMaterial{}, e
	}
	if uint64(utf8.RuneCount(raw)) > base.InputBound || base.InputBound > store.MockInputLimit || base.OutputBound > store.MockOutputLimit {
		return domain.GenerationMaterial{}, store.ErrDenied
	}
	h := sha256.Sum256(raw)
	base.Canonical = raw
	base.Binding.RequestHash = hex.EncodeToString(h[:])
	return base, nil
}

// Preview is a fixed owning builder, not a permission or caller supplied body.
func (m *MemoryService) BuildRecoveryContextTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, plan domain.RecoveryPlan, evidence store.OriginalResultEvidence) (domain.GenerationMaterial, domain.OriginalContextSnapshot, error) {
	var zero domain.GenerationMaterial
	var snapshot domain.OriginalContextSnapshot
	body, e := m.recoveryOriginalTx(ctx, tx, s, task, plan, evidence)
	if e != nil {
		return zero, snapshot, e
	}
	var raw []byte
	if e = tx.QueryRowContext(ctx, "SELECT document FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&raw); e != nil {
		return zero, snapshot, e
	}
	var original domain.GenerationBinding
	if json.Unmarshal(raw, &original) != nil {
		return zero, snapshot, store.ErrDenied
	}
	base, e := currentGenerationMaterial(ctx, tx, generationSelection(s, task, original))
	if e != nil {
		return zero, snapshot, e
	}
	snapshot, e = m.snapshotContextTx(ctx, tx, s, task, base.Binding)
	if e != nil {
		return zero, snapshot, e
	}
	material, e := recoveryMaterial(base, plan, body)
	if e != nil {
		return zero, snapshot, e
	}
	plan.BaseBinding = base.Binding
	plan.Binding = material.Binding
	snapshot.Binding = material.Binding
	snapshot.Recovery = &plan
	return material, snapshot, nil
}
