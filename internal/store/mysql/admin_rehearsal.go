package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

type DraftRehearsalOwner interface {
	RehearsalSourcesLiveTx(context.Context, Tx, domain.ProductScope, domain.RehearsalBinding) (bool, error)
	PlanRehearsalHistoryLocks(context.Context, domain.ProductScope, domain.RehearsalBinding) ([]LockKey, error)
	InspectRehearsalSourcesTx(context.Context, Tx, domain.ProductScope, domain.RehearsalBinding) error
	PlanRehearsalLocks(context.Context, domain.ProductScope, string) ([]LockKey, error)
	BuildRehearsalTx(context.Context, Tx, domain.ProductScope, string, generated.RehearsalRequest) (domain.RehearsalBinding, error)
}
type DraftRehearsalSourceReader interface {
	ReadRehearsalSourceEvidence(context.Context, domain.ProductScope, domain.RehearsalBinding, generated.RehearsalRequest) (CandidateSourceEvidence, error)
}
type rehearsalIntakeProvision struct {
	binding domain.RehearsalBinding
	request generated.RehearsalRequest
}

// This fixed internal intake is not exposed until the registered isolated
// executor and its full accounting obligations are installed.
// An exact original receipt reuses the accepted rehearsal; a new intake verifies source bytes and rebuilds the same binding in its owning transaction before persisting the original inputs and limits.
func (s *Store) AcceptDraftRehearsal(ctx context.Context, scope domain.ProductScope, draft, key string, request generated.RehearsalRequest) (out generated.AdminTaskReceipt, err error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	if s.Guard == nil || s.DraftRehearsalOwner == nil || s.DraftRehearsalReader == nil || s.SettlementAuthority == nil || scope.Validate() != nil || domain.ValidateID(draft) != nil || domain.ValidateIdempotencyKey(key) != nil || string(request.ProductID) != scope.ProductID {
		return out, ErrDenied
	}
	if e = typedJSON("RehearsalRequest", request); e != nil {
		return out, e
	}
	limits, e := domain.LimitsFor("REHEARSAL", len(request.Inputs))
	if e != nil {
		return out, e
	}
	_, hash, e := harnessPayload(struct {
		Scope   domain.ProductScope
		Draft   string
		Request generated.RehearsalRequest
	}{scope, draft, request})
	if e != nil {
		return out, e
	}
	g, e := s.rehearsalGates(ctx, scope)
	if e != nil {
		return out, e
	}
	if old, e := s.originalRehearsal(ctx, scope, key, hash); e == nil {
		return old, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	keys, e := s.DraftRehearsalOwner.PlanRehearsalLocks(ctx, scope, draft)
	if e != nil {
		return out, e
	}
	task := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, "REHEARSAL", key)
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "REHEARSAL_PREPARE", CommandKey: key, CommitKey: turnIdentity(task, "prepare"), GateVersions: g, PayloadHash: hash, Locks: keys, commandOnly: true}
	var m domain.RehearsalBinding
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		var e error
		m, e = s.DraftRehearsalOwner.BuildRehearsalTx(ctx, tx, scope, draft, request)
		if e != nil {
			return e
		}
		return errRehearsalInspected
	})
	if !errors.Is(e, errRehearsalInspected) {
		return out, e
	}
	evidence, e := s.DraftRehearsalReader.ReadRehearsalSourceEvidence(ctx, scope, m, request)
	if e != nil {
		return out, e
	}
	defer evidence.Close()
	ctx, service, done, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	quote := turnIdentity(task, "quote")
	r.Operation = "REHEARSAL_INTAKE"
	r.CommitKey = turnIdentity(task, "accepted")
	r.originalSettlementService = &service
	r.rehearsalIntake = &rehearsalIntakeProvision{m, request}
	r.Locks = append(r.Locks, harnessTaskKey(scope, task), harnessTaskKey(scope, quote), LockKey{Rank: 90, Kind: "content_project", IDs: []string{scope.TenantID, scope.ProductID, m.ProjectID}})
	holds := []Reservation{}
	for _, a := range r0CandidateAccounts(scope, m.ProjectID) {
		r.Locks = append(r.Locks, LockKey{Rank: 70, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID}})
		if a.Kind != "supplier_global" && a.Kind != "supplier_tenant" {
			a.ReservationID = turnIdentity(task, "parent", a.Kind)
			holds = append(holds, a)
		}
	}
	r.Locks = append(r.Locks, holdLocks(holds)...)
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.checkRehearsalIntakeFinish(ctx, u, r); e != nil {
			return e
		}
		if e := CheckNewRouteAdmissionTx(ctx, tx, scope, domain.InstalledMockConnection, domain.InstalledMockModel); e != nil {
			return e
		}
		if e := rehearsalProjectIdentity(ctx, u.tx, scope, m); e != nil {
			return e
		}
		var now time.Time
		if e := u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		if e := u.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: scope, OwnerSubjectID: scope.SubjectID}, TaskID: task, ProjectID: m.ProjectID, Kind: "ADMIN", NextRunAt: now}, "REHEARSAL"); e != nil {
			return e
		}
		var route [32]byte
		decoded, e := hex.DecodeString(m.RouteHash)
		if e != nil || len(decoded) != 32 {
			return ErrDenied
		}
		copy(route[:], decoded)
		q := HarnessQuote{ID: quote, ProjectID: m.ProjectID, Kind: "REHEARSAL", Inputs: len(request.Inputs), RequestHash: hash, RouteHash: route}
		qraw, _, e := harnessPayload(q)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_quotes(tenant_id,product_id,quote_id,owner_subject_id,project_id,document,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, quote, scope.SubjectID, m.ProjectID, qraw, now, now.Add(time.Minute)); e != nil {
			return e
		}
		intake := HarnessIntake{TaskID: task, QuoteID: quote, RequestHash: hash, RouteHash: route, ParentHolds: holds}
		raw, _, e := harnessPayload(intake)
		if e != nil {
			return e
		}
		for _, h := range holds {
			if e = reserveHarnessHold(ctx, u.tx, scope.TenantID, scope.ProductID, task, "PARENT", h); e != nil {
				return e
			}
		}
		execute := now.Add(time.Duration(limits.Seconds) * time.Second)
		settle := execute.Add(time.Minute)
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_envelopes(tenant_id,product_id,task_id,quote_id,document,accepted_at,execute_by,settle_by,max_calls,max_steps,supplier_cap) VALUES(?,?,?,?,?,?,?,?,?,?,0)", scope.TenantID, scope.ProductID, task, quote, raw, now, execute, settle, limits.Calls, limits.Steps); e != nil {
			return e
		}
		binding, e := json.Marshal(m)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_admin_rehearsals(tenant_id,product_id,task_id,owner_subject_id,original_key,command_hash,draft_id,binding,accepted_commit_key) VALUES(?,?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, task, scope.SubjectID, key, hash[:], draft, binding, r.CommitKey); e != nil {
			return e
		}
		body, e := json.Marshal(request)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_rehearsal_input_sources(tenant_id,product_id,task_id,draft_id,document) VALUES(?,?,?,?,?)", scope.TenantID, scope.ProductID, task, draft, body); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_settlement_bindings VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, task, service.Installation, service.ServiceID, service.Policy); e != nil {
			return e
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_cutoff_work(tenant_id,product_id,task_id,next_run_at) VALUES(?,?,?,?)", scope.TenantID, scope.ProductID, task, settle)
		return e
	})
	if e != nil {
		return out, e
	}
	return s.originalRehearsal(ctx, scope, key, hash)
}

