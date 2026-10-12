package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"sort"
)

func prepareDispatch(d DispatchRequest) (DispatchRequest, error) {
	if d.candidateExecution {
		a := turnIdentity(d.TaskID, "candidate-attempt", fmt.Sprint(d.Slot))
		if d.WorldScope != nil || d.Slot > 1 || d.candidateAttempt != a || d.AttemptID != a || d.AuthorizationID != turnIdentity(a, "authorization") || d.RequestKey != turnIdentity(a, "request") {
			return d, ErrDenied
		}
	}
	if d.candidateExecution {
		expected := map[string]Reservation{}
		for _, a := range r0CandidateAccounts(d.ProductScope, "") {
			if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
				a.ReservationID = turnIdentity(d.AttemptID, "supplier", a.Kind)
				expected[a.Kind] = a
			}
		}
		if len(d.Reservations) != 2 {
			return d, ErrDenied
		}
		for _, a := range d.Reservations {
			want, ok := expected[a.Kind]
			if !ok || a != want {
				return d, ErrDenied
			}
			delete(expected, a.Kind)
		}
	}
	d.harnessDispatchRoute = d.Route
	d.harnessDispatchHash = d.RequestHash
	if d.Purpose != ExecutionPurpose {
		return d, ErrDenied
	}
	if e := validateBarrier(d.BarrierRequest); e != nil {
		return d, e
	}
	for _, id := range []string{d.AttemptID, d.AuthorizationID, d.Route} {
		if e := domain.ValidateID(id); e != nil {
			return d, e
		}
	}
	if e := domain.ValidateIdempotencyKey(d.RequestKey); e != nil {
		return d, e
	}
	// The extra slot requires the private owning plan and live original-body
	// evidence. Both boundaries repeat the program reason and actual input.
	if d.Slot > 1 || d.Slot == 1 && (d.HarnessQuoteID == "" || d.harnessRecoveryAttempt != d.AttemptID || d.recoveryEvidence == nil) || len(d.Reservations) == 0 {
		return d, ErrDenied
	}
	d.Reservations = append([]Reservation(nil), d.Reservations...)
	t, p := d.ProductScope.TenantID, d.ProductScope.ProductID
	seen := map[string]bool{}
	for _, r := range d.Reservations {
		if e := domain.ValidateID(r.AccountID); e != nil {
			return d, e
		}
		if e := domain.ValidateID(r.ReservationID); e != nil {
			return d, e
		}
		if r.Amount < 0 || r.ScopeKey == "" {
			return d, ErrDenied
		}
		if r.Kind == "supplier_global" || r.Kind == "platform_risk" {
			if r.ScopeKey != "GLOBAL" {
				return d, ErrDenied
			}
		}
		if r.Kind == "supplier_tenant" && r.ScopeKey != t {
			return d, ErrDenied
		}
		address := r.Kind + "\x00" + r.ScopeKey + "\x00" + r.AccountID
		if seen[address] {
			return d, ErrConflict
		}
		seen[address] = true
		d.Locks = append(d.Locks, LockKey{Rank: 70, Kind: r.Kind, IDs: []string{r.ScopeKey, r.AccountID}}, LockKey{Rank: 80, Kind: r.Kind, IDs: []string{r.ScopeKey, r.AccountID, r.ReservationID}})
	}
	d.Locks = append(d.Locks, LockKey{Rank: 60, IDs: []string{t, p, d.TaskID, d.AttemptID, d.AuthorizationID}})
	// Every retry uses the same frozen request fingerprint, independent of worker
	// fence, lease or commit response. Exact amounts and account addresses included.
	sort.Slice(d.Reservations, func(i, j int) bool {
		a, b := d.Reservations[i], d.Reservations[j]
		return less(LockKey{Rank: 80, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID, a.ReservationID}}, LockKey{Rank: 80, Kind: b.Kind, IDs: []string{b.ScopeKey, b.AccountID, b.ReservationID}})
	})
	hash, e := dispatchFingerprint(d)
	if e != nil {
		return d, e
	}
	d.PayloadHash = hash
	return d, nil
}

