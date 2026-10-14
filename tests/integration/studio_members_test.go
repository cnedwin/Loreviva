package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	"testing"
)

func TestStudioLastMaintainerCurrentCaller(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	secret := "synthetic-replacement-maintainer-000000000001"
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "replacement", secret); e != nil {
		t.Fatal(e)
	}
	sc := domain.SettingsScope{Kind: "TENANT", TenantID: scope.TenantID}
	grant := func(id, action, use, expected string) generated.StudioMemberCommandRequest {
		raw, _ := json.Marshal(map[string]any{"scope": sc, "kind": "SET_SCOPE_GRANT", "expected_revision": expected, "credential_id": id, "subject_id": scope.SubjectID, "grant": map[string]any{"scope": sc, "action": action, "use": use, "effect": "ALLOW", "enabled": true, "expires_at": nil}})
		return raw
	}
	if _, e := r.Store.ApplyMember(ctx, "replacement-grant", grant("replacement", "maintenance.manage", "MAINTENANCE_COMMAND", "0")); e != nil {
		t.Fatal(e)
	}
	disable := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_CREDENTIAL_STATE","expected_revision":"1","credential_id":"owner","subject_id":"creator","enabled":false,"expires_at":null}`)
	if _, e := r.Store.ApplyMember(ctx, "owner-cannot-use-registered-replacement", disable); e == nil {
		t.Fatal("registry-only replacement justified caller lockout")
	}
	replacement, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE studio_credential_state SET expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE credential_id='replacement'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyMember(replacement, "finite-replacement", disable); e == nil {
		t.Fatal("finite credential justified removal of last stable maintainer")
	}
	if _, e = r.Store.DB.Exec("UPDATE studio_credential_state SET expires_at=NULL WHERE credential_id='replacement'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyMember(replacement, "actual-current-replacement", disable); e != nil {
		t.Fatal(e)
	}
	if _, _, e = r.Auth.Authenticate(context.Background(), "Bearer "+runtimeToken); e == nil {
		t.Fatal("disabled owner authenticated")
	}
}
func TestStudioMembersExactDelegation(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "member", "synthetic-member-boundaries-00000000000001"); e != nil {
		t.Fatal(e)
	}
	sc := domain.SettingsScope{Kind: "PRODUCT", TenantID: scope.TenantID, ProductID: &scope.ProductID}
	raw := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"SET_SCOPE_GRANT","expected_revision":"0","credential_id":"member","subject_id":"creator","grant":{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"action":"content.read","use":"PRODUCT_ADMIN","effect":"ALLOW","enabled":true,"expires_at":null}}`)
	if _, e := r.Store.ApplyMember(ctx, "member-content", raw); e != nil {
		t.Fatal(e)
	}
	page, e := r.Store.ListMembers(ctx, sc, 100, "")
	if e != nil || len(page.Items) < 2 {
		t.Fatal(page, e)
	}
	next, _, e := r.Auth.Authenticate(context.Background(), "Bearer synthetic-member-boundaries-00000000000001")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Store.CheckOperator(next, scope, "content.read"); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.CheckOperator(next, scope, "content.edit"); e == nil {
		t.Fatal("grant became product wildcard")
	}
	if _, e = r.Store.ApplyMember(next, "member-cannot-delegate", raw); e == nil {
		t.Fatal("content grant became members.manage")
	}
}
