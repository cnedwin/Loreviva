package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func TestOriginalMeterPageHasOneHundredActualCommittedAttempts(t *testing.T) {
	ctx := context.Background()
	r, draft, req := candidateHTTPFixture(t)
	for n := 0; n < 101; n++ {
		addPendingCandidateMeter(t, r, draft, req, fmt.Sprintf("page-meter-%03d", n), false)
	}
	var committed, service, unknown, calls, steps int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts WHERE operation='CANDIDATE_COMMIT'),(SELECT COUNT(*) FROM service_events WHERE status='PENDING'),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN'),SUM(calls_allocated),SUM(steps_used) FROM harness_envelopes").Scan(&committed, &service, &unknown, &calls, &steps); e != nil || committed != 101 || service != 101 || unknown != 101 || calls != 101 || steps != 909 {
		t.Fatal("not actual committed page", committed, service, unknown, calls, steps, e)
	}
	start := time.Now()
	done, failed, e := r.Store.RunOriginalServiceMetering(ctx)
	if e != nil || done != 0 || failed != 100 || time.Since(start) > 30*time.Second {
		t.Fatal(done, failed, time.Since(start), e)
	}
	var retried, untouched int
	if e = r.Store.DB.QueryRow("SELECT SUM(meter_failures=1),SUM(meter_failures=0) FROM harness_attempt_metering").Scan(&retried, &untouched); e != nil || retried != 100 || untouched != 1 {
		t.Fatal(retried, untouched, e)
	}
	done, failed, e = r.Store.RunOriginalServiceMetering(ctx)
	if e != nil || done != 0 || failed != 1 {
		t.Fatal("101st actual attempt starved", done, failed, e)
	}
	var lines int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_service_lines").Scan(&lines); e != nil || lines != 0 {
		t.Fatal("unknown units fabricated", lines, e)
	}
}

func TestOriginalMeterOrdinaryProcessReopen(t *testing.T) {
	ctx := context.Background()
	r, id := pendingCandidateMeter(t, true)
	var database string
	if e := r.Store.DB.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
		t.Fatal(e)
	}
	if !regexp.MustCompile(`^loreviva_test_[0-9a-f]{24}$`).MatchString(database) {
		t.Fatal("not isolated owned fixture")
	}
	if e := r.Local.Participant.Close(ctx); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	args := []string{"-test.run=^TestOriginalMeterReopenChildProcess$", "-test.v", "-test.timeout=25s"}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, executable, args...)
	command.Env = append(os.Environ(), "LOREVIVA_METER_REOPEN_DB="+database, "LOREVIVA_METER_REOPEN_ROOT="+filepath.Dir(r.Local.Coordinator.Directory()), "LOREVIVA_METER_REOPEN_FILES="+r.Files.RootPath)
	t.Log("actual helper executable", executable, "args", args)
	out, e := command.CombinedOutput()
	t.Log(string(out))
	if e != nil {
		t.Fatal("ordinary child process reopen", e)
	}
	var state string
	var count, lines, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT s.status,(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),e.calls_allocated,e.steps_used FROM service_events s JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE s.task_id=?", id.TaskID).Scan(&state, &count, &lines, &calls, &steps); e != nil || state != "COMMITTED" || count != 1 || lines != 1 || calls != 1 || steps != 9 {
		t.Fatal(state, count, lines, calls, steps, e)
	}
}

