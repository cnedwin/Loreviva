package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

// HarnessContextValidator is installed once by app.NewRuntime. It is an owning
// module dependency, not a request-supplied callback or permission purpose.
type HarnessContextValidator interface {
	BuildRecoveryContextTx(context.Context, Tx, domain.Scope, string, domain.RecoveryPlan, OriginalResultEvidence) (domain.GenerationMaterial, domain.OriginalContextSnapshot, error)
	InspectApprovedTurnTx(context.Context, Tx, domain.Scope, string, domain.Versions) (domain.CandidateCommit, error)
	PlanGenerationLocks(context.Context, domain.Scope, string) ([]LockKey, error)
	OriginalResultBarrier(context.Context, domain.TaskLease, string) (BarrierRequest, error)
	SnapshotOriginalContextTx(context.Context, Tx, domain.Scope, string) (domain.OriginalContextSnapshot, error)
	ValidateOriginalResultContextTx(context.Context, Tx, domain.Scope, string, string, OriginalResultEvidence) (domain.GenerationBinding, error)
	ValidateResultSourcesTx(context.Context, Tx, domain.Scope, string, domain.MockResultMetadata, OriginalResultEvidence) error
	BuildGenerationRequestTx(context.Context, Tx, domain.Scope, string) (domain.GenerationMaterial, error)
	BindResultSourcesTx(context.Context, Tx, domain.Scope, string, string, string, string, OriginalResultEvidence) error
	FreezeHarnessBindingTx(context.Context, Tx, domain.Scope, string, domain.GenerationSelection) (domain.GenerationBinding, error)
	ValidateHarnessContextTx(context.Context, Tx, domain.Scope, string) (domain.GenerationBinding, error)
}

// InitializeHarnessContext is the sole managed-task first-binding path. The
// fixed owning validator assembles current authority in this WORLD UoW. This
// method cannot perform business writes through a caller-supplied callback.
func (s *Store) InitializeHarnessContext(ctx context.Context, r BarrierRequest, selection domain.GenerationSelection) (CommitReceipt, error) {
	if s.GenerationContext == nil || r.WorldScope == nil || r.Purpose != ExecutionPurpose || r.Operation != "HARNESS_CONTEXT" {
		return CommitReceipt{}, ErrDenied
	}
	_, hash, e := harnessPayload(selection)
	if e != nil {
		return CommitReceipt{}, e
	}
	r.PayloadHash = hash
	r.harnessInitializeContext = true
	if e := s.planPerception(ctx, &r, selection.ActorID); e != nil {
		return CommitReceipt{}, e
	}
	return s.WithBarrier(ctx, r, func(tx Tx) error {
		b, e := s.GenerationContext.FreezeHarnessBindingTx(ctx, tx, *r.WorldScope, r.TaskID, selection)
		if e != nil {
			return e
		}
		if e = checkHarnessQuotedRoute(ctx, tx.(*unit), r.ProductScope, r.TaskID, b); e != nil {
			return e
		}
		return advanceTurn(ctx, tx.(*unit), "CONTEXT_BOUND")
	})
}

// This fixed precollection runs before BeginTx. It loads only the immutable
// owning actor binding and never relaxes the subsequent current-context check.
func (s *Store) planBoundGeneration(ctx context.Context, r *BarrierRequest) error {
	if r.rehearsalExecution != nil {
		return nil
	} // Fixed owning factory already collected the complete exact rehearsal graph.
	if e := s.planCandidateExecution(ctx, r); e != nil {
		return e
	}
	if r.WorldScope == nil || r.commandOnly || r.Purpose != ExecutionPurpose {
		return nil
	}
	var raw []byte
	e := s.DB.QueryRowContext(ctx, "SELECT document FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if s.GenerationContext == nil {
		return ErrDenied
	}
	var b domain.GenerationBinding
	if json.Unmarshal(raw, &b) != nil {
		return ErrDenied
	}
	return s.planPerception(ctx, r, b.ActorID)
}

// Only this fixed owning call can populate the private release lock allowlist.
// It is rebuilt per operation, never deserialized from a request or receipt.
func (s *Store) planPerception(ctx context.Context, r *BarrierRequest, actor string) error {
	if s.GenerationContext == nil || r.WorldScope == nil {
		return ErrDenied
	}
	keys, e := s.GenerationContext.PlanGenerationLocks(ctx, *r.WorldScope, actor)
	if e != nil {
		return e
	}
	r.harnessPerceptionReleases = map[string]struct{}{}
	for _, k := range keys {
		if k.Rank == 90 && k.Kind == "content_release" && len(k.IDs) == 3 && k.IDs[0] == r.ProductScope.TenantID && k.IDs[1] == r.ProductScope.ProductID {
			r.harnessPerceptionReleases[k.IDs[2]] = struct{}{}
		}
	}
	r.Locks = append(r.Locks, keys...)
	return nil
}

