package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/migrations"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"strings"
	"testing"
)

var studioTables = []string{"studio_credential_state", "studio_operator_grants", "studio_self_consent_current", "studio_self_consent_facts", "studio_reference_catalog", "studio_connection_metadata", "studio_scoped_settings", "studio_settings_commands"}

func studioSchemaSix(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db := dbfixture.Open(t)
	for i, source := range []string{migrations.Core, migrations.Content, migrations.FileCleanup, migrations.Runtime, migrations.MemoryGovernance, migrations.TurnAttempt} {
		for _, q := range strings.Split(source, ";") {
			if strings.TrimSpace(q) != "" {
				if _, e := db.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
		}
		h := sha256.Sum256([]byte(source))
		if _, e := db.Exec("INSERT INTO schema_migrations(version,sha256) VALUES(?,?)", i+1, h[:]); e != nil {
			t.Fatal(e)
		}
	}
	return store.New(db), db
}

func TestStudioMigrationEveryPartialDDLPrefix(t *testing.T) {
	var statements []string
	for _, q := range strings.Split(migrations.StudioSettings, ";") {
		if strings.TrimSpace(q) != "" {
			statements = append(statements, q)
		}
	}
	for prefix := 0; prefix <= len(statements); prefix++ {
		t.Run(string(rune('A'+prefix)), func(t *testing.T) {
			s, db := studioSchemaSix(t)
			for _, q := range statements[:prefix] {
				if _, e := db.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Migrate(context.Background()); e != nil {
				t.Fatal("partial DDL resume", prefix, e)
			}
			if e := s.Migrate(context.Background()); e != nil {
				t.Fatal("repeat completed migration", e)
			}
		})
	}
}

func TestStudioMigrationIssuerRequiresExactFamilyProof(t *testing.T) {
	ctx := context.Background()
	r, draft, request := candidateHTTPFixture(t)
	ctx, _, authErr := r.Auth.Authenticate(ctx, "Bearer "+runtimeToken)
	if authErr != nil {
		t.Fatal(authErr)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	good, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "legacy-good", request)
	if e != nil {
		t.Fatal(e)
	}
	bad, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "legacy-bad", request)
	if e != nil {
		t.Fatal(e)
	}
	var rehearsal generated.RehearsalRequest
	if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &rehearsal); e != nil {
		t.Fatal(e)
	}
	rh, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "legacy-rehearsal", rehearsal)
	if e != nil {
		t.Fatal(e)
	}
	cleanup, e := app.QueueCleanup(ctx, r.Store, scope, "legacy-cleanup-proof")
	if e != nil {
		t.Fatal(e)
	}
	// Only these legacy families had the old hardcoded-owner gate. A registry
	// owner alone is not proof for an unrelated CONTROL/MIGRATION job.
	for _, q := range []string{
		"UPDATE harness_admin_candidates SET accepted_commit_key='missing' WHERE original_key='legacy-bad'",
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,task_kind,next_run_at) VALUES('demo-local','coffee','unknown-control','creator','PRODUCT','ACCEPTED','CONTROL',UTC_TIMESTAMP(6)),('demo-local','coffee','unknown-migration','creator','PRODUCT','ACCEPTED','MIGRATION',UTC_TIMESTAMP(6))",
		"INSERT INTO registered_jobs(tenant_id,product_id,task_id,operation) VALUES('demo-local','coffee','unknown-control','INSTANCE_CONTROL'),('demo-local','coffee','unknown-migration','INSTANCE_MIGRATION')",
		"UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'",
		"UPDATE registered_jobs SET issuer_credential_id=NULL,issuer_provenance=NULL",
		"DELETE FROM schema_migrations WHERE version=7",
	} {
		if _, e = r.Store.DB.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if e = r.Store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct{ id, provenance, credential string }{{string(good.TaskID), "LEGACY_ORIGINAL_OWNER", "owner"}, {string(rh.TaskID), "LEGACY_ORIGINAL_OWNER", "owner"}, {string(cleanup.TaskID), "PROGRAM_OWNED", ""}, {string(bad.TaskID), "UNKNOWN_LEGACY", ""}, {"unknown-control", "UNKNOWN_LEGACY", ""}, {"unknown-migration", "UNKNOWN_LEGACY", ""}} {
		var p, id sql.NullString
		if e = r.Store.DB.QueryRow("SELECT issuer_provenance,issuer_credential_id FROM registered_jobs WHERE task_id=?", c.id).Scan(&p, &id); e != nil {
			t.Fatal(e)
		}
		if p.String != c.provenance || id.String != c.credential {
			t.Fatal(c.id, p, id)
		}
	}
}

func TestStudioMigrationDuplicateBearerBeforeWrites(t *testing.T) {
	s, db := studioSchemaSix(t)
	for _, q := range []string{"INSERT INTO tenant_gates VALUES('t',1,1)", "INSERT INTO product_gates VALUES('t','p',1,1)", "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('t','p','owner',1,1,1,1)", "INSERT INTO operator_credentials VALUES('one','t','p','owner',UNHEX(SHA2('synthetic-test-duplicate',256)),1),('two','t','p','owner',UNHEX(SHA2('synthetic-test-duplicate',256)),1)"} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Migrate(context.Background()); e == nil {
		t.Fatal("duplicate bearer accepted")
	}
	var n int
	if e := db.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME LIKE 'studio_%'").Scan(&n); e != nil || n != 0 {
		t.Fatal("wrote schema7 before duplicate preflight", n, e)
	}
}

