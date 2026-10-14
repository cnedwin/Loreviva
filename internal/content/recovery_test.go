package content_test

import (
	"context"
	"database/sql"
	"errors"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestDeletionAndUnknownCommitReturnOriginalResult(t *testing.T) {
	s, scope, ctx := fixture(t)
	d, e := s.Create(ctx, scope, "WORLD", "", coffee(t), "create-delete")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.Impact(ctx, scope, string(d.DraftID), 1, "DELETE", "delete-check")
	if e != nil {
		t.Fatal(e)
	}
	s.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost acknowledgement")
	}
	_, e = s.Command(ctx, scope, string(d.DraftID), 1, "DELETE", string(check.ReportID), "", "", "delete")
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("expected unknown", e)
	}
	s.Store.Commit = nil
	result, e := s.Command(ctx, scope, string(d.DraftID), 1, "DELETE", string(check.ReportID), "", "", "delete")
	if e != nil || len(result) == 0 {
		t.Fatal("original deletion receipt unrecoverable", e)
	}
	if _, e = s.GetDraft(ctx, scope, string(d.DraftID), ""); e == nil {
		t.Fatal("deleted body remained visible")
	}
}
