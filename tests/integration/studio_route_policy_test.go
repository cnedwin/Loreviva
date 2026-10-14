package integration

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"testing"
)

func TestStudioPersistedDisableOriginalRecovery(t *testing.T) {
	r, draft, request := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := r.Auth.Scope()
	original, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "unknown-before-disabled", request)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDropOriginalReply
	if e = r.Worker.Once(context.Background()); e == nil {
		t.Fatal("drop fault not observed")
	}
	r.Worker.Turns.Fault = 0
	queued, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "queued-before-disabled", request)
	if e != nil {
		t.Fatal(e)
	}
	disable := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"DISABLE_CONNECTION","expected_revision":"1","connection_id":"mock-local"}`)
	if _, e = r.Store.ApplySettings(ctx, "persisted-off", disable); e != nil {
		t.Fatal(e)
	}
	independent := filepath.Dir(r.Local.Coordinator.Directory())
	private := r.Files.RootPath
	if e = r.Local.Participant.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	freshStore := store.New(r.Store.DB)
	freshFiles, e := files.New(freshStore, private)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { freshFiles.Close() })
	fresh := app.NewRuntime(freshStore, freshFiles, auth.New(freshStore, scope, "owner", runtimeToken))
	local, e := app.OpenLocal(context.Background(), freshStore, independent, private)
	if e != nil {
		t.Fatal(e)
	}
	fresh.BindLocal(local)
	fresh.Memory.Ledger = local.Ledger
	t.Cleanup(func() {
		if e := local.Participant.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	ctx = runtimeContext(t, fresh)
	if _, e = fresh.Store.AcceptDraftCandidate(ctx, scope, draft, "new-after-restart-disabled", request); !errors.Is(e, store.ErrDenied) {
		t.Fatal("persisted tenant disable did not block new intake", e)
	}
	if e = fresh.Worker.Once(context.Background()); e == nil {
		t.Fatal("previously accepted unsent task sent after disable")
	}
	var sent int
	if e = fresh.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND send_started_at IS NOT NULL", queued.TaskID).Scan(&sent); e != nil || sent != 0 {
		t.Fatal("queued task sent", sent, e)
	}
	if _, e = fresh.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", original.TaskID); e != nil {
		t.Fatal(e)
	}
	if e = fresh.Worker.QueryOriginalOnce(context.Background()); e != nil {
		t.Fatal("disable erased original query", e)
	}
	if _, _, e = fresh.Store.RunOriginalBills(context.Background()); e != nil {
		t.Fatal("disable erased original accounting", e)
	}
	if _, e = fresh.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),next_run_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", original.TaskID); e != nil {
		t.Fatal(e)
	}
	if e = fresh.Worker.Once(context.Background()); e != nil {
		t.Fatal("original sent task recovery", e)
	}
	view, e := fresh.Store.ReadDraftCandidateTask(ctx, scope, string(original.TaskID))
	if e != nil || view.TaskState != "COMMITTED" {
		t.Fatal(view, e)
	}
	if e = fresh.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=?", original.TaskID).Scan(&sent); e != nil || sent != 1 {
		t.Fatal("recovery acquired another attempt", sent, e)
	}
}

func TestStudioDefaultsFrozenForAcceptedCandidate(t *testing.T) {
	r, draft, request := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := r.Auth.Scope()
	current, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	for i, raw := range current.Input.Objects {
		var o map[string]any
		_ = json.Unmarshal(raw, &o)
		if o["object_id"] == string(request.ObjectID) {
			o["generation"] = map[string]any{"mode": "INHERIT", "required_capabilities": []string{}, "allowed_external_uses": []string{"NONE"}}
			current.Input.Objects[i], _ = json.Marshal(o)
		}
	}
	saved, e := r.Content.SaveDraft(ctx, scope, draft, current.Input, 1, "inherit-current-default")
	if e != nil {
		t.Fatal(e)
	}
	request.ExpectedDraftRevision = saved.DraftRevision
	receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "before-default-none", request)
	if e != nil {
		t.Fatal(e)
	}
	defaults := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"SET_DEFAULTS","expected_revision":"0","defaults":{"product_pack_ref":null,"module_defaults":[{"module_id":"AUTHORING","generation_settings":{"mode":"NONE","required_capabilities":[],"allowed_external_uses":["NONE"]}}]}}`)
	if _, e = r.Store.ApplySettings(ctx, "none-after-intake", defaults); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draft, "after-default-none", request); e == nil {
		t.Fatal("new intake ignored current NONE")
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal("new default rewrote accepted route", e)
	}
	view, e := r.Store.ReadDraftCandidateTask(ctx, scope, string(receipt.TaskID))
	if e != nil || view.TaskState != "COMMITTED" {
		t.Fatal(view, e)
	}
}

func TestStudioMockMetadataActualDescriptor(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	sc := domain.SettingsScope{Kind: "TENANT", TenantID: scope.TenantID}
	settings, e := r.Store.ReadSettings(ctx, sc)
	if e != nil || len(settings.Connections) != 1 || len(settings.ReferenceCatalog.Providers) != 1 {
		t.Fatal(settings, e)
	}
	var connection map[string]any
	if json.Unmarshal(settings.Connections[0], &connection) != nil || connection["connection_id"] != "mock-local" || connection["status"] != "UNVERIFIED" || connection["endpoint_alias_ref"] != nil || connection["secret_alias_ref"] != nil {
		t.Fatal(connection)
	}
	var descriptor string
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(document,'$.descriptor_hash')) FROM studio_reference_catalog WHERE scope_type='TENANT' AND ref_kind='PROVIDER'").Scan(&descriptor); e != nil || descriptor != domain.InstalledMockDescriptorHash() {
		t.Fatal(descriptor, e)
	}
	request := generated.StudioSettingsCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"DISABLE_CONNECTION","expected_revision":"1","connection_id":"mock-local"}`)
	if _, e = r.Store.ApplySettings(ctx, "persist-disable-mock", request); e != nil {
		t.Fatal(e)
	}
	settings, e = r.Store.ReadSettings(ctx, sc)
	if e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(settings.Connections[0], &connection)
	if connection["status"] != "DISABLED" || connection["revision"] != "2" {
		t.Fatal(connection)
	}
	connection["status"] = "UNVERIFIED"
	reenable, _ := json.Marshal(map[string]any{"scope": sc, "kind": "UPSERT_CONNECTION_METADATA", "expected_revision": "2", "connection": connection})
	if _, e = r.Store.DB.Exec("UPDATE studio_reference_catalog SET document=JSON_SET(document,'$.descriptor_hash',?) WHERE scope_type='TENANT' AND ref_kind='PROVIDER'", string(make([]byte, 64))); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplySettings(ctx, "wrong-installed-descriptor", reenable); !errors.Is(e, store.ErrDenied) {
		t.Fatal("metadata accepted an unproven installed descriptor", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE studio_reference_catalog SET document=JSON_SET(document,'$.descriptor_hash',?) WHERE scope_type='TENANT' AND ref_kind='PROVIDER'", domain.InstalledMockDescriptorHash()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplySettings(ctx, "reenable-exact-mock", reenable); e != nil {
		t.Fatal("explicit current CAS could not reenable installed Mock", e)
	}
}
