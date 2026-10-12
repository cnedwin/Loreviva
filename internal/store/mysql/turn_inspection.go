package mysql

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

var errApprovedInspected = errors.New("approved result inspected; rollback read")

// InspectApprovedTurn is a fixed read-only owning entry point. Its release locks
// come only from the registered current perception planner, never a request.
func (s *Store) InspectApprovedTurn(ctx context.Context, scope domain.Scope, task string) (domain.CandidateCommit, error) {
	var out domain.CandidateCommit
	if scope.Validate() != nil || domain.ValidateID(task) != nil || s.GenerationContext == nil {
		return out, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if s.WorldAccountingAuthority == nil {
		return out, ErrDenied
	}
	ctx, installation, historyRelease, e := s.WorldAccountingAuthority.EnterWorldAccounting(ctx)
	if e != nil {
		return out, e
	}
	defer historyRelease()
	var actor, status string
	var v domain.Versions
	e = s.DB.QueryRowContext(ctx, "SELECT h.actor_id,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status FROM harness_turns h JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE h.tenant_id=? AND h.product_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND h.task_id=?", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID, task).Scan(&actor, &v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status)
	if e != nil {
		return out, e
	}
	var g GateVersions
	var allowed bool
	e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,(t.enabled AND p.enabled AND s.enabled AND s.can_execute) FROM subject_governance s JOIN product_gates p ON p.tenant_id=s.tenant_id AND p.product_id=s.product_id JOIN tenant_gates t ON t.tenant_id=s.tenant_id WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &allowed)
	if e != nil {
		return out, e
	}
	if !allowed {
		return out, ErrDenied
	}
	key := turnIdentity(task, "approved-read")
	r := BarrierRequest{approvedHistoryRead: true, approvedInstallation: &installation, ExpectedWorldStatus: status, ProductScope: scope.ProductScope, WorldScope: &scope, Versions: &v, OwnerSubjectID: scope.SubjectID, Operation: "HARNESS_APPROVED_READ", CommandKey: key, CommitKey: key, GateVersions: g, commandOnly: true, TaskID: task, Locks: []LockKey{{Rank: 50, IDs: []string{scope.TenantID, scope.ProductID, task}}}}
	rows, e := s.DB.QueryContext(ctx, "SELECT a.attempt_id,z.authorization_id FROM attempts a JOIN dispatch_authorizations z USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", scope.TenantID, scope.ProductID, task)
	if e != nil {
		return out, e
	}
	count := 0
	for rows.Next() {
		var attempt, authorization string
		if e = rows.Scan(&attempt, &authorization); e != nil {
			rows.Close()
			return out, e
		}
		count++
		r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, task, attempt, authorization}})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if count < 1 || count > 2 {
		return out, ErrDenied
	}
	if e = s.planCommittedHistory(ctx, &r); e != nil {
		return out, e
	}
	if e = s.planPerception(ctx, &r, actor); e != nil {
		return out, e
	}
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		var e error
		out, e = s.GenerationContext.InspectApprovedTurnTx(ctx, tx, scope, task, v)
		if e != nil {
			return e
		}
		costs := []domain.MockResultMetadata{out.Metadata}
		if out.OriginalDependency != nil {
			costs = []domain.MockResultMetadata{*out.OriginalDependency, out.Metadata}
		}
		if !allOriginalUsageKnown(costs) {
			if e = checkOriginalServiceMeteringTx(ctx, tx.(*unit), scope.ProductScope, task, turnIdentity(task, "service"), turnIdentity(task, "world-commit"), costs); e != nil {
				return e
			}
		}
		return errApprovedInspected
	})
	if !errors.Is(e, errApprovedInspected) {
		if e == nil {
			e = ErrDenied
		}
		return domain.CandidateCommit{}, e
	}
	if s.ProviderEvidence == nil {
		return domain.CandidateCommit{}, ErrDenied
	}
	metas := []domain.MockResultMetadata{out.Metadata}
	if out.OriginalDependency != nil {
		metas = append(metas, *out.OriginalDependency)
	}
	for _, meta := range metas {
		live, e := s.ProviderEvidence.OriginalMetadata(ctx, meta.Identity)
		if e != nil {
			return domain.CandidateCommit{}, e
		}
		if live != meta {
			return domain.CandidateCommit{}, ErrConflict
		}
	}
	return out, nil
}
