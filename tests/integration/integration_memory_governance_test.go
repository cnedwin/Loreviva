package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func selfMemoryFixture(t *testing.T, profile string, changes ...func(*app.Runtime, context.Context, *generated.DraftInput)) (*app.Runtime, context.Context, context.Context, generated.ClientSessionRequest, generated.ClientSession, string, memory.Aggregate) {
	t.Helper()
	actions := []string{"READ", "MEMORY_VIEW", "MEMORY_GOVERN"}
	var change func(*app.Runtime, context.Context, *generated.DraftInput)
	if len(changes) == 1 {
		change = changes[0]
	} else if len(changes) != 0 {
		t.Fatal("closed fixture change arity")
	}
	r, _, service, request, registration := integrationCreationFixtureActions(t, change, time.Hour, actions)
	world, e := r.World.CreateIntegrationInstance(service, "s7-world", request)
	if e != nil {
		t.Fatal(e)
	}
	input := generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: actions}
	session, e := r.Store.ExchangeIntegrationSession(service, "s7-session", profile, input)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world.Scope.WorldInstanceID, session.SubjectID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if json.Unmarshal(raw, &a) != nil || len(a.Nodes) == 0 {
		t.Fatal("real CREATE must install initial memory")
	}
	id := ""
	for k := range a.Nodes {
		id = k
		break
	}
	if _, n, _, e := r.Memory.ReadIntegrationMemoryMetadata(client, world.Scope.WorldInstanceID, id); e != nil || n.Body != "" {
		t.Fatal("legal current metadata without positive use prerequisite", e)
	}
	return r, client, service, input, session, world.Scope.WorldInstanceID, a
}

func selfMemoryHTTP(r *app.Runtime, session generated.ClientSession, profile, method, path, key string, raw []byte) *httptest.ResponseRecorder {
	q := httptest.NewRequest(method, path, bytes.NewReader(raw))
	q.Header.Set("Authorization", "Bearer "+session.AccessToken)
	q.Header.Set("Loreviva-Client-Contract", profile)
	if method != "GET" {
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Idempotency-Key", key)
	}
	out := httptest.NewRecorder()
	r.Handler().ServeHTTP(out, q)
	return out
}

// This first causal boundary starts from actual Registry/CREATE/CLIENT and
// metadata permission. A missing route is not a compiler-only failure.
func TestIntegrationClientMemoryListProfiles(t *testing.T) {
	for _, profile := range []string{"public_v1", "integration_r0_v1"} {
		t.Run(profile, func(t *testing.T) {
			r, client, service, input, session, world, a := selfMemoryFixture(t, profile)
			out := selfMemoryHTTP(r, session, profile, "GET", "/world/v1/instances/"+world+"/memories", "", nil)
			if out.Code != http.StatusOK {
				t.Fatalf("legal CLIENT metadata list must reach owning route: status=%d", out.Code)
			}
			if out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private metadata cache policy")
			}
			schema := "MemoryPage"
			single := "Memory"
			if profile == "integration_r0_v1" {
				schema = "PlayerMemoryMetadataPage"
				single = "PlayerMemoryMetadata"
			}
			if e := generated.Validate(schema, out.Body.Bytes()); e != nil {
				t.Fatal("actual profile list schema", e)
			}
			var result struct {
				Memories []json.RawMessage `json:"memories"`
				Next     *string           `json:"next_cursor"`
			}
			if json.Unmarshal(out.Body.Bytes(), &result) != nil || len(result.Memories) == 0 {
				t.Fatal("metadata items absent")
			}
			ids := []string{}
			for key := range a.Nodes {
				ids = append(ids, key)
			}
			sort.Strings(ids)
			for _, raw := range result.Memories {
				var item map[string]any
				if json.Unmarshal(raw, &item) != nil {
					t.Fatal("item JSON")
				}
				if profile == "public_v1" && item["content"] != "" {
					t.Fatal("public metadata leaked content")
				}
				if profile == "integration_r0_v1" {
					if item["body_readable"] != false {
						t.Fatal("metadata label granted positive use")
					}
					if _, exists := item["content"]; exists {
						t.Fatal("integration metadata contains body field")
					}
				}
			}
			base := "/world/v1/instances/" + world + "/memories"
			get := selfMemoryHTTP(r, session, profile, "GET", base+"/"+ids[0], "", nil)
			if get.Code != 200 || generated.Validate(single, get.Body.Bytes()) != nil {
				t.Fatal("single profile adapter", get.Code)
			}
			denied := selfMemoryHTTP(r, session, profile, "GET", base+"?include=provenance_v1", "", nil)
			if denied.Code != 403 {
				t.Fatal("provenance before use", denied.Code)
			}
			for _, query := range []string{"?limit=0", "?limit=101", "?limit=01", "?limit=1&limit=2", "?include=", "?include=other", "?cursor=", "?other=x"} {
				bad := selfMemoryHTTP(r, session, profile, "GET", base+query, "", nil)
				if bad.Code != 400 {
					t.Fatal("invalid bounded query", query, bad.Code)
				}
			}
			for _, query := range []string{
				"DELETE FROM memory_subject_permissions WHERE action='memory.metadata.read'",
				"UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.metadata.read'",
				"UPDATE memory_subject_permissions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE action='memory.metadata.read'",
				"UPDATE memory_subject_permissions SET caller_subject_id='s66-service' WHERE action='memory.metadata.read'",
				"UPDATE world_controls SET view_epoch=view_epoch+1",
			} {
				var injectionErr error
				called := false
				r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					called = true
					res, e := tx.ExecContext(ctx, query)
					if e != nil {
						injectionErr = e
						return e
					}
					n, e := res.RowsAffected()
					if e != nil {
						injectionErr = e
						return e
					}
					if n == 0 {
						injectionErr = fmt.Errorf("fault prerequisite updated zero rows")
						return injectionErr
					}
					return nil
				}
				bad := selfMemoryHTTP(r, session, profile, "GET", base, "", nil)
				r.Store.BeforeReadFinish = nil
				if !called || injectionErr != nil {
					t.Fatal("fault prerequisite", query, called, injectionErr)
				}
				if bad.Code != 403 {
					t.Fatal("final metadata/watermark fault must deny", query, bad.Code)
				}
				good := selfMemoryHTTP(r, session, profile, "GET", base, "", nil)
				if good.Code != 200 {
					t.Fatal("read rollback/release", good.Code)
				}
			}
			// Actual INITIAL may use a distinct lawful same-player Session. No SQL
			// permission or positive-use row is minted by this test fixture.
			consent, err := r.Store.ExchangeIntegrationSession(service, "s7-positive-session", "integration_r0_v1", input)
			if err != nil {
				t.Fatal(err)
			}
			retainSessionScanMaterial(t, consent.AccessToken)
			consentCtx, _, err := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+consent.AccessToken, domain.IntegrationClient)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Store.ApplyPlayerSelfConsent(consentCtx, "s7-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); err != nil {
				t.Fatal(err)
			}
			body := selfMemoryHTTP(r, session, profile, "GET", base+"?include=provenance_v1", "", nil)
			if body.Code != 200 || generated.Validate("MemoryProvenancePageV1", body.Body.Bytes()) != nil {
				t.Fatal("real positive provenance list", body.Code)
			}
			body = selfMemoryHTTP(r, session, profile, "GET", base+"/"+ids[0]+"?include=provenance_v1", "", nil)
			if body.Code != 200 || generated.Validate("MemoryProvenanceV1", body.Body.Bytes()) != nil {
				t.Fatal("real positive provenance single", body.Code)
			}
			page, err := r.Memory.ListIntegrationMetadata(client, world, "", "", profile, 1, false)
			if err != nil || len(page.Nodes) != 1 {
				t.Fatal("page1", err)
			}
			if len(a.Nodes) > 1 {
				if page.Next == "" {
					t.Fatal("missing bounded continuation")
				}
				next := selfMemoryHTTP(r, session, profile, "GET", base+"?limit=1&cursor="+url.QueryEscape(page.Next), "", nil)
				if next.Code != 200 {
					t.Fatal("real same-snapshot continuation", next.Code)
				}
				changed := page.Next[:len(page.Next)-1] + "!"
				if bad := selfMemoryHTTP(r, session, profile, "GET", base+"?cursor="+url.QueryEscape(changed), "", nil); bad.Code != 403 {
					t.Fatal("tampered cursor accepted")
				}
				other, err := r.Store.ExchangeIntegrationSession(service, "s7-other-session", profile, input)
				if err != nil {
					t.Fatal(err)
				}
				retainSessionScanMaterial(t, other.AccessToken)
				if bad := selfMemoryHTTP(r, other, profile, "GET", base+"?cursor="+url.QueryEscape(page.Next), "", nil); bad.Code != 403 {
					t.Fatal("foreign Session cursor accepted")
				}
			} else {
				t.Log("single actual initial node: continuation cardinality case not reached; separate producer fixture required")
			}
			if _, err = r.Store.ApplyPlayerSelfConsent(consentCtx, "s7-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); err != nil {
				t.Fatal(err)
			}
			page, err = r.Memory.ListIntegrationMetadata(client, world, "", "", profile, 50, false)
			if err != nil || page.BodyReadable || len(page.Nodes) == 0 {
				t.Fatal("withdrawn use must preserve legal metadata", err)
			}
			if bad := selfMemoryHTTP(r, session, profile, "GET", base+"?include=provenance_v1", "", nil); bad.Code != 403 {
				t.Fatal("withdrawn use body exposed", bad.Code)
			}
		})
	}
	for _, profile := range []string{"public_v1", "integration_r0_v1"} {
		t.Run("correct-renew-current-"+profile, func(t *testing.T) { verifySelfCorrectionCurrentReads(t, profile) })
	}
}

func TestIntegrationClientGovernanceOwningAndRecovery(t *testing.T) {
	r, client, service, _, session, world, a := selfMemoryFixture(t, "integration_r0_v1")
	var canGovern bool
	if e := r.Store.DB.QueryRow("SELECT can_govern FROM subject_governance WHERE tenant_id='demo-local' AND product_id='coffee' AND subject_id=?", session.SubjectID).Scan(&canGovern); e != nil {
		t.Fatal("actual registered subject prerequisite", e)
	}
	if !canGovern {
		t.Fatal("real initial MEMORY_GOVERN registration must issue its subject can_govern flag")
	}
	started, e := r.World.ControlIntegrationInstance(service, world, "s7-governance-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil {
		t.Fatal("real control prerequisite", e)
	}
	lease, e := r.Store.ClaimRegistered(context.Background(), "s7-governance-control", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: started.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.World.Process(context.Background(), lease, "s7-governance-control"); e != nil {
		t.Fatal("actual control worker", e)
	}
	id := ""
	for k := range a.Nodes {
		id = k
		break
	}
	raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"synthetic self request"}`, id, id, fmt.Sprint(a.Nodes[id].Revision)))
	if e := generated.Validate("MemoryGovernanceCommandV1", raw); e != nil {
		t.Fatal("source-valid own command prerequisite", e)
	}
	if e := r.Memory.BindIntegrationGovernance(); e != nil {
		t.Fatal("actual owner binding", e)
	}
	plan, e := r.Store.PlanIntegrationMemoryGovernance(client, world, "s7-own-delete", raw)
	if e != nil {
		t.Fatal("actual self planning", e)
	}
	plan.Close()
	hookReached := false
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { hookReached = true; return nil }
	var actualControl uint64
	if e := r.Store.DB.QueryRow("SELECT control_version FROM world_controls WHERE world_instance_id=?", world).Scan(&actualControl); e != nil {
		t.Fatal(e)
	}
	t.Log("actual accepted world control version", actualControl)
	direct, governErr := r.Memory.GovernIntegration(client, world, "s7-own-delete", raw)
	r.Store.BeforeCommit = nil
	if governErr != nil {
		var tasks, governance int
		r.Store.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_kind='GOVERNANCE'").Scan(&tasks)
		r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_governance").Scan(&governance)
		t.Fatalf("owning command failed: type=%T error=%v hook=%v tasks=%d governance=%d", governErr, governErr, hookReached, tasks, governance)
	}
	if direct.TaskKind != "MEMORY_GOVERNANCE" {
		t.Fatal("actual original task kind", direct.TaskKind)
	}
	out := selfMemoryHTTP(r, session, "integration_r0_v1", "POST", "/world/v1/instances/"+world+"/memory-governance", "s7-own-delete", raw)
	if out.Code != http.StatusAccepted {
		t.Fatalf("legal self governance must reach original Ledger/accept owner: status=%d", out.Code)
	}
	t.Run("real-registration-writer-and-replay", verifySelfGovernanceRegistration)
	t.Run("known-accepted-task-recovers-without-post", func(t *testing.T) {
		other := ""
		for k := range a.Nodes {
			if k != id {
				other = k
				break
			}
		}
		if other == "" {
			t.Fatal("second actual initial node prerequisite")
		}
		command := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"original lost acceptance ACK"}`, other, other, fmt.Sprint(a.Nodes[other].Revision)))
		committed := false
		r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
			if e := tx.Commit(); e != nil {
				return e
			}
			committed = true
			return errors.New("synthetic original acceptance ACK lost")
		}
		_, err := r.Memory.GovernIntegration(client, world, "s7-lost-accept", command)
		r.Store.Commit = nil
		var pending *memory.PreservationPending
		if !committed || !errors.As(err, &pending) || pending.TaskID == "" {
			t.Fatal("actual committed unknown prerequisite", committed, err)
		}
		view, err := r.Store.ReadIntegrationTask(client, pending.TaskID)
		if err != nil || view.Task.TaskID != pending.TaskID || view.Task.TaskState != "COMMITTED" {
			t.Fatal("known original task must finish existing preservation without replay POST", err, view.Task.TaskState)
		}
	})
	t.Run("correct-revoked-use-and-stable-person-replay", verifySelfGovernanceCorrection)
}

