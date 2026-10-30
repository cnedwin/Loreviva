package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wepkg"
	"loreviva.local/loreviva/internal/wire"
	"sort"
)

const packageImportOperation = "importContentPackage"

func packageImportKey(scope domain.ProductScope, id string) store.LockKey {
	return Key(scope, "content_check", Identity("package-import-slot", id))
}

// Metadata recovery needs no archive, model, enabled route or current draft
// revision. It proves the original committed command/result and owning import.
func (s *Service) GetPackageImportResult(ctx context.Context, scope domain.ProductScope, key string) (generated.PackageImportResult, error) {
	var out generated.PackageImportResult
	if domain.ValidateIdempotencyKey(key) != nil {
		return out, wire.ErrJSON
	}
	read, err := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if err != nil {
		return out, err
	}
	defer read.Close()
	id := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, packageImportOperation, key)
	var raw, payloadHash []byte
	err = read.QueryRowContext(ctx, "SELECT a.document,c.payload_hash FROM admin_results a JOIN commands c ON c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.subject_id=a.owner_subject_id JOIN commit_receipts r ON r.commit_key=a.result_id AND r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.owner_subject_id=c.subject_id AND r.operation=c.operation AND r.command_key=c.idem_key AND r.payload_hash=c.payload_hash WHERE a.tenant_id=? AND a.product_id=? AND a.owner_subject_id=? AND a.result_id=? AND a.schema_name='PackageImportResult' AND c.operation='importContentPackage' AND c.idem_key=? AND c.scope_kind='PRODUCT' AND r.scope_kind='PRODUCT' AND r.phase='COMMIT'", scope.TenantID, scope.ProductID, scope.SubjectID, id, key).Scan(&raw, &payloadHash)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if generated.Decode("PackageImportResult", raw, &out) != nil || out.RequestKey != key {
		return generated.PackageImportResult{}, ErrBlocked
	}
	expectedHash, err := Hash(struct {
		Scope             domain.ProductScope
		Operation, Target string
		Input             any
	}{scope, packageImportOperation, string(out.Receipt.ImportID), struct{ RequestHash string }{out.RequestSha256}})
	if err != nil || expectedHash != hex.EncodeToString(payloadHash) {
		return generated.PackageImportResult{}, ErrBlocked
	}
	var original, contentHash []byte
	err = read.QueryRowContext(ctx, "SELECT document,content_hash FROM content_package_imports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND import_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, out.Receipt.ImportID).Scan(&original, &contentHash)
	if err != nil {
		return generated.PackageImportResult{}, err
	}
	var receipt generated.PackageImportReceipt
	if generated.Decode("PackageImportReceipt", original, &receipt) != nil || receipt.Scope != Scope(scope) || receipt.ContentSha256 != hex.EncodeToString(contentHash) {
		return generated.PackageImportResult{}, ErrBlocked
	}
	a, _ := json.Marshal(receipt)
	b, _ := json.Marshal(out.Receipt)
	if !bytes.Equal(a, b) {
		return generated.PackageImportResult{}, ErrBlocked
	}
	if err = read.Finish(ctx); err != nil {
		return generated.PackageImportResult{}, err
	}
	return out, nil
}

func normalizedPackageMapping(options packagePreviewOptions) (string, error) {
	options.Namespace = "" // Preview addresses never become installation authority.
	sort.Slice(options.DependencyMappings, func(i, j int) bool {
		return options.DependencyMappings[i].Pointer < options.DependencyMappings[j].Pointer
	})
	sort.Slice(options.AdditionalLocks, func(i, j int) bool {
		a, _ := Hash(options.AdditionalLocks[i])
		b, _ := Hash(options.AdditionalLocks[j])
		return a < b
	})
	sort.Slice(options.ModelMappings, func(i, j int) bool {
		return options.ModelMappings[i].CapabilityID < options.ModelMappings[j].CapabilityID
	})
	sort.Slice(options.AssetMappings, func(i, j int) bool { return options.AssetMappings[i].FilePath < options.AssetMappings[j].FilePath })
	for i := range options.Rights {
		sort.Strings(options.Rights[i].AllowedUses)
	}
	sort.Slice(options.Rights, func(i, j int) bool { return options.Rights[i].ObjectID < options.Rights[j].ObjectID })
	return Hash(options)
}

