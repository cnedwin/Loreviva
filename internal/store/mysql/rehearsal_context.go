package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
)

type rehearsalExecution struct {
	root           domain.TaskLease
	worker         string
	initialization rehearsalInitialization
	child          rehearsalChildStart
	keys           map[string]bool
}
type rehearsalExecutionKey struct{}
type rehearsalExecutionContext struct {
	store       *Store
	root, child domain.TaskLease
	worker      string
}

// This opaque context carries original identity only. Every consumer rebuilds
// the owning plan and revalidates both current leases and source authority.
func (s *Store) EnterRehearsalChildExecution(ctx context.Context, root, child domain.TaskLease, worker string) (context.Context, func(), error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, nil, e
	}
	r, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		close()
		return nil, nil, e
	}
	r.Operation = "REHEARSAL_EXECUTION_INSPECT"
	r.CommandKey = turnIdentity(child.TaskID, "execution-inspect")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, _ = harnessPayload([]string{root.TaskID, child.TaskID})
	done := errors.New("current rehearsal execution inspected")
	_, _, e = s.barrier(ctx, r, func(Tx) error { return done })
	if !errors.Is(e, done) {
		close()
		if e == nil {
			e = ErrDenied
		}
		return nil, nil, e
	}
	return context.WithValue(ctx, rehearsalExecutionKey{}, rehearsalExecutionContext{s, root, child, worker}), close, nil
}

func (s *Store) RehearsalResultBarrier(ctx context.Context, child domain.TaskLease, worker string) (*BarrierRequest, error) {
	p, ok := ctx.Value(rehearsalExecutionKey{}).(rehearsalExecutionContext)
	if !ok {
		return nil, nil
	}
	// ExpiresAt is an observed scheduling value, not lease authority. Existing
	// fixed recovery readers reconstruct only immutable identity and fence;
	// current expiration is always read under the actual task locks below.
	if p.store != s || p.worker != worker || p.child.ProductScope != child.ProductScope || p.child.TaskID != child.TaskID || p.child.OwnerSubjectID != child.OwnerSubjectID || p.child.Fence != child.Fence || p.child.WorldInstanceID == nil || child.WorldInstanceID == nil || *p.child.WorldInstanceID != *child.WorldInstanceID {
		return nil, ErrDenied
	}
	r, e := s.rehearsalChildBarrier(ctx, p.root, child, worker)
	if e != nil {
		return nil, e
	}
	// Preserve the exact originally accepted control/privacy/view basis for all
	// precommit result uses; only S46 resource revision may later normalize.
	var raw []byte
	if e = s.DB.QueryRowContext(ctx, "SELECT versions FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=?", child.TenantID, child.ProductID, child.TaskID, *child.WorldInstanceID).Scan(&raw); e != nil {
		return nil, e
	}
	var v domain.Versions
	if json.Unmarshal(raw, &v) != nil {
		return nil, ErrDenied
	}
	r.Versions = &v
	return &r, nil
}

func (s *Store) PrepareRehearsalTurn(ctx context.Context, root, child domain.TaskLease, worker string) (DispatchRequest, error) {
	if _, e := s.BindRehearsalChildContext(ctx, root, child, worker); e != nil {
		return DispatchRequest{}, e
	}
	r, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		return DispatchRequest{}, e
	}
	return s.OriginalTurnDispatch(ctx, r)
}

// Only the fixed owning execution context can select the isolated preparation.
// Its stored root is identity, not cached authority; preparation rebuilds both
// current lease/source/deadline proofs in the real transaction.
func (s *Store) PrepareCurrentRehearsalTurn(ctx context.Context, child domain.TaskLease, worker string) (*DispatchRequest, error) {
	p, ok := ctx.Value(rehearsalExecutionKey{}).(rehearsalExecutionContext)
	if !ok {
		return nil, nil
	}
	if _, e := s.RehearsalResultBarrier(ctx, child, worker); e != nil {
		return nil, e
	}
	d, e := s.PrepareRehearsalTurn(ctx, p.root, child, worker)
	if e != nil {
		return nil, e
	}
	return &d, nil
}

// This predicate does not mint the private plan. It only lets the fixed Mock
// adapter distinguish its already-authorized typed request from online TURN.
func (d DispatchRequest) IsRehearsalChild() bool {
	return d.rehearsalExecution != nil && d.rehearsalExecution.child.child == d.TaskID
}

