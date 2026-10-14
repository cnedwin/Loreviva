package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
)

func TestActualGovernanceOldPairAndOldIndependentHead(t *testing.T) {
	r, scope, task, actor := originalResultFixture(t, false)
	ctx := runtimeContext(t, r)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	body, _, e := r.Files.ReadApprovedTurn(ctx, scope, task)
	if e != nil || len(body) == 0 {
		t.Fatal("VALID original committed bytes", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id=?", scope.SubjectID); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"memory.govern", "memory.source.govern"} {
		if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
			t.Fatal(e)
		}
	}
	oldDB, oldFiles := captureOwnedDatabase(t, r.Store.DB), captureOwnedFiles(t, r.Files.RootPath)
	ledgerDir := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "ledger")
	oldG := captureOwnedFiles(t, ledgerDir)
	var source string
	var raw []byte
	var a memory.Aggregate
	if e = r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", task).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, scope.SubjectID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	g, e := r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: actor, PlayerID: scope.SubjectID}, memory.MemoryGovernanceCommand{Key: "actual-govern-before-old-pair", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: a.Sources[source].Revision}}})
	if e != nil || g.Preservation != "CONFIRMED" || g.Cleanup != "LOCAL_COMPLETE" {
		t.Fatal("actual govern", g, e)
	}
	if e = r.Local.Participant.Check(ctx); e != nil {
		t.Fatal("same OPEN installation", e)
	}
	if e = r.Memory.CheckResultGovernance(ctx); e != nil {
		t.Fatal("VALID current G and governed DB", e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, scope, task); e == nil || len(b) != 0 {
		t.Fatal("governed body", e)
	}
	currentDB, currentFiles, currentG := captureOwnedDatabase(t, r.Store.DB), captureOwnedFiles(t, r.Files.RootPath), captureOwnedFiles(t, ledgerDir)
	coordPath := filepath.Join(r.Local.Coordinator.Directory(), "coordinator.json")
	currentCoord, e := os.ReadFile(coordPath)
	if e != nil {
		t.Fatal(e)
	}
	t.Run("OLD_PAIR_CURRENT_G", func(t *testing.T) {
		restoreOwnedDatabase(t, r.Store.DB, oldDB)
		restoreOwnedFiles(t, r.Files.RootPath, oldFiles)
		defer func() {
			restoreOwnedDatabase(t, r.Store.DB, currentDB)
			restoreOwnedFiles(t, r.Files.RootPath, currentFiles)
		}()
		if e := r.Local.Participant.Check(ctx); e != nil {
			t.Fatal("negative must retain OPEN qualification", e)
		}
		if e := r.Memory.CheckResultGovernance(ctx); !errors.Is(e, memory.ErrPreservation) {
			t.Fatal("missing original G application must reject old DB", e)
		}
		if b, _, e := r.Files.ReadApprovedTurn(ctx, scope, task); !errors.Is(e, memory.ErrPreservation) || len(b) != 0 {
			t.Fatal("old pair must not resurrect body", e)
		}
	})
	t.Run("OLD_G_CURRENT_TRUSTED_HEAD", func(t *testing.T) {
		restoreOwnedFiles(t, ledgerDir, oldG)
		defer restoreOwnedFiles(t, ledgerDir, currentG)
		if e := r.Local.Participant.Check(ctx); e != nil {
			t.Fatal("negative must retain OPEN qualification", e)
		}
		if e := r.Local.Ledger.Ready(ctx); !errors.Is(e, memory.ErrPreservation) {
			t.Fatal("self-consistent old G must not replace known head", e)
		}
	})
	for _, mode := range []string{"MISSING_BINDING", "WRONG_ACCEPT_RECEIPT", "WRONG_FINISH_RECEIPT"} {
		t.Run(mode, func(t *testing.T) {
			defer restoreOwnedDatabase(t, r.Store.DB, currentDB)
			query := "DELETE FROM memory_governance WHERE task_id=?"
			if mode == "WRONG_ACCEPT_RECEIPT" {
				query = "UPDATE commit_receipts SET operation='WRONG_ACCEPT' WHERE operation='MEMORY_GOVERNANCE_ACCEPT' AND world_instance_id=(SELECT world_instance_id FROM memory_governance WHERE task_id=?)"
			}
			if mode == "WRONG_FINISH_RECEIPT" {
				query = "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ab',32)) WHERE task_id=? AND operation='MEMORY_GOVERNANCE'"
			}
			if _, e := r.Store.DB.Exec(query, g.TaskID); e != nil {
				t.Fatal(e)
			}
			if e := r.Memory.CheckResultGovernance(ctx); !errors.Is(e, memory.ErrPreservation) {
				t.Fatal("exact original application evidence", e)
			}
		})
	}
	for _, mode := range []string{"CONFIRM_GAP", "MISSING_APPEND_BYTES", "UNRESERVED_TAIL", "MISSING_TRUSTED_GENESIS"} {
		t.Run(mode, func(t *testing.T) {
			defer func() {
				restoreOwnedFiles(t, ledgerDir, currentG)
				if e := os.WriteFile(coordPath, currentCoord, 0600); e != nil {
					t.Fatal(e)
				}
			}()
			var c map[string]json.RawMessage
			if e := json.Unmarshal(currentCoord, &c); e != nil {
				t.Fatal(e)
			}
			var continuity maintenance.GovernanceContinuity
			if e := json.Unmarshal(c["GovernanceContinuity"], &continuity); e != nil {
				t.Fatal(e)
			}
			var first memory.LedgerEntry
			lines := bytes.Split(currentG["journal.jsonl"], []byte{'\n'})
			if e := json.Unmarshal(lines[0], &first); e != nil {
				t.Fatal(e)
			}
			actual := continuity.Head
			continuity.Head = maintenance.GovernanceHead{Sequence: first.Sequence, Hash: first.Hash}
			continuity.Pending = &actual
			if mode == "UNRESERVED_TAIL" {
				continuity.Pending = nil
			}
			if mode == "MISSING_APPEND_BYTES" {
				restoreOwnedFiles(t, ledgerDir, oldG)
			}
			c["GovernanceContinuity"], _ = json.Marshal(continuity)
			if mode == "MISSING_TRUSTED_GENESIS" {
				delete(c, "GovernanceContinuity")
			}
			b, _ := json.Marshal(c)
			if e := os.WriteFile(coordPath, b, 0600); e != nil {
				t.Fatal(e)
			}
			if mode == "CONFIRM_GAP" {
				if e := r.Local.Ledger.Ready(ctx); e != nil {
					t.Fatal("exact actual append closes only original pending confirmation", e)
				}
				if e := r.Memory.CheckResultGovernance(ctx); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := r.Local.Ledger.Ready(ctx); !errors.Is(e, memory.ErrPreservation) {
					t.Fatal("missing trusted proof", e)
				}
				before := captureOwnedFiles(t, ledgerDir)
				if local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath); e == nil {
					local.Participant.Close(ctx)
					t.Fatal("ordinary reopen repaired unproven G")
				}
				for k, v := range before {
					after, e := os.ReadFile(filepath.Join(ledgerDir, k))
					if e != nil || !bytes.Equal(after, v) {
						t.Fatal("ordinary open changed unproven G", k, e)
					}
				}
			}
		})
	}
	if e = r.Memory.CheckResultGovernance(ctx); e != nil {
		t.Fatal("VALID restored current independent evidence", e)
	}
	end, e := os.ReadFile(coordPath)
	if e != nil || !bytes.Equal(currentCoord, end) {
		t.Fatal("read checks changed coordinator", e)
	}
}

