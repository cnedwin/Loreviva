// Package files stores synthetic/private uploads outside all static roots.
package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxFileBytes = 16 << 20

var ErrNotFound = errors.New("private file missing or not visible")
var ErrMetadata = errors.New("invalid bounded private upload")

type MemoryAssetPolicy interface {
	AuthorizeMemoryAsset(context.Context, domain.ProductScope, string) error
}
type Store struct {
	MemoryPolicy MemoryAssetPolicy
	DB           *store.Store
	root         *os.Root
	RootPath     string
}
type StagedFile struct {
	key, relative string
	Size          uint64
	Hash          string
}

func New(db *store.Store, path string) (*Store, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, ErrMetadata
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, ErrMetadata
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil || !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(abs)) {
		return nil, ErrMetadata
	}
	info, e := os.Lstat(abs)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMetadata
	}
	root, e := os.OpenRoot(abs)
	if e != nil {
		return nil, ErrMetadata
	}
	return &Store{DB: db, root: root, RootPath: abs}, nil
}
func (f *Store) Close() error { return f.root.Close() }
func (f *Store) Check(ctx context.Context) error {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return permitError
	}
	defer release()
	if e := ctx.Err(); e != nil {
		return e
	}
	info, e := os.Lstat(f.RootPath)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrMetadata
	}
	var random [12]byte
	if _, e = rand.Read(random[:]); e != nil {
		return e
	}
	name := ".probe-" + hex.EncodeToString(random[:])
	file, e := f.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ErrMetadata
	}
	e = file.Close()
	if remove := f.root.Remove(name); e == nil {
		e = remove
	}
	return e
}
func (f *Store) productDir(scope domain.ProductScope) (string, error) {
	if e := scope.Validate(); e != nil {
		return "", ErrMetadata
	}
	dir := scope.TenantID + "/" + scope.ProductID
	for _, d := range []string{scope.TenantID, dir} {
		info, e := f.root.Lstat(d)
		if os.IsNotExist(e) {
			if e = f.root.Mkdir(d, 0700); e != nil && !os.IsExist(e) {
				return "", ErrMetadata
			}
			info, e = f.root.Lstat(d)
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrMetadata
		}
	}
	return dir, nil
}
func metadata(m generated.AssetUploadMetadata) (uint64, error) {
	if strings.TrimSpace(m.Name) != m.Name || m.Name == "" || len(m.Name) > 128 || strings.ContainsAny(m.Name, "/\\:\x00") || m.Name == "." || m.Name == ".." || strings.HasSuffix(m.Name, ".") {
		return 0, ErrMetadata
	}
	size, e := strconv.ParseUint(string(m.ExpectedSize), 10, 64)
	if e != nil || size == 0 || size > MaxFileBytes {
		return 0, ErrMetadata
	}
	h, e := hex.DecodeString(m.ExpectedSha256)
	if e != nil || len(h) != 32 {
		return 0, ErrMetadata
	}
	switch m.DeclaredType {
	case "text/plain", "text/markdown", "image/png", "image/jpeg", "application/pdf", "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
	default:
		return 0, ErrMetadata
	}
	return size, nil
}
func (f *Store) Stage(ctx context.Context, scope domain.ProductScope, input io.Reader, m generated.AssetUploadMetadata) (out StagedFile, err error) {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return out, permitError
	}
	defer release()
	size, e := metadata(m)
	if e != nil || string(m.ProductID) != scope.ProductID {
		return out, ErrMetadata
	}
	dir, e := f.productDir(scope)
	if e != nil {
		return out, e
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return out, e
	}
	out.key = hex.EncodeToString(random[:])
	out.relative = dir + "/" + out.key + ".part"
	// The durable index exists before bytes can be abandoned by a crash.
	gates, e := content.Gate(ctx, f.DB, scope)
	if e != nil {
		return out, e
	}
	stageLock := content.Key(scope, "asset", "staging."+out.key)
	payload := sha256.Sum256([]byte(out.key))
	_, e = f.DB.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "REGISTER_STAGE", CommandKey: "stage." + out.key, CommitKey: content.Identity(scope.TenantID, scope.ProductID, "stage", out.key), GateVersions: gates, PayloadHash: payload, Locks: []store.LockKey{stageLock}}, func(tx store.Tx) error {
		if e := tx.RequireLock(stageLock); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO file_staging(tenant_id,product_id,storage_key,owner_subject_id) VALUES(?,?,?,?)", scope.TenantID, scope.ProductID, out.key, scope.SubjectID)
		return e
	})
	if e != nil {
		return out, e
	}
	file, e := f.root.OpenFile(out.relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return out, ErrMetadata
	}
	defer func() {
		file.Close()
		if err != nil {
			_ = f.root.Remove(out.relative)
		}
	}()
	h := sha256.New()
	limited := io.LimitReader(input, int64(size)+1)
	reader := &contextReader{ctx: ctx, r: limited}
	n, e := io.Copy(io.MultiWriter(file, h), reader)
	if e != nil {
		return out, e
	}
	if uint64(n) != size {
		return out, ErrMetadata
	}
	out.Size = uint64(n)
	out.Hash = hex.EncodeToString(h.Sum(nil))
	if out.Hash != m.ExpectedSha256 {
		return out, ErrMetadata
	}
	if e = file.Sync(); e != nil {
		return out, e
	}
	if e = file.Close(); e != nil {
		return out, e
	}
	opened, e := f.root.Open(out.relative)
	if e != nil {
		return out, e
	}
	prefix := make([]byte, 512)
	n2, _ := opened.Read(prefix)
	opened.Close()
	prefix = prefix[:n2]
	detected := http.DetectContentType(prefix)
	switch m.DeclaredType {
	case "text/plain", "text/markdown":
		text, e := f.root.Open(out.relative)
		if e != nil {
			return out, e
		}
		all, readError := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: text}, MaxFileBytes+1))
		text.Close()
		if readError != nil || len(all) > MaxFileBytes || !utf8.Valid(all) {
			return out, ErrMetadata
		}
		if strings.Contains(detected, "html") || (!strings.HasPrefix(detected, "text/plain") && detected != "application/octet-stream") {
			return out, ErrMetadata
		}
	case "image/png", "image/jpeg", "application/pdf":
		if detected != m.DeclaredType {
			return out, ErrMetadata
		}
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		if detected != "application/zip" {
			return out, ErrMetadata
		}
	}
	var state string
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT state FROM file_staging WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, out.key).Scan(&state); e != nil || state != "ACTIVE" {
		return out, ErrMetadata
	}
	return out, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func (f *Store) discard(stage StagedFile) { _ = f.root.Remove(stage.relative) }
