package mysql

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

type packageImportInspection struct {
	input        generated.DraftInput
	kind         string
	expectations map[string][][]string
	model        bool
}

// Only the owning command unit can arm a fixed content inspection. No callback,
// SQL, external owner or execution purpose can be supplied by an HTTP caller.
func (s *Store) ArmPackageImportInspection(ctx context.Context, tx Tx, scope domain.ProductScope, input generated.DraftInput, kind string, expectations map[string][][]string, model bool) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore != s || u.scope != scope || !u.commandOnly || u.packagePreviewRead || u.packageImportInspection != nil || (u.operation != "importContentPackage" && u.operation != "approvePackageDraft" && u.operation != "publishContentDraft" && !(u.operation == "commandContentDraft" && u.contentAction == "COPY")) || s.PackagePreviewOwner == nil {
		return ErrDenied
	}
	raw, err := json.Marshal(input)
	if err != nil || generated.Validate("DraftInput", raw) != nil {
		return ErrDenied
	}
	var frozen generated.DraftInput
	if json.Unmarshal(raw, &frozen) != nil {
		return ErrDenied
	}
	var expected map[string][][]string
	raw, err = json.Marshal(expectations)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &expected) != nil {
		return ErrDenied
	}
	inspection := &packageImportInspection{frozen, kind, expected, model}
	if err = s.inspectPackageImportFinish(ctx, u, inspection); err != nil {
		return err
	}
	u.packageImportInspection = inspection
	return nil
}

func (s *Store) inspectPackageImportFinish(ctx context.Context, u *unit, inspection *packageImportInspection) error {
	if u.operatorStore != s || s.PackagePreviewOwner == nil {
		return ErrDenied
	}
	result, err := s.PackagePreviewOwner.InspectPackagePreviewTx(ctx, u, u.scope, inspection.input, inspection.kind, inspection.expectations, inspection.model)
	if err != nil {
		return err
	}
	if len(result.Findings) > 0 {
		return ErrDenied
	}
	return u.err
}

// Reusing an imported object in a new draft is a copy, even when the ordinary
// editor endpoint was used. The extra permission is checked again at commit.
func (s *Store) RequirePackageImportCopy(ctx context.Context, tx Tx, scope domain.ProductScope) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore != s || u.scope != scope || !u.commandOnly {
		return ErrDenied
	}
	if err := CheckOperatorTx(ctx, tx, scope, "content.copy"); err != nil {
		return err
	}
	for _, action := range u.memoryHTTPActions {
		if action == "content.copy" {
			return nil
		}
	}
	u.memoryHTTPActions = append(u.memoryHTTPActions, "content.copy")
	return nil
}