var errRehearsalInspected = errors.New("rehearsal source inspection completed without mutation")

func (u *unit) PurgeRehearsalInputs(ctx context.Context, scope domain.ProductScope, draft string) error {
	if u.scope != scope || !u.commandOnly {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "content_draft", IDs: []string{scope.TenantID, scope.ProductID, draft}}); e != nil {
		return e
	}
	var deleted bool
	if e := u.tx.QueryRowContext(ctx, "SELECT deleted FROM content_drafts WHERE tenant_id=? AND product_id=? AND draft_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, draft, scope.SubjectID).Scan(&deleted); e != nil {
		return e
	}
	if !deleted {
		return ErrDenied
	}
	_, e := u.tx.ExecContext(ctx, "DELETE FROM harness_rehearsal_input_sources WHERE tenant_id=? AND product_id=? AND draft_id=?", scope.TenantID, scope.ProductID, draft)
	return e
}

func (s *Store) rehearsalGates(ctx context.Context, scope domain.ProductScope) (GateVersions, error) {
	var g GateVersions
	if e := s.CheckOperator(ctx, scope, "content.rehearse"); e != nil {
		return g, e
	}
	var ok bool
	e := s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled AND s.can_execute FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &ok)
	if e == nil && !ok {
		e = ErrDenied
	}
	return g, e
}
func (s *Store) originalRehearsal(ctx context.Context, scope domain.ProductScope, key string, hash [32]byte) (out generated.AdminTaskReceipt, err error) {
	var id, state string
	var stored []byte
	err = s.DB.QueryRowContext(ctx, "SELECT a.task_id,t.state,a.command_hash FROM harness_admin_rehearsals a JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN commit_receipts c ON c.commit_key=a.accepted_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.owner_subject_id=a.owner_subject_id AND c.command_key=a.original_key AND c.payload_hash=a.command_hash AND c.operation='REHEARSAL_INTAKE' AND c.phase='COMMIT' AND c.scope_kind='PRODUCT' AND c.world_instance_id IS NULL WHERE a.tenant_id=? AND a.product_id=? AND a.owner_subject_id=? AND a.original_key=? AND t.owner_subject_id=a.owner_subject_id AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='REHEARSAL'", scope.TenantID, scope.ProductID, scope.SubjectID, key).Scan(&id, &state, &stored)
	if err != nil {
		return
	}
	if !bytes.Equal(stored, hash[:]) {
		err = ErrConflict
		return
	}
	out = generated.AdminTaskReceipt{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(id), TaskKind: "REHEARSAL", TaskState: state, CorrelationID: generated.Identity(id)}
	return
}
func (s *Store) checkRehearsalIntakeFinish(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalIntake
	if p == nil {
		return nil
	}
	if s.DraftRehearsalOwner == nil || !r.commandOnly || r.WorldScope != nil || r.Operation != "REHEARSAL_INTAKE" {
		return ErrDenied
	}
	m, e := s.DraftRehearsalOwner.BuildRehearsalTx(ctx, u, r.ProductScope, p.binding.DraftID, p.request)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(m, p.binding) {
		return ErrReplan
	}
	return nil
}
func rehearsalProjectIdentity(ctx context.Context, tx *sql.Tx, scope domain.ProductScope, m domain.RehearsalBinding) error {
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" {
		return ErrDenied
	}
	var ok bool
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM content_drafts d JOIN world_projects p ON p.tenant_id=d.tenant_id AND p.product_id=d.product_id AND p.project_id=d.project_id WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND p.owner_subject_id=d.owner_subject_id AND d.project_id=? AND d.deleted=0)", scope.TenantID, scope.ProductID, m.DraftID, scope.SubjectID, m.ProjectID).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return nil
}
func provisionR0RehearsalAccount(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	if r.rehearsalIntake == nil || !r.commandOnly || r.WorldScope != nil || r.Operation != "REHEARSAL_INTAKE" {
		return ErrDenied
	}
	m := r.rehearsalIntake.binding
	if e := rehearsalProjectIdentity(ctx, tx, r.ProductScope, m); e != nil {
		return e
	}
	valid := false
	for _, a := range r0CandidateAccounts(r.ProductScope, m.ProjectID) {
		valid = valid || (a.Kind == k.Kind && a.ScopeKey == k.IDs[0] && a.AccountID == k.IDs[1])
	}
	if !valid {
		return ErrDenied
	}
	var tenant, product, project any
	if k.Kind != "supplier_global" && k.Kind != "platform_risk" {
		tenant = r.ProductScope.TenantID
		if k.Kind != "supplier_tenant" {
			product = r.ProductScope.ProductID
		}
	}
	if k.Kind == "project" {
		project = m.ProjectID
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,project_id,balance,held) VALUES(?,?,?,?,?,?,0,0)", k.Kind, k.IDs[0], k.IDs[1], tenant, product, project)
	return e
}

