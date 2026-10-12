package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"time"
)

// HarnessQuote is an internal immutable price/route binding. Public R0 callers
// can only create zero caps; nonzero inputs are isolated synthetic test policy.
type HarnessQuote struct {
	ID, ProjectID, Kind         string
	Inputs                      int
	RequestHash, RouteHash      [32]byte
	ServiceAtoms, SupplierAtoms int64
}
type HarnessIntake struct {
	TaskID, QuoteID        string
	RequestHash, RouteHash [32]byte
	ParentHolds            []Reservation
}

func harnessCommand(c CommandBarrierRequest) BarrierRequest {
	return BarrierRequest{ProductScope: c.ProductScope, WorldScope: c.WorldScope, Versions: c.Versions, OwnerSubjectID: c.OwnerSubjectID, Operation: c.Operation, CommandKey: c.CommandKey, CommitKey: c.CommitKey, GateVersions: c.GateVersions, PayloadHash: c.PayloadHash, Locks: append([]LockKey{}, c.Locks...), commandOnly: true}
}
func harnessPayload(v any) ([]byte, [32]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, [32]byte{}, e
	}
	canonical, e := wire.Canonical(raw)
	// Canonical exponent notation is only the identity preimage. Persist typed
	// integer JSON so MySQL does not normalize 6e1 to a floating 60.0 value.
	return raw, sha256.Sum256(canonical), e
}
func harnessTaskKey(s domain.ProductScope, id string) LockKey {
	return LockKey{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}
}
func holdLocks(v []Reservation) []LockKey {
	out := []LockKey{}
	for _, r := range v {
		out = append(out, LockKey{Rank: 70, Kind: r.Kind, IDs: []string{r.ScopeKey, r.AccountID}}, LockKey{Rank: 80, Kind: r.Kind, IDs: []string{r.ScopeKey, r.AccountID, r.ReservationID}})
	}
	return out
}

func (s *Store) QuoteHarness(ctx context.Context, c CommandBarrierRequest, q HarnessQuote) (CommitReceipt, error) {
	if c.Operation != "HARNESS_QUOTE" || c.Purpose != ExecutionPurpose || c.Runtime != nil || domain.ValidateID(q.ID) != nil || domain.ValidateID(q.ProjectID) != nil || q.ServiceAtoms < 0 || q.SupplierAtoms < 0 || q.RequestHash == [32]byte{} || q.RouteHash == [32]byte{} {
		return CommitReceipt{}, ErrDenied
	}
	if _, e := domain.LimitsFor(q.Kind, q.Inputs); e != nil {
		return CommitReceipt{}, e
	}
	if (q.Kind == "TURN") != (c.WorldScope != nil) {
		return CommitReceipt{}, ErrDenied
	}
	raw, hash, e := harnessPayload(q)
	if e != nil {
		return CommitReceipt{}, e
	}
	r := harnessCommand(c)
	r.PayloadHash = hash
	r.Locks = append(r.Locks, harnessTaskKey(c.ProductScope, q.ID))
	receipt, _, e := s.barrier(ctx, r, func(port Tx) error {
		u := port.(*unit)
		var world any
		if c.WorldScope != nil {
			world = c.WorldScope.WorldInstanceID
		}
		_, e := u.tx.ExecContext(ctx, "INSERT INTO harness_quotes(tenant_id,product_id,quote_id,owner_subject_id,world_instance_id,project_id,document,created_at,expires_at) VALUES(?,?,?,?,?,?,?,UTC_TIMESTAMP(6),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 60 SECOND))", c.ProductScope.TenantID, c.ProductScope.ProductID, q.ID, c.OwnerSubjectID, world, q.ProjectID, raw)
		return e
	})
	return receipt, e
}

