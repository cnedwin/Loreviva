package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"

	"loreviva.local/loreviva/internal/documents"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

var ErrDocumentPreviewLimit = errors.New("document preview exceeds encoded response limit")

// PreviewDocument holds the ordinary operation permit and guarded source reader
// through parsing and final current-authority inspection. No derived object or
// reusable source permission is persisted by this port.
func (f *Store) PreviewDocument(ctx context.Context, scope domain.ProductScope, id, expectedSHA256 string) (generated.DocumentPreview, error) {
	var out generated.DocumentPreview
	ctx, release, err := f.DB.EnterOperation(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, documents.Timeout)
	defer cancel()
	before, err := f.Get(ctx, scope, id)
	if err != nil || before.State != "READY" || before.Purpose != "KNOWLEDGE" || before.Sha256 != expectedSHA256 {
		return out, ErrNotFound
	}
	format := map[string]string{"text/plain": "TXT", "text/markdown": "MD", "application/pdf": "PDF", "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "DOCX"}[before.MediaType]
	if format == "" {
		return out, ErrMetadata
	}
	size, err := strconv.ParseUint(string(before.SizeBytes), 10, 64)
	if err != nil || size > documents.MaxInputBytes {
		return out, ErrMetadata
	}
	r, err := f.OpenAuthorized(ctx, scope, id)
	if err != nil {
		return out, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, documents.MaxInputBytes+1))
	if err != nil || len(raw) > documents.MaxInputBytes || uint64(len(raw)) != size {
		return out, ErrMetadata
	}
	report, err := documents.Extract(ctx, raw, format)
	if err != nil {
		return out, ErrMetadata
	}
	if report.SourceSHA256 != expectedSHA256 {
		return out, ErrMetadata
	}
	after, err := f.Get(ctx, scope, id)
	if err != nil || !reflect.DeepEqual(before, after) || after.State != "READY" {
		return out, ErrNotFound
	}
	b, err := json.Marshal(map[string]any{"asset": after, "report": report})
	if err == nil && len(b)+1 > documents.MaxResponseBytes {
		return out, ErrDocumentPreviewLimit
	}
	if err != nil || generated.Decode("DocumentPreview", b, &out) != nil {
		return generated.DocumentPreview{}, ErrMetadata
	}
	if err = ctx.Err(); err != nil {
		return generated.DocumentPreview{}, err
	}
	return out, nil
}
