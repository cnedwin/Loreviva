package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/migrations"
	"loreviva.local/loreviva/tests/integration/dbfixture"
)

func TestTask6MigrationRecoversOriginalDDLWithoutMarker(t *testing.T) {
	for _, mode := range []string{"PARTIAL", "ALL_DDL_NO_MARKER"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := store.New(dbfixture.Open(t))
			execute := func(source string) {
				for _, statement := range strings.Split(source, ";") {
					if strings.TrimSpace(statement) != "" {
						if _, e := s.DB.ExecContext(ctx, statement); e != nil {
							t.Fatal(e)
						}
					}
				}
			}
			prior := []string{migrations.Core, migrations.Content, migrations.FileCleanup, migrations.Runtime, migrations.MemoryGovernance}
			for i, source := range prior {
				execute(source)
				sum := sha256.Sum256([]byte(source))
				if _, e := s.DB.ExecContext(ctx, "INSERT INTO schema_migrations(version,sha256) VALUES(?,?)", i+1, sum[:]); e != nil {
					t.Fatal(e)
				}
			}
			for _, statement := range strings.Split(migrations.TurnAttempt, ";") {
				if strings.TrimSpace(statement) == "" {
					continue
				}
				execute(statement)
				if mode == "PARTIAL" && strings.Contains(statement, "harness_candidate_recoveries (") {
					break
				}
			}
			var marker, tables int
			if e := s.DB.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=6").Scan(&marker); e != nil || marker != 0 {
				t.Fatal(marker, e)
			}
			matches := regexp.MustCompile(`CREATE TABLE (?:IF NOT EXISTS )?(\w+)`).FindAllStringSubmatch(migrations.TurnAttempt, -1)
			if len(matches) != 49 {
				t.Fatal("unexpected migration fixture table set", len(matches))
			}
			definitions := func() map[string]string {
				out := map[string]string{}
				for _, m := range matches {
					var exists int
					if e := s.DB.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", m[1]).Scan(&exists); e != nil {
						t.Fatal(e)
					}
					if exists == 0 {
						continue
					}
					var name, ddl string
					if e := s.DB.QueryRow("SHOW CREATE TABLE "+m[1]).Scan(&name, &ddl); e != nil {
						t.Fatal(e)
					}
					out[name] = ddl
				}
				return out
			}
			before := definitions()
			tables = len(before)
			if (mode == "PARTIAL" && tables != 39) || (mode == "ALL_DDL_NO_MARKER" && tables != 49) {
				t.Fatal("did not create intended interruption state", tables)
			}
			if e := s.Migrate(ctx); e != nil {
				t.Fatal("original migration must resume committed DDL", e)
			}
			after := definitions()
			if len(after) != 49 {
				t.Fatal("incomplete tables", len(after))
			}
			for name, ddl := range before {
				if after[name] != ddl {
					t.Fatal("existing table changed", name)
				}
			}
			for i, source := range append(prior, migrations.TurnAttempt, migrations.StudioSettings) {
				var got []byte
				sum := sha256.Sum256([]byte(source))
				if e := s.DB.QueryRow("SELECT sha256 FROM schema_migrations WHERE version=?", i+1).Scan(&got); e != nil || !bytes.Equal(got, sum[:]) {
					t.Fatal("actual migration hash", i+1, e)
				}
			}
			if e := s.Migrate(ctx); e != nil {
				t.Fatal("second replay", e)
			}
			if !reflect.DeepEqual(after, definitions()) {
				t.Fatal("replay changed schema")
			}
			if e := s.DB.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&marker); e != nil || marker != 7 {
				t.Fatal(marker, e)
			}
		})
	}
}

func TestTask6CapabilitiesMatchInstalledGenerationHandlers(t *testing.T) {
	for _, mode := range []string{"READY", "MISSING_REHEARSAL_ADAPTER", "UNBOUND"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, candidate := candidateHTTPFixture(t)
			local, reader := r.Local, r.Store.RehearsalReportReader
			defer func() { r.Store.RehearsalReportReader = reader; r.BindLocal(local) }()
			if mode == "MISSING_REHEARSAL_ADAPTER" {
				r.Store.RehearsalReportReader = nil
			}
			if mode == "UNBOUND" {
				r.BindLocal(nil)
			}
			candidateReady, rehearsalReady := mode != "UNBOUND", mode == "READY"
			if r.Worker.Turns.CandidateReady() != candidateReady || r.Worker.Turns.RehearsalReady() != rehearsalReady {
				t.Fatal("fixture did not reach actual readiness")
			}
			check := func(candidateReady, rehearsalReady bool) {
				var caps generated.CreatorCapabilities
				runtimeHTTP(t, r, "GET", "/capabilities?product_id=coffee", "", nil, 200, &caps)
				for job, want := range map[string]bool{"CANDIDATE": candidateReady, "REHEARSAL": rehearsalReady} {
					supported, unimplemented := 0, 0
					for _, v := range caps.SupportedJobs {
						if string(v) == job {
							supported++
						}
					}
					for _, v := range caps.UnimplementedJobs {
						if string(v) == job {
							unimplemented++
						}
					}
					if (want && (supported != 1 || unimplemented != 0)) || (!want && (supported != 0 || unimplemented != 1)) {
						t.Fatalf("%s current readiness=%v supported=%d unimplemented=%d", job, want, supported, unimplemented)
					}
				}
			}
			check(candidateReady, rehearsalReady)
			var request generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &request); e != nil {
				t.Fatal(e)
			}
			if rehearsalReady {
				var receipt generated.AdminTaskReceipt
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/rehearsals", "caps-ready", request, 202, &receipt)
				if e := r.Worker.Once(context.Background()); e != nil {
					t.Fatal(e)
				}
				var n int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_rehearsal_reports WHERE task_id=? AND state='COMMITTED'", receipt.TaskID).Scan(&n); e != nil || n != 1 {
					t.Fatal("advertised handler did not commit real report", n, e)
				}
			} else {
				counts := func() string {
					var out string
					if e := r.Store.DB.QueryRow("SELECT CONCAT((SELECT COUNT(*) FROM tasks),':',(SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts))").Scan(&out); e != nil {
						t.Fatal(e)
					}
					return out
				}
				before := counts()
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/rehearsals", "caps-missing", request, 503, nil)
				if !candidateReady {
					runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "caps-missing", candidate, 503, nil)
				}
				if counts() != before {
					t.Fatal("unavailable handler accepted work")
				}
				r.Store.RehearsalReportReader = reader
				r.BindLocal(local)
				check(true, true)
			}
		})
	}
}
