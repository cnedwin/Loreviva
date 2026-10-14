package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

type RehearsalInputOwner interface {
	InspectRehearsalNegativeSourcesTx(context.Context, Tx, domain.Scope, string, domain.Versions, domain.RehearsalNegativeOutcome) error
	InspectRehearsalChildResultTx(context.Context, Tx, domain.Scope, string, domain.Versions, domain.CandidateCommit) error
	AcceptRehearsalInputTx(context.Context, Tx, domain.TurnInput) error
	ValidateRehearsalAppliedSourcesTx(context.Context, Tx, domain.Scope, string, domain.CandidateCommit) error
}
type rehearsalChildStart struct {
	child, quote, actor, source string
	index                       int
}

// The caller supplies only the current root lease. Input, order, child identity,
// actor and budget attribution are derived from the original accepted root.
func (s *Store) StartRehearsalChild(ctx context.Context, lease domain.TaskLease, worker string) (out generated.TurnAccepted, err error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	if lease.WorldInstanceID != nil || lease.OwnerSubjectID != lease.SubjectID || s.RehearsalInputOwner == nil || s.RehearsalInitializer == nil || s.DraftRehearsalOwner == nil || s.DraftRehearsalReader == nil {
		return out, ErrDenied
	}
	g, e := s.rehearsalGates(ctx, lease.ProductScope)
	if e != nil {
		return out, e
	}
	var bound, body []byte
	var index int
	e = s.DB.QueryRowContext(ctx, "SELECT a.binding,i.document,a.next_input FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", lease.TenantID, lease.ProductID, lease.TaskID, lease.SubjectID).Scan(&bound, &body, &index)
	if e != nil {
		return out, e
	}
	var b domain.RehearsalBinding
	var request generated.RehearsalRequest
	if json.Unmarshal(bound, &b) != nil || generated.Decode("RehearsalRequest", body, &request) != nil || index < 0 || index >= len(request.Inputs) {
		return out, ErrDenied
	}
	evidence, e := s.DraftRehearsalReader.ReadRehearsalSourceEvidence(ctx, lease.ProductScope, b, request)
	if e != nil {
		return out, e
	}
	defer evidence.Close()
	keys, e := s.DraftRehearsalOwner.PlanRehearsalLocks(ctx, lease.ProductScope, b.DraftID)
	if e != nil {
		return out, e
	}
	more, e := s.RehearsalInitializer.PlanRehearsalInitialization(ctx, lease.ProductScope, lease.TaskID, b, request)
	if e != nil {
		return out, e
	}
	keys = append(keys, more...)
	world := turnIdentity(lease.TaskID, "rehearsal-world")
	version := turnIdentity(lease.TaskID, "rehearsal-version")
	child := turnIdentity(lease.TaskID, "rehearsal-child", strconv.Itoa(index))
	quote := turnIdentity(child, "quote")
	key := turnIdentity(child, "accepted")
	var actor string
	if e = s.DB.QueryRowContext(ctx, "SELECT actor_id FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND slot_id=?", lease.TenantID, lease.ProductID, world, request.Inputs[index].ActorSlot).Scan(&actor); e != nil {
		return out, e
	}
	p := &rehearsalChildStart{child: child, quote: quote, actor: actor, source: turnIdentity(child, "input"), index: index}
	keys = append(keys, harnessTaskKey(lease.ProductScope, child), harnessTaskKey(lease.ProductScope, quote))
	r := BarrierRequest{ProductScope: lease.ProductScope, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, GateVersions: g, Operation: "REHEARSAL_CHILD_START", CommandKey: key, CommitKey: key, Locks: keys, rehearsalInitialization: &rehearsalInitialization{lease.TaskID, world, version, b, request}, rehearsalChildStart: p}
	_, r.PayloadHash, e = harnessPayload(struct {
		Root    string
		Index   int
		Binding domain.RehearsalBinding
	}{lease.TaskID, index, b})
	if e != nil {
		return out, e
	}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		// Receipt replay is historical metadata; claim and execution revalidate current authority.
		u := port.(*unit)
		if e := s.checkRehearsalChildStart(ctx, u, r); e != nil {
			return e
		}
		var now, rootEnd, settle time.Time
		var oldExpiry sql.NullTime
		var generation uint64
		if e := u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6),h.execute_by,h.settle_by,h.narrative_generation,t.lease_expires_at FROM harness_envelopes h JOIN tasks t USING(tenant_id,product_id,task_id) WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&now, &rootEnd, &settle, &generation, &oldExpiry); e != nil {
			return e
		}
		end := now.Add(time.Minute)
		if rootEnd.Before(end) {
			end = rootEnd
		}
		if !end.After(now) || !settle.After(now) || !oldExpiry.Valid || !oldExpiry.Time.After(now) {
			return ErrDenied
		}
		t, pr := lease.TenantID, lease.ProductID
		// Only the first child-start transaction may cover this child's original deadline.
		// An existing live lease is never shortened, and an expired lease is never revived.
		if oldExpiry.Time.Before(end) {
			result, e := u.tx.ExecContext(ctx, "UPDATE tasks t SET lease_expires_at=? WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind='ADMIN' AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.state='RUNNING' AND t.cancelled=0 AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND ?>UTC_TIMESTAMP(6) AND EXISTS(SELECT 1 FROM registered_jobs j JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id AND j.operation='REHEARSAL' AND h.state='ACTIVE' AND h.execute_by=? AND h.settle_by=? AND h.execute_by>UTC_TIMESTAMP(6) AND h.settle_by>UTC_TIMESTAMP(6))", end, t, pr, lease.TaskID, lease.OwnerSubjectID, lease.Fence, worker, oldExpiry.Time, end, rootEnd, settle)
			if e != nil {
				return e
			}
			rows, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if rows != 1 {
				var live bool
				e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind='ADMIN' AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.state='RUNNING' AND t.cancelled=0 AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND ?>UTC_TIMESTAMP(6) AND j.operation='REHEARSAL' AND h.state='ACTIVE' AND h.execute_by=? AND h.settle_by=? AND h.execute_by>UTC_TIMESTAMP(6) AND h.settle_by>UTC_TIMESTAMP(6))", t, pr, lease.TaskID, lease.OwnerSubjectID, lease.Fence, worker, end, rootEnd, settle).Scan(&live)
				if e != nil {
					return e
				}
				if !live {
					return ErrDenied
				}
				// A failed CAS is never promoted to success by observing a newer value.
				return ErrReplan
			}
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,world_instance_id,state,fence,next_run_at,task_kind,project_id) VALUES(?,?,?,?,'WORLD',?,'ACCEPTED',0,?,'TURN',?)", t, pr, child, lease.SubjectID, world, now, b.ProjectID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO registered_jobs(tenant_id,product_id,task_id,operation) VALUES(?,?,?,'REHEARSAL_TURN')", t, pr, child); e != nil {
			return e
		}
		routeID, e := s.RehearsalInitializer.RehearsalRouteHashTx(ctx, u, domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: world}, actor)
		if e != nil {
			return e
		}
		var route [32]byte
		decoded, e := hex.DecodeString(routeID)
		if e != nil || len(decoded) != 32 {
			return ErrDenied
		}
		copy(route[:], decoded)
		q := HarnessQuote{ID: quote, ProjectID: b.ProjectID, Kind: "TURN", RequestHash: r.PayloadHash, RouteHash: route}
		qraw, _, e := harnessPayload(q)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_quotes(tenant_id,product_id,quote_id,owner_subject_id,world_instance_id,project_id,document,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)", t, pr, quote, lease.SubjectID, world, b.ProjectID, qraw, now, now.Add(time.Minute)); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turn_quotes(tenant_id,product_id,quote_id,actor_id,input_bound,output_bound,public_document) VALUES(?,?,?,?,16000,4000,JSON_OBJECT())", t, pr, quote, actor); e != nil {
			return e
		}
		intake := HarnessIntake{TaskID: child, QuoteID: quote, RequestHash: r.PayloadHash, RouteHash: route, ParentHolds: []Reservation{}}
		raw, _, e := harnessPayload(intake)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_envelopes(tenant_id,product_id,task_id,quote_id,document,accepted_at,execute_by,settle_by,max_calls,max_steps,supplier_cap) VALUES(?,?,?,?,?,?,?,?,2,20,0)", t, pr, child, quote, raw, now, end, settle); e != nil {
			return e
		}
		var versions domain.Versions
		if e = u.tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", t, pr, world).Scan(&versions.ReleaseID, &versions.WorldRevision, &versions.ControlVersion, &versions.PrivacyEpoch, &versions.ViewEpoch); e != nil {
			return e
		}
		vraw, _ := json.Marshal(versions)
		inputHash := sha256.Sum256([]byte(request.Inputs[index].Text))
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turns(tenant_id,product_id,task_id,world_instance_id,owner_subject_id,original_key,command_hash,quote_id,actor_id,input_source_id,input_hash,versions,accepted_commit_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", t, pr, child, world, lease.SubjectID, key, r.PayloadHash[:], quote, actor, p.source, inputHash[:], vraw, key); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_rehearsal_children(tenant_id,product_id,root_task_id,input_index,child_task_id,world_instance_id,actor_id,input_source_id,input_hash,root_generation,started_at,execute_by,accepted_receipt) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", t, pr, lease.TaskID, index, child, world, actor, p.source, inputHash[:], generation, now, end, key); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_settlement_bindings SELECT tenant_id,product_id,?,installation_id,service_id,policy FROM harness_settlement_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", child, t, pr, lease.TaskID); e != nil {
			return e
		}
		return s.RehearsalInputOwner.AcceptRehearsalInputTx(ctx, u, domain.TurnInput{Scope: domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: world}, TaskID: child, ActorID: actor, SourceID: p.source, Body: request.Inputs[index].Text})
	})
	if e != nil {
		return out, e
	}
	out = generated.TurnAccepted{TurnID: child, QuoteID: quote, Task: generated.TaskReceipt{TaskID: child, TaskKind: "TURN", TaskState: "ACCEPTED", Scope: turnScope(domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: world}), CorrelationID: child}}
	return out, nil
}

