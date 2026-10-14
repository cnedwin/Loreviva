package files_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"loreviva.local/loreviva/tests/integration/testprincipal"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type interrupted struct{}

func (interrupted) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestPrivateFilesNeverStaticAndInterruptedStageCleaned(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"INSERT INTO tenant_gates VALUES('t',1,1)", "INSERT INTO product_gates VALUES('t','p',1,1)", "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('t','p','owner',1,1,1,1)"} {
		if _, e := s.DB.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	scope := domain.ProductScope{TenantID: "t", ProductID: "p", SubjectID: "owner"}
	ctx = testprincipal.Register(t, s, scope)
	root := t.TempDir()
	f, e := files.New(s, root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = f.Close() })
	body := []byte("synthetic private file")
	h := sha256.Sum256(body)
	m := generated.AssetUploadMetadata{ProductID: "p", Name: "note.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "22", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "test", License: "Apache-2.0", AllowedUses: []string{"EDIT", "COPY", "PUBLISH"}}}
	if len(body) != 22 {
		t.Fatal("fixture size")
	}
	record, e := f.Upload(ctx, scope, bytes.NewReader(body), m, "upload")
	if e != nil {
		t.Fatal(e)
	}
	reader, e := f.OpenAuthorized(ctx, scope, string(record.AssetID))
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(reader)
	reader.Close()
	if e != nil || !bytes.Equal(got, body) {
		t.Fatal("private bytes", e)
	}
	wrong := scope
	wrong.SubjectID = "other"
	if _, e = f.OpenAuthorized(ctx, wrong, string(record.AssetID)); !errors.Is(e, files.ErrNotFound) {
		t.Fatal("wrongprincipal disclosed", e)
	}
	if _, e = f.Stage(ctx, scope, interrupted{}, m); e == nil {
		t.Fatal("interrupted upload accepted")
	}
	if e = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if filepath.Ext(p) == ".part" {
			t.Error("abandoned stage")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	// Stage registration is now its own durable barrier. Intake/finalization
	// acknowledgement failures therefore occur on commits 2 and 3.
	for _, lostAt := range []int{2, 3} {
		calls := 0
		s.Commit = func(_ context.Context, tx *sql.Tx) error {
			calls++
			if e := tx.Commit(); e != nil {
				return e
			}
			if calls == lostAt {
				return errors.New("synthetic lost acknowledgement")
			}
			return nil
		}
		key := fmt.Sprintf("lost-%d", lostAt)
		if _, e = f.Upload(ctx, scope, bytes.NewReader(body), m, key); !errors.Is(e, store.ErrCommitUnknown) {
			t.Fatal("expected commit unknown", lostAt, e)
		}
		s.Commit = nil
		recovered, e := f.Upload(ctx, scope, bytes.NewReader(body), m, key)
		if e != nil || recovered.State != "READY" {
			t.Fatal("file receipt unrecoverable", lostAt, e)
		}
		again, e := f.Upload(ctx, scope, bytes.NewReader(body), m, key)
		if e != nil || again.AssetID != recovered.AssetID {
			t.Fatal("file idempotency", e)
		}
		reader, e := f.OpenAuthorized(ctx, scope, string(recovered.AssetID))
		if e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(reader)
		reader.Close()
		if e != nil || !bytes.Equal(got, body) {
			t.Fatal("recovered bytes", e)
		}
	}
	invalid := append(bytes.Repeat([]byte("a"), 600), 0xff)
	bad := m
	bad.ExpectedSize = generated.ExactUnsigned(strconv.Itoa(len(invalid)))
	hash := sha256.Sum256(invalid)
	bad.ExpectedSha256 = hex.EncodeToString(hash[:])
	if _, e = f.Stage(ctx, scope, bytes.NewReader(invalid), bad); e == nil {
		t.Fatal("invalid text beyond sniff prefix accepted")
	}
	for _, name := range []string{"../escape", "/absolute", "x/y", "x\\y"} {
		m.Name = name
		if _, e = f.Stage(ctx, scope, bytes.NewReader(body), m); e == nil {
			t.Fatal("unsafe name accepted", name)
		}
	}
}
