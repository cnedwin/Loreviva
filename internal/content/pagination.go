package content

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"time"
)

// Cursors expire on restart, scope/gate/filter changes, or collection revisions.
// Metadata scanning is bounded; an oversized catalog fails closed before paging.
const maxCatalogRows = 10000

type PageOptions struct {
	Limit  int
	Cursor string
}
type cursorValue struct {
	Binding, Snapshot, Last string
	Expires                 int64
}

func (s *Service) cursorCipher() (cipher.AEAD, error) {
	s.cursorOnce.Do(func() { _, s.cursorError = rand.Read(s.cursorKey[:]) })
	if s.cursorError != nil {
		return nil, s.cursorError
	}
	block, e := aes.NewCipher(s.cursorKey[:])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}
func (s *Service) sealCursor(v cursorValue) (string, error) {
	a, e := s.cursorCipher()
	if e != nil {
		return "", e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(a.Seal(nonce, nonce, b, []byte("Loreviva-content-page-v1"))), nil
}
func (s *Service) openCursor(token string) (cursorValue, error) {
	var v cursorValue
	if len(token) > 2048 {
		return v, wire.ErrJSON
	}
	a, e := s.cursorCipher()
	if e != nil {
		return v, e
	}
	b, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(b) < a.NonceSize() {
		return v, wire.ErrJSON
	}
	b, e = a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("Loreviva-content-page-v1"))
	if e != nil || json.Unmarshal(b, &v) != nil || v.Expires <= time.Now().Unix() {
		return v, wire.ErrJSON
	}
	return v, nil
}

type pageEntry struct {
	ID, Revision string
	Hash         []byte
}

func (s *Service) page(ctx context.Context, scope domain.ProductScope, kind string, filters []string, opt PageOptions, query string, args []any, documentQuery string, documentArgs func(pageEntry) []any) ([][]byte, *string, error) {
	g, e := Gate(ctx, s.Store, scope)
	if e != nil {
		return nil, nil, ErrNotFound
	}
	if opt.Limit == 0 {
		opt.Limit = 50
	}
	if opt.Limit < 1 || opt.Limit > 100 {
		return nil, nil, wire.ErrJSON
	}
	binding, _ := Hash(struct {
		Scope   domain.ProductScope
		Gates   any
		Kind    string
		Filters []string
		Limit   int
	}{scope, g, kind, filters, opt.Limit})
	var cursor cursorValue
	if opt.Cursor != "" {
		cursor, e = s.openCursor(opt.Cursor)
		if e != nil || cursor.Binding != binding {
			return nil, nil, wire.ErrJSON
		}
	}
	tx, e := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if e != nil {
		return nil, nil, e
	}
	defer tx.Close()
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, nil, e
	}
	entries := []pageEntry{}
	h := sha256.New()
	for rows.Next() {
		var v pageEntry
		if e = rows.Scan(&v.ID, &v.Revision, &v.Hash); e != nil {
			rows.Close()
			return nil, nil, e
		}
		if len(entries) >= maxCatalogRows {
			rows.Close()
			return nil, nil, ErrBlocked
		}
		b, _ := json.Marshal(v)
		h.Write(b)
		entries = append(entries, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, nil, e
	}
	snapshot := hex.EncodeToString(h.Sum(nil))
	if opt.Cursor != "" && cursor.Snapshot != snapshot {
		return nil, nil, wire.ErrJSON
	}
	start := 0
	if opt.Cursor != "" {
		found := false
		for i, v := range entries {
			if v.ID == cursor.Last {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, nil, wire.ErrJSON
		}
	}
	end := min(start+opt.Limit, len(entries))
	documents := [][]byte{}
	for _, v := range entries[start:end] {
		var b []byte
		if e = tx.QueryRowContext(ctx, documentQuery, documentArgs(v)...).Scan(&b); e != nil {
			return nil, nil, e
		}
		documents = append(documents, b)
	}
	var next *string
	if end < len(entries) {
		token, e := s.sealCursor(cursorValue{binding, snapshot, entries[end-1].ID, time.Now().Add(time.Hour).Unix()})
		if e != nil {
			return nil, nil, e
		}
		next = &token
	}
	if e = tx.Finish(ctx); e != nil {
		return nil, nil, e
	}
	return documents, next, nil
}
func firstOptions(options []PageOptions) PageOptions {
	if len(options) > 0 {
		return options[0]
	}
	return PageOptions{}
}
func decodeDraftPage(documents [][]byte, next *string) (generated.DraftPage, error) {
	out := generated.DraftPage{Items: []generated.ContentDraft{}, NextCursor: next}
	for _, b := range documents {
		var d generated.ContentDraft
		if e := generated.Decode("ContentDraft", b, &d); e != nil {
			return out, e
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}
