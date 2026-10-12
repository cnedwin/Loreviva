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
)

// Internal metadata only. The HTTP adapter projects generated contract types.
type AdminTaskMetadata struct {
	ID, Kind, State, World string
	ResultAssetID          *generated.Identity
	Accounting             AdminTaskAccounting
}
type AdminTaskAccounting struct{ Service, Supplier, Risk string }
type AdminTaskSelection struct {
	ExactID, After string
	Limit          int
}
type AdminTaskMetadataPage struct {
	Items              []AdminTaskMetadata
	Last, CurrentStamp string
	More               bool
}

func (s *Store) ListAdminTaskMetadata(ctx context.Context, scope domain.ProductScope, in AdminTaskSelection) (AdminTaskMetadataPage, error) {
	out := AdminTaskMetadataPage{Items: []AdminTaskMetadata{}}
	if scope.Validate() != nil || in.Limit < 1 || in.Limit > 100 || (in.ExactID != "" && (domain.ValidateID(in.ExactID) != nil || in.After != "")) || (in.After != "" && domain.ValidateID(in.After) != nil) {
		return out, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	// This is a fixed read transaction, never an execution or cost authority.
	// All marker inserts are rolled back and no command/FSM/receipt is created.
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	keys := []LockKey{{Rank: 10, IDs: []string{scope.TenantID}}, {Rank: 20, IDs: []string{scope.TenantID, scope.ProductID}}, {Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return out, e
		}
		var locked string
		switch k.Rank {
		case 10:
			e = tx.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", scope.TenantID).Scan(&locked)
		case 20:
			e = tx.QueryRowContext(ctx, "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", scope.TenantID, scope.ProductID).Scan(&locked)
		case 30:
			e = tx.QueryRowContext(ctx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&locked)
		}
		if e != nil {
			return out, e
		}
	}
	stamp, e := adminListCurrent(ctx, tx, scope)
	if e != nil {
		return out, e
	}
	rows, e := tx.QueryContext(ctx, adminListCandidatesSQL, scope.TenantID, scope.ProductID, scope.SubjectID, in.ExactID, in.ExactID, in.After, in.Limit+1)
	if e != nil {
		return out, e
	}
	candidates := []AdminTaskMetadata{}
	for rows.Next() {
		var c AdminTaskMetadata
		if e = rows.Scan(&c.ID, &c.Kind, &c.State, &c.World); e != nil {
			rows.Close()
			return out, e
		}
		candidates = append(candidates, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	out.More = len(candidates) > in.Limit
	if out.More {
		candidates = candidates[:in.Limit]
	}
	// The set comes only from the fixed scope-first query, never caller locks.
	keys = nil
	for _, c := range candidates {
		if c.World != "" {
			keys = append(keys, LockKey{Rank: 40, IDs: []string{scope.TenantID, scope.ProductID, c.World}})
		}
		keys = append(keys, harnessTaskKey(scope, c.ID))
	}
	keys, e = PlanLocks(keys)
	if e != nil {
		return out, e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return out, e
		}
		var id string
		query := "SELECT task_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE"
		if k.Rank == 40 {
			query = "SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE"
		}
		if e = tx.QueryRowContext(ctx, query, scope.TenantID, scope.ProductID, k.IDs[2]).Scan(&id); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
	}
	// Fault injection runs before the final per-item inspection; no item is
	// published from an earlier cached classification or receipt proof.
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return AdminTaskMetadataPage{}, e
		}
	}
	for _, c := range candidates {
		out.Last = c.ID
		actual, err := inspectAdminListItem(ctx, tx, scope, c.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrDenied) || errors.Is(err, ErrConflict) {
				continue
			}
			return AdminTaskMetadataPage{}, err
		}
		out.Items = append(out.Items, actual)
	}
	after, e := adminListCurrent(ctx, tx, scope)
	if e != nil {
		return AdminTaskMetadataPage{}, e
	}
	if after != stamp {
		return AdminTaskMetadataPage{}, ErrDenied
	}
	if e = tx.Rollback(); e != nil {
		return AdminTaskMetadataPage{}, e
	}
	out.CurrentStamp = stamp
	return out, nil
}

