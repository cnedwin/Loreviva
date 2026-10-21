package mysql

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestIntegrationLockKindsPreserveStudioBytes(t *testing.T) {
	old := map[int][]string{15: {"studio_credential"}, 25: {"studio_grants"}, 35: {"studio_scope"}, 45: {"", "studio_command"}, 90: {"item", "actor", "relationship", "memory", "content_project", "content_draft", "content_release", "asset", "content_check", "studio_settings", "studio_connection", "studio_consent"}, 110: {"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts", "studio_receipt"}}
	for rank, kinds := range old {
		for index, kind := range kinds {
			b := lockBytes(LockKey{Rank: rank, Kind: kind, IDs: []string{"x"}})
			if len(b) != 5 || b[0] != byte(rank) || b[1] != byte(index) || string(b[2:]) != "\x00\x01x" {
				t.Fatalf("old lock encoding changed: %d/%s", rank, kind)
			}
		}
	}
	seen := map[int]map[string]bool{}
	for rank, kinds := range rankKinds {
		seen[rank] = map[string]bool{}
		for _, kind := range kinds {
			if seen[rank][kind] {
				t.Fatal("duplicate registration", rank, kind)
			}
			seen[rank][kind] = true
		}
	}
	external := strings.Repeat("🙂", 128)
	if len(external) != 512 {
		t.Fatal("fixture")
	}
	raw := LockKey{Rank: 35, Kind: "integration_mapping", IDs: []string{"tenant", "product", "customer", external}}
	if _, e := PlanLocks([]LockKey{raw}); e == nil {
		t.Fatal("raw external identity bypassed 256-byte lock bound")
	}
	h := sha256.Sum256([]byte(external))
	raw.IDs[3] = hex.EncodeToString(h[:])
	keys := []LockKey{raw, {Rank: 25, Kind: "integration_entitlement", IDs: []string{"t", "p", "c", "e"}}, {Rank: 15, Kind: "integration_principal", IDs: []string{"p"}}, credentialHashKey(h)}
	p, e := PlanLocks(keys)
	if e != nil {
		t.Fatal(e)
	}
	for i, r := range []int{5, 15, 25, 35} {
		if p[i].Rank != r {
			t.Fatal("integration lock order changed")
		}
	}
}