func studioMigrationFixture(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db := dbfixture.Open(t)
	s := store.New(db)
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	return s, db
}

func TestStudioMigrationEightTablesAndRetry(t *testing.T) {
	s, db := studioMigrationFixture(t)
	var version int
	if e := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); e != nil || version != 7 {
		t.Fatalf("need schema7, got %d: %v", version, e)
	}
	for _, table := range studioTables {
		var n int
		if e := db.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", table).Scan(&n); e != nil || n != 1 {
			t.Fatalf("missing %s: %v", table, e)
		}
	}
	if _, e := db.Exec("DELETE FROM schema_migrations WHERE version=7"); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal("retry complete DDL", e)
	}
}

func TestStudioMigrationPartialDDLRejectsAlteredShape(t *testing.T) {
	for _, mode := range []string{"column", "index", "check"} {
		t.Run(mode, func(t *testing.T) {
			s, db := studioMigrationFixture(t)
			if _, e := db.Exec("DELETE FROM schema_migrations WHERE version=7"); e != nil {
				t.Fatal(e)
			}
			q := map[string]string{"column": "ALTER TABLE studio_scoped_settings MODIFY revision INT UNSIGNED NOT NULL", "index": "ALTER TABLE operator_credentials DROP INDEX studio_unique_bearer", "check": "ALTER TABLE studio_scoped_settings ALTER CHECK studio_settings_scope_branch NOT ENFORCED"}[mode]
			if _, e := db.Exec(q); e != nil {
				t.Fatal(e)
			}
			if mode == "index" {
				if _, e := db.Exec("ALTER TABLE operator_credentials ADD INDEX studio_unique_bearer(token_sha256)"); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Migrate(context.Background()); e == nil {
				t.Fatal("accepted altered DDL")
			}
			var n int
			if e := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=7").Scan(&n); e != nil || n != 0 {
				t.Fatal("false applied marker", n, e)
			}
		})
	}
}

func TestStudioMigrationTenantIdentity(t *testing.T) {
	_, db := studioMigrationFixture(t)
	q := "INSERT INTO studio_scoped_settings(tenant_id,scope_type,product_id,scope_address,section,revision,document) VALUES('demo-local','TENANT',NULL,'T','DEFAULTS',1,'{}')"
	if _, e := db.Exec(q); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(q); e == nil {
		t.Fatal("nullable tenant duplicate accepted")
	}
	if _, e := db.Exec("INSERT INTO studio_scoped_settings(tenant_id,scope_type,product_id,scope_address,section,revision,document) VALUES('demo-local','TENANT','fake','T','QUOTA',1,'{}')"); e == nil {
		t.Fatal("fake product tenant accepted")
	}
}

func TestStudioMigrationConsentBaselinePreservesFalseAndTrue(t *testing.T) {
	s, db := studioSchemaSix(t)
	for _, q := range []string{
		"INSERT INTO tenant_gates VALUES('t',1,1)",
		"INSERT INTO product_gates VALUES('t','p',1,1)",
		"INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('t','p','yes',1,1,1,1),('t','p','no',1,1,1,1)",
		"INSERT INTO operator_credentials VALUES('yes','t','p','yes',UNHEX(SHA2('synthetic-yes',256)),0),('no','t','p','no',UNHEX(SHA2('synthetic-no',256)),1)",
		"INSERT INTO r0_generation_consent VALUES('t','p','yes','yes','R0_OWNER_PRIVATE_GENERATION_30D',1,'2026-01-01','2026-01-31'),('t','p','no','no','R0_OWNER_PRIVATE_GENERATION_30D',0,'2026-01-01',NULL)",
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, sub := range []string{"yes", "no"} {
		var action string
		var accepted, expiry sql.NullTime
		var revision uint64
		var legacy bool
		if e := db.QueryRow("SELECT last_action,accepted_at,expires_at,revision,legacy_baseline FROM studio_self_consent_current WHERE subject_id=?", sub).Scan(&action, &accepted, &expiry, &revision, &legacy); e != nil {
			t.Fatal(e)
		}
		if revision != 1 || !legacy || accepted.Valid != (sub == "yes") || expiry.Valid != (sub == "yes") {
			t.Fatal("historical acceptance invented or lost", sub, action)
		}
		if sub == "no" && action != "LEGACY_FALSE" || sub == "yes" && action != "LEGACY_TRUE" {
			t.Fatal(action)
		}
	}
	if _, e := db.Exec("DELETE FROM schema_migrations WHERE version=7"); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	var facts, permissions int
	if e := db.QueryRow("SELECT COUNT(*) FROM studio_self_consent_facts").Scan(&facts); e != nil || facts != 2 {
		t.Fatal("baseline duplicated", facts, e)
	}
	if e := db.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions").Scan(&permissions); e != nil || permissions != 0 {
		t.Fatal("migration granted permission", permissions, e)
	}
}
