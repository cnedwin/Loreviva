package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// This separate authored fixture explicitly licenses its synthetic text for
// player-private initial import. It never edits the original coffee release.
func integrationCreationFixture(t *testing.T) (*app.Runtime, context.Context, context.Context, generated.CreateInstanceRequest, domain.RegistryCommand) {
	return integrationCreationFixtureInput(t, nil, time.Hour)
}
func integrationCreationFixtureInput(t *testing.T, change func(*generated.DraftInput), ttl time.Duration) (*app.Runtime, context.Context, context.Context, generated.CreateInstanceRequest, domain.RegistryCommand) {
	return integrationCreationFixturePrepared(t, func(_ *app.Runtime, _ context.Context, input *generated.DraftInput) {
		if change != nil {
			change(input)
		}
	}, ttl)
}
func integrationCreationFixturePrepared(t *testing.T, change func(*app.Runtime, context.Context, *generated.DraftInput), ttl time.Duration) (*app.Runtime, context.Context, context.Context, generated.CreateInstanceRequest, domain.RegistryCommand) {
	return integrationCreationFixtureActions(t, change, ttl, []string{"READ"})
}
func integrationCreationFixtureActions(t *testing.T, change func(*app.Runtime, context.Context, *generated.DraftInput), ttl time.Duration, actions []string, customerTTL ...time.Duration) (*app.Runtime, context.Context, context.Context, generated.CreateInstanceRequest, domain.RegistryCommand) {
	t.Helper()
	r, scope, original := runtimeFixture(t)
	operator := runtimeContext(t, r)
	old, e := r.Content.GetRelease(operator, scope, original)
	if e != nil {
		t.Fatal(e)
	}
	input := old.Input
	input.Name = "S66 synthetic reader-licensed fixture"
	for i, b := range input.Objects {
		var object map[string]any
		if e = json.Unmarshal(b, &object); e != nil {
			t.Fatal(e)
		}
		object["source_refs"] = []string{}
		object["audience_policy"] = "PRODUCT_READERS"
		object["rights"] = map[string]any{"origin_kind": "SYNTHETIC", "source_label": "new S66 authored synthetic fixture", "license": "Apache-2.0", "allowed_uses": []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}
		input.Objects[i], e = json.Marshal(object)
		if e != nil {
			t.Fatal(e)
		}
	}
	if change != nil {
		change(r, operator, &input)
	}
	draft, e := r.Content.Create(operator, scope, "WORLD", "", input, "s66-draft")
	if e != nil {
		t.Fatal("authored draft prerequisite", e)
	}
	report, e := r.Content.CheckPublish(operator, scope, string(draft.DraftID), 1, []string{"MOCK_DEMO"}, "s66-check")
	if e != nil || !report.Passed {
		t.Fatal("real publication check prerequisite", report, e)
	}
	release, e := r.Content.Publish(operator, scope, string(draft.DraftID), 1, string(report.ReportID), []string{"MOCK_DEMO"}, "s66-publish")
	if e != nil {
		t.Fatal("real publication prerequisite", e)
	}
	var installation string
	var now time.Time
	if e = r.Store.DB.QueryRow("SELECT installation_id,UTC_TIMESTAMP(6) FROM memory_installation WHERE singleton=1").Scan(&installation, &now); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeIntegrationRegistrar(operator, "s66-registrar", installation); e != nil {
		t.Fatal(e)
	}
	secret := "synthetic-s66-create-service-secret-32"
	path := filepath.Join(t.TempDir(), "service-key")
	if e = os.WriteFile(path, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	customerLifetime := 2 * time.Hour
	if len(customerTTL) == 1 {
		customerLifetime = customerTTL[0]
	}
	customer := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "s66-customer", Customer: &domain.RegisterCustomer{CustomerID: "s66-customer", ServiceSubjectID: "s66-service", ConsumerID: "s66-consumer", SecretHandleRef: path, ExpiresAt: now.Add(customerLifetime)}}
	if _, e = r.Store.ApplyIntegrationRegistry(operator, "s66-customer", customer); e != nil {
		t.Fatal(e)
	}
	player := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: "s66-player", Player: &domain.RegisterPlayerAccess{CustomerID: "s66-customer", ExternalIDBytes: []byte("S66-player-external"), PlayerSubjectID: "s66-player", EntitlementID: "s66-entitlement-one", ReleaseID: string(release.ReleaseID), Actions: append([]string(nil), actions...), ExpiresAt: now.Add(ttl)}}
	if _, e = r.Store.ApplyIntegrationRegistry(operator, "s66-player", player); e != nil {
		t.Fatal("published release precedes player registration", e)
	}
	service, p, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
	if e != nil || p.StableSubject != "s66-service" {
		t.Fatal("real SERVICE prerequisite", e)
	}
	if _, _, e = r.Store.AuthenticateOperator(context.Background(), "Bearer "+secret); e == nil {
		t.Fatal("SERVICE gained Operator identity")
	}
	var use int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='s66-player' AND action='memory.generation.use'").Scan(&use); e != nil || use != 0 {
		t.Fatal("create prerequisite granted positive use", use, e)
	}
	t.Log("real publication, registry, SERVICE and zero positive-use prerequisites established")
	return r, operator, service, generated.CreateInstanceRequest{ProductID: "coffee", ReleaseID: string(release.ReleaseID), Mode: "PRIVATE", InitialParticipants: []string{"s66-player"}, ClientCapabilities: []string{"TEXT"}}, player
}