func (s *Store) InspectRehearsalSourceMaterial(ctx context.Context, scope domain.ProductScope, m domain.RehearsalBinding, request generated.RehearsalRequest) (out []CandidateSourceAsset, err error) {
	if s.Guard == nil || s.DraftRehearsalOwner == nil || len(m.Sources) == 0 {
		return nil, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer close()
	g, e := s.rehearsalGates(ctx, scope)
	if e != nil {
		return nil, e
	}
	keys, e := s.DraftRehearsalOwner.PlanRehearsalLocks(ctx, scope, m.DraftID)
	if e != nil {
		return nil, e
	}
	key := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, m.DraftID, m.SourceHash, "rehearsal-source-inspection")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "REHEARSAL_SOURCE_INSPECT", CommandKey: key, CommitKey: key, GateVersions: g, Locks: keys, commandOnly: true}
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		u.routePolicyFrozen = true
		u.routePolicy = m.RoutePolicy
		current, e := s.DraftRehearsalOwner.BuildRehearsalTx(ctx, tx, scope, m.DraftID, request)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(current, m) {
			return ErrReplan
		}
		out, e = candidateSourceAssetsTx(ctx, tx.(*unit), scope, domain.DraftCandidateMaterial{Sources: m.Sources})
		if e != nil {
			return e
		}
		return errRehearsalInspected
	})
	if !errors.Is(e, errRehearsalInspected) {
		if e == nil {
			e = ErrDenied
		}
		return nil, e
	}
	return out, nil
}