func (s *Store) AcceptHarness(ctx context.Context, c CommandBarrierRequest, in HarnessIntake) (CommitReceipt, error) {
	return s.acceptHarness(ctx, c, in, nil)
}
func (s *Store) acceptHarness(ctx context.Context, c CommandBarrierRequest, in HarnessIntake, turn *turnIntake) (CommitReceipt, error) {
	if c.Operation != "HARNESS_INTAKE" || c.Purpose != ExecutionPurpose || c.Runtime != nil || domain.ValidateID(in.TaskID) != nil || domain.ValidateID(in.QuoteID) != nil || in.TaskID == in.QuoteID {
		return CommitReceipt{}, ErrDenied
	}
	var settlement *domain.SettlementServiceIdentity
	if turn != nil {
		if s.SettlementAuthority == nil {
			return CommitReceipt{}, ErrDenied
		}
		next, identity, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
		if e != nil {
			return CommitReceipt{}, e
		}
		defer release()
		ctx = next
		settlement = &identity
	}
	in.ParentHolds = append([]Reservation{}, in.ParentHolds...)
	sort.Slice(in.ParentHolds, func(i, j int) bool {
		a, b := in.ParentHolds[i], in.ParentHolds[j]
		return less(LockKey{Rank: 80, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID, a.ReservationID}}, LockKey{Rank: 80, Kind: b.Kind, IDs: []string{b.ScopeKey, b.AccountID, b.ReservationID}})
	})
	expected := map[string]bool{"customer": true, "project": true, "platform_risk": true}
	if c.WorldScope != nil {
		expected["world"] = true
	}
	for _, h := range in.ParentHolds {
		if !expected[h.Kind] || h.Amount < 0 || h.ScopeKey == "" || domain.ValidateID(h.AccountID) != nil || domain.ValidateID(h.ReservationID) != nil {
			return CommitReceipt{}, ErrDenied
		}
		delete(expected, h.Kind)
	}
	if len(expected) != 0 {
		return CommitReceipt{}, ErrDenied
	}
	raw, hash, e := harnessPayload(in)
	if e != nil {
		return CommitReceipt{}, e
	}
	r := harnessCommand(c)
	r.PayloadHash = hash
	r.harnessIntakeQuote = in.QuoteID
	r.originalSettlementService = settlement
	if turn != nil {
		r.PayloadHash = turn.Hash
		if e = s.planPerception(ctx, &r, turn.Request.PrimaryActorID); e != nil {
			return CommitReceipt{}, e
		}
		r.r0TurnProvision = true
		for _, a := range r0Accounts(*c.WorldScope, turn.ProjectID) {
			r.Locks = append(r.Locks, LockKey{Rank: 70, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID}})
		}
	}
	r.Locks = append(r.Locks, harnessTaskKey(c.ProductScope, in.QuoteID), harnessTaskKey(c.ProductScope, in.TaskID))
	r.Locks = append(r.Locks, holdLocks(in.ParentHolds)...)
	receipt, _, e := s.barrier(ctx, r, func(port Tx) error {
		u := port.(*unit)
		t, p := c.ProductScope.TenantID, c.ProductScope.ProductID
		var qraw []byte
		var owner, project string
		var world sql.NullString
		var valid bool
		if e := u.tx.QueryRowContext(ctx, "SELECT document,owner_subject_id,world_instance_id,project_id,expires_at>UTC_TIMESTAMP(6) FROM harness_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", t, p, in.QuoteID).Scan(&qraw, &owner, &world, &project, &valid); e != nil {
			return e
		}
		if !valid || owner != c.OwnerSubjectID || world.Valid != (c.WorldScope != nil) || world.String != worldKey(r) {
			return ErrDenied
		}
		var q HarnessQuote
		if e := json.Unmarshal(qraw, &q); e != nil {
			return e
		}
		if q.ID != in.QuoteID || q.ProjectID != project || q.RequestHash != in.RequestHash || q.RouteHash != in.RouteHash {
			return ErrConflict
		}
		if turn != nil {
			if s.TurnOwner == nil || q.ServiceAtoms != 0 || q.SupplierAtoms != 0 {
				return ErrDenied
			}
			route, e := s.TurnOwner.ResolveTurnRouteTx(ctx, u, *c.WorldScope, turn.Request.PrimaryActorID)
			if e != nil {
				return e
			}
			if route.ProjectID != q.ProjectID || route.RouteID != fmt.Sprintf("%x", q.RouteHash) {
				return ErrConflict
			}
		}
		limits, e := domain.LimitsFor(q.Kind, q.Inputs)
		if e != nil {
			return e
		}
		for _, h := range in.ParentHolds {
			amount := q.ServiceAtoms
			if h.Kind == "platform_risk" {
				amount = q.SupplierAtoms
			}
			if h.Amount != amount {
				return ErrConflict
			}
		}
		var now time.Time
		if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		kind := "ADMIN"
		if c.WorldScope != nil {
			kind = "TURN"
		}
		if e = u.CreateTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: c.ProductScope, WorldScope: c.WorldScope, OwnerSubjectID: c.OwnerSubjectID}, TaskID: in.TaskID, Kind: kind, ProjectID: q.ProjectID, NextRunAt: now}); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO registered_jobs(tenant_id,product_id,task_id,operation) VALUES(?,?,?,?)", t, p, in.TaskID, q.Kind); e != nil {
			return e
		}
		for _, h := range in.ParentHolds {
			if e = reserveHarnessHold(ctx, u.tx, t, p, in.TaskID, "PARENT", h); e != nil {
				return e
			}
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_envelopes(tenant_id,product_id,task_id,quote_id,document,accepted_at,execute_by,settle_by,max_calls,max_steps,supplier_cap) VALUES(?,?,?,?,?,?,?,?,?,?,?)", t, p, in.TaskID, in.QuoteID, raw, now, now.Add(time.Duration(limits.Seconds)*time.Second), now.Add(time.Duration(limits.Seconds+60)*time.Second), limits.Calls, limits.Steps, q.SupplierAtoms)
		if e != nil {
			return e
		}
		if settlement != nil {

			if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_settlement_bindings VALUES(?,?,?,?,?,?)", t, p, in.TaskID, settlement.Installation, settlement.ServiceID, settlement.Policy); e != nil {
				return e
			}
			if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_cutoff_work(tenant_id,product_id,task_id,next_run_at) VALUES(?,?,?,?)", t, p, in.TaskID, now.Add(time.Duration(limits.Seconds+60)*time.Second)); e != nil {
				return e
			}
		}
		if turn != nil {
			versions, _ := json.Marshal(c.Versions)
			if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turns(tenant_id,product_id,task_id,world_instance_id,owner_subject_id,original_key,command_hash,quote_id,actor_id,input_source_id,input_hash,versions,accepted_commit_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", t, p, in.TaskID, c.WorldScope.WorldInstanceID, c.OwnerSubjectID, c.CommandKey, turn.Hash[:], in.QuoteID, turn.Request.PrimaryActorID, turn.SourceID, turn.InputHash[:], versions, c.CommitKey); e != nil {
				return e
			}
			u.acceptedTurnID = in.TaskID
			e = s.TurnOwner.AcceptTurnInputTx(ctx, u, domain.TurnInput{Scope: *c.WorldScope, TaskID: in.TaskID, ActorID: turn.Request.PrimaryActorID, SourceID: turn.SourceID, Body: turn.Body})
			u.acceptedTurnID = ""
		}
		return e
	})
	return receipt, e
}

