package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func TestProductMockIdentityKeepsOriginalWorldBytes(t *testing.T) {
	type legacyIdentity struct {
		AuthorizationID                                                         string
		Scope                                                                   Scope
		TaskID, AttemptID, RequestKey, RequestHash, RouteID, QuoteID, MarkerKey string
	}
	old := legacyIdentity{AuthorizationID: "auth", Scope: Scope{ProductScope: ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, WorldInstanceID: "original-world"}, TaskID: "task", AttemptID: "attempt", RequestKey: "request", RequestHash: "hash", RouteID: "route", QuoteID: "quote", MarkerKey: "marker"}
	raw, _ := json.Marshal(old)
	var current MockRequestIdentity
	if e := json.Unmarshal(raw, &current); e != nil {
		t.Fatal(e)
	}
	got, _ := json.Marshal(current)
	if !bytes.Equal(raw, got) || sha256.Sum256(raw) != sha256.Sum256(got) {
		t.Fatal("existing WORLD journal preimage changed")
	}
	if e := current.Validate(); e != nil {
		t.Fatal(e)
	}
	current.Kind = "CANDIDATE"
	current.Scope.WorldInstanceID = ""
	if e := current.Validate(); e != nil {
		t.Fatalf("real product identity needs no fabricated WORLD: %v", e)
	}
	current.Scope.WorldInstanceID = "fake"
	if e := current.Validate(); e == nil {
		t.Fatal("product discriminator carried a fake world")
	}
	current.Kind = "OTHER"
	current.Scope.WorldInstanceID = ""
	if e := current.Validate(); e == nil {
		t.Fatal("open discriminator")
	}
}
