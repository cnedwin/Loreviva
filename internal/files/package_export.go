package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"strconv"
)

// This is the ordinary governed file reader, never a storage-key shortcut.
// The owning export transaction additionally rechecks frozen asset metadata.
func (f *Store) ReadPackageExportAsset(ctx context.Context, scope domain.ProductScope, id string) ([]byte, generated.FileRecord, error) {
	record, e := f.Get(ctx, scope, id)
	if e != nil || record.State != "READY" {
		return nil, generated.FileRecord{}, ErrNotFound
	}
	reader, e := f.OpenAuthorized(ctx, scope, id)
	if e != nil {
		return nil, generated.FileRecord{}, e
	}
	defer reader.Close()
	raw, e := io.ReadAll(io.LimitReader(reader, wepkg.DefaultLimits.FileBytes+1))
	if e != nil || int64(len(raw)) > wepkg.DefaultLimits.FileBytes {
		return nil, generated.FileRecord{}, ErrMetadata
	}
	hash := sha256.Sum256(raw)
	if string(record.SizeBytes) != strconv.Itoa(len(raw)) || record.Sha256 != hex.EncodeToString(hash[:]) {
		return nil, generated.FileRecord{}, ErrMetadata
	}
	return raw, record, nil
}
