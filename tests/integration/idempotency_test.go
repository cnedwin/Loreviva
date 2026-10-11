package integration

import (
	"context"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestCommandCannotRegenerateCommitKeyAndRepeatBody(t *testing.T) {
	s, _, r := fixture(t, false)
	ctx := context.Background()
	if _, e := s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
	r.CommitKey = "changed-key"
	ran := false
	_, e := s.WithBarrier(ctx, r, func(store.Tx) error { ran = true; return nil })
	if e == nil || ran {
		t.Fatal("same command used a fresh commit key to rerun effect")
	}
}
