package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Only the explicitly diagnosed positive bootstrap fixtures call this helper.
// It creates the real local installation/CONFIG participant and grants nothing.
func initializeTestConfiguration(t *testing.T, s *store.Store, filesRoot string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if e := app.InitializeLocal(ctx, s, root, filesRoot); e != nil {
		t.Fatal(e)
	}
	local, e := app.OpenLocal(ctx, s, root, filesRoot)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := local.Participant.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
}

func TestIntegrationFreshBootstrapReservesBearer(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_credential_hashes h JOIN operator_credentials c ON c.token_sha256=h.token_hash JOIN memory_installation i ON i.installation_id=h.installation_id WHERE h.kind='OPERATOR' AND h.principal_id=c.credential_id AND c.credential_id='owner'").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 1 {
		t.Fatal("fresh bootstrap omitted the global original bearer reservation", n)
	}
}

func TestIntegrationConfiguredCheckAndRegistryReadiness(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	if e := r.Auth.Check(ctx); e != nil {
		t.Fatal(e)
	}
	if e := r.Auth.RegistryReady(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE integration_credential_hashes SET installation_id='foreign' WHERE principal_id='owner'"); e != nil {
		t.Fatal(e)
	}
	if e := r.Auth.Check(ctx); e == nil {
		t.Error("configured credential ignored global binding")
	}
	if e := r.Auth.RegistryReady(ctx); e == nil {
		t.Error("registry ready without any globally registered credential")
	}
}

func TestIntegrationRegistryRejectsTaskIssuerAsDirectRegistrar(t *testing.T) {
	for _, port := range []string{"initialize", "command", "receipt"} {
		t.Run(port, func(t *testing.T) {
			r, draft, request := candidateHTTPFixture(t)
			direct := runtimeContext(t, r)
			var installation string
			var now time.Time
			if e := r.Store.DB.QueryRow("SELECT installation_id,UTC_TIMESTAMP(6) FROM memory_installation").Scan(&installation, &now); e != nil {
				t.Fatal(e)
			}
			if port != "initialize" {
				if _, e := r.Store.InitializeIntegrationRegistrar(direct, "direct-registrar", installation); e != nil {
					t.Fatal(e)
				}
			}
			accepted, e := r.Store.AcceptDraftCandidate(direct, r.Auth.Scope(), draft, "issuer-boundary", request)
			if e != nil {
				t.Fatal(e)
			}
			lease, e := r.Store.ClaimRegistered(context.Background(), "registry-proof-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE", TaskID: string(accepted.TaskID)}})
			if e != nil {
				t.Fatal(e)
			}
			issuer, e := r.Store.EnterDraftTaskIssuer(context.Background(), lease.TaskLease, "registry-proof-worker")
			if e != nil {
				t.Fatal("real original issuer precondition", e)
			}
			ref := filepath.Join(t.TempDir(), "key")
			if e = os.WriteFile(ref, []byte("synthetic-direct-registry-key-012345678901"), 0600); e != nil {
				t.Fatal(e)
			}
			command := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "direct-customer", Customer: &domain.RegisterCustomer{CustomerID: "direct-customer", ServiceSubjectID: "direct-service", ConsumerID: "direct-consumer", SecretHandleRef: ref, ExpiresAt: now.Add(time.Hour)}}
			call := func(ctx context.Context) error {
				switch port {
				case "initialize":
					_, e := r.Store.InitializeIntegrationRegistrar(ctx, "direct-registrar", installation)
					return e
				case "command":
					_, e := r.Store.ApplyIntegrationRegistry(ctx, "direct-customer", command)
					return e
				default:
					_, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "direct-registrar")
					return e
				}
			}
			if e = call(issuer); e == nil {
				t.Fatal("job-bound original proof became direct registrar", port)
			}
			if e = call(direct); e != nil {
				t.Fatal("real direct operator lost registry authority", e)
			}
			if _, e = r.Store.EnterDraftTaskIssuer(context.Background(), lease.TaskLease, "registry-proof-worker"); e != nil {
				t.Fatal("registry rejection damaged original task proof", e)
			}
		})
	}
}