func (u *unit) RehearsalBasis(scope domain.Scope) (*domain.RehearsalBasis, error) {
	var init *rehearsalInitialization
	var child *rehearsalChildStart
	if u.rehearsalExecution != nil {
		init = &u.rehearsalExecution.initialization
		child = &u.rehearsalExecution.child
	} else if u.rehearsalChildStart != nil {
		init = u.rehearsalInitialization
		child = u.rehearsalChildStart
	} else {
		return nil, nil
	}
	if init == nil || child == nil || (u.commandOnly && !u.rehearsalReportRead) || u.purpose != ExecutionPurpose || u.scope != scope.ProductScope || scope.WorldInstanceID != init.world {
		return nil, ErrDenied
	}
	for _, k := range []LockKey{{Rank: 40, IDs: []string{scope.TenantID, scope.ProductID, scope.WorldInstanceID}}, harnessTaskKey(scope.ProductScope, init.root), harnessTaskKey(scope.ProductScope, child.child)} {
		if e := u.RequireLock(k); e != nil {
			return nil, e
		}
	}
	return &domain.RehearsalBasis{RootTaskID: init.root, ActorID: child.actor, Binding: init.binding, Request: init.request}, nil
}

func (s *Store) BindRehearsalChildContext(ctx context.Context, root, child domain.TaskLease, worker string) (out domain.GenerationMaterial, err error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	if s.GenerationContext == nil {
		return out, ErrDenied
	}
	r, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		return out, e
	}
	r.Operation = "HARNESS_CONTEXT"
	r.CommandKey = turnIdentity(child.TaskID, "context")
	r.CommitKey = r.CommandKey
	r.harnessInitializeContext = true
	sel := domain.GenerationSelection{ActorID: r.rehearsalExecution.child.actor, DataSubjectID: child.SubjectID, TargetKind: "PLAYER", TargetID: child.SubjectID}
	_, r.PayloadHash, e = harnessPayload(sel)
	if e != nil {
		return out, e
	}
	_, replay, e := s.barrier(ctx, r, func(tx Tx) error {
		if _, e := s.GenerationContext.FreezeHarnessBindingTx(ctx, tx, *r.WorldScope, child.TaskID, sel); e != nil {
			return e
		}
		var e error
		out, e = s.GenerationContext.BuildGenerationRequestTx(ctx, tx, *r.WorldScope, child.TaskID)
		if e != nil {
			return e
		}
		if e = checkHarnessQuotedRoute(ctx, tx.(*unit), child.ProductScope, child.TaskID, out.Binding); e != nil {
			return e
		}
		return advanceTurn(ctx, tx.(*unit), "CONTEXT_BOUND")
	})
	if e != nil {
		return domain.GenerationMaterial{}, e
	}
	if replay {
		// The original receipt is not a body or context capability. Revalidate the
		// same current graph in a pure owning transaction with no new step/receipt.
		r.Operation = "REHEARSAL_CONTEXT_READ"
		r.CommandKey = turnIdentity(child.TaskID, "context-read")
		r.CommitKey = r.CommandKey
		done := errors.New("rehearsal context inspected")
		_, _, e = s.barrier(ctx, r, func(tx Tx) error {
			var e error
			out, e = s.GenerationContext.BuildGenerationRequestTx(ctx, tx, *r.WorldScope, child.TaskID)
			if e != nil {
				return e
			}
			return done
		})
		if !errors.Is(e, done) {
			return domain.GenerationMaterial{}, e
		}
	}
	return out, nil
}

