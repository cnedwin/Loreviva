package integration

import (
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestContentLockKindsFrozenAfterMemory(t *testing.T) {
	names := []string{"item", "actor", "relationship", "memory", "content_project", "content_draft", "content_release", "asset", "content_check"}
	var input []store.LockKey
	for i := len(names) - 1; i >= 0; i-- {
		input = append(input, store.LockKey{Rank: 90, Kind: names[i], IDs: []string{"t", "p", "a"}})
	}
	plan, e := store.PlanLocks(input)
	if e != nil {
		t.Fatal(e)
	}
	for i, k := range plan {
		if k.Kind != names[i] {
			t.Fatal("content order drift", plan)
		}
	}
}
