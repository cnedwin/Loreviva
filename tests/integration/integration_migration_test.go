package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"loreviva.local/loreviva/migrations"
	"strings"
	"testing"

	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
)

func TestIntegrationMigrationRegistrationExpiry(t *testing.T) {
	db := dbfixture.Open(t)
	s := store.New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=8").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("integration migration completion missing", n)
	}
	for _, table := range []string{"integration_customers", "integration_external_subjects"} {
		var typ, nullable string
		if err := db.QueryRow("SELECT COLUMN_TYPE,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME='expires_at'", table).Scan(&typ, &nullable); err != nil {
			t.Fatal(err)
		}
		if typ != "datetime(6)" || nullable != "NO" {
			t.Fatal("registration expiry is not a durable required timestamp", table, typ, nullable)
		}
	}
}

func integrationDDL() []string {
	var out []string
	for _, q := range strings.Split(migrations.DeliveryIdentity, ";") {
		if strings.TrimSpace(q) != "" {
			out = append(out, strings.TrimSpace(q))
		}
	}
	return out
}
func TestIntegrationMigrationCompletedGlobalBinding(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	if _, e := r.Store.DB.Exec("DELETE FROM integration_credential_hashes WHERE kind='OPERATOR'"); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.Migrate(context.Background()); e == nil {
		t.Fatal("completed migration accepted missing original operator hash mapping")
	}
}
func TestIntegrationMigrationPartialDDLAndStructure(t *testing.T) {
	statements := integrationDDL()
	if len(statements) != 17 {
		t.Fatal("exact17 CREATE", len(statements))
	}
	for prefix := 0; prefix <= 17; prefix++ {
		t.Run(fmt.Sprintf("prefix-%02d", prefix), func(t *testing.T) {
			db := dbfixture.Open(t)
			s := store.New(db)
			ctx := context.Background()
			if e := s.Migrate(ctx); e != nil {
				t.Fatal(e)
			}
			for _, q := range statements {
				parts := strings.Fields(q)
				if len(parts) < 6 || strings.Join(parts[:5], " ") != "CREATE TABLE IF NOT EXISTS" {
					t.Fatal("noncreate")
				}
				if _, e := db.Exec("DROP TABLE " + parts[5]); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := db.Exec("DELETE FROM schema_migrations WHERE version=8"); e != nil {
				t.Fatal(e)
			}
			for _, q := range statements[:prefix] {
				if _, e := db.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Migrate(ctx); e != nil {
				t.Fatal("partial restart", e)
			}
			if e := s.Migrate(ctx); e != nil {
				t.Fatal("completed replay", e)
			}
			var count int
			if e := db.QueryRow("SELECT COUNT(*) FROM integration_registrars").Scan(&count); e != nil || count != 0 {
				t.Fatal("migration minted registrar", count, e)
			}
			var hash []byte
			expected := sha256.Sum256([]byte(migrations.DeliveryIdentity))
			if e := db.QueryRow("SELECT sha256 FROM schema_migrations WHERE version=8").Scan(&hash); e != nil || string(hash) != string(expected[:]) {
				t.Fatal("marker hash", e)
			}
		})
	}
	for _, fault := range []string{"nullable-expiry", "missing-index", "disabled-check"} {
		t.Run(fault, func(t *testing.T) {
			db := dbfixture.Open(t)
			s := store.New(db)
			if e := s.Migrate(context.Background()); e != nil {
				t.Fatal(e)
			}
			q := map[string]string{"nullable-expiry": "ALTER TABLE integration_customers MODIFY expires_at DATETIME(6) NULL", "missing-index": "ALTER TABLE integration_customers DROP INDEX integration_customer_consumer_id", "disabled-check": "ALTER TABLE integration_principals ALTER CHECK integration_principal_branch NOT ENFORCED"}[fault]
			if _, e := db.Exec(q); e != nil {
				t.Fatal(e)
			}
			if e := s.Migrate(context.Background()); e == nil {
				t.Fatal("completed structure drift accepted")
			}
		})
	}
}
func TestIntegrationMigrationRealOperatorBackfill(t *testing.T) {
	for _, mode := range []string{"valid", "collision"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			db := r.Store.DB
			if _, e := db.Exec("DELETE FROM schema_migrations WHERE version=8"); e != nil {
				t.Fatal(e)
			}
			if mode == "valid" {
				if _, e := db.Exec("DELETE FROM integration_credential_hashes"); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e := db.Exec("UPDATE integration_credential_hashes SET kind='SERVICE' WHERE principal_id='owner'"); e != nil {
					t.Fatal(e)
				}
			}
			e := r.Store.Migrate(context.Background())
			if mode == "collision" {
				if e == nil {
					t.Fatal("cross-kind backfill collision accepted")
				}
				var n int
				if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=8").Scan(&n); err != nil || n != 0 {
					t.Fatal(n, err)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e = r.Store.AuthenticateOperator(context.Background(), "Bearer "+runtimeToken); e != nil {
				t.Fatal("original credential changed", e)
			}
			var grants int
			if e = db.QueryRow("SELECT (SELECT COUNT(*) FROM integration_registrars)+(SELECT COUNT(*) FROM integration_customers)").Scan(&grants); e != nil || grants != 0 {
				t.Fatal("metadata migration granted authority", grants, e)
			}
		})
	}
}

func TestIntegrationMigrationRejectsUnexpectedForeignKey(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("marker-complete-%t", completed), func(t *testing.T) {
			db := dbfixture.Open(t)
			s := store.New(db)
			ctx := context.Background()
			if err := s.Migrate(ctx); err != nil {
				t.Fatal("legal completion", err)
			}
			if err := s.Migrate(ctx); err != nil {
				t.Fatal("legal completed reentry", err)
			}
			if !completed {
				if _, err := db.Exec("DELETE FROM schema_migrations WHERE version=8"); err != nil {
					t.Fatal(err)
				}
			}
			// Both sides use their existing three-column PRIMARY index. An extra
			// index must not accidentally be the reason this malformed schema fails.
			readIndexes := func() string {
				// Read every row; GROUP_CONCAT can silently truncate the proof.
				rows, err := db.Query("SELECT CONCAT(TABLE_NAME,':',INDEX_NAME,':',SEQ_IN_INDEX,':',COLUMN_NAME,':',NON_UNIQUE,':',COALESCE(SUB_PART,0),':',INDEX_TYPE,':',IS_VISIBLE) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN ('integration_customers','integration_world_participants') ORDER BY TABLE_NAME,INDEX_NAME,SEQ_IN_INDEX")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var result []string
				for rows.Next() {
					var row string
					if err := rows.Scan(&row); err != nil {
						t.Fatal(err)
					}
					result = append(result, row)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if len(result) != 19 {
					t.Fatal("expected all twelve customer and seven world PRIMARY/index rows", len(result))
				}
				return strings.Join(result, "\n")
			}
			indexesBefore := readIndexes()
			if _, err := db.Exec("ALTER TABLE integration_customers ADD CONSTRAINT unexpected_customer_world_fk FOREIGN KEY (tenant_id,product_id,customer_id) REFERENCES integration_world_participants(tenant_id,product_id,world_instance_id)"); err != nil {
				t.Fatal("real foreign-key setup", err)
			}
			indexesAfter := readIndexes()
			if indexesBefore != indexesAfter {
				t.Fatal("foreign key must reuse the original indexes")
			}
			var foreignKeys int
			fkQuery := "SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='integration_customers' AND CONSTRAINT_TYPE='FOREIGN KEY' AND CONSTRAINT_NAME='unexpected_customer_world_fk'"
			if err := db.QueryRow(fkQuery).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatal("real foreign key missing before migration", foreignKeys, err)
			}
			migrationErr := s.Migrate(ctx)
			if migrationErr == nil {
				t.Error("migration accepted an undeclared foreign key using unchanged PRIMARY indexes")
			}
			if err := db.QueryRow(fkQuery).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Error("migration must reject, never remove the foreign key", foreignKeys, err)
			}
			var markers int
			wantMarkers := 0
			if completed {
				wantMarkers = 1
				var hash []byte
				expected := sha256.Sum256([]byte(migrations.DeliveryIdentity))
				if err := db.QueryRow("SELECT sha256 FROM schema_migrations WHERE version=8").Scan(&hash); err != nil || string(hash) != string(expected[:]) {
					t.Error("original completed marker changed", err)
				}
			}
			if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=8").Scan(&markers); err != nil || markers != wantMarkers {
				t.Error("malformed schema changed completion state", markers, wantMarkers, err)
			}
			t.Logf("unchanged_indexes=%t retained_foreign_keys=%d marker_count=%d rejected=%t", indexesBefore == indexesAfter, foreignKeys, markers, migrationErr != nil)
		})
	}
}
