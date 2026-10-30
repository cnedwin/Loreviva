package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

// PackagePreviewOwner is installed once by the content module. HTTP callers
// cannot select an owner, SQL statement, lock set or transaction purpose.
type PackagePreviewOwner interface {
	PlanPackagePreviewLocks(context.Context, domain.ProductScope, generated.DraftInput, string) ([]LockKey, error)
	InspectPackagePreviewTx(context.Context, Tx, domain.ProductScope, generated.DraftInput, string, map[string][][]string, bool) (PackagePreviewInspection, error)
}
type PackagePreviewInspection struct {
	BasisHash string
	Input     generated.DraftInput
	Findings  []generated.FieldError
	RouteHash string
}

// InspectPackagePreview owns an entirely rolled-back transaction. The private
// flag denies all unit writes; final authority checks run before the sentinel.
func (s *Store) InspectPackagePreview(ctx context.Context, scope domain.ProductScope, input generated.DraftInput, kind string, expectations map[string][][]string, requiresModel bool) (PackagePreviewInspection, error) {
	var out PackagePreviewInspection
	if s.PackagePreviewOwner == nil || scope.Validate() != nil {
		return out, ErrDenied
	}
	raw, err := json.Marshal(input)
	if err != nil || generated.Validate("DraftInput", raw) != nil {
		return out, ErrDenied
	}
	keys, err := s.PackagePreviewOwner.PlanPackagePreviewLocks(ctx, scope, input, kind)
	if err != nil {
		return out, err
	}
	if len(keys) > 10000 {
		return out, ErrDenied
	}
	var gates GateVersions
	var enabled bool
	err = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if err != nil {
		return out, err
	}
	if !enabled {
		return out, ErrDenied
	}
	digest := sha256.Sum256(raw)
	key := turnIdentity(scope.TenantID, scope.ProductID, scope.SubjectID, kind, string(digest[:]), "package-local-preview")
	request := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "PACKAGE_LOCAL_PREVIEW", CommandKey: key, CommitKey: key, GateVersions: gates, PayloadHash: digest, Locks: keys, commandOnly: true, packagePreview: true}
	if err = s.prepareOperatorBarrier(ctx, &request); err != nil {
		return out, err
	}
	inspected := errors.New("package preview inspected without mutation")
	_, _, err = s.barrier(ctx, request, func(tx Tx) error {
		u := tx.(*unit)
		var e error
		out, e = s.PackagePreviewOwner.InspectPackagePreviewTx(ctx, tx, scope, input, kind, expectations, requiresModel)
		if e != nil {
			return e
		}
		if u.err != nil {
			return u.err
		}
		if e = s.checkFinish(ctx, u.tx, request, u); e != nil {
			return e
		}
		// Finish hooks may expose a late expiry or source change in tests. Repeat
		// the owning source and route assertions before releasing any candidate.
		out, e = s.PackagePreviewOwner.InspectPackagePreviewTx(ctx, tx, scope, input, kind, expectations, requiresModel)
		if e != nil {
			return e
		}
		if u.err != nil {
			return u.err
		}
		return inspected
	})
	if errors.Is(err, inspected) {
		basis, _ := json.Marshal(struct {
			Scope domain.ProductScope
			Gates GateVersions
		}{scope, gates})
		sum := sha256.Sum256(basis)
		out.BasisHash = hex.EncodeToString(sum[:])
		return out, nil
	}
	return PackagePreviewInspection{}, err
}
