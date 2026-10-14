package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func physicalRehearsalDraft(t *testing.T) (*app.Runtime, string, []string, generated.RehearsalRequest) {
	t.Helper()
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	assets := []string{}
	rights := generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "rehearsal source", License: "Apache-2.0", AllowedUses: []string{"EDIT"}}
	for n := 0; n < 2; n++ {
		body := []byte(fmt.Sprintf("Physical rehearsal source %d: no model plaintext", n))
		hash := sha256.Sum256(body)
		a, e := r.Files.Upload(ctx, scope, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: "coffee", Name: fmt.Sprintf("origin%d.txt", n), Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: generated.ExactUnsigned(fmt.Sprint(len(body))), ExpectedSha256: hex.EncodeToString(hash[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: rights}, fmt.Sprintf("rehearsal-source-%d", n))
		if e != nil {
			t.Fatal(e)
		}
		assets = append(assets, string(a.AssetID))
		def := generated.AssetDefinition{ObjectID: generated.Identity(fmt.Sprintf("physical-origin-%d", n)), Kind: "ASSET", Name: "Source", Description: "original source metadata", Tags: []string{}, Rights: rights, AudiencePolicy: "PRODUCT_EDITORS", Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, AssetID: a.AssetID, AssetSha256: a.Sha256, SizeBytes: a.SizeBytes, MediaType: "text/plain", ClientCapabilities: []generated.Identity{}, Fallback: "BLOCK"}
		if n == 0 {
			def.DependencyRefs = []generated.DefinitionRef{json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"physical-origin-1"}`)}
		}
		raw, _ := json.Marshal(def)
		d.Input.Objects = append(d.Input.Objects, raw)
	}
	for n, raw := range d.Input.Objects {
		var obj map[string]any
		json.Unmarshal(raw, &obj)
		if obj["object_id"] == string(d.Input.RootObjectID) {
			obj["dependency_refs"] = []generated.DefinitionRef{json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"physical-origin-0"}`)}
			d.Input.Objects[n], _ = json.Marshal(obj)
		}
	}
	saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 1, "physical-rehearsal-basis")
	if e != nil {
		t.Fatal(e)
	}
	var req generated.RehearsalRequest
	if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"2","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":"Pass the sign and remember my request."}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS"]}`), &req); e != nil {
		t.Fatal(e)
	}
	req.ExpectedDraftRevision = saved.DraftRevision
	return r, draft, assets, req
}

func TestRehearsalPhysicalSourceRepairAndPartialCleanup(t *testing.T) {
	r, draft, assets, req := physicalRehearsalDraft(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "physical-rehearsal", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal("physical source→private init→repair→joint report", e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || len(body) == 0 {
		t.Fatal(e)
	}
	if bytes.Contains(body, []byte("Physical rehearsal source")) || bytes.Contains(journalBytes(t, r), []byte("Physical rehearsal source")) {
		t.Fatal("raw source leaked to report or journal")
	}
	var calls, actions, memories int
	if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,(SELECT COUNT(*) FROM events),(SELECT COUNT(*) FROM memory_asset_lineage) FROM harness_envelopes e WHERE e.task_id=?", root.TaskID).Scan(&calls, &actions, &memories); e != nil || calls != 2 || actions != 1 || memories < 2 {
		t.Fatal("real actor/action/body lineage", calls, actions, memories, e)
	}
	// Healthy dependencies survive ordinary bounded stage cleanup.
	var upper uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	page, e := r.Files.CleanupPage(ctx, scope, cutoff, 0, upper)
	if e != nil || page.Removed != 0 {
		t.Fatal("healthy rehearsal bodies lost", page, e)
	}
	// A deleted original ancestor invalidates descendants, not the sibling blob.
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", assets[1]); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
		t.Fatal("ancestor deletion ignored by history")
	}
	page, e = r.Files.CleanupPage(ctx, scope, cutoff.Add(time.Second), 0, upper)
	if e != nil || page.Removed != 3 {
		t.Fatal("partial source must retire original0, extra1 and report", page, e)
	}
	var pending int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1 AND purged=0").Scan(&pending); e != nil || pending != 0 {
		t.Fatal("unacknowledged physical purge", pending, e)
	}
	if receipt, e := r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil || !receipt.Committed {
		t.Fatal("cleanup erased original business fact", e)
	}
	var sibling string
	if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", assets[0]).Scan(&sibling); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, sibling+".blob")); e != nil {
		t.Fatal("unrelated physical source purged", e)
	}
}

