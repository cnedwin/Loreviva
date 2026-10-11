package files

import (
	"context"
	"encoding/json"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"strconv"
	"time"
)

func (f *Store) Get(ctx context.Context, scope domain.ProductScope, id string) (generated.FileRecord, error) {
	var out generated.FileRecord
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return out, permitError
	}
	defer release()
	if _, e := content.Gate(ctx, f.DB, scope); e != nil {
		return out, ErrNotFound
	}
	var derived bool
	if e := f.DB.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND asset_id=?)", scope.TenantID, scope.ProductID, id).Scan(&derived); e != nil {
		return out, ErrNotFound
	}
	if derived {
		if f.MemoryPolicy == nil {
			return out, ErrNotFound
		}
		if e := f.MemoryPolicy.AuthorizeMemoryAsset(ctx, scope, id); e != nil {
			return out, ErrNotFound
		}
	}
	var b []byte
	var state string
	e := f.DB.DB.QueryRowContext(ctx, "SELECT state,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, id).Scan(&state, &b)
	if e != nil {
		return out, ErrNotFound
	}
	if e = generated.Decode("FileRecord", b, &out); e != nil {
		return out, ErrNotFound
	}
	if out.AudiencePolicy == "PRODUCT_EDITORS" && string(out.OwnerSubjectID) != scope.SubjectID {
		return generated.FileRecord{}, ErrNotFound
	}
	out.State = state
	return out, nil
}
func (f *Store) OpenAuthorized(ctx context.Context, scope domain.ProductScope, id string) (io.ReadCloser, error) {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return nil, permitError
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	r, e := f.Get(ctx, scope, id)
	if e != nil || r.State != "READY" {
		return nil, ErrNotFound
	}
	var key string
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT storage_key FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=? AND state='READY'", scope.TenantID, scope.ProductID, id).Scan(&key); e != nil {
		return nil, ErrNotFound
	}
	p, e := f.path(scope, key, ".blob")
	if e != nil {
		return nil, ErrNotFound
	}
	info, e := f.root.Lstat(p)
	if e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrNotFound
	}
	opened, e := f.root.Open(p)
	if e != nil {
		return nil, ErrNotFound
	}
	transferred = true
	return &guardedReader{ReadCloser: opened, release: release, check: func() error { _, e := f.Get(ctx, scope, id); return e }}, nil
}

type guardedReader struct {
	io.ReadCloser
	release func()
	check   func() error
}

func (r *guardedReader) Read(b []byte) (int, error) {
	if e := r.check(); e != nil {
		return 0, e
	}
	return r.ReadCloser.Read(b)
}
func (r *guardedReader) Close() error { e := r.ReadCloser.Close(); r.release(); return e }
func (f *Store) Upload(ctx context.Context, scope domain.ProductScope, input io.Reader, m generated.AssetUploadMetadata, key string) (generated.FileRecord, error) {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return generated.FileRecord{}, permitError
	}
	defer release()
	var out generated.FileRecord
	stage, e := f.Stage(ctx, scope, input, m)
	if e != nil {
		return out, e
	}
	defer f.discard(stage)
	assetID := content.Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "upload", key)
	stageLock := content.Key(scope, "asset", "staging."+stage.key)
	locks := []store.LockKey{content.Key(scope, "asset", assetID), stageLock}
	raw, e := content.Command(ctx, f.DB, scope, "uploadContentAsset", key, "", "FileRecord", m, locks, func(tx store.Tx) (any, error) {
		r := generated.FileRecord{Scope: content.Scope(scope), AssetID: generated.Identity(assetID), State: "PENDING", Name: m.Name, MediaType: m.DeclaredType, SizeBytes: generated.ExactUnsigned(strconv.FormatUint(stage.Size, 10)), Sha256: stage.Hash, Purpose: m.Purpose, Rights: m.Rights, AudiencePolicy: m.AudiencePolicy, OwnerSubjectID: generated.Identity(scope.SubjectID), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		b, _ := json.Marshal(r)
		if e := tx.RequireLock(content.Key(scope, "asset", assetID)); e != nil {
			return nil, e
		}
		if e := tx.RequireLock(stageLock); e != nil {
			return nil, e
		}
		var stageState string
		if e := tx.QueryRowContext(ctx, "SELECT state FROM file_staging WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, stage.key).Scan(&stageState); e != nil {
			return nil, e
		}
		if stageState != "ACTIVE" {
			return nil, ErrMetadata
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO assets(tenant_id,product_id,asset_id,owner_subject_id,state,storage_key,document) VALUES(?,?,?,?,'PENDING',?,?)", scope.TenantID, scope.ProductID, assetID, scope.SubjectID, stage.key, b)
		return r, e
	})
	if e != nil {
		return out, e
	}
	if e = generated.Decode("FileRecord", raw, &out); e != nil {
		return out, e
	}
	current, e := f.Get(ctx, scope, assetID)
	if e != nil {
		return out, e
	}
	if current.State == "READY" {
		return current, nil
	}
	var storage string
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT storage_key FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, assetID, scope.SubjectID).Scan(&storage); e != nil {
		return out, ErrNotFound
	}
	if e = f.finalize(scope, storage, stage); e != nil {
		return out, e
	}
	raw, e = content.Command(ctx, f.DB, scope, "finalizeContentAsset", key, assetID, "FileRecord", m, locks, func(tx store.Tx) (any, error) {
		if e := tx.RequireLock(content.Key(scope, "asset", assetID)); e != nil {
			return nil, e
		}
		var state string
		if e := tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, assetID).Scan(&state); e != nil {
			return nil, e
		}
		if state != "PENDING" {
			return nil, ErrMetadata
		}
		current.State = "READY"
		b, _ := json.Marshal(current)
		_, e := tx.ExecContext(ctx, "UPDATE assets SET state='READY',document=? WHERE tenant_id=? AND product_id=? AND asset_id=? AND state='PENDING'", b, scope.TenantID, scope.ProductID, assetID)
		return current, e
	})
	if e == nil {
		e = generated.Decode("FileRecord", raw, &out)
	}
	return out, e
}

