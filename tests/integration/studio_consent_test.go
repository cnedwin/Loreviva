package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"testing"
	"time"
)

func TestStudioConsentTransitionsAndActualUse(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	sc := domain.SettingsScope{Kind: "PRODUCT", TenantID: scope.TenantID, ProductID: &scope.ProductID}
	// Synthetic historical false is retained as a baseline, never displayed as consent.
	if _, e := r.Store.DB.Exec("UPDATE studio_self_consent_current SET last_action='LEGACY_FALSE',accepted_at=NULL,expires_at=NULL"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("DELETE FROM memory_subject_permissions WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	before, e := r.Store.ReadSelfConsent(ctx, sc)
	if e != nil || string(before.AcceptedAt) != "null" || before.CurrentUseEnabled {
		t.Fatal(before, e)
	}
	raw := generated.StudioSelfConsentCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"INITIAL","expected_revision":"1","policy_id":"R0_OWNER_PRIVATE_GENERATION_30D","accepted":true}`)
	receipt, e := r.Store.ApplySelfConsent(ctx, "self-initial", raw)
	if e != nil {
		t.Fatal(e)
	}
	after, e := r.Store.ReadSelfConsent(ctx, sc)
	if e != nil || !after.CurrentUseEnabled || after.State != "ACTIVE" || after.ConsentRevision != "2" {
		t.Fatal(after, e)
	}
	var accepted, expires time.Time
	var enabled bool
	if e = r.Store.DB.QueryRow("SELECT c.accepted_at,p.expires_at,p.enabled FROM studio_self_consent_current c JOIN memory_subject_permissions p ON p.tenant_id=c.tenant_id AND p.product_id=c.product_id AND p.caller_subject_id=c.subject_id AND p.data_subject_id=c.subject_id WHERE p.action='memory.generation.use' AND p.allowed_use='PRIVATE_RESPONSE'").Scan(&accepted, &expires, &enabled); e != nil || !enabled || expires.Sub(accepted) != 30*24*time.Hour {
		t.Fatal(accepted, expires, enabled, e)
	}
	replay, e := r.Store.ApplySelfConsent(ctx, "self-initial", raw)
	if e != nil || !bytes.Equal(receipt, replay) {
		t.Fatal("replay changed expiry", e)
	}
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic all-or-nothing") }
	revoke := generated.StudioSelfConsentCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"REVOKE","expected_revision":"2","policy_id":"R0_OWNER_PRIVATE_GENERATION_30D"}`)
	if _, e = r.Store.ApplySelfConsent(ctx, "rollback-revoke", revoke); e == nil {
		t.Fatal("injected commit failure")
	}
	r.Store.BeforeCommit = nil
	if after, e = r.Store.ReadSelfConsent(ctx, sc); e != nil || !after.CurrentUseEnabled || after.ConsentRevision != "2" {
		t.Fatal("partial permission/consent mutation", after, e)
	}
	if _, e = r.Store.ApplySelfConsent(ctx, "self-revoke", revoke); e != nil {
		t.Fatal(e)
	}
	if after, e = r.Store.ReadSelfConsent(ctx, sc); e != nil || after.CurrentUseEnabled || after.State != "REVOKED" {
		t.Fatal(after, e)
	}
	renew := bytes.Replace(raw, []byte(`"INITIAL"`), []byte(`"RENEW"`), 1)
	renew = bytes.Replace(renew, []byte(`"expected_revision":"1"`), []byte(`"expected_revision":"3"`), 1)
	if _, e = r.Store.ApplySelfConsent(ctx, "self-renew", renew); e != nil {
		t.Fatal(e)
	}
	if after, e = r.Store.ReadSelfConsent(ctx, sc); e != nil || !after.CurrentUseEnabled || after.ConsentRevision != "4" {
		t.Fatal(after, e)
	}
	if _, e = r.Store.ApplySelfConsent(context.Background(), "forged-self", renew); e == nil {
		t.Fatal("plain scope renewed consent")
	}
}
