package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

// QueryOriginalTurnCommit resolves world COMMIT knowledge only. It does not
// authorize narrative delivery, result bytes, or a new provider request.
func (s *Store) QueryOriginalTurnCommit(ctx context.Context, scope domain.Scope, task string) (CommitReceipt, error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil {
		return CommitReceipt{}, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	var key string
	e = s.DB.QueryRowContext(ctx, "SELECT c.receipt_key FROM harness_turn_commits c JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.WorldInstanceID, scope.SubjectID).Scan(&key)
	if e != nil {
		return CommitReceipt{}, e
	}
	receipt, e := s.QueryCommitScoped(ctx, scope.ProductScope, &scope, scope.SubjectID, key)
	if e != nil {
		return receipt, e
	}
	row, e := queryReceipt(ctx, s.DB, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	if !receipt.Committed || row.operation != "HARNESS_TURN_COMMIT" || row.phase != "COMMIT" || row.task != task {
		return CommitReceipt{}, ErrConflict
	}
	return receipt, nil
}

// CommitOriginalTurn consumes real locked result evidence through the owning
// adapter. All current authority and program decisions repeat in one WORLD UoW.
func (s *Store) CommitOriginalTurn(ctx context.Context, lease domain.TaskLease, worker string) (CommitReceipt, error) {
	if lease.WorldInstanceID == nil {
		return CommitReceipt{}, ErrDenied
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	if receipt, e := s.QueryOriginalTurnCommit(ctx, scope, lease.TaskID); e == nil {
		return receipt, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return CommitReceipt{}, e
	}
	id, e := s.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return CommitReceipt{}, e
	}
	return s.CommitAttemptTurn(ctx, lease, worker, id.AttemptID)
}
func (s *Store) CommitAttemptTurn(ctx context.Context, lease domain.TaskLease, worker, attempt string) (CommitReceipt, error) {
	if lease.WorldInstanceID == nil || lease.OwnerSubjectID != lease.SubjectID || s.CandidateOwner == nil || s.GenerationContext == nil || s.OriginalResultReader == nil {
		return CommitReceipt{}, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	requested, e := s.AttemptMockIdentity(ctx, scope, lease.TaskID, attempt)
	if e != nil {
		return CommitReceipt{}, e
	}
	if receipt, e := s.QueryOriginalTurnCommit(ctx, scope, lease.TaskID); e == nil {
		var raw []byte
		var committed domain.CandidateCommit
		if e := s.DB.QueryRowContext(ctx, "SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, lease.TaskID).Scan(&raw); e != nil {
			return CommitReceipt{}, e
		}
		if json.Unmarshal(raw, &committed) != nil || committed.Metadata.Identity != requested {
			return CommitReceipt{}, ErrConflict
		}
		return receipt, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return CommitReceipt{}, e
	}
	evidence, e := s.OriginalResultReader.ReadAttemptEvidence(ctx, lease, worker, attempt)
	if e != nil {
		return CommitReceipt{}, e
	}
	if evidence == nil {
		return CommitReceipt{}, ErrDenied
	}
	defer evidence.Close()
	body, meta := evidence.Bytes(), evidence.Metadata()
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != meta.BodyHash || !validOriginalUsageMetadata(minimalOriginalMetadata(meta)) {
		return CommitReceipt{}, ErrDenied
	}
	r, e := s.GenerationContext.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	ctx, service, queryRelease, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return CommitReceipt{}, e
	}
	defer queryRelease()
	if service == nil {
		return CommitReceipt{}, ErrDenied
	}
	r.originalQueryService = service
	r.recoveryEvidence = evidence
	if e = s.prepareOriginalResultBarrier(ctx, &r, meta.Identity.AttemptID); e != nil {
		return CommitReceipt{}, e
	}
	result := turnIdentity(r.TaskID, "result")
	serviceID := turnIdentity(r.TaskID, "service")
	locks, e := s.CandidateOwner.PlanOriginalCandidate(ctx, scope, r.TaskID, result, body)
	if e != nil {
		return CommitReceipt{}, e
	}
	r.Locks = append(r.Locks, locks...)
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}})
	costs := []domain.MockResultMetadata{meta}
	var recovery *domain.RecoveryPlan
	var recoveryRaw []byte
	recoveryError := s.DB.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, r.TaskID).Scan(&recoveryRaw)
	if recoveryError == nil {
		var plan domain.RecoveryPlan
		if json.Unmarshal(recoveryRaw, &plan) != nil || plan.AttemptID != meta.Identity.AttemptID {
			return CommitReceipt{}, ErrDenied
		}
		recovery = &plan
		costs = []domain.MockResultMetadata{plan.Original, meta}
		r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, r.TaskID, plan.Original.Identity.AttemptID, plan.Original.Identity.AuthorizationID}})
	} else if !errors.Is(recoveryError, sql.ErrNoRows) {
		return CommitReceipt{}, recoveryError
	}

	rows, e := s.DB.QueryContext(ctx, "SELECT kind,scope_key,account_id,reservation_id,amount FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY kind,scope_key,account_id,reservation_id LIMIT 9", scope.TenantID, scope.ProductID, r.TaskID)
	if e != nil {
		return CommitReceipt{}, e
	}
	holds := []Reservation{}
	for rows.Next() {
		var h Reservation
		if e = rows.Scan(&h.Kind, &h.ScopeKey, &h.AccountID, &h.ReservationID, &h.Amount); e != nil {
			rows.Close()
			return CommitReceipt{}, e
		}
		holds = append(holds, h)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return CommitReceipt{}, e
	}
	parentHoldCount := 4
	if r.rehearsalExecution != nil {
		parentHoldCount = 0
	}
	if len(holds) != parentHoldCount+2*len(costs) {
		return CommitReceipt{}, ErrDenied
	}
	r.Locks = append(r.Locks, holdLocks(holds)...)
	if r.rehearsalExecution == nil {
		r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "service_events", IDs: []string{scope.TenantID, scope.ProductID, serviceID}})
		for _, h := range holds {
			r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "postings", IDs: []string{scope.TenantID, scope.ProductID, turnIdentity(r.TaskID, "posting", h.ReservationID)}})
		}
	}
	r.Operation = "HARNESS_TURN_COMMIT"
	r.CommandKey = turnIdentity(r.TaskID, "world-commit")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return CommitReceipt{}, e
	}
	r.localCandidateRound, e = s.prepareCandidateRound(ctx, r, meta, "WORLD_COMMIT")
	if e != nil {
		return CommitReceipt{}, e
	}
	if r.localCandidateRound == nil {
		return s.QueryOriginalTurnCommit(ctx, scope, r.TaskID)
	}
	receipt, e := s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := useCandidateRound(ctx, tx, r); e != nil {
			return e
		}
		if e := checkResultInstallation(ctx, tx, r, *service); e != nil {
			return e
		}
		if e := s.GenerationContext.ValidateResultSourcesTx(ctx, tx, scope, r.TaskID, meta, r.recoveryEvidence); e != nil {
			return e
		}
		var raw []byte
		var approval domain.CandidateApproval
		if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_candidate_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&raw); e != nil {
			return e
		}
		if json.Unmarshal(raw, &approval) != nil || approval.Metadata != meta || approval.ResultID != result {
			return ErrConflict
		}
		// World revision may change without touching a relevant resource; a pure
		// semantic revalidation is sufficient. Relevant differences cannot commit.
		resources, e := s.CandidateOwner.EvaluateOriginalCandidateTx(ctx, tx, scope, r.TaskID, meta.Identity.AttemptID, body)
		if e != nil {
			bindCandidateConflict(r, e)
			return e
		}
		if !reflect.DeepEqual(resources, approval.Resources) {
			return ErrConflict
		}
		u := tx.(*unit)
		var eligible bool
		if e = u.tx.QueryRowContext(ctx, "SELECT e.state='ACTIVE' AND e.execute_by>UTC_TIMESTAMP(6) AND e.settle_by>UTC_TIMESTAMP(6) AND e.calls_allocated=? AND q.document->'$.ServiceAtoms'=0 AND q.document->'$.SupplierAtoms'=0 FROM harness_envelopes e JOIN harness_quotes q ON q.tenant_id=e.tenant_id AND q.product_id=e.product_id AND q.quote_id=e.quote_id WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", len(costs), scope.TenantID, scope.ProductID, r.TaskID).Scan(&eligible); e != nil {
			return e
		}
		if !eligible {
			return ErrDenied
		}
		for _, cost := range costs {
			var raw []byte
			var saved domain.MockResultMetadata
			if !validOriginalUsageMetadata(minimalOriginalMetadata(cost)) || cost.State != "SUCCEEDED" {
				return ErrDenied
			}
			if e = u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND provider_state='SUCCEEDED'", scope.TenantID, scope.ProductID, r.TaskID, cost.Identity.AttemptID).Scan(&raw); e != nil {
				return e
			}
			if json.Unmarshal(raw, &saved) != nil || saved != cost {
				return ErrConflict
			}
		}
		supplierHolds := map[string]int{}
		parentHolds := map[string]int{}
		for _, h := range holds {
			var amount int64
			var state, purpose string
			if e = u.tx.QueryRowContext(ctx, "SELECT amount,status,purpose FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind=? AND scope_key=? AND account_id=? AND reservation_id=?", scope.TenantID, scope.ProductID, r.TaskID, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID).Scan(&amount, &state, &purpose); e != nil {
				return e
			}
			want := "PARENT"
			if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
				want = purpose
				found := false
				for _, cost := range costs {
					if cost.Identity.AttemptID == purpose {
						found = true
					}
				}
				if !found {
					return ErrDenied
				}
				supplierHolds[purpose+"/"+h.Kind]++
			}
			if want == "PARENT" {
				parentHolds[h.Kind]++
			}
			if amount != 0 || h.Amount != 0 || state != "HELD" || purpose != want {
				return ErrDenied
			}
		}
		for _, cost := range costs {
			for _, kind := range []string{"supplier_global", "supplier_tenant"} {
				if supplierHolds[cost.Identity.AttemptID+"/"+kind] != 1 {
					return ErrDenied
				}
			}
		}
		if r.rehearsalExecution == nil {
			for _, kind := range []string{"customer", "project", "world", "platform_risk"} {
				if parentHolds[kind] != 1 {
					return ErrDenied
				}
			}
		} else if len(parentHolds) != 0 {
			return ErrDenied
		}
		committed, e := s.CandidateOwner.ApplyOriginalCandidateTx(ctx, tx, scope, r.TaskID, meta.Identity.AttemptID, result, *r.Versions, body)
		if e != nil {
			return e
		}
		committed.Metadata = meta
		if recovery != nil {
			original := recovery.Original
			committed.OriginalDependency = &original
		}
		committed.ReceiptKey = r.CommitKey
		if e = u.tx.QueryRowContext(ctx, "SELECT narrative_generation FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, r.TaskID).Scan(&committed.NarrativeGeneration); e != nil {
			return e
		}
		if len(committed.EventIDs) == 0 {
			if _, e = u.tx.ExecContext(ctx, "INSERT INTO approved_results(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES(?,?,?,?,?,'COMMITTED',?)", scope.TenantID, scope.ProductID, result, r.TaskID, scope.SubjectID, committed.Versions.WorldRevision); e != nil {
				return e
			}
			if _, e = u.tx.ExecContext(ctx, "INSERT INTO outbox(tenant_id,product_id,record_id,task_id,owner_subject_id,consumer_id,message_id,status,revision) VALUES(?,?,?,?,?,'runtime',?,'PENDING',?)", scope.TenantID, scope.ProductID, result, r.TaskID, scope.SubjectID, result, committed.Versions.WorldRevision); e != nil {
				return e
			}
		}
		if r.rehearsalExecution != nil {
			// A child commits isolated facts only. Its original supplier holds and
			// usage remain under the immutable root for the one report settlement.
			// Even Task4's action outbox cannot become ordinary runtime delivery.
			if _, e = u.tx.ExecContext(ctx, "UPDATE outbox SET consumer_id=? WHERE tenant_id=? AND product_id=? AND task_id=? AND record_id=?", turnIdentity(r.rehearsalExecution.root.TaskID, "rehearsal-consumer"), scope.TenantID, scope.ProductID, r.TaskID, result); e != nil {
				return e
			}
		} else {
			if e = bindOriginalServiceMeteringTx(ctx, u, r, serviceID, costs); e != nil {
				return e
			}
			for _, h := range holds {
				status := originalCostHoldState(h, costs)
				if status == "HELD" {
					continue
				}
				if _, e = u.tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", status, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
					return e
				}
				if _, e = u.tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID); e != nil {
					return e
				}
				if _, e = u.tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,0,?,1)", scope.TenantID, scope.ProductID, turnIdentity(r.TaskID, "posting", h.ReservationID), r.TaskID, scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID, status); e != nil {
					return e
				}
			}
		}
		raw, e = json.Marshal(committed)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turn_commits VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, r.TaskID, result, r.CommitKey, raw); e != nil {
			return e
		}
		if e = advanceTurn(ctx, u, "COMMITTED"); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='COMMITTED' WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, r.TaskID); e != nil {
			return e
		}
		if p := r.rehearsalExecution; p != nil {
			// Ordered progress is part of this exact child business COMMIT. It
			// cannot advance on validation, provider success or an unknown outcome.
			res, e := u.tx.ExecContext(ctx, "UPDATE harness_admin_rehearsals SET next_input=next_input+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND next_input=? AND fsm_state='ISOLATED'", scope.TenantID, scope.ProductID, p.root.TaskID, p.child.index)
			if e != nil {
				return e
			}
			n, e := res.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return ErrDenied
			}
		}
		if len(committed.EventIDs) == 0 {
			return tx.SetTaskState(ctx, "COMMITTED")
		}
		// Task4 ApplyActions already marks the same task in this still-uncommitted
		// UoW. Its state and every later memory/service write roll back together.
		return nil
	})
	return receipt, s.recordCandidateRollback(ctx, r, meta, e)
}