func TestRehearsalCleanupAtomicClosure(t *testing.T) {
	for _, mode := range []string{"FINISH_LINEAGE", "FINISH_BINDING", "FINISH_INIT", "FINISH_PURGED", "ACK_COMMIT", "ACK_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, assets, req := physicalRehearsalDraft(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "cleanup-atomic", req)
			if e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			var before []byte
			if e = r.Store.DB.QueryRow("SELECT CAST(JSON_OBJECT('calls',calls_allocated,'steps',steps_used) AS CHAR) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&before); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", assets[1]); e != nil {
				t.Fatal(e)
			}
			var upper uint64
			var cutoff time.Time
			if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging").Scan(&upper, &cutoff); e != nil {
				t.Fatal(e)
			}
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var retiring bool
				if e := tx.QueryRowContext(ctx, "SELECT delete_intent FROM harness_rehearsal_reports WHERE task_id=?", root.TaskID).Scan(&retiring); e != nil {
					return e
				}
				if !retiring {
					return nil
				}
				switch mode {
				case "FINISH_PURGED":
					_, e := tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET purged=0 WHERE purged=1")
					return e
				case "FINISH_INIT":
					_, e := tx.ExecContext(ctx, "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('00',32)) WHERE operation='REHEARSAL_INIT'")
					return e
				case "FINISH_LINEAGE":
					_, e := tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET delete_intent=0")
					return e
				case "FINISH_BINDING":
					_, e := tx.ExecContext(ctx, "UPDATE harness_admin_rehearsals SET binding=JSON_SET(binding,'$.SourceHash',?) WHERE task_id=?", fmt.Sprintf("%064d", 0), root.TaskID)
					return e
				}
				return nil
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if mode == "ACK_COMMIT" {
						if e := tx.Commit(); e != nil {
							return e
						}
					} else {
						if e := tx.Rollback(); e != nil {
							return e
						}
					}
					return errors.New("synthetic cleanup ACK lost")
				}
			}
			_, e = r.Files.CleanupPage(ctx, scope, cutoff, 0, upper)
			if e == nil {
				t.Fatal("unproved cleanup accepted")
			}
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			var intents int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1").Scan(&intents); e != nil {
				t.Fatal(e)
			}
			if mode != "ACK_COMMIT" && mode != "FINISH_PURGED" && intents != 0 {
				t.Fatal("partial rollback", intents)
			}
			page, e := r.Files.CleanupPage(ctx, scope, cutoff, 0, upper)
			want := 3
			if mode == "FINISH_PURGED" {
				want = 2
			}
			if e != nil || page.Removed != want {
				t.Fatal("original cleanup continuation", page, e)
			}
			var pending int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1 AND purged=0").Scan(&pending); e != nil || pending != 0 {
				t.Fatal("physical acknowledgment lost", pending, e)
			}
			var after []byte
			if e = r.Store.DB.QueryRow("SELECT CAST(JSON_OBJECT('calls',calls_allocated,'steps',steps_used) AS CHAR) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&after); e != nil || !bytes.Equal(before, after) {
				t.Fatal("cleanup changed execution", e)
			}
		})
	}
}

func TestRehearsalPhysicalCleanupWaitsForActualPair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	r, draft, assets, req := physicalRehearsalDraft(t)
	ctx = runtimeContextWithParent(t, r, ctx)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	worker := "physical-held"
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "physical-held", req)
	if e != nil {
		t.Fatal(e)
	}
	rl, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, worker); e != nil {
		t.Fatal(e)
	}
	cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e != nil {
		t.Fatal(e)
	}
	childCtx, release, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	e = r.Worker.Turns.Execute(childCtx, cl.TaskLease, worker)
	release()
	if e != nil {
		t.Fatal(e)
	}
	held, e := r.Files.ReadRehearsalReportEvidence(ctx, rl.TaskLease, worker)
	if e != nil {
		t.Fatal(e)
	}
	defer held.Close()
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", assets[1]); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	// Isolate the derived-body page. The older uploaded-source staging rows
	// correctly wait on the separately held source locks before this page.
	var cursor uint64
	if e = r.Store.DB.QueryRow("SELECT MIN(s.sequence_id)-1 FROM file_staging s JOIN harness_result_sources r ON r.tenant_id=s.tenant_id AND r.product_id=s.product_id AND r.storage_key=s.storage_key WHERE r.task_id=?", cl.TaskID).Scan(&cursor); e != nil {
		t.Fatal(e)
	}
	go func() {
		page, e := r.Files.CleanupPage(ctx, scope, cutoff, cursor, upper)
		if e == nil && page.Removed != 2 {
			e = fmt.Errorf("expected both bodies, got %d", page.Removed)
		}
		done <- e
	}()
	finished := false
	defer func() {
		held.Close()
		cancel()
		if !finished {
			<-done
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var intents int
		if e = r.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE delete_intent=1").Scan(&intents); e != nil {
			t.Fatal(e)
		}
		if intents == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("intent not committed while bytes held")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case e := <-done:
		finished = true
		t.Fatal("cleanup bypassed original pair", e)
	default:
	}
	if _, e = r.Store.CommitRehearsalReport(ctx, rl.TaskLease, worker); e == nil {
		t.Fatal("retired source generated report")
	}
	held.Close()
	select {
	case e := <-done:
		finished = true
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("cleanup deadlocked", ctx.Err())
	}
}