func (f *Store) path(scope domain.ProductScope, key, ext string) (string, error) {
	dir, e := f.productDir(scope)
	if e != nil {
		return "", e
	}
	if len(key) != 32 {
		return "", ErrNotFound
	}
	if _, e = hex.DecodeString(key); e != nil {
		return "", ErrNotFound
	}
	return dir + "/" + key + ext, nil
}
func (f *Store) finalize(scope domain.ProductScope, key string, stage StagedFile) error {
	part, e := f.path(scope, key, ".part")
	if e != nil {
		return e
	}
	ready, _ := f.path(scope, key, ".blob")
	if info, e := f.root.Lstat(ready); e == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrMetadata
		}
		return nil
	}
	if info, e := f.root.Lstat(part); e == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrMetadata
		}
	} else if os.IsNotExist(e) {
		if e = f.root.Rename(stage.relative, part); e != nil {
			return ErrMetadata
		}
	} else {
		return ErrMetadata
	}
	if e = f.root.Rename(part, ready); e != nil {
		return ErrMetadata
	}
	return nil
}

func (f *Store) AllocateDerived(scope domain.ProductScope, body string) (StagedFile, error) {
	if len(body) == 0 || len(body) > MaxFileBytes || !utf8.ValidString(body) {
		return StagedFile{}, ErrMetadata
	}
	dir, e := f.productDir(scope)
	if e != nil {
		return StagedFile{}, e
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return StagedFile{}, e
	}
	key := hex.EncodeToString(nonce[:])
	h := sha256.Sum256([]byte(body))
	return StagedFile{key: key, relative: dir + "/" + key + ".part", Size: uint64(len(body)), Hash: hex.EncodeToString(h[:])}, nil
}
func (f *Store) DerivedStorage(stage StagedFile) string { return stage.key }
func (f *Store) lockDerived(ctx context.Context, scope domain.ProductScope, key string) (func(), error) {
	if _, e := f.path(scope, key, ".part"); e != nil {
		return nil, e
	}
	root, e := localstate.Open(filepath.Join(f.RootPath, scope.TenantID, scope.ProductID, ".memory-locks", key), true)
	if e != nil {
		return nil, e
	}
	return root.Lock(ctx, "operations.lock")
}
func (f *Store) WriteDerived(ctx context.Context, scope domain.ProductScope, stage StagedFile, body string) error {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return permitError
	}
	defer release()
	done, e := f.lockDerived(ctx, scope, stage.key)
	if e != nil {
		return e
	}
	defer done()
	var permitted bool
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND delete_intent=0)", scope.TenantID, scope.ProductID, stage.key).Scan(&permitted); e != nil || !permitted {
		return ErrMetadata
	}
	h := sha256.Sum256([]byte(body))
	if uint64(len(body)) != stage.Size || hex.EncodeToString(h[:]) != stage.Hash {
		return ErrMetadata
	}
	for _, ext := range []string{".part", ".blob"} {
		path, _ := f.path(scope, stage.key, ext)
		existing, x := f.root.Open(path)
		if x == nil {
			data, x := io.ReadAll(io.LimitReader(existing, MaxFileBytes+1))
			existing.Close()
			actual := sha256.Sum256(data)
			if x != nil || hex.EncodeToString(actual[:]) != stage.Hash {
				return ErrMetadata
			}
			return nil
		}
		if !os.IsNotExist(x) {
			return x
		}
	}
	file, e := f.root.OpenFile(stage.relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer file.Close()
	if _, e = file.WriteString(body); e != nil {
		return e
	}
	return file.Sync()
}

func (f *Store) RecoverDerived(scope domain.ProductScope, key, body string) (StagedFile, error) {
	path, e := f.path(scope, key, ".part")
	if e != nil {
		return StagedFile{}, e
	}
	h := sha256.Sum256([]byte(body))
	return StagedFile{key: key, relative: path, Size: uint64(len(body)), Hash: hex.EncodeToString(h[:])}, nil
}
