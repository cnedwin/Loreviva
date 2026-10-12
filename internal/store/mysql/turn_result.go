package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"sync/atomic"
)

// OriginalSendPermit exists only after a fresh marker's known COMMIT. It cannot
// be serialized, reconstructed from a receipt, or minted by a caller.
type OriginalSendPermit struct {
	store          *Store
	dispatch       DispatchRequest
	material       domain.GenerationMaterial
	candidate      *domain.DraftCandidateMaterial
	taken, replied atomic.Bool
}

func (a DispatchAuthorization) OriginalSend() *OriginalSendPermit { return a.send }
func (p *OriginalSendPermit) Take(ctx context.Context, s *Store) (DispatchRequest, domain.GenerationMaterial, error) {
	if p == nil || p.candidate != nil {
		return DispatchRequest{}, domain.GenerationMaterial{}, ErrDenied
	}
	return p.take(ctx, s)
}
func (p *OriginalSendPermit) TakeCandidate(ctx context.Context, s *Store) (DispatchRequest, domain.DraftCandidateMaterial, error) {
	if p == nil || p.candidate == nil {
		return DispatchRequest{}, domain.DraftCandidateMaterial{}, ErrDenied
	}
	d, _, e := p.take(ctx, s)
	if e != nil {
		return DispatchRequest{}, domain.DraftCandidateMaterial{}, e
	}
	m := *p.candidate
	m.Canonical = append([]byte(nil), m.Canonical...)
	return d, m, nil
}
func (p *OriginalSendPermit) take(ctx context.Context, s *Store) (DispatchRequest, domain.GenerationMaterial, error) {
	if p == nil || p.store != s || !p.taken.CompareAndSwap(false, true) {
		return DispatchRequest{}, domain.GenerationMaterial{}, ErrDenied
	}
	d := cloneOriginalDispatch(p.dispatch)
	release, e := s.prepareRecoveryDispatch(ctx, &d)
	if e != nil {
		return DispatchRequest{}, domain.GenerationMaterial{}, e
	}
	defer release()
	ctx, query, queryRelease, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return DispatchRequest{}, domain.GenerationMaterial{}, e
	}
	defer queryRelease()
	ctx, settlement, settlementRelease, e := s.turnSettlementAdmission(ctx, d)
	if e != nil {
		return DispatchRequest{}, domain.GenerationMaterial{}, e
	}
	defer settlementRelease()
	d.originalQueryService = query
	d.originalSettlementService = settlement
	r := d.BarrierRequest
	r.phase = ""
	r.Operation = "HARNESS_SEND_CONSUME"
	r.CommandKey = turnIdentity(d.AttemptID, "send-consume")
	r.CommitKey = r.CommandKey
	r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{d.CommitKey}})
	inspected := errors.New("current original send permit inspected")
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		tx := port.(*unit).tx
		if e := checkSupplierDebt(ctx, tx, d.Reservations); e != nil {
			return e
		}
		if e := validateReservations(ctx, tx, d); e != nil {
			return e
		}
		a, e := readAttempt(ctx, tx, d)
		if e != nil {
			return e
		}
		if !a.matches(d) || !a.started.Valid {
			return ErrDenied
		}
		receipt, e := queryReceipt(ctx, tx, d.CommitKey)
		if e != nil {
			return e
		}
		if !receipt.matches(d.BarrierRequest) {
			return ErrConflict
		}
		return inspected
	})
	if !errors.Is(e, inspected) {
		if e == nil {
			e = ErrDenied
		}
		return DispatchRequest{}, domain.GenerationMaterial{}, e
	}
	return cloneOriginalDispatch(p.dispatch), p.material, nil
}

func cloneOriginalDispatch(d DispatchRequest) DispatchRequest {
	if d.WorldScope != nil {
		v := *d.WorldScope
		d.WorldScope = &v
	}
	if d.Versions != nil {
		v := *d.Versions
		d.Versions = &v
	}
	d.Reservations = append([]Reservation(nil), d.Reservations...)
	keys := make([]LockKey, len(d.Locks))
	for i, k := range d.Locks {
		keys[i] = k
		keys[i].IDs = append([]string(nil), k.IDs...)
	}
	d.Locks = keys
	return d
}
func mockIdentity(d DispatchRequest) domain.MockRequestIdentity {
	if d.candidateExecution && d.WorldScope == nil {
		return domain.MockRequestIdentity{Kind: "CANDIDATE", AuthorizationID: d.AuthorizationID, Scope: domain.Scope{ProductScope: d.ProductScope}, TaskID: d.TaskID, AttemptID: d.AttemptID, RequestKey: d.RequestKey, RequestHash: hex.EncodeToString(d.RequestHash[:]), RouteID: d.Route, QuoteID: d.HarnessQuoteID, MarkerKey: d.CommitKey}
	}
	return domain.MockRequestIdentity{AuthorizationID: d.AuthorizationID, Scope: *d.WorldScope, TaskID: d.TaskID, AttemptID: d.AttemptID, RequestKey: d.RequestKey, RequestHash: hex.EncodeToString(d.RequestHash[:]), RouteID: d.Route, QuoteID: d.HarnessQuoteID, MarkerKey: d.CommitKey}
}

