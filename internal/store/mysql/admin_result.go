package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"strconv"
)

// Candidate staging is a fixed pending-result path. It does not authorize a
// terminal asset or accept caller supplied lineage, scope, route or cost.
type candidateResultGuard struct {
	dispatch      DispatchRequest
	storage, hash string
}

func (s *Store) BindCandidateResultStage(ctx context.Context, d DispatchRequest, storage, hash string) error {
	if !d.candidateExecution || d.WorldScope != nil || len(mustHash(storage)) != 16 || len(mustHash(hash)) != 32 {
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
	r := candidateResultBarrier(d, storage, hash)
	r.originalQueryService = service
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := checkCandidateMarker(ctx, u, d, *service); e != nil {
			return e
		}
		m, e := s.candidateMaterialTx(ctx, u, r)
		if e != nil {
			return e
		}
		m.Canonical = nil
		raw, e := json.Marshal(m)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO file_staging(tenant_id,product_id,storage_key,owner_subject_id) VALUES(?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, storage, d.OwnerSubjectID); e != nil {
			return e
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_candidate_results(tenant_id,product_id,task_id,attempt_id,storage_key,body_hash,source_binding) VALUES(?,?,?,?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, storage, hash, raw)
		return e
	})
	return e
}
func candidateResultBarrier(d DispatchRequest, storage, hash string) BarrierRequest {
	r := d.BarrierRequest
	r.candidateOriginalRead = d.Slot == 0
	r.candidateResultGuard = &candidateResultGuard{dispatch: d, storage: storage, hash: hash}
	r.phase = ""
	r.Operation = "CANDIDATE_RESULT_STAGE"
	r.CommandKey = turnIdentity(d.AttemptID, "body", storage)
	r.CommitKey = r.CommandKey
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, d.AuthorizationID}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, "staging." + storage}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{d.CommitKey}})
	_, r.PayloadHash, _ = harnessPayload([]string{d.AttemptID, storage, hash})
	return r
}
func checkCandidateMarker(ctx context.Context, u *unit, d DispatchRequest, service domain.QueryServiceIdentity) error {
	if !d.candidateExecution || d.WorldScope != nil || !candidateAttemptID(d.TaskID, d.AttemptID) {
		return ErrDenied
	}
	a, e := readAttempt(ctx, u.tx, d)
	if e != nil {
		return e
	}
	if !a.matches(d) || !a.started.Valid || !a.marker.Valid || a.marker.String != d.CommitKey {
		return ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, d.CommitKey)
	if e != nil {
		return e
	}
	marker := d.BarrierRequest
	marker.phase = "SEND"
	// prepareDispatch owns the actual original send preimage.
	prepared, e := prepareDispatch(d)
	if e != nil {
		return e
	}
	marker.PayloadHash = prepared.PayloadHash
	if !receipt.matches(marker) {
		return ErrConflict
	}
	var raw []byte
	var installation, serviceID, policy string
	if e = u.tx.QueryRowContext(ctx, "SELECT request_identity,installation_id,service_id,policy FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&raw, &installation, &serviceID, &policy); e != nil {
		return e
	}
	var id domain.MockRequestIdentity
	if json.Unmarshal(raw, &id) != nil || id != mockIdentity(d) || installation != service.Installation || serviceID != service.ServiceID || policy != service.Policy {
		return ErrDenied
	}
	var frozen, accepted []byte
	if e = u.tx.QueryRowContext(ctx, "SELECT a.document,c.binding FROM harness_candidate_attempts a JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&frozen, &accepted); e != nil {
		return e
	}
	var original, intake domain.DraftCandidateMaterial
	if json.Unmarshal(accepted, &intake) != nil {
		return ErrDenied
	}
	if d.Slot == 0 {
		if json.Unmarshal(frozen, &original) != nil || !sameCandidateBinding(original, intake) {
			return ErrDenied
		}
	} else {
		var snapshot candidateRepairSnapshot
		var plan candidateRepairPlan
		var raw []byte
		if e := u.tx.QueryRowContext(ctx, "SELECT document FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&raw); e != nil {
			return e
		}
		if json.Unmarshal(frozen, &snapshot) != nil || json.Unmarshal(raw, &plan) != nil || !sameCandidateBinding(snapshot.Base, intake) || !sameCandidateBinding(snapshot.Base, plan.Base) || !sameCandidateBinding(snapshot.Binding, plan.Binding) || snapshot.Original != plan.Original || snapshot.ApprovalID != plan.ApprovalID {
			return ErrDenied
		}
		original = snapshot.Binding
	}
	if mustArrayHash(original.RequestHash) != d.RequestHash || original.RouteHash != d.Route {
		return ErrDenied
	}
	return nil
}