// Recheck the original next input and isolated state inside the owning transaction.
// If its actor changed after planning, replan rather than substituting a new child identity.
func (s *Store) checkRehearsalChildStart(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalChildStart
	init := r.rehearsalInitialization
	if p == nil || init == nil || p.index < 0 || p.index >= len(init.request.Inputs) {
		return ErrDenied
	}
	if e := s.checkRehearsalInitialization(ctx, u, r); e != nil {
		return e
	}
	var actual int
	var state string
	if e := u.tx.QueryRowContext(ctx, "SELECT next_input,fsm_state FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&actual, &state); e != nil {
		return e
	}
	if actual != p.index || state != "ISOLATED" {
		return ErrDenied
	}
	var actor string
	if e := u.tx.QueryRowContext(ctx, "SELECT actor_id FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND slot_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, init.world, init.request.Inputs[p.index].ActorSlot).Scan(&actor); e != nil {
		return e
	}
	if actor != p.actor {
		return ErrReplan
	}
	return nil
}

func (u *unit) AssertRehearsalInput(in domain.TurnInput) error {
	p, init := u.rehearsalChildStart, u.rehearsalInitialization
	if p == nil || init == nil || u.commandOnly || u.purpose != ExecutionPurpose || u.taskID != init.root || u.scope != in.Scope.ProductScope || in.Scope.WorldInstanceID != init.world || in.TaskID != p.child || in.ActorID != p.actor || in.SourceID != p.source || in.Body != init.request.Inputs[p.index].Text {
		return ErrDenied
	}
	return u.RequireLock(harnessTaskKey(u.scope, p.child))
}

