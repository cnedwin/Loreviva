package domain

import (
	"strings"
	"testing"
)

func TestScopeRejectsEmptyFields(t *testing.T) {
	for _, s := range []Scope{{}, {ProductScope: ProductScope{TenantID: "tenant", SubjectID: "subject"}, WorldInstanceID: "world"}, {ProductScope: ProductScope{TenantID: "tenant", ProductID: "product"}, WorldInstanceID: "world"}} {
		if s.Validate() == nil {
			t.Fatal("empty field accepted")
		}
	}
	s := Scope{ProductScope: ProductScope{TenantID: "Tenant", ProductID: "product", SubjectID: "subject"}, WorldInstanceID: "world"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if (RelationshipScope{Scope: s, ActorInstanceID: "actor", PlayerID: "player"}).Validate() != nil {
		t.Fatal("valid relation rejected")
	}
	if (RelationshipScope{Scope: s, ActorInstanceID: "actor"}).Validate() == nil {
		t.Fatal("empty player accepted")
	}
}
func TestIdentityRules(t *testing.T) {
	for _, id := range []string{"", "_bad", "空", strings.Repeat("a", 65), "a b"} {
		if ValidateID(id) == nil {
			t.Fatalf("invalid ID accepted %q", id)
		}
	}
	for _, id := range []string{"Case", "case", "a.b_-1", strings.Repeat("a", 64)} {
		if ValidateID(id) != nil {
			t.Fatal("valid ID rejected")
		}
	}
	for _, id := range []string{"é", "e\u0301", "身份", strings.Repeat("界", 128)} {
		if ValidateExternalIdentity(id) != nil {
			t.Fatal("external identity rejected")
		}
	}
	if ValidateExternalIdentity(strings.Repeat("界", 129)) == nil || ValidateExternalIdentity(string([]byte{0xff})) == nil {
		t.Fatal("invalid external identity")
	}
	if ValidateIdempotencyKey("") == nil || ValidateIdempotencyKey("non ascii 空") == nil || ValidateIdempotencyKey(strings.Repeat("a", 129)) == nil {
		t.Fatal("invalid key")
	}
	if ValidateIdempotencyKey("key_1.-") == nil {
		return
	}
	t.Fatal("valid key rejected")
}
func TestVersionsAndActorRelationshipRequireTheirIDs(t *testing.T) {
	if (Versions{}).Validate() == nil {
		t.Fatal("empty release accepted")
	}
	if (Versions{ReleaseID: "release"}).Validate() != nil {
		t.Fatal("zero versions must remain valid")
	}
	scope := Scope{ProductScope: ProductScope{TenantID: "tenant", ProductID: "product", SubjectID: "subject"}, WorldInstanceID: "world"}
	if (ActorRelationshipScope{Scope: scope, ActorInstanceID: "actor"}).Validate() == nil {
		t.Fatal("empty actor target accepted")
	}
}
