package mysql

import (
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
	"strings"
)

func operatorKeys(p domain.OperatorPrincipal) []LockKey {
	return []LockKey{{Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{p.TenantID, p.CredentialID}}, {Rank: 25, Kind: domain.StudioGrantsLock, IDs: []string{p.TenantID, p.CredentialID, "PRODUCT", "P:" + p.ProductID}}}
}

// This preparation captures the private principal and actions for the mapped operator branches; other branches retain their own barrier checks. The captured identity is checked again in the owning transaction.
func (s *Store) prepareOperatorBarrier(ctx context.Context, r *BarrierRequest) error {
	if e := s.prepareContentWithdrawal(ctx, r); e != nil {
		return e
	}
	if r.integrationTurn != nil {
		if r.integrationTurn.store != s {
			return ErrDenied
		}
		return nil
	}
	if r.integrationControl != nil {
		if r.integrationControl.store != s {
			return ErrDenied
		}
		return nil // Exact private plan is checked before body/replay and final commit.
	}
	var actions []string
	switch r.Operation {
	case withdrawalOperation:
		actions = []string{"content.archive"}
	case "createContentDraft", "saveContentDraft":
		actions = []string{"content.edit"}
	case "checkDraftImpact", "checkDraftForPublish":
		actions = []string{"content.check"}
	case "publishContentDraft":
		actions = []string{"content.publish"}
	case "commandContentDraft":
		switch r.contentAction {
		case "REMOVE_BINDING":
			actions = []string{"content.edit"}
		case "COPY", "ARCHIVE", "RESTORE", "DELETE":
			actions = []string{"content." + strings.ToLower(r.contentAction)}
		default:
			return ErrDenied
		}
	case "uploadContentAsset", "finalizeContentAsset":
		actions = []string{"asset.upload"}
	case "createAdminInstance":
		actions = []string{"runtime.create"}
	case "controlAdminInstance":
		actions = []string{"runtime.control"}
	case "previewInstanceMigration", "executeInstanceMigration", "cancelInstanceMigration":
		actions = []string{"runtime.migrate"}
	case "cancelAdminTask":
		actions = []string{"task.cancel"}
	case "queueFileCleanup":
		actions = []string{"asset.upload"}
	case "CANDIDATE_PREPARE", "CANDIDATE_INTAKE", "CANDIDATE_SOURCE_INSPECT":
		actions = []string{"content.assist"}
	case "REHEARSAL_PREPARE", "REHEARSAL_INTAKE", "REHEARSAL_SOURCE_INSPECT", "REHEARSAL_INSPECT":
		actions = []string{"content.rehearse"}
	case "DRAFT_GENERATION_RESOLUTION":
		actions = []string{"content.read"}
	}
	if r.candidateExecution {
		actions = []string{"content.assist"}
	}
	if r.Runtime != nil && !r.commandOnly && r.Purpose == ControlPurpose {
		actions = []string{"runtime.control"}
	}
	if r.Runtime != nil && !r.commandOnly && r.Purpose == MigrationPurpose {
		actions = []string{"runtime.migrate"}
	}
	if r.rehearsalInitialization != nil || r.rehearsalExecution != nil {
		actions = []string{"content.rehearse"}
	}
	if r.candidateHistoryRead != nil || r.rehearsalHistory != nil {
		actions = []string{"content.read", "asset.read"}
	}
	identityOnly := r.commandOnly && (r.Operation == "MEMORY_GOVERNANCE_ACCEPT" || r.Operation == "MEMORY_STATEMENT" || r.Operation == "MEMORY_GRANT" || r.Operation == "MEMORY_TELL" || r.Operation == "MEMORY_INITIALIZE" || r.Operation == "MEMORY_WITNESS" || r.Operation == "MEMORY_DERIVED_STAGE" || r.Operation == "MEMORY_DERIVED_FILE" || r.Operation == "MEMORY_DERIVED_READY")
	if len(actions) == 0 && !identityOnly {
		return nil
	}
	p, ok := ctx.Value(operatorContextKey{}).(currentOperator)
	if !ok || p.store != s || p.principal.ProductScope != r.ProductScope {
		return ErrDenied
	}
	if p.job != "" && r.commandOnly && r.Operation != "REHEARSAL_SOURCE_INSPECT" && r.Operation != "REHEARSAL_INSPECT" && r.Operation != "CANDIDATE_SOURCE_INSPECT" && !r.rehearsalReportRead {
		return ErrDenied
	}
	r.operator = &p
	r.operatorActions = actions
	return nil
}

func (s *Store) checkOperatorBarrier(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	if e := s.checkIntegrationTurn(ctx, tx, r); e != nil {
		return e
	}
	if e := s.checkIntegrationControl(ctx, tx, r); e != nil {
		return e
	}
	if e := checkProgramBarrier(ctx, tx, r); e != nil {
		return e
	}
	if r.operator != nil {
		if _, e := currentOperatorTx(ctx, tx, s, r.ProductScope); e != nil {
			return e
		}
	}
	for _, a := range r.operatorActions {
		if e := operatorActionTx(ctx, tx, s, r.ProductScope, a); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) acquireOperatorKey(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	if r.operator == nil {
		return ErrDenied
	}
	p := r.operator.principal
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	if k.Rank == 15 {
		var id string
		if e := tx.QueryRowContext(ctx, "SELECT credential_id FROM operator_credentials WHERE credential_id=? AND tenant_id=? FOR UPDATE", p.CredentialID, p.TenantID).Scan(&id); e != nil {
			return e
		}
		var revision uint64
		e := tx.QueryRowContext(ctx, "SELECT revision FROM studio_credential_state WHERE credential_id=? FOR UPDATE", p.CredentialID).Scan(&revision)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		return nil
	}
	rows, e := tx.QueryContext(ctx, "SELECT action,allowed_use FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type='PRODUCT' AND scope_address=? ORDER BY action,allowed_use FOR UPDATE", p.TenantID, p.CredentialID, "P:"+p.ProductID)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var action, use string
		if e = rows.Scan(&action, &use); e != nil {
			return e
		}
	}
	return rows.Err()
}
