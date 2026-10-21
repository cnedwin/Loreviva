package integration

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIntegrationRegistryOriginalMappingProof(t *testing.T) {
	r, operator, _, _, command := integrationCreationFixture(t)
	// This is an explicit integrity fault after lawful registration, not a
	// supported identity-change API or a fabricated authority prerequisite.
	changed := []byte("S66-external-integrity-fault")
	result, e := r.Store.DB.Exec("UPDATE integration_external_subjects SET external_id=? WHERE tenant_id='demo-local' AND product_id='coffee' AND customer_id=? AND external_id=?", changed, command.Player.CustomerID, command.Player.ExternalIDBytes)
	if e != nil {
		t.Fatal(e)
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	command.Player.ExternalIDBytes = changed
	command.Player.EntitlementID = "fix1-appended"
	var before, after int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts").Scan(&before); e != nil {
		t.Fatal(e)
	}
	_, e = r.Store.ApplyIntegrationRegistry(operator, "fix1-integrity", command)
	if e == nil {
		t.Fatal("original mapping full external bytes mismatch was accepted")
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts").Scan(&after); e != nil || before != after {
		t.Fatal("rejection wrote receipt", before, after, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_entitlements WHERE entitlement_id='fix1-appended'").Scan(&after); e != nil || after != 0 {
		t.Fatal("rejection wrote entitlement", after, e)
	}
}

func TestIntegrationRegistryOriginalProofBoundaries(t *testing.T) {
	for _, mode := range []string{"customer_identity", "original_actions", "original_release", "final_mapping", "final_customer", "create_preplanned_customer"} {
		t.Run(mode, func(t *testing.T) {
			r, op, service, req, cmd := integrationCreationFixture(t)
			var before, after int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts").Scan(&before); e != nil {
				t.Fatal(e)
			}
			fault := "UPDATE integration_customers SET consumer_id='integrity-other-consumer' WHERE customer_id='s66-customer'"
			switch mode {
			case "original_actions":
				fault = `UPDATE integration_entitlements SET allowed_actions=JSON_ARRAY('READ','MEMORY_VIEW') WHERE entitlement_id='s66-entitlement-one'`
			case "original_release":
				fault = "UPDATE integration_entitlements SET release_id='integrity-other-release' WHERE entitlement_id='s66-entitlement-one'"
			case "final_mapping":
				fault = "UPDATE integration_external_subjects SET external_id='integrity-other-external' WHERE customer_id='s66-customer'"
			}
			if mode == "create_preplanned_customer" {
				p, e := r.Store.PrepareIntegrationCreation(service, "fix1-stale-customer", req)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec(fault); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.RunIntegrationCreation(service, p); e == nil || integrationWorldRows(t, r.Store, p.WorldID()) != 0 {
					t.Fatal("stale original customer accepted", e)
				}
			} else {
				if mode == "final_mapping" || mode == "final_customer" {
					// Real registration Tx, deliberately corrupted only by this
					// test hook: final proof must roll back the fault and new grant.
					r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error { _, e := tx.ExecContext(ctx, fault); return e }
				} else if _, e := r.Store.DB.Exec(fault); e != nil {
					t.Fatal(e)
				}
				cmd.Player.EntitlementID = "fix1-proof-boundary"
				_, e := r.Store.ApplyIntegrationRegistry(op, "fix1-proof-boundary", cmd)
				r.Store.BeforeCommit = nil
				if e == nil {
					t.Fatal("original registration integrity fault accepted", mode)
				}
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_entitlements WHERE entitlement_id='fix1-proof-boundary'").Scan(&after); e != nil || after != 0 {
					t.Fatal("partial grant", after, e)
				}
			}
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts").Scan(&after); e != nil || after != before {
				t.Fatal("partial receipt", before, after, e)
			}
			if mode == "final_mapping" || mode == "final_customer" {
				var external, consumer string
				if e := r.Store.DB.QueryRow("SELECT external_id,consumer_id FROM integration_external_subjects JOIN integration_customers USING(tenant_id,product_id,customer_id) WHERE customer_id='s66-customer'").Scan(&external, &consumer); e != nil || external != "S66-player-external" || consumer != "s66-consumer" {
					t.Fatal("fault escaped rollback", e)
				}
			}
		})
	}
	t.Run("consumed_disabled_rotated_short_replay", func(t *testing.T) {
		r, op, service, req, cmd := integrationCreationFixture(t)
		first, e := r.World.CreateIntegrationInstance(service, "fix1-original-world", req)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.ApplyIntegrationRegistry(op, "fix1-disable-original", domain.RegistryCommand{Kind: "DISABLE_ENTITLEMENT", ProductID: "coffee", TargetID: "s66-entitlement-one", ExpectedRevision: 2, QualifiedTarget: &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}}); e != nil {
			t.Fatal(e)
		}
		var principal string
		if e = r.Store.DB.QueryRow("SELECT principal_id FROM integration_principals WHERE customer_id='s66-customer' AND kind='SERVICE'").Scan(&principal); e != nil {
			t.Fatal(e)
		}
		secret := "synthetic-fix1-rotated-service-secret-32"
		path := filepath.Join(t.TempDir(), "key")
		if e = os.WriteFile(path, []byte(secret), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.ApplyIntegrationRegistry(op, "fix1-rotate", domain.RegistryCommand{Kind: "ROTATE_SERVICE_KEY", ProductID: "coffee", TargetID: principal, ExpectedRevision: 1, Rotation: &domain.RotateServiceKey{CustomerID: "s66-customer", OldPrincipalID: principal, NewPrincipalID: "fix1-rotated", SecretHandleRef: path}}); e != nil {
			t.Fatal(e)
		}
		service, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
		if e != nil {
			t.Fatal(e)
		}
		cmd.Player.EntitlementID = "fix1-after-consumed"
		cmd.Player.ExpiresAt = cmd.Player.ExpiresAt.Add(-30 * time.Minute)
		a, e := r.Store.ApplyIntegrationRegistry(op, "fix1-after-consumed", cmd)
		if e != nil {
			t.Fatal("historical mutable status erased original proof", e)
		}
		b, e := r.Store.ApplyIntegrationRegistry(op, "fix1-after-consumed", cmd)
		if e != nil || !reflect.DeepEqual(a, b) {
			t.Fatal("short original-key replay changed", e)
		}
		read, e := r.Store.ReadIntegrationRegistryReceipt(op, "fix1-after-consumed")
		if e != nil || !reflect.DeepEqual(a, read) || !a.RegistrationExpiry.EntitlementExpiresAt.Before(*a.RegistrationExpiry.MappingExpiresAt) {
			t.Fatal("short expiry receipt", e)
		}
		second, e := r.World.CreateIntegrationInstance(service, "fix1-new-world", req)
		if e != nil || second.Scope.WorldInstanceID == first.Scope.WorldInstanceID {
			t.Fatal("same player second world", e)
		}
		var revision, enabled int
		var world string
		if e = r.Store.DB.QueryRow("SELECT revision,enabled,world_instance_id FROM integration_entitlements WHERE entitlement_id='s66-entitlement-one'").Scan(&revision, &enabled, &world); e != nil || revision != 3 || enabled != 0 || world != first.Scope.WorldInstanceID {
			t.Fatal("original grant changed", revision, enabled, world, e)
		}
	})
}