// Invoked only by the exact owning fixture above. No database creation, schema
// setup, bootstrap or source input is performed in the reopened process.
func TestOriginalMeterReopenChildProcess(t *testing.T) {
	database := os.Getenv("LOREVIVA_METER_REOPEN_DB")
	if database == "" {
		t.Skip("only original fixture subprocess")
	}
	if !regexp.MustCompile(`^loreviva_test_[0-9a-f]{24}$`).MatchString(database) {
		t.Fatal("invalid isolated fixture name")
	}
	cfg, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("invalid private fixture configuration")
	}
	if cfg.Addr != "127.0.0.1:33267" || cfg.User == "root" {
		t.Fatal("invalid fixture authority")
	}
	cfg.DBName = database
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	db, e := sql.Open("mysql", cfg.FormatDSN())
	if e != nil {
		t.Fatal("fixture connect unavailable")
	}
	defer db.Close()
	s := store.New(db)
	ctx := context.Background()
	local, e := app.OpenLocal(ctx, s, os.Getenv("LOREVIVA_METER_REOPEN_ROOT"), os.Getenv("LOREVIVA_METER_REOPEN_FILES"))
	if e != nil {
		t.Fatal(e)
	}
	defer local.Participant.Close(ctx)
	done, failed, e := s.RunOriginalServiceMetering(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	done, failed, e = s.RunOriginalServiceMetering(ctx)
	if e != nil || done != 0 || failed != 0 {
		t.Fatal("reopen repeats metering", done, failed, e)
	}
}

func pendingCandidateMeter(t *testing.T, publish bool) (*app.Runtime, domain.MockRequestIdentity) {
	t.Helper()
	r, draft, req := candidateHTTPFixture(t)
	return r, addPendingCandidateMeter(t, r, draft, req, "meter-delivery", publish)
}

func addPendingCandidateMeter(t *testing.T, r *app.Runtime, draft string, req generated.CandidateRequest, key string, publish bool) domain.MockRequestIdentity {
	t.Helper()
	var accepted generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", key, req, 202, &accepted)
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e := r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT request_identity FROM harness_original_queries WHERE task_id=?", accepted.TaskID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var id domain.MockRequestIdentity
	if e := json.Unmarshal(raw, &id); e != nil {
		t.Fatal(e)
	}
	if publish {
		r.Worker.Turns.Fault = harness.MockFault(19)
		if e := r.Worker.Turns.DeliverLateMockBills(context.Background(), id); e != nil {
			t.Fatal(e)
		}
	}
	return id
}

func TestOriginalMeterDeliveryLockWaitAndUnpersistedFailure(t *testing.T) {
	for _, mode := range []string{"TASK_LOCK", "WORK_LOCK", "EXHAUSTED_CONTEXT"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, id := pendingCandidateMeter(t, mode != "WORK_LOCK")
			blocker, e := r.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			query := "SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE"
			if mode == "WORK_LOCK" {
				query = "SELECT task_id FROM harness_attempt_metering WHERE task_id=? FOR UPDATE"
			}
			var original string
			if e = blocker.QueryRow(query, id.TaskID).Scan(&original); e != nil {
				t.Fatal(e)
			}
			wait := 8 * time.Second
			if mode == "EXHAUSTED_CONTEXT" {
				wait = 80 * time.Millisecond
			}
			bounded, cancel := context.WithTimeout(ctx, wait)
			defer cancel()
			start := time.Now()
			done, failed, runErr := r.Store.RunOriginalServiceMetering(bounded)
			elapsed := time.Since(start)
			t.Log("actual original meter item wall", elapsed)
			if done != 0 || elapsed > 5500*time.Millisecond {
				t.Fatal(done, failed, elapsed, runErr)
			}
			if mode == "TASK_LOCK" && (runErr != nil || failed != 1 || elapsed < 3*time.Second) {
				t.Fatal(done, failed, elapsed, runErr)
			}
			if mode != "TASK_LOCK" && runErr == nil {
				t.Fatal("failed record/context unexpectedly succeeded")
			}
			if e = blocker.Rollback(); e != nil {
				t.Fatal(e)
			}
			var failures, numeric, lines, calls, steps int
			var due bool
			if e = r.Store.DB.QueryRow("SELECT a.meter_failures,a.meter_next_run_at<=UTC_TIMESTAMP(6),a.input_units IS NOT NULL OR a.output_units IS NOT NULL,(SELECT COUNT(*) FROM harness_service_lines),e.calls_allocated,e.steps_used FROM harness_attempt_metering a JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.task_id=?", id.TaskID).Scan(&failures, &due, &numeric, &lines, &calls, &steps); e != nil {
				t.Fatal(e)
			}
			if numeric != 0 || lines != 0 || calls != 1 || steps != 9 {
				t.Fatal("partial metering/narrative", numeric, lines, calls, steps)
			}
			if mode == "TASK_LOCK" {
				if failures != 1 || due {
					t.Fatal(failures, due)
				}
			} else if failures != 0 || !due {
				t.Fatal("unpersisted retry skipped original work", failures, due)
			}
			if mode == "WORK_LOCK" {
				r.Worker.Turns.Fault = harness.MockFault(19)
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "TASK_LOCK" {
				if _, e = r.Store.DB.Exec("UPDATE harness_attempt_metering SET meter_next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", id.TaskID); e != nil {
					t.Fatal(e)
				}
			}
			done, failed, e = r.Store.RunOriginalServiceMetering(ctx)
			if e != nil || done != 1 || failed != 0 {
				t.Fatal("original pending work lost", done, failed, e)
			}
		})
	}
}

