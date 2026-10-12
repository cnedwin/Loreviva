package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
)

type CommittedRehearsalReport struct {
	Asset    generated.FileRecord
	Storage  string
	Children []domain.RehearsalChildOutcome
	Sources  []CandidateSourceAsset
}
type rehearsalHistoryPlan struct {
	init                 rehearsalInitialization
	children             []BarrierRequest
	keys                 map[string]bool
	versions             domain.Versions
	storage, hash, asset string
	evidenceHash         []byte
}

func (u *unit) RehearsalReportHistoryBasis(s domain.Scope) (*domain.RehearsalBasis, error) {
	if u.rehearsalHistory == nil {
		return nil, nil
	}
	if !u.commandOnly || !u.rehearsalReportRead || u.rehearsalExecution == nil || u.scope != s.ProductScope || u.rehearsalHistory.init.world != s.WorldInstanceID {
		return nil, ErrDenied
	}
	p := u.rehearsalExecution
	if p.initialization.root != u.rehearsalHistory.init.root {
		return nil, ErrDenied
	}
	return u.RehearsalBasis(s)
}

func acquireRehearsalHistoryWorld(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	p := r.rehearsalHistory
	if p == nil || !r.commandOnly || len(k.IDs) != 3 || k.IDs[2] != p.init.world {
		return ErrDenied
	}
	var v domain.Versions
	var owner, status, root string
	e := tx.QueryRowContext(ctx, "SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,i.owner_subject_id,x.root_task_id FROM world_controls w JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) JOIN harness_rehearsal_worlds x USING(tenant_id,product_id,world_instance_id) WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? AND i.active_migration_task IS NULL FOR UPDATE", r.ProductScope.TenantID, r.ProductScope.ProductID, p.init.world).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status, &owner, &root)
	if e != nil {
		return e
	}
	if owner != r.OwnerSubjectID || root != p.init.root || status != "RUNNING" || v != p.versions || v.ReleaseID != p.init.release {
		return ErrDenied
	}
	return nil
}

