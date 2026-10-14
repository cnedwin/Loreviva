package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	"testing"
)

func TestStudioAdmissionAllWritersRespectActualDrain(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	if e := r.Local.Coordinator.BeginClose(context.Background(), "studio-drain"); e != nil {
		t.Fatal(e)
	}
	settings := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_LOCAL_NOTIFICATIONS","expected_revision":"0","preference":{"channel":"LOCAL_DISPLAY","enabled":false,"events":[],"frequency":"ON_OPEN"}}`)
	members := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_CREDENTIAL_STATE","expected_revision":"1","credential_id":"owner","subject_id":"creator","enabled":true,"expires_at":null}`)
	consent := generated.StudioSelfConsentCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"INITIAL","expected_revision":"1","policy_id":"R0_OWNER_PRIVATE_GENERATION_30D","accepted":true}`)
	if _, e := r.Store.ApplySettings(ctx, "closed-settings", settings); e == nil {
		t.Fatal("settings bypassed closing")
	}
	if _, e := r.Store.ApplyMember(ctx, "closed-member", members); e == nil {
		t.Fatal("member bypassed closing")
	}
	if _, e := r.Store.ApplySelfConsent(ctx, "closed-consent", consent); e == nil {
		t.Fatal("consent bypassed closing")
	}
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "closed-register", "synthetic-closed-token-00000000000000001"); e == nil {
		t.Fatal("registration bypassed closing")
	}
	if e := maintenance.InitializeLocalAdministration(ctx, r.Store); e == nil {
		t.Fatal("initialization bypassed closing")
	}
	if e := r.Local.Coordinator.Drain(context.Background(), "studio-drain"); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM studio_settings_commands").Scan(&n); e != nil || n != 0 {
		t.Fatal("closed writer persisted", n, e)
	}
}
func TestStudioAliasRegistrationAndDelegationCeilings(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	connection := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"UPSERT_CONNECTION_METADATA","expected_revision":"0","connection":{"connection_id":"synthetic-unregistered","revision":"0","name":"Synthetic missing refs","provider_ref":"missing-provider","model_id":"synthetic-model","endpoint_alias_ref":"missing-endpoint","secret_alias_ref":"missing-secret","status":"UNVERIFIED"}}`)
	if _, e := r.Store.ApplySettings(ctx, "no-invented-alias", connection); e == nil {
		t.Fatal("unregistered safe aliases accepted")
	}
	quota := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_QUOTA_POLICY","expected_revision":"0","quota_policy_ref":"not-registered"}`)
	if _, e := r.Store.ApplySettings(ctx, "no-invented-quota", quota); e == nil {
		t.Fatal("unregistered quota accepted")
	}
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "finite-target", "synthetic-finite-target-00000000000000001"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE studio_credential_state SET expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	sc := domain.SettingsScope{Kind: "PRODUCT", TenantID: scope.TenantID, ProductID: &scope.ProductID}
	raw, _ := json.Marshal(map[string]any{"scope": sc, "kind": "SET_SCOPE_GRANT", "expected_revision": "0", "credential_id": "finite-target", "subject_id": scope.SubjectID, "grant": map[string]any{"scope": sc, "action": "content.read", "use": "PRODUCT_ADMIN", "effect": "ALLOW", "enabled": true, "expires_at": nil}})
	if _, e := r.Store.ApplyMember(ctx, "unbounded-child", raw); e == nil {
		t.Fatal("finite issuer delegated unbounded grant")
	}
	var expires string
	if e := r.Store.DB.QueryRow("SELECT DATE_FORMAT(DATE_ADD(UTC_TIMESTAMP(),INTERVAL 30 MINUTE),'%Y-%m-%dT%H:%i:%sZ')").Scan(&expires); e != nil {
		t.Fatal(e)
	}
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	value["grant"].(map[string]any)["expires_at"] = expires
	raw, _ = json.Marshal(value)
	if _, e := r.Store.ApplyMember(ctx, "bounded-child", raw); e != nil {
		t.Fatal("actual bounded subset rejected", e)
	}
	state := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_CREDENTIAL_STATE","expected_revision":"1","credential_id":"finite-target","subject_id":"creator","enabled":true,"expires_at":null}`)
	if _, e := r.Store.ApplyMember(ctx, "unbounded-credential", state); e == nil {
		t.Fatal("finite issuer extended credential indefinitely")
	}
}