// BindDerived inserts only a freshly staged private server-rendered asset. The
// memory module records verified lineage in the same preplanned transaction.
func (f *Store) BindDerived(ctx context.Context, tx store.Tx, scope domain.ProductScope, id string, stage StagedFile) (generated.FileRecord, error) {
	r := generated.FileRecord{Scope: content.Scope(scope), AssetID: generated.Identity(id), State: "PENDING", Name: "private-memory.txt", MediaType: "text/plain", SizeBytes: generated.ExactUnsigned(strconv.FormatUint(stage.Size, 10)), Sha256: stage.Hash, Purpose: "MEDIA", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "R0 local memory fixture", License: "private synthetic fixture", AllowedUses: []string{}}, AudiencePolicy: "PRODUCT_EDITORS", OwnerSubjectID: generated.Identity(scope.SubjectID), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if e := tx.RequireLock(content.Key(scope, "asset", "staging."+stage.key)); e != nil {
		return r, e
	}
	var active bool
	if e := tx.QueryRowContext(ctx, "SELECT state='ACTIVE' AND owner_subject_id=? FROM file_staging WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.SubjectID, scope.TenantID, scope.ProductID, stage.key).Scan(&active); e != nil || !active {
		return r, ErrMetadata
	}
	if e := tx.RequireLock(content.Key(scope, "asset", id)); e != nil {
		return r, e
	}
	b, _ := json.Marshal(r)
	_, e := tx.ExecContext(ctx, "INSERT INTO assets(tenant_id,product_id,asset_id,owner_subject_id,state,storage_key,document) VALUES(?,?,?,?,'PENDING',?,?)", scope.TenantID, scope.ProductID, id, scope.SubjectID, stage.key, b)
	return r, e
}
func (f *Store) DerivedStageLock(scope domain.ProductScope, stage StagedFile) store.LockKey {
	return content.Key(scope, "asset", "staging."+stage.key)
}

func (f *Store) FinalizeDerived(ctx context.Context, scope domain.ProductScope, id string, stage StagedFile) error {
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	var key string
	if e := f.DB.DB.QueryRowContext(ctx, "SELECT storage_key FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, id).Scan(&key); e != nil {
		return ErrNotFound
	}
	done, e := f.lockDerived(ctx, scope, key)
	if e != nil {
		return e
	}
	defer done()
	var permitted bool
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM assets a JOIN memory_asset_lineage m USING(tenant_id,product_id,asset_id) WHERE a.tenant_id=? AND a.product_id=? AND a.asset_id=? AND a.state='PENDING' AND m.delete_intent=0)", scope.TenantID, scope.ProductID, id).Scan(&permitted); e != nil || !permitted {
		return ErrNotFound
	}
	return f.finalize(scope, key, stage)
}

// PurgeDerived removes only lineage-owned assets with durable delete intent.
// Metadata and the tombstone remain; arbitrary paths and static assets cannot
// enter this port. The caller advances the original task checkpoint afterward.
func (f *Store) purgeDerived(ctx context.Context, scope domain.ProductScope, key string) error {
	done, e := f.lockDerived(ctx, scope, key)
	if e != nil {
		return e
	}
	defer done()
	var intent bool
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT delete_intent FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, key).Scan(&intent); e != nil || !intent {
		return ErrNotFound
	}
	for _, ext := range []string{".blob", ".part"} {
		path, e := f.path(scope, key, ext)
		if e != nil {
			return e
		}
		if e = f.root.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}

func (f *Store) PurgeDerived(ctx context.Context, scope domain.ProductScope, key string) error {
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	return f.purgeDerived(ctx, scope, key)
}
func (f *Store) PurgeDerivedReplay(ctx context.Context, p *maintenance.ClosedPermit, scope domain.ProductScope, key string) error {
	release, e := p.PinReplay(ctx)
	if e != nil {
		return e
	}
	defer release()
	return f.purgeDerived(ctx, scope, key)
}