func verifySelfGovernanceCorrection(t *testing.T) {
	r, client, service, input, session, world, a := selfMemoryFixture(t, "integration_r0_v1")
	startSelfGovernance(t, r, service, world)
	id := ""
	for k, node := range a.Nodes {
		if node.Domain != memory.WorldFact {
			id = k
			break
		}
	}
	if id == "" {
		t.Fatal("real initial actor-belief node prerequisite")
	}
	other := ""
	for k, node := range a.Nodes {
		if node.ActorID != "" && node.ActorID != a.Nodes[id].ActorID {
			other = k
			break
		}
	}
	if other == "" {
		t.Fatal("actual second published/created actor prerequisite")
	}
	cross := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q,%q],"expected_revisions":{%q:%q,%q:%q}}`, id, other, id, fmt.Sprint(a.Nodes[id].Revision), other, fmt.Sprint(a.Nodes[other].Revision)))
	if e := generated.Validate("MemoryGovernanceCommandV1", cross); e != nil {
		t.Fatal("valid cross-actor wire", e)
	}
	if p, e := r.Store.PlanIntegrationMemoryGovernance(client, world, "s7-cross-actor", cross); e == nil {
		p.Close()
		t.Fatal("two real actors admitted as one governance scope")
	}
	over := map[string]any{"operation": "DELETE", "memory_ids": []string{}, "expected_revisions": map[string]string{}}
	ids := make([]string, 101)
	expected := map[string]string{}
	for i := range ids {
		ids[i] = fmt.Sprintf("bounded-node-%d", i)
		expected[ids[i]] = "1"
	}
	over["memory_ids"] = ids
	over["expected_revisions"] = expected
	oversized, _ := json.Marshal(over)
	if generated.Validate("MemoryGovernanceCommandV1", oversized) == nil {
		t.Fatal("source 100 item bound")
	}
	raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","selector":{"kind":"IDS","target_kind":"MEMORY_NODE","ids":[%q],"expected_revisions":{%q:%q}},"replacement":"synthetic corrected preference","reason":"self correction"}`, id, id, fmt.Sprint(a.Nodes[id].Revision)))
	if e := generated.Validate("MemoryGovernanceCommandV1", raw); e != nil {
		t.Fatal("original selector wire", e)
	}
	if _, e := r.Store.ApplyPlayerSelfConsent(client, "s7-correct-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ApplyPlayerSelfConsent(client, "s7-correct-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e := r.Memory.ReadIntegrationMemory(client, world, id); e == nil {
		t.Fatal("revoked body unexpectedly readable")
	}
	out := selfMemoryHTTP(r, session, "integration_r0_v1", "POST", "/world/v1/instances/"+world+"/memory-governance", "s7-correct", raw)
	if out.Code != 202 {
		t.Fatalf("revoked positive-use must not prevent necessary lawful self correction: %d", out.Code)
	}
	var receipt generated.TaskReceipt
	if e := json.Unmarshal(out.Body.Bytes(), &receipt); e != nil || receipt.TaskState != "COMMITTED" {
		t.Fatal("original cleanup task", e, receipt.TaskState)
	}
	epochs := func() [2]uint64 {
		t.Helper()
		var p, v uint64
		if e := r.Store.DB.QueryRow("SELECT privacy_epoch,view_epoch FROM subject_governance WHERE subject_id=?", session.SubjectID).Scan(&p, &v); e != nil {
			t.Fatal(e)
		}
		return [2]uint64{p, v}
	}
	before := epochs()
	next, e := r.Store.ExchangeIntegrationSession(service, "s7-correct-next-session", "integration_r0_v1", input)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, next.AccessToken)
	replay := selfMemoryHTTP(r, next, "integration_r0_v1", "POST", "/world/v1/instances/"+world+"/memory-governance", "s7-correct", raw)
	var replayReceipt generated.TaskReceipt
	json.Unmarshal(replay.Body.Bytes(), &replayReceipt)
	if replay.Code != 202 || replayReceipt.TaskID != receipt.TaskID || before != epochs() {
		t.Fatal("same-person new Session replay changed original task/epochs", replay.Code)
	}
	for _, changed := range [][]byte{bytes.Replace(raw, []byte("self correction"), []byte("changed reason"), 1), append([]byte(" "), raw...)} {
		bad := selfMemoryHTTP(r, next, "integration_r0_v1", "POST", "/world/v1/instances/"+world+"/memory-governance", "s7-correct", changed)
		if bad.Code != 409 {
			t.Fatal("same key must conflict for different exact original wire/reason", bad.Code)
		}
	}
	view, e := r.Store.ReadIntegrationTask(client, receipt.TaskID)
	if e != nil || view.Task.TaskID != receipt.TaskID || view.Billing.CustomerConsumedUnits != "0" || len(view.Attempts) != 0 {
		t.Fatal("real original governance task accounting", e)
	}
	if _, e = r.Store.ReadIntegrationTask(client, "not-an-original-task"); e == nil {
		t.Fatal("unassociated unknown task manufactured")
	}
	cancelled := selfMemoryHTTP(r, next, "integration_r0_v1", "POST", "/world/v1/tasks/"+receipt.TaskID+"/cancel", "s7-no-governance-cancel", []byte(`{}`))
	if cancelled.Code == 202 || cancelled.Code == 200 {
		t.Fatal("governance inherited generic task cancellation")
	}
	entry, e := r.Local.Ledger.Lookup(context.Background(), receipt.TaskID)
	if e != nil || entry.Kind != "APPENDED" || entry.Binding == nil || entry.Binding.Action != "CORRECT" || entry.Binding.ReplacementID == "" {
		t.Fatal("real original preservation binding", e)
	}
	var doc []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world, session.SubjectID).Scan(&doc); e != nil {
		t.Fatal(e)
	}
	var after memory.Aggregate
	if e = json.Unmarshal(doc, &after); e != nil {
		t.Fatal(e)
	}
	replacement, ok := after.Nodes[entry.Binding.ReplacementID]
	if !ok || replacement.Body != "synthetic corrected preference" {
		t.Fatal("actual replacement body absent")
	}
}

func verifySelfGovernanceRegistration(t *testing.T) {
	r, operator, _, _, original := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "MEMORY_GOVERN"})
	command := func(name string, actions []string) domain.RegistryCommand {
		v := *original.Player
		v.PlayerSubjectID = name
		v.ExternalIDBytes = []byte(name)
		v.EntitlementID = name + "-entitlement"
		v.Actions = append([]string(nil), actions...)
		return domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: name, Player: &v}
	}
	flag := func(subject string) bool {
		t.Helper()
		var value bool
		if e := r.Store.DB.QueryRow("SELECT can_govern FROM subject_governance WHERE tenant_id='demo-local' AND product_id='coffee' AND subject_id=?", subject).Scan(&value); e != nil {
			t.Fatal(e)
		}
		return value
	}
	for _, tc := range []struct {
		name    string
		actions []string
		want    bool
	}{{"register-govern", []string{"READ", "MEMORY_GOVERN"}, true}, {"register-read", []string{"READ"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := command(tc.name, tc.actions)
			if _, e := r.Store.ApplyIntegrationRegistry(operator, tc.name, c); e != nil {
				t.Fatal(e)
			}
			if flag(tc.name) != tc.want {
				t.Fatal("initial subject flag differs from actual validated actions")
			}
		})
	}
	for _, actions := range [][]string{{"READ", "MEMORY_GOVERN", "MEMORY_GOVERN"}, {"READ", "UNKNOWN_ACTION"}} {
		c := command("register-invalid", actions)
		if _, e := r.Store.ApplyIntegrationRegistry(operator, "register-invalid", c); e == nil {
			t.Fatal("invalid or duplicate action accepted")
		}
	}
	for _, tc := range []struct {
		name  string
		grant bool
		mode  string
	}{{"true-to-false", true, "flip"}, {"false-to-true", false, "flip"}, {"missing-original", true, "missing"}, {"wrong-identity", true, "move"}} {
		t.Run(tc.name, func(t *testing.T) {
			actions := []string{"READ"}
			if tc.grant {
				actions = append(actions, "MEMORY_GOVERN")
			}
			subject := "registry-fault-" + tc.name
			c := command(subject, actions)
			called := false
			var injectionErr error
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				called = true
				exec := func(query string, args ...any) error {
					res, e := tx.ExecContext(ctx, query, args...)
					if e != nil {
						return e
					}
					n, e := res.RowsAffected()
					if e != nil {
						return e
					}
					if n < 1 {
						return fmt.Errorf("registry fault changed zero rows")
					}
					return nil
				}
				if tc.mode == "flip" {
					injectionErr = exec("UPDATE subject_governance SET can_govern=? WHERE tenant_id='demo-local' AND product_id='coffee' AND subject_id=?", !tc.grant, subject)
					return injectionErr
				}
				for _, q := range []string{"DELETE FROM memory_subject_permissions WHERE data_subject_id=?", "DELETE FROM integration_entitlements WHERE player_subject_id=?", "DELETE FROM integration_external_subjects WHERE player_subject_id=?", "DELETE FROM subject_governance WHERE subject_id=?"} {
					if e := exec(q, subject); e != nil {
						injectionErr = e
						return e
					}
				}
				if tc.mode == "move" {
					injectionErr = exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled,can_govern) VALUES('demo-local','coffee',?,1,1,1,1,1)", subject+"-other")
				}
				return injectionErr
			}
			receipt, e := r.Store.ApplyIntegrationRegistry(operator, subject, c)
			r.Store.BeforeCommit = nil
			if !called || injectionErr != nil {
				t.Fatal("registry injection prerequisite", called, injectionErr)
			}
			if e == nil || receipt.ReceiptRef != "" {
				t.Fatal("changed writer tuple committed")
			}
			var n int
			err := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM subject_governance WHERE subject_id IN (?,?))+(SELECT COUNT(*) FROM integration_external_subjects WHERE player_subject_id=?)+(SELECT COUNT(*) FROM integration_entitlements WHERE player_subject_id=?)+(SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id=?)+(SELECT COUNT(*) FROM integration_receipts WHERE original_key=?)", subject, subject+"-other", subject, subject, subject, subject).Scan(&n)
			if err != nil || n != 0 {
				t.Fatal("full registry rollback", n, err)
			}
		})
	}
	t.Run("existing-revocation-is-not-reissued", func(t *testing.T) {
		c := command("register-govern", []string{"READ", "MEMORY_GOVERN"})
		res, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=0 WHERE tenant_id='demo-local' AND product_id='coffee' AND subject_id=? AND can_govern=1", c.Player.PlayerSubjectID)
		if e != nil {
			t.Fatal(e)
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			t.Fatal("owned deny fixture", n, e)
		}
		if _, e = r.Store.ApplyIntegrationRegistry(operator, "register-govern", c); e != nil {
			t.Fatal("original replay", e)
		}
		if flag(c.Player.PlayerSubjectID) {
			t.Fatal("original replay regranted")
		}
		c.Player.EntitlementID = "register-govern-appended"
		if _, e = r.Store.ApplyIntegrationRegistry(operator, "register-govern-append", c); e != nil {
			t.Fatal("existing subject append", e)
		}
		if flag(c.Player.PlayerSubjectID) {
			t.Fatal("append regranted revoked subject")
		}
	})
}