func (s *Store) BindOriginalResultBody(ctx context.Context, d DispatchRequest, storage, hash string) error {
	if s.GenerationContext == nil || d.WorldScope == nil {
		return ErrDenied
	}
	ctx, service, release, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return e
	}
	defer release()
	if service == nil {
		return ErrDenied
	}
	id, e := s.AttemptMockIdentity(ctx, *d.WorldScope, d.TaskID, d.AttemptID)
	if e != nil {
		return e
	}
	if id != mockIdentity(d) {
		return ErrConflict
	}
	r := d.BarrierRequest
	r.harnessRecoveryAttempt = ""
	r.recoveryEvidence = nil
	if d.Slot == 1 {
		if s.OriginalResultReader == nil {
			return ErrDenied
		}
		lease := domain.TaskLease{ProductScope: d.ProductScope, WorldInstanceID: &d.WorldScope.WorldInstanceID, OwnerSubjectID: d.OwnerSubjectID, TaskID: d.TaskID, Fence: d.Fence}
		evidence, err := s.OriginalResultReader.ReadOriginalEvidence(ctx, lease, d.WorkerID)
		if err != nil {
			return err
		}
		defer evidence.Close()
		r.recoveryEvidence = evidence
	}
	r.originalQueryService = service
	if e := s.prepareOriginalResultBarrier(ctx, &r, d.AttemptID); e != nil {
		return e
	}
	r.phase = ""
	if len(mustHash(storage)) != 16 || len(mustHash(hash)) != 32 {
		return ErrDenied
	}
	r.harnessResultStorage = storage
	r.Operation = "HARNESS_RESULT_STAGE"
	r.CommandKey = storage
	r.CommitKey = turnIdentity(d.AttemptID, "result-stage", storage)
	r.Locks = append(r.Locks, LockKey{Rank: 90, Kind: "asset", IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, "staging." + storage}})
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, d.AttemptID, d.AuthorizationID}})
	_, r.PayloadHash, _ = harnessPayload([]string{d.AttemptID, storage, hash})
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := checkResultInstallation(ctx, tx, r, *service); e != nil {
			return e
		}
		return s.GenerationContext.BindResultSourcesTx(ctx, tx, *d.WorldScope, d.TaskID, d.AttemptID, storage, hash, r.recoveryEvidence)
	})
	return e
}

// Fixed evidence-only callers retain all original versions except the current
// resource revision; acquire rechecks that revision under the normal world lock.
func (s *Store) prepareOriginalResultBarrier(ctx context.Context, r *BarrierRequest, attempt string) error {
	if r.WorldScope == nil || r.Versions == nil || attempt == "" {
		return ErrDenied
	}
	v := *r.Versions
	if e := s.DB.QueryRowContext(ctx, "SELECT world_revision FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.WorldScope.WorldInstanceID).Scan(&v.WorldRevision); e != nil {
		return e
	}
	r.Versions = &v
	r.harnessOriginalResultAttempt = attempt
	r.harnessRecoveryAttempt = ""
	var raw []byte
	e := s.DB.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, attempt).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	var plan domain.RecoveryPlan
	if json.Unmarshal(raw, &plan) != nil || plan.AttemptID != attempt {
		return ErrDenied
	}
	return s.planRecoveryEvidence(ctx, r, plan, r.recoveryEvidence)
}
func (u *unit) BindHarnessResult(ctx context.Context, s domain.Scope, task, attempt string, b domain.ResultSourceBinding) error {
	if u.resultStorage == "" || u.resultStorage != b.Storage {
		return ErrDenied
	}
	if e := u.AssertWorldTask(s, task); e != nil {
		return e
	}
	if len(b.Nodes) == 0 || len(b.Nodes) > 64 || b.SubjectID != s.SubjectID || len(mustHash(b.BodyHash)) != 32 {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "asset", IDs: []string{s.TenantID, s.ProductID, "staging." + b.Storage}}); e != nil {
		return e
	}
	var count int
	if e := u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, b.SubjectID).Scan(&count); e != nil {
		return e
	}
	if count >= 1000 {
		return ErrDenied
	}
	var valid bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attempts a JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=? AND a.send_started_at IS NOT NULL AND e.state='ACTIVE' AND e.execute_by>UTC_TIMESTAMP(6))", s.TenantID, s.ProductID, task, attempt).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return e
	}
	_, hash, e := harnessPayload(b)
	if e != nil {
		return e
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO file_staging(tenant_id,product_id,storage_key,owner_subject_id) VALUES(?,?,?,?)", s.TenantID, s.ProductID, b.Storage, s.SubjectID); e != nil {
		return e
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_result_sources(tenant_id,product_id,task_id,attempt_id,storage_key,document) VALUES(?,?,?,?,?,?)", s.TenantID, s.ProductID, task, attempt, b.Storage, raw); e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO memory_asset_lineage(tenant_id,product_id,storage_key,render_id,world_instance_id,data_subject_id,actor_id,node_id,node_revision,source_epoch,lineage_hash) VALUES(?,?,?,?,?,?,?,?,1,?,?)", s.TenantID, s.ProductID, b.Storage, turnIdentity(attempt, "result"), s.WorldInstanceID, b.SubjectID, b.ActorID, turnIdentity(attempt, "result-node"), b.Epoch, hex.EncodeToString(hash[:]))
	return e
}
