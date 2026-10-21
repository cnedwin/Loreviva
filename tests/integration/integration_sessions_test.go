package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIntegrationSessionConsentHTTP(t *testing.T) {
	r, _, service, request, registration := integrationCreationFixture(t)
	world, err := r.World.CreateIntegrationInstance(service, "s3-http-world", request)
	if err != nil {
		t.Fatal(err)
	}
	input := generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: []string{"READ"}}
	body, _ := json.Marshal(input)
	call := func(method, path, token, profile, key string, raw []byte) *httptest.ResponseRecorder {
		q := httptest.NewRequest(method, path, bytes.NewReader(raw))
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Loreviva-Client-Contract", profile)
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		return w
	}
	w := call("POST", "/world/v1/client-sessions", "synthetic-s66-create-service-secret-32", "integration_r0_v1", "s3-http-session", body)
	if w.Code != 201 {
		t.Fatalf("real exchange HTTP status=%d", w.Code)
	}
	var session generated.ClientSession
	if generated.Decode("ClientSession", w.Body.Bytes(), &session) != nil {
		t.Fatal("invalid Session response")
	}
	retainSessionScanMaterial(t, session.AccessToken)
	for _, test := range []struct {
		profile string
		status  int
	}{{"integration_r0_v1", 200}, {"public_v1", 409}, {"unknown", 400}, {"", 409}} {
		w = call("GET", "/world/v1/self-consent", session.AccessToken, test.profile, "", nil)
		if w.Code != test.status {
			t.Fatalf("self metadata profile %q status=%d expected=%d", test.profile, w.Code, test.status)
		}
	}
	w = call("POST", "/world/v1/self-consent/commands", session.AccessToken, "integration_r0_v1", "s3-http-initial", []byte(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
	if w.Code != 200 {
		t.Fatalf("consent HTTP status=%d", w.Code)
	}
	w = call("GET", "/world/v1/self-consent", "synthetic-s66-create-service-secret-32", "integration_r0_v1", "", nil)
	if w.Code != 401 {
		t.Fatal("SERVICE self endpoint authentication", w.Code)
	}
}

func sessionConsentFixture(t *testing.T) (*app.Runtime, context.Context, context.Context, generated.ClientSessionRequest, generated.ClientSession) {
	t.Helper()
	r, _, service, req, reg := integrationCreationFixture(t)
	w, e := r.World.CreateIntegrationInstance(service, "s3-proof-world", req)
	if e != nil {
		t.Fatal(e)
	}
	input := generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: []string{"READ"}}
	session, e := r.Store.ExchangeIntegrationSession(service, "s3-proof-session", "integration_r0_v1", input)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	return r, client, service, input, session
}

func TestIntegrationConsentOriginalProofAndAtomicity(t *testing.T) {
	for _, mutation := range []string{"receipt-scope", "receipt-reference", "fact-final"} {
		t.Run(mutation, func(t *testing.T) {
			r, client, _, _, _ := sessionConsentFixture(t)
			initial := json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
			if mutation == "fact-final" {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					_, e := tx.ExecContext(ctx, "DELETE FROM integration_self_consent_facts")
					return e
				}
				_, e := r.Store.ApplyPlayerSelfConsent(client, "s3-proof-initial", initial)
				r.Store.BeforeCommit = nil
				if e == nil {
					t.Fatal("commit accepted missing append-only choice fact")
				}
				return
			}
			if _, e := r.Store.ApplyPlayerSelfConsent(client, "s3-proof-initial", initial); e != nil {
				t.Fatal(e)
			}
			field := "$.snapshot.scope.product_id"
			if mutation == "receipt-reference" {
				field = "$.receipt_ref"
			}
			if _, e := r.Store.DB.Exec("UPDATE integration_receipts SET result_metadata=JSON_SET(result_metadata,?, 'foreign-proof') WHERE operation='PLAYER_SELF_CONSENT'", field); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ApplyPlayerSelfConsent(client, "s3-proof-initial", initial); e == nil {
				t.Fatal("original proof accepted foreign receipt binding")
			}
		})
	}
}