func TestIntegrationCreatePrivateOwning(t *testing.T) {
	r, _, ctx, request, _ := integrationCreationFixture(t)
	// The interface keeps the RED executable before this owning port exists;
	// the test still calls the real Runtime implementation once it is installed.
	creator, ok := any(r.World).(interface {
		CreateIntegrationInstance(context.Context, string, generated.CreateInstanceRequest) (generated.Instance, error)
	})
	if !ok {
		t.Fatal("real SERVICE prerequisites succeeded: create-only owning port is unavailable")
	}
	out, e := creator.CreateIntegrationInstance(ctx, "s66-create", request)
	if e != nil {
		t.Fatal("real SERVICE create rejected", e)
	}
	if out.Mode != "PRIVATE" || out.InstanceState != "CREATED" || out.Versions.WorldRevision != "0" || out.Versions.PrivacyEpoch != "1" {
		t.Fatal(out)
	}
	var participants, streams, positive int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_world_participants WHERE world_instance_id=?),(SELECT COUNT(*) FROM delivery_streams WHERE world_instance_id=? AND update_seq=0),(SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='s66-player' AND action='memory.generation.use')", out.Scope.WorldInstanceID, out.Scope.WorldInstanceID).Scan(&participants, &streams, &positive); e != nil || participants != 1 || streams != 1 || positive != 0 {
		t.Fatal(participants, streams, positive, e)
	}
}

func TestIntegrationRegistryAppendPreservesOriginalMapping(t *testing.T) {
	r, ctx, _, _, command := integrationCreationFixture(t)
	original, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "s66-player")
	if e != nil {
		t.Fatal(e)
	}
	second := *command.Player
	second.EntitlementID = "s66-entitlement-two"
	second.ExpiresAt = second.ExpiresAt.Add(-30 * time.Minute)
	command.Player = &second
	receipt, e := r.Store.ApplyIntegrationRegistry(ctx, "s66-player-append", command)
	if e != nil {
		t.Fatal("same real mapping append rejected", e)
	}
	x := receipt.RegistrationExpiry
	if x == nil || !x.MappingExpiresAt.Equal(*original.RegistrationExpiry.MappingExpiresAt) || !x.EntitlementExpiresAt.Equal(second.ExpiresAt) || !x.EntitlementExpiresAt.Before(*x.MappingExpiresAt) {
		t.Fatal("original mapping or shorter entitlement changed", receipt)
	}
	read, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "s66-player-append")
	if e != nil || !reflect.DeepEqual(receipt, read) {
		t.Fatal("unequal-expiry receipt read", e)
	}
	replay, e := r.Store.ApplyIntegrationRegistry(ctx, "s66-player-append", command)
	if e != nil || !reflect.DeepEqual(receipt, replay) {
		t.Fatal("original key replay", e)
	}
}
