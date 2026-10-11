package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"sort"
)

func prepareDispatch(d DispatchRequest) (DispatchRequest, error) {
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
	// The two-call ceiling is not authority to spend the extra slot. Task6
	// must first supply durable program approval and completed original-result
	// evidence for a permitted repair/recompute. Until then both authorization
	// and the final StartSend boundary accept only the original slot.
	if d.Slot != 0 || len(d.Reservations) == 0 {
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
	payload, e := json.Marshal(struct {
		Attempt, Authorization, Route, Request string
		Hash                                   [32]byte
		Slot                                   uint8
		Reservations                           []Reservation
	}{d.AttemptID, d.AuthorizationID, d.Route, d.RequestKey, d.RequestHash, d.Slot, d.Reservations})
	if e != nil {
		return d, e
	}
	d.PayloadHash = sha256.Sum256(payload)
	return d, nil
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
	request.phase = "AUTHORIZE"
	d, e := prepareDispatch(request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	result := DispatchAuthorization{AttemptID: d.AttemptID}
	receipt, replay, e := s.barrier(ctx, d.BarrierRequest, func(port Tx) error {
		tx := port.(*unit).tx
		t, p := d.ProductScope.TenantID, d.ProductScope.ProductID
		a, e := readAttempt(ctx, tx, d)
		if e == nil {
			if !a.matches(d) {
				return ErrConflict
			}
			if e = validateReservations(ctx, tx, d); e != nil {
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
		return nil
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
	request.phase = "SEND"
	if request.AttemptID != attemptID {
		return DispatchAuthorization{}, ErrConflict
	}
	d, e := prepareDispatch(request)
	if e != nil {
		return DispatchAuthorization{}, e
	}
	result := DispatchAuthorization{AttemptID: attemptID}
	receipt, replay, e := s.barrier(ctx, d.BarrierRequest, func(port Tx) error {
		tx := port.(*unit).tx
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
		if a.state != "AUTHORIZED" {
			return ErrDenied
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
		result.CanSend = true
		return nil
	})
	if e != nil {
		return DispatchAuthorization{}, e
	}
	result.Receipt = receipt
	if replay {
		result.CanSend = false
		result.QueryOriginal = true
	}
	return result, nil
}
