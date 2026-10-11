package content

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"os"
	"testing"
)

func TestFix1PinnedOwnerEligibilityBeforeFallback(t *testing.T) {
	b, e := os.ReadFile("../../examples/coffee/definitions.json")
	if e != nil {
		t.Fatal(e)
	}
	var in generated.DraftInput
	if e = generated.Decode("DraftInput", b, &in); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"PROGRAM", "SKILL", "AGENT", "CHARACTER"} {
		t.Run(kind, func(t *testing.T) {
			ref := map[string]any{"kind": "PINNED_DEFINITION", "release_id": "release", "object_id": "owner", "version": "1", "content_hash": "hash"}
			owner := map[string]any{"kind": "ROLE", "implementation_kind": kind, "generation": map[string]any{"mode": "INHERIT"}}
			if kind == "SKILL" || kind == "CHARACTER" {
				owner["kind"] = kind
			}
			key := referenceKey(ref)
			resolved := ResolvedDefinitions{key: owner}
			_, enabled, e := effectiveReferenceRoute(in, ref, "INTERACTION", resolved)
			if kind == "PROGRAM" || kind == "SKILL" {
				if e == nil || enabled {
					t.Fatalf("ineligible pinned %s inherited route", kind)
				}
			} else if e != nil || !enabled {
				t.Fatalf("eligible pinned %s blocked: %v", kind, e)
			}
			for _, mode := range []string{"OVERRIDE", "NONE"} {
				var g any
				json.Unmarshal([]byte(`{"mode":"OVERRIDE","connection_ref":"mock-local","model_id":"synthetic-text-v1","required_capabilities":["TEXT"],"allowed_external_uses":["NONE"]}`), &g)
				g.(map[string]any)["mode"] = mode
				owner["generation"] = g
				_, enabled, e = effectiveReferenceRoute(in, ref, "INTERACTION", resolved)
				if (kind == "PROGRAM" || kind == "SKILL") && (e == nil || enabled) {
					t.Fatalf("ineligible pinned %s %s accepted", kind, mode)
				}
			}
		})
	}
}
