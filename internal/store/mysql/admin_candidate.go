package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

type DraftCandidateOwner interface {
	PlanDraftGenerationLocks(context.Context, domain.ProductScope, domain.DraftCandidateSelection) ([]LockKey, error)
	InspectDraftGenerationTx(context.Context, Tx, domain.ProductScope, domain.DraftCandidateSelection) (domain.DraftGeneration, error)
	PlanCandidateLocks(context.Context, domain.ProductScope, domain.DraftCandidateSelection) ([]LockKey, error)
	BuildCandidateTx(context.Context, Tx, domain.ProductScope, domain.DraftCandidateSelection) (domain.DraftCandidateMaterial, error)
	ValidateCandidateTx(context.Context, Tx, domain.ProductScope, domain.DraftCandidateSelection, []byte) (string, error)
	PlanCandidateHistoryLocks(context.Context, domain.ProductScope, domain.DraftCandidateMaterial) ([]LockKey, error)
	InspectCandidateSourcesTx(context.Context, Tx, domain.ProductScope, domain.DraftCandidateMaterial) error
	CandidateSourcesLiveTx(context.Context, Tx, domain.ProductScope, domain.DraftCandidateMaterial) (bool, error)
}

func (s *Store) AcceptDraftCandidate(ctx context.Context, scope domain.ProductScope, draft, key string, request generated.CandidateRequest) (generated.AdminTaskReceipt, error) {
	var out generated.AdminTaskReceipt
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if s.DraftCandidateOwner == nil || s.SettlementAuthority == nil || scope.Validate() != nil || domain.ValidateID(draft) != nil || domain.ValidateIdempotencyKey(key) != nil || string(request.ProductID) != scope.ProductID {
		return out, ErrDenied
	}
	if e = typedJSON("CandidateRequest", request); e != nil {
		return out, e
	}
	_, hash, e := harnessPayload(struct {
		Scope   domain.ProductScope
		Draft   string
		Request generated.CandidateRequest
	}{scope, draft, request})
	if e != nil {
		return out, e
	}
	g, e := s.candidateGates(ctx, scope)
	if e != nil {
		return out, e
	}
	if original, e := s.originalCandidate(ctx, scope, key, hash); e == nil {
		return original, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	revision, e := strconv.ParseUint(string(request.ExpectedDraftRevision), 10, 64)
	if e != nil {
		return out, e
	}
	selection := domain.DraftCandidateSelection{DraftID: draft, Revision: revision, ObjectID: string(request.ObjectID), ModuleID: request.ModuleID, Fields: append([]string{}, request.SelectedFields...)}
	locks, e := s.DraftCandidateOwner.PlanCandidateLocks(ctx, scope, selection)
	if e != nil {
		return out, e
	}
	task := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, "CANDIDATE", key)
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "CANDIDATE_PREPARE", CommandKey: key, CommitKey: turnIdentity(task, "prepare"), GateVersions: g, PayloadHash: hash, Locks: locks, commandOnly: true}
	var material domain.DraftCandidateMaterial
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		var e error
		material, e = s.DraftCandidateOwner.BuildCandidateTx(ctx, tx, scope, selection)
		if e != nil {
			return e
		}
		return errCandidatePrepared
	})
	if !errors.Is(e, errCandidatePrepared) {
		return out, e
	}
	if s.DraftCandidateReader == nil {
		return out, ErrDenied
	}
	sourceEvidence, e := s.DraftCandidateReader.ReadCandidateSourceEvidence(ctx, scope, material)
	if e != nil {
		return out, e
	}
	defer sourceEvidence.Close()
	ctx, service, done, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	quote := turnIdentity(task, "quote")
	r.Operation = "CANDIDATE_INTAKE"
	r.CommitKey = turnIdentity(task, "accepted")
	r.originalSettlementService = &service
	r.candidateIntake = &candidateIntakeProvision{DraftID: draft, ProjectID: material.ProjectID, selection: selection, requestHash: material.RequestHash}
	r.Locks = append(r.Locks, harnessTaskKey(scope, task), harnessTaskKey(scope, quote), LockKey{Rank: 90, Kind: "content_project", IDs: []string{scope.TenantID, scope.ProductID, material.ProjectID}})
	holds := []Reservation{}
	for _, a := range r0CandidateAccounts(scope, material.ProjectID) {
		r.Locks = append(r.Locks, LockKey{Rank: 70, Kind: a.Kind, IDs: []string{a.ScopeKey, a.AccountID}})
		if a.Kind != "supplier_global" && a.Kind != "supplier_tenant" {
			a.ReservationID = turnIdentity(task, "parent", a.Kind)
			holds = append(holds, a)
		}
	}
	r.Locks = append(r.Locks, holdLocks(holds)...)
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		current, e := s.DraftCandidateOwner.BuildCandidateTx(ctx, tx, scope, selection)
		if e != nil {
			return e
		}
		if current.ProjectID != material.ProjectID || current.RequestHash != material.RequestHash || !bytes.Equal(current.Canonical, material.Canonical) {
			return ErrReplan
		}
		if e = ensureCandidateProject(ctx, u, *r.candidateIntake); e != nil {
			return e
		}
		var now time.Time
		if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		if e = u.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: scope, OwnerSubjectID: scope.SubjectID}, TaskID: task, ProjectID: material.ProjectID, Kind: "ADMIN", NextRunAt: now}, "CANDIDATE"); e != nil {
			return e
		}
		var rh [32]byte
		decoded, e := hex.DecodeString(material.RouteHash)
		if e != nil || len(decoded) != 32 {
			return ErrDenied
		}
		copy(rh[:], decoded)
		q := HarnessQuote{ID: quote, ProjectID: material.ProjectID, Kind: "CANDIDATE", RequestHash: hash, RouteHash: rh}
		qraw, _, e := harnessPayload(q)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_quotes(tenant_id,product_id,quote_id,owner_subject_id,project_id,document,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, quote, scope.SubjectID, material.ProjectID, qraw, now, now.Add(time.Minute)); e != nil {
			return e
		}
		intake := HarnessIntake{TaskID: task, QuoteID: quote, RequestHash: hash, RouteHash: rh, ParentHolds: holds}
		iraw, _, e := harnessPayload(intake)
		if e != nil {
			return e
		}
		for _, h := range holds {
			if e = reserveHarnessHold(ctx, u.tx, scope.TenantID, scope.ProductID, task, "PARENT", h); e != nil {
				return e
			}
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_envelopes(tenant_id,product_id,task_id,quote_id,document,accepted_at,execute_by,settle_by,max_calls,max_steps,supplier_cap) VALUES(?,?,?,?,?,?,?,?,2,20,0)", scope.TenantID, scope.ProductID, task, quote, iraw, now, now.Add(120*time.Second), now.Add(180*time.Second)); e != nil {
			return e
		}
		selectionRaw, _ := json.Marshal(selection)
		material.Canonical = nil
		binding, _ := json.Marshal(material)
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_admin_candidates(tenant_id,product_id,task_id,owner_subject_id,original_key,command_hash,draft_id,selection,binding,accepted_commit_key) VALUES(?,?,?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, task, scope.SubjectID, key, hash[:], draft, selectionRaw, binding, r.CommitKey); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_settlement_bindings VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, task, service.Installation, service.ServiceID, service.Policy); e != nil {
			return e
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_cutoff_work(tenant_id,product_id,task_id,next_run_at) VALUES(?,?,?,?)", scope.TenantID, scope.ProductID, task, now.Add(180*time.Second))
		return e
	})
	if e != nil {
		return out, e
	}
	return s.originalCandidate(ctx, scope, key, hash)
}