func TestIntegrationCreatePreplannedOriginalBoundaries(t *testing.T) {
	for _, mode := range []string{"late_grant", "different_payload", "different_key", "real_disable", "cancelled", "closing", "commit_ack"} {
		t.Run(mode, func(t *testing.T) {
			r, op, ctx, req, cmd := integrationCreationFixture(t)
			key := "fix1-prepared-" + mode
			first, e := r.Store.PrepareIntegrationCreation(ctx, key, req)
			if e != nil {
				t.Fatal(e)
			}
			other := req
			otherKey := key
			if mode == "different_payload" {
				other.ClientCapabilities = []string{"TEXT", "AUDIO"}
			}
			if mode == "different_key" {
				otherKey += "-other"
			}
			second, e := r.Store.PrepareIntegrationCreation(ctx, otherKey, other)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "commit_ack" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic ACK loss after actual COMMIT")
				}
			}
			_, e = r.Store.RunIntegrationCreation(ctx, first)
			r.Store.Commit = nil
			if mode == "commit_ack" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if mode == "late_grant" {
				cmd.Player.EntitlementID = "fix1-late-grant"
				if _, e = r.Store.ApplyIntegrationRegistry(op, "fix1-late-grant", cmd); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "real_disable" {
				if _, e = r.Store.ApplyIntegrationRegistry(op, "fix1-after-first-disable", domain.RegistryCommand{Kind: "DISABLE_ENTITLEMENT", ProductID: "coffee", TargetID: "s66-entitlement-one", ExpectedRevision: 2, QualifiedTarget: &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}}); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "cancelled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if mode == "closing" {
				if e = r.Local.Coordinator.BeginClose(context.Background(), "fix1-closing"); e != nil {
					t.Fatal(e)
				}
			}
			var before []byte
			if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", first.WorldID()).Scan(&before); e != nil {
				t.Fatal(e)
			}
			out, e := r.Store.RunIntegrationCreation(ctx, second)
			success := mode == "late_grant" || mode == "commit_ack"
			if success && (e != nil || out.Scope.WorldInstanceID != first.WorldID()) {
				t.Fatal("original result lost", mode, e)
			}
			if !success && e == nil {
				t.Fatal("invalid original request accepted", mode)
			}
			if mode == "closing" {
				if e = r.Local.Coordinator.Drain(context.Background(), "fix1-closing"); e != nil {
					t.Fatal(e)
				}
			}
			var after []byte
			var revision int
			if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", first.WorldID()).Scan(&after); e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("committed initialization changed", e)
			}
			want := 2
			if mode == "real_disable" {
				want = 3
			}
			if e = r.Store.DB.QueryRow("SELECT revision FROM integration_entitlements WHERE entitlement_id='s66-entitlement-one'").Scan(&revision); e != nil || revision != want {
				t.Fatal("extra consumption", revision, e)
			}
			if mode == "late_grant" {
				var unbound bool
				if e = r.Store.DB.QueryRow("SELECT world_instance_id IS NULL AND revision=1 FROM integration_entitlements WHERE entitlement_id='fix1-late-grant'").Scan(&unbound); e != nil || !unbound {
					t.Fatal("reselected late grant", e)
				}
			}
			if mode == "different_key" && integrationWorldRows(t, r.Store, second.WorldID()) != 0 {
				t.Fatal("different-key loser wrote partial world")
			}
		})
	}
}