// S57's only terminal mint. No caller leases, child lists, history flags or
// callbacks. This projection is not a reusable permission to read any bytes.
func (s *Store) InspectCommittedRehearsalReport(ctx context.Context, scope domain.ProductScope, task string) (out CommittedRehearsalReport, err error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil || s.Guard == nil || s.DraftRehearsalOwner == nil || s.RehearsalInitializer == nil || s.RehearsalInputOwner == nil || s.ProviderEvidence == nil {
		return out, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	p := &rehearsalHistoryPlan{keys: map[string]bool{}}
	p.init.root = task
	p.init.world = turnIdentity(task, "rehearsal-world")
	p.init.release = turnIdentity(task, "rehearsal-version")
	var binding, input []byte
	e = s.DB.QueryRowContext(ctx, "SELECT a.binding,i.document,r.storage_key,r.body_hash,r.asset_id,r.evidence_hash FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN harness_rehearsal_reports r USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=? AND r.state='COMMITTED' AND r.delete_intent=0", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&binding, &input, &p.storage, &p.hash, &p.asset, &p.evidenceHash)
	if e != nil {
		return out, e
	}
	if json.Unmarshal(binding, &p.init.binding) != nil || generated.Decode("RehearsalRequest", input, &p.init.request) != nil || len(p.init.binding.Inputs) < 1 || len(p.init.binding.Inputs) > 20 || p.asset != turnIdentity(task, "report-asset") {
		return out, ErrDenied
	}
	var gates GateVersions
	var enabled bool
	e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil {
		return out, e
	}
	if !enabled {
		return out, ErrDenied
	}
	e = s.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", scope.TenantID, scope.ProductID, p.init.world).Scan(&p.versions.ReleaseID, &p.versions.WorldRevision, &p.versions.ControlVersion, &p.versions.PrivacyEpoch, &p.versions.ViewEpoch)
	if e != nil {
		return out, e
	}
	keys, e := s.DraftRehearsalOwner.PlanRehearsalHistoryLocks(ctx, scope, p.init.binding)
	if e != nil {
		return out, e
	}
	more, e := s.RehearsalInitializer.PlanRehearsalHistoryRuntimeLocks(ctx, scope, task)
	if e != nil {
		return out, e
	}
	keys = append(keys, more...)
	for index := range p.init.binding.Inputs {
		child := turnIdentity(task, "rehearsal-child", strconv.Itoa(index))
		var actor, source, quote string
		var actual int
		e = s.DB.QueryRowContext(ctx, "SELECT c.actor_id,c.input_source_id,h.quote_id,c.input_index FROM harness_rehearsal_children c JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? AND c.child_task_id=? AND c.world_instance_id=?", scope.TenantID, scope.ProductID, task, child, p.init.world).Scan(&actor, &source, &quote, &actual)
		if e != nil {
			return out, e
		}
		if actual != index {
			return out, ErrDenied
		}
		exec := &rehearsalExecution{root: domain.TaskLease{ProductScope: scope, TaskID: task, OwnerSubjectID: scope.SubjectID}, initialization: p.init, child: rehearsalChildStart{child: child, quote: quote, actor: actor, source: source, index: index}}
		world := domain.Scope{ProductScope: scope, WorldInstanceID: p.init.world}
		r := BarrierRequest{ProductScope: scope, WorldScope: &world, Versions: &p.versions, OwnerSubjectID: scope.SubjectID, TaskID: child, rehearsalExecution: exec, commandOnly: true, rehearsalReportRead: true}
		p.children = append(p.children, r)
		rows, e := s.DB.QueryContext(ctx, "SELECT a.attempt_id,z.authorization_id FROM attempts a JOIN dispatch_authorizations z USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", scope.TenantID, scope.ProductID, child)
		if e != nil {
			return out, e
		}
		n := 0
		for rows.Next() {
			var a, z string
			if e = rows.Scan(&a, &z); e != nil {
				rows.Close()
				return out, e
			}
			keys = append(keys, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, child, a, z}})
			n++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		if n < 1 || n > 2 {
			return out, ErrDenied
		}
	}
	keys = append(keys, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, p.asset}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, "staging." + p.storage}})
	for _, k := range keys {
		p.keys[string(lockBytes(k))] = true
	}
	key := turnIdentity(task, "report-history-read")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "REHEARSAL_REPORT_HISTORY_READ", CommandKey: key, CommitKey: key, GateVersions: gates, Locks: keys, commandOnly: true, rehearsalHistory: p}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		u := port.(*unit)
		if e := s.checkRehearsalHistoryRootTx(ctx, u, r, binding, input); e != nil {
			return e
		}
		if e := s.DraftRehearsalOwner.InspectRehearsalSourcesTx(ctx, u, scope, p.init.binding); e != nil {
			return e
		}
		for _, child := range p.children {
			view := *u
			view.taskID = child.TaskID
			view.world = p.init.world
			view.rehearsalExecution = child.rehearsalExecution
			view.rehearsalReportRead = true
			if e := checkRehearsalChildIdentity(ctx, &view, child); e != nil {
				return e
			}
			var valid bool
			e := u.tx.QueryRowContext(ctx, "SELECT c.root_generation=e.narrative_generation AND c.input_index<a.next_input AND cr.operation='REHEARSAL_CHILD_START' AND cr.phase='COMMIT' AND cr.scope_kind='PRODUCT' AND cr.world_instance_id IS NULL AND cr.task_id=c.root_task_id AND cr.owner_subject_id=h.owner_subject_id AND cr.command_key=c.accepted_receipt AND cr.payload_hash=h.command_hash FROM harness_rehearsal_children c JOIN harness_envelopes e ON e.tenant_id=c.tenant_id AND e.product_id=c.product_id AND e.task_id=c.root_task_id JOIN harness_admin_rehearsals a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.task_id=c.root_task_id JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id JOIN commit_receipts cr ON cr.commit_key=c.accepted_receipt AND cr.tenant_id=c.tenant_id AND cr.product_id=c.product_id WHERE c.tenant_id=? AND c.product_id=? AND c.child_task_id=?", scope.TenantID, scope.ProductID, child.TaskID).Scan(&valid)
			if e != nil {
				return e
			}
			if !valid {
				return ErrDenied
			}
			var n int
			if e = u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, child.TaskID).Scan(&n); e != nil {
				return e
			}
			if n < 1 || n > 2 {
				return ErrDenied
			}
			c, e := s.inspectRehearsalChildOutcomeTx(ctx, &view, child, n)
			if e != nil {
				return e
			}
			out.Children = append(out.Children, c)
		}
		_, h, e := harnessPayload(out.Children)
		if e != nil {
			return e
		}
		if !bytes.Equal(h[:], p.evidenceHash) {
			return ErrDenied
		}
		business := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "REHEARSAL_REPORT_COMMIT", CommandKey: turnIdentity(task, "report-commit"), CommitKey: turnIdentity(task, "report-commit")}
		_, business.PayloadHash, e = harnessPayload(struct {
			Children      []domain.RehearsalChildOutcome
			Storage, Hash string
		}{out.Children, p.storage, p.hash})
		if e != nil {
			return e
		}
		receipt, e := queryReceipt(ctx, u.tx, business.CommitKey)
		if e != nil {
			return e
		}
		if !receipt.matches(business) {
			return ErrDenied
		}
		costs := reportCosts(&rehearsalReportInspection{expected: out.Children})
		if !allOriginalUsageKnown(costs) {
			if e := checkOriginalServiceMeteringTx(ctx, u, scope, task, turnIdentity(task, "service"), business.CommitKey, costs); e != nil {
				return e
			}
		}
		var storedCommit, storedEvidence []byte
		if e = u.tx.QueryRowContext(ctx, "SELECT commit_hash,evidence_hash FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&storedCommit, &storedEvidence); e != nil {
			return e
		}
		if !bytes.Equal(storedCommit, business.PayloadHash[:]) || !bytes.Equal(storedEvidence, p.evidenceHash) {
			return ErrDenied
		}
		var doc []byte
		var state, storage, owner string
		if e = u.tx.QueryRowContext(ctx, "SELECT state,storage_key,owner_subject_id,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, p.asset).Scan(&state, &storage, &owner, &doc); e != nil {
			return e
		}
		if generated.Decode("FileRecord", doc, &out.Asset) != nil || state != "READY" || out.Asset.State != "READY" || storage != p.storage || owner != scope.SubjectID || string(out.Asset.OwnerSubjectID) != owner || out.Asset.Scope.TenantID != scope.TenantID || out.Asset.Scope.ProductID != scope.ProductID || string(out.Asset.AssetID) != p.asset || out.Asset.Sha256 != p.hash || out.Asset.AudiencePolicy != "PRODUCT_EDITORS" {
			return ErrDenied
		}
		out.Storage = p.storage
		out.Sources, e = candidateSourceAssetsTx(ctx, u, scope, domain.DraftCandidateMaterial{Sources: p.init.binding.Sources})
		if e != nil {
			return e
		}
		return errApprovedInspected
	})
	if !errors.Is(e, errApprovedInspected) {
		if e == nil {
			e = ErrDenied
		}
		return CommittedRehearsalReport{}, e
	}
	for _, c := range out.Children {
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append(metas, *c.OriginalDependency)
		}
		for _, m := range metas {
			current, e := s.ProviderEvidence.OriginalMetadata(ctx, m.Identity)
			if e != nil {
				return CommittedRehearsalReport{}, e
			}
			if current != m {
				return CommittedRehearsalReport{}, ErrDenied
			}
		}
	}
	return out, nil
}