// The detail surface is a fixed ADMIN-only consumer of the exact same current
// read transaction. The result reference grants no access to asset bytes.
func (s *Store) ReadAdminTaskMetadata(ctx context.Context, scope domain.ProductScope, task string) (generated.AdminTaskView, error) {
	var out generated.AdminTaskView
	page, e := s.ListAdminTaskMetadata(ctx, scope, AdminTaskSelection{ExactID: task, Limit: 1})
	if e != nil {
		return out, e
	}
	if len(page.Items) != 1 || page.Items[0].Kind == "TURN" {
		return out, sql.ErrNoRows
	}
	v := page.Items[0]
	return generated.AdminTaskView{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(task), OwnerSubjectID: generated.Identity(scope.SubjectID), TaskKind: generated.AdminTaskKind(v.Kind), TaskState: v.State, MockOnly: true, Findings: []generated.FieldError{}, ResultAssetID: v.ResultAssetID}, nil
}

const adminListFamilySQL = `((t.task_kind='ADMIN' AND j.operation IN ('REHEARSAL','CANDIDATE','FILE_CLEANUP','PACKAGE_PREFLIGHT','PACKAGE_IMPORT','PACKAGE_EXPORT','DOCUMENT_EXTRACTION')) OR (t.task_kind='CONTROL' AND j.operation='INSTANCE_CONTROL') OR (t.task_kind='MIGRATION' AND j.operation='INSTANCE_MIGRATION') OR (t.task_kind='TURN' AND j.operation='TURN' AND EXISTS(SELECT 1 FROM harness_turns h WHERE h.tenant_id=t.tenant_id AND h.product_id=t.product_id AND h.task_id=t.task_id AND h.owner_subject_id=t.owner_subject_id AND h.world_instance_id=t.world_instance_id))) AND NOT EXISTS(SELECT 1 FROM harness_rehearsal_worlds rw WHERE rw.tenant_id=t.tenant_id AND rw.product_id=t.product_id AND rw.world_instance_id=t.world_instance_id) AND NOT EXISTS(SELECT 1 FROM harness_rehearsal_children rc WHERE rc.tenant_id=t.tenant_id AND rc.product_id=t.product_id AND rc.child_task_id=t.task_id)`
const adminListCandidatesSQL = `SELECT t.task_id,j.operation,t.state,COALESCE(t.world_instance_id,'') FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.owner_subject_id=? AND (?='' OR t.task_id=?) AND t.task_id>? AND ` + adminListFamilySQL + ` ORDER BY t.task_id LIMIT ?`

func adminListCurrent(ctx context.Context, q rowQuerier, s domain.ProductScope) (string, error) {
	var enabled, allowed bool
	var gates GateVersions
	var credential []byte
	e := q.QueryRowContext(ctx, `SELECT t.enabled AND p.enabled AND g.enabled AND c.enabled,t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,c.token_sha256,EXISTS(SELECT 1 FROM operator_actions a WHERE a.credential_id=c.credential_id AND a.action='task.read') FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN operator_credentials c ON c.tenant_id=g.tenant_id AND c.product_id=g.product_id AND c.subject_id=g.subject_id AND c.credential_id='owner' WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?`, s.TenantID, s.ProductID, s.SubjectID).Scan(&enabled, &gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &credential, &allowed)
	if e != nil {
		return "", e
	}
	if !enabled || !allowed {
		return "", ErrDenied
	}
	var install string
	var generation uint64
	var count int
	if e = q.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MIN(installation_id),''),COALESCE(MIN(generation),0) FROM memory_installation").Scan(&count, &install, &generation); e != nil {
		return "", e
	}
	if count != 1 || generation == 0 {
		return "", ErrDenied
	}
	_, hash, e := harnessPayload(struct {
		Scope        domain.ProductScope
		Gates        GateVersions
		Credential   []byte
		Installation string
		Generation   uint64
	}{s, gates, credential, install, generation})
	return hex.EncodeToString(hash[:]), e
}

