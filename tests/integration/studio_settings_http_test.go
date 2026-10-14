package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestStudioSettingsHTTPAndConcurrentCAS(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	raw := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_LOCAL_NOTIFICATIONS","expected_revision":"0","preference":{"channel":"LOCAL_DISPLAY","enabled":true,"events":[],"frequency":"IMMEDIATE"}}`)
	errs := make(chan error, 2)
	var group sync.WaitGroup
	for _, key := range []string{"race-one", "race-two"} {
		group.Add(1)
		go func(key string) { defer group.Done(); _, e := r.Store.ApplySettings(ctx, key, raw); errs <- e }(key)
	}
	group.Wait()
	close(errs)
	passed, conflicted := 0, 0
	for e := range errs {
		if e == nil {
			passed++
		} else if errors.Is(e, store.ErrConflict) {
			conflicted++
		} else {
			t.Fatal(e)
		}
	}
	if passed != 1 || conflicted != 1 {
		t.Fatal(passed, conflicted)
	}
	call := func(method, path, key, body string, want int, schema string) {
		t.Helper()
		q := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		if w.Code != want {
			t.Fatal(path, w.Code, w.Body.String())
		}
		var v any
		if e := generated.Decode(schema, w.Body.Bytes(), &v); e != nil {
			t.Fatal(path, "wire", e, w.Body.String())
		}
	}
	call("GET", "/world/v1/admin/settings?scope_type=TENANT&tenant_id=demo-local", "", "", 200, "StudioSettingsSnapshot")
	call("GET", "/world/v1/admin/settings/members?scope_type=TENANT&tenant_id=demo-local", "", "", 200, "StudioMemberPage")
	call("GET", "/world/v1/admin/settings/self-consent?scope_type=PRODUCT&tenant_id=demo-local&product_id=coffee", "", "", 200, "StudioSelfConsentSnapshot")
	call("POST", "/world/v1/admin/settings/commands", "stale-http", string(raw), 409, "StudioLocalCommandError")
	raw = bytes.Replace(raw, []byte(`"expected_revision":"0"`), []byte(`"expected_revision":"1"`), 1)
	call("POST", "/world/v1/admin/settings/commands", "http-notify", string(raw), 200, "StudioLocalCommandReceipt")
	call("GET", "/world/v1/admin/settings/commands/http-notify?scope_type=TENANT&tenant_id=demo-local&family=SETTINGS", "", "", 200, "StudioLocalCommandReceipt")
	call("POST", "/world/v1/admin/settings/self-consent/commands", "http-self", `{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"INITIAL","expected_revision":"1","policy_id":"R0_OWNER_PRIVATE_GENERATION_30D","accepted":true}`, 200, "StudioLocalCommandReceipt")
	call("POST", "/world/v1/admin/settings/members/commands", "http-member", `{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"SET_SCOPE_GRANT","expected_revision":"0","credential_id":"owner","subject_id":"creator","grant":{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"action":"content.read","use":"PRODUCT_ADMIN","effect":"ALLOW","enabled":true,"expires_at":null}}`, 200, "StudioLocalCommandReceipt")
	call("GET", "/world/v1/admin/settings?scope_type=TENANT&tenant_id=demo-local&product_id=coffee", "", "", 400, "StudioLocalCommandError")
	call("GET", "/world/v1/admin/settings/commands/missing?scope_type=TENANT&tenant_id=demo-local&family=SETTINGS", "", "", 404, "StudioLocalCommandError")
}
func TestStudioMembersCursorAndCurrentAuthority(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	sc := domain.SettingsScope{Kind: "TENANT", TenantID: scope.TenantID}
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "member-cursor", "synthetic-cursor-member-00000000000000001"); e != nil {
		t.Fatal(e)
	}
	page, e := r.Store.ListMembers(ctx, sc, 1, "")
	if e != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatal(page, e)
	}
	next, e := r.Store.ListMembers(ctx, sc, 1, *page.NextCursor)
	if e != nil || len(next.Items) != 1 || next.Items[0].CredentialID == page.Items[0].CredentialID {
		t.Fatal(next, e)
	}
	if _, e = r.Store.ListMembers(ctx, sc, 2, *page.NextCursor); e == nil {
		t.Fatal("cursor changed query")
	}
	if _, e = r.Store.ListMembers(ctx, sc, 1, *page.NextCursor+"forged"); e == nil {
		t.Fatal("forged cursor")
	}
	if _, e = r.Store.DB.Exec("UPDATE studio_operator_grants SET revision=revision+1,expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE credential_id='owner' AND action='members.manage' AND scope_type='TENANT'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ListMembers(ctx, sc, 1, *page.NextCursor); !errors.Is(e, store.ErrConflict) {
		t.Fatal("stale current-grant cursor", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE studio_operator_grants SET enabled=0 WHERE action='members.manage' AND scope_type='TENANT'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ListMembers(ctx, sc, 1, *page.NextCursor); !errors.Is(e, store.ErrDenied) {
		t.Fatal("revoked cursor", e)
	}
	b, _ := json.Marshal(next)
	if bytes.Contains(b, []byte("token_sha256")) {
		t.Fatal("private registry data exposed")
	}
}
