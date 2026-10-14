package mysql

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
)

func TestStudioLockOrderPreservesExistingEncoding(t *testing.T) {
	old := map[int][]string{10: {""}, 20: {""}, 30: {""}, 40: {""}, 45: {""}, 50: {""}, 60: {""}, 70: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"}, 80: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"}, 90: {"item", "actor", "relationship", "memory", "content_project", "content_draft", "content_release", "asset", "content_check"}, 100: {""}, 110: {"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts"}}
	for rank, kinds := range old {
		for i, kind := range kinds {
			key := LockKey{Rank: rank, Kind: kind, IDs: []string{"a", "bc"}}
			want := fmt.Sprintf("%02x%02x00016100026263", rank, i)
			if got := hex.EncodeToString(lockBytes(key)); got != want {
				t.Fatal(rank, kind, got, want)
			}
		}
	}
	keys := []LockKey{{Rank: 35, Kind: "studio_scope", IDs: []string{"t", "TENANT", "T"}}, {Rank: 25, Kind: "studio_grants", IDs: []string{"t", "c", "PRODUCT", "P:p"}}, {Rank: 15, Kind: "studio_credential", IDs: []string{"t", "c"}}}
	for _, rank := range []int{110, 100, 90, 80, 70, 60, 50, 45, 40, 30, 20, 10} {
		keys = append(keys, LockKey{Rank: rank, Kind: old[rank][0], IDs: []string{"a"}})
	}
	plan, e := PlanLocks(keys)
	if e != nil {
		t.Fatal(e)
	}
	var got []int
	for _, k := range plan {
		got = append(got, k.Rank)
	}
	if !reflect.DeepEqual(got, []int{10, 15, 20, 25, 30, 35, 40, 45, 50, 60, 70, 80, 90, 100, 110}) {
		t.Fatal(got)
	}
	if _, e = PlanLocks([]LockKey{{Rank: 60, Kind: "studio_connection", IDs: []string{"t", "p", "connection"}}}); e == nil {
		t.Fatal("rank60 reinterpreted as connection")
	}
	for _, rank := range []int{15, 25, 35} {
		if len(rankKinds[rank]) != 1 {
			t.Fatal("duplicate new rank", rank)
		}
	}
}