func TestIntegrationRegistryConcurrentHashAndCAS(t *testing.T) {
	r, ctx, _, p := integrationRegisteredCustomer(t)
	secret := "synthetic-concurrent-global-key-0123456789012345"
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	var now time.Time
	if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		t.Fatal(e)
	}
	command := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "racing-customer", Customer: &domain.RegisterCustomer{CustomerID: "racing-customer", ServiceSubjectID: "racing-service", ConsumerID: "racing-consumer", ExpiresAt: now.Add(time.Hour), SecretHandleRef: ref}}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, e := r.Store.ApplyIntegrationRegistry(ctx, "race-service", command); results <- e }()
	go func() {
		<-start
		results <- r.Store.RegisterLocalCredential(ctx, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, "race-operator", secret)
	}()
	close(start)
	passed, conflict := 0, 0
	for i := 0; i < 2; i++ {
		e := <-results
		if e == nil {
			passed++
		} else if errors.Is(e, store.ErrConflict) {
			conflict++
		} else {
			t.Fatal("unexpected global-hash race outcome", e)
		}
	}
	if passed != 1 || conflict != 1 {
		t.Fatal("cross-kind bearer collision", passed, conflict)
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_credential_hashes WHERE token_hash=UNHEX(SHA2(?,256))", secret).Scan(&n); e != nil || n != 1 {
		t.Fatal("reservation lost", n, e)
	}
	start = make(chan struct{})
	passed, conflict = 0, 0
	for _, key := range []string{"cas-one", "cas-two"} {
		go func(key string) {
			<-start
			_, e := r.Store.ApplyIntegrationRegistry(ctx, key, domain.RegistryCommand{Kind: "DISABLE_CUSTOMER", ProductID: "coffee", TargetID: p.Customer, ExpectedRevision: 1})
			results <- e
		}(key)
	}
	close(start)
	for i := 0; i < 2; i++ {
		e := <-results
		if e == nil {
			passed++
		} else if errors.Is(e, store.ErrConflict) {
			conflict++
		} else {
			t.Fatal("unexpected CAS race outcome", e)
		}
	}
	if passed != 1 || conflict != 1 {
		t.Fatal("revision CAS lost", passed, conflict)
	}
	var revision int
	if e := r.Store.DB.QueryRow("SELECT revision FROM integration_customers WHERE customer_id=?", p.Customer).Scan(&revision); e != nil || revision != 2 {
		t.Fatal(revision, e)
	}
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts WHERE original_key IN('cas-one','cas-two')").Scan(&n); e != nil || n != 1 {
		t.Fatal("failed CAS left receipt", n, e)
	}
}

func TestIntegrationRegistryExternalBytesAndDrain(t *testing.T) {
	r, ctx, _, p := integrationRegisteredCustomer(t)
	var release string
	if e := r.Store.DB.QueryRow("SELECT release_id FROM releases LIMIT 1").Scan(&release); e != nil {
		t.Fatal(e)
	}
	external := []byte(strings.Repeat("🙂", 128))
	command := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: "wide-player", Player: &domain.RegisterPlayerAccess{CustomerID: p.Customer, ExternalIDBytes: external, PlayerSubjectID: "wide-player", EntitlementID: "wide-entitlement", ReleaseID: release, Actions: []string{"READ"}, ExpiresAt: p.ExpiresAt}}
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "wide-register", command); e != nil {
		t.Fatal("valid 512-byte identity could not use bounded lock", e)
	}
	var stored []byte
	if e := r.Store.DB.QueryRow("SELECT external_id FROM integration_external_subjects WHERE player_subject_id='wide-player'").Scan(&stored); e != nil || !reflect.DeepEqual(stored, external) {
		t.Fatal("external identity truncated or replaced with hash", e)
	}
	command.Player.ExternalIDBytes = append(external, []byte("🙂")...)
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "too-wide", command); e == nil {
		t.Fatal("external identity limit bypassed")
	}
	if e := r.Local.Coordinator.BeginClose(ctx, "registry-drain"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "closed-disable", domain.RegistryCommand{Kind: "DISABLE_CUSTOMER", ProductID: "coffee", TargetID: p.Customer, ExpectedRevision: 1}); e == nil {
		t.Fatal("registry bypassed CLOSING")
	}
	if _, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "customer"); e == nil {
		t.Fatal("registry receipt bypassed CLOSING")
	}
	if _, e := r.Store.InitializeIntegrationRegistrar(ctx, "closed-init", "untrusted"); e == nil {
		t.Fatal("registrar init bypassed CLOSING")
	}
	if e := r.Local.Coordinator.Drain(ctx, "registry-drain"); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_receipts WHERE original_key LIKE 'closed-%'").Scan(&n); e != nil || n != 0 {
		t.Fatal("closed writer left receipt", n, e)
	}
}