func TestIntegrationSessionKeyAndConsentOverflow(t *testing.T) {
	r, client, service, input, session := sessionConsentFixture(t)
	initial := json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
	if _, e := r.Store.ApplyPlayerSelfConsent(client, "s3-key-initial", initial); e != nil {
		t.Fatal(e)
	}
	// Fault injection into an owned synthetic row proves overflow rollback;
	// it does not stand in for a real consent or expiry writer.
	if _, e := r.Store.DB.Exec("UPDATE integration_self_consent_current SET use_authority_generation=18446744073709551615"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ApplyPlayerSelfConsent(client, "s3-overflow", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e == nil {
		t.Fatal("generation overflow accepted")
	}
	var facts int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_self_consent_facts").Scan(&facts); e != nil || facts != 1 {
		t.Fatal("overflow wrote fact", e)
	}
	var clear int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_receipts WHERE LOCATE(?,result_metadata)>0)+(SELECT COUNT(*) FROM integration_principals WHERE LOCATE(?,COALESCE(client_actions,''))>0)", session.AccessToken, session.AccessToken).Scan(&clear); e != nil || clear != 0 {
		t.Fatal("plaintext token persisted", e)
	}
	// Delete only this fixture's collected independent key. Existing Session
	// references must prevent both authentication and original-key repair.
	t.Run("owned-key-loss", func(t *testing.T) {
		root := os.Getenv("TMP")
		channel := os.Getenv("LOREVIVA_TEST_CUSTODY_FILE")
		if channel == "" {
			t.Skip("key-loss fixture requires the explicit bounded custody root")
		}
		if root == "" || filepath.Dir(channel) != root {
			t.Fatal("invalid owned key fixture root")
		}
		paths := []string{}
		if e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return os.ErrPermission
			}
			if !d.IsDir() && d.Name() == "session-hmac-v1.key" {
				paths = append(paths, p)
			}
			return nil
		}); e != nil || len(paths) != 1 {
			t.Fatal("exact own key inventory", e)
		}
		if e := os.Remove(paths[0]); e != nil {
			t.Fatal("owned key removal failed")
		}
		if _, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient); e == nil {
			t.Fatal("missing installation key authenticated")
		}
		if _, e := r.Store.ExchangeIntegrationSession(service, "s3-proof-session", "integration_r0_v1", input); e == nil {
			t.Fatal("referenced key recreated")
		}
	})
}

// Session issuance must follow real publication, registration and private
// participant creation. Neither an Operator context nor SQL can mint this proof.
func TestIntegrationSessionRealOwning(t *testing.T) {
	r, operator, service, request, registration := integrationCreationFixture(t)
	world, err := r.World.CreateIntegrationInstance(service, "s3-session-world", request)
	if err != nil {
		t.Fatal("real private participant prerequisite", err)
	}
	issuer, ok := any(r.Store).(interface {
		ExchangeIntegrationSession(context.Context, string, string, generated.ClientSessionRequest) (generated.ClientSession, error)
	})
	if !ok {
		t.Fatal("real Registry/CREATE/SERVICE prerequisites established; Session owning port unavailable")
	}
	input := generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: []string{"READ"}}
	if _, err = issuer.ExchangeIntegrationSession(operator, "s3-wrong-kind", "integration_r0_v1", input); err == nil {
		t.Fatal("Operator minted a Session")
	}
	var before time.Time
	if err = r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&before); err != nil {
		t.Fatal(err)
	}
	session, err := issuer.ExchangeIntegrationSession(service, "s3-session", "integration_r0_v1", input)
	if err != nil {
		t.Fatal("real Session exchange", err)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	if session.AccessToken == "" || session.SubjectID != "s66-player" || session.Scope.WorldInstanceID != world.Scope.WorldInstanceID {
		t.Fatal("Session scope or token missing")
	}
	expires, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	if err != nil || expires.Before(before.Add(14*time.Minute+59*time.Second)) || expires.After(before.Add(15*time.Minute+10*time.Second)) {
		t.Fatal("Session lifetime is not the original fifteen minutes")
	}
	replay, err := issuer.ExchangeIntegrationSession(service, "s3-session", "integration_r0_v1", input)
	if err != nil || replay.AccessToken != session.AccessToken || replay.ExpiresAt != session.ExpiresAt {
		t.Fatal("original-key replay changed Session", err)
	}
	_, principal, err := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if err != nil || principal.Kind != domain.IntegrationClient || principal.World != world.Scope.WorldInstanceID || principal.StableSubject != "s66-player" {
		t.Fatal("current typed CLIENT authentication", err)
	}
	if _, _, err = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationService); err == nil {
		t.Fatal("CLIENT promoted to SERVICE")
	}
	if _, _, err = r.Store.AuthenticateOperator(context.Background(), "Bearer "+session.AccessToken); err == nil {
		t.Fatal("CLIENT promoted to Operator")
	}
	input.RequestedActions = []string{"TURN"}
	if _, err = issuer.ExchangeIntegrationSession(service, "s3-session", "integration_r0_v1", input); err == nil {
		t.Fatal("changed original payload accepted")
	}
	if _, err = r.Store.ApplyIntegrationRegistry(operator, "s3-revoke-session", domain.RegistryCommand{Kind: "REVOKE_CLIENT_SESSION", ProductID: "coffee", TargetID: principal.PrincipalID, ExpectedRevision: 1}); err != nil {
		t.Fatal("real registry Session revocation", err)
	}
	if _, _, err = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient); err == nil {
		t.Fatal("revoked Session authenticated")
	}
	input.RequestedActions = []string{"READ"}
	if _, err = issuer.ExchangeIntegrationSession(service, "s3-session", "integration_r0_v1", input); err == nil {
		t.Fatal("revoked original key reminted Session")
	}
}