func TestOriginalMeterRetryAckAndDedicatedSession(t *testing.T) {
	for _, mode := range []string{"RECORD_COMMIT_ACK", "RECORD_ROLLBACK_ACK", "SESSION", "DISCARD"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, id := pendingCandidateMeter(t, false)
			r.Store.DB.SetMaxOpenConns(1)
			r.Store.DB.SetMaxIdleConns(1)
			var before, after uint64
			if e := r.Store.DB.QueryRow("SELECT CONNECTION_ID()").Scan(&before); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("SET SESSION innodb_lock_wait_timeout=19"); e != nil {
				t.Fatal(e)
			}
			bounded, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode != "SESSION" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if mode == "RECORD_ROLLBACK_ACK" {
						if e := tx.Rollback(); e != nil {
							return e
						}
						return errors.New("synthetic retry rollback ACK")
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					if mode == "DISCARD" {
						cancel()
						return nil
					}
					return errors.New("synthetic retry commit ACK")
				}
			}
			done, failed, e := r.Store.RunOriginalServiceMetering(bounded)
			r.Store.Commit = nil
			if done != 0 || failed != 1 {
				t.Fatal(done, failed, e)
			}
			if mode == "RECORD_COMMIT_ACK" || mode == "RECORD_ROLLBACK_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			var failures, timeout int
			var due bool
			if e = r.Store.DB.QueryRow("SELECT meter_failures,meter_next_run_at<=UTC_TIMESTAMP(6) FROM harness_attempt_metering WHERE task_id=?", id.TaskID).Scan(&failures, &due); e != nil {
				t.Fatal(e)
			}
			if mode == "RECORD_ROLLBACK_ACK" {
				if failures != 0 || !due {
					t.Fatal(failures, due)
				}
			} else if failures != 1 || due {
				t.Fatal(failures, due)
			}
			if e = r.Store.DB.QueryRow("SELECT CONNECTION_ID(),@@SESSION.innodb_lock_wait_timeout").Scan(&after, &timeout); e != nil {
				t.Fatal(e)
			}
			if mode == "DISCARD" {
				if after == before || timeout == 4 || timeout == 19 {
					t.Fatal("polluted session reused", before, after, timeout)
				}
			} else if after != before || timeout != 19 {
				t.Fatal("session not restored", before, after, timeout)
			}
			r.Worker.Turns.Fault = harness.MockFault(19)
			if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_attempt_metering SET meter_next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", id.TaskID); e != nil {
				t.Fatal(e)
			}
			done, failed, e = r.Store.RunOriginalServiceMetering(ctx)
			if e != nil || done != 1 || failed != 0 {
				t.Fatal(done, failed, e)
			}
			done, failed, e = r.Store.RunOriginalServiceMetering(ctx)
			if e != nil || done != 0 || failed != 0 {
				t.Fatal("duplicate service", done, failed, e)
			}
		})
	}
}