func TestIntegrationRegistryReceiptRejectsBrokenMetadata(t *testing.T) {
	r, ctx, _, _ := integrationRegisteredCustomer(t)
	var original []byte
	if e := r.Store.DB.QueryRow("SELECT result_metadata FROM integration_receipts WHERE original_key='customer'").Scan(&original); e != nil {
		t.Fatal(e)
	}
	for _, fault := range []string{"missing_expiry", "wrong_revision", "wrong_hash", "extra_field"} {
		t.Run(fault, func(t *testing.T) {
			var v map[string]any
			if e := json.Unmarshal(original, &v); e != nil {
				t.Fatal(e)
			}
			switch fault {
			case "missing_expiry":
				delete(v, "registration_expiry")
			case "wrong_revision":
				v["revision"] = 2
			case "wrong_hash":
				v["hash"] = "different"
			case "extra_field":
				v["secret"] = "synthetic-forbidden-metadata"
			}
			b, e := json.Marshal(v)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE integration_receipts SET result_metadata=? WHERE original_key='customer'", b); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ReadIntegrationRegistryReceipt(ctx, "customer"); e == nil {
				t.Error("broken durable metadata accepted", fault)
			}
			if _, e = r.Store.DB.Exec("UPDATE integration_receipts SET result_metadata=? WHERE original_key='customer'", original); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestIntegrationRevokeClientMetadataRefusals(t *testing.T) {
	r, ctx, _, service := integrationRegisteredCustomer(t)
	command := domain.RegistryCommand{Kind: "REVOKE_CLIENT_SESSION", ProductID: "coffee", TargetID: service.PrincipalID, ExpectedRevision: 1}
	for _, tc := range []struct {
		name, key string
		ctx       context.Context
		command   domain.RegistryCommand
		conflict  bool
	}{
		{"wrong_kind", "wrong-kind", ctx, command, false},
		{"missing_principal", "no-proof", context.Background(), command, false},
		{"missing_session", "missing", ctx, domain.RegistryCommand{Kind: command.Kind, ProductID: "coffee", TargetID: "unissued-client", ExpectedRevision: 1}, false},
		{"foreign_scope", "foreign", ctx, domain.RegistryCommand{Kind: command.Kind, ProductID: "other", TargetID: service.PrincipalID, ExpectedRevision: 1}, false},
		{"original_key_conflict", "customer", ctx, command, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := r.Store.ApplyIntegrationRegistry(tc.ctx, tc.key, tc.command)
			if e == nil || tc.conflict && !errors.Is(e, store.ErrConflict) {
				t.Fatal("client-only metadata refusal", e)
			}
		})
	}
	var revision, revoked, receipts, clients, hashes int
	if e := r.Store.DB.QueryRow("SELECT revision,revoked FROM integration_principals WHERE principal_id=?", service.PrincipalID).Scan(&revision, &revoked); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_receipts),(SELECT COUNT(*) FROM integration_principals WHERE kind='CLIENT'),(SELECT COUNT(*) FROM integration_credential_hashes)").Scan(&receipts, &clients, &hashes); e != nil {
		t.Fatal(e)
	}
	if revision != 1 || revoked != 0 || receipts != 2 || clients != 0 || hashes != 2 {
		t.Fatal("negative revoke wrote state", revision, revoked, receipts, clients, hashes)
	}
}