func inspectAdminListItem(ctx context.Context, tx *sql.Tx, s domain.ProductScope, id string) (AdminTaskMetadata, error) {
	var out AdminTaskMetadata
	var kind string
	e := tx.QueryRowContext(ctx, `SELECT t.task_id,t.task_kind,j.operation,t.state,COALESCE(t.world_instance_id,'') FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.owner_subject_id=? AND t.task_id=? AND `+adminListFamilySQL, s.TenantID, s.ProductID, s.SubjectID, id).Scan(&out.ID, &kind, &out.Kind, &out.State, &out.World)
	if e != nil {
		return out, e
	}
	if kind == "TURN" {
		return inspectListedTurn(ctx, tx, s, out)
	}
	if !registeredAdminKind(kind, out.Kind) {
		return out, ErrDenied
	}
	if out.Kind == "CANDIDATE" || out.Kind == "REHEARSAL" {
		table, operation := "harness_admin_candidates", "CANDIDATE_INTAKE"
		if out.Kind == "REHEARSAL" {
			table, operation = "harness_admin_rehearsals", "REHEARSAL_INTAKE"
		}
		var key, original, owner string
		var hash []byte
		if e = tx.QueryRowContext(ctx, "SELECT accepted_commit_key,original_key,owner_subject_id,command_hash FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, id).Scan(&key, &original, &owner, &hash); e != nil {
			return out, e
		}
		receipt, e := queryReceipt(ctx, tx, key)
		if e != nil {
			return out, e
		}
		if owner != s.SubjectID || receipt.tenant != s.TenantID || receipt.product != s.ProductID || receipt.owner != s.SubjectID || receipt.kind != "PRODUCT" || receipt.world.Valid || receipt.command != original || receipt.operation != operation || receipt.phase != "COMMIT" || !bytes.Equal(hash, receipt.hash) {
			return out, ErrConflict
		}
		var view generated.AdminTaskView
		if out.Kind == "CANDIDATE" {
			view, e = readDraftCandidateTaskRow(ctx, tx, s, id)
		} else {
			view, e = readDraftRehearsalTaskRow(ctx, tx, s, id)
		}
		if e != nil {
			return out, e
		}
		out.State, out.ResultAssetID = view.TaskState, view.ResultAssetID
	} else if e = inspectLegacyAdminIntake(ctx, tx, s, out); e != nil {
		return out, e
	}
	return out, nil
}