func (s *Store) rehearsalChildBarrier(ctx context.Context, root, child domain.TaskLease, worker string) (r BarrierRequest, err error) {
	if root.WorldInstanceID != nil || child.WorldInstanceID == nil || root.ProductScope != child.ProductScope || root.SubjectID != root.OwnerSubjectID || child.SubjectID != child.OwnerSubjectID || s.DraftRehearsalOwner == nil || s.DraftRehearsalReader == nil || s.RehearsalInitializer == nil {
		return r, ErrDenied
	}
	g, e := s.rehearsalGates(ctx, root.ProductScope)
	if e != nil {
		return r, e
	}
	var raw, body []byte
	p := &rehearsalExecution{root: root, worker: worker}
	e = s.DB.QueryRowContext(ctx, "SELECT a.binding,i.document,c.input_index,c.actor_id,c.input_source_id,h.quote_id FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN harness_rehearsal_children c ON c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.root_task_id=a.task_id JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND c.child_task_id=? AND a.owner_subject_id=? AND c.world_instance_id=?", root.TenantID, root.ProductID, root.TaskID, child.TaskID, root.SubjectID, *child.WorldInstanceID).Scan(&raw, &body, &p.child.index, &p.child.actor, &p.child.source, &p.child.quote)
	if e != nil {
		return r, e
	}
	p.child.child = child.TaskID
	p.initialization.root = root.TaskID
	p.initialization.world = *child.WorldInstanceID
	p.initialization.release = turnIdentity(root.TaskID, "rehearsal-version")
	if json.Unmarshal(raw, &p.initialization.binding) != nil || generated.Decode("RehearsalRequest", body, &p.initialization.request) != nil {
		return r, ErrDenied
	}

	fail := func(e error) (BarrierRequest, error) { return BarrierRequest{}, e }
	keys, e := s.DraftRehearsalOwner.PlanRehearsalLocks(ctx, root.ProductScope, p.initialization.binding.DraftID)
	if e != nil {
		return fail(e)
	}
	more, e := s.RehearsalInitializer.PlanRehearsalInitialization(ctx, root.ProductScope, root.TaskID, p.initialization.binding, p.initialization.request)
	if e != nil {
		return fail(e)
	}
	keys = append(keys, more...)
	p.keys = map[string]bool{}
	for _, k := range keys {
		p.keys[string(lockBytes(k))] = true
	}
	var v domain.Versions
	if e = s.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", root.TenantID, root.ProductID, *child.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch); e != nil {
		return fail(e)
	}
	scope := domain.Scope{ProductScope: child.ProductScope, WorldInstanceID: *child.WorldInstanceID}
	r = BarrierRequest{ProductScope: child.ProductScope, WorldScope: &scope, Versions: &v, OwnerSubjectID: child.SubjectID, TaskID: child.TaskID, WorkerID: worker, Fence: child.Fence, GateVersions: g, Locks: keys, rehearsalExecution: p}
	return r, nil
}

func (s *Store) checkRehearsalExecution(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalExecution
	if p == nil || r.WorldScope == nil || p.child.child != r.TaskID || p.root.ProductScope != r.ProductScope || p.initialization.world != r.WorldScope.WorldInstanceID {
		return ErrDenied
	}
	rootRequest := BarrierRequest{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, TaskID: p.root.TaskID, rehearsalInitialization: &p.initialization}
	if e := s.checkRehearsalInitialization(ctx, u, rootRequest); e != nil {
		return e
	}
	if e := checkRehearsalChildIdentity(ctx, u, r); e != nil {
		return e
	}
	advanced := 0
	if r.rehearsalNegative != nil && r.rehearsalNegative.applied == u {
		if e := checkRehearsalNegativeApplied(ctx, u.tx, r); e != nil {
			return e
		}
		advanced = 1
	}
	if r.Operation == "HARNESS_TURN_COMMIT" && r.localCandidateRound != nil && r.localCandidateRound.phase == "WORLD_COMMIT" && r.localCandidateRound.used.Load() {
		if e := s.checkRehearsalAppliedCommit(ctx, u, r); e != nil {
			return e
		}
		advanced = 1
	}
	var valid bool
	e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_children c JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id JOIN harness_envelopes e ON e.tenant_id=c.tenant_id AND e.product_id=c.product_id AND e.task_id=c.root_task_id JOIN harness_admin_rehearsals a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.task_id=c.root_task_id JOIN commit_receipts receipt ON receipt.commit_key=c.accepted_receipt WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? AND c.child_task_id=? AND c.world_instance_id=? AND c.actor_id=? AND c.input_source_id=h.input_source_id AND c.input_hash=h.input_hash AND c.root_generation=e.narrative_generation AND a.next_input=c.input_index+? AND a.fsm_state='ISOLATED' AND c.execute_by>UTC_TIMESTAMP(6) AND receipt.operation='REHEARSAL_CHILD_START' AND receipt.task_id=c.root_task_id AND receipt.tenant_id=c.tenant_id AND receipt.product_id=c.product_id AND receipt.owner_subject_id=? AND receipt.payload_hash=h.command_hash AND receipt.phase='COMMIT')", r.ProductScope.TenantID, r.ProductScope.ProductID, p.root.TaskID, r.TaskID, r.WorldScope.WorldInstanceID, p.child.actor, advanced, r.OwnerSubjectID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	var raw []byte
	if e = u.tx.QueryRowContext(ctx, "SELECT binding FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, p.root.TaskID).Scan(&raw); e != nil {
		return e
	}
	var b domain.RehearsalBinding
	if json.Unmarshal(raw, &b) != nil || !reflect.DeepEqual(b, p.initialization.binding) {
		return ErrConflict
	}
	return checkCurrentLease(ctx, u.tx, BarrierRequest{ProductScope: r.ProductScope, TaskID: p.root.TaskID, WorkerID: p.worker, Fence: p.root.Fence})
}

