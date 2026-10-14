package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/memory"
	"net/http/httptest"
	"testing"
)

func TestStudioMemoryCompositeHTTPRechecksActualAction(t *testing.T) {
	for _, extra := range []string{"memory.correct", "memory.source.govern", "memory.policy"} {
		t.Run(extra, func(t *testing.T) {
			r, scope, release := runtimeFixture(t)
			instance := runtimeControl(t, r, createRuntimeWorld(t, r, release, "composite-memory"), "START")
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", instance.Scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var aggregate memory.Aggregate
			if e := json.Unmarshal(raw, &aggregate); e != nil {
				t.Fatal(e)
			}
			var node memory.MemoryRecord
			for _, v := range aggregate.Nodes {
				node = v
				break
			}
			for _, a := range []string{"memory.govern", extra} {
				if _, e := r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner',?)", a); e != nil {
					t.Fatal(e)
				}
				if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", a); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id=?", scope.SubjectID); e != nil {
				t.Fatal(e)
			}
			path := "/world/v1/admin/instances/" + instance.Scope.WorldInstanceID + "/memory-governance"
			command := map[string]any{"operation": "CORRECT", "replacement": "corrected synthetic", "memory_ids": []string{node.ID}, "expected_revisions": map[string]string{node.ID: "1"}}
			if extra == "memory.source.govern" {
				command = map[string]any{"operation": "DELETE", "selector": map[string]any{"kind": "ALL_EXISTING", "target_kind": "SOURCE"}}
			}
			request := map[string]any{"product_id": "coffee", "data_subject_id": "creator", "actor_instance_id": node.ActorID, "command": command}
			if extra == "memory.policy" {
				path = "/world/v1/admin/instances/" + instance.Scope.WorldInstanceID + "/memory-use-policy"
				request = map[string]any{"product_id": "coffee", "data_subject_id": "creator", "actor_instance_id": node.ActorID, "operation": "SET_USE_POLICY", "action": "DISABLE", "uses": []string{"PRIVATE_RESPONSE"}, "expected_policy_revision": "1"}
			}
			raw, _ = json.Marshal(request)
			reached := false
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var accepting bool
				if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_GOVERNANCE_ACCEPT' AND command_key='composite')").Scan(&accepting); e != nil {
					return e
				}
				if accepting {
					reached = true
					_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action=?", extra)
					return e
				}
				return nil
			}
			q := httptest.NewRequest("POST", path, bytes.NewReader(raw))
			q.Header.Set("Authorization", "Bearer "+runtimeToken)
			q.Header.Set("Content-Type", "application/json")
			q.Header.Set("Idempotency-Key", "composite")
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			r.Store.BeforeCommit = nil
			if !reached || w.Code != 403 {
				t.Fatal("composite action was not rejected at owning commit", reached, w.Code, w.Body.String())
			}
			var count int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_governance WHERE world_instance_id=?", instance.Scope.WorldInstanceID).Scan(&count); e != nil || count != 0 {
				t.Fatal("revoked composite persisted", count, e)
			}
		})
	}
}

func TestStudioMemoryDirectRequiresRealSelfPrincipal(t *testing.T) {
	r, scope, release := runtimeFixture(t)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, release, "studio-memory"), "START")
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: scope, WorldInstanceID: instance.Scope.WorldInstanceID}, DataSubjectID: scope.SubjectID}
	var actor string
	if e := r.Store.DB.QueryRow("SELECT actor_id FROM actors WHERE world_instance_id=? LIMIT 1", ns.WorldInstanceID).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	// Direct self intake never required an administrator's memory registry action.
	if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE action LIKE 'memory.%'"); e != nil {
		t.Fatal(e)
	}
	svc := memory.New(r.Store, nil)
	if e := svc.RecordStatement(context.Background(), ns, "missing", "missing", actor, "synthetic", []memory.Use{memory.PrivateResponse}); e == nil {
		t.Fatal("plain namespace manufactured a principal")
	}
	ctx, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+runtimeToken)
	if e != nil {
		t.Fatal(e)
	}
	if e = svc.RecordStatement(ctx, ns, "self", "self", actor, "synthetic", []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal("actual direct self acquired registry requirement", e)
	}
	wrong := ns
	wrong.SubjectID = "different-subject"
	if e = svc.RecordStatement(ctx, wrong, "wrong", "wrong", actor, "synthetic", []memory.Use{memory.PrivateResponse}); e == nil {
		t.Fatal("principal crossed namespace subject")
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'")
		return e
	}
	if e = svc.RecordStatement(ctx, ns, "revoked", "revoked", actor, "synthetic", []memory.Use{memory.PrivateResponse}); e == nil {
		t.Fatal("credential revoked before owning commit accepted")
	}
	r.Store.BeforeCommit = nil
}

func TestStudioMemoryHTTPKeepsBothPermissionLayers(t *testing.T) {
	r, scope, release := runtimeFixture(t)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, release, "studio-memory-http"), "START")
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: scope, WorldInstanceID: instance.Scope.WorldInstanceID}, DataSubjectID: scope.SubjectID}
	var actor string
	if e := r.Store.DB.QueryRow("SELECT actor_id FROM actors WHERE world_instance_id=? LIMIT 1", ns.WorldInstanceID).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	ctx, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+runtimeToken)
	if e != nil {
		t.Fatal(e)
	}
	get := func() int {
		q := httptest.NewRequest("GET", "/world/v1/admin/instances/"+ns.WorldInstanceID+"/memories?product_id=coffee&data_subject_id=creator&actor_instance_id="+actor, nil)
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		return w.Code
	}
	if _, e = r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner','memory.metadata.read')"); e != nil {
		t.Fatal(e)
	}
	if code := get(); code != 403 {
		t.Fatal("admin action replaced self/use permission", code)
	}
	if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.metadata.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	if code := get(); code != 200 {
		t.Fatal("valid HTTP layers rejected", code)
	}
	if _, _, e = r.Memory.AdminRead(context.Background(), ns, actor, "memory.metadata.read"); e == nil {
		t.Fatal("read used plain namespace authority")
	}
	if _, _, e = r.Memory.AdminRead(ctx, ns, actor, "memory.metadata.read"); e != nil {
		t.Fatal("direct read with real self failed", e)
	}
	r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE action='memory.metadata.read'")
		return e
	}
	if code := get(); code != 403 {
		t.Fatal("HTTP entry authority survived owning transaction revocation", code)
	}
	r.Store.BeforeReadFinish = nil
	// Direct self reads must retain current product/subject gates even without
	// an HTTP action proof. The owning transaction observes late revocation.
	for _, statement := range []string{
		"UPDATE product_gates SET enabled=0 WHERE tenant_id='demo-local' AND product_id='coffee'",
		"UPDATE subject_governance SET enabled=0 WHERE tenant_id='demo-local' AND product_id='coffee' AND subject_id='creator'",
	} {
		r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, statement)
			return e
		}
		if _, _, e = r.Memory.AdminRead(ctx, ns, actor, "memory.metadata.read"); e == nil {
			t.Fatal("direct self read survived current scope revocation")
		}
	}
	r.Store.BeforeReadFinish = nil
}
