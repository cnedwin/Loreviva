package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func memoryCrashCommand(node string) memory.MemoryGovernanceCommand {
	return memory.MemoryGovernanceCommand{Key: "crash-govern", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node}, Expected: map[string]uint64{node: 1}}}
}
func TestMemoryCrashHelper(t *testing.T) {
	mode := os.Getenv("LOREVIVA_MEMORY_CRASH")
	if mode == "" {
		t.Skip("child process only")
	}
	ctx := context.Background()
	s := store.New(dbfixture.ReopenHelper(t))
	var authError error
	ctx, _, authError = s.AuthenticateOperator(ctx, "Bearer "+runtimeToken)
	if authError != nil {
		t.Fatal(authError)
	}
	local, e := app.OpenLocal(ctx, s, os.Getenv("LOREVIVA_MEMORY_ROOT"), os.Getenv("LOREVIVA_MEMORY_FILES"))
	if e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, os.Getenv("LOREVIVA_MEMORY_FILES"))
	if e != nil {
		t.Fatal(e)
	}
	svc := memory.New(s, local.Ledger)
	svc.Files = f
	f.MemoryPolicy = svc
	n := memory.Namespace{Scope: domain.Scope{ProductScope: domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, WorldInstanceID: os.Getenv("LOREVIVA_MEMORY_WORLD")}, DataSubjectID: "creator"}
	node, actor := os.Getenv("LOREVIVA_MEMORY_NODE"), os.Getenv("LOREVIVA_MEMORY_ACTOR")
	matches := func(tx *sql.Tx) bool {
		var yes bool
		query := ""
		switch mode {
		case "stage-before-bytes":
			query = "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_DERIVED_STAGE')"
		case "bytes-before-bind":
			query = "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_DERIVED_FILE')"
		case "bound-before-finalize":
			query = "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_DERIVED_FILE')"
		case "G0":
			query = "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_GOVERNANCE_ACCEPT')"
		case "G1":
			query = "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='MEMORY_GOVERNANCE_ACCEPT')"
		case "G2":
			query = "SELECT EXISTS(SELECT 1 FROM tasks WHERE task_kind='GOVERNANCE' AND lease_owner='memory-preserver')"
		case "G3":
			query = "SELECT EXISTS(SELECT 1 FROM memory_governance WHERE preservation='CONFIRMED')"
		}
		if e := tx.QueryRowContext(ctx, query).Scan(&yes); e != nil {
			t.Fatal(e)
		}
		return yes
	}
	if mode == "bytes-before-bind" || mode == "G0" || mode == "G3" {
		s.BeforeCommit = func(_ context.Context, tx *sql.Tx) error {
			if matches(tx) {
				os.Exit(54)
			}
			return nil
		}
	} else {
		s.Commit = func(_ context.Context, tx *sql.Tx) error {
			crash := matches(tx)
			if e := tx.Commit(); e != nil {
				return e
			}
			if crash {
				os.Exit(54)
			}
			return nil
		}
	}
	if strings.HasPrefix(mode, "G") {
		_, e = svc.Govern(ctx, domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: actor, PlayerID: "creator"}, memoryCrashCommand(node))
	} else {
		_, e = svc.RenderPrivateText(ctx, n, actor, node, "crash-render")
	}
	t.Fatal("crash boundary not reached", e)
}
func TestMemoryActualProcessCrashBoundaries(t *testing.T) {
	for _, mode := range []string{"stage-before-bytes", "bytes-before-bind", "bound-before-finalize", "G0", "G1", "G2", "G3"} {
		t.Run(mode, func(t *testing.T) {
			r, s, release := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "process-crash"), "START")
			ctx := runtimeContext(t, r)
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			for _, right := range []string{"memory.govern", "memory.private_body.read"} {
				if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
					t.Fatal(e)
				}
			}
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var a memory.Aggregate
			if e := json.Unmarshal(raw, &a); e != nil {
				t.Fatal(e)
			}
			var node memory.MemoryRecord
			for _, v := range a.Nodes {
				node = v
				break
			}
			var database string
			if e := r.Store.DB.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryCrashHelper$")
			cmd.Env = append(os.Environ(), "LOREVIVA_MEMORY_CRASH="+mode, "LOREVIVA_HELPER_DATABASE="+database, "LOREVIVA_MEMORY_ROOT="+filepath.Dir(r.Local.Coordinator.Directory()), "LOREVIVA_MEMORY_FILES="+r.Files.RootPath, "LOREVIVA_MEMORY_WORLD="+i.Scope.WorldInstanceID, "LOREVIVA_MEMORY_NODE="+node.ID, "LOREVIVA_MEMORY_ACTOR="+node.ActorID)
			output, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 54 {
				t.Fatalf("child did not crash at %s: %v %s", mode, e, output)
			}
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_kind='GOVERNANCE'"); e != nil {
				t.Fatal(e)
			}
			scope := domain.RelationshipScope{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, ActorInstanceID: node.ActorID, PlayerID: s.SubjectID}
			receipt, e := r.Memory.Govern(ctx, scope, memoryCrashCommand(node.ID))
			if e != nil || receipt.Cleanup != "LOCAL_COMPLETE" {
				t.Fatal("original crash recovery", receipt, e)
			}
			var pending int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1 AND purged=0").Scan(&pending); e != nil || pending != 0 {
				t.Fatal("staging obligation not purged", pending, e)
			}
			matches, e := filepath.Glob(filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, "*.*"))
			if e != nil {
				t.Fatal(e)
			}
			for _, name := range matches {
				if strings.HasSuffix(name, ".blob") || strings.HasSuffix(name, ".part") {
					t.Fatal("private crash bytes survived governance", name)
				}
			}
			n := memory.Namespace{Scope: scope.Scope, DataSubjectID: s.SubjectID}
			if _, e = r.Memory.RenderPrivateText(ctx, n, node.ActorID, node.ID, "late-after-delete"); e == nil {
				t.Fatal("late render revived erased node")
			}
			if e = r.Local.Ledger.Ready(ctx); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestMemorySealCrashHelper(t *testing.T) {
	mode := os.Getenv("LOREVIVA_SEAL_CRASH")
	if mode == "" {
		t.Skip("child only")
	}
	ctx := context.Background()
	s := store.New(dbfixture.ReopenHelper(t))
	var id, db, files string
	if e := s.DB.QueryRow("SELECT installation_id,database_binding,files_binding FROM memory_installation").Scan(&id, &db, &files); e != nil {
		t.Fatal(e)
	}
	root := os.Getenv("LOREVIVA_MEMORY_ROOT")
	c, e := maintenance.OpenCoordinator(filepath.Join(root, "coordination"), id, db, files)
	if e != nil {
		t.Fatal(e)
	}
	p, done, e := c.AcquireReplay(ctx, "seal-plan")
	if e != nil {
		t.Fatal(e)
	}
	defer done()
	l, e := memory.OpenLedger(filepath.Join(root, "ledger"), id, "governance", "trusted-anchor")
	if e != nil {
		t.Fatal(e)
	}
	svc := memory.New(s, l)
	h, e := l.LatestHead(ctx, "child-seal")
	if e != nil {
		t.Fatal(e)
	}
	if e = svc.ReplayDeny(ctx, p, h); e != nil {
		t.Fatal(e)
	}
	if mode == "BEFORE_SEAL" {
		os.Exit(55)
	}
	candidate, e := svc.SealReplayOpenCandidate(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if mode == "SEALED" {
		os.Exit(55)
	}
	if mode == "DB_BOUND" {
		s.Commit = func(_ context.Context, tx *sql.Tx) error {
			if e := tx.Commit(); e != nil {
				return e
			}
			os.Exit(55)
			return nil
		}
	}
	receipt, e := svc.BindReplayOpenCandidate(ctx, p, candidate)
	if e != nil {
		t.Fatal(e)
	}
	if e = svc.ConfirmGovernanceReady(ctx, p, candidate, receipt); e != nil {
		t.Fatal(e)
	}
	if mode == "G_READY" {
		os.Exit(55)
	}
	t.Fatal("no crash")
}
func TestMemorySealCrashRecoveryRemainsClosedAndRejectsForgedPermits(t *testing.T) {
	for _, mode := range []string{"BEFORE_SEAL", "SEALED", "DB_BOUND", "G_READY"} {
		t.Run(mode, func(t *testing.T) {
			r, _, release := runtimeFixture(t)
			_ = createRuntimeWorld(t, r, release, "seal-crash")
			ctx := runtimeContext(t, r)
			if e := r.Local.Coordinator.BeginClose(ctx, "seal-plan"); e != nil {
				t.Fatal(e)
			}
			if e := r.Local.Coordinator.Drain(ctx, "seal-plan"); e != nil {
				t.Fatal(e)
			}
			var database string
			if e := r.Store.DB.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
				t.Fatal(e)
			}
			root := filepath.Dir(r.Local.Coordinator.Directory())
			cmd := exec.Command(os.Args[0], "-test.run=^TestMemorySealCrashHelper$")
			cmd.Env = append(os.Environ(), "LOREVIVA_SEAL_CRASH="+mode, "LOREVIVA_HELPER_DATABASE="+database, "LOREVIVA_MEMORY_ROOT="+root)
			out, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 55 {
				t.Fatal(mode, e, string(out))
			}
			raw, e := os.ReadFile(filepath.Join(root, "coordination", "coordinator.json"))
			if e != nil {
				t.Fatal(e)
			}
			var state struct {
				Phase      string
				Governance *maintenance.GovernanceSeal
			}
			if json.Unmarshal(raw, &state) != nil || (state.Governance == nil && mode != "BEFORE_SEAL") || state.Phase != "CLOSED" {
				t.Fatal("crash lost closed seal")
			}
			originalID := ""
			if state.Governance != nil {
				originalID = state.Governance.CandidateID
			}
			p, done, e := r.Local.Coordinator.AcquireReplay(ctx, "seal-plan")
			if e != nil {
				t.Fatal(e)
			}
			defer done()
			candidate, e := r.Memory.SealReplayOpenCandidate(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Memory.ConfirmGovernanceReady(ctx, p, candidate, nil); e == nil {
				t.Fatal("missing committed receipt accepted")
			}
			receipt, e := r.Memory.BindReplayOpenCandidate(ctx, p, candidate)
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Memory.ConfirmGovernanceReady(ctx, p, candidate, receipt); e != nil {
				t.Fatal(e)
			}
			raw, e = os.ReadFile(filepath.Join(root, "coordination", "coordinator.json"))
			if e != nil {
				t.Fatal(e)
			}
			if json.Unmarshal(raw, &state) != nil || state.Phase != "CLOSED" || state.Governance.State != "G_READY" || (originalID != "" && state.Governance.CandidateID != originalID) {
				t.Fatal("same candidate did not recover G-only", state)
			}
			var b store.MemoryCandidateBinding
			b = state.Governance.MemoryCandidateBinding
			if _, e = r.Store.BindMemoryGovernanceCandidate(ctx, nil, b); e == nil {
				t.Fatal("nil permit")
			}
			if _, e = r.Store.BindMemoryGovernanceCandidate(ctx, &store.MemoryReplayPermit{}, b); e == nil {
				t.Fatal("zero forged permit")
			}
			permit, e := p.MemoryStorePermit(ctx)
			if e != nil {
				t.Fatal(e)
			}
			// An actual exclusive handle on another root cannot authorize this database.
			var installation, db, files string
			if e = r.Store.DB.QueryRow("SELECT installation_id,database_binding,files_binding FROM memory_installation").Scan(&installation, &db, &files); e != nil {
				t.Fatal(e)
			}
			other, e := maintenance.CreateCoordinator(t.TempDir(), installation, db, files)
			if e != nil {
				t.Fatal(e)
			}
			if e = other.BeginClose(ctx, "seal-plan"); e != nil {
				t.Fatal(e)
			}
			if e = other.Drain(ctx, "seal-plan"); e != nil {
				t.Fatal(e)
			}
			op, odone, e := other.AcquireReplay(ctx, "seal-plan")
			if e != nil {
				t.Fatal(e)
			}
			otherPermit, e := op.MemoryStorePermit(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.BindMemoryGovernanceCandidate(ctx, otherPermit, b); e == nil {
				t.Fatal("different root permit accepted")
			}
			odone()
			unrelated, e := localstate.Open(t.TempDir(), true)
			if e != nil {
				t.Fatal(e)
			}
			handle, e := unrelated.ExclusiveAdmission(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if e = handle.BindClosed(ctx, localstate.ClosedBinding{Installation: installation, Database: db, Files: files, Plan: "seal-plan", Generation: 2}); e == nil {
				t.Fatal("arbitrary leaf root forged CLOSED")
			}
			handle.Close()
			done()
			if _, e = r.Store.BindMemoryGovernanceCandidate(ctx, permit, b); e == nil {
				t.Fatal("released permit accepted")
			}
			if e = r.Memory.ConfirmGovernanceReady(ctx, p, candidate, receipt); e == nil {
				t.Fatal("released candidate accepted")
			}
			if _, e = app.OpenLocal(ctx, r.Store, root, r.Files.RootPath); e == nil {
				t.Fatal("G-only reopened old business state")
			}
		})
	}
}