// Pure original receipt preimage, with no permission or send capability.
func dispatchFingerprint(d DispatchRequest) ([32]byte, error) {
	d.Reservations = append([]Reservation(nil), d.Reservations...)
	sort.Slice(d.Reservations, func(i, j int) bool {
		a, b := d.Reservations[i], d.Reservations[j]
		return less(LockKey{Rank: 80, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID, a.ReservationID}}, LockKey{Rank: 80, Kind: b.Kind, IDs: []string{b.ScopeKey, b.AccountID, b.ReservationID}})
	})
	payload, e := json.Marshal(struct {
		Attempt, Authorization, Route, Request string
		Hash                                   [32]byte
		Slot                                   uint8
		Reservations                           []Reservation
	}{d.AttemptID, d.AuthorizationID, d.Route, d.RequestKey, d.RequestHash, d.Slot, d.Reservations})
	if e != nil {
		return [32]byte{}, e
	}
	// Preserve the immutable pre-Harness dispatch preimage for existing requests.
	// Only newly managed envelopes bind the additive quote identity.
	if d.HarnessQuoteID != "" {
		payload, e = json.Marshal(struct {
			Original     json.RawMessage
			HarnessQuote string
		}{payload, d.HarnessQuoteID})
		if e != nil {
			return [32]byte{}, e
		}
	}
	return sha256.Sum256(payload), nil
}

type attemptRow struct {
	slot                                 uint8
	request, route, state, authorization string
	hash                                 []byte
	started                              sql.NullTime
	marker                               sql.NullString
}