func reserveHarnessHold(ctx context.Context, tx *sql.Tx, t, p, task, purpose string, h Reservation) error {
	result, e := tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held+?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND balance-held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrDenied
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO reservations(tenant_id,product_id,task_id,kind,scope_key,account_id,reservation_id,purpose,amount,status) VALUES(?,?,?,?,?,?,?,?,?,'HELD')", t, p, task, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID, purpose, h.Amount)
	return e
}

// checkHarnessAttempt runs only under the actual task50, attempt60, accounts70
// and reservations80 plan. Presence of a managed envelope forbids a legacy
// DispatchRequest from bypassing its frozen cap or expiry.
func checkHarnessAttempt(ctx context.Context, tx *sql.Tx, d DispatchRequest, allocate bool) error {
	var quote, state string
	var deadline time.Time
	var cap, used int64
	var calls, maxCalls int
	e := tx.QueryRowContext(ctx, "SELECT quote_id,state,execute_by,supplier_cap,supplier_allocated,calls_allocated,max_calls FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID).Scan(&quote, &state, &deadline, &cap, &used, &calls, &maxCalls)
	if errors.Is(e, sql.ErrNoRows) {
		if d.HarnessQuoteID != "" {
			return ErrDenied
		}
		return nil
	}
	if e != nil {
		return e
	}
	if d.originalSettlementService != nil {
		id := d.originalSettlementService
		var bound bool
		if e = tx.QueryRowContext(ctx, "SELECT installation_id=? AND service_id=? AND policy=? FROM harness_settlement_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", id.Installation, id.ServiceID, id.Policy, d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID).Scan(&bound); e != nil {
			return e
		}
		if !bound {
			return ErrDenied
		}
	}
	if quote != d.HarnessQuoteID || state != "ACTIVE" || len(d.Reservations) != 2 {
		return ErrDenied
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	if !now.Before(deadline) {
		return ErrDenied
	}
	amount := d.Reservations[0].Amount
	seen := map[string]bool{}
	for _, h := range d.Reservations {
		if h.Kind != "supplier_global" && h.Kind != "supplier_tenant" || seen[h.Kind] || h.Amount != amount {
			return ErrDenied
		}
		seen[h.Kind] = true
	}
	var original int64
	e = tx.QueryRowContext(ctx, "SELECT supplier_atoms FROM harness_attempt_allocations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&original)
	if e == nil {
		if original != amount {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if !allocate || amount > cap-used || calls >= maxCalls {
		return ErrDenied
	}
	if parent := d.rehearsalExecution; parent != nil {
		result, e := tx.ExecContext(ctx, "UPDATE harness_envelopes SET calls_allocated=calls_allocated+1,supplier_allocated=supplier_allocated+? WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND calls_allocated<max_calls AND supplier_cap-supplier_allocated>=? AND execute_by>UTC_TIMESTAMP(6)", amount, d.ProductScope.TenantID, d.ProductScope.ProductID, parent.root.TaskID, amount)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrDenied
		}
	}
	result, e := tx.ExecContext(ctx, "UPDATE harness_envelopes SET supplier_allocated=supplier_allocated+?,calls_allocated=calls_allocated+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND supplier_cap-supplier_allocated>=? AND calls_allocated<max_calls", amount, d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, amount)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrDenied
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO harness_attempt_allocations VALUES(?,?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, amount)
	return e
}