func TestIntegrationCreateSameKeyPreparedOriginal(t *testing.T) {
	r, _, service, request, _ := integrationCreationFixture(t)
	first, e := r.Store.PrepareIntegrationCreation(service, "fix1-double-prepare", request)
	if e != nil {
		t.Fatal(e)
	}
	second, e := r.Store.PrepareIntegrationCreation(service, "fix1-double-prepare", request)
	if e != nil {
		t.Fatal(e)
	}
	if first.WorldID() != second.WorldID() {
		t.Fatal("same original key changed fixed world")
	}
	// Both plans exist before either commit. No sleep or scheduler race is used.
	a, e := r.Store.RunIntegrationCreation(service, first)
	if e != nil {
		t.Fatal("first original commit", e)
	}
	var before []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", a.Scope.WorldInstanceID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	b, e := r.Store.RunIntegrationCreation(service, second)
	if e != nil {
		t.Fatal("second same-key preplanned request lost original result", e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("original result changed", a, b)
	}
	var after []byte
	var revision, worlds, participants, streams, receipts, results int
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", a.Scope.WorldInstanceID).Scan(&after); e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("original initialization rewritten", e)
	}
	if e = r.Store.DB.QueryRow("SELECT revision FROM integration_entitlements WHERE entitlement_id='s66-entitlement-one'").Scan(&revision); e != nil || revision != 2 {
		t.Fatal("entitlement consumed again", revision, e)
	}
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM world_controls WHERE world_instance_id=?),(SELECT COUNT(*) FROM integration_world_participants WHERE world_instance_id=?),(SELECT COUNT(*) FROM delivery_streams WHERE world_instance_id=?),(SELECT COUNT(*) FROM commit_receipts WHERE commit_key=?),(SELECT COUNT(*) FROM admin_results WHERE result_id=?)", a.Scope.WorldInstanceID, a.Scope.WorldInstanceID, a.Scope.WorldInstanceID, a.Scope.WorldInstanceID, a.Scope.WorldInstanceID).Scan(&worlds, &participants, &streams, &receipts, &results); e != nil || worlds != 1 || participants != 1 || streams != 1 || receipts != 1 || results != 1 {
		t.Fatal(worlds, participants, streams, receipts, results, e)
	}
}

func TestIntegrationRegistryCustomerOffsetCompatibility(t *testing.T) {
	integrationOffsetCompatibility(t, true)
}

