package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
	"strconv"
)

// This value has no caller-supplied body or persistent authority. Only the
// installed Files owner can consume its own live concrete evidence.
type RehearsalReportEvidence interface{ Close() }
type RehearsalReportChild struct {
	Index  int
	Commit domain.RehearsalChildOutcome
	Body   []byte
}
type RehearsalReportReader interface {
	ReadRehearsalReportEvidence(context.Context, domain.TaskLease, string) (RehearsalReportEvidence, error)
	RehearsalReportChildren(RehearsalReportEvidence, domain.TaskLease, string) ([]RehearsalReportChild, error)
	RehearsalReportContext(RehearsalReportEvidence, domain.TaskLease, string) (context.Context, error)
	StageRehearsalReport(RehearsalReportEvidence, domain.TaskLease, string, []byte) error
	RehearsalReportArtifact(RehearsalReportEvidence, domain.TaskLease, string) (RehearsalReportArtifact, error)
}

type RehearsalReportArtifact struct {
	Storage, Hash string
	Body          []byte
}

type rehearsalReportInspection struct {
	root       domain.TaskLease
	children   []BarrierRequest
	expected   []domain.RehearsalChildOutcome
	assertions []domain.RehearsalAssertion
	counts     []int
	keys       map[string]bool
	holds      []reportHold
	evidence   RehearsalReportEvidence
	applied    *unit
	artifact   RehearsalReportArtifact
}
type reportHold struct {
	Reservation
	task, purpose string
}

// Inspection is not a report business COMMIT. It proves the entire currently
// locked child/source/financial set in one UoW, with no nested transaction/IO.
func (s *Store) InspectRehearsalReport(ctx context.Context, root domain.TaskLease, worker string) ([]domain.RehearsalChildOutcome, error) {
	if s.RehearsalReportReader == nil {
		return nil, ErrDenied
	}
	evidence, e := s.RehearsalReportReader.ReadRehearsalReportEvidence(ctx, root, worker)
	if e != nil {
		return nil, e
	}
	defer evidence.Close()
	r, e := s.planRehearsalReport(ctx, evidence, root, worker)
	if e != nil {
		return nil, e
	}
	ctx, e = s.RehearsalReportReader.RehearsalReportContext(evidence, root, worker)
	if e != nil {
		return nil, e
	}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		if e := s.inspectRehearsalReportTx(ctx, port.(*unit), r); e != nil {
			return e
		}
		return errApprovedInspected
	})
	if !errors.Is(e, errApprovedInspected) {
		if e == nil {
			e = ErrDenied
		}
		return nil, e
	}
	return r.rehearsalReport.expected, nil
}