func integrationTestCreationID(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func startSelfGovernance(t *testing.T, r *app.Runtime, service context.Context, world string) {
	t.Helper()
	receipt, e := r.World.ControlIntegrationInstance(service, world, "s7-final-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(context.Background(), "s7-final-control", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: receipt.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.World.Process(context.Background(), lease, "s7-final-control"); e != nil {
		t.Fatal(e)
	}
	if e = r.Memory.BindIntegrationGovernance(); e != nil {
		t.Fatal(e)
	}
}

func TestIntegrationClientGovernanceFinalAndClose(t *testing.T) {
	r, client, service, _, session, world, a := selfMemoryFixture(t, "integration_r0_v1")
	startSelfGovernance(t, r, service, world)
	id := ""
	for k := range a.Nodes {
		id = k
		break
	}
	raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"bounded final test"}`, id, id, fmt.Sprint(a.Nodes[id].Revision)))
	plan := func(key string) *store.IntegrationMemoryGovernancePlan {
		t.Helper()
		p, e := r.Store.PlanIntegrationMemoryGovernance(client, world, key, raw)
		if e != nil {
			t.Fatal("lawful private plan", e)
		}
		return p
	}
	t.Run("copied-plan", func(t *testing.T) {
		p := plan("s7-copy")
		defer p.Close()
		copy := *p
		if _, e := r.Store.GovernIntegrationMemory(client, &copy); e == nil {
			t.Fatal("copy recreated Store seal")
		}
	})
	t.Run("closed-plan", func(t *testing.T) {
		p := plan("s7-closed")
		p.Close()
		if _, e := r.Store.GovernIntegrationMemory(client, p); e == nil {
			t.Fatal("closed plan admitted")
		}
	})
	t.Run("foreign-store", func(t *testing.T) {
		p := plan("s7-foreign")
		defer p.Close()
		foreign := &store.Store{}
		if _, e := foreign.GovernIntegrationMemory(client, p); e == nil {
			t.Fatal("foreign Store admitted")
		}
	})
	t.Run("replacement-owner-and-dependencies", func(t *testing.T) {
		foreign := memory.New(r.Store, r.Local.Ledger)
		foreign.Files = r.Files
		if e := foreign.BindIntegrationGovernance(); e == nil {
			t.Fatal("different memory object bound to same Store")
		}
		saved := r.Memory.Files
		r.Memory.Files = nil
		_, e := r.Memory.GovernIntegration(client, world, "s7-replaced-files", raw)
		r.Memory.Files = saved
		if e == nil {
			t.Fatal("replacement Files admitted")
		}
		ledger := r.Memory.Ledger
		r.Memory.Ledger = nil
		_, e = r.Memory.GovernIntegration(client, world, "s7-replaced-ledger", raw)
		r.Memory.Ledger = ledger
		if e == nil {
			t.Fatal("replacement Ledger admitted")
		}
	})
	t.Run("current-caller-is-not-a-bearer-plan", func(t *testing.T) {
		p := plan("s7-wrong-caller")
		defer p.Close()
		if _, e := r.Store.GovernIntegrationMemory(service, p); e == nil {
			t.Fatal("SERVICE context reused CLIENT sealed plan")
		}
	})
	if t.Failed() {
		return
	}
	snapshot := func() []byte {
		t.Helper()
		var doc []byte
		if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world, session.SubjectID).Scan(&doc); e != nil {
			t.Fatal(e)
		}
		return doc
	}
	before := snapshot()
	faults := []struct {
		name, sql string
		args      []any
	}{
		{"permission", "UPDATE memory_subject_permissions SET enabled=0 WHERE data_subject_id=? AND action='memory.govern'", []any{session.SubjectID}},
		{"subject-flag", "UPDATE subject_governance SET can_govern=0 WHERE subject_id=?", []any{session.SubjectID}},
		{"world-revision", "UPDATE world_controls SET world_revision=world_revision+1 WHERE world_instance_id=?", []any{world}},
		{"target-document", "UPDATE memory_aggregates SET document=JSON_SET(document,'$.Revision',999) WHERE world_instance_id=? AND data_subject_id=?", []any{world, session.SubjectID}},
		{"binding", "UPDATE memory_governance SET binding=JSON_SET(binding,'$.Action','CORRECT') WHERE task_id=?", nil},
		{"request-hash", "UPDATE memory_governance SET request_hash=UNHEX(REPEAT('ff',32)) WHERE task_id=?", nil},
		{"task-owner", "UPDATE tasks SET owner_subject_id='s66-service' WHERE task_id=?", nil},
		{"task-next-run", "UPDATE tasks SET next_run_at=next_run_at+INTERVAL 1 SECOND WHERE task_id=?", nil},
		{"registered-operation", "UPDATE registered_jobs SET operation='INSTANCE_CONTROL' WHERE task_id=?", nil},
		{"receipt-key", "UPDATE commit_receipts SET command_key='other-key' WHERE operation='MEMORY_GOVERNANCE_ACCEPT' AND owner_subject_id=?", []any{session.SubjectID}},
	}
	for _, fault := range faults {
		t.Run("afterhook-"+fault.name, func(t *testing.T) {
			key := "s7-final-" + fault.name
			p := plan(key)
			task := integrationTestCreationID("demo-local", "coffee", session.SubjectID, world, "MEMORY_GOVERNANCE", key)
			called := false
			var injectionErr error
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				called = true
				args := fault.args
				if args == nil {
					args = []any{task}
				}
				res, e := tx.ExecContext(ctx, fault.sql, args...)
				if e != nil {
					injectionErr = e
					return e
				}
				n, e := res.RowsAffected()
				if e != nil || n < 1 {
					injectionErr = fmt.Errorf("actual fault rows %d: %v", n, e)
					return injectionErr
				}
				return nil
			}
			receipt, e := r.Store.GovernIntegrationMemory(client, p)
			r.Store.BeforeCommit = nil
			if !called || injectionErr != nil {
				t.Fatal("fault prerequisite", called, injectionErr)
			}
			if e == nil || receipt.TaskID != "" {
				t.Fatal("changed final tuple committed")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks WHERE task_id=?)+(SELECT COUNT(*) FROM memory_governance WHERE task_id=?)+(SELECT COUNT(*) FROM commit_receipts WHERE operation='MEMORY_GOVERNANCE_ACCEPT' AND command_key=?)", task, task, key).Scan(&n); e != nil || n != 0 {
				t.Fatal("accept transaction rollback", n, e)
			}
			if !bytes.Equal(before, snapshot()) {
				t.Fatal("memory document rollback")
			}
			entry, e := r.Local.Ledger.Lookup(context.Background(), task)
			if e != nil || entry.Kind != "REJECTED" {
				t.Fatal("definite rollback must retain original rejection proof", entry.Kind, e)
			}
		})
	}
	var committedTask string
	t.Run("normal-after-faults", func(t *testing.T) {
		out, e := r.Memory.GovernIntegration(client, world, "s7-final-positive", raw)
		if e != nil || out.TaskState != "COMMITTED" {
			t.Fatal("normal final positive", e, out.TaskState)
		}
		committedTask = out.TaskID
	})
	if committedTask == "" {
		t.Fatal("lawful committed task required")
	}
	for _, fault := range []struct{ name, column, value string }{{"preservation-pending", "preservation", "PENDING"}, {"preservation-blocked", "preservation", "BLOCKED_NEEDS_REPLACEMENT"}, {"cleanup-pending", "cleanup", "PENDING"}} {
		t.Run("read-final-"+fault.name, func(t *testing.T) {
			good, e := r.Store.ReadIntegrationGovernanceTask(client, committedTask)
			if e != nil || good.Task.TaskState != "COMMITTED" {
				t.Fatal("real completed task read positive", e)
			}
			called := 0
			var injectionErr error
			r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				called++
				res, e := tx.ExecContext(ctx, "UPDATE memory_governance SET "+fault.column+"=? WHERE task_id=?", fault.value, committedTask)
				if e != nil {
					injectionErr = e
					return e
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					injectionErr = fmt.Errorf("owned read fault rows %d: %v", n, e)
					return injectionErr
				}
				return nil
			}
			out, e := r.Store.ReadIntegrationGovernanceTask(client, committedTask)
			r.Store.BeforeReadFinish = nil
			if called == 0 || injectionErr != nil {
				t.Fatal("actual read-hook fault not reached", called, injectionErr)
			}
			if e == nil || out.Task.TaskID != "" {
				t.Errorf("inconsistent completed governance must deny read, error=%v state=%s", e, out.Task.TaskState)
			}
			var preservation, cleanup string
			if e = r.Store.DB.QueryRow("SELECT preservation,cleanup FROM memory_governance WHERE task_id=?", committedTask).Scan(&preservation, &cleanup); e != nil || preservation != "CONFIRMED" || cleanup != "LOCAL_COMPLETE" {
				t.Fatal("read fault rollback", e, preservation, cleanup)
			}
			good, e = r.Store.ReadIntegrationGovernanceTask(client, committedTask)
			if e != nil || good.Task.TaskState != "COMMITTED" {
				t.Fatal("read locks and ownership released", e)
			}
		})
	}
	t.Run("real-close", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := r.Local.Coordinator.BeginClose(ctx, "s7-final-close"); e != nil {
			t.Fatal(e)
		}
		if _, e := r.Memory.GovernIntegration(client, world, "s7-after-close", raw); e == nil {
			t.Fatal("Close admitted new governance")
		}
	})
}

func TestIntegrationCapabilitiesAndMemoryAdapter(t *testing.T) {
	r, _, _, _, session, world, a := selfMemoryFixture(t, "integration_r0_v1")
	handler := r.Handler()
	request := func(token string) *httptest.ResponseRecorder {
		q := httptest.NewRequest("GET", "/world/v1/integration-capabilities", nil)
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, q)
		return out
	}
	out := request(session.AccessToken)
	if out.Code != 200 {
		t.Fatalf("current CLIENT must reach actual capability owner: %d", out.Code)
	}
	if e := generated.Validate("IntegrationCapabilities", out.Body.Bytes()); e != nil {
		t.Fatal("capability source shape", e)
	}
	var response struct {
		Operations map[string]string `json:"operations"`
		SourceHash string            `json:"source_hash"`
		Profiles   []json.RawMessage `json:"profiles"`
	}
	if e := json.Unmarshal(out.Body.Bytes(), &response); e != nil || len(response.Operations) != 65 || response.SourceHash != generated.SourceHash || len(response.Profiles) != 2 {
		t.Fatal("complete source-owned inventory", e)
	}
	for _, id := range []string{"getIntegrationCapabilities", "getSnapshot", "getEvents", "streamEvents", "readRuntimeMedia", "getMemory", "listMemories", "governMemories", "commandPlayerSelfConsent"} {
		if response.Operations[id] != "AVAILABLE" {
			t.Fatal("actual mounted owner unavailable", id, response.Operations[id])
		}
	}
	if response.Operations["getInstance"] != "UNAVAILABLE" || response.Operations["getUsage"] != "UNAVAILABLE" || response.Operations["createPaymentIntent"] != "PAYMENT_GATE_PENDING" {
		t.Fatal("unsupported/payments misrepresented")
	}
	if bad := request("invalid-bearer"); bad.Code != 401 {
		t.Fatal("capabilities bypassed authentication", bad.Code)
	}
	saved := r.Memory
	r.Memory = nil
	out = request(session.AccessToken)
	r.Memory = saved
	if out.Code != 200 {
		t.Fatal("independent capabilities after owner missing", out.Code)
	}
	if e := json.Unmarshal(out.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"getMemory", "listMemories", "governMemories"} {
		if response.Operations[id] != "UNAVAILABLE" {
			t.Fatal("missing owner reported available", id)
		}
	}
	for id := range a.Nodes {
		single := selfMemoryHTTP(r, session, "integration_r0_v1", "GET", "/world/v1/instances/"+world+"/memories/"+id, "", nil)
		if single.Code != 200 || generated.Validate("PlayerMemoryMetadata", single.Body.Bytes()) != nil {
			t.Fatal("single registered adapter", single.Code)
		}
		break
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := r.Local.Coordinator.BeginClose(ctx, "s7-capabilities-close"); e != nil {
		t.Fatal(e)
	}
	if out = request(session.AccessToken); out.Code == 200 {
		t.Fatal("closed owner still available")
	}
}

// Real owning APIs create the correction and restore a lawful current use.
// No SQL mutation manufactures a lineage, permission or positive-use record.
func verifySelfCorrectionCurrentReads(t *testing.T, profile string) {
	t.Helper()
	r, client, service, input, session, world, a := selfMemoryFixture(t, profile)
	startSelfGovernance(t, r, service, world)
	if _, e := r.Store.ApplyPlayerSelfConsent(client, "s7-lineage-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal("lawful initial", e)
	}
	ids := []string{}
	for id := range a.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	target, unchanged := "", ""
	for _, id := range ids {
		if target == "" && a.Nodes[id].Domain != memory.WorldFact {
			target = id
		} else if unchanged == "" {
			unchanged = id
		}
	}
	if target == "" || unchanged == "" {
		t.Fatal("two real initial nodes, one correctable, required")
	}
	base := "/world/v1/instances/" + world + "/memories"
	for _, id := range []string{target, unchanged} {
		out := selfMemoryHTTP(r, session, profile, "GET", base+"/"+id+"?include=provenance_v1", "", nil)
		if out.Code != 200 || generated.Validate("MemoryProvenanceV1", out.Body.Bytes()) != nil {
			t.Fatal("lawful original provenance positive", out.Code)
		}
	}
	raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","selector":{"kind":"IDS","target_kind":"MEMORY_NODE","ids":[%q],"expected_revisions":{%q:%q}},"replacement":"synthetic current correction body","reason":"current lineage read"}`, target, target, fmt.Sprint(a.Nodes[target].Revision)))
	if e := generated.Validate("MemoryGovernanceCommandV1", raw); e != nil {
		t.Fatal("source-valid correction", e)
	}
	out := selfMemoryHTTP(r, session, profile, "POST", "/world/v1/instances/"+world+"/memory-governance", "s7-lineage-correct", raw)
	var receipt generated.TaskReceipt
	if out.Code != 202 || json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt.TaskState != "COMMITTED" {
		t.Fatal("real correction completion", out.Code, receipt.TaskState)
	}
	entry, e := r.Local.Ledger.Lookup(context.Background(), receipt.TaskID)
	if e != nil || entry.Kind != "APPENDED" || entry.Binding == nil || entry.Binding.Action != "CORRECT" || entry.Binding.ReplacementID == "" {
		t.Fatal("actual correction Ledger", e)
	}
	next, e := r.Store.ExchangeIntegrationSession(service, "s7-lineage-current-session", profile, input)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, next.AccessToken)
	nextCtx, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+next.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(nextCtx, "s7-lineage-renew", json.RawMessage(`{"command":"RENEW","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal("lawful current renewal", e)
	}
	page, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, false)
	if e != nil || !page.BodyReadable {
		t.Fatal("current metadata positive must be reachable", e, page.BodyReadable)
	}
	single := "Memory"
	if profile == "integration_r0_v1" {
		single = "PlayerMemoryMetadata"
	}
	for _, tc := range []struct{ name, id string }{{"unchanged", unchanged}, {"replacement", entry.Binding.ReplacementID}} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := selfMemoryHTTP(r, next, profile, "GET", base+"/"+tc.id, "", nil)
			if metadata.Code != 200 || generated.Validate(single, metadata.Body.Bytes()) != nil {
				t.Fatal("legal current item metadata prerequisite", metadata.Code)
			}
			if profile == "integration_r0_v1" {
				var fields map[string]any
				if json.Unmarshal(metadata.Body.Bytes(), &fields) != nil || fields["body_readable"] != true {
					t.Fatal("actual positive readability claim missing")
				}
			}
			body := selfMemoryHTTP(r, next, profile, "GET", base+"/"+tc.id+"?include=provenance_v1", "", nil)
			if body.Code != 200 || generated.Validate("MemoryProvenanceV1", body.Body.Bytes()) != nil {
				t.Errorf("current corrected graph must retain lawful %s provenance: status=%d", tc.name, body.Code)
			}
		})
	}
	out = selfMemoryHTTP(r, next, profile, "GET", base+"?include=provenance_v1", "", nil)
	if out.Code != 200 || generated.Validate("MemoryProvenancePageV1", out.Body.Bytes()) != nil {
		t.Errorf("current corrected graph provenance page: status=%d", out.Code)
	}
	if t.Failed() {
		return
	}
	snapshot := func() []byte {
		t.Helper()
		var doc []byte
		if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world, next.SubjectID).Scan(&doc); e != nil {
			t.Fatal(e)
		}
		return doc
	}
	original := snapshot()
	// Faults alter only the owned synthetic document and restore its exact
	// original JSON. They do not mint acceptance, Ledger, grants or use.
	for _, name := range []string{"source-body", "source-estimated-time", "source-intake", "replacement-corrects", "replacement-body", "node-kind"} {
		t.Run("current-proof-"+name, func(t *testing.T) {
			var changed memory.Aggregate
			if json.Unmarshal(original, &changed) != nil {
				t.Fatal("original document decode")
			}
			replacement := entry.Binding.ReplacementID
			node := changed.Nodes[replacement]
			sid := node.Sources[0]
			src := changed.Sources[sid]
			switch name {
			case "source-body":
				src.Body += " forged"
			case "source-estimated-time":
				src.EstimatedTime = !src.EstimatedTime
			case "source-intake":
				src.Intake++
			case "replacement-corrects":
				node.Corrects = unchanged
			case "replacement-body":
				node.Body += " forged"
			case "node-kind":
				if node.Kind == "GOAL" {
					node.Kind = "BELIEF"
				} else {
					node.Kind = "GOAL"
				}
			}
			changed.Sources[sid] = src
			changed.Nodes[replacement] = node
			raw, e := json.Marshal(changed)
			if e != nil {
				t.Fatal(e)
			}
			restore := func() {
				t.Helper()
				res, e := r.Store.DB.Exec("UPDATE memory_aggregates SET document=CAST(? AS JSON) WHERE world_instance_id=? AND data_subject_id=?", string(original), world, next.SubjectID)
				if e != nil {
					t.Fatal("restore original fault document", e)
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("restore must change one original row", n, e)
				}
			}
			res, e := r.Store.DB.Exec("UPDATE memory_aggregates SET document=CAST(? AS JSON) WHERE world_instance_id=? AND data_subject_id=?", string(raw), world, next.SubjectID)
			if e != nil {
				t.Fatal("fault prerequisite", e)
			}
			n, e := res.RowsAffected()
			if e != nil || n != 1 {
				t.Fatal("fault row count", n, e)
			}
			func() {
				defer restore()
				metadata, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, false)
				if e != nil || metadata.BodyReadable {
					t.Error("safe metadata must not grant unproved current graph", e, metadata.BodyReadable)
				}
				denied := selfMemoryHTTP(r, next, profile, "GET", base+"/"+replacement+"?include=provenance_v1", "", nil)
				if denied.Code != 403 {
					t.Error("unproved current body escaped", denied.Code)
				}
				if name == "source-body" {
					independent := ""
					for id, n := range changed.Nodes {
						if !n.Blocked && len(n.Sources) == 1 && n.Sources[0] != sid {
							independent = id
							break
						}
					}
					if independent == "" {
						t.Fatal("real independent initial source prerequisite")
					}
					one, e := r.Memory.ListIntegrationMetadata(nextCtx, world, independent, "", profile, 50, false)
					if e != nil || !one.BodyReadable || len(one.Nodes) != 1 {
						t.Error("separately proven item metadata lost body qualification", e, one.BodyReadable)
					}
					allowed := selfMemoryHTTP(r, next, profile, "GET", base+"/"+independent+"?include=provenance_v1", "", nil)
					if allowed.Code != 200 || generated.Validate("MemoryProvenanceV1", allowed.Body.Bytes()) != nil {
						t.Error("independent real item body lost", allowed.Code)
					}
					whole := selfMemoryHTTP(r, next, profile, "GET", base+"?include=provenance_v1", "", nil)
					if whole.Code != 403 {
						t.Error("whole page borrowed independent proof", whole.Code)
					}
				}
			}()
			if !bytes.Equal(original, snapshot()) {
				t.Fatal("original raw restore differs")
			}
			restored := selfMemoryHTTP(r, next, profile, "GET", base+"/"+replacement+"?include=provenance_v1", "", nil)
			if restored.Code != 200 {
				t.Fatal("restored proof remains readable", restored.Code)
			}
		})
	}

	t.Run("selection-only-restricts-and-empty-does-not-grant", func(t *testing.T) {
		read, e := r.Store.BeginIntegrationMemoryMetadata(nextCtx, world)
		if e != nil {
			t.Fatal(e)
		}
		defer read.Close()
		copy := *read
		if copy.SelectItems([]string{unchanged}) == nil {
			t.Fatal("copied read acquired restriction capability")
		}
		copy.Close()
		if e = read.SelectItems(nil); e != nil {
			t.Fatal(e)
		}
		if read.SelectItems([]string{unchanged}) == nil {
			t.Fatal("selection expanded after sealing")
		}
		if e = read.Finish(nextCtx); e != nil {
			t.Fatal(e)
		}
		if read.FinalBodyReadable() {
			t.Fatal("empty set upgraded body authority")
		}
	})
	t.Run("p0-false-cannot-upgrade-at-finish", func(t *testing.T) {
		update := func(enabled int) {
			t.Helper()
			res, e := r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=? WHERE data_subject_id=? AND action='memory.private_body.read'", enabled, next.SubjectID)
			if e != nil {
				t.Fatal(e)
			}
			n, e := res.RowsAffected()
			if e != nil || n != 1 {
				t.Fatal("permission fault row count", n, e)
			}
		}
		update(0)
		defer update(1)
		called := false
		var injected error
		r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
			called = true
			res, e := tx.ExecContext(ctx, "UPDATE memory_subject_permissions SET enabled=1 WHERE data_subject_id=? AND action='memory.private_body.read'", next.SubjectID)
			if e != nil {
				injected = e
				return e
			}
			n, e := res.RowsAffected()
			if e != nil || n != 1 {
				injected = fmt.Errorf("P0 fault row count %d %v", n, e)
				return injected
			}
			return nil
		}
		page, e := r.Memory.ListIntegrationMetadata(nextCtx, world, unchanged, "", profile, 50, false)
		r.Store.BeforeReadFinish = nil
		if !called || injected != nil {
			t.Fatal("actual P0/current hook", called, injected)
		}
		if e != nil || page.BodyReadable {
			t.Error("false P0 upgraded after hook", e, page.BodyReadable)
		}
	})
	for _, mode := range []string{"body-permission-disabled", "body-permission-expired", "source-raw-final"} {
		t.Run("read-finish-"+mode, func(t *testing.T) {
			for _, body := range []bool{false, true} {
				called := false
				var injected error
				r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					called = true
					var res sql.Result
					var e error
					if mode == "source-raw-final" {
						var changed memory.Aggregate
						if json.Unmarshal(original, &changed) != nil {
							return fmt.Errorf("original decode")
						}
						sid := changed.Nodes[entry.Binding.ReplacementID].Sources[0]
						v := changed.Sources[sid]
						v.Body += " hook"
						changed.Sources[sid] = v
						raw, e := json.Marshal(changed)
						if e != nil {
							return e
						}
						res, e = tx.ExecContext(ctx, "UPDATE memory_aggregates SET document=CAST(? AS JSON) WHERE world_instance_id=? AND data_subject_id=?", string(raw), world, next.SubjectID)
						injected = e
					} else {
						q := "UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.private_body.read' AND data_subject_id=?"
						if mode == "body-permission-expired" {
							q = "UPDATE memory_subject_permissions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE action='memory.private_body.read' AND data_subject_id=?"
						}
						res, e = tx.ExecContext(ctx, q, next.SubjectID)
						injected = e
					}
					if injected != nil {
						return injected
					}
					n, e := res.RowsAffected()
					if e != nil || n != 1 {
						injected = fmt.Errorf("hook row count %d %v", n, e)
						return injected
					}
					return nil
				}
				page, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, body)
				r.Store.BeforeReadFinish = nil
				if !called || injected != nil {
					t.Fatal("actual final hook prerequisite", called, injected)
				}
				if body || mode == "source-raw-final" {
					if e == nil {
						t.Error("changed final body/current proof returned a page")
					}
				} else if e != nil || page.BodyReadable {
					t.Error("metadata must remain safe and downgrade only its body eligibility", e, page.BodyReadable)
				}
				if !bytes.Equal(original, snapshot()) {
					t.Fatal("read final fault did not rollback")
				}
				good, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, true)
				if e != nil || !good.BodyReadable {
					t.Fatal("current positive after final rollback", e)
				}
			}
		})
	}

	t.Run("accept-original-command-key", func(t *testing.T) {
		commit := integrationTestCreationID(receipt.TaskID, "accept")
		var oldKey string
		if e := r.Store.DB.QueryRow("SELECT command_key FROM commit_receipts WHERE commit_key=?", commit).Scan(&oldKey); e != nil {
			t.Fatal(e)
		}
		for _, key := range []string{"different-valid-original-key", "invalid key"} {
			func() {
				res, e := r.Store.DB.Exec("UPDATE commit_receipts SET command_key=? WHERE commit_key=?", key, commit)
				if e != nil {
					t.Fatal("actual receipt fault prerequisite", e)
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("actual receipt fault count", n, e)
				}
				defer func() {
					res, e := r.Store.DB.Exec("UPDATE commit_receipts SET command_key=? WHERE commit_key=?", oldKey, commit)
					if e != nil {
						t.Fatal(e)
					}
					n, e := res.RowsAffected()
					if e != nil || n != 1 {
						t.Fatal("restore original receipt key", n, e)
					}
				}()
				page, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, false)
				if e != nil || page.BodyReadable {
					t.Error("wrong original acceptance key must leave safe metadata without body authority", e, page.BodyReadable)
				}
				bad := selfMemoryHTTP(r, next, profile, "GET", base+"/"+entry.Binding.ReplacementID+"?include=provenance_v1", "", nil)
				if bad.Code != 403 {
					t.Error("wrong original acceptance command key admitted body", bad.Code)
				}
			}()
		}
		good := selfMemoryHTTP(r, next, profile, "GET", base+"/"+entry.Binding.ReplacementID+"?include=provenance_v1", "", nil)
		if good.Code != 200 {
			t.Fatal("restored exact acceptance key", good.Code)
		}
	})
	for _, column := range []string{"aggregate_id", "revision"} {
		t.Run("final-aggregate-"+column, func(t *testing.T) {
			var id string
			var revision uint64
			if e := r.Store.DB.QueryRow("SELECT aggregate_id,revision FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world, next.SubjectID).Scan(&id, &revision); e != nil {
				t.Fatal(e)
			}
			for _, body := range []bool{false, true} {
				called := false
				var injected error
				r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					called = true
					q := "UPDATE memory_aggregates SET revision=revision+1 WHERE world_instance_id=? AND data_subject_id=?"
					if column == "aggregate_id" {
						q = "UPDATE memory_aggregates SET aggregate_id='owned-wrong-aggregate' WHERE world_instance_id=? AND data_subject_id=?"
					}
					res, e := tx.ExecContext(ctx, q, world, next.SubjectID)
					if e != nil {
						injected = e
						return e
					}
					n, e := res.RowsAffected()
					if e != nil || n != 1 {
						injected = fmt.Errorf("owned aggregate hook row count %d %v", n, e)
						return injected
					}
					return nil
				}
				_, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, body)
				r.Store.BeforeReadFinish = nil
				if !called || injected != nil {
					t.Fatal("aggregate hook prerequisite", called, injected)
				}
				if e == nil {
					t.Error("changed aggregate identity/revision escaped final read")
				}
				var afterID string
				var afterRevision uint64
				if e := r.Store.DB.QueryRow("SELECT aggregate_id,revision FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", world, next.SubjectID).Scan(&afterID, &afterRevision); e != nil || afterID != id || afterRevision != revision {
					t.Fatal("aggregate fault rollback", e, afterID, afterRevision)
				}
				good, e := r.Memory.ListIntegrationMetadata(nextCtx, world, "", "", profile, 50, true)
				if e != nil || !good.BodyReadable {
					t.Fatal("positive after aggregate rollback", e)
				}
			}
		})
	}
	t.Run("two-corrections-and-late-old-target-delete", func(t *testing.T) {
		second := entry.Binding.ReplacementID
		command := []byte(fmt.Sprintf(`{"operation":"CORRECT","memory_ids":[%q],"expected_revisions":{%q:"1"},"replacement":"second real correction","reason":"chain"} `, second, second))
		if e := generated.Validate("MemoryGovernanceCommandV1", command); e != nil {
			t.Fatal("chain wire prerequisite", e)
		}
		accepted, e := r.Memory.GovernIntegration(nextCtx, world, "s7-chain-second", command)
		if e != nil || accepted.TaskState != "COMMITTED" {
			t.Fatal("second original correction", e)
		}
		record, e := r.Local.Ledger.Lookup(context.Background(), accepted.TaskID)
		if e != nil || record.Binding == nil {
			t.Fatal("second actual preservation", e)
		}
		if _, e = r.Store.ApplyPlayerSelfConsent(nextCtx, "s7-chain-renew", json.RawMessage(`{"command":"RENEW","expected_revision":"2","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal(e)
		}
		for _, id := range []string{unchanged, record.Binding.ReplacementID} {
			out := selfMemoryHTTP(r, next, profile, "GET", base+"/"+id+"?include=provenance_v1", "", nil)
			if out.Code != 200 {
				t.Fatal("cleared intermediate must not require lost plaintext", out.Code)
			}
		}
		oldDelete := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:"2"},"reason":"old target only"}`, target, target))
		if _, e = r.Memory.GovernIntegration(nextCtx, world, "s7-chain-old-delete", oldDelete); e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.ApplyPlayerSelfConsent(nextCtx, "s7-chain-renew-last", json.RawMessage(`{"command":"RENEW","expected_revision":"3","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal(e)
		}
		out := selfMemoryHTTP(r, next, profile, "GET", base+"/"+record.Binding.ReplacementID+"?include=provenance_v1", "", nil)
		if out.Code != 200 {
			t.Fatal("old target delete must not invent descendant block", out.Code)
		}
	})
}