func (s *Store) checkRehearsalHistoryRootTx(ctx context.Context, u *unit, r BarrierRequest, binding, input []byte) error {
	p := r.rehearsalHistory
	var privacy, view uint64
	var stageKey string
	if e := u.tx.QueryRowContext(ctx, "SELECT subject_privacy,subject_view,stage_receipt FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&privacy, &view, &stageKey); e != nil {
		return e
	}
	if privacy != r.GateVersions.Privacy || view != r.GateVersions.View || stageKey != turnIdentity(r.TaskID, "report-stage") {
		return ErrDenied
	}
	stage := BarrierRequest{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, TaskID: r.TaskID, Operation: "REHEARSAL_REPORT_STAGE", CommandKey: stageKey, CommitKey: stageKey}
	_, stage.PayloadHash, _ = harnessPayload(struct {
		Storage, Hash string
		Privacy, View uint64
	}{p.storage, p.hash, privacy, view})
	saved, stageErr := queryReceipt(ctx, u.tx, stageKey)
	if stageErr != nil {
		return stageErr
	}
	if !saved.matches(stage) {
		return ErrDenied
	}
	var b, in, hash []byte
	var original string
	var next, count int
	var valid bool
	e := u.tx.QueryRowContext(ctx, "SELECT a.binding,i.document,a.command_hash,a.original_key,a.next_input,(SELECT COUNT(*) FROM harness_rehearsal_children x WHERE x.tenant_id=a.tenant_id AND x.product_id=a.product_id AND x.root_task_id=a.task_id),t.state='COMMITTED' AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND t.owner_subject_id=a.owner_subject_id AND j.operation='REHEARSAL' AND a.fsm_state='COMMITTED' AND a.own_steps=2 AND e.state='COMMITTED' AND r.state='COMMITTED' AND r.delete_intent=0 AND r.storage_key=? AND r.body_hash=? AND r.asset_id=? AND r.receipt_key=? AND c.operation='REHEARSAL_INTAKE' AND c.phase='COMMIT' AND c.scope_kind='PRODUCT' AND c.world_instance_id IS NULL AND c.task_id IS NULL AND c.owner_subject_id=a.owner_subject_id AND c.command_key=a.original_key AND c.payload_hash=a.command_hash AND f.state='ACTIVE' FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_rehearsal_reports r USING(tenant_id,product_id,task_id) JOIN file_staging f USING(tenant_id,product_id,storage_key) JOIN commit_receipts c ON c.commit_key=a.accepted_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", p.storage, p.hash, p.asset, turnIdentity(r.TaskID, "report-commit"), r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.OwnerSubjectID).Scan(&b, &in, &hash, &original, &next, &count, &valid)
	if e != nil {
		return e
	}
	if !valid || !bytes.Equal(b, binding) || !bytes.Equal(in, input) || next != len(p.children) || count != next || turnIdentity(r.ProductScope.TenantID, r.ProductScope.ProductID, r.OwnerSubjectID, "REHEARSAL", original) != r.TaskID {
		return ErrDenied
	}
	_, h, e := harnessPayload(struct {
		Scope   domain.ProductScope
		Draft   string
		Request generated.RehearsalRequest
	}{r.ProductScope, p.init.binding.DraftID, p.init.request})
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(hash, h[:]) {
		return ErrDenied
	}
	return nil
}
