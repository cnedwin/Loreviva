package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wepkg"
	"strconv"
	"time"
)

func (s *Service) PlanPackageExportLocks(ctx context.Context, scope domain.ProductScope, id string) ([]store.LockKey, error) {
	release, e := s.GetRelease(ctx, scope, id)
	if e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, release.Input)
	if e != nil {
		return nil, e
	}
	keys = append(keys, Key(scope, "content_release", id), Key(scope, "content_draft", string(release.DraftID)))
	for _, dep := range release.DependencyLocks {
		r, e := s.GetRelease(ctx, scope, string(dep.ReleaseID))
		if e != nil {
			return nil, e
		}
		more, e := s.candidateSourceLocks(ctx, scope, r.Input)
		if e != nil {
			return nil, e
		}
		keys = append(keys, more...)
		keys = append(keys, Key(scope, "content_release", string(dep.ReleaseID)))
	}
	if len(keys) > 10000 {
		return nil, ErrBlocked
	}
	return keys, nil
}
func loadPackageRelease(ctx context.Context, tx store.Tx, scope domain.ProductScope, id string) (generated.Release, error) {
	var out generated.Release
	if e := tx.RequireLock(Key(scope, "content_release", id)); e != nil {
		return out, e
	}
	var raw, hash []byte
	e := tx.QueryRowContext(ctx, "SELECT c.document,c.content_hash FROM content_releases c JOIN releases r USING(tenant_id,product_id,release_id) WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", scope.TenantID, scope.ProductID, id).Scan(&raw, &hash)
	if e != nil {
		return out, e
	}
	if generated.Decode("Release", raw, &out) != nil || out.Scope != Scope(scope) || string(out.ReleaseID) != id {
		return out, ErrBlocked
	}
	actual, e := Hash(struct {
		Input        generated.DraftInput
		Dependencies []generated.DependencyLock
		Mappings     any
	}{out.Input, out.DependencyLocks, out.RouteMappings})
	if e != nil || actual != out.ContentHash || actual != hex.EncodeToString(hash) {
		return out, ErrBlocked
	}
	return out, nil
}
func (s *Service) MaterializePackageExportTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, id string) (store.PackageExportMaterial, error) {
	out := store.PackageExportMaterial{Catalog: map[string]generated.Release{}, Assets: map[string]generated.FileRecord{}}
	if e := store.CheckOperatorTx(ctx, tx, scope, "content.copy"); e != nil {
		return out, e
	}
	root, e := loadPackageRelease(ctx, tx, scope, id)
	if e != nil {
		return out, e
	}
	out.Root = root
	out.Catalog[id] = root
	if e = tx.RequireLock(Key(scope, "content_draft", string(root.DraftID))); e != nil {
		return out, e
	}
	var owner string
	if e = tx.QueryRowContext(ctx, "SELECT owner_subject_id FROM content_drafts WHERE tenant_id=? AND product_id=? AND draft_id=?", scope.TenantID, scope.ProductID, root.DraftID).Scan(&owner); e != nil || owner != scope.SubjectID {
		return out, ErrBlocked
	}
	findings, _, e := validateDependencies(ctx, tx, scope, root.Input)
	if e != nil || len(findings) > 0 {
		return out, ErrBlocked
	}
	for _, dep := range root.DependencyLocks {
		if _, ok := out.Catalog[string(dep.ReleaseID)]; !ok {
			r, e := loadPackageRelease(ctx, tx, scope, string(dep.ReleaseID))
			if e != nil {
				return out, e
			}
			out.Catalog[string(dep.ReleaseID)] = r
		}
	}
	// Resolve only assets that the source-derived object closure actually uses.
	projection, e := projectPackageExportWithAssets(root, out.Catalog, func(asset string) (generated.FileRecord, error) {
		if cached, ok := out.Assets[asset]; ok {
			return cached, nil
		}
		if e := tx.RequireLock(Key(scope, "asset", asset)); e != nil {
			return generated.FileRecord{}, e
		}
		var document []byte
		var state, owner string
		if e := tx.QueryRowContext(ctx, "SELECT state,owner_subject_id,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, asset).Scan(&state, &owner, &document); e != nil {
			return generated.FileRecord{}, e
		}
		var file generated.FileRecord
		if generated.Decode("FileRecord", document, &file) != nil || state != "READY" || file.State != "READY" || file.Scope != Scope(scope) || string(file.AssetID) != asset || string(file.OwnerSubjectID) != owner {
			return file, ErrBlocked
		}
		out.Assets[asset] = file
		return file, nil
	}, scope.SubjectID)
	if e != nil {
		return out, e
	}
	if len(projection.Files) > 0 {
		if e = s.Store.RequirePackageExportAssetRead(ctx, tx, scope); e != nil {
			return out, e
		}
	}
	source := struct {
		RootHash string
		Body     json.RawMessage
		Files    map[string]generated.FileRecord
	}{root.ContentHash, projection.Body, map[string]generated.FileRecord{}}
	for _, asset := range projection.Files {
		source.Files[asset] = out.Assets[asset]
	}
	out.Basis, e = Hash(source)
	return out, e
}
func (s *Service) InspectPackageExportTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, id string) (string, error) {
	out, e := s.MaterializePackageExportTx(ctx, tx, scope, id)
	return out.Basis, e
}

