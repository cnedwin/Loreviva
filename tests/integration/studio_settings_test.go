package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestStudioSettingsCASAndACK(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	sc := domain.SettingsScope{Kind: "TENANT", TenantID: scope.TenantID}
	request := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_LOCAL_NOTIFICATIONS","expected_revision":"0","preference":{"channel":"LOCAL_DISPLAY","enabled":true,"events":["TASK_STATUS"],"frequency":"ON_OPEN"}}`)
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	if _, e := r.Store.ApplySettings(ctx, "notify-original", request); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("lost ACK", e)
	}
	r.Store.Commit = nil
	receipt, e := r.Store.ReadSettingsCommand(ctx, sc, "SETTINGS", "notify-original")
	if e != nil {
		t.Fatal(e)
	}
	replay, e := r.Store.ApplySettings(ctx, "notify-original", request)
	if e != nil || !bytes.Equal(receipt, replay) {
		t.Fatal("original receipt", e, string(receipt), string(replay))
	}
	if _, e = r.Store.ApplySettings(ctx, "notify-collision", request); !errors.Is(e, store.ErrConflict) {
		t.Fatal("CAS collision", e)
	}
	changed := bytes.Replace(request, []byte(`"enabled":true`), []byte(`"enabled":false`), 1)
	if _, e = r.Store.ApplySettings(ctx, "notify-original", changed); !errors.Is(e, store.ErrConflict) {
		t.Fatal("same key different payload", e)
	}
	snapshot, e := r.Store.ReadSettings(ctx, sc)
	if e != nil || snapshot.LocalNotifications.Revision != "1" || !snapshot.LocalNotifications.Enabled {
		t.Fatal(snapshot, e)
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM studio_settings_commands WHERE command_key='notify-original'").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE studio_operator_grants SET enabled=0 WHERE action='settings.notifications.write'")
		return e
	}
	next := bytes.Replace(request, []byte(`"expected_revision":"0"`), []byte(`"expected_revision":"1"`), 1)
	if _, e = r.Store.ApplySettings(ctx, "revoked-before-commit", next); !errors.Is(e, store.ErrDenied) {
		t.Fatal("owning revoked grant", e)
	}
	r.Store.BeforeCommit = nil
	if _, e = r.Store.DB.Exec("UPDATE studio_operator_grants SET enabled=0 WHERE action='settings.notifications.write'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ReadSettingsCommand(ctx, sc, "SETTINGS", "notify-original"); !errors.Is(e, store.ErrDenied) {
		t.Fatal("receipt borrowed withdrawn authority", e)
	}
	// Tenant settings remain independent of the operator's home product gate.
	if _, e = r.Store.DB.Exec("UPDATE product_gates SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ReadSettings(ctx, sc); e != nil {
		t.Fatal("tenant consulted home product", e)
	}
	if _, e = r.Store.ReadSettings(context.Background(), sc); e == nil {
		t.Fatal("read missing principal")
	}
	var v any
	if json.Unmarshal(receipt, &v) != nil {
		t.Fatal("receipt invalid JSON")
	}
}