func (s *Store) planRehearsalReport(ctx context.Context, evidence RehearsalReportEvidence, root domain.TaskLease, worker string) (BarrierRequest, error) {
	empty := BarrierRequest{}
	children, e := s.RehearsalReportReader.RehearsalReportChildren(evidence, root, worker)
	if e != nil {
		return empty, e
	}
	ctx, e = s.RehearsalReportReader.RehearsalReportContext(evidence, root, worker)
	if e != nil {
		return empty, e
	}
	plan := &rehearsalReportInspection{root: root, keys: map[string]bool{}, evidence: evidence}
	keys := []LockKey{}
	for index, child := range children {
		if index != child.Index {
			return empty, ErrDenied
		}
		world := turnIdentity(root.TaskID, "rehearsal-world")
		lease := domain.TaskLease{ProductScope: root.ProductScope, WorldInstanceID: &world, OwnerSubjectID: root.OwnerSubjectID, TaskID: turnIdentity(root.TaskID, "rehearsal-child", strconv.Itoa(index))}
		r, e := s.rehearsalChildBarrier(ctx, root, lease, worker)
		if e != nil {
			return empty, e
		}
		r.commandOnly = true
		r.rehearsalReportRead = true
		r.Operation = "REHEARSAL_CHILD_RESULT_READ"
		r.CommandKey = turnIdentity(lease.TaskID, "parent-result-read")
		r.CommitKey = r.CommandKey
		plan.children = append(plan.children, r)
		plan.expected = append(plan.expected, child.Commit)
		keys = append(keys, r.Locks...)
		keys = append(keys, harnessTaskKey(root.ProductScope, lease.TaskID))
		metas := []domain.MockResultMetadata{child.Commit.Metadata}
		if child.Commit.OriginalDependency != nil {
			metas = append(metas, *child.Commit.OriginalDependency)
		}
		plan.counts = append(plan.counts, len(metas))
		for _, m := range metas {
			keys = append(keys, LockKey{Rank: 60, IDs: []string{root.TenantID, root.ProductID, lease.TaskID, m.Identity.AttemptID, m.Identity.AuthorizationID}})
		}
	}
	if len(children) < 1 || len(children) > 20 {
		return empty, ErrDenied
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT r.task_id,r.kind,r.scope_key,r.account_id,r.reservation_id,r.amount,r.purpose FROM reservations r WHERE r.tenant_id=? AND r.product_id=? AND (r.task_id=? OR EXISTS(SELECT 1 FROM harness_rehearsal_children c WHERE c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.child_task_id=r.task_id AND c.root_task_id=?)) ORDER BY r.kind,r.scope_key,r.account_id,r.reservation_id LIMIT 84", root.TenantID, root.ProductID, root.TaskID, root.TaskID)
	if e != nil {
		return empty, e
	}
	for rows.Next() {
		var h reportHold
		if e = rows.Scan(&h.task, &h.Kind, &h.ScopeKey, &h.AccountID, &h.ReservationID, &h.Amount, &h.purpose); e != nil {
			rows.Close()
			return empty, e
		}
		plan.holds = append(plan.holds, h)
		keys = append(keys, holdLocks([]Reservation{h.Reservation})...)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return empty, e
	}
	for _, k := range keys {
		plan.keys[string(lockBytes(k))] = true
	}
	first := plan.children[0]
	key := turnIdentity(root.TaskID, "report-inspection")
	r := BarrierRequest{ProductScope: root.ProductScope, OwnerSubjectID: root.OwnerSubjectID, TaskID: root.TaskID, WorkerID: worker, Fence: root.Fence, GateVersions: first.GateVersions, Operation: "REHEARSAL_REPORT_INSPECT", CommandKey: key, CommitKey: key, Locks: keys, rehearsalInitialization: &first.rehearsalExecution.initialization, rehearsalReport: plan}
	_, r.PayloadHash, e = harnessPayload(plan.expected)
	if e != nil {
		return empty, e
	}
	return r, nil
}

func (s *Store) inspectRehearsalReportTx(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalReport
	if p == nil || p.root.TaskID != r.TaskID || len(p.children) != len(p.expected) {
		return ErrDenied
	}
	// Locks keep other transactions out, but the owning body/finish may itself
	// change governance. Repeat the actual current gates at both boundaries.
	var gates bool
	if e := u.tx.QueryRowContext(ctx, "SELECT t.enabled AND t.permission_version=? AND p.enabled AND p.permission_version=? AND s.enabled AND s.can_execute AND s.permission_version=? AND s.privacy_epoch=? AND s.view_epoch=? FROM tenant_gates t JOIN product_gates p USING(tenant_id) JOIN subject_governance s USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", r.GateVersions.Tenant, r.GateVersions.Product, r.GateVersions.Subject, r.GateVersions.Privacy, r.GateVersions.View, r.ProductScope.TenantID, r.ProductScope.ProductID, r.OwnerSubjectID).Scan(&gates); e != nil {
		return e
	}
	if !gates {
		return ErrDenied
	}
	held, ok := ctx.Value(rehearsalReadKey{}).(*rehearsalReadAdmission)
	if !ok || held.closed.Load() || !held.report || held.store != s || held.root != p.root || held.worker != r.WorkerID {
		return ErrDenied
	}
	var current domain.Versions
	if e := u.tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.rehearsalInitialization.world).Scan(&current.ReleaseID, &current.WorldRevision, &current.ControlVersion, &current.PrivacyEpoch, &current.ViewEpoch); e != nil {
		return e
	}
	if e := s.checkRehearsalInitialization(ctx, u, r); e != nil {
		return e
	}
	var count, next int
	if e := u.tx.QueryRowContext(ctx, "SELECT a.next_input,(SELECT COUNT(*) FROM harness_rehearsal_children c WHERE c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.root_task_id=a.task_id) FROM harness_admin_rehearsals a WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&next, &count); e != nil {
		return e
	}
	if next != len(p.children) || count != next {
		return ErrDenied
	}
	suppliers := map[string]int{}
	expectedHolds := map[string]reportHold{}
	for _, a := range r0CandidateAccounts(r.ProductScope, r.rehearsalInitialization.binding.ProjectID) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			for _, m := range reportCosts(p) {
				h := a
				h.ReservationID = turnIdentity(m.Identity.AttemptID, "supplier", a.Kind)
				expectedHolds[h.ReservationID] = reportHold{h, m.Identity.TaskID, m.Identity.AttemptID}
			}
		} else {
			a.ReservationID = turnIdentity(r.TaskID, "parent", a.Kind)
			expectedHolds[a.ReservationID] = reportHold{a, r.TaskID, "PARENT"}
		}
	}
	for i, child := range p.children {
		if child.Versions == nil || *child.Versions != current {
			return ErrReplan
		}
		view := *u
		view.taskID = child.TaskID
		view.world = child.WorldScope.WorldInstanceID
		view.commandOnly = true
		view.rehearsalExecution = child.rehearsalExecution
		view.rehearsalReportRead = true
		view.rehearsalInitialization = nil
		if e := s.checkRehearsalReportRead(ctx, &view, child); e != nil {
			return e
		}
		actual, e := s.inspectRehearsalChildOutcomeTx(ctx, &view, child, p.counts[i])
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(actual, p.expected[i]) {
			return ErrConflict
		}
		if i == 0 {
			assertions, e := s.RehearsalInitializer.EvaluateRehearsalAssertionsTx(ctx, &view, *child.WorldScope)
			if e != nil {
				return e
			}
			if p.assertions != nil && !reflect.DeepEqual(p.assertions, assertions) {
				return ErrConflict
			}
			p.assertions = assertions
		}
		metas := []domain.MockResultMetadata{actual.Metadata}
		if actual.OriginalDependency != nil {
			metas = append(metas, *actual.OriginalDependency)
		}
		for _, m := range metas {
			if !validOriginalUsageMetadata(minimalOriginalMetadata(m)) {
				return ErrDenied
			}
			var raw []byte
			var saved domain.MockResultMetadata
			if e := u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, child.TaskID, m.Identity.AttemptID).Scan(&raw); e != nil {
				return e
			}
			if json.Unmarshal(raw, &saved) != nil || saved != m {
				return ErrConflict
			}
			suppliers[child.TaskID+"/"+m.Identity.AttemptID] = 0
		}
	}
	if len(p.holds) != 3+2*len(suppliers) {
		return ErrReplan
	}
	seen := map[string]bool{}
	for _, h := range p.holds {
		if want, ok := expectedHolds[h.ReservationID]; !ok || want != h {
			return ErrDenied
		}
		var amount int64
		var state, purpose, task string
		if e := u.tx.QueryRowContext(ctx, "SELECT task_id,amount,status,purpose FROM reservations WHERE tenant_id=? AND product_id=? AND kind=? AND scope_key=? AND account_id=? AND reservation_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID).Scan(&task, &amount, &state, &purpose); e != nil {
			return e
		}
		wantState := "HELD"
		if p.applied != nil {
			wantState = reportHoldState(p, h)
		}
		if task != h.task || purpose != h.purpose || amount != h.Amount || amount != 0 || state != wantState {
			return ErrDenied
		}
		key := task + "/" + purpose + "/" + h.Kind
		if seen[key] {
			return ErrDenied
		}
		seen[key] = true
		if task == r.TaskID {
			if purpose != "PARENT" || (h.Kind != "customer" && h.Kind != "project" && h.Kind != "platform_risk") {
				return ErrDenied
			}
		} else {
			key := task + "/" + purpose
			if _, ok := suppliers[key]; !ok || (h.Kind != "supplier_global" && h.Kind != "supplier_tenant") {
				return ErrDenied
			}
			suppliers[key]++
		}
	}
	for _, kind := range []string{"customer", "project", "platform_risk"} {
		if !seen[r.TaskID+"/PARENT/"+kind] {
			return ErrDenied
		}
	}
	for _, n := range suppliers {
		if n != 2 {
			return ErrDenied
		}
	}
	return nil
}