func acquireRehearsalChildIdentity(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	p := r.rehearsalChildStart
	if p == nil || len(k.IDs) != 3 || (k.IDs[2] != p.child && k.IDs[2] != p.quote) {
		return ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	if k.IDs[2] == p.quote {
		return nil
	}
	// The rank50 marker and sidecar do not substitute for the actual task row.
	// Claim/lease writers and the cost pipeline serialize on this same row.
	var taskOwner, taskKind, scopeKind string
	var taskWorld sql.NullString
	rowErr := tx.QueryRowContext(ctx, "SELECT owner_subject_id,task_kind,scope_kind,world_instance_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", r.ProductScope.TenantID, r.ProductScope.ProductID, p.child).Scan(&taskOwner, &taskKind, &scopeKind, &taskWorld)
	if rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
		return rowErr
	}
	if rowErr == nil && (r.rehearsalInitialization == nil || taskOwner != r.OwnerSubjectID || taskKind != "TURN" || scopeKind != "WORLD" || taskWorld.String != r.rehearsalInitialization.world) {
		return ErrDenied
	}
	var root string
	e := tx.QueryRowContext(ctx, "SELECT root_task_id FROM harness_rehearsal_children WHERE tenant_id=? AND product_id=? AND child_task_id=? FOR UPDATE", r.ProductScope.TenantID, r.ProductScope.ProductID, p.child).Scan(&root)
	if errors.Is(e, sql.ErrNoRows) {
		if rowErr == nil {
			return ErrDenied
		}
		return nil
	}
	if e != nil {
		return e
	}
	if root != r.TaskID || rowErr != nil {
		return ErrDenied
	}
	return nil
}