func integrationBirthFixture(t *testing.T, profile string) integrationFixFixture {
	t.Helper()
	actions := []string{"READ", "TURN", "MEMORY_VIEW", "MEMORY_GOVERN", "CONTROL_PAUSE", "CONTROL_RESUME"}
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, actions)
	world, e := r.World.CreateIntegrationInstance(service, "birth-world", request)
	if e != nil {
		t.Fatal(e)
	}
	session, e := r.Store.ExchangeIntegrationSession(service, "birth-session", profile, generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: actions})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "birth-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	f := integrationFixFixture{r: r, service: service, client: client, world: world.Scope}
	f.control(t, service, "START", "0", "birth-start")
	var wr, cv uint64
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),w.world_revision,w.control_version FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", world.Scope.WorldInstanceID).Scan(&f.actor, &wr, &cv); e != nil {
		t.Fatal(e)
	}
	var itemRaw []byte
	var item struct{ CustodianID string }
	if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? LIMIT 1", world.Scope.WorldInstanceID).Scan(&itemRaw); e != nil || json.Unmarshal(itemRaw, &item) != nil || item.CustodianID == "" {
		t.Fatal("real original item custodian prerequisite", e)
	}
	f.actor = item.CustodianID
	f.input = generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: fmt.Sprint(wr), ControlVersion: fmt.Sprint(cv)}, PrimaryActorID: f.actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	return f
}