func TestIntegrationRegistryMappingOffsetCompatibility(t *testing.T) {
	integrationOffsetCompatibility(t, false)
}

// Each path gets its own genuine registration fixture. A failed append cannot
// hide CREATE, and no SQL changes a deadline or fabricates an issuance proof.
func integrationOffsetCompatibility(t *testing.T, customerOffset bool) {
	for _, path := range []string{"append", "create"} {
		t.Run(path, func(t *testing.T) {
			r, op, service, request, command := integrationCreationFixture(t)
			var now time.Time
			if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
				t.Fatal(e)
			}
			offset := time.FixedZone("probe-plus-eight", 8*60*60)
			expires := now.Add(time.Hour)
			customer := "s66-customer"
			if customerOffset {
				customer = "offset-customer"
				secret := "synthetic-offset-customer-service-secret-32"
				file := filepath.Join(t.TempDir(), "offset-key")
				if e := os.WriteFile(file, []byte(secret), 0600); e != nil {
					t.Fatal(e)
				}
				deadline := now.Add(2 * time.Hour).In(offset)
				cmd := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: customer, Customer: &domain.RegisterCustomer{CustomerID: customer, ServiceSubjectID: "offset-service", ConsumerID: "offset-consumer", SecretHandleRef: file, ExpiresAt: deadline}}
				receipt, e := r.Store.ApplyIntegrationRegistry(op, "offset-customer", cmd)
				if e != nil {
					t.Fatal("nonzero-offset customer registration prerequisite", e)
				}
				if receipt.RegistrationExpiry == nil || !receipt.RegistrationExpiry.CustomerExpiresAt.Equal(deadline) {
					t.Fatal("customer absolute expiry changed")
				}
				t.Logf("real customer COMMITTED: request=%s receipt=%s equal_instant=true", deadline.Format(time.RFC3339Nano), receipt.RegistrationExpiry.CustomerExpiresAt.Format(time.RFC3339Nano))
				service, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
				if e != nil {
					t.Fatal("real current SERVICE prerequisite", e)
				}
				if _, _, e = r.Store.AuthenticateOperator(context.Background(), "Bearer "+secret); e == nil {
					t.Fatal("SERVICE became Operator")
				}
			} else {
				expires = expires.In(offset)
			}
			command.TargetID = "offset-player"
			command.Player = &domain.RegisterPlayerAccess{CustomerID: customer, ExternalIDBytes: []byte("offset-external"), PlayerSubjectID: "offset-player", EntitlementID: "offset-original", ReleaseID: request.ReleaseID, Actions: []string{"READ"}, ExpiresAt: expires}
			receipt, e := r.Store.ApplyIntegrationRegistry(op, "offset-player", command)
			if e != nil {
				t.Fatal("real mapping registration prerequisite", e)
			}
			if receipt.RegistrationExpiry == nil || receipt.RegistrationExpiry.MappingExpiresAt == nil || !receipt.RegistrationExpiry.MappingExpiresAt.Equal(expires) {
				t.Fatal("mapping absolute expiry changed")
			}
			t.Logf("real mapping COMMITTED: request=%s receipt=%s equal_instant=true", expires.Format(time.RFC3339Nano), receipt.RegistrationExpiry.MappingExpiresAt.Format(time.RFC3339Nano))
			var positive int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='offset-player' AND action='memory.generation.use'").Scan(&positive); e != nil || positive != 0 {
				t.Fatal("positive use prerequisite changed", e)
			}
			t.Log("genuine publication/registration/current SERVICE and zero generation use ready; probing independent owning path", path)
			if path == "append" {
				command.Player.EntitlementID = "offset-appended"
				command.Player.ExpiresAt = now.Add(30 * time.Minute)
				if _, e = r.Store.ApplyIntegrationRegistry(op, "offset-append", command); e != nil {
					t.Fatal("legal absolute expiry representation rejected by R1", e)
				}
			} else {
				request.InitialParticipants = []string{"offset-player"}
				if _, e = r.World.CreateIntegrationInstance(service, "offset-create", request); e != nil {
					t.Fatal("legal absolute expiry representation rejected by CREATE", e)
				}
			}
		})
	}
}