func readAttempt(ctx context.Context, tx *sql.Tx, d DispatchRequest) (a attemptRow, e error) {
	e = tx.QueryRowContext(ctx, "SELECT a.slot,a.request_key,a.route,a.request_hash,a.state,a.send_started_at,a.marker_commit_key,z.authorization_id FROM attempts a JOIN dispatch_authorizations z ON z.tenant_id=a.tenant_id AND z.product_id=a.product_id AND z.task_id=a.task_id AND z.attempt_id=a.attempt_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&a.slot, &a.request, &a.route, &a.hash, &a.state, &a.started, &a.marker, &a.authorization)
	return
}
func (a attemptRow) matches(d DispatchRequest) bool {
	return a.slot == d.Slot && a.request == d.RequestKey && a.route == d.Route && a.authorization == d.AuthorizationID && bytes.Equal(a.hash, d.RequestHash[:])
}
func validateReservations(ctx context.Context, tx *sql.Tx, d DispatchRequest) error {
	t, p := d.ProductScope.TenantID, d.ProductScope.ProductID
	for _, r := range d.Reservations {
		var tenant, product, task, purpose, status string
		var amount int64
		e := tx.QueryRowContext(ctx, "SELECT tenant_id,product_id,task_id,purpose,amount,status FROM reservations WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", r.Kind, r.ScopeKey, r.AccountID, r.ReservationID).Scan(&tenant, &product, &task, &purpose, &amount, &status)
		if e != nil {
			return e
		}
		if tenant != t || product != p || task != d.TaskID || purpose != d.AttemptID || amount != r.Amount || status != "HELD" {
			return ErrConflict
		}
	}
	return nil
}
func (s *Store) AuthorizeDispatch(ctx context.Context, request DispatchRequest) (DispatchAuthorization, error) {
	releaseEvidence, e := s.prepareRecoveryDispatch(ctx, &request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	defer releaseEvidence()
	request.phase = "AUTHORIZE"
	d, e := prepareDispatch(request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	ctx, settlement, settlementRelease, e := s.turnSettlementAdmission(ctx, d)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	defer settlementRelease()
	d.originalSettlementService = settlement
	if d.candidateExecution && d.Slot == 1 {
		var releaseQuery func()
		ctx, d.originalQueryService, releaseQuery, e = s.turnQueryAdmission(ctx, d)
		if e != nil {
			return DispatchAuthorization{}, e
		}
		defer releaseQuery()
	}
	result := DispatchAuthorization{AttemptID: d.AttemptID}
	receipt, replay, e := s.barrier(ctx, d.BarrierRequest, func(port Tx) error {
		tx := port.(*unit).tx
		if e := checkSupplierDebt(ctx, tx, d.Reservations); e != nil {
			return e
		}
		t, p := d.ProductScope.TenantID, d.ProductScope.ProductID
		a, e := readAttempt(ctx, tx, d)
		if e == nil {
			if !a.matches(d) {
				return ErrConflict
			}
			if e = validateReservations(ctx, tx, d); e != nil {
				return e
			}
			if e = checkHarnessAttempt(ctx, tx, d, false); e != nil {
				return e
			}
			result.QueryOriginal = a.started.Valid
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var used uint8
		if e = tx.QueryRowContext(ctx, "SELECT calls_used FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, d.TaskID).Scan(&used); e != nil {
			return e
		}
		if used != d.Slot || used >= 2 {
			return ErrDenied
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO attempts(tenant_id,product_id,task_id,attempt_id,slot,request_key,route,request_hash,state) VALUES(?,?,?,?,?,?,?,?,'AUTHORIZED')", t, p, d.TaskID, d.AttemptID, d.Slot, d.RequestKey, d.Route, d.RequestHash[:]); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO dispatch_authorizations(tenant_id,product_id,task_id,attempt_id,authorization_id,fence,owner_subject_id) VALUES(?,?,?,?,?,?,?)", t, p, d.TaskID, d.AttemptID, d.AuthorizationID, d.Fence, d.OwnerSubjectID); e != nil {
			return e
		}
		if e = checkHarnessAttempt(ctx, tx, d, true); e != nil {
			return e
		}
		for _, r := range d.Reservations {
			res, e := tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held+?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND balance-held>=?", r.Amount, r.Kind, r.ScopeKey, r.AccountID, r.Amount)
			if e != nil {
				return e
			}
			n, e := res.RowsAffected()
			if e != nil || n != 1 {
				return ErrDenied
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO reservations(tenant_id,product_id,task_id,kind,scope_key,account_id,reservation_id,purpose,amount,status) VALUES(?,?,?,?,?,?,?,?,?,'HELD')", t, p, d.TaskID, r.Kind, r.ScopeKey, r.AccountID, r.ReservationID, d.AttemptID, r.Amount); e != nil {
				return e
			}
		}
		res, e := tx.ExecContext(ctx, "UPDATE tasks SET calls_used=calls_used+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND fence=? AND calls_used=?", t, p, d.TaskID, d.Fence, used)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return ErrDenied
		}
		if e = advanceTurn(ctx, port.(*unit), "AUTHORIZED"); e != nil {
			return e
		}
		return s.executeTextSkills(ctx, port.(*unit), d)
	})
	if e != nil {
		return DispatchAuthorization{}, e
	}
	result.Receipt = receipt
	if replay {
		// Receipt replay does not know whether an attempt has subsequently been sent.
		// Conservative query is safe; callers may explicitly try StartSend which
		// re-enters current gates and returns QueryOriginal for an existing marker.
		result.QueryOriginal = true
	}
	return result, nil
}
func (s *Store) StartSend(ctx context.Context, request DispatchRequest, attemptID string) (DispatchAuthorization, error) {
	releaseEvidence, e := s.prepareRecoveryDispatch(ctx, &request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	defer releaseEvidence()
	request.phase = "SEND"
	if request.AttemptID != attemptID {
		return DispatchAuthorization{}, ErrConflict
	}
	d, e := prepareDispatch(request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	ctx, queryService, release, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	defer release()
	d.originalQueryService = queryService
	ctx, settlement, settlementRelease, e := s.turnSettlementAdmission(ctx, d)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	defer settlementRelease()
	d.originalSettlementService = settlement
	result := DispatchAuthorization{AttemptID: attemptID}
	var material domain.GenerationMaterial
	var candidate *domain.DraftCandidateMaterial
	receipt, replay, e := s.barrier(ctx, d.BarrierRequest, func(port Tx) error {
		tx := port.(*unit).tx
		if e := checkSupplierDebt(ctx, tx, d.Reservations); e != nil {
			return e
		}
		a, e := readAttempt(ctx, tx, d)
		if e != nil {
			return e
		}
		if !a.matches(d) {
			return ErrConflict
		}
		if e = validateReservations(ctx, tx, d); e != nil {
			return e
		}
		if a.started.Valid {
			result.QueryOriginal = true
			return nil
		}
		if e = checkHarnessAttempt(ctx, tx, d, false); e != nil {
			return e
		}
		if a.state != "AUTHORIZED" {
			return ErrDenied
		}
		if queryService != nil && d.candidateExecution {
			m, err := s.candidateMaterialTx(ctx, port.(*unit), d.BarrierRequest)
			if err != nil {
				return err
			}
			candidate = &m
			frozen := m
			frozen.Canonical = nil
			raw, err := s.candidateSnapshotTx(ctx, port.(*unit), d, frozen)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO harness_candidate_attempts(tenant_id,product_id,task_id,attempt_id,document) VALUES(?,?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, raw); err != nil {
				return err
			}
		} else if queryService != nil {
			if s.GenerationContext == nil {
				return ErrDenied
			}
			var snapshot domain.OriginalContextSnapshot
			if d.Slot == 1 {
				material, snapshot, e = s.checkRecoveryDispatchTx(ctx, port, d.BarrierRequest)
			} else {
				material, e = s.GenerationContext.BuildGenerationRequestTx(ctx, port, *d.WorldScope, d.TaskID)
				if e == nil {
					snapshot, e = s.GenerationContext.SnapshotOriginalContextTx(ctx, port, *d.WorldScope, d.TaskID)
				}
			}
			if e != nil {
				return e
			}
			if len(material.Canonical) == 0 {
				return ErrDenied
			}
			if snapshot.Binding != material.Binding {
				return ErrConflict
			}
			raw, e := json.Marshal(snapshot)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO harness_attempt_contexts(tenant_id,product_id,task_id,attempt_id,document) VALUES(?,?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, raw); e != nil {
				return e
			}
		}
		t, p := d.ProductScope.TenantID, d.ProductScope.ProductID
		res, e := tx.ExecContext(ctx, "UPDATE dispatch_authorizations SET fence=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND owner_subject_id=?", d.Fence, t, p, d.TaskID, d.AttemptID, d.OwnerSubjectID)
		if e != nil {
			return e
		}
		if _, e = res.RowsAffected(); e != nil {
			return e
		}
		res, e = tx.ExecContext(ctx, "UPDATE attempts SET state='SENT',send_started_at=UTC_TIMESTAMP(6),marker_commit_key=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND send_started_at IS NULL AND state='AUTHORIZED'", d.CommitKey, t, p, d.TaskID, attemptID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return ErrDenied
		}
		if e = bindOriginalQuery(ctx, port.(*unit), d, queryService); e != nil {
			return e
		}
		result.CanSend = true
		return advanceTurn(ctx, port.(*unit), "SEND_MARKED")
	})
	if e != nil {
		return DispatchAuthorization{}, e
	}
	result.Receipt = receipt
	if replay {
		result.CanSend = false
		result.QueryOriginal = true
	}
	if result.CanSend && queryService != nil {
		result.send = &OriginalSendPermit{store: s, dispatch: cloneOriginalDispatch(d), material: material, candidate: candidate}
	}
	return result, nil
}