// FIX2 T1: actual installed intake/Mock producers, never fabricated SQL birth
// records. These complete targets begin with qualified pre-TURN body access.
func TestIntegrationTurnInputMemoryBirthProof(t *testing.T) {
	t.Run("actual-input-codec-capacity", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		empty := []byte(`{"kind":"TEXT","text":""}`)
		text := strings.Repeat("界", store.MockInputLimit-utf8.RuneCount(empty))
		input, e := json.Marshal(map[string]string{"kind": "TEXT", "text": text})
		if e != nil || utf8.RuneCount(input) != store.MockInputLimit || len(input) <= store.MockInputLimit {
			t.Fatal("legal Unicode capacity shape", e)
		}
		q, e := f.quote(f.service, "capacity-quote")
		if e != nil {
			t.Fatal(e)
		}
		request := f.input
		request.QuoteID = q.QuoteID
		request.Input = input
		wire, e := json.Marshal(request)
		if e != nil {
			t.Fatal(e)
		}
		var checked generated.TurnRequest
		if e = generated.Decode("TurnRequest", wire, &checked); e != nil {
			t.Fatal("capacity actual source validator", e)
		}
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "capacity-input", request)
		if e != nil {
			t.Fatal("legal maximum quoted INPUT", e)
		}
		proof := assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
		page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 100, true)
		if e != nil || !page.BodyReadable {
			t.Fatal("legal capacity current proof", e)
		}
		found := false
		for _, n := range page.Nodes {
			if n.Body == string(input) {
				found = true
			}
		}
		if !found {
			t.Fatal("capacity body exact original bytes")
		}
		over, e := json.Marshal(map[string]string{"kind": "TEXT", "text": text + "界"})
		if e != nil || utf8.RuneCount(over) != store.MockInputLimit+1 {
			t.Fatal(e)
		}
		q, e = f.quote(f.service, "capacity-boundary-quote")
		if e != nil {
			t.Fatal(e)
		}
		request.QuoteID = q.QuoteID
		request.Input = over
		wire, e = json.Marshal(request)
		if e != nil || generated.Decode("TurnRequest", wire, &checked) != nil {
			t.Fatal("boundary remains schema-valid", e)
		}
		var before, after int
		if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&before); e != nil {
			t.Fatal(e)
		}
		if _, e = f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "capacity-over", request); e == nil {
			t.Fatal("quoted metering boundary accepted")
		}
		if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&after); e != nil || before != after {
			t.Fatal("boundary wrote intake", e)
		}
		t.Logf("actual INPUT capacity canonical_runes=%d raw_bytes=%d source_members=%d node_members=%d boundary_runes=%d zero_new_intake=true", utf8.RuneCount(input), len(input), len(proof.SourcesBorn), len(proof.NodesBorn), utf8.RuneCount(over))
	})
	for _, profile := range []string{"public_v1", "integration_r0_v1"} {
		t.Run(profile, func(t *testing.T) {
			f := integrationBirthFixture(t, profile)
			before, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 50, true)
			if e != nil || len(before.Nodes) == 0 {
				t.Fatal("actual current initial body prerequisite", e)
			}
			q, e := f.quote(f.service, "birth-input-quote")
			if e != nil {
				t.Fatal(e)
			}
			request := f.input
			request.QuoteID = q.QuoteID
			accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "birth-input", request)
			if e != nil {
				t.Fatal("real input acceptance", e)
			}
			var source string
			var count int
			if e = f.r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", accepted.TurnID).Scan(&source); e != nil || source == "" {
				t.Fatal("actual persisted input", e)
			}
			if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns h JOIN commit_receipts r ON r.commit_key=h.accepted_commit_key WHERE h.task_id=? AND r.operation='HARNESS_INTAKE' AND r.phase='COMMIT'", accepted.TurnID).Scan(&count); e != nil || count != 1 {
				t.Fatal("original acceptance COMMIT", count, e)
			}
			assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
			metadata, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 50, false)
			if e != nil || len(metadata.Nodes) <= len(before.Nodes) {
				t.Fatal("actual input must remain in metadata membership", e)
			}
			if !metadata.BodyReadable {
				t.Error("causal RED: actual accepted TURN input removes authorized body qualification")
			}
			body, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 50, true)
			if e != nil {
				t.Error("causal RED: actual accepted TURN input provenance denied", e)
			} else if len(body.Nodes) != len(metadata.Nodes) {
				t.Error("body hides producer members")
			}
		})
	}
}

func TestIntegrationTurnDerivedMemoryBirthAndGovernance(t *testing.T) {

	t.Run("ten-independent-input-correct-components", func(t *testing.T) {
		for _, profile := range []string{"integration_r0_v1", "public_v1"} {
			t.Run(profile, func(t *testing.T) {
				f := integrationBirthFixture(t, profile)
				initialAggregate := integrationBirthAggregateRead(t, f)
				initial := ""
				for id := range initialAggregate.Nodes {
					initial = id
					break
				}
				if initial == "" {
					t.Fatal("initial node prerequisite")
				}
				targets := []string{}
				for i := 0; i < 10; i++ {
					key := fmt.Sprintf("distinct-correct-input-%d", i)
					q, e := f.quote(f.service, key+"-quote")
					if e != nil {
						t.Fatal(e)
					}
					req := f.input
					req.QuoteID = q.QuoteID
					v, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, key, req)
					if e != nil {
						t.Fatal(e)
					}
					proof := assertIntegrationBirthRecord(t, f, v.TurnID, "INPUT")
					targets = append(targets, proof.MemoryIDs[0])
				}
				if page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 100, true); e != nil || !page.BodyReadable {
					t.Fatal("real owner/List/body prerequisite after ten INPUTs", e)
				}
				replacements := []string{}
				for i, id := range targets {
					raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","memory_ids":[%q],"expected_revisions":{%q:"1"},"replacement":%q,"reason":"independent input correction"}`, id, id, fmt.Sprintf("real corrected input %d", i)))
					receipt, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("distinct-correct-%d", i), raw)
					if e != nil || receipt.TaskState != "COMMITTED" {
						t.Fatal("real distinct CORRECT/Clean", i, e)
					}
					entry, e := f.r.Local.Ledger.Lookup(context.Background(), receipt.TaskID)
					if e != nil || entry.Binding == nil || entry.Binding.ReplacementID == "" {
						t.Fatal("actual immutable correction binding", e)
					}
					replacements = append(replacements, entry.Binding.ReplacementID)
					cleaned := integrationBirthAggregateRead(t, f)
					if cleaned.Nodes[id].Body != "" || !cleaned.Nodes[id].Blocked {
						t.Fatal("real cleanup not completed")
					}
					if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("distinct-correct-renew-%d", i), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+1)))); e != nil {
						t.Fatal(e)
					}
				}
				if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, initial); e != nil || n.Body == "" {
					t.Fatal("unchanged initial body after ten independent real CORRECTs", e)
				}
				for i, id := range replacements {
					if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, id); e != nil || n.Body != fmt.Sprintf("real corrected input %d", i) {
						t.Fatal("real replacement body", i, e)
					}
				}
				if page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 100, true); e != nil || !page.BodyReadable {
					t.Fatal("complete current profile body qualification", e)
				}
				_, session := integrationBirthExchange(t, f, "distinct-correct-http-"+profile, profile)
				currentAggregate := integrationBirthAggregateRead(t, f)
				for _, id := range append([]string{initial}, replacements...) {
					out := selfMemoryHTTP(f.r, session, profile, "GET", "/world/v1/instances/"+f.world.WorldInstanceID+"/memories/"+id+"?include=provenance_v1", "", nil)
					var proof generated.MemoryProvenanceV1
					if out.Code != 200 || generated.Validate("MemoryProvenanceV1", out.Body.Bytes()) != nil || json.Unmarshal(out.Body.Bytes(), &proof) != nil {
						t.Fatal("actual HTTP provenance after ten independent CORRECTs", profile, id, out.Code)
					}
					node := currentAggregate.Nodes[id]
					if proof.Memory.MemoryID != id || proof.Memory.Content != node.Body || len(proof.Memory.Sources) != len(node.Sources) || len(node.Sources) == 0 {
						t.Fatal("actual provenance member/body/source set")
					}
					expected := map[string]bool{}
					for _, sid := range node.Sources {
						expected[sid] = true
					}
					for _, source := range proof.Memory.Sources {
						real, exists := currentAggregate.Sources[source.SourceID]
						if !exists || !expected[source.SourceID] || source.SourceRevision != fmt.Sprint(real.Revision) || source.AcquiredByActorID != node.ActorID {
							t.Fatal("HTTP provenance did not preserve original source facts")
						}
						delete(expected, source.SourceID)
					}
					if len(expected) != 0 {
						t.Fatal("HTTP provenance omitted original source")
					}
				}

			})
		}
	})

	t.Run("completed-correct-chain-independent-old-replacements", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		a := integrationBirthAggregateRead(t, f)
		target := ""
		for id, n := range a.Nodes {
			if n.Domain != memory.WorldFact {
				target = id
				break
			}
		}
		if target == "" {
			t.Fatal("real corrigible initial node")
		}
		if page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 100, true); e != nil || !page.BodyReadable {
			t.Fatal("original owner/current body prerequisite", e)
		}
		old := []string{}
		for i := 0; i < 10; i++ {
			old = append(old, target)
			a = integrationBirthAggregateRead(t, f)
			raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","memory_ids":[%q],"expected_revisions":{%q:%q},"replacement":%q,"reason":"real completed correction chain"}`, target, target, fmt.Sprint(a.Nodes[target].Revision), fmt.Sprintf("replacement %d", i)))
			receipt, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("closed-correct-%d", i), raw)
			if e != nil || receipt.TaskState != "COMMITTED" {
				t.Fatal("real CORRECT/Clean", i, e)
			}
			entry, e := f.r.Local.Ledger.Lookup(context.Background(), receipt.TaskID)
			if e != nil || entry.Binding == nil || entry.Binding.ReplacementID == "" {
				t.Fatal("original correction Ledger", e)
			}
			cleaned := integrationBirthAggregateRead(t, f)
			if cleaned.Nodes[target].Body != "" || !cleaned.Nodes[target].Blocked {
				t.Fatal("original cleanup incomplete")
			}
			target = entry.Binding.ReplacementID
			if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("closed-correct-renew-%d", i), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+1)))); e != nil {
				t.Fatal(e)
			}
		}
		q, e := f.quote(f.service, "after-closed-chain-quote")
		if e != nil {
			t.Fatal(e)
		}
		req := f.input
		req.QuoteID = q.QuoteID
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "after-closed-chain", req)
		if e != nil {
			t.Fatal(e)
		}
		proof := assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, proof.MemoryIDs[0]); e != nil || n.Body == "" {
			t.Fatal("qualified completed CORRECT chain and INPUT", e)
		}
		for i, id := range old {
			a = integrationBirthAggregateRead(t, f)
			raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"old replacement cleanup"}`, id, id, fmt.Sprint(a.Nodes[id].Revision)))
			receipt, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("old-replacement-delete-%d", i), raw)
			if e != nil || receipt.TaskState != "COMMITTED" {
				t.Fatal("actual different-node DELETE/Clean", i, e)
			}
			if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("old-replacement-renew-%d", i), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+11)))); e != nil {
				t.Fatal(e)
			}
		}
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, proof.MemoryIDs[0]); e != nil || n.Body == "" {
			t.Fatal("independent B after actual same-source old replacement deletes", e)
		}
	})
	t.Run("correct-clean-input-ten-old-deletes", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		a := integrationBirthAggregateRead(t, f)
		target := ""
		for id, n := range a.Nodes {
			if n.Domain != memory.WorldFact {
				target = id
				break
			}
		}
		if target == "" {
			t.Fatal("real corrigible initial node")
		}
		if page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 100, true); e != nil || !page.BodyReadable {
			t.Fatal("original installed owner/current pre-TURN body prerequisite", e)
		}
		raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","memory_ids":[%q],"expected_revisions":{%q:%q},"replacement":"original correction before new input","reason":"mixed historical chain"}`, target, target, fmt.Sprint(a.Nodes[target].Revision)))
		corrected, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, "mixed-correct", raw)
		if e != nil || corrected.TaskState != "COMMITTED" {
			t.Fatal("actual prefix CORRECT/Clean", e)
		}
		if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "mixed-renew-0", json.RawMessage(`{"command":"RENEW","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
			t.Fatal(e)
		}
		q, e := f.quote(f.service, "mixed-input-quote")
		if e != nil {
			t.Fatal(e)
		}
		req := f.input
		req.QuoteID = q.QuoteID
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "mixed-input", req)
		if e != nil {
			t.Fatal(e)
		}
		proof := assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, proof.MemoryIDs[0]); e != nil || n.Body == "" {
			t.Fatal("qualified mixed prefix before ten deletes", e)
		}
		for i := 0; i < 10; i++ {
			a = integrationBirthAggregateRead(t, f)
			raw = []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"old corrected target"}`, target, target, fmt.Sprint(a.Nodes[target].Revision)))
			r, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("mixed-delete-%d", i), raw)
			if e != nil || r.TaskState != "COMMITTED" {
				t.Fatal("real DELETE/Clean", i, e)
			}
			if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("mixed-renew-%d", i+1), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+2)))); e != nil {
				t.Fatal(e)
			}
		}
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, proof.MemoryIDs[0]); e != nil || n.Body == "" {
			t.Fatal("independent INPUT after completed CORRECT and ten DELETE/Clean must remain readable", e)
		}
	})
	t.Run("ten-independent-input-delete-accepts", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		a := integrationBirthAggregateRead(t, f)
		initial := ""
		for id := range a.Nodes {
			initial = id
			break
		}
		if initial == "" {
			t.Fatal("initial node")
		}
		targets := []string{}
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("independent-input-%d", i)
			q, e := f.quote(f.service, key+"-quote")
			if e != nil {
				t.Fatal(e)
			}
			req := f.input
			req.QuoteID = q.QuoteID
			v, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, key, req)
			if e != nil {
				t.Fatal(e)
			}
			proof := assertIntegrationBirthRecord(t, f, v.TurnID, "INPUT")
			targets = append(targets, proof.MemoryIDs[0])
		}
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, initial); e != nil || n.Body == "" {
			t.Fatal("qualified initial node after ten real INPUTs", e)
		}
		for i, id := range targets {
			raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:"1"},"reason":"independent original input"}`, id, id))
			r, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("independent-delete-%d", i), raw)
			if e != nil || r.TaskState != "COMMITTED" {
				t.Fatal("real distinct DELETE/Clean", i, e)
			}
			if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("independent-renew-%d", i), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+1)))); e != nil {
				t.Fatal(e)
			}
		}
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, initial); e != nil || n.Body == "" {
			t.Fatal("undeleted initial node after ten independent real accepts must remain readable", e)
		}
	})
	t.Run("repeated-real-delete-keeps-unrelated-input", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		initial := integrationBirthAggregateRead(t, f)
		target := ""
		for id, n := range initial.Nodes {
			if n.Domain != memory.WorldFact {
				target = id
				break
			}
		}
		if target == "" {
			t.Fatal("initial corrigible member")
		}
		q, e := f.quote(f.service, "repeat-input-quote")
		if e != nil {
			t.Fatal(e)
		}
		req := f.input
		req.QuoteID = q.QuoteID
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "repeat-input", req)
		if e != nil {
			t.Fatal(e)
		}
		proof := assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
		if _, e = f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 100, true); e != nil {
			t.Fatal("real initial/input proof before repeated deletion", e)
		}
		for i := 0; i < 10; i++ {
			a := integrationBirthAggregateRead(t, f)
			raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"repeated real deletion"}`, target, target, fmt.Sprint(a.Nodes[target].Revision)))
			r, e := f.r.Memory.GovernIntegration(f.client, f.world.WorldInstanceID, fmt.Sprintf("repeat-delete-%d", i), raw)
			if e != nil || r.TaskState != "COMMITTED" {
				t.Fatal("real repeated DELETE", i, e, r.TaskState)
			}
			if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, fmt.Sprintf("repeat-renew-%d", i), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(i+1)))); e != nil {
				t.Fatal(e)
			}
		}
		if _, n, _, e := f.r.Memory.ReadIntegrationMemory(f.client, f.world.WorldInstanceID, proof.MemoryIDs[0]); e != nil || n.Body == "" {
			t.Fatal("unrelated real INPUT after ten lawful DELETE/Clean", e)
		}
	})
	t.Run("ten-real-inputs-then-empty-Mock-batches", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		tasks := []string{}
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("empty-chain-%d", i)
			q, e := f.quote(f.service, key+"-quote")
			if e != nil {
				t.Fatal(e)
			}
			req := f.input
			req.QuoteID = q.QuoteID
			v, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, key, req)
			if e != nil {
				t.Fatal("actual INPUT", i, e)
			}
			tasks = append(tasks, v.TurnID)
		}
		var row uint64
		if e := f.r.Store.DB.QueryRow("SELECT revision FROM memory_aggregates WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&row); e != nil {
			t.Fatal(e)
		}
		for i, task := range tasks {
			worker := fmt.Sprintf("empty-chain-worker-%d", i)
			lease, e := f.r.Store.ClaimRegistered(context.Background(), worker, time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: task}})
			if e != nil {
				t.Fatal(e)
			}
			if e = f.r.Worker.Turns.Execute(context.Background(), lease.TaskLease, worker); e != nil {
				t.Fatal("actual empty Mock worker", i, e)
			}
			proof := assertIntegrationBirthRecord(t, f, task, "DERIVED")
			if len(proof.MemoryIDs) != 0 || proof.BeforeAggregateRevision != row || proof.AfterAggregateRevision != row {
				t.Fatal("real empty batch must retain the same memory revision", i)
			}
		}
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 100, true); e != nil || !p.BodyReadable || len(p.Nodes) < 10 {
			t.Fatal("ten actual empty batches are equivalent and complete", e)
		}
	})
	for _, kind := range []string{"initial", "input", "derived"} {
		t.Run("correct-chain-"+kind, func(t *testing.T) { verifyIntegrationBirthGovernance(t, kind) })
	}
	for _, tc := range []struct {
		name, text string
		nonempty   bool
	}{{"nonempty", "Pass the sign and remember my request.", true}, {"empty", "May I borrow the sign?", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := integrationBirthFixture(t, "integration_r0_v1")
			before, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true)
			if e != nil || len(before.Nodes) == 0 {
				t.Fatal("actual current initial body prerequisite", e)
			}
			q, e := f.quote(f.service, "birth-derived-quote")
			if e != nil {
				t.Fatal(e)
			}
			request := f.input
			request.QuoteID = q.QuoteID
			request.Input = json.RawMessage(fmt.Sprintf(`{"kind":"TEXT","text":%q}`, tc.text))
			accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "birth-derived", request)
			if e != nil {
				t.Fatal("actual input acceptance", e)
			}
			lease, e := f.r.Store.ClaimRegistered(context.Background(), "birth-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			if e = f.r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "birth-worker"); e != nil {
				t.Fatal("real Mock/approval/commit prerequisite", e)
			}
			var doc []byte
			if e = f.r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", accepted.TurnID).Scan(&doc); e != nil {
				t.Fatal(e)
			}
			var committed domain.CandidateCommit
			if e = json.Unmarshal(doc, &committed); e != nil {
				t.Fatal(e)
			}
			if tc.nonempty && len(committed.MemoryIDs) == 0 || !tc.nonempty && len(committed.MemoryIDs) != 0 {
				t.Fatal("actual Mock branch must produce the declared nonempty/empty batch", len(committed.MemoryIDs))
			}
			var aggregateRaw []byte
			if e = f.r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&aggregateRaw); e != nil {
				t.Fatal(e)
			}
			var aggregate memory.Aggregate
			if e = json.Unmarshal(aggregateRaw, &aggregate); e != nil {
				t.Fatal(e)
			}
			for _, id := range committed.MemoryIDs {
				node, ok := aggregate.Nodes[id]
				if !ok || node.Domain != memory.ActorBelief || len(node.Parents) != 1 || len(node.Sources) != 1 || node.Body == "" {
					t.Fatal("real parent-linked derived birth prerequisite")
				}
			}
			assertIntegrationBirthRecord(t, f, accepted.TurnID, "INPUT")
			proof := assertIntegrationBirthRecord(t, f, accepted.TurnID, "DERIVED")
			if len(proof.MemoryIDs) != len(committed.MemoryIDs) {
				t.Fatal("complete derived IDs")
			}
			for _, profile := range []string{"integration_r0_v1"} {
				metadata, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 50, false)
				if e != nil || len(metadata.Nodes) != len(aggregate.Nodes) {
					t.Fatal("metadata must retain complete actual producer membership", e)
				}
				if !metadata.BodyReadable {
					t.Error("causal RED: committed actual Mock batch removes authorized body qualification", profile)
				}
				body, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", profile, 50, true)
				if e != nil {
					t.Error("causal RED: committed actual Mock batch provenance denied", profile, e)
				} else if len(body.Nodes) != len(aggregate.Nodes) {
					t.Error("committed producer members hidden", profile)
				}
			}
		})
	}
}