func TestOriginalMeterWholePassIncludesSlowItemsAndCleanup(t *testing.T) {
	ctx := context.Background()
	r, draft, req := candidateHTTPFixture(t)
	var ids []domain.MockRequestIdentity
	for n := 0; n < 8; n++ {
		ids = append(ids, addPendingCandidateMeter(t, r, draft, req, fmt.Sprintf("slow-meter-%d", n), true))
	}
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	for _, id := range ids {
		var task string
		if e = blocker.QueryRow("SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE", id.TaskID).Scan(&task); e != nil {
			t.Fatal(e)
		}
	}
	start := time.Now()
	done, failed, runErr := r.Store.RunOriginalServiceMetering(ctx)
	elapsed := time.Since(start)
	t.Log("actual full-pass wall including failure/cleanup", elapsed, "done", done, "failed", failed)
	if elapsed < 28*time.Second || elapsed > 31*time.Second || done != 0 || failed < 7 || runErr == nil {
		t.Fatal(done, failed, elapsed, runErr)
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	var unknown, due, lines int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*),SUM(meter_next_run_at<=UTC_TIMESTAMP(6)),(SELECT COUNT(*) FROM harness_service_lines) FROM harness_attempt_metering WHERE usage_state='UNKNOWN'").Scan(&unknown, &due, &lines); e != nil || unknown != 8 || due < 1 || lines != 0 {
		t.Fatal(unknown, due, lines, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_attempt_metering SET meter_next_run_at=UTC_TIMESTAMP(6)"); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunOriginalServiceMetering(ctx)
	if e != nil || done != 8 || failed != 0 {
		t.Fatal("original work skipped after whole-pass exhaustion", done, failed, e)
	}
}

func TestOriginalMeterCurrentEvidenceAndFinisher(t *testing.T) {
	for _, mode := range []string{"J_MISSING", "J_CORRUPT", "J_REPLACED", "SERVICE", "GENERATION", "MARKER_HASH", "CLOSING_DRAIN"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, id := pendingCandidateMeter(t, true)
			finished := make(chan error, 1)
			if mode == "J_MISSING" || mode == "J_CORRUPT" || mode == "J_REPLACED" {
				name := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "anchor.json")
				if mode == "J_CORRUPT" {
					name = filepath.Join(filepath.Dir(name), "journal.jsonl")
				}
				original, e := os.ReadFile(name)
				if e != nil {
					t.Fatal(e)
				}
				defer os.WriteFile(name, original, 0600)
				if mode == "J_MISSING" {
					e = os.Remove(name)
				} else if mode == "J_CORRUPT" {
					e = os.WriteFile(name, []byte("invalid\n"), 0600)
				} else {
					var anchor map[string]any
					if e = json.Unmarshal(original, &anchor); e != nil {
						t.Fatal(e)
					}
					anchor["Installation"] = "other-installation"
					changed, _ := json.Marshal(anchor)
					e = os.WriteFile(name, changed, 0600)
				}
				if e != nil {
					t.Fatal(e)
				}
			} else {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					switch mode {
					case "SERVICE":
						_, e := tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
						return e
					case "GENERATION":
						_, e := tx.ExecContext(c, "UPDATE memory_installation SET generation=generation+1")
						return e
					case "MARKER_HASH":
						_, e := tx.ExecContext(c, "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ab',32)) WHERE commit_key=?", id.MarkerKey)
						return e
					case "CLOSING_DRAIN":
						go func() {
							bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
							defer cancel()
							e := r.Local.Coordinator.BeginClose(bounded, "meter-current-close")
							if e == nil {
								e = r.Local.Coordinator.Drain(bounded, "meter-current-close")
							}
							finished <- e
						}()
						select {
						case e := <-finished:
							return fmt.Errorf("closing crossed active metering SQL: %v", e)
						case <-time.After(60 * time.Millisecond):
						}
						short, cancel := context.WithTimeout(c, 60*time.Millisecond)
						defer cancel()
						proof, e := r.Local.Mock.ReadOriginalUsageEvidence(short, id)
						if e == nil {
							proof.Close()
							return errors.New("new usage work crossed held installation proof")
						}
					}
					return nil
				}
			}
			_, e := r.Store.ReconcileOriginalServiceMetering(ctx, id)
			r.Store.BeforeCommit = nil
			if mode == "CLOSING_DRAIN" {
				if e != nil {
					t.Fatal(e)
				}
				select {
				case e = <-finished:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(9 * time.Second):
					t.Fatal("meter lifetime leaked")
				}
				if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e == nil {
					t.Fatal("closed original metering replay authorized")
				}
			} else {
				if e == nil {
					t.Fatal("invalid current original metering proof committed")
				}
				var lines, unknown int
				var state string
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN'),status FROM service_events").Scan(&lines, &unknown, &state); e != nil || lines != 0 || unknown != 1 || state != "PENDING" {
					t.Fatal("partial metering commit", lines, unknown, state, e)
				}
			}
		})
	}
}