func inspectLegacyAdminIntake(ctx context.Context, tx *sql.Tx, s domain.ProductScope, item AdminTaskMetadata) error {
	operation, identity := "", ""
	switch item.Kind {
	case "INSTANCE_CONTROL":
		operation, identity = "controlAdminInstance", "control"
	case "INSTANCE_MIGRATION":
		operation, identity = "executeInstanceMigration", "migration"
	case "FILE_CLEANUP":
		operation, identity = "queueFileCleanup", "cleanup"
	default:
		return ErrDenied // No actual intake owner exists for the other reserved families yet.
	}
	rows, e := tx.QueryContext(ctx, "SELECT a.result_id,a.document FROM admin_results a JOIN commit_receipts c ON c.commit_key=a.result_id AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.owner_subject_id=a.owner_subject_id WHERE a.tenant_id=? AND a.product_id=? AND a.owner_subject_id=? AND a.schema_name='AdminTaskReceipt' AND c.operation=? AND JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(CONVERT(a.document USING utf8mb4)),CONVERT(a.document USING utf8mb4),'{}'),'$.task_id'))=? LIMIT 2", s.TenantID, s.ProductID, s.SubjectID, operation, item.ID)
	if e != nil {
		return e
	}
	var key string
	var raw []byte
	count := 0
	for rows.Next() {
		count++
		if e = rows.Scan(&key, &raw); e != nil {
			rows.Close()
			return e
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count != 1 {
		return ErrConflict
	}
	var accepted generated.AdminTaskReceipt
	if generated.Decode("AdminTaskReceipt", raw, &accepted) != nil || string(accepted.TaskID) != item.ID || string(accepted.CorrelationID) != item.ID || string(accepted.TaskKind) != item.Kind || accepted.TaskState != "ACCEPTED" || accepted.Scope.TenantID != s.TenantID || accepted.Scope.ProductID != s.ProductID {
		return ErrConflict
	}
	r, e := queryReceipt(ctx, tx, key)
	if e != nil {
		return e
	}
	if r.tenant != s.TenantID || r.product != s.ProductID || r.owner != s.SubjectID || r.operation != operation || r.phase != "COMMIT" || r.nullTask.Valid || len(r.hash) != 32 || key != turnIdentity(s.TenantID, s.ProductID, s.SubjectID, operation, r.command) {
		return ErrConflict
	}
	var input any = struct{ Kind string }{"FILE_CLEANUP"}
	if identity == "cleanup" {
		if item.World != "" || r.kind != "PRODUCT" || r.world.Valid || item.ID != turnIdentity(s.TenantID, s.ProductID, s.SubjectID, identity, r.command) {
			return ErrConflict
		}
		var exists bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM file_cleanup_tasks WHERE tenant_id=? AND product_id=? AND task_id=?)", s.TenantID, s.ProductID, item.ID).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return ErrConflict
		}
	} else {
		if item.World == "" || r.kind != "WORLD" || !r.world.Valid || r.world.String != item.World || item.ID != turnIdentity(s.TenantID, s.ProductID, s.SubjectID, item.World, identity, r.command) {
			return ErrConflict
		}
		var valid bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_tasks r JOIN runtime_instances i ON i.tenant_id=r.tenant_id AND i.product_id=r.product_id AND i.world_instance_id=? WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND i.owner_subject_id=?)", item.World, s.TenantID, s.ProductID, item.ID, s.SubjectID).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrConflict
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, item.ID).Scan(&raw); e != nil {
			return e
		}
		var original struct {
			Scope                  domain.Scope
			Versions               domain.Versions
			Status, Action, PlanID string
		}
		if json.Unmarshal(raw, &original) != nil || original.Scope != (domain.Scope{ProductScope: s, WorldInstanceID: item.World}) {
			return ErrConflict
		}
		v := original.Versions
		versions := generated.Versions{ReleaseID: v.ReleaseID, WorldRevision: strconv.FormatUint(v.WorldRevision, 10), ControlVersion: strconv.FormatUint(v.ControlVersion, 10), PrivacyEpoch: strconv.FormatUint(v.PrivacyEpoch, 10), ViewEpoch: strconv.FormatUint(v.ViewEpoch, 10)}
		if identity == "control" {
			input = generated.AdminControlRequest{ProductID: generated.Identity(s.ProductID), Command: original.Action, ExpectedVersions: versions}
		} else {
			var raw []byte
			var hash string
			if e = tx.QueryRowContext(ctx, "SELECT document,plan_hash FROM migration_plans WHERE tenant_id=? AND product_id=? AND plan_id=? AND owner_subject_id=? AND world_instance_id=?", s.TenantID, s.ProductID, original.PlanID, s.SubjectID, item.World).Scan(&raw, &hash); e != nil {
				return e
			}
			var plan generated.MigrationPlan
			if generated.Decode("MigrationPlan", raw, &plan) != nil || plan.ExpectedVersions != versions {
				return ErrConflict
			}
			_, actual, e := harnessPayload(plan)
			if e != nil {
				return e
			}
			if hex.EncodeToString(actual[:]) != hash {
				return ErrConflict
			}
			input = generated.MigrationExecuteRequest{ProductID: generated.Identity(s.ProductID), PlanID: generated.Identity(original.PlanID), PlanHash: hash, ExpectedVersions: versions, ResolutionMap: plan.ResolutionMap}
		}
	}
	_, hash, e := harnessPayload(struct {
		Scope             domain.ProductScope
		Operation, Target string
		Input             any
	}{s, operation, item.World, input})
	if e != nil {
		return e
	}
	if !bytes.Equal(hash[:], r.hash) {
		return ErrConflict
	}
	return nil
}

