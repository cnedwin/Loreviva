package mysql

import (
	"context"
	"testing"
)

func TestPackagePreviewUnitRejectsWrites(t *testing.T) {
	u := &unit{packagePreviewRead: true}
	if _, err := u.ExecContext(context.Background(), "DELETE FROM content_drafts"); err == nil || u.err == nil {
		t.Fatal("preview unit accepted a mutation without a database")
	}
}

func TestPackagePreviewPurposeCannotBeForged(t *testing.T) {
	for _, request := range []BarrierRequest{
		{Operation: "PACKAGE_LOCAL_PREVIEW"},
		{Operation: "OTHER", packagePreview: true},
		{Operation: "PACKAGE_LOCAL_PREVIEW", packagePreview: true},
	} {
		if validateBarrier(request) == nil {
			t.Fatal("incomplete or mismatched private preview purpose accepted")
		}
	}
}