func TestIntegrationRegistryExpiryRollbackAndACK(t *testing.T) {
	r, ctx, _, _ := integrationRegisteredCustomer(t)
	var now, ceiling time.Time
	var release string
	if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6),expires_at FROM integration_customers WHERE customer_id='customer-one'").Scan(&now, &ceiling); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT release_id FROM content_releases WHERE tenant_id='demo-local' AND product_id='coffee'").Scan(&release); e != nil {
		t.Fatal(e)
	}
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte("synthetic-registry-expiry-bounds-key32"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name   string
		expiry time.Time
	}{{"past", now.Add(-time.Second)}, {"over-30-days", now.Add(31 * 24 * time.Hour)}, {"null", time.Time{}}, {"sub-microsecond", now.Add(time.Hour + time.Nanosecond)}} {
		t.Run(tc.name, func(t *testing.T) {
			c := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "invalid-customer", Customer: &domain.RegisterCustomer{CustomerID: "invalid-customer", ServiceSubjectID: "invalid-service", ConsumerID: "invalid-consumer", SecretHandleRef: ref, ExpiresAt: tc.expiry}}
			if _, e := r.Store.ApplyIntegrationRegistry(ctx, tc.name, c); e == nil {
				t.Fatal("invalid durable expiry accepted")
			}
			var n int
			if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_customers WHERE customer_id='invalid-customer')+(SELECT COUNT(*) FROM subject_governance WHERE subject_id='invalid-service')+(SELECT COUNT(*) FROM integration_receipts WHERE original_key=?)", tc.name).Scan(&n); e != nil || n != 0 {
				t.Fatal("invalid expiry wrote facts", n, e)
			}
		})
	}
	p := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: "bounded-player", Player: &domain.RegisterPlayerAccess{CustomerID: "customer-one", ExternalIDBytes: []byte("bounded-player"), PlayerSubjectID: "bounded-player", EntitlementID: "bounded-entitlement", ReleaseID: release, Actions: []string{"READ"}, ExpiresAt: ceiling.Add(time.Microsecond)}}
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "beyond-customer", p); e == nil {
		t.Fatal("entitlement exceeded actual customer expiry")
	}
	p.Player.ExpiresAt = ceiling
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	_, e := r.Store.ApplyIntegrationRegistry(ctx, "original-player", p)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("lost ACK misreported", e)
	}
	a, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "original-player")
	if e != nil {
		t.Fatal(e)
	}
	b, e := r.Store.ApplyIntegrationRegistry(ctx, "original-player", p)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("ACK recovery did not return original", e)
	}
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_external_subjects WHERE player_subject_id='bounded-player'").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if a.RegistrationExpiry == nil || !a.RegistrationExpiry.CustomerExpiresAt.Equal(ceiling) || !a.RegistrationExpiry.MappingExpiresAt.Equal(ceiling) || !a.RegistrationExpiry.EntitlementExpiresAt.Equal(ceiling) {
		t.Fatal(a)
	}
}

func TestIntegrationRegistrarCurrentAuthorityAndRollback(t *testing.T) {
	for _, mode := range []string{"maintenance", "bootstrap-proof", "current-registrar", "current-credential", "self-disable"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			ctx := runtimeContext(t, r)
			var installation string
			if e := r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation").Scan(&installation); e != nil {
				t.Fatal(e)
			}
			if mode == "maintenance" {
				if _, e := r.Store.DB.Exec("UPDATE studio_operator_grants SET enabled=0 WHERE action='maintenance.manage'"); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "bootstrap-proof" {
				if _, e := r.Store.DB.Exec("DELETE FROM r0_generation_consent"); e != nil {
					t.Fatal(e)
				}
			}
			_, e := r.Store.InitializeIntegrationRegistrar(ctx, "registrar", installation)
			if mode == "maintenance" || mode == "bootstrap-proof" {
				if e == nil {
					t.Fatal("unproved registrar initialized")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if mode == "self-disable" {
				if _, e = r.Store.ApplyIntegrationRegistry(ctx, "self-disable", domain.RegistryCommand{Kind: "DISABLE_REGISTRAR", ProductID: "coffee", TargetID: "owner", ExpectedRevision: 1}); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.InitializeIntegrationRegistrar(ctx, "new-registrar", installation); e == nil {
					t.Fatal("disabled registrar reissued")
				}
				return
			}
			ref := filepath.Join(t.TempDir(), "key")
			if e = os.WriteFile(ref, []byte("synthetic-registry-final-current-key32"), 0600); e != nil {
				t.Fatal(e)
			}
			var now time.Time
			if e = r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
				t.Fatal(e)
			}
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				q := "UPDATE integration_registrars SET enabled=0 WHERE credential_id='owner'"
				if mode == "current-credential" {
					q = "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"
				}
				_, err := tx.ExecContext(ctx, q)
				return err
			}
			_, e = r.Store.ApplyIntegrationRegistry(ctx, "denied-final", domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "denied-customer", Customer: &domain.RegisterCustomer{CustomerID: "denied-customer", ServiceSubjectID: "denied-service", ConsumerID: "denied-consumer", SecretHandleRef: ref, ExpiresAt: now.Add(time.Hour)}})
			r.Store.BeforeCommit = nil
			if e == nil {
				t.Fatal("final authority change ignored")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_customers)+(SELECT COUNT(*) FROM integration_principals)+(SELECT COUNT(*) FROM integration_credential_hashes WHERE kind='SERVICE')+(SELECT COUNT(*) FROM subject_governance WHERE subject_id='denied-service')+(SELECT COUNT(*) FROM integration_receipts WHERE original_key='denied-final')").Scan(&n); e != nil || n != 0 {
				t.Fatal("denied registration partially committed", n, e)
			}
		})
	}
}

