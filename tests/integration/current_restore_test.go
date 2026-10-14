package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
)

// Test-only logical row backup, tied to this exact isolated fixture database.
// No production restore adapter or SQL callback is exposed by this helper.
type ownedTableImage struct {
	name    string
	columns []string
	rows    [][]any
}
type ownedDatabaseImage struct {
	name   string
	tables []ownedTableImage
}

func captureOwnedDatabase(t *testing.T, db *sql.DB) ownedDatabaseImage {
	t.Helper()
	var out ownedDatabaseImage
	if e := db.QueryRow("SELECT DATABASE()").Scan(&out.name); e != nil || !regexp.MustCompile(`^loreviva_test_[0-9a-f]{24}$`).MatchString(out.name) {
		t.Fatal("not an isolated fixture")
	}
	rows, e := db.Query("SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' ORDER BY table_name")
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			t.Fatal(e)
		}
		names = append(names, name)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	rows.Close()
	for _, name := range names {
		if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(name) {
			t.Fatal("unexpected fixture table")
		}
		columnRows, e := db.Query("SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND generation_expression='' ORDER BY ordinal_position", name)
		if e != nil {
			t.Fatal(e)
		}
		selected := []string{}
		for columnRows.Next() {
			var c string
			if e = columnRows.Scan(&c); e != nil {
				t.Fatal(e)
			}
			if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(c) {
				t.Fatal("unexpected fixture column")
			}
			selected = append(selected, "`"+c+"`")
		}
		if e = columnRows.Err(); e != nil {
			t.Fatal(e)
		}
		columnRows.Close()
		q, e := db.Query("SELECT " + strings.Join(selected, ",") + " FROM `" + name + "`")
		if e != nil {
			t.Fatal(e)
		}
		columns, e := q.Columns()
		if e != nil {
			t.Fatal(e)
		}
		table := ownedTableImage{name: name, columns: columns}
		for q.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if e = q.Scan(targets...); e != nil {
				t.Fatal(e)
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = bytes.Clone(b)
				}
			}
			table.rows = append(table.rows, values)
		}
		if e = q.Err(); e != nil {
			t.Fatal(e)
		}
		q.Close()
		out.tables = append(out.tables, table)
	}
	return out
}
func restoreOwnedDatabase(t *testing.T, db *sql.DB, image ownedDatabaseImage) {
	t.Helper()
	ctx := context.Background()
	conn, e := db.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	var current string
	if e = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); e != nil || current != image.name || !regexp.MustCompile(`^loreviva_test_[0-9a-f]{24}$`).MatchString(current) {
		t.Fatal("restore target differs")
	}
	if _, e = conn.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=0"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if _, e := conn.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=1"); e != nil {
			t.Fatal("fixture connection reset", e)
		}
	}()
	tx, e := conn.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for _, table := range image.tables {
		if _, e = tx.ExecContext(ctx, "DELETE FROM `"+table.name+"`"); e != nil {
			t.Fatal(e)
		}
	}
	for _, table := range image.tables {
		columns := []string{}
		for _, c := range table.columns {
			if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(c) {
				t.Fatal("unexpected fixture column")
			}
			columns = append(columns, "`"+c+"`")
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
		for _, row := range table.rows {
			if _, e = tx.ExecContext(ctx, "INSERT INTO `"+table.name+"` ("+strings.Join(columns, ",")+") VALUES ("+marks+")", row...); e != nil {
				t.Fatal(table.name, e)
			}
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}
func captureOwnedFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected fixture symlink")
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(path)
		if e == nil {
			out[rel] = b
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	return out
}
func restoreOwnedFiles(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, b := range files {
		path := filepath.Join(root, rel)
		check, e := filepath.Rel(root, path)
		if e != nil || strings.HasPrefix(check, "..") || filepath.IsAbs(check) {
			t.Fatal("fixture path escaped")
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

func TestCurrentParticipantsPairedRowsFilesCannotReopenClosedInstallation(t *testing.T) {
	r, _, asset, _, sources := currentParticipantFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	read := func() []byte {
		t.Helper()
		stream, e := r.Files.OpenAuthorized(ctx, scope, asset)
		if e != nil {
			t.Fatal("positive current source", e)
		}
		defer stream.Close()
		b := make([]byte, 1)
		if n, e := stream.Read(b); e != nil || n != 1 {
			t.Fatal(n, e)
		}
		return b
	}
	read()
	oldDB := captureOwnedDatabase(t, r.Store.DB)
	oldFiles := captureOwnedFiles(t, r.Files.RootPath)
	if len(oldDB.tables) < 96 || len(oldFiles) < 5 {
		t.Fatal("incomplete actual fixture backup", len(oldDB.tables), len(oldFiles))
	}
	// Actual StageClosure purge and its independent physical ACK occur after the
	// paired backup, in the same installation as negative/metering/Skill domains.
	if _, e := r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", sources[1]); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	var cutoff time.Time
	if e := r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	page, e := r.Files.CleanupPage(ctx, scope, cutoff, 0, upper)
	if e != nil || page.Removed < 5 {
		t.Fatal("actual combined derivative purge", page, e)
	}
	var purged int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1 AND purged=1").Scan(&purged); e != nil || purged < 3 {
		t.Fatal("actual physical ACK", purged, e)
	}
	localRoot := filepath.Dir(r.Local.Coordinator.Directory())
	independent := captureOwnedFiles(t, localRoot)
	if e = r.Local.Coordinator.BeginClose(ctx, "paired-current-c"); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Coordinator.Drain(ctx, "paired-current-c"); e != nil {
		t.Fatal(e)
	}
	closed, e := os.ReadFile(filepath.Join(r.Local.Coordinator.Directory(), "coordinator.json"))
	if e != nil {
		t.Fatal(e)
	}
	restoreOwnedDatabase(t, r.Store.DB, oldDB)
	restoreOwnedFiles(t, r.Files.RootPath, oldFiles)
	// These are actual restored old logical rows and original bytes, not a fake
	// terminal enum. Independent coordinator/G/J are never restored with them.
	var restored int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1").Scan(&restored); e != nil || restored != 0 {
		t.Fatal("old rows not restored", restored, e)
	}
	if _, e = app.OpenLocal(ctx, r.Store, localRoot, r.Files.RootPath); !errors.Is(e, maintenance.ErrMaintenance) {
		t.Fatal("paired restore escaped current CLOSED", e)
	}
	if _, e = r.Files.OpenAuthorized(ctx, scope, asset); !errors.Is(e, maintenance.ErrMaintenance) {
		t.Fatal("restored original bytes leaked", e)
	}
	if _, _, e = r.Store.RunOriginalServiceMetering(ctx); !errors.Is(e, maintenance.ErrMaintenance) {
		t.Fatal("restored queue resurrected writer", e)
	}
	q := httptest.NewRequest("GET", "/world/v1/admin/tasks?product_id=coffee", nil)
	q.Header.Set("Authorization", "Bearer "+runtimeToken)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, q)
	if w.Code != 503 {
		t.Fatal("restored metadata bypassed current coordinator", w.Code)
	}
	current, e := os.ReadFile(filepath.Join(r.Local.Coordinator.Directory(), "coordinator.json"))
	if e != nil || !bytes.Equal(current, closed) {
		t.Fatal("restore reconstructed coordinator", e)
	}
	for rel, b := range independent {
		if !strings.HasPrefix(filepath.ToSlash(rel), "ledger/") && !strings.HasPrefix(filepath.ToSlash(rel), "mock/") {
			continue
		}
		now, e := os.ReadFile(filepath.Join(localRoot, rel))
		if e != nil || !bytes.Equal(now, b) {
			t.Fatal("paired restore rewrote independent G/J", rel, e)
		}
	}
	t.Log("actual paired logical tables", len(oldDB.tables), "files", len(oldFiles), "physical purge ACK", purged)
}

func TestCurrentParticipantsIndependentMissingWrongProofs(t *testing.T) {
	r, _, asset, pending, _ := currentParticipantFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	root := filepath.Dir(r.Local.Coordinator.Directory())
	counters := func() [4]int {
		t.Helper()
		var n [4]int
		if err := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM service_events),SUM(calls_allocated),SUM(steps_used) FROM harness_envelopes").Scan(&n[0], &n[1], &n[2], &n[3]); err != nil {
			t.Fatal(err)
		}
		return n
	}
	initial := counters()
	defer func() {
		if after := counters(); after != initial {
			t.Fatal("proof-only reads changed original facts", initial, after)
		}
	}()
	for _, mode := range []string{"G_MISSING", "G_WRONG", "J_MISSING", "J_WRONG", "COORD_WRONG", "J_OLD_KNOWN_HEAD"} {
		t.Run(mode, func(t *testing.T) {
			sub, name := "ledger", "anchor.json"
			expected := memory.ErrPreservation
			if strings.HasPrefix(mode, "J_") {
				sub = "mock"
				expected = harness.ErrMockContinuity
			}
			if mode == "COORD_WRONG" {
				sub = "coordination"
				name = "coordinator.json"
				expected = maintenance.ErrMaintenance
			}
			path := filepath.Join(root, sub, name)
			original, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			stream, e := r.Files.OpenAuthorized(ctx, scope, asset)
			if e != nil {
				t.Fatal("VALID before mutation", e)
			}
			defer stream.Close()
			if mode == "J_OLD_KNOWN_HEAD" {
				journalPath := filepath.Join(root, "mock", "journal.jsonl")
				oldJournal, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				r.Worker.Turns.Fault = harness.MockFault(19)
				if err = r.Worker.Turns.DeliverLateMockBills(ctx, pending); err != nil {
					t.Fatal(err)
				}
				freshAnchor, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				freshJournal, err := os.ReadFile(journalPath)
				if err != nil || bytes.Equal(freshJournal, oldJournal) {
					t.Fatal("actual original usage append", err)
				}
				defer os.WriteFile(path, freshAnchor, 0600)
				defer os.WriteFile(journalPath, freshJournal, 0600)
				if err = os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(journalPath, oldJournal, 0600); err != nil {
					t.Fatal(err)
				}
				if err = r.Local.Mock.Ready(ctx); !errors.Is(err, harness.ErrMockContinuity) {
					t.Fatal("known old J", err)
				}
				if _, err = r.Files.OpenAuthorized(ctx, scope, asset); err == nil {
					t.Fatal("old independent J accepted by actual report reader")
				}
				if err = os.WriteFile(path, freshAnchor, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(journalPath, freshJournal, 0600); err != nil {
					t.Fatal(err)
				}
				current, err := r.Files.OpenAuthorized(ctx, scope, asset)
				if err != nil {
					t.Fatal("current J positive", err)
				}
				current.Close()
				stream.Close()
				return
			}
			if strings.HasSuffix(mode, "MISSING") {
				e = os.Remove(path)
			} else {
				var data map[string]any
				if e = json.Unmarshal(original, &data); e != nil {
					t.Fatal(e)
				}
				data["Installation"] = "wrong-installation"
				wrong, _ := json.Marshal(data)
				e = os.WriteFile(path, wrong, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			defer os.WriteFile(path, original, 0600)
			if sub == "ledger" {
				e = r.Local.Ledger.Ready(ctx)
			} else if sub == "mock" {
				e = r.Local.Mock.Ready(ctx)
			} else {
				e = r.Local.Participant.Check(ctx)
			}
			if !errors.Is(e, expected) {
				t.Fatal("named independent proof", mode, e)
			}
			if sub == "ledger" {
				if _, err := r.Files.Get(ctx, scope, asset); !errors.Is(err, memory.ErrPreservation) {
					t.Fatal("fixed report metadata G gate", err)
				}
				one := bytes.Repeat([]byte{0xa5}, 64)
				if n, err := stream.Read(one); n != 0 || !errors.Is(err, memory.ErrPreservation) || !bytes.Equal(one, bytes.Repeat([]byte{0xa5}, 64)) {
					t.Fatal("live stream independent G gate", n, err)
				}
			}
			stream.Close()
			if _, e = r.Files.OpenAuthorized(ctx, scope, asset); e == nil {
				t.Fatal("actual asset accepted missing/wrong independent proof", mode)
			}
			q := httptest.NewRequest("GET", "/world/v1/admin/tasks?product_id=coffee", nil)
			q.Header.Set("Authorization", "Bearer "+runtimeToken)
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			if w.Code != 503 {
				t.Fatal("HTTP current proof", mode, w.Code, w.Body.String())
			}
			if e = os.WriteFile(path, original, 0600); e != nil {
				t.Fatal(e)
			}
			stream, e = r.Files.OpenAuthorized(ctx, scope, asset)
			if e != nil {
				t.Fatal("VALID after restoring exact independent original", e)
			}
			stream.Close()
		})
	}
}
