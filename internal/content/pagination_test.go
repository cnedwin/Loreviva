package content_test

import (
	"context"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/wire"
	"testing"
)

func TestOpaquePaginationScopeFilterAndRevisionBound(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, e := s.Create(ctx, scope, "WORLD", "", coffee(t), fmt.Sprintf("page-%d", i)); e != nil {
			t.Fatal(e)
		}
	}
	first, e := s.ListDrafts(ctx, scope, "", "WORLD", "", 2)
	if e != nil || len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatal("missing bounded continuation", first, e)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, d := range page.Items {
			if seen[string(d.DraftID)] {
				t.Fatal("duplicate")
			}
			seen[string(d.DraftID)] = true
		}
		if page.NextCursor == nil {
			break
		}
		page, e = s.ListDrafts(ctx, scope, "", "WORLD", "", 2, *page.NextCursor)
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != 5 {
		t.Fatal("omitted drafts", len(seen))
	}
	for _, cursor := range []string{"malformed", *first.NextCursor + "x"} {
		if _, e = s.ListDrafts(ctx, scope, "", "WORLD", "", 2, cursor); !errors.Is(e, wire.ErrJSON) {
			t.Fatal("tampered cursor accepted", e)
		}
	}
	if _, e = s.ListDrafts(ctx, scope, "", "CHARACTER", "", 2, *first.NextCursor); !errors.Is(e, wire.ErrJSON) {
		t.Fatal("filter mismatch accepted", e)
	}
	other := scope
	other.SubjectID = "other"
	if _, e = s.ListDrafts(ctx, other, "", "WORLD", "", 2, *first.NextCursor); !errors.Is(e, content.ErrNotFound) {
		t.Fatal("scope mismatch accepted", e)
	}
	d := first.Items[0]
	if _, e = s.SaveDraft(ctx, scope, string(d.DraftID), d.Input, 1, "change-page"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ListDrafts(ctx, scope, "", "WORLD", "", 2, *first.NextCursor); !errors.Is(e, wire.ErrJSON) {
		t.Fatal("revision mismatch accepted", e)
	}
	revisions, e := s.Revisions(ctx, scope, string(d.DraftID), content.PageOptions{Limit: 1})
	if e != nil || revisions.NextCursor == nil {
		t.Fatal("revision cursor missing", e)
	}
	next, e := s.Revisions(ctx, scope, string(d.DraftID), content.PageOptions{Limit: 1, Cursor: *revisions.NextCursor})
	if e != nil || len(next.Items) != 1 || next.Items[0].DraftRevision == revisions.Items[0].DraftRevision {
		t.Fatal("revision continuation", next, e)
	}
	for i, item := range first.Items {
		revision := uint64(1)
		if i == 0 {
			revision = 2
		}
		check, e := s.CheckPublish(ctx, scope, string(item.DraftID), revision, []string{"MOCK_DEMO"}, fmt.Sprintf("page-check-%d", i))
		if e != nil || !check.Passed {
			t.Fatal(check, e)
		}
		if _, e = s.Publish(ctx, scope, string(item.DraftID), revision, string(check.ReportID), []string{"MOCK_DEMO"}, fmt.Sprintf("page-publish-%d", i)); e != nil {
			t.Fatal(e)
		}
	}
	releases, e := s.ListReleases(ctx, scope, "", "", content.PageOptions{Limit: 1})
	if e != nil || releases.NextCursor == nil {
		t.Fatal("release continuation missing", e)
	}
	more, e := s.ListReleases(ctx, scope, "", "", content.PageOptions{Limit: 1, Cursor: *releases.NextCursor})
	if e != nil || len(more.Items) != 1 || more.Items[0].ReleaseID == releases.Items[0].ReleaseID || more.NextCursor != nil {
		t.Fatal("release duplicate/omission", more, e)
	}
	if _, e = s.ListReleases(ctx, scope, "", string(d.DraftID), content.PageOptions{Limit: 1, Cursor: *releases.NextCursor}); !errors.Is(e, wire.ErrJSON) {
		t.Fatal("release filter mismatch", e)
	}

}