func TestOriginalMeterOpaqueEvidenceAndOrdinaryServiceReopen(t *testing.T) {
	t.Run("OPAQUE", func(t *testing.T) {
		ctx := context.Background()
		r, id := pendingCandidateMeter(t, true)
		ctx, service, release, e := r.Store.SettlementAuthority.EnterDeadlineSettlement(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer release()
		evidence, e := r.Local.Mock.ReadOriginalUsageEvidence(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Local.Mock.CheckOriginalUsageEvidence(evidence, service); e != nil {
			evidence.Close()
			t.Fatal(e)
		}
		wrong := service
		wrong.Generation++
		if e = r.Local.Mock.CheckOriginalUsageEvidence(evidence, wrong); e == nil {
			evidence.Close()
			t.Fatal("old generation accepted")
		}
		wrong = service
		wrong.Installation = "different-installation"
		if e = r.Local.Mock.CheckOriginalUsageEvidence(evidence, wrong); e == nil {
			evidence.Close()
			t.Fatal("different installation accepted")
		}
		evidence.Close()
		if e = r.Local.Mock.CheckOriginalUsageEvidence(evidence, service); e == nil {
			t.Fatal("closed original evidence reused")
		}
		wrongID := id
		wrongID.RequestHash = "different-original-hash"
		if proof, e := r.Local.Mock.ReadOriginalUsageEvidence(ctx, wrongID); e == nil {
			proof.Close()
			t.Fatal("different original request accepted")
		}
		other, _, _ := candidateHTTPFixture(t)
		if e = other.Local.Mock.CheckOriginalUsageEvidence(evidence, service); e == nil {
			t.Fatal("another owning journal accepted evidence")
		}
		if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e != nil {
			t.Fatal("original proof acquisition after Close", e)
		}
	})
	for _, mode := range []string{"DISABLED", "MISSING"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := candidateHTTPFixture(t)
			var id domain.MockRequestIdentity
			if mode == "DISABLED" {
				id = addPendingCandidateMeter(t, r, draft, req, "disabled-reopen", true)
			}
			query := "DELETE FROM harness_settlement_services"
			if mode == "DISABLED" {
				query = "UPDATE harness_settlement_services SET enabled=0"
			}
			if _, e := r.Store.DB.Exec(query); e != nil {
				t.Fatal(e)
			}
			if e := r.Local.Participant.Close(ctx); e != nil {
				t.Fatal(e)
			}
			local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
			if mode == "MISSING" {
				if e == nil || local != nil {
					if local != nil {
						local.Participant.Close(ctx)
					}
					t.Fatal("missing settlement service did not close ordinary reopen")
				}
			} else {
				if e != nil {
					t.Fatal("ordinary reopen", e)
				}
				defer local.Participant.Close(ctx)
			}
			var available int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_settlement_services WHERE enabled=1").Scan(&available); e != nil || available != 0 {
				t.Fatal("ordinary reopen seeded settlement authority", available, e)
			}
			if mode == "MISSING" {
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_settlement_services").Scan(&available); e != nil || available != 0 {
					t.Fatal("missing service recreated", available, e)
				}
				return
			}
			if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e == nil {
				t.Fatal("disabled original service authorized metering")
			}
			var lines, unknown int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN')").Scan(&lines, &unknown); e != nil || lines != 0 || unknown != 1 {
				t.Fatal(lines, unknown, e)
			}
		})
	}
}
