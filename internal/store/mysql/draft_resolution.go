package mysql

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

func draftResolutionReadTx(ctx context.Context, tx *sql.Tx, scope domain.ProductScope) error {
	var allowed bool
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials c JOIN operator_actions a USING(credential_id) WHERE c.credential_id='owner' AND c.tenant_id=? AND c.product_id=? AND c.subject_id=? AND c.enabled=1 AND a.action='content.read')", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&allowed)
	if e != nil {
		return e
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// The only mint for S59's fixed pure resolution guard. No caller-selected
// purpose, lock list or callback can obtain this branch.
func (s *Store) InspectDraftGeneration(ctx context.Context, scope domain.ProductScope, selection domain.DraftCandidateSelection) (domain.DraftGeneration, error) {
	var out domain.DraftGeneration
	if s.DraftCandidateOwner == nil || scope.Validate() != nil || domain.ValidateID(selection.DraftID) != nil || domain.ValidateID(selection.ObjectID) != nil || selection.Revision == 0 || len(selection.Fields) != 0 {
		return out, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	var gates GateVersions
	var enabled bool
	e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil {
		return out, e
	}
	if !enabled {
		return out, ErrDenied
	}
	keys, e := s.DraftCandidateOwner.PlanDraftGenerationLocks(ctx, scope, selection)
	if e != nil {
		return out, e
	}
	key := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, selection.DraftID, selection.ObjectID, selection.ModuleID, "draft-resolution")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "DRAFT_GENERATION_RESOLUTION", CommandKey: key, CommitKey: key, GateVersions: gates, Locks: keys, commandOnly: true, draftResolution: true}
	inspected := errors.New("draft resolution read without mutation")
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		var err error
		out, err = s.DraftCandidateOwner.InspectDraftGenerationTx(ctx, tx, scope, selection)
		if err != nil {
			return err
		}
		if err = s.finish(ctx, tx.(*unit).tx, r, tx.(*unit)); err != nil {
			return err
		}
		return inspected
	})
	if errors.Is(e, inspected) {
		return out, nil
	}
	return domain.DraftGeneration{}, e
}