// A changed offset at the same instant must recover a newly issued V2 result;
// changed instants and sub-microsecond input must never issue or renew anything.
func TestIntegrationRegistryV2CanonicalReplay(t *testing.T) {
	for _, kind := range []string{"customer", "player"} {
		t.Run(kind, func(t *testing.T) {
			r, op, _, request, command := integrationCreationFixture(t)
			var now time.Time
			if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
				t.Fatal(e)
			}
			deadline := now.Add(30 * time.Minute).In(time.FixedZone("caller-offset", 8*3600))
			if kind == "customer" {
				key := filepath.Join(t.TempDir(), "v2-service-key")
				if e := os.WriteFile(key, []byte("synthetic-v2-service-key-at-least32"), 0600); e != nil {
					t.Fatal(e)
				}
				command = domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "v2-customer", Customer: &domain.RegisterCustomer{CustomerID: "v2-customer", ServiceSubjectID: "v2-service", ConsumerID: "v2-consumer", SecretHandleRef: key, ExpiresAt: deadline}}
			} else {
				command.TargetID = "v2-player"
				command.Player = &domain.RegisterPlayerAccess{CustomerID: "s66-customer", ExternalIDBytes: []byte("v2-external"), PlayerSubjectID: "v2-player", EntitlementID: "v2-entitlement", ReleaseID: request.ReleaseID, Actions: []string{"READ"}, ExpiresAt: deadline}
			}
			expiry := func() *time.Time {
				if command.Customer != nil {
					return &command.Customer.ExpiresAt
				}
				return &command.Player.ExpiresAt
			}
			counts := func() [8]int {
				var n [8]int
				if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_receipts),(SELECT COUNT(*) FROM integration_customers),(SELECT COUNT(*) FROM integration_principals),(SELECT COUNT(*) FROM integration_external_subjects),(SELECT COUNT(*) FROM integration_entitlements),(SELECT COUNT(*) FROM subject_governance),(SELECT COUNT(*) FROM integration_credential_hashes),(SELECT COUNT(*) FROM memory_subject_permissions)").Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6], &n[7]); e != nil {
					t.Fatal(e)
				}
				return n
			}
			before := counts()
			*expiry() = deadline.Add(time.Nanosecond)
			if _, e := r.Store.ApplyIntegrationRegistry(op, "v2-invalid-precision", command); !errors.Is(e, store.ErrDenied) {
				t.Fatal("invalid precision accepted", e)
			}
			if counts() != before || *expiry() != deadline.Add(time.Nanosecond) {
				t.Fatal("precision failure wrote rows or changed caller")
			}
			*expiry() = deadline
			original, e := r.Store.ApplyIntegrationRegistry(op, "v2-original", command)
			if e != nil {
				t.Fatal(e)
			}
			if *expiry() != deadline {
				t.Fatal("normalization mutated caller expiry")
			}
			if command.Player != nil && (string(command.Player.ExternalIDBytes) != "v2-external" || !reflect.DeepEqual(command.Player.Actions, []string{"READ"})) {
				t.Fatal("normalization mutated caller slices")
			}
			after := counts()
			*expiry() = deadline.In(time.FixedZone("equivalent-offset", -5*3600))
			caller := *expiry()
			replayed, e := r.Store.ApplyIntegrationRegistry(op, "v2-original", command)
			if e != nil {
				t.Fatal("same instant changed-offset V2 replay denied", e)
			}
			if !reflect.DeepEqual(original, replayed) || *expiry() != caller || counts() != after {
				t.Fatal("replay changed result, caller, or rows")
			}
			*expiry() = caller.Add(time.Microsecond)
			if _, e = r.Store.ApplyIntegrationRegistry(op, "v2-original", command); !errors.Is(e, store.ErrConflict) {
				t.Fatal("different instant did not conflict", e)
			}
			read, e := r.Store.ReadIntegrationRegistryReceipt(op, "v2-original")
			if e != nil || !reflect.DeepEqual(original, read) || counts() != after {
				t.Fatal("replay rewrote original", e)
			}
			*expiry() = caller
			if _, e = r.Store.ApplyIntegrationRegistry(op, "v2-disable-registrar", domain.RegistryCommand{Kind: "DISABLE_REGISTRAR", ProductID: "coffee", TargetID: "owner", ExpectedRevision: 1}); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ApplyIntegrationRegistry(op, "v2-original", command); !errors.Is(e, store.ErrDenied) {
				t.Fatal("replay bypassed current registrar", e)
			}
			if _, e = r.Store.ReadIntegrationRegistryReceipt(op, "v2-original"); !errors.Is(e, store.ErrDenied) {
				t.Fatal("query bypassed current registrar", e)
			}
		})
	}
}