func (s *Service) packageImportExists(ctx context.Context, scope domain.ProductScope, id string) (bool, error) {
	read, err := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if err != nil {
		return false, err
	}
	defer read.Close()
	var n int
	err = read.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_package_imports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND import_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, id).Scan(&n)
	if err != nil {
		return false, err
	}
	if err = read.Finish(ctx); err != nil {
		return false, err
	}
	return n == 1, nil
}

// The original archive and exact options are caller input, not a trusted preview
// report. One transaction owns the import slot, protected lineage and draft.
func (s *Service) ImportPackage(ctx context.Context, scope domain.ProductScope, archive, rawOptions []byte, key string) (generated.PackageImportResult, error) {
	var out generated.PackageImportResult
	var options generated.PackageImportOptions
	if len(rawOptions) > 65536 || int64(len(archive)) > wepkg.DefaultLimits.ArchiveBytes || domain.ValidateIdempotencyKey(key) != nil || generated.Decode("PackageImportOptions", rawOptions, &options) != nil {
		return out, wire.ErrJSON
	}
	archiveHash := sha256.Sum256(archive)
	requestHash, err := Hash(struct {
		Archive string
		Options json.RawMessage
	}{hex.EncodeToString(archiveHash[:]), rawOptions})
	if err != nil {
		return out, err
	}
	// Recover a completed request before mutable source/route or archive checks.
	if original, e := s.GetPackageImportResult(ctx, scope, key); e == nil {
		if original.RequestSha256 != requestHash {
			return out, store.ErrConflict
		}
		return original, nil
	} else if !errors.Is(e, ErrNotFound) {
		return out, e
	}
	report := wepkg.Check(archive, wepkg.DefaultLimits)
	if report.Verdict != "PASS" {
		return out, ErrBlocked
	}
	var mapping packagePreviewOptions
	if json.Unmarshal(options.Mapping, &mapping) != nil {
		return out, wire.ErrJSON
	}
	// Normalize a separate copy: P2 still validates the original declared options.
	var normalized packagePreviewOptions
	json.Unmarshal(options.Mapping, &normalized)
	mappingHash, err := normalizedPackageMapping(normalized)
	if err != nil {
		return out, err
	}
	copyKey := ""
	if options.CopyKey != nil {
		copyKey = *options.CopyKey
	}
	importID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "package-import", report.PackageID, report.PackageVersion, copyKey)
	draftID := Identity(importID, "isolated-draft")
	projectID := ""
	locks := []store.LockKey{packageImportKey(scope, importID)}
	var input generated.DraftInput
	var plan wepkg.PortablePlan
	expectations := map[string][][]string{}
	requiresModel := false
	candidate := false
	exists, err := s.packageImportExists(ctx, scope, importID)
	if err != nil {
		return out, err
	}
	if !exists {
		preview, err := s.PreviewPackage(ctx, scope, archive, options.Mapping)
		if err != nil {
			return out, err
		}
		var checked struct {
			Status string `json:"status"`
			Hash   string `json:"preview_sha256"`
		}
		json.Unmarshal(preview, &checked)
		if checked.Status != "RESOLVED" {
			return out, ErrBlocked
		}
		if checked.Hash != options.ExpectedPreviewSha256 {
			return out, store.ErrConflict
		}
		var installation string
		if err = s.Store.DB.QueryRowContext(ctx, "SELECT CONCAT(@@server_uuid,'/',DATABASE())").Scan(&installation); err != nil {
			return out, err
		}
		var nonce [32]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return out, err
		}
		namespace := Identity(installation, importID, hex.EncodeToString(nonce[:]), "package-import-objects")
		var findings []wepkg.Finding
		plan, findings = wepkg.BuildPortablePlan(archive, wepkg.DefaultLimits, mapping.Entrypoint, namespace)
		if len(findings) > 0 {
			return out, ErrBlocked
		}
		var issues []generated.FieldError
		input, issues = projectPortable(plan, mapping)
		if len(issues) > 0 {
			return out, ErrBlocked
		}
		keys, err := s.PlanPackagePreviewLocks(ctx, scope, input, plan.Kind)
		if err != nil {
			return out, err
		}
		locks = append(locks, keys...)
		selected := map[string]generated.DependencyLock{}
		for _, item := range mapping.DependencyMappings {
			selected[item.Pointer] = item.Target
		}
		for _, reference := range plan.References {
			if reference.Kind != "BUNDLE_OBJECT" {
				target := selected[reference.Pointer]
				k := referenceKey(pinReference(target))
				expectations[k] = append(expectations[k], reference.ExpectedKinds)
			}
		}
		requiresModel = len(mapping.ModelMappings) > 0
		for _, cap := range plan.Requirements.Model {
			requiresModel = requiresModel || (cap.Required && cap.CapabilityID == "text")
		}
		locks = append(locks, Key(scope, "content_draft", draftID))
		if plan.Kind == "WORLD" {
			projectID = Identity(importID, "isolated-project")
			locks = append(locks, Key(scope, "content_project", projectID))
		}
		locks = append(locks, importLineageLocks(scope, draftID, input)...)
		candidate = true
	}
	raw, err := Command(ctx, s.Store, scope, packageImportOperation, key, importID, "PackageImportResult", struct{ RequestHash string }{requestHash}, locks, func(tx store.Tx) (any, error) {
		if err := tx.RequireLock(packageImportKey(scope, importID)); err != nil {
			return nil, err
		}
		var existing, existingContent, existingMapping []byte
		err := tx.QueryRowContext(ctx, "SELECT document,content_hash,mapping_hash FROM content_package_imports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND import_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, importID).Scan(&existing, &existingContent, &existingMapping)
		result := generated.PackageImportResult{RequestKey: key, RequestSha256: requestHash, Persisted: true}
		if err == nil {
			if hex.EncodeToString(existingContent) != report.ContentHashComputed || hex.EncodeToString(existingMapping) != mappingHash {
				return nil, store.ErrConflict
			}
			if generated.Decode("PackageImportReceipt", existing, &result.Receipt) != nil {
				return nil, ErrBlocked
			}
			return result, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !candidate {
			return nil, ErrBlocked
		}
		if err = s.Store.ArmPackageImportInspection(ctx, tx, scope, input, plan.Kind, expectations, requiresModel); err != nil {
			return nil, err
		}
		draft := makeDraft(scope, draftID, projectID, plan.Kind, input)
		draft.State = "ISOLATED_IMPORT"
		receipt := generated.PackageImportReceipt{ImportID: generated.Identity(importID), Scope: Scope(scope), PackageID: report.PackageID, PackageVersion: report.PackageVersion, ContentSha256: report.ContentHashComputed, ArchiveSha256: report.ArchiveSHA256, Entrypoint: plan.Entrypoint, Namespace: plan.Namespace, DraftID: draft.DraftID, ProjectID: draft.ProjectID, DraftStateAtImport: "ISOLATED_IMPORT", CreatedAt: draft.CreatedAt}
		body, err := json.Marshal(receipt)
		if err != nil || generated.Validate("PackageImportReceipt", body) != nil {
			return nil, ErrBlocked
		}
		contentBytes, _ := hex.DecodeString(report.ContentHashComputed)
		mappingBytes, _ := hex.DecodeString(mappingHash)
		if err = tx.RequireLock(packageImportKey(scope, importID)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO content_package_imports(tenant_id,product_id,import_id,owner_subject_id,package_id,package_version,copy_key,content_hash,mapping_hash,document) VALUES(?,?,?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, importID, scope.SubjectID, report.PackageID, report.PackageVersion, copyKey, contentBytes, mappingBytes, body); err != nil {
			return nil, err
		}
		if err = s.applyImportLineage(ctx, tx, scope, &draft, map[string]bool{importID: true}); err != nil {
			return nil, err
		}
		if err = createRows(ctx, tx, scope, draft, projectID != ""); err != nil {
			return nil, err
		}
		result.Receipt = receipt
		return result, nil
	})
	if err != nil {
		return out, err
	}
	if err = generated.Decode("PackageImportResult", raw, &out); err != nil {
		return generated.PackageImportResult{}, err
	}
	return out, nil
}