func sameCandidateBinding(a, b domain.DraftCandidateMaterial) bool {
	if len(a.Canonical) != 0 || len(b.Canonical) != 0 {
		return false
	}
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && bytes.Equal(x, y)
}
func (s *Store) InspectCandidateResultStage(ctx context.Context, d DispatchRequest, storage, hash string) error {
	ctx, service, release, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return e
	}
	defer release()
	if service == nil {
		return ErrDenied
	}
	r := candidateResultBarrier(d, storage, hash)
	r.originalQueryService = service
	r.Operation = "CANDIDATE_RESULT_INSPECT"
	r.CommandKey = turnIdentity(d.AttemptID, "inspect", storage)
	r.CommitKey = r.CommandKey
	inspected := errors.New("candidate pending body inspected")
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if _, e := s.checkCandidateResultTx(ctx, u, d, storage, hash, *service); e != nil {
			return e
		}
		return inspected
	})
	if errors.Is(e, inspected) {
		return nil
	}
	return e
}

func (s *Store) checkCandidateResultTx(ctx context.Context, u *unit, d DispatchRequest, storage, hash string, service domain.QueryServiceIdentity) (domain.DraftCandidateMaterial, error) {
	var zero domain.DraftCandidateMaterial
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "asset", IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, "staging." + storage}}); e != nil {
		return zero, e
	}
	if e := checkCandidateMarker(ctx, u, d, service); e != nil {
		return zero, e
	}
	var raw []byte
	var actualHash string
	if e := u.tx.QueryRowContext(ctx, "SELECT r.body_hash,r.source_binding FROM harness_candidate_results r JOIN file_staging f USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND r.delete_intent=0 AND f.state='ACTIVE'", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, storage).Scan(&actualHash, &raw); e != nil {
		return zero, e
	}
	var bound domain.DraftCandidateMaterial
	if json.Unmarshal(raw, &bound) != nil || actualHash != hash {
		return zero, ErrDenied
	}
	readBarrier := d.BarrierRequest
	readBarrier.originalQueryService = &service
	readBarrier.candidateOriginalRead = d.Slot == 0
	current, e := s.candidateMaterialTx(ctx, u, readBarrier)
	if e != nil {
		return zero, e
	}
	current.Canonical = nil
	if !sameCandidateBinding(current, bound) {
		return zero, ErrDenied
	}
	return current, nil
}

func (s *Store) CandidateMockIdentity(ctx context.Context, scope domain.ProductScope, task string) (domain.MockRequestIdentity, error) {
	return s.CandidateAttemptMockIdentity(ctx, scope, task, turnIdentity(task, "candidate-attempt", "0"))
}
func (s *Store) CandidateAttemptMockIdentity(ctx context.Context, scope domain.ProductScope, task, attempt string) (domain.MockRequestIdentity, error) {
	var out domain.MockRequestIdentity
	var raw []byte
	if scope.Validate() != nil || domain.ValidateID(task) != nil || !candidateAttemptID(task, attempt) {
		return out, ErrDenied
	}
	e := s.DB.QueryRowContext(ctx, "SELECT q.request_identity FROM harness_original_queries q JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.owner_subject_id=? AND a.attempt_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID, attempt).Scan(&raw)
	if e != nil {
		return out, e
	}
	if json.Unmarshal(raw, &out) != nil || out.Kind != "CANDIDATE" || out.Scope.ProductScope != scope || out.Scope.WorldInstanceID != "" || out.TaskID != task || out.AttemptID != attempt {
		return domain.MockRequestIdentity{}, ErrDenied
	}
	return out, nil
}

// Readable metadata here is only for the fixed, currently executing Files path.
// It is not the future S55 terminal reader and produces no receipt or FSM step.
func (s *Store) PendingCandidateBody(ctx context.Context, d DispatchRequest) (domain.MockResultMetadata, error) {
	var zero domain.MockResultMetadata
	if s.ProviderEvidence == nil {
		return zero, ErrDenied
	}
	id, e := s.CandidateAttemptMockIdentity(ctx, d.ProductScope, d.TaskID, d.AttemptID)
	if e != nil || id != mockIdentity(d) {
		return zero, fmt.Errorf("candidate pending identity mismatch: %w", ErrDenied)
	}
	m, e := s.ProviderEvidence.OriginalMetadata(ctx, id)
	if e != nil {
		return zero, e
	}
	if m.State != "SUCCEEDED" || !m.BodyAvailable || m.Identity != id {
		return zero, fmt.Errorf("candidate pending provider incomplete: %w", ErrDenied)
	}
	if e = s.InspectCandidateResultStage(ctx, d, m.BodyStorage, m.BodyHash); e != nil {
		return zero, e
	}
	return m, nil
}

func (u *unit) KeepCandidateStage(ctx context.Context, scope domain.ProductScope, key string) (bool, error) {
	if !u.commandOnly || u.scope != scope || len(mustHash(key)) != 16 {
		return false, ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, "staging." + key}}); e != nil {
		return false, e
	}
	var live bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_candidate_results r JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) JOIN content_drafts d ON d.tenant_id=c.tenant_id AND d.product_id=c.product_id AND d.draft_id=c.draft_id WHERE r.tenant_id=? AND r.product_id=? AND r.storage_key=? AND r.delete_intent=0 AND d.deleted=0 AND NOT EXISTS(SELECT 1 FROM harness_candidate_results sibling WHERE sibling.tenant_id=r.tenant_id AND sibling.product_id=r.product_id AND sibling.task_id=r.task_id AND sibling.delete_intent=1))", scope.TenantID, scope.ProductID, key).Scan(&live); e != nil {
		return false, e
	}
	if live {
		if u.candidateSourcesOwner == nil {
			return false, ErrDenied
		}
		bound, e := candidateStageBinding(ctx, u.tx, scope, key)
		if e != nil {
			return false, e
		}
		// Legacy incomplete evidence is not reconstructed or retroactively used
		// to authorize a source read. Whole-draft deletion still closes it above.
		if len(bound.Sources) == 0 {
			return true, nil
		}
		live, e = u.candidateSourcesOwner.CandidateSourcesLiveTx(ctx, u, scope, bound)
		if e != nil {
			return false, e
		}
		if live {
			return true, nil
		}
	}
	keys, task, asset, e := candidateStageClosure(ctx, u.tx, scope, key)
	if e != nil {
		return false, e
	}
	for _, k := range keys {
		if e = u.RequireLock(k); e != nil {
			return false, e
		}
	}
	if task == "" {
		return false, nil
	}
	// The active result and original dependency are one derivation closure.
	// All asset/staging locks were acquired in the normal binary lock order.
	if _, e = u.tx.ExecContext(ctx, "UPDATE harness_candidate_results SET delete_intent=1 WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task); e != nil {
		return false, e
	}
	if asset != "" {
		_, e = u.tx.ExecContext(ctx, "UPDATE assets SET state='DELETED' WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, asset)
	}
	return false, e
}