func inspectListedTurn(ctx context.Context, tx *sql.Tx, s domain.ProductScope, out AdminTaskMetadata) (AdminTaskMetadata, error) {
	scope := domain.Scope{ProductScope: s, WorldInstanceID: out.World}
	if scope.Validate() != nil {
		return out, ErrDenied
	}
	var key, original string
	var hash []byte
	e := tx.QueryRowContext(ctx, `SELECT h.accepted_commit_key,h.original_key,h.command_hash FROM harness_turns h JOIN runtime_instances i ON i.tenant_id=h.tenant_id AND i.product_id=h.product_id AND i.world_instance_id=h.world_instance_id AND i.owner_subject_id=h.owner_subject_id WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=?`, s.TenantID, s.ProductID, out.ID, out.World, s.SubjectID).Scan(&key, &original, &hash)
	if e != nil {
		return out, e
	}
	receipt, e := queryReceipt(ctx, tx, key)
	if e != nil {
		return out, e
	}
	if receipt.tenant != s.TenantID || receipt.product != s.ProductID || receipt.owner != s.SubjectID || receipt.kind != "WORLD" || receipt.world.String != out.World || receipt.operation != "HARNESS_INTAKE" || receipt.command != original || receipt.phase != "COMMIT" || !bytes.Equal(receipt.hash, hash) {
		return out, ErrConflict
	}
	projection := generated.TurnView{Task: generated.TaskReceipt{TaskState: out.State}, Billing: generated.BillingSummary{ServiceSettlementState: "RESERVED"}}
	if e = projectTurnSettlementTx(ctx, tx, scope, out.ID, &projection); e != nil {
		return out, e
	}
	out.State = projection.Task.TaskState
	out.Accounting = AdminTaskAccounting{Service: projection.Billing.ServiceSettlementState, Supplier: "NOT_APPLICABLE", Risk: "NOT_APPLICABLE"}
	rows, e := tx.QueryContext(ctx, `SELECT a.marker_commit_key,l.attempt_id IS NOT NULL,COALESCE(l.final_known,0),COALESCE(l.disputed,0),COALESCE(l.debt_atoms>0,0),COALESCE(q.marker_key=a.marker_commit_key AND q.provider_state='SUCCEEDED' AND JSON_EXTRACT(q.metadata,'$.NoFurtherCost')=true,0) FROM attempts a LEFT JOIN harness_supplier_liabilities l USING(tenant_id,product_id,task_id,attempt_id) LEFT JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot`, s.TenantID, s.ProductID, out.ID)
	if e != nil {
		return out, e
	}
	sent, pending, dispute, debt := false, false, false, false
	for rows.Next() {
		var marker sql.NullString
		var liability, final, conflict, owed, noFurther bool
		if e = rows.Scan(&marker, &liability, &final, &conflict, &owed, &noFurther); e != nil {
			rows.Close()
			return out, e
		}
		if marker.Valid {
			sent = true
			pending = pending || (liability && !final) || (!liability && !noFurther)
		}
		dispute = dispute || conflict
		debt = debt || owed
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	switch {
	case dispute:
		out.Accounting.Supplier = "DISPUTED"
	case pending:
		out.Accounting.Supplier = "PENDING_VERIFICATION"
	case sent:
		out.Accounting.Supplier = "RECONCILED"
	}
	var state string
	if e = tx.QueryRowContext(ctx, "SELECT status FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind='platform_risk' AND purpose='PARENT'", s.TenantID, s.ProductID, out.ID).Scan(&state); e != nil {
		return out, e
	}
	switch state {
	case "HELD":
		out.Accounting.Risk = "RESERVED"
	case "RISK":
		out.Accounting.Risk = "TRANSFERRED"
	case "RELEASED", "SETTLED":
		if sent {
			out.Accounting.Risk = "RESOLVED"
		}
	default:
		return out, ErrConflict
	}
	var unresolved bool
	e = tx.QueryRowContext(ctx, "SELECT unresolved FROM harness_risk_cases WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, out.ID).Scan(&unresolved)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if e == nil {
		if unresolved {
			out.Accounting.Risk = "TRANSFERRED"
		} else {
			out.Accounting.Risk = "RESOLVED"
		}
	}
	if debt {
		out.Accounting.Risk = "DEBT_BLOCKED"
	}
	return out, nil
}