func TestRehearsalPhysicalSourceSendBoundaries(t *testing.T) {
	for _, mode := range []string{"AUTHORIZE", "START_SEND", "CONSUME", "CONSUME_CORRUPT"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, assets, req := physicalRehearsalDraft(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			worker := "physical-send"
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "physical-send", req)
			if e != nil {
				t.Fatal(e)
			}
			rl, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, worker); e != nil {
				t.Fatal(e)
			}
			cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e != nil {
				t.Fatal(e)
			}
			childCtx, release, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			d, e := r.Store.PrepareRehearsalTurn(childCtx, rl.TaskLease, cl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			var permit *store.OriginalSendPermit
			if mode != "AUTHORIZE" {
				if _, e = r.Store.AuthorizeDispatch(childCtx, d); e != nil {
					t.Fatal(e)
				}
			}
			d.CommandKey = content.Identity(cl.TaskID, "send")
			d.CommitKey = d.CommandKey
			if mode == "CONSUME" || mode == "CONSUME_CORRUPT" {
				a, e := r.Store.StartSend(childCtx, d, d.AttemptID)
				if e != nil || !a.CanSend {
					t.Fatal(e)
				}
				permit = a.OriginalSend()
			}
			var storage string
			if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", assets[1]).Scan(&storage); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".blob")
			if mode == "CONSUME_CORRUPT" {
				e = os.WriteFile(path, []byte("changed original source"), 0600)
			} else {
				e = os.Remove(path)
			}
			if e != nil {
				t.Fatal(e)
			}
			journal := journalBytes(t, r)
			var before, after string
			q := "SELECT CONCAT((SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT COUNT(*) FROM service_events))"
			if e = r.Store.DB.QueryRow(q).Scan(&before); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "AUTHORIZE":
				_, e = r.Store.AuthorizeDispatch(childCtx, d)
			case "START_SEND":
				_, e = r.Store.StartSend(childCtx, d, d.AttemptID)
			default:
				e = r.Worker.Turns.Generate(childCtx, permit)
			}
			if e == nil {
				t.Fatal("missing/corrupt actual physical source accepted", mode)
			}
			if e = r.Store.DB.QueryRow(q).Scan(&after); e != nil || before != after || !bytes.Equal(journal, journalBytes(t, r)) {
				t.Fatal("source refusal wrote lifecycle or Generate", before, after, e)
			}
		})
	}
}

func TestProductLateBillAppendedAfterRuntimeReopen(t *testing.T) {
	r, l, _ := validCandidateBusiness(t, false)
	ctx := runtimeContext(t, r)
	if _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	original, e := r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	if done, failed, e := r.Store.RunOriginalBills(ctx); e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	before := journalBytes(t, r)
	if e = r.Local.Participant.Close(ctx); e != nil {
		t.Fatal(e)
	}
	local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Participant.Close(ctx)
	recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
	recovered.BindLocal(local)
	if done, failed, e := recovered.Store.RunOriginalBills(ctx); e != nil || done != 0 || failed != 0 {
		t.Fatal("new Runtime had no late item yet", done, failed, e)
	}
	recovered.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = recovered.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(journalBytes(t, recovered), before) || bytes.Equal(journalBytes(t, recovered), before) {
		t.Fatal("late append changed old journal or did not append")
	}
	if done, failed, e := recovered.Store.RunOriginalBills(ctx); e != nil || done != 2 || failed != 0 {
		t.Fatal("new Runtime failed to discover later append", done, failed, e)
	}
	if done, failed, e := recovered.Store.RunOriginalBills(ctx); e != nil || done != 0 || failed != 0 {
		t.Fatal("late delivery not idempotent", done, failed, e)
	}
	var debt string
	var calls, services, items, blocks int
	if e = recovered.Store.DB.QueryRow("SELECT CAST(debt_atoms AS CHAR),(SELECT calls_used FROM tasks WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?),(SELECT COUNT(*) FROM harness_account_blocks) FROM harness_supplier_liabilities WHERE attempt_id=?", l.TaskID, l.TaskID, l.TaskID, id.AttemptID).Scan(&debt, &calls, &services, &items, &blocks); e != nil || debt != "3" || calls != 1 || services != 1 || items != 3 || blocks != 2 {
		t.Fatal("original liability, no repurchase or second service", debt, calls, services, items, blocks, e)
	}
	after, e := recovered.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil || !bytes.Equal(original, after) {
		t.Fatal("late cost changed original result", e)
	}
}