var errCandidatePrepared = errors.New("candidate current source inspected")

type candidateIntakeProvision struct {
	DraftID, ProjectID string
	selection          domain.DraftCandidateSelection
	requestHash        string
}

func (s *Store) checkCandidateIntakeFinish(ctx context.Context, u *unit, r BarrierRequest) error {
	if r.candidateIntake == nil {
		return nil
	}
	if u == nil || u.tx == nil || s.DraftCandidateOwner == nil || r.Operation != "CANDIDATE_INTAKE" || !r.commandOnly || r.WorldScope != nil {
		return ErrDenied
	}
	current, e := s.DraftCandidateOwner.BuildCandidateTx(ctx, u, r.ProductScope, r.candidateIntake.selection)
	if e != nil {
		return e
	}
	if current.RequestHash != r.candidateIntake.requestHash || current.ProjectID != r.candidateIntake.ProjectID {
		return ErrConflict
	}
	return nil
}

func (s *Store) candidateGates(ctx context.Context, scope domain.ProductScope) (GateVersions, error) {
	var g GateVersions
	var allowed bool
	e := s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled AND s.can_execute AND EXISTS(SELECT 1 FROM operator_credentials c JOIN operator_actions a USING(credential_id) WHERE c.credential_id='owner' AND c.tenant_id=s.tenant_id AND c.product_id=s.product_id AND c.subject_id=s.subject_id AND c.enabled=1 AND a.action='content.assist') FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &allowed)
	if e != nil {
		return g, e
	}
	if !allowed {
		return g, ErrDenied
	}
	return g, nil
}
func (s *Store) originalCandidate(ctx context.Context, scope domain.ProductScope, key string, hash [32]byte) (generated.AdminTaskReceipt, error) {
	var out generated.AdminTaskReceipt
	var id, state string
	var stored []byte
	e := s.DB.QueryRowContext(ctx, "SELECT a.task_id,t.state,a.command_hash FROM harness_admin_candidates a JOIN tasks t USING(tenant_id,product_id,task_id) JOIN commit_receipts c ON c.commit_key=a.accepted_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.owner_subject_id=a.owner_subject_id AND c.command_key=a.original_key AND c.payload_hash=a.command_hash AND c.operation='CANDIDATE_INTAKE' AND c.phase='COMMIT' AND c.scope_kind='PRODUCT' AND c.world_instance_id IS NULL WHERE a.tenant_id=? AND a.product_id=? AND a.owner_subject_id=? AND a.original_key=? AND t.owner_subject_id=a.owner_subject_id AND t.world_instance_id IS NULL", scope.TenantID, scope.ProductID, scope.SubjectID, key).Scan(&id, &state, &stored)
	if e != nil {
		return out, e
	}
	if !bytes.Equal(stored, hash[:]) {
		return out, ErrConflict
	}
	return generated.AdminTaskReceipt{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(id), TaskKind: "CANDIDATE", TaskState: state, CorrelationID: generated.Identity(id)}, nil
}
func r0CandidateAccounts(scope domain.ProductScope, project string) []Reservation {
	product := scope.TenantID + "." + scope.ProductID
	return []Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "r0-mock-supplier"}, {Kind: "supplier_tenant", ScopeKey: scope.TenantID, AccountID: "r0-mock-supplier"}, {Kind: "platform_risk", ScopeKey: "GLOBAL", AccountID: "r0-mock-risk"}, {Kind: "customer", ScopeKey: product, AccountID: "r0-owner"}, {Kind: "project", ScopeKey: product, AccountID: project}}
}
func candidateProjectIdentity(ctx context.Context, tx *sql.Tx, scope domain.ProductScope, p candidateIntakeProvision) (bool, error) {
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" || scope.SubjectID != "creator" {
		return false, ErrDenied
	}
	var original sql.NullString
	if e := tx.QueryRowContext(ctx, "SELECT project_id FROM content_drafts WHERE tenant_id=? AND product_id=? AND draft_id=? AND owner_subject_id=? AND deleted=0", scope.TenantID, scope.ProductID, p.DraftID, scope.SubjectID).Scan(&original); e != nil {
		return false, e
	}
	want := original.String
	if !original.Valid {
		want = turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, "R0_AUTHORING_PROJECT")
	}
	if want != p.ProjectID {
		return false, ErrDenied
	}
	return !original.Valid, nil
}
func ensureCandidateProject(ctx context.Context, u *unit, p candidateIntakeProvision) error {
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "content_project", IDs: []string{u.scope.TenantID, u.scope.ProductID, p.ProjectID}}); e != nil {
		return e
	}
	fallback, e := candidateProjectIdentity(ctx, u.tx, u.scope, p)
	if e != nil {
		return e
	}
	var owner, name string
	e = u.tx.QueryRowContext(ctx, "SELECT owner_subject_id,name FROM world_projects WHERE tenant_id=? AND product_id=? AND project_id=?", u.scope.TenantID, u.scope.ProductID, p.ProjectID).Scan(&owner, &name)
	if e == nil {
		if owner != u.scope.SubjectID {
			return ErrDenied
		}
		if fallback {
			var purpose string
			if name != "R0 workspace authoring budget" {
				return ErrConflict
			}
			if e = u.tx.QueryRowContext(ctx, "SELECT purpose FROM harness_authoring_projects WHERE tenant_id=? AND product_id=? AND project_id=? AND owner_subject_id=?", u.scope.TenantID, u.scope.ProductID, p.ProjectID, u.scope.SubjectID).Scan(&purpose); e != nil || purpose != "R0_AUTHORING_PROJECT" {
				return ErrConflict
			}
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) || !fallback {
		return ErrDenied
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO world_projects VALUES(?,?,?,?,'R0 workspace authoring budget')", u.scope.TenantID, u.scope.ProductID, p.ProjectID, u.scope.SubjectID); e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_authoring_projects VALUES(?,?,?,?,'R0_AUTHORING_PROJECT')", u.scope.TenantID, u.scope.ProductID, p.ProjectID, u.scope.SubjectID)
	return e
}
func provisionR0CandidateAccount(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	if r.candidateIntake == nil || !r.commandOnly || r.WorldScope != nil || r.Operation != "CANDIDATE_INTAKE" {
		return ErrDenied
	}
	if _, e := candidateProjectIdentity(ctx, tx, r.ProductScope, *r.candidateIntake); e != nil {
		return e
	}
	valid := false
	for _, a := range r0CandidateAccounts(r.ProductScope, r.candidateIntake.ProjectID) {
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
		project = r.candidateIntake.ProjectID
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,project_id,balance,held) VALUES(?,?,?,?,?,?,0,0)", k.Kind, k.IDs[0], k.IDs[1], tenant, product, project)
	return e
}