func TestGovernanceAppendHeldLifetimeDrainsWithoutLateAppend(t *testing.T) {
	r, scope, _, _ := originalResultFixture(t, false)
	ctx := runtimeContext(t, r)
	ledgerDir := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "ledger")
	root, e := localstate.Open(ledgerDir, false)
	if e != nil {
		t.Fatal(e)
	}
	before := map[string][]byte{}
	for _, name := range []string{"anchor.json", "journal.jsonl"} {
		b, e := os.ReadFile(filepath.Join(ledgerDir, name))
		if e != nil {
			t.Fatal(e)
		}
		before[name] = b
	}
	hold, e := root.Lock(ctx, "state.lock")
	if e != nil {
		t.Fatal(e)
	}
	defer hold()
	done := make(chan error, 1)
	go func() {
		done <- r.Local.Ledger.ReserveOnce(ctx, memory.Reservation{ID: "held-governance-reservation", RequestHash: "actual-hash", Scope: []string{scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID, scope.SubjectID}})
	}()
	coord, e := localstate.Open(r.Local.Coordinator.Directory(), false)
	if e != nil {
		hold()
		t.Fatal(e)
	}
	// Exclusive admission must actually observe the append's held shared lease.
	deadline := time.Now().Add(time.Second)
	admitted := false
	for time.Now().Before(deadline) {
		short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		unlock, e := coord.Lock(short, "operations.lock")
		cancel()
		if e != nil {
			admitted = true
			break
		}
		unlock()
		time.Sleep(time.Millisecond)
	}
	if !admitted {
		hold()
		<-done
		t.Fatal("append did not enter shared lifetime")
	}
	if e = r.Local.Coordinator.BeginClose(ctx, "held-g-append"); e != nil {
		hold()
		t.Fatal(e)
	}
	drained := make(chan error, 1)
	go func() { drained <- r.Local.Coordinator.Drain(ctx, "held-g-append") }()
	select {
	case e := <-drained:
		hold()
		t.Fatal("drain passed held governance append", e)
	case <-time.After(60 * time.Millisecond):
	}
	hold()
	if e = <-done; !errors.Is(e, memory.ErrPreservation) {
		t.Fatal("retired append", e)
	}
	select {
	case e = <-drained:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("drain deadlocked")
	}
	for k, v := range before {
		after, e := os.ReadFile(filepath.Join(ledgerDir, k))
		if e != nil || !bytes.Equal(v, after) {
			t.Fatal("closed generation appended", k, e)
		}
	}
}