// Planning grants no reading or deleting authority. KeepCandidateStage repeats
// the exact closure inside the cleanup transaction and rejects missing locks.
func (s *Store) CandidateStageCleanupLocks(ctx context.Context, scope domain.ProductScope, key string) ([]LockKey, error) {
	keys, task, _, e := candidateStageClosure(ctx, s.DB, scope, key)
	if e != nil || task == "" {
		return keys, e
	}
	if s.DraftCandidateOwner == nil {
		return nil, ErrDenied
	}
	bound, e := candidateStageBinding(ctx, s.DB, scope, key)
	if e != nil {
		return nil, e
	}
	if len(bound.Sources) == 0 {
		return keys, nil
	}
	sourceKeys, e := s.DraftCandidateOwner.PlanCandidateHistoryLocks(ctx, scope, bound)
	if e != nil {
		return nil, e
	}
	return append(keys, sourceKeys...), nil
}
func candidateStageBinding(ctx context.Context, q rowQuerier, scope domain.ProductScope, key string) (domain.DraftCandidateMaterial, error) {
	var raw []byte
	var out domain.DraftCandidateMaterial
	e := q.QueryRowContext(ctx, "SELECT c.binding FROM harness_candidate_results r JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE r.tenant_id=? AND r.product_id=? AND r.storage_key=? AND c.owner_subject_id=?", scope.TenantID, scope.ProductID, key, scope.SubjectID).Scan(&raw)
	if e != nil {
		return out, e
	}
	if json.Unmarshal(raw, &out) != nil {
		return out, ErrDenied
	}
	return out, nil
}
func candidateStageClosure(ctx context.Context, q rowQuerier, scope domain.ProductScope, key string) ([]LockKey, string, string, error) {
	var task string
	e := q.QueryRowContext(ctx, "SELECT task_id FROM harness_candidate_results WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, key).Scan(&task)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, "", "", nil
	}
	if e != nil {
		return nil, "", "", e
	}
	keys := []LockKey{}
	for slot := 0; slot < 2; slot++ {
		var storage string
		e = q.QueryRowContext(ctx, "SELECT storage_key FROM harness_candidate_results WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, task, turnIdentity(task, "candidate-attempt", strconv.Itoa(slot))).Scan(&storage)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, "", "", e
		}
		keys = append(keys, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, "staging." + storage}})
	}
	var asset string
	var count int
	e = q.QueryRowContext(ctx, "SELECT COUNT(DISTINCT asset_id),COALESCE(MIN(asset_id),'') FROM harness_candidate_results WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&count, &asset)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, "", "", e
	}
	if asset != "" {
		if count != 1 || asset != turnIdentity(task, "candidate-asset") {
			return nil, "", "", ErrConflict
		}
		keys = append(keys, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, asset}})
	}
	return keys, task, asset, nil
}