// This envelope is an upper bound over the actual fixed Mock producer domain,
// not a valid witness and never passed to an authority writer. domain.ValidateID
// bounds identities to 64 JSON-safe ASCII bytes; ValidateIdempotencyKey bounds
// Original.Command and Metadata.Identity.RequestKey to 128. Other strings are
// fixed closed enums, SHA256 hex, the 32-hex Files key, or the 64-hex source epoch.
// No private plaintext or external Unicode identity is a document field.
// Giving every optional field its longest representation (including two metadata
// values, four uses, maximum integers and time offsets) conservatively covers all
// phases. Nil MediaRefs serializes as null, longer than the permitted empty [].
func integrationMockBirthEnvelope() store.IntegrationMemoryBirthDocument {
	var d store.IntegrationMemoryBirthDocument
	var fill func(reflect.Value, string)
	fill = func(v reflect.Value, name string) {
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("maximum-offset", 23*3600+59*60))))
			return
		}
		switch v.Kind() {
		case reflect.String:
			n := 64
			if name == "Command" || name == "RequestKey" {
				n = 128
			}
			v.SetString(strings.Repeat("x", n))
		case reflect.Uint64:
			v.SetUint(^uint64(0))
		case reflect.Int64:
			v.SetInt(-9223372036854775808)
		case reflect.Bool:
			v.SetBool(false)
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
			fill(v.Elem(), name)
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				fill(v.Field(i), v.Type().Field(i).Name)
			}
		case reflect.Slice:
			if name == "MediaRefs" {
				return
			}
			n := 1
			if name == "Uses" || name == "Disabled" {
				n = 4
			}
			v.Set(reflect.MakeSlice(v.Type(), n, n))
			for i := 0; i < n; i++ {
				fill(v.Index(i), name)
			}
		default:
			panic("unaccounted birth codec field " + v.Type().String())
		}
	}
	fill(reflect.ValueOf(&d).Elem(), "")
	return d
}
func assertIntegrationMockBirthCapacity(t *testing.T, d store.IntegrationMemoryBirthDocument, raw []byte) {
	t.Helper()
	max := integrationMockBirthEnvelope()
	limit, e := json.Marshal(max)
	if e != nil {
		t.Fatal(e)
	}
	// A successful INPUT adds a new retained Source. Clean never removes sources.
	// MaxSources=1000, each task has one accepted identity and at most one committed
	// DERIVED identity; unique original keys and the complete Store task enumeration
	// prohibit extra documents. 2000 is conservative even before initial sources.
	if len(limit) != 12349 || uint64(len(limit))*2*uint64(memory.MaxSources) >= 32<<20 || len(limit) >= 16<<20 {
		t.Fatal("explicit admitted-set byte envelope changed", len(limit))
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
	var check func(reflect.Value, reflect.Value, string)
	check = func(v, bound reflect.Value, path string) {
		if v.Type() == reflect.TypeOf(time.Time{}) {
			encoded, err := json.Marshal(v.Interface())
			upper, _ := json.Marshal(bound.Interface())
			if err != nil || len(encoded) > len(upper) {
				t.Fatal("time encoding outside admitted envelope", path)
			}
			return
		}
		switch v.Kind() {
		case reflect.String:
			if len(v.String()) > len(bound.String()) || !safe.MatchString(v.String()) {
				t.Fatal("string length/escaping outside fixed producer domain", path)
			}
		case reflect.Pointer:
			if !v.IsNil() {
				check(v.Elem(), bound.Elem(), path)
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				check(v.Field(i), bound.Field(i), path+"."+v.Type().Field(i).Name)
			}
		case reflect.Slice:
			if v.Len() > bound.Len() {
				t.Fatal("collection outside fixed Mock cardinality", path, v.Len(), bound.Len())
			}
			for i := 0; i < v.Len(); i++ {
				check(v.Index(i), bound.Index(i), path)
			}
		case reflect.Uint64, reflect.Int64, reflect.Bool:
		default:
			t.Fatal("unaccounted actual codec type", path, v.Type())
		}
	}
	check(reflect.ValueOf(d), reflect.ValueOf(max), "birth")
	if len(raw) > len(limit) {
		t.Fatal("actual canonical bytes exceed envelope", len(raw), len(limit))
	}
	if d.Kind == "INPUT" {
		if len(d.SourcesBorn) != 1 || len(d.NodesBorn) != 1 || len(d.MemoryIDs) != 1 || len(d.AddedGrants) != 1 || len(d.AddedAcquisitions) != 1 || len(d.ParentNodes) != 0 || len(d.ParentSources) != 0 || d.Metadata != nil || d.OriginalDependency != nil {
			t.Fatal("INPUT shape outside actual producer")
		}
	} else {
		if d.Kind != "DERIVED" || d.Metadata == nil || len(d.NodesBorn) > 1 || len(d.SourcesBorn) != 0 || len(d.AddedGrants) != 0 {
			t.Fatal("Mock DERIVED shape")
		}
		n := len(d.NodesBorn)
		if len(d.MemoryIDs) != n || len(d.ParentNodes) != n || len(d.ParentSources) != n || len(d.AddedAcquisitions) != n {
			t.Fatal("Mock singleton-source parent closure")
		}
		if n == 1 {
			if len(d.NodesBorn[0].Sources) != 1 || len(d.NodesBorn[0].Parents) != 1 || len(d.NodesBorn[0].MediaRefs) != 0 || len(d.ParentNodes[0].Sources) != 1 || len(d.ParentNodes[0].Parents) != 0 {
				t.Fatal("Mock actual input parent family")
			}
		}
	}
	t.Logf("actual codec ledger kind=%s repair=%t bytes=%d envelope=%d maxDocs=%d historyBound=%d; field-domain bound, not a minted envelope", d.Kind, d.OriginalDependency != nil, len(raw), len(limit), 2*memory.MaxSources, len(limit)*2*memory.MaxSources)
}

func assertIntegrationBirthRecord(t *testing.T, f integrationFixFixture, task, kind string) store.IntegrationMemoryBirthDocument {
	t.Helper()
	var raw, hash []byte
	var operation, original string
	e := f.r.Store.DB.QueryRow(`SELECT a.document,r.payload_hash,r.operation,r.command_key FROM admin_results a JOIN commit_receipts r ON r.commit_key=a.result_id WHERE a.tenant_id=? AND a.product_id=? AND r.task_id=? AND a.schema_name='IntegrationMemoryBirthV1' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.Kind'))=?`, f.world.TenantID, f.world.ProductID, task, kind).Scan(&raw, &hash, &operation, &original)
	if e != nil {
		t.Fatal("actual companion row", kind, e)
	}
	sum := sha256.Sum256(raw)
	var d store.IntegrationMemoryBirthDocument
	if json.Unmarshal(raw, &d) != nil || !bytes.Equal(hash, sum[:]) || d.ProofID != original || operation != "INTEGRATION_MEMORY_BIRTH_V1" || d.TaskID != task || d.Kind != kind {
		t.Fatal("actual owning companion binding")
	}
	for _, text := range []string{"Pass the sign and remember my request.", "May I borrow the sign?"} {
		if bytes.Contains(raw, []byte(text)) {
			t.Fatal("proof contains original text")
		}
	}
	if len(raw) == 0 || len(raw) > 16<<20 {
		t.Fatal("proof codec bound", len(raw))
	}
	assertIntegrationMockBirthCapacity(t, d, raw)
	t.Logf("actual %s codec bytes=%d sources=%d nodes=%d parentNodes=%d parentSources=%d revision=%d->%d", kind, len(raw), len(d.SourcesBorn), len(d.NodesBorn), len(d.ParentNodes), len(d.ParentSources), d.BeforeAggregateRevision, d.AfterAggregateRevision)
	return d
}

func integrationBirthExchange(t *testing.T, f integrationFixFixture, key, profile string) (context.Context, generated.ClientSession) {
	t.Helper()
	var external, entitlement string
	if e := f.r.Store.DB.QueryRow("SELECT external_id,entitlement_id FROM integration_world_participants WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&external, &entitlement); e != nil {
		t.Fatal(e)
	}
	session, e := f.r.Store.ExchangeIntegrationSession(f.service, key, profile, generated.ClientSessionRequest{ProductID: f.world.ProductID, WorldInstanceID: f.world.WorldInstanceID, ExternalSubjectID: external, EntitlementReference: entitlement, RequestedActions: []string{"READ", "TURN", "MEMORY_VIEW", "MEMORY_GOVERN", "CONTROL_PAUSE", "CONTROL_RESUME"}})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	ctx, _, e := f.r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	return ctx, session
}
func integrationBirthCommitMock(t *testing.T, f integrationFixFixture, key string) (string, domain.CandidateCommit) {
	t.Helper()
	q, e := f.quote(f.service, key+"-quote")
	if e != nil {
		t.Fatal(e)
	}
	req := f.input
	req.QuoteID = q.QuoteID
	req.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
	accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, key, req)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := f.r.Store.ClaimRegistered(context.Background(), key+"-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.r.Worker.Turns.Execute(context.Background(), lease.TaskLease, key+"-worker"); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	var c domain.CandidateCommit
	if e = f.r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", accepted.TurnID).Scan(&raw); e != nil || json.Unmarshal(raw, &c) != nil || len(c.MemoryIDs) != 1 {
		t.Fatal("actual nonempty Mock COMMIT", e)
	}
	return accepted.TurnID, c
}
func integrationBirthAggregateRead(t *testing.T, f integrationFixFixture) memory.Aggregate {
	t.Helper()
	var raw []byte
	var a memory.Aggregate
	if e := f.r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&raw); e != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal(e)
	}
	return a
}
func verifyIntegrationBirthGovernance(t *testing.T, kind string) {
	f := integrationBirthFixture(t, "integration_r0_v1")
	initial := integrationBirthAggregateRead(t, f)
	task, c := integrationBirthCommitMock(t, f, "birth-govern-turn")
	proof := assertIntegrationBirthRecord(t, f, task, "INPUT")
	assertIntegrationBirthRecord(t, f, task, "DERIVED")
	target := proof.MemoryIDs[0]
	if kind == "derived" {
		target = c.MemoryIDs[0]
	}
	if kind == "initial" {
		target = ""
		for id, n := range initial.Nodes {
			if n.Domain != memory.WorldFact {
				target = id
				break
			}
		}
		if target == "" {
			t.Fatal("initial corrigible node")
		}
	}
	if page, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !page.BodyReadable {
		t.Fatal("actual post-TURN read and installed owner prerequisite", e)
	}
	current := f.client
	revision := 1
	originalTarget := target
	for step := 0; step < 2; step++ {
		a := integrationBirthAggregateRead(t, f)
		n := a.Nodes[target]
		raw := []byte(fmt.Sprintf(`{"operation":"CORRECT","memory_ids":[%q],"expected_revisions":{%q:%q},"replacement":%q,"reason":"real post-TURN correction"}`, target, target, fmt.Sprint(n.Revision), fmt.Sprintf("birth corrected %s %d", kind, step)))
		if e := generated.Validate("MemoryGovernanceCommandV1", raw); e != nil {
			t.Fatal(e)
		}
		receipt, e := f.r.Memory.GovernIntegration(current, f.world.WorldInstanceID, fmt.Sprintf("birth-correct-%s-%d", kind, step), raw)
		if e != nil || receipt.TaskState != "COMMITTED" {
			t.Fatal("real post-TURN CORRECT", kind, step, e, receipt.TaskState)
		}
		entry, e := f.r.Local.Ledger.Lookup(context.Background(), receipt.TaskID)
		if e != nil || entry.Binding == nil || entry.Binding.ReplacementID == "" {
			t.Fatal("original appended correction proof", e)
		}
		cleaned := integrationBirthAggregateRead(t, f)
		if cleaned.Nodes[target].Body != "" || !cleaned.Nodes[target].Blocked {
			t.Fatal("original cleanup not completed")
		}
		target = entry.Binding.ReplacementID
		next, _ := integrationBirthExchange(t, f, fmt.Sprintf("birth-correct-session-%s-%d", kind, step), "integration_r0_v1")
		current = next
		if _, e = f.r.Store.ApplyPlayerSelfConsent(current, fmt.Sprintf("birth-renew-%s-%d", kind, step), json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(revision)))); e != nil {
			t.Fatal("real same-person new Session RENEW", e)
		}
		revision++
		for _, profile := range []string{"public_v1", "integration_r0_v1"} {
			ctx, session := integrationBirthExchange(t, f, fmt.Sprintf("birth-read-%s-%d-%s", kind, step, profile), profile)
			base := "/world/v1/instances/" + f.world.WorldInstanceID + "/memories/"
			out := selfMemoryHTTP(f.r, session, profile, "GET", base+target+"?include=provenance_v1", "", nil)
			if out.Code != 200 || generated.Validate("MemoryProvenanceV1", out.Body.Bytes()) != nil {
				t.Fatal("post-TURN replacement provenance", kind, step, profile, out.Code)
			}
			_, node, _, e := f.r.Memory.ReadIntegrationMemory(ctx, f.world.WorldInstanceID, target)
			if e != nil || node.Body != fmt.Sprintf("birth corrected %s %d", kind, step) {
				t.Fatal("same-handle replacement body", e)
			}
			for id, n := range initial.Nodes {
				if id != originalTarget && !n.Blocked {
					out = selfMemoryHTTP(f.r, session, profile, "GET", base+id+"?include=provenance_v1", "", nil)
					if out.Code != 200 {
						t.Fatal("unchanged initial item after cleanup", profile, out.Code)
					}
					break
				}
			}
		}
	}
	a := integrationBirthAggregateRead(t, f)
	raw := []byte(fmt.Sprintf(`{"operation":"DELETE","memory_ids":[%q],"expected_revisions":{%q:%q},"reason":"old target after chain"}`, originalTarget, originalTarget, fmt.Sprint(a.Nodes[originalTarget].Revision)))
	if receipt, e := f.r.Memory.GovernIntegration(current, f.world.WorldInstanceID, "birth-late-old-delete-"+kind, raw); e != nil || receipt.TaskState != "COMMITTED" {
		t.Fatal("late old target deletion", e)
	}
	if _, e := f.r.Store.ApplyPlayerSelfConsent(current, "birth-final-renew-"+kind, json.RawMessage(fmt.Sprintf(`{"command":"RENEW","expected_revision":%q,"policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`, fmt.Sprint(revision)))); e != nil {
		t.Fatal(e)
	}
	if _, n, _, e := f.r.Memory.ReadIntegrationMemory(current, f.world.WorldInstanceID, target); e != nil || n.Body == "" {
		t.Fatal("cleared intermediate proof remains usable", kind, e)
	}
}