func checkHarnessQuotedRoute(ctx context.Context, u *unit, s domain.ProductScope, task string, b domain.GenerationBinding) error {
	var raw []byte
	if e := u.tx.QueryRowContext(ctx, "SELECT q.document FROM harness_envelopes e JOIN harness_quotes q ON q.tenant_id=e.tenant_id AND q.product_id=e.product_id AND q.quote_id=e.quote_id WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", s.TenantID, s.ProductID, task).Scan(&raw); e != nil {
		return e
	}
	var q HarnessQuote
	if e := json.Unmarshal(raw, &q); e != nil {
		return e
	}
	if q.Kind != "TURN" || hex.EncodeToString(q.RouteHash[:]) != b.RouteID {
		return ErrConflict
	}
	return nil
}

func (u *unit) BindHarnessContext(ctx context.Context, s domain.Scope, task string, b domain.GenerationBinding) error {
	if e := u.AssertWorldTask(s, task); e != nil {
		return e
	}
	if e := u.RequireLock(harnessTaskKey(s.ProductScope, task)); e != nil {
		return e
	}
	for _, id := range []string{b.ActorID, b.DataSubjectID, b.TargetID, b.RouteID} {
		if domain.ValidateID(id) != nil {
			return ErrDenied
		}
	}
	if b.Audience != "PRIVATE" || b.Use != "PRIVATE_RESPONSE" || b.TargetKind != "PLAYER" && b.TargetKind != "ACTOR" {
		return ErrDenied
	}
	for _, hash := range []string{b.ContextHash, b.RequestHash, b.WorldHash} {
		raw, e := hex.DecodeString(hash)
		if e != nil || len(raw) != 32 {
			return ErrDenied
		}
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return e
	}
	var old []byte
	e = u.tx.QueryRowContext(ctx, "SELECT document FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&old)
	if e == nil {
		var previous domain.GenerationBinding
		if e = json.Unmarshal(old, &previous); e != nil {
			return e
		}
		if previous != b {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	// Binding is immutable once frozen. Extra contexts need their own approved
	// attempt-scoped snapshot in the recovery phase; this does not overwrite it.
	_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_context_bindings VALUES(?,?,?,?)", s.TenantID, s.ProductID, task, raw)
	return e
}

func (s *Store) checkHarnessContext(ctx context.Context, u *unit, r BarrierRequest) error {
	// Product candidate intake is durable before its executor is wired. Existing
	// generic dispatch must not bypass that future fixed owning context adapter.
	// No production candidate handler is registered until the full path exists.
	if r.WorldScope == nil && !r.commandOnly && r.Purpose == ExecutionPurpose {
		var candidate bool
		if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&candidate); e != nil {
			return e
		}
		if candidate {
			_, e := s.candidateMaterialTx(ctx, u, r)
			return e
		}
	}
	if r.WorldScope == nil || r.commandOnly || r.Purpose != ExecutionPurpose {
		return nil
	}
	var bound, managed bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?),EXISTS(SELECT 1 FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&bound, &managed); e != nil {
		return e
	}
	if !bound {
		if managed && !r.harnessInitializeContext {
			return ErrDenied
		}
		return nil
	}
	if s.GenerationContext == nil {
		return ErrDenied
	}
	var binding domain.GenerationBinding
	if e := s.checkPersistedSkills(ctx, u, r); e != nil {
		return e
	}
	var e error
	if r.harnessRecoveryAttempt != "" {
		var material domain.GenerationMaterial
		material, _, e = s.checkRecoveryDispatchTx(ctx, u, r)
		binding = material.Binding
	} else if r.harnessOriginalResultAttempt != "" {
		binding, e = s.GenerationContext.ValidateOriginalResultContextTx(ctx, u, *r.WorldScope, r.TaskID, r.harnessOriginalResultAttempt, r.recoveryEvidence)
		if e == nil {
			e = s.checkRecoveryDependencyTx(ctx, u, r)
		}
	} else {
		binding, e = s.GenerationContext.ValidateHarnessContextTx(ctx, u, *r.WorldScope, r.TaskID)
	}
	if e != nil {
		return e
	}
	if managed {
		if e = checkHarnessQuotedRoute(ctx, u, r.ProductScope, r.TaskID, binding); e != nil {
			return e
		}
	}
	if r.phase == "AUTHORIZE" || r.phase == "SEND" {
		if binding.RouteID != r.harnessDispatchRoute || binding.RequestHash != hex.EncodeToString(r.harnessDispatchHash[:]) {
			return ErrConflict
		}
	}
	return nil
}
