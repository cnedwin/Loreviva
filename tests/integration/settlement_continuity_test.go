package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOriginalCutoffsRequireIndependentJournal(t *testing.T) {
	t.Run("WORLD", func(t *testing.T) {
		r, scope, task, _ := originalResultFixture(t, true)
		expireCutoff(t, r.Store.DB, task)
		anchor := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "anchor.json")
		if e := os.Remove(anchor); e != nil {
			t.Fatal(e)
		}
		if _, e := r.Store.CutoffHarnessTask(context.Background(), scope, task); e == nil {
			t.Fatal("WORLD cutoff accepted missing independent journal anchor")
		}
		var count int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?", task).Scan(&count); e != nil || count != 0 {
			t.Fatal(count, e)
		}
	})
	t.Run("CANDIDATE", func(t *testing.T) {
		r, l := candidateDispatchFixture(t)
		expireCutoff(t, r.Store.DB, l.TaskID)
		anchor := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "anchor.json")
		if e := os.Remove(anchor); e != nil {
			t.Fatal(e)
		}
		if _, e := r.Store.CutoffDraftCandidate(context.Background(), l.ProductScope, l.TaskID); e == nil {
			t.Fatal("CANDIDATE cutoff accepted missing independent journal anchor")
		}
		var count int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?", l.TaskID).Scan(&count); e != nil || count != 0 {
			t.Fatal(count, e)
		}
	})
}