func acquireRehearsalExecutionRoot(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	p := r.rehearsalExecution
	if p == nil || k.IDs[2] != p.root.TaskID {
		return ErrDenied
	}
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT owner_subject_id=? AND task_kind='ADMIN' AND scope_kind='PRODUCT' AND world_instance_id IS NULL AND state='RUNNING' AND cancelled=0 AND fence=? AND lease_owner=? AND lease_expires_at>UTC_TIMESTAMP(6) FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", r.OwnerSubjectID, p.root.Fence, p.worker, r.ProductScope.TenantID, r.ProductScope.ProductID, p.root.TaskID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

func checkRehearsalChildIdentity(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalExecution
	if p == nil || p.child.index < 0 || p.child.index >= len(p.initialization.request.Inputs) || p.child.child != turnIdentity(p.root.TaskID, "rehearsal-child", strconv.Itoa(p.child.index)) || p.child.quote != turnIdentity(p.child.child, "quote") || p.child.source != turnIdentity(p.child.child, "input") || p.initialization.world != turnIdentity(p.root.TaskID, "rehearsal-world") {
		return ErrDenied
	}
	var actor string
	if e := u.tx.QueryRowContext(ctx, "SELECT actor_id FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND slot_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, p.initialization.world, p.initialization.request.Inputs[p.child.index].ActorSlot).Scan(&actor); e != nil {
		return e
	}
	if actor != p.child.actor {
		return ErrReplan
	}
	inputHash := sha256.Sum256([]byte(p.initialization.request.Inputs[p.child.index].Text))
	_, commandHash, e := harnessPayload(struct {
		Root    string
		Index   int
		Binding domain.RehearsalBinding
	}{p.root.TaskID, p.child.index, p.initialization.binding})
	if e != nil {
		return e
	}
	var exact bool
	e = u.tx.QueryRowContext(ctx, "SELECT c.input_hash=? AND h.input_hash=? AND h.command_hash=? AND h.quote_id=? AND h.actor_id=? AND h.accepted_commit_key=? AND c.accepted_receipt=h.accepted_commit_key AND h.original_key=h.accepted_commit_key AND c.input_source_id=? AND c.input_index=? AND c.started_at=e.accepted_at AND c.execute_by=e.execute_by AND j.operation='REHEARSAL_TURN' FROM harness_rehearsal_children c JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id JOIN harness_envelopes e ON e.tenant_id=h.tenant_id AND e.product_id=h.product_id AND e.task_id=h.task_id JOIN registered_jobs j ON j.tenant_id=h.tenant_id AND j.product_id=h.product_id AND j.task_id=h.task_id WHERE c.tenant_id=? AND c.product_id=? AND c.child_task_id=?", inputHash[:], inputHash[:], commandHash[:], p.child.quote, p.child.actor, turnIdentity(p.child.child, "accepted"), p.child.source, p.child.index, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&exact)
	if e != nil {
		return e
	}
	if !exact {
		return ErrDenied
	}
	return nil
}

// Only the fixed candidate owner can have applied this result in this UoW.
// Its full original graph was revalidated under the same locks immediately
// before Apply. Legitimate new derived nodes are not the old prompt graph.
func (s *Store) checkRehearsalAppliedCommit(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.localCandidateRound
	if r.rehearsalExecution == nil || p == nil || p.phase != "WORLD_COMMIT" || !p.used.Load() || r.recoveryEvidence == nil || r.originalQueryService == nil {
		return ErrDenied
	}
	var raw []byte
	var state string
	e := u.tx.QueryRowContext(ctx, "SELECT c.document,h.fsm_state FROM harness_turn_commits c JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.receipt_key=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.CommitKey).Scan(&raw, &state)
	if e != nil {
		return e
	}
	var c domain.CandidateCommit
	if json.Unmarshal(raw, &c) != nil || state != "COMMITTED" || c.ReceiptKey != r.CommitKey || c.Metadata != r.recoveryEvidence.Metadata() || c.Metadata.Identity.AttemptID != p.attempt || c.ResultID != turnIdentity(r.TaskID, "result") {
		return ErrDenied
	}
	var current domain.Versions
	if e = u.tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.WorldScope.WorldInstanceID).Scan(&current.ReleaseID, &current.WorldRevision, &current.ControlVersion, &current.PrivacyEpoch, &current.ViewEpoch); e != nil {
		return e
	}
	if current != c.Versions {
		return ErrDenied
	}
	if s.RehearsalInputOwner == nil {
		return ErrDenied
	}
	if e = s.RehearsalInputOwner.ValidateRehearsalAppliedSourcesTx(ctx, u, *r.WorldScope, r.TaskID, c); e != nil {
		return e
	}
	return checkResultInstallation(ctx, u, r, *r.originalQueryService)
}