func (s *Service) ExportPackage(ctx context.Context, scope domain.ProductScope, request generated.PackageExportRequest, key string) (generated.PackageExportReceipt, error) {
	var out generated.PackageExportReceipt
	requestRaw, _ := json.Marshal(request)
	if generated.Validate("PackageExportRequest", requestRaw) != nil || string(request.ProductID) != scope.ProductID {
		return out, ErrBlocked
	}
	target := string(request.ReleaseID)
	if found, e := s.hasTransferResult(ctx, scope, "exportContentPackage", key); e != nil {
		return out, e
	} else if found {
		raw, e := Command(ctx, s.Store, scope, "exportContentPackage", key, target, "PackageExportReceipt", request, nil, func(store.Tx) (any, error) { return nil, ErrBlocked })
		if e == nil {
			e = generated.Decode("PackageExportReceipt", raw, &out)
		}
		return out, e
	}
	material, e := s.Store.InspectPackageExportSource(ctx, scope, target)
	if e != nil {
		return out, e
	}
	projection, e := projectPackageExport(material.Root, material.Catalog, material.Assets, scope.SubjectID)
	if e != nil {
		return out, e
	}
	files := map[string][]byte{}
	total := int64(len(projection.Body))
	for _, id := range projection.Files {
		size, err := strconv.ParseInt(string(material.Assets[id].SizeBytes), 10, 64)
		if err != nil || size < 0 || size > wepkg.DefaultLimits.FileBytes {
			return out, ErrBlocked
		}
		total += size
		if total > wepkg.DefaultLimits.ArchiveBytes-wepkg.DefaultLimits.ManifestBytes-1<<20 {
			return out, ErrBlocked
		}
	}
	for name, id := range projection.Files {
		if s.PackageFiles == nil {
			return out, ErrBlocked
		}
		raw, record, e := s.PackageFiles.ReadPackageExportAsset(ctx, scope, id)
		if e != nil {
			return out, e
		}
		before, _ := Hash(material.Assets[id])
		after, _ := Hash(record)
		if before != after {
			return out, store.ErrConflict
		}
		files[name] = raw
	}
	bodyPath := "definitions/content.json"
	m := wepkg.Manifest{FormatVersion: "1.0", PackageID: request.PackageID, PackageVersion: request.PackageVersion, Kind: packageRootKind(material.Root), Compatibility: wepkg.Compatibility{PublicAPIMajor: 1, PackageFormatMajor: 1, MinimumRuntimeContract: projection.MinimumRuntimeContract, RulePrimitiveSet: "core.v1"}, Entrypoints: []string{"content"}, Entries: []wepkg.Entry{{LogicalID: "content", Kind: packageRootKind(material.Root), Path: bodyPath, Version: request.PackageVersion, References: []string{}}}, Files: []wepkg.FileRecord{{Path: bodyPath, MediaType: "application/json", Rights: wepkg.Rights{License: projection.License, SourceReference: "wepkg:static-authoring", RedistributionAllowed: true, AllowedUses: []string{"WORLD_CONTENT"}}}}, Dependencies: []wepkg.Dependency{}, Requirements: projection.Requirements, Provenance: wepkg.Provenance{AuthorDisplayName: request.AuthorDisplayName, License: projection.License, SourceReferences: []string{"wepkg:static-authoring"}, CreatedAt: material.Root.PublishedAt, SyntheticExamplesOnly: true}}
	m.Files = append(m.Files, projection.FileRecords...)
	archive, m, e := wepkg.ExportBundle(m, projection.Body, files)
	if e != nil {
		return out, e
	}
	sum := sha256.Sum256(archive)
	exportID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "package-export", key)
	locks, e := s.PlanPackageExportLocks(ctx, scope, target)
	if e != nil {
		return out, e
	}
	locks = append(locks, Key(scope, "content_check", exportID))
	result, e := Command(ctx, s.Store, scope, "exportContentPackage", key, target, "PackageExportReceipt", request, locks, func(tx store.Tx) (any, error) {
		if e := s.Store.ArmPackageExportInspection(ctx, tx, scope, target, material.Basis); e != nil {
			return nil, e
		}
		receipt := generated.PackageExportReceipt{ExportID: generated.Identity(exportID), Scope: Scope(scope), ReleaseID: request.ReleaseID, ReleaseHash: material.Root.ContentHash, RequestKey: key, PackageID: request.PackageID, PackageVersion: request.PackageVersion, ContentSha256: m.ContentSHA256, ArchiveSha256: hex.EncodeToString(sum[:]), SizeBytes: strconv.Itoa(len(archive)), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if e := tx.RequireLock(Key(scope, "content_check", exportID)); e != nil {
			return nil, e
		}
		document, _ := json.Marshal(receipt)
		sourceHash, _ := hex.DecodeString(material.Basis)
		_, e := tx.ExecContext(ctx, "INSERT INTO content_package_exports(tenant_id,product_id,export_id,owner_subject_id,release_id,archive_hash,source_hash,archive,document) VALUES(?,?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, exportID, scope.SubjectID, target, sum[:], sourceHash, archive, document)
		return receipt, e
	})
	if e == nil {
		e = generated.Decode("PackageExportReceipt", result, &out)
	}
	return out, e
}
func (s *Service) GetPackageExport(ctx context.Context, scope domain.ProductScope, id string) (generated.PackageExportReceipt, error) {
	var out generated.PackageExportReceipt
	read, e := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if e != nil {
		return out, e
	}
	defer read.Close()
	var raw []byte
	e = read.QueryRowContext(ctx, "SELECT document FROM content_package_exports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND export_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, id).Scan(&raw)
	if e != nil {
		return out, ErrNotFound
	}
	if e = generated.Decode("PackageExportReceipt", raw, &out); e != nil {
		return out, e
	}
	if out.Scope != Scope(scope) || string(out.ExportID) != id {
		return out, ErrBlocked
	}
	if e = read.Finish(ctx); e != nil {
		return generated.PackageExportReceipt{}, e
	}
	return out, nil
}