func TestIntegrationRegistryQualifiedDisable(t *testing.T) {
	r, ctx, _, _ := integrationRegisteredCustomer(t)
	var expiry time.Time
	var release string
	if e := r.Store.DB.QueryRow("SELECT expires_at FROM integration_customers WHERE customer_id='customer-one'").Scan(&expiry); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT release_id FROM content_releases WHERE tenant_id='demo-local' AND product_id='coffee'").Scan(&release); e != nil {
		t.Fatal(e)
	}
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte("synthetic-registry-second-customer-key32"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "customer-two", domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "customer-two", Customer: &domain.RegisterCustomer{CustomerID: "customer-two", ServiceSubjectID: "service-two", ConsumerID: "consumer-two", SecretHandleRef: ref, ExpiresAt: expiry}}); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"one", "two"} {
		c := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: "player-" + suffix, Player: &domain.RegisterPlayerAccess{CustomerID: "customer-" + suffix, ExternalIDBytes: []byte("same-external"), PlayerSubjectID: "player-" + suffix, EntitlementID: "same-entitlement", ReleaseID: release, Actions: []string{"READ", "MEMORY_VIEW"}, ExpiresAt: expiry}}
		if _, e := r.Store.ApplyIntegrationRegistry(ctx, "player-"+suffix, c); e != nil {
			t.Fatal(e)
		}
	}
	for _, kind := range []string{"DISABLE_ENTITLEMENT", "DISABLE_PLAYER_MAPPING"} {
		target := "same-entitlement"
		if kind == "DISABLE_PLAYER_MAPPING" {
			target = "player-one"
		}
		c := domain.RegistryCommand{Kind: kind, ProductID: "coffee", TargetID: target, ExpectedRevision: 1}
		if _, e := r.Store.ApplyIntegrationRegistry(ctx, "missing-"+kind, c); e == nil {
			t.Fatal("missing customer accepted")
		}
		c.QualifiedTarget = &domain.RegistryQualifiedTarget{CustomerID: "missing"}
		if _, e := r.Store.ApplyIntegrationRegistry(ctx, "wrong-"+kind, c); e == nil {
			t.Fatal("unqualified target accepted")
		}
		c.QualifiedTarget.CustomerID = "customer-one"
		a, e := r.Store.ApplyIntegrationRegistry(ctx, "exact-"+kind, c)
		if e != nil {
			t.Fatal("qualified disable rejected", kind, e)
		}
		if a.CustomerID != "customer-one" || a.Revision != 2 {
			t.Fatal(a)
		}
		c.QualifiedTarget.CustomerID = "customer-two"
		if _, e = r.Store.ApplyIntegrationRegistry(ctx, "exact-"+kind, c); e == nil {
			t.Fatal("original key crossed customer")
		}
	}
	var one, two int
	if e := r.Store.DB.QueryRow("SELECT SUM(customer_id='customer-one' AND enabled=0),SUM(customer_id='customer-two' AND enabled=1) FROM integration_entitlements WHERE entitlement_id='same-entitlement'").Scan(&one, &two); e != nil || one != 1 || two != 1 {
		t.Fatal(one, two, e)
	}
	if e := r.Store.DB.QueryRow("SELECT SUM(customer_id='customer-one' AND enabled=0),SUM(customer_id='customer-two' AND enabled=1) FROM integration_external_subjects WHERE external_id='same-external'").Scan(&one, &two); e != nil || one != 1 || two != 1 {
		t.Fatal(one, two, e)
	}
}