func TestIntegrationPlayerConsentLifecycle(t *testing.T) {
	r, operator, service, request, registration := integrationCreationFixture(t)
	world, err := r.World.CreateIntegrationInstance(service, "s3-consent-world", request)
	if err != nil {
		t.Fatal(err)
	}
	session, err := r.Store.ExchangeIntegrationSession(service, "s3-consent-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: []string{"READ"}})
	if err != nil {
		t.Fatal(err)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, err := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := any(r.Store).(interface {
		ReadPlayerSelfConsent(context.Context) (generated.PlayerSelfConsentSnapshot, error)
		ApplyPlayerSelfConsent(context.Context, string, generated.PlayerSelfConsentCommandRequest) (generated.PlayerSelfConsentReceipt, error)
	})
	if !ok {
		t.Fatal("real CLIENT prerequisites established; player self-consent owning port unavailable")
	}
	before, err := owner.ReadPlayerSelfConsent(client)
	if err != nil || before.ConsentRevision != "0" || before.CurrentUseEnabled || !before.Eligible || before.State != "NOT_GRANTED" {
		t.Fatal("initial independent eligibility", err)
	}
	initial := json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
	for _, ctx := range []context.Context{operator, service, context.Background()} {
		if _, err = owner.ApplyPlayerSelfConsent(ctx, "s3-foreign", initial); err == nil {
			t.Fatal("non-CLIENT self-consent accepted")
		}
	}
	first, err := owner.ApplyPlayerSelfConsent(client, "s3-initial", initial)
	if err != nil || first.Snapshot.ConsentRevision != "1" || !first.Snapshot.CurrentUseEnabled || first.Snapshot.State != "ACTIVE" {
		t.Fatal("INITIAL without positive use", err)
	}
	again, err := owner.ApplyPlayerSelfConsent(client, "s3-initial", initial)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("original self-consent replay changed", err)
	}
	var generation, pv, privacy, view uint64
	if err = r.Store.DB.QueryRow("SELECT c.use_authority_generation,g.permission_version,g.privacy_epoch,g.view_epoch FROM integration_self_consent_current c JOIN subject_governance g ON g.tenant_id=c.tenant_id AND g.product_id=c.product_id AND g.subject_id=c.player_subject_id WHERE c.player_subject_id='s66-player'").Scan(&generation, &pv, &privacy, &view); err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatal("INITIAL must establish generation one", generation)
	}
	revoke := json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
	second, err := owner.ApplyPlayerSelfConsent(client, "s3-revoke", revoke)
	if err != nil || second.Snapshot.CurrentUseEnabled || second.Snapshot.State != "REVOKED" {
		t.Fatal("REVOKE", err)
	}
	var g2, p2, r2, v2 uint64
	if err = r.Store.DB.QueryRow("SELECT c.use_authority_generation,g.permission_version,g.privacy_epoch,g.view_epoch FROM integration_self_consent_current c JOIN subject_governance g ON g.tenant_id=c.tenant_id AND g.product_id=c.product_id AND g.subject_id=c.player_subject_id WHERE c.player_subject_id='s66-player'").Scan(&g2, &p2, &r2, &v2); err != nil || g2 != generation+1 || p2 != pv+1 || r2 != privacy+1 || v2 != view+1 {
		t.Fatal("REVOKE did not atomically advance all epochs", err)
	}
	if _, err = owner.ApplyPlayerSelfConsent(client, "s3-revoke", revoke); err != nil {
		t.Fatal(err)
	}
	renew := json.RawMessage(`{"command":"RENEW","expected_revision":"2","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
	third, err := owner.ApplyPlayerSelfConsent(client, "s3-renew", renew)
	if err != nil || !third.Snapshot.CurrentUseEnabled || third.Snapshot.ConsentRevision != "3" {
		t.Fatal("RENEW", err)
	}
	var finalGeneration uint64
	var facts int
	if err = r.Store.DB.QueryRow("SELECT use_authority_generation,(SELECT COUNT(*) FROM integration_self_consent_facts WHERE player_subject_id='s66-player') FROM integration_self_consent_current WHERE player_subject_id='s66-player'").Scan(&finalGeneration, &facts); err != nil || finalGeneration != g2 || facts != 3 {
		t.Fatal("replay or RENEW changed revocation generation/facts", err)
	}
}

func TestIntegrationClientMemoryCurrentRead(t *testing.T) {
	r, operator, service, request, registration := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "MEMORY_VIEW"})
	w, err := r.World.CreateIntegrationInstance(service, "s3-memory-world", request)
	if err != nil {
		t.Fatal(err)
	}
	session, err := r.Store.ExchangeIntegrationSession(service, "s3-memory-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: []string{"READ", "MEMORY_VIEW"}})
	if err != nil {
		t.Fatal(err)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, err := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := any(r.Memory).(interface {
		ReadIntegrationMemory(context.Context, string, string) (memory.Aggregate, memory.MemoryRecord, uint64, error)
	})
	if !ok {
		t.Fatal("real registered MEMORY_VIEW and CLIENT ready; current memory owning port unavailable")
	}
	var raw []byte
	if err = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='s66-player'", w.Scope.WorldInstanceID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var a memory.Aggregate
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("initial aggregate")
	}
	id := ""
	for k := range a.Nodes {
		id = k
		break
	}
	if id == "" {
		t.Fatal("missing real initial memory")
	}
	if _, _, _, err = reader.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); err == nil {
		t.Fatal("body exposed before positive use")
	}
	if _, err = r.Store.ApplyPlayerSelfConsent(client, "s3-memory-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); err != nil {
		t.Fatal(err)
	}
	_, node, _, err := reader.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id)
	if err != nil || node.ID != id || node.Body == "" {
		t.Fatal("current own memory read", err)
	}
	for _, include := range []string{"", "?include=provenance_v1"} {
		q := httptest.NewRequest("GET", "/world/v1/instances/"+w.Scope.WorldInstanceID+"/memories/"+id+include, nil)
		q.Header.Set("Authorization", "Bearer "+session.AccessToken)
		q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		out := httptest.NewRecorder()
		r.Handler().ServeHTTP(out, q)
		schema := "PlayerMemoryMetadata"
		if include != "" {
			schema = "MemoryProvenanceV1"
		}
		if out.Code != 200 || generated.Validate(schema, out.Body.Bytes()) != nil {
			t.Fatal("current memory HTTP response", out.Code, schema)
		}
	}
	for _, ctx := range []context.Context{operator, service, context.Background()} {
		if _, _, _, err = reader.ReadIntegrationMemory(ctx, w.Scope.WorldInstanceID, id); err == nil {
			t.Fatal("foreign proof read body")
		}
	}
	if _, _, _, err = reader.ReadIntegrationMemory(client, "different-world", id); err == nil {
		t.Fatal("cross-world read")
	}
	if _, err = r.Store.ApplyPlayerSelfConsent(client, "s3-memory-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = reader.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); err == nil {
		t.Fatal("body exposed after use revoke")
	}
}

func TestIntegrationSlice3CurrentRollbackAndClose(t *testing.T) {
	r, _, service, request, registration := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "s3-current-world", request)
	if e != nil {
		t.Fatal(e)
	}
	session, e := r.Store.ExchangeIntegrationSession(service, "s3-current-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(registration.Player.ExternalIDBytes), EntitlementReference: registration.Player.EntitlementID, RequestedActions: []string{"READ", "MEMORY_VIEW"}})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	initial := json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
	// A failure after the actual writes rolls back choice, facts, permission,
	// epochs and receipt together; it is an injected fault, not a governance API.
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic rollback") }
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "s3-rollback", initial); e == nil {
		t.Fatal("failed commit returned success")
	}
	r.Store.BeforeCommit = nil
	var n int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_self_consent_current)+(SELECT COUNT(*) FROM integration_self_consent_facts)+(SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use')+(SELECT COUNT(*) FROM integration_receipts WHERE operation='PLAYER_SELF_CONSENT')").Scan(&n); e != nil || n != 0 {
		t.Fatal("partial consent write", n, e)
	}
	// Original-key recovery after an actual commit with an ambiguous response.
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic ambiguous commit")
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "s3-unknown", initial); e == nil {
		t.Fatal("ambiguous commit not reported")
	}
	r.Store.Commit = nil
	original, e := r.Store.ApplyPlayerSelfConsent(client, "s3-unknown", initial)
	if e != nil || original.Snapshot.ConsentRevision != "1" {
		t.Fatal("original commit recovery", e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", w.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("aggregate")
	}
	id := ""
	for k := range a.Nodes {
		id = k
		break
	}
	for _, query := range []string{
		"UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'",
		"UPDATE integration_principals SET revoked=1 WHERE kind='CLIENT'",
		"UPDATE releases SET available=0",
		"UPDATE memory_aggregates SET document=JSON_SET(document,'$.PolicyRevision',999)",
	} {
		r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error { _, e := tx.ExecContext(ctx, query); return e }
		if _, _, _, e = r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); e == nil {
			t.Fatal("final current read allowed injected change")
		}
		r.Store.BeforeReadFinish = nil
		if _, _, _, e = r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); e != nil {
			t.Fatal("failed read did not release and rollback", e)
		}
	}
	read, e := r.Store.BeginIntegrationMemoryRead(client, w.Scope.WorldInstanceID)
	if e != nil {
		t.Fatal(e)
	}
	copyScope := read.Scope()
	copyScope.SubjectID = "other"
	_ = copyScope
	read.Close()
	if e = read.Finish(client); e == nil {
		t.Fatal("closed read regained authority")
	}
	if e = r.Local.Coordinator.BeginClose(context.Background(), "s3-close"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ReadPlayerSelfConsent(client); e == nil {
		t.Fatal("Close allowed consent read")
	}
	if _, _, _, e = r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); e == nil {
		t.Fatal("Close allowed memory read")
	}
	if e = r.Local.Coordinator.Drain(context.Background(), "s3-close"); e != nil {
		t.Fatal("released reads failed Drain", e)
	}
}

// Only the private test custodian opts into this out-of-band scan channel.
// No key or token is printed, attached to a receipt, or placed in source output.
func retainSessionScanMaterial(t *testing.T, token string) {
	t.Helper()
	channel := os.Getenv("LOREVIVA_TEST_CUSTODY_FILE")
	if channel == "" {
		return
	}
	root := os.Getenv("TMP")
	if root == "" || filepath.Dir(channel) != root {
		t.Fatal("invalid private custody location")
	}
	values := []string{}
	if token != "" {
		values = append(values, token)
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return os.ErrPermission
		}
		if entry.IsDir() || entry.Name() != "session-hmac-v1.key" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var material struct{ Key []byte }
		if json.Unmarshal(body, &material) != nil || len(material.Key) != 32 {
			return os.ErrInvalid
		}
		values = append(values, base64.StdEncoding.EncodeToString(material.Key), hex.EncodeToString(material.Key))
		return nil
	})
	if err != nil || len(values) < 2 {
		t.Fatal("private custody key collection failed")
	}
	f, err := os.OpenFile(channel, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("private custody channel unavailable")
	}
	err = json.NewEncoder(f).Encode(values)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal("private custody channel failed")
	}
}

// Metadata remains available to the current self with MEMORY_VIEW, independent
// of positive generation use. Body and provenance keep their separate gates.
// Removing the exact subject metadata row must deny both admission and Finish;
// material grants and an action-bearing Session cannot substitute for it.
func TestIntegrationMetadataSubjectPermissionAndGetter(t *testing.T) {
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "MEMORY_VIEW"})
	w, err := r.World.CreateIntegrationInstance(service, "fix2-metadata-world", request)
	if err != nil {
		t.Fatal(err)
	}
	session, err := r.Store.ExchangeIntegrationSession(service, "fix2-metadata-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: []string{"READ", "MEMORY_VIEW"}})
	if err != nil {
		t.Fatal(err)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, err := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if err != nil {
		t.Fatal(err)
	}
	tenant, product := string(w.Scope.TenantID), string(w.Scope.ProductID)
	var expiry time.Time
	if err = r.Store.DB.QueryRow("SELECT expires_at FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id='s66-player' AND data_subject_id='s66-player' AND action='memory.metadata.read' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1", tenant, product).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	// The legitimate metadata control must not depend on the separate body row.
	if _, err = r.Store.DB.Exec("DELETE FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id='s66-player' AND data_subject_id='s66-player' AND action='memory.private_body.read'", tenant, product); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", w.Scope.WorldInstanceID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var aggregate memory.Aggregate
	if err = json.Unmarshal(raw, &aggregate); err != nil {
		t.Fatal(err)
	}
	id := ""
	for key := range aggregate.Nodes {
		id = key
		break
	}
	if id == "" {
		t.Fatal("real initialized memory missing")
	}
	positive := func(t *testing.T) {
		t.Helper()
		var positiveUse int
		if err := r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND data_subject_id='s66-player' AND action='memory.generation.use'", tenant, product).Scan(&positiveUse); err != nil || positiveUse != 0 {
			t.Fatal("positive use prerequisite changed", err)
		}
		_, node, _, err := r.Memory.ReadIntegrationMemoryMetadata(client, w.Scope.WorldInstanceID, id)
		if err != nil || node.ID != id || node.Body != "" {
			t.Fatal("legal metadata without body/use permission rejected", err)
		}
		if _, _, _, err = r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, id); err == nil {
			t.Fatal("body permission inferred from metadata")
		}
	}
	t.Run("legal-metadata-no-body-or-use", positive)
	t.Run("metadata-publication-getter", func(t *testing.T) {
		handle, err := r.Store.BeginIntegrationMemoryMetadata(client, w.Scope.WorldInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		if len(handle.Publications()) != 0 {
			t.Error("metadata handle exposes complete published source bodies")
		}
		if handle.Lineage() != nil {
			t.Error("metadata handle exposes full lineage")
		}
		if err = handle.Finish(client); err != nil {
			t.Fatal(err)
		}
	})
	where := " WHERE tenant_id=? AND product_id=? AND caller_subject_id='s66-player' AND data_subject_id='s66-player' AND action='memory.metadata.read' AND allowed_use='PRIVATE_RESPONSE'"
	faults := []struct{ name, sql string }{
		{"missing", "DELETE FROM memory_subject_permissions" + where},
		{"disabled", "UPDATE memory_subject_permissions SET enabled=0" + where},
		{"expired-fault-not-natural", "UPDATE memory_subject_permissions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND" + where},
		{"wrong-caller", "UPDATE memory_subject_permissions SET caller_subject_id='s66-service'" + where},
		{"wrong-data-subject", "UPDATE memory_subject_permissions SET data_subject_id='s66-service'" + where},
		{"wrong-use", "UPDATE memory_subject_permissions SET allowed_use='PUBLIC_RESPONSE'" + where},
	}
	restore := func(t *testing.T) {
		t.Helper()
		// Only this fixture's single metadata permission can have moved among
		// these exact identities. No other account, scope or permission is touched.
		if _, err := r.Store.DB.Exec("DELETE FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id IN ('s66-player','s66-service') AND data_subject_id IN ('s66-player','s66-service') AND action='memory.metadata.read' AND allowed_use IN ('PRIVATE_RESPONSE','PUBLIC_RESPONSE')", tenant, product); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Store.DB.Exec("INSERT INTO memory_subject_permissions(tenant_id,product_id,caller_subject_id,data_subject_id,action,allowed_use,expires_at,enabled) VALUES(?,?,'s66-player','s66-player','memory.metadata.read','PRIVATE_RESPONSE',?,1)", tenant, product, expiry); err != nil {
			t.Fatal(err)
		}
	}
	for _, fault := range faults {
		t.Run("begin-"+fault.name, func(t *testing.T) {
			result, err := r.Store.DB.Exec(fault.sql, tenant, product)
			if err != nil {
				t.Fatal("fault injection failed", err)
			}
			defer restore(t)
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatal("fault did not replace one original row", err)
			}
			if _, _, _, err = r.Memory.ReadIntegrationMemoryMetadata(client, w.Scope.WorldInstanceID, id); err == nil {
				t.Error("metadata Begin accepted invalid subject permission")
			}
		})
		t.Run("finish-"+fault.name, func(t *testing.T) {
			called := false
			r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				called = true
				result, err := tx.ExecContext(ctx, fault.sql, tenant, product)
				if err != nil {
					t.Errorf("fault injection failed: %v", err)
					return err
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Errorf("fault did not replace one original row: %v", err)
				}
				return nil
			}
			defer func() { r.Store.BeforeReadFinish = nil }()
			if _, _, _, err = r.Memory.ReadIntegrationMemoryMetadata(client, w.Scope.WorldInstanceID, id); err == nil {
				t.Error("metadata Finish accepted invalid subject permission")
			}
			if !called {
				t.Error("Finish fault was not reached")
			}
			r.Store.BeforeReadFinish = nil
			positive(t) // The rejected read transaction must return its original row.
		})
	}
}

func TestIntegrationSlice3FixMetadata(t *testing.T) {
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "fix-meta-world", request)
	if e != nil {
		t.Fatal(e)
	}
	session, e := r.Store.ExchangeIntegrationSession(service, "fix-meta-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: []string{"READ", "MEMORY_VIEW"}})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", w.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var aggregate memory.Aggregate
	if json.Unmarshal(raw, &aggregate) != nil {
		t.Fatal("aggregate")
	}
	id, secretBody := "", ""
	for k, n := range aggregate.Nodes {
		id = k
		secretBody = n.Body
		break
	}
	if id == "" || secretBody == "" {
		t.Fatal("real initial knowledge missing")
	}
	for _, phase := range []string{"before", "active", "revoked"} {
		if phase != "before" {
			command := `{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`
			if phase == "revoked" {
				command = `{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`
			}
			if _, e = r.Store.ApplyPlayerSelfConsent(client, "fix-meta-"+phase, json.RawMessage(command)); e != nil {
				t.Fatal(e)
			}
		}
		t.Run(phase, func(t *testing.T) {
			for _, include := range []string{"", "?include=provenance_v1"} {
				q := httptest.NewRequest("GET", "/world/v1/instances/"+w.Scope.WorldInstanceID+"/memories/"+id+include, nil)
				q.Header.Set("Authorization", "Bearer "+session.AccessToken)
				q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
				out := httptest.NewRecorder()
				r.Handler().ServeHTTP(out, q)
				if include == "" {
					var body generated.Memory
					if out.Code != 200 || generated.Decode("Memory", out.Body.Bytes(), &body) != nil {
						t.Errorf("metadata status=%d", out.Code)
						continue
					}
					if body.Content != "" || bytes.Contains(out.Body.Bytes(), []byte(secretBody)) {
						t.Error("default metadata disclosed body")
					}
					var fields map[string]any
					_ = json.Unmarshal(out.Body.Bytes(), &fields)
					if !reflect.DeepEqual(body.AllowedUses, []string{"PRIVATE_RESPONSE"}) || len(body.Sources) != len(aggregate.Nodes[id].Sources) || len(body.Sources) == 0 {
						t.Error("required real metadata absent")
					}
					for _, source := range body.Sources {
						original, ok := aggregate.Sources[source.SourceID]
						if !ok || source.SourceRevision != fmt.Sprint(original.Revision) || source.AcquiredByActorID != aggregate.Nodes[id].ActorID {
							t.Error("source identity substituted")
						}
						found := false
						for _, acq := range aggregate.Acquisitions {
							found = found || acq.SourceID == source.SourceID && acq.ActorID == source.AcquiredByActorID && string(acq.Kind) == source.AcquisitionKind
						}
						if !found {
							t.Error("acquisition substituted")
						}
					}
					for _, key := range []string{"source_epoch_ids", "parent_memory_ids", "occurred_at", "provenance"} {
						if _, ok := fields[key]; ok {
							t.Error("extra provenance", key)
						}
					}
					for _, source := range fields["sources"].([]any) {
						if len(source.(map[string]any)) != 5 {
							t.Error("source exceeds required metadata")
						}
					}
				} else if phase == "active" {
					if out.Code != 200 || generated.Validate("MemoryProvenanceV1", out.Body.Bytes()) != nil {
						t.Error("active provenance denied", out.Code)
					}
				} else if out.Code == 200 {
					t.Error("provenance passed without use")
				}
			}
		})
	}
	for _, fault := range []string{"acquisition", "permission", "audience"} {
		t.Run("metadata-current-"+fault, func(t *testing.T) {
			var damaged memory.Aggregate
			if e := json.Unmarshal(raw, &damaged); e != nil {
				t.Fatal(e)
			}
			switch fault {
			case "acquisition":
				damaged.Acquisitions = nil
			case "permission":
				damaged.Grants = nil
			case "audience":
				for id, source := range damaged.Sources {
					source.Audience = "INVALID"
					damaged.Sources[id] = source
				}
			}
			body, e := json.Marshal(damaged)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", body, w.Scope.WorldInstanceID); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if _, e := r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, w.Scope.WorldInstanceID); e != nil {
					t.Error(e)
				}
			}()
			if _, _, _, e = r.Memory.ReadIntegrationMemoryMetadata(client, w.Scope.WorldInstanceID, id); e == nil {
				t.Error("invisible source metadata accepted")
			}
		})
	}
}

// These mutations are faults inside an owned test transaction, never positive
// issuance fixtures. Rejection must roll back facts, use and all three epochs.
func TestIntegrationSlice3FixConsentBinding(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE integration_self_consent_facts SET use_authority_generation=2",
		"UPDATE integration_self_consent_facts SET principal_ref='foreign-issuer'",
		"UPDATE integration_self_consent_facts SET eligibility_ref='foreign-entitlement'",
		"UPDATE integration_self_consent_facts SET action='RENEW'",
		"UPDATE integration_self_consent_current SET action='RENEW'",
		"UPDATE integration_self_consent_current SET eligibility_ref='foreign-entitlement'",
	} {
		t.Run(mutation, func(t *testing.T) {
			r, client, _, _, _ := sessionConsentFixture(t)
			var before, after [3]uint64
			read := func(v *[3]uint64) {
				t.Helper()
				if e := r.Store.DB.QueryRow("SELECT permission_version,privacy_epoch,view_epoch FROM subject_governance WHERE subject_id='s66-player'").Scan(&v[0], &v[1], &v[2]); e != nil {
					t.Fatal(e)
				}
			}
			read(&before)
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, mutation)
				if e != nil {
					t.Errorf("fault injection failed: %v", e)
				}
				return e
			}
			_, e := r.Store.ApplyPlayerSelfConsent(client, "fix-binding", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
			r.Store.BeforeCommit = nil
			if e == nil {
				t.Error("foreign writer tuple committed")
			}
			read(&after)
			if before != after {
				t.Error("epochs escaped rollback")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_self_consent_current)+(SELECT COUNT(*) FROM integration_self_consent_facts)+(SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use')+(SELECT COUNT(*) FROM integration_receipts WHERE operation='PLAYER_SELF_CONSENT')").Scan(&n); e != nil || n != 0 {
				t.Error("partial writer state", n, e)
			}
		})
	}
	for _, corrupt := range []bool{false, true} {
		name := "new-session-original-replay"
		if corrupt {
			name = "foreign-original-issuance"
		}
		t.Run(name, func(t *testing.T) {
			r, client, service, input, _ := sessionConsentFixture(t)
			command := json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)
			first, e := r.Store.ApplyPlayerSelfConsent(client, "fix-replay", command)
			if e != nil {
				t.Fatal(e)
			}
			second, e := r.Store.ExchangeIntegrationSession(service, "fix-second-session", "integration_r0_v1", input)
			if e != nil {
				t.Fatal(e)
			}
			retainSessionScanMaterial(t, second.AccessToken)
			fresh, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+second.AccessToken, domain.IntegrationClient)
			if e != nil {
				t.Fatal(e)
			}
			if corrupt {
				if _, e = r.Store.DB.Exec("UPDATE integration_self_consent_facts SET principal_ref='foreign-issuer'"); e != nil {
					t.Fatal(e)
				}
			}
			got, e := r.Store.ApplyPlayerSelfConsent(fresh, "fix-replay", command)
			if corrupt {
				if e == nil {
					t.Error("foreign original issuer accepted")
				}
			} else if e != nil || !reflect.DeepEqual(got, first) {
				t.Error("legitimate new Session lost original replay", e)
			}
			var generation uint64
			var facts int
			if e = r.Store.DB.QueryRow("SELECT use_authority_generation,(SELECT COUNT(*) FROM integration_self_consent_facts) FROM integration_self_consent_current").Scan(&generation, &facts); e != nil || generation != 1 || facts != 1 {
				t.Error("replay mutated history", e)
			}
		})
	}
}

func TestIntegrationSlice3FixCreationCurrent(t *testing.T) {
	mutations := []string{
		"UPDATE runtime_instances SET initialization=JSON_SET(CONVERT(initialization USING utf8mb4),'$.Participants',JSON_ARRAY('foreign-player'))",
		"UPDATE runtime_instances SET owner_subject_id='s66-player'",
		"UPDATE runtime_instances SET initial_release_id='foreign-release'",
		"UPDATE integration_world_participants SET creation_receipt='foreign-create'",
		"UPDATE integration_world_participants SET revision=2",
		"UPDATE commit_receipts SET phase='AUTHORIZE' WHERE operation='createInstance'",
		"DELETE FROM commit_receipts WHERE operation='createInstance'",
		"UPDATE admin_results SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.scope.world_instance_id','foreign-world') WHERE schema_name='Instance'",
	}
	for _, mutation := range mutations {
		t.Run(mutation, func(t *testing.T) {
			r, _, service, request, reg := integrationCreationFixture(t)
			w, e := r.World.CreateIntegrationInstance(service, "fix-create-world", request)
			if e != nil {
				t.Fatal(e)
			}
			input := generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: []string{"READ"}}
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, mutation)
				if e != nil {
					t.Errorf("fault injection failed: %v", e)
				}
				return e
			}
			got, e := r.Store.ExchangeIntegrationSession(service, "fix-create-session", "integration_r0_v1", input)
			r.Store.BeforeCommit = nil
			// Even an erroneously accepted result is captured for output scanning.
			retainSessionScanMaterial(t, got.AccessToken)
			if e == nil {
				t.Error("Session accepted changed original CREATE proof")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_principals WHERE kind='CLIENT'").Scan(&n); e != nil || n != 0 {
				t.Error("Session did not roll back", n, e)
			}
		})
	}
	t.Run("issued-client-current", func(t *testing.T) {
		r, client, _, _, session := sessionConsentFixture(t)
		if _, e := r.Store.DB.Exec("UPDATE runtime_instances SET owner_subject_id='s66-player'"); e != nil {
			t.Fatal(e)
		}
		if _, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient); e == nil {
			t.Error("CLIENT accepted changed original owner")
		}
		if _, e := r.Store.ReadPlayerSelfConsent(client); e == nil {
			t.Error("self metadata accepted changed original owner")
		}
		if _, e := r.Store.ApplyPlayerSelfConsent(client, "fix-owner-consent", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e == nil {
			t.Error("self writer accepted changed original owner")
		}
	})
}
