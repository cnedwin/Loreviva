package mysql

import (
	"context"
	"crypto/sha256"
	"loreviva.local/loreviva/internal/domain"
)

type RehearsalPurgeEvidence interface{ PurgeEvidence() }
type RehearsalPurgeOwner interface {
	CheckRehearsalPurgeEvidence(RehearsalPurgeEvidence, domain.ProductScope, string) (CommandBarrierRequest, error)
}
type rehearsalPurgeACK struct {
	evidence RehearsalPurgeEvidence
	key      string
	claim    CommandBarrierRequest
}
type purgeAdmission interface {
	EnterRehearsalPurgeACK(context.Context) (context.Context, func(), error)
}

// Fixed owning continuation, not a caller-selected command/purpose/callback.
func (s *Store) FinishRehearsalStagePurge(ctx context.Context, scope domain.ProductScope, key string, evidence RehearsalPurgeEvidence) (CommitReceipt, error) {
	if s.RehearsalPurgeOwner == nil {
		return CommitReceipt{}, ErrDenied
	}
	claim, e := s.RehearsalPurgeOwner.CheckRehearsalPurgeEvidence(evidence, scope, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	locks, e := s.RehearsalStageCleanupLocks(ctx, scope, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	if len(locks) == 0 {
		return CommitReceipt{}, ErrDenied
	}
	locks = append(locks, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, "staging." + key}})
	command := "purged." + key
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "REHEARSAL_STAGE_PURGED", CommandKey: command, CommitKey: purgeCommitKey(scope, command), PayloadHash: sha256.Sum256([]byte(command)), GateVersions: claim.GateVersions, Locks: locks, commandOnly: true, rehearsalPurgeACK: &rehearsalPurgeACK{evidence, key, claim}}
	receipt, _, e := s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.checkRehearsalPurgeACK(ctx, u, r); e != nil {
			return e
		}
		return tx.AcknowledgeRehearsalStagePurge(ctx, scope, key)
	})
	return receipt, e
}
func purgeCommitKey(s domain.ProductScope, key string) string {
	return turnIdentity(s.TenantID, s.ProductID, key)
}
func (s *Store) checkRehearsalPurgeACK(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalPurgeACK
	if p == nil || s.RehearsalPurgeOwner == nil || !r.commandOnly || r.WorldScope != nil || r.Operation != "REHEARSAL_STAGE_PURGED" {
		return ErrDenied
	}
	current, e := s.RehearsalPurgeOwner.CheckRehearsalPurgeEvidence(p.evidence, r.ProductScope, p.key)
	if e != nil {
		return e
	}
	if current.CommitKey != p.claim.CommitKey || current.PayloadHash != p.claim.PayloadHash || current.CommandKey != p.claim.CommandKey {
		return ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, p.claim.CommitKey)
	if e != nil {
		return e
	}
	want := BarrierRequest{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, Operation: "CLAIM_STAGE_DELETE", CommandKey: p.claim.CommandKey, CommitKey: p.claim.CommitKey, PayloadHash: p.claim.PayloadHash, commandOnly: true}
	if !receipt.matches(want) {
		return ErrDenied
	}
	// These rows are already in the original sorted plan. Recheck the current
	// permissions/epochs after the owning mutation seam without new OS IO.
	var currentGates bool
	e = u.tx.QueryRowContext(ctx, "SELECT t.enabled=1 AND p.enabled=1 AND g.enabled=1 AND g.can_execute=1 AND t.permission_version=? AND p.permission_version=? AND g.permission_version=? AND g.privacy_epoch=? AND g.view_epoch=? FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?", r.GateVersions.Tenant, r.GateVersions.Product, r.GateVersions.Subject, r.GateVersions.Privacy, r.GateVersions.View, r.ProductScope.TenantID, r.ProductScope.ProductID, r.ProductScope.SubjectID).Scan(&currentGates)
	if e != nil {
		return e
	}
	if !currentGates {
		return ErrDenied
	}
	return nil
}