func TestIntegrationTurnMemoryProofFinalAndRecovery(t *testing.T) {
	t.Run("committed-history-entry-and-query-progression", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		task, c := integrationBirthCommitMock(t, f, "birth-retained-history")
		attempt := c.Metadata.Identity.AttemptID
		positive := func() {
			t.Helper()
			if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
				t.Fatal("retained current positive", e)
			}
		}
		positive()
		type entryFault struct {
			name, table, column, key string
			bad                      any
		}
		cases := []entryFault{
			{"request-hash", "attempts", "request_hash", "attempt_id", bytes.Repeat([]byte{0xef}, 32)},
			{"route", "attempts", "route", "attempt_id", "wrong-route"},
			{"query-identity", "harness_original_queries", "request_identity", "attempt_id", []byte(`{}`)},
			{"validation-hash", "harness_original_validations", "body_hash", "attempt_id", fmt.Sprintf("%064x", 1)},
		}
		for _, v := range cases {
			t.Run(v.name, func(t *testing.T) {
				var before []byte
				if e := f.r.Store.DB.QueryRow("SELECT "+v.column+" FROM "+v.table+" WHERE task_id=? AND "+v.key+"=?", task, attempt).Scan(&before); e != nil {
					t.Fatal(e)
				}
				set := func(value any) {
					t.Helper()
					res, e := f.r.Store.DB.Exec("UPDATE "+v.table+" SET "+v.column+"=? WHERE task_id=? AND "+v.key+"=?", value, task, attempt)
					if e != nil {
						t.Fatal(e)
					}
					if n, _ := res.RowsAffected(); n != 1 {
						t.Fatal("owned entry-fault row count", n)
					}
				}
				set(v.bad)
				defer set(before)
				p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, false)
				if e != nil || p.BodyReadable || len(p.Nodes) == 0 {
					t.Fatal("entry failure preserves narrow metadata", e)
				}
				if _, e = f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e == nil {
					t.Fatal("entry retained dependency fault granted body")
				}
			})
			positive()
		}
		// This is the real original-query owner, not a SQL counter mutation. It
		// progresses query lease/fence/count while the original committed body proof stays valid.
		var before, after int
		if e := f.r.Store.DB.QueryRow("SELECT queries_used FROM harness_original_queries WHERE task_id=? AND attempt_id=?", task, attempt).Scan(&before); e != nil {
			t.Fatal(e)
		}
		time.Sleep(1100 * time.Millisecond)
		lease, e := f.r.Store.BeginOriginalQuery(context.Background(), c.Metadata.Identity)
		if e != nil {
			t.Fatal("real retained original query admission", e)
		}
		if _, e = f.r.Store.RecordOriginalQuery(context.Background(), lease); e != nil {
			t.Fatal("real retained original query completion", e)
		}
		if e = f.r.Store.DB.QueryRow("SELECT queries_used FROM harness_original_queries WHERE task_id=? AND attempt_id=?", task, attempt).Scan(&after); e != nil || after != before+1 {
			t.Fatal("actual original-query progression", before, after, e)
		}
		positive()
		// Owned candidate-file absence is a negative resource fixture, not a claim
		// that a governance deletion occurred. Birth reading must use retained commitments.
		if !filepath.IsLocal(c.Metadata.BodyStorage) {
			t.Fatal("nonlocal owned result")
		}
		original := filepath.Join(f.r.Files.RootPath, f.world.TenantID, f.world.ProductID, c.Metadata.BodyStorage+".part")
		if e = os.Remove(original); e != nil {
			t.Fatal("owned original candidate absence", e)
		}
		positive()
	})
	t.Run("real-conflict-recompute-committed-birth", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		ctx := context.Background()
		worker := "birth-conflict-worker"
		q, e := f.quote(f.service, "birth-conflict-quote")
		if e != nil {
			t.Fatal(e)
		}
		req := f.input
		req.QuoteID = q.QuoteID
		req.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "birth-conflict-first", req)
		if e != nil {
			t.Fatal(e)
		}
		lease, e := f.r.Store.ClaimRegistered(ctx, worker, time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
		if e != nil {
			t.Fatal(e)
		}
		enter := func() (context.Context, func()) {
			t.Helper()
			p, close, e := f.r.Local.Participant.Enter(ctx, maintenance.HarnessWriter)
			if e != nil {
				t.Fatal(e)
			}
			p, e = f.r.Store.EnterIntegrationTaskIssuer(p, lease.TaskLease, worker)
			if e != nil {
				close()
				t.Fatal(e)
			}
			return p, close
		}
		stage, close := enter()
		d, e := f.r.Turns.PrepareOriginalTurn(stage, lease.TaskLease, worker)
		if e != nil {
			close()
			t.Fatal(e)
		}
		if _, e = f.r.Store.AuthorizeDispatch(stage, d); e != nil {
			close()
			t.Fatal(e)
		}
		d.CommandKey = content.Identity(accepted.TurnID, "send")
		d.CommitKey = d.CommandKey
		auth, e := f.r.Store.StartSend(stage, d, d.AttemptID)
		if e != nil || !auth.CanSend {
			close()
			t.Fatal("original send", e)
		}
		if e = f.r.Worker.Turns.Generate(stage, auth.OriginalSend()); e != nil {
			close()
			t.Fatal(e)
		}
		if _, e = f.r.Store.ObserveOriginalReply(stage, auth.OriginalSend()); e != nil {
			close()
			t.Fatal(e)
		}
		if e = f.r.Store.ValidateOriginalTurn(stage, lease.TaskLease, worker); e != nil {
			close()
			t.Fatal(e)
		}
		if e = f.r.Store.ApproveOriginalTurn(stage, lease.TaskLease, worker); e != nil {
			close()
			t.Fatal(e)
		}
		close()
		// A second real accepted TURN commits a world change; no SQL manufacture
		// of the conflict round, recovery approval, attempt, result or receipt.
		integrationBirthCommitMock(t, f, "birth-conflict-competitor")
		stage, close = enter()
		_, e = f.r.Store.CommitOriginalTurn(stage, lease.TaskLease, worker)
		close()
		if errors.Is(e, store.ErrConflict) && !errors.Is(e, store.ErrCandidateResourceConflict) {
			var attempts, repairs int
			if qerr := f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM attempts WHERE task_id=?),(SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?)", accepted.TurnID, accepted.TurnID).Scan(&attempts, &repairs); qerr != nil || attempts != 1 || repairs != 0 {
				t.Fatal("current-context refusal created recovery", attempts, repairs, qerr)
			}
			t.Skip("NOT_REACHED: real competing TURN changes original integration context before candidate conflict; original gate retained; no recovery evidence manufactured")
		}
		if !errors.Is(e, store.ErrCandidateResourceConflict) {
			t.Fatal("actual original conflict", e)
		}
		stage, close = enter()
		e = f.r.Worker.Turns.ExecuteRecovery(stage, lease.TaskLease, worker)
		close()
		if e != nil {
			t.Fatal("actual recompute", e)
		}
		var raw []byte
		var commit domain.CandidateCommit
		if e = f.r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", accepted.TurnID).Scan(&raw); e != nil || json.Unmarshal(raw, &commit) != nil || commit.OriginalDependency == nil {
			t.Fatal("actual recompute commitment", e)
		}
		assertIntegrationBirthRecord(t, f, accepted.TurnID, "DERIVED")
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
			t.Fatal("actual recompute birth current proof", e)
		}
	})
	t.Run("real-structure-repair-committed-birth", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		f.r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
		task, c := integrationBirthCommitMock(t, f, "birth-actual-repair")
		if c.OriginalDependency == nil || c.Metadata.Identity.AttemptID == c.OriginalDependency.Identity.AttemptID {
			t.Fatal("real shared repair dependency absent")
		}
		var calls int
		var reason string
		if e := f.r.Store.DB.QueryRow("SELECT t.calls_used,JSON_UNQUOTE(JSON_EXTRACT(CONVERT(r.document USING utf8mb4),'$.Reason')) FROM tasks t JOIN harness_recovery_approvals r USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task).Scan(&calls, &reason); e != nil || calls != 2 || reason != "STRUCTURE_REPAIR" {
			t.Fatal("actual repair qualification", calls, reason, e)
		}
		assertIntegrationBirthRecord(t, f, task, "DERIVED")
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
			t.Fatal("actual repaired birth current proof", e)
		}
	})
	t.Run("current-final-and-retained-proof", func(t *testing.T) {
		f := integrationBirthFixture(t, "integration_r0_v1")
		task, c := integrationBirthCommitMock(t, f, "birth-final-turn")
		input := assertIntegrationBirthRecord(t, f, task, "INPUT")
		derived := assertIntegrationBirthRecord(t, f, task, "DERIVED")
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
			t.Fatal("actual current positive prerequisite", e)
		}
		type fault struct {
			name, sql string
			args      []any
		}
		faults := []fault{
			{"aggregate-revision", "UPDATE memory_aggregates SET revision=revision+1 WHERE world_instance_id=?", []any{f.world.WorldInstanceID}},
			{"source-body", "UPDATE memory_aggregates SET document=JSON_SET(document,?,?) WHERE world_instance_id=?", []any{"$.Sources.\"" + input.InputSourceID + "\".Body", "changed input data", f.world.WorldInstanceID}},
			{"node-body", "UPDATE memory_aggregates SET document=JSON_SET(document,?,?) WHERE world_instance_id=?", []any{"$.Nodes.\"" + c.MemoryIDs[0] + "\".Body", "changed derived data", f.world.WorldInstanceID}},
			{"companion-missing", "DELETE FROM commit_receipts WHERE commit_key=?", []any{derived.ProofID}},
			{"proof-missing", "DELETE FROM admin_results WHERE result_id=?", []any{input.ProofID}},
			{"proof-version", "UPDATE admin_results SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.Format','unknown') WHERE result_id=?", []any{derived.ProofID}},
			{"proof-hash", "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ff',32)) WHERE commit_key=?", []any{derived.ProofID}},
			{"root-key", "UPDATE commit_receipts SET command_key='wrong-original' WHERE commit_key=?", []any{input.Original.Key}},
			{"root-owner", "UPDATE commit_receipts SET owner_subject_id='s66-service' WHERE commit_key=?", []any{input.Original.Key}},
			{"attempt-request-hash", "UPDATE attempts SET request_hash=UNHEX(REPEAT('ef',32)) WHERE task_id=? AND attempt_id=?", []any{task, c.Metadata.Identity.AttemptID}},
			{"attempt-route", "UPDATE attempts SET route='wrong-route' WHERE task_id=? AND attempt_id=?", []any{task, c.Metadata.Identity.AttemptID}},
			{"query-result-identity", "UPDATE harness_original_queries SET metadata=JSON_SET(metadata,'$.Identity.RequestKey','wrong-request') WHERE task_id=? AND attempt_id=?", []any{task, c.Metadata.Identity.AttemptID}},
			{"validation-body", "UPDATE harness_original_validations SET body_hash=REPEAT('e',64) WHERE task_id=? AND attempt_id=?", []any{task, c.Metadata.Identity.AttemptID}},
			{"approval-receipt", "DELETE FROM commit_receipts WHERE commit_key=(SELECT receipt_key FROM harness_candidate_approvals WHERE task_id=? AND attempt_id=?)", []any{task, c.Metadata.Identity.AttemptID}},
			{"actual-memory-ids", "UPDATE harness_turn_commits SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.MemoryIDs',JSON_ARRAY()) WHERE task_id=?", []any{task}},
			{"body-permission", "UPDATE memory_subject_permissions SET enabled=0 WHERE data_subject_id=? AND action='memory.private_body.read'", []any{input.DataSubjectID}},
		}
		for _, v := range faults {
			t.Run(v.name, func(t *testing.T) {
				for _, body := range []bool{false, true} {
					called := false
					var injection error
					f.r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
						called = true
						res, e := tx.ExecContext(ctx, v.sql, v.args...)
						if e != nil {
							injection = e
							return e
						}
						n, e := res.RowsAffected()
						if e != nil || n < 1 {
							injection = fmt.Errorf("actual read fault rows %d: %v", n, e)
						}
						return injection
					}
					p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, body)
					f.r.Store.BeforeReadFinish = nil
					if !called || injection != nil {
						t.Fatal("actual final fault prerequisite", called, injection)
					}
					if v.name == "body-permission" && !body {
						if e != nil || p.BodyReadable || len(p.Nodes) == 0 {
							t.Fatal("metadata must safely downgrade when body permission changes", e)
						}
						for _, n := range p.Nodes {
							if n.Body != "" {
								t.Fatal("downgraded metadata exposed body")
							}
						}
					} else if e == nil || len(p.Nodes) > 0 {
						t.Fatal("final drift escaped as metadata qualification or body", v.name, body)
					}
					if good, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !good.BodyReadable {
						t.Fatal("read transaction rollback", e)
					}
				}
			})
		}
		// Actual old/missing evidence is not reconstructed from the live aggregate.
		var retained []byte
		if e := f.r.Store.DB.QueryRow("SELECT document FROM admin_results WHERE result_id=?", derived.ProofID).Scan(&retained); e != nil {
			t.Fatal(e)
		}
		res, e := f.r.Store.DB.Exec("DELETE FROM admin_results WHERE result_id=?", derived.ProofID)
		if e != nil {
			t.Fatal(e)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatal("owned retained-proof fault")
		}
		restore := func() {
			res, e := f.r.Store.DB.Exec("INSERT INTO admin_results(tenant_id,product_id,result_id,owner_subject_id,schema_name,document) VALUES(?,?,?,?,?,?)", f.world.TenantID, f.world.ProductID, derived.ProofID, derived.DataSubjectID, "IntegrationMemoryBirthV1", retained)
			if e != nil {
				t.Fatal("exact owned proof restoration", e)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatal("restore count")
			}
		}
		defer func() {
			if retained != nil {
				restore()
			}
		}()
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, false); e != nil || p.BodyReadable || len(p.Nodes) == 0 {
			t.Fatal("missing legacy witness must preserve metadata only", e)
		}
		if _, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e == nil {
			t.Fatal("missing legacy witness synthesized")
		}
		restore()
		retained = nil
		if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
			t.Fatal("exact restoration positive", e)
		}
	})
	for _, kind := range []string{"INPUT", "DERIVED"} {
		for _, mode := range []string{"aggregate", "document", "receipt", "original", "ack-lost"} {
			t.Run(kind+"-writer-"+mode, func(t *testing.T) { verifyIntegrationBirthWriterFinal(t, kind, mode) })
		}
	}
}
func verifyIntegrationBirthWriterFinal(t *testing.T, kind, mode string) {
	f := integrationBirthFixture(t, "integration_r0_v1")
	q, e := f.quote(f.service, "birth-writer-quote")
	if e != nil {
		t.Fatal(e)
	}
	req := f.input
	req.QuoteID = q.QuoteID
	req.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
	task := ""
	// Scope.SubjectID belongs to the SERVICE response; use the actual registered
	// player identity already bound by the world participant record.
	var self string
	if e = f.r.Store.DB.QueryRow("SELECT player_subject_id FROM integration_world_participants WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&self); e != nil {
		t.Fatal(e)
	}
	task = integrationTestCreationID(f.world.TenantID, f.world.ProductID, self, f.world.WorldInstanceID, "TURN", "birth-writer")
	called := false
	var injection error
	var proofID, rootID string
	active := func(ctx context.Context, tx *sql.Tx) bool {
		var n int
		e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_results a JOIN commit_receipts r ON r.commit_key=a.result_id WHERE r.task_id=? AND a.schema_name='IntegrationMemoryBirthV1' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.Kind'))=?", task, kind).Scan(&n)
		if e != nil {
			injection = e
			return false
		}
		if n == 0 {
			return false
		}
		if n != 1 {
			injection = fmt.Errorf("writer proof count %d", n)
			return false
		}
		e = tx.QueryRowContext(ctx, "SELECT a.result_id,JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.Original.Key')) FROM admin_results a JOIN commit_receipts r ON r.commit_key=a.result_id WHERE r.task_id=? AND a.schema_name='IntegrationMemoryBirthV1' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.Kind'))=?", task, kind).Scan(&proofID, &rootID)
		if e != nil {
			injection = e
			return false
		}
		return true
	}
	install := func() {
		if mode == "ack-lost" {
			f.r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
				hit := !called && active(ctx, tx)
				if injection != nil {
					return injection
				}
				if e := tx.Commit(); e != nil {
					return e
				}
				if hit {
					called = true
					return errors.New("synthetic original COMMIT ACK lost")
				}
				return nil
			}
		} else {
			f.r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				if called || !active(ctx, tx) {
					return injection
				}
				called = true
				query := ""
				args := []any{}
				switch mode {
				case "aggregate":
					query = "UPDATE memory_aggregates SET revision=revision+1 WHERE world_instance_id=?"
					args = []any{f.world.WorldInstanceID}
				case "document":
					query = "UPDATE admin_results SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.AfterIntake',999) WHERE result_id=?"
					args = []any{proofID}
				case "receipt":
					query = "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ee',32)) WHERE commit_key=?"
					args = []any{proofID}
				case "original":
					query = "UPDATE commit_receipts SET command_key='wrong-original' WHERE commit_key=?"
					args = []any{rootID}
				}
				res, e := tx.ExecContext(ctx, query, args...)
				if e != nil {
					injection = e
					return e
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					injection = fmt.Errorf("writer fault rows %d: %v", n, e)
				}
				return injection
			}
		}
	}
	if kind == "INPUT" {
		install()
	}
	accepted, acceptErr := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "birth-writer", req)
	var observed error = acceptErr
	if kind == "DERIVED" {
		if acceptErr != nil {
			t.Fatal("real acceptance prerequisite", acceptErr)
		}
		if accepted.TurnID != task {
			t.Fatal("original task identity")
		}
		install()
		lease, e := f.r.Store.ClaimRegistered(context.Background(), "birth-final-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: task}})
		if e != nil {
			t.Fatal(e)
		}
		observed = f.r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "birth-final-worker")
	}
	f.r.Store.BeforeCommit = nil
	f.r.Store.Commit = nil
	if !called || injection != nil {
		t.Fatal("original active writer hook prerequisite", called, injection)
	}
	var proofs, roots int
	if e = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM admin_results WHERE result_id=?),(SELECT COUNT(*) FROM commit_receipts WHERE commit_key=?)", proofID, rootID).Scan(&proofs, &roots); e != nil {
		t.Fatal(e)
	}
	if mode != "ack-lost" {
		if observed == nil || proofs != 0 || roots != 0 {
			t.Fatal("expected frozen writer tuple escaped rollback", kind, mode, observed, proofs, roots)
		}
		return
	}
	if proofs != 1 || roots != 1 {
		t.Fatal("unknown lost committed pair", proofs, roots)
	}
	// Worker may safely resolve its original commit internally. Its persisted
	// original calls counter must remain one; no second supplier send is allowed.
	if kind == "INPUT" {
		if !errors.Is(observed, store.ErrCommitUnknown) {
			t.Fatal("input unknown outcome", observed)
		}
		read, e := f.r.Store.ReadIntegrationIntakeReceipt(f.client, f.world.WorldInstanceID, "birth-writer")
		if e != nil || read.Acceptance.TurnID != task {
			t.Fatal("original key resolves existing acceptance", e)
		}
	} else {
		var calls int
		if e = f.r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", task).Scan(&calls); e != nil || calls != 1 {
			t.Fatal("original single supplier call", calls, e)
		}
	}
	before := integrationBirthAggregateRead(t, f)
	again, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "birth-writer", req)
	if e != nil || again.TurnID != task {
		t.Fatal("original key replay", e)
	}
	after := integrationBirthAggregateRead(t, f)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("replay changed aggregate")
	}
	assertIntegrationBirthRecord(t, f, task, kind)
	if p, e := f.r.Memory.ListIntegrationMetadata(f.client, f.world.WorldInstanceID, "", "", "integration_r0_v1", 50, true); e != nil || !p.BodyReadable {
		t.Fatal("actual committed unknown proof", e)
	}
}