func TestOriginalCutoffRejectsReplacedTrustedJournal(t *testing.T) {
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	base := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock")
	anchorPath := filepath.Join(base, "anchor.json")
	journalPath := filepath.Join(base, "journal.jsonl")
	anchor, e := os.ReadFile(anchorPath)
	if e != nil {
		t.Fatal(e)
	}
	journal, e := os.ReadFile(journalPath)
	if e != nil {
		t.Fatal(e)
	}
	coordinatorPath := filepath.Join(r.Local.Coordinator.Directory(), "coordinator.json")
	coordinator, e := os.ReadFile(coordinatorPath)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"ANCHOR_INSTALLATION", "ANCHOR_STALE_HEAD", "JOURNAL_CORRUPT", "COORDINATOR_REPLACED", "PARTICIPANT_MISSING"} {
		t.Run(mode, func(t *testing.T) {
			defer os.WriteFile(anchorPath, anchor, 0600)
			defer os.WriteFile(journalPath, journal, 0600)
			defer os.WriteFile(coordinatorPath, coordinator, 0600)
			switch mode {
			case "JOURNAL_CORRUPT":
				e = os.WriteFile(journalPath, []byte("invalid-current-journal\n"), 0600)
			case "COORDINATOR_REPLACED", "PARTICIPANT_MISSING":
				var obj map[string]any
				if e = json.Unmarshal(coordinator, &obj); e != nil {
					t.Fatal(e)
				}
				if mode == "COORDINATOR_REPLACED" {
					obj["Installation"] = "other-installation"
				} else {
					obj["Participants"] = map[string]any{}
				}
				b, _ := json.Marshal(obj)
				e = os.WriteFile(coordinatorPath, b, 0600)
			default:
				var obj struct {
					Installation, Binding string
					Head                  domain.MockHead
				}
				if e = json.Unmarshal(anchor, &obj); e != nil {
					t.Fatal(e)
				}
				if mode == "ANCHOR_INSTALLATION" {
					obj.Installation = "other-installation"
				} else {
					obj.Head = domain.MockGenesis(obj.Installation, obj.Binding)
				}
				b, _ := json.Marshal(obj)
				e = os.WriteFile(anchorPath, b, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.CutoffHarnessTask(context.Background(), scope, task); e == nil {
				t.Fatal("replacement accepted")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
				t.Fatal(n, e)
			}
		})
	}
	if _, e = r.Store.CutoffHarnessTask(context.Background(), scope, task); e != nil {
		t.Fatal("original current evidence retry", e)
	}
}

type falseSettlementEvidence struct{}

func (falseSettlementEvidence) Close() {}

func TestSettlementEvidenceExactOwnerAndLiveIdentity(t *testing.T) {
	r, _ := candidateDispatchFixture(t)
	other, _ := candidateDispatchFixture(t)
	ctx, id, release, e := r.Store.SettlementAuthority.EnterDeadlineSettlement(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	evidence, e := r.Local.Mock.ReadSettlementContinuity(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	if _, e = r.Local.Mock.CheckSettlementContinuity(evidence, id); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"wrong-owner", "foreign-proof", "generation", "installation", "policy", "closed"} {
		t.Run(mode, func(t *testing.T) {
			changed := id
			var proof domain.SettlementContinuityEvidence = evidence
			owner := r.Local.Mock
			switch mode {
			case "wrong-owner":
				owner = other.Local.Mock
			case "foreign-proof":
				proof = falseSettlementEvidence{}
			case "generation":
				changed.Generation++
			case "installation":
				changed.Installation = "different-installation"
			case "policy":
				changed.Policy = "different-policy"
			case "closed":
				evidence.Close()
			}
			if _, e := owner.CheckSettlementContinuity(proof, changed); e == nil {
				t.Fatal("invalid settlement proof accepted")
			}
		})
	}
}

func TestOriginalCutoffHoldsContinuityThroughActualTransaction(t *testing.T) {
	for _, mode := range []string{"CLOSING_DRAIN", "FINISH_GENERATION", "FINISH_SERVICE"} {
		t.Run(mode, func(t *testing.T) {
			r, l := candidateDispatchFixture(t)
			expireCutoff(t, r.Store.DB, l.TaskID)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			var injected bool
			r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
				injected = true
				if mode == "FINISH_GENERATION" {
					_, e := tx.ExecContext(c, "UPDATE memory_installation SET generation=generation+1")
					return e
				}
				if mode == "FINISH_SERVICE" {
					_, e := tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
					return e
				}
				go func() {
					e := r.Local.Coordinator.BeginClose(ctx, "cutoff-close")
					if e == nil {
						e = r.Local.Coordinator.Drain(ctx, "cutoff-close")
					}
					finished <- e
				}()
				select {
				case e := <-finished:
					return errors.New("closing/drain crossed live SQL proof: " + fmt.Sprint(e))
				case <-time.After(60 * time.Millisecond):
				}
				short, done := context.WithTimeout(c, 60*time.Millisecond)
				defer done()
				if proof, e := r.Local.Mock.ReadSettlementContinuity(short); e == nil {
					proof.Close()
					return errors.New("new work crossed held coordinator generation")
				}
				return nil
			}
			out, e := r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
			r.Store.BeforeCommit = nil
			if !injected {
				t.Fatal("finisher did not run", e)
			}
			if mode == "CLOSING_DRAIN" {
				if e != nil || out.State != "CUTOFF" {
					t.Fatal(out, e)
				}
				select {
				case e := <-finished:
					if e != nil {
						t.Fatal(e)
					}
				case <-ctx.Done():
					t.Fatal("closing/drain leaked", ctx.Err())
				}
				if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e == nil {
					t.Fatal("closed installation replay authorized")
				}
			} else {
				if e == nil {
					t.Fatal("finisher mutation committed")
				}
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?", l.TaskID).Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
				if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
					t.Fatal("rollback original retry", e)
				}
			}
		})
	}
}

func TestHeldSettlementJournalBlocksClosingUntilRelease(t *testing.T) {
	r, _ := candidateDispatchFixture(t)
	ctx := context.Background()
	evidence, e := r.Local.Mock.ReadSettlementContinuity(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if e = r.Local.Coordinator.BeginClose(short, "settlement-drain"); e == nil {
		t.Fatal("maintenance changed installation generation through live settlement proof")
	}
	evidence.Close()
	if e = r.Local.Coordinator.BeginClose(ctx, "settlement-drain"); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Coordinator.Drain(ctx, "settlement-drain"); e != nil {
		t.Fatal(e)
	}
	if proof, e := r.Local.Mock.ReadSettlementContinuity(ctx); e == nil {
		proof.Close()
		t.Fatal("closed installation minted settlement proof")
	}
}