func TestIntegrationOperatorTombstoneAndConfigState(t *testing.T) {
	for _, mode := range []string{"tombstone", "config-state"} {
		t.Run(mode, func(t *testing.T) {
			r, scope, _ := runtimeFixture(t)
			ctx := runtimeContext(t, r)
			secret := "synthetic-registry-operator-temporary32"
			if e := r.Store.RegisterLocalCredential(ctx, scope, "temporary", secret); e != nil {
				t.Fatal(e)
			}
			if mode == "tombstone" {
				if _, e := r.Store.DB.Exec("DELETE FROM studio_credential_state WHERE credential_id='temporary'"); e != nil {
					t.Fatal(e)
				}
				if _, e := r.Store.DB.Exec("DELETE FROM operator_credentials WHERE credential_id='temporary'"); e != nil {
					t.Fatal(e)
				}
				if e := r.Store.RegisterLocalCredential(ctx, scope, "temporary", secret); e == nil {
					t.Fatal("tombstone alone recreated a deleted credential")
				}
				return
			}
			guard := r.Store.ConfigGuard
			r.Store.ConfigGuard = nil
			request := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"TENANT","tenant_id":"demo-local"},"kind":"SET_CREDENTIAL_STATE","expected_revision":"1","credential_id":"temporary","subject_id":"creator","enabled":false,"expires_at":null}`)
			_, e := r.Store.ApplyMember(ctx, "disable-without-config", request)
			r.Store.ConfigGuard = guard
			if e == nil {
				t.Fatal("credential state mutated without registered CONFIG admission")
			}
			if _, e = r.Store.ApplyMember(ctx, "disable-current", request); e != nil {
				t.Fatal(e)
			}
			if _, _, e = r.Store.AuthenticateOperator(context.Background(), "Bearer "+secret); e == nil {
				t.Fatal("disabled credential authenticated")
			}
		})
	}
}

func integrationRegisteredCustomer(t *testing.T) (*app.Runtime, context.Context, string, domain.IntegrationPrincipal) {
	t.Helper()
	r, _, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	var installation string
	var now time.Time
	if e := r.Store.DB.QueryRow("SELECT installation_id,UTC_TIMESTAMP(6) FROM memory_installation WHERE singleton=1").Scan(&installation, &now); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.InitializeIntegrationRegistrar(ctx, "registrar", installation); e != nil {
		t.Fatal(e)
	}
	secret := "synthetic-registry-service-original-key32"
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	c := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "customer-one", Customer: &domain.RegisterCustomer{CustomerID: "customer-one", ServiceSubjectID: "service-one", ConsumerID: "consumer-one", SecretHandleRef: ref, ExpiresAt: now.Add(time.Hour)}}
	if _, e := r.Store.ApplyIntegrationRegistry(ctx, "customer", c); e != nil {
		t.Fatal(e)
	}
	_, p, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
	if e != nil {
		t.Fatal(e)
	}
	return r, ctx, secret, p
}
func TestIntegrationRegistryRotationDisableAndTombstones(t *testing.T) {
	r, ctx, secret, p := integrationRegisteredCustomer(t)
	newSecret := "synthetic-registry-service-rotated-key32"
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte(newSecret), 0600); e != nil {
		t.Fatal(e)
	}
	c := domain.RegistryCommand{Kind: "ROTATE_SERVICE_KEY", ProductID: "coffee", TargetID: p.PrincipalID, ExpectedRevision: 1, Rotation: &domain.RotateServiceKey{CustomerID: p.Customer, OldPrincipalID: p.PrincipalID, NewPrincipalID: "rotated-service", SecretHandleRef: ref}}
	a, e := r.Store.ApplyIntegrationRegistry(ctx, "rotate", c)
	if e != nil {
		t.Fatal("real service rotation refused", e)
	}
	if a.Revision != 2 || a.RegistrationExpiry != nil {
		t.Fatal(a)
	}
	if _, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService); e == nil {
		t.Fatal("old service key remained live")
	}
	_, newP, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+newSecret, domain.IntegrationService)
	if e != nil || newP.StableSubject != p.StableSubject || newP.Customer != p.Customer || !newP.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatal(newP, e)
	}
	b, e := r.Store.ApplyIntegrationRegistry(ctx, "rotate", c)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("rotation replay altered identity", e)
	}
	if e = r.Store.RegisterLocalCredential(ctx, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, "reused-service", secret); e == nil {
		t.Fatal("retired Service secret reminted as Operator")
	}
	d := domain.RegistryCommand{Kind: "DISABLE_CUSTOMER", ProductID: "coffee", TargetID: p.Customer, ExpectedRevision: 1}
	if _, e = r.Store.ApplyIntegrationRegistry(ctx, "disable-stale", domain.RegistryCommand{Kind: d.Kind, ProductID: d.ProductID, TargetID: d.TargetID, ExpectedRevision: 2}); e == nil {
		t.Fatal("stale customer revision accepted")
	}
	if _, e = r.Store.ApplyIntegrationRegistry(ctx, "disable", d); e != nil {
		t.Fatal(e)
	}
	if _, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+newSecret, domain.IntegrationService); e == nil {
		t.Fatal("disabled customer authenticated")
	}
	if _, e = r.Store.ReadIntegrationRegistryReceipt(ctx, "customer"); e != nil {
		t.Fatal("safe receipt lost after disable", e)
	}
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_credential_hashes WHERE kind='SERVICE'").Scan(&n); e != nil || n != 2 {
		t.Fatal("tombstones lost", n, e)
	}
}

func TestIntegrationRegistryCustomerAndPlayerStorage(t *testing.T) {
	r, _, release := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	var installation string
	var now time.Time
	if e := r.Store.DB.QueryRow("SELECT installation_id,UTC_TIMESTAMP(6) FROM memory_installation WHERE singleton=1").Scan(&installation, &now); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.InitializeIntegrationRegistrar(ctx, "registrar", installation); e != nil {
		t.Fatal(e)
	}
	secret := "synthetic-integration-service-credential-32"
	ref := filepath.Join(t.TempDir(), "key")
	if e := os.WriteFile(ref, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	expiry := now.Add(24 * time.Hour)
	c := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "customer-one", Customer: &domain.RegisterCustomer{CustomerID: "customer-one", ServiceSubjectID: "service-one", ConsumerID: "consumer-one", SecretHandleRef: ref, ExpiresAt: expiry}}
	a, e := r.Store.ApplyIntegrationRegistry(ctx, "customer", c)
	if e != nil {
		t.Fatal("real registry rejected customer registration", e)
	}
	if a.RegistrationExpiry == nil || !a.RegistrationExpiry.CustomerExpiresAt.Equal(expiry) || a.RegistrationExpiry.ServiceKeyExpiresAt == nil || !a.RegistrationExpiry.ServiceKeyExpiresAt.Equal(expiry) {
		t.Fatal("original expiry absent", a)
	}
	b, e := r.Store.ReadIntegrationRegistryReceipt(ctx, "customer")
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("receipt changed", a, b, e)
	}
	b, e = r.Store.ApplyIntegrationRegistry(ctx, "customer", c)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("key replay changed original", e)
	}
	c.Customer.ExpiresAt = expiry.Add(time.Minute)
	if _, e = r.Store.ApplyIntegrationRegistry(ctx, "customer", c); e == nil {
		t.Fatal("same key renewed registration")
	}
	c.Customer.ExpiresAt = expiry
	_, principal, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
	if e != nil || principal.Customer != "customer-one" || principal.StableSubject != "service-one" {
		t.Fatal(principal, e)
	}
	if _, _, e = r.Store.AuthenticateOperator(context.Background(), "Bearer "+secret); e == nil {
		t.Fatal("service authenticated as operator")
	}
	if _, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationClient); e == nil {
		t.Fatal("service authenticated as client")
	}
	player := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: "coffee", TargetID: "player-one", Player: &domain.RegisterPlayerAccess{CustomerID: "customer-one", ExternalIDBytes: []byte("玩家🙂"), PlayerSubjectID: "player-one", EntitlementID: "entitlement-one", ReleaseID: release, Actions: []string{"READ", "TURN", "MEMORY_VIEW", "MEMORY_GOVERN"}, ExpiresAt: expiry}}
	x, e := r.Store.ApplyIntegrationRegistry(ctx, "player", player)
	if e != nil {
		t.Fatal("real registry rejected player access", e)
	}
	if x.RegistrationExpiry == nil || x.RegistrationExpiry.MappingExpiresAt == nil || x.RegistrationExpiry.EntitlementExpiresAt == nil || !x.RegistrationExpiry.CustomerExpiresAt.Equal(expiry) {
		t.Fatal(x)
	}
	var metadata, positive, eligibility int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM memory_subject_permissions WHERE caller_subject_id='player-one' AND data_subject_id='player-one' AND action IN('memory.metadata.read','memory.private_body.read','memory.govern','memory.correct')),(SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='player-one' AND action='memory.generation.use'),(SELECT COUNT(*) FROM integration_entitlements WHERE player_subject_id='player-one' AND self_consent_eligible=1 AND eligibility_policy='SELF_PRIVATE_R0')").Scan(&metadata, &positive, &eligibility); e != nil || metadata != 4 || positive != 0 || eligibility != 1 {
		t.Fatal(metadata, positive, eligibility, e)
	}
}

func TestIntegrationRegistrarExplicitOriginalOwner(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	var installation string
	if e := r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_registrars").Scan(&n); e != nil || n != 0 {
		t.Fatal("Open must not create registrar", n, e)
	}
	if _, e := r.Store.InitializeIntegrationRegistrar(context.Background(), "no-proof", installation); e == nil {
		t.Fatal("plain context gained registrar")
	}
	if _, e := r.Store.InitializeIntegrationRegistrar(ctx, "wrong-install", "foreign"); e == nil {
		t.Fatal("foreign installation gained registrar")
	}
	a, e := r.Store.InitializeIntegrationRegistrar(ctx, "init-registrar", installation)
	if e != nil {
		t.Fatal("original authenticated owner could not explicitly initialize independent registrar", e)
	}
	if a.Operation != "INSTALL_INTEGRATION_REGISTRAR_V1" || a.State != "COMMITTED" || a.Revision != 1 || a.RegistrationExpiry != nil {
		t.Fatal(a)
	}
	b, e := r.Store.InitializeIntegrationRegistrar(ctx, "init-registrar", installation)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("original receipt changed", a, b, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_registrars WHERE credential_id='owner' AND product_id='coffee' AND action='customer.register' AND allowed_use='TENANT_CUSTOMER_REGISTRATION' AND expires_at IS NULL AND enabled=1 AND bootstrap_receipt=?", a.ReceiptRef).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&n); e != nil || n != 0 {
		t.Fatal("registrar granted use", n, e)
	}
}

func TestIntegrationOperatorRequiresCurrentGlobalBinding(t *testing.T) {
	for _, fault := range []string{"missing", "kind", "identity", "installation"} {
		t.Run(fault, func(t *testing.T) {
			r, sc, _ := runtimeFixture(t)
			ctx := runtimeContext(t, r)
			if err := r.Store.CheckOperator(ctx, sc, "content.read"); err != nil {
				t.Fatal(err)
			}
			q := map[string]string{
				"missing":      "DELETE FROM integration_credential_hashes WHERE principal_id='owner'",
				"kind":         "UPDATE integration_credential_hashes SET kind='SERVICE' WHERE principal_id='owner'",
				"identity":     "UPDATE integration_credential_hashes SET principal_id='other' WHERE principal_id='owner'",
				"installation": "UPDATE integration_credential_hashes SET installation_id='foreign' WHERE principal_id='owner'",
			}[fault]
			if _, err := r.Store.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.Store.AuthenticateOperator(context.Background(), "Bearer "+runtimeToken); err == nil {
				t.Fatal("fresh authentication ignored the global binding")
			}
			if err := r.Store.CheckOperator(ctx, sc, "content.read"); err == nil {
				t.Fatal("owning current gate ignored the global binding")
			}
		})
	}
}
