package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountingOnlyMigrationKeepsUnknownSupplier(t *testing.T) {
	ctx := context.Background()
	r, s, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, s, task); e != nil {
		t.Fatal(e)
	}
	i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
	if e != nil {
		t.Fatal(e)
	}
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s.ProductScope, i.Versions.ReleaseID, "expression_style", "migration retains unknown costs")
	if _, e = r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), s, generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "direct-preview"); e != nil {
		t.Fatalf("owning preview diagnostic: %T %v", e, e)
	}
	var plan generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+s.WorldInstanceID+"/migration-previews", "seal-preview", generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &plan)
	var seals int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_world_seals").Scan(&seals); e != nil || seals != 0 {
		t.Fatal("preview must not seal", seals, e)
	}
	hash, e := content.Hash(plan)
	if e != nil {
		t.Fatal(e)
	}
	var receipt generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+s.WorldInstanceID+"/migrations", "seal-execute", generated.MigrationExecuteRequest{ProductID: generated.Identity(s.ProductID), PlanID: plan.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: plan.ResolutionMap}, 202, &receipt)
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	after, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
	if e != nil || after.Versions.ReleaseID != string(target.ReleaseID) {
		t.Fatal(after, e)
	}
	var state, usage string
	var supplier, risk, services int
	e = r.Store.DB.QueryRow("SELECT h.state,IF(q.metadata IS NULL,'UNKNOWN','AUTHORITATIVE'),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND kind IN ('supplier_global','supplier_tenant') AND status='HELD'),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND kind='platform_risk' AND status='RISK'),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM harness_envelopes h JOIN attempts a USING(tenant_id,product_id,task_id) JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE h.task_id=?", task, task, task, task).Scan(&state, &usage, &supplier, &risk, &services)
	if e != nil || state != "ACCOUNTING_ONLY" || usage != "UNKNOWN" || supplier != 2 || risk != 1 || services != 0 {
		t.Fatal(state, usage, supplier, risk, services, e)
	}
	before := journalBytes(t, r)
	if e = r.Worker.QueryNecessaryCostOnce(ctx); e != nil {
		t.Fatal("necessary original cost after world seal", e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cost path changed provider journal")
	}
	if done, failed, e := r.Store.RunOriginalBills(ctx); e != nil || done != 1 || failed != 0 {
		t.Fatal("late original settlement after migration", done, failed, e)
	}
}

func accountingMigration(t *testing.T, r *app.Runtime, s domain.Scope, key string) (generated.Instance, generated.MigrationExecuteRequest) {
	t.Helper()
	ctx := context.Background()
	i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
	if e != nil {
		t.Fatal(e)
	}
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s.ProductScope, i.Versions.ReleaseID, "expression_style", "preserve historical packet")
	var p generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+s.WorldInstanceID+"/migration-previews", key+"-preview", generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &p)
	h, e := content.Hash(p)
	if e != nil {
		t.Fatal(e)
	}
	return i, generated.MigrationExecuteRequest{ProductID: generated.Identity(s.ProductID), PlanID: p.PlanID, PlanHash: h, ExpectedVersions: i.Versions, ResolutionMap: p.ResolutionMap}
}
func TestAccountingSealAndMigrationQueueAreOneOriginalCommit(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "ack-unknown"}[ack], func(t *testing.T) {
			ctx := context.Background()
			r, s, task, _ := originalResultFixture(t, true)
			expireCutoff(t, r.Store.DB, task)
			if _, e := r.Store.CutoffHarnessTask(ctx, s, task); e != nil {
				t.Fatal(e)
			}
			_, req := accountingMigration(t, r, s, "atomic")
			if ack {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic original execute ACK lost")
				}
			} else {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic seal rollback") }
			}
			_, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "atomic-execute")
			if e == nil {
				t.Fatal("fault not observed")
			}
			if ack && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			var seals, retired, jobs int
			var active bool
			e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_world_seals),(SELECT COUNT(*) FROM harness_task_retirements),(SELECT COUNT(*) FROM tasks WHERE task_kind='MIGRATION'),active_migration_task IS NOT NULL FROM runtime_instances WHERE world_instance_id=?", s.WorldInstanceID).Scan(&seals, &retired, &jobs, &active)
			want := 0
			if ack {
				want = 1
			}
			if e != nil || seals != want || retired != want || jobs != want || active != ack {
				t.Fatal(seals, retired, jobs, active, e)
			}
			receipt, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "atomic-execute")
			if e != nil {
				t.Fatal("original execution recovery", e)
			}
			again, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "atomic-execute")
			if e != nil || again.TaskID != receipt.TaskID {
				t.Fatal(again, e)
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestAccountingMigrationRequiresAllWorldTasksAndOwningWriters(t *testing.T) {
	for _, mode := range []string{"live-turn", "missing-writer", "missing-cutoff-proof", "world-commit-pending"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, s, task, _ := originalResultFixture(t, mode != "world-commit-pending")
			if mode == "world-commit-pending" {
				lease, worker := currentResultLease(t, task, s, r.Store.DB)
				if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "world-commit-pending" {
				lease, worker := currentResultLease(t, task, s, r.Store.DB)
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return tx.Commit()
					}
					if e := tx.Rollback(); e != nil {
						return e
					}
					return errors.New("synthetic original WORLD ACK unknown")
				}
				_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
				r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			}
			if mode != "live-turn" && mode != "world-commit-pending" {
				expireCutoff(t, r.Store.DB, task)
				if _, e := r.Store.CutoffHarnessTask(ctx, s, task); e != nil {
					t.Fatal(e)
				}
			}
			i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
			if e != nil {
				t.Fatal(e)
			}
			i = runtimeControl(t, r, i, "PAUSE")
			target := nextRuntimeRelease(t, r, s.ProductScope, i.Versions.ReleaseID, "expression_style", "blocked migration")
			if mode == "missing-writer" {
				r.Store.WorldAccountingAuthority = nil
			}
			if mode == "missing-cutoff-proof" {
				if _, e = r.Store.DB.Exec("DELETE FROM harness_cutoffs WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), s, generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "blocked"); e == nil {
				t.Fatal("incomplete closure accepted")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_world_seals").Scan(&n); e != nil || n != 0 {
				t.Fatal(n, e)
			}
		})
	}
}
func TestCommittedPacketSurvivesAccountingSealAndActualMigration(t *testing.T) {
	r, s, task, _ := originalResultFixture(t, false)
	ctx := runtimeContext(t, r)
	lease, worker := currentResultLease(t, task, s, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	before, _, e := r.Files.ReadApprovedTurn(ctx, s, task)
	if e != nil {
		t.Fatal(e)
	}
	_, req := accountingMigration(t, r, s, "history")
	if paused, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e != nil || !bytes.Equal(before, paused) {
		t.Fatal("paused actual history", e)
	}
	if _, e = r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "history-execute"); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	after, _, e := r.Files.ReadApprovedTurn(ctx, s, task)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("committed history lost", e)
	}
	view, e := r.Turns.GetTurn(ctx, s, task)
	if e != nil || view.Task.TaskState != "COMMITTED" || len(view.ApprovedPackets) != 1 || view.Billing.ServiceSettlementState != "SETTLED" {
		t.Fatal(view, e)
	}
	// A second real migration must traverse both original migration receipts.
	i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
	if e != nil {
		t.Fatal(e)
	}
	old, e := r.Content.GetRelease(ctx, s.ProductScope, i.Versions.ReleaseID)
	if e != nil {
		t.Fatal(e)
	}
	in := old.Input
	for n, raw := range in.Objects {
		var object map[string]any
		if e = json.Unmarshal(raw, &object); e != nil {
			t.Fatal(e)
		}
		if object["object_id"] == "alex-character" {
			object["expression_style"] = "second actual migration"
			in.Objects[n], e = json.Marshal(object)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	d, e := r.Content.SaveDraft(ctx, s.ProductScope, string(old.DraftID), in, 2, "second-save")
	if e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, s.ProductScope, string(d.DraftID), 3, []string{"MOCK_DEMO"}, "second-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	target, e := r.Content.Publish(ctx, s.ProductScope, string(d.DraftID), 3, string(check.ReportID), []string{"MOCK_DEMO"}, "second-publish")
	if e != nil {
		t.Fatal(e)
	}
	plan, e := r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), s, generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "second-preview")
	if e != nil {
		t.Fatal(e)
	}
	hash, e := content.Hash(plan)
	if e != nil {
		t.Fatal(e)
	}
	second, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, generated.MigrationExecuteRequest{ProductID: generated.Identity(s.ProductID), PlanID: plan.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: plan.ResolutionMap}, "second-execute")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e != nil || !bytes.Equal(before, b) {
		t.Fatal("complete two-migration history", e)
	}
	// Corrupt one first-link record in this disposable fixture, then restore it.
	var firstMigration string
	if e = r.Store.DB.QueryRow("SELECT task_id FROM tasks WHERE task_kind='MIGRATION' AND task_id<>?", second.TaskID).Scan(&firstMigration); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE commit_receipts SET operation='BROKEN_HISTORY' WHERE task_id=? AND operation='INSTANCE_MIGRATION'", firstMigration); e != nil {
		t.Fatal(e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e == nil || len(b) != 0 {
		t.Fatal("latest receipt hid missing first migration link")
	}
	if _, e = r.Store.DB.Exec("UPDATE commit_receipts SET operation='INSTANCE_MIGRATION' WHERE task_id=? AND operation='BROKEN_HISTORY'", firstMigration); e != nil {
		t.Fatal(e)
	}
	for _, boundary := range []string{"privacy", "seal", "retirement", "route", "selected-source", "owner", "consent"} {
		t.Run(boundary, func(t *testing.T) {
			restore := func() {}
			defer func() { restore() }()
			readScope := s
			e = nil
			switch boundary {
			case "privacy":
				if _, e = r.Store.DB.Exec("UPDATE world_controls SET privacy_epoch=privacy_epoch+1 WHERE world_instance_id=?", s.WorldInstanceID); e != nil {
					t.Fatal(e)
				}
				restore = func() {
					_, _ = r.Store.DB.Exec("UPDATE world_controls SET privacy_epoch=privacy_epoch-1 WHERE world_instance_id=?", s.WorldInstanceID)
				}
			case "seal":
				var saved string
				if e = r.Store.DB.QueryRow("SELECT set_hash FROM harness_world_seals").Scan(&saved); e != nil {
					t.Fatal(e)
				}
				_, e = r.Store.DB.Exec("UPDATE harness_world_seals SET set_hash=REPEAT('0',64)")
				restore = func() { _, _ = r.Store.DB.Exec("UPDATE harness_world_seals SET set_hash=?", saved) }
			case "retirement":
				_, e = r.Store.DB.Exec("UPDATE harness_envelopes SET narrative_generation=narrative_generation+1 WHERE task_id=?", task)
				restore = func() {
					_, _ = r.Store.DB.Exec("UPDATE harness_envelopes SET narrative_generation=narrative_generation-1 WHERE task_id=?", task)
				}
			case "route":
				var raw []byte
				if e = r.Store.DB.QueryRow("SELECT document FROM content_releases WHERE release_id=?", target.ReleaseID).Scan(&raw); e != nil {
					t.Fatal(e)
				}
				var changed generated.Release
				if e = json.Unmarshal(raw, &changed); e != nil {
					t.Fatal(e)
				}
				mutations := 0
				for i := range changed.RouteMappings {
					if changed.RouteMappings[i].ModuleID == "INTERACTION" {
						changed.RouteMappings[i].ModelID = "unsupported-mutated-model"
						mutations++
					}
				}
				if mutations == 0 {
					t.Fatal("no interaction route mutation")
				}
				body, err := json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				_, e = r.Store.DB.Exec("UPDATE content_releases SET document=? WHERE release_id=?", body, target.ReleaseID)
				restore = func() {
					_, _ = r.Store.DB.Exec("UPDATE content_releases SET document=? WHERE release_id=?", raw, target.ReleaseID)
				}
			case "selected-source":
				_, e = r.Store.DB.Exec("UPDATE memory_asset_lineage SET delete_intent=1 WHERE world_instance_id=?", s.WorldInstanceID)
				restore = func() {
					_, _ = r.Store.DB.Exec("UPDATE memory_asset_lineage SET delete_intent=0 WHERE world_instance_id=?", s.WorldInstanceID)
				}
			case "owner":
				readScope.SubjectID = "other-owner"
			case "consent":
				_, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'")
				restore = func() {
					_, _ = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=1 WHERE action='memory.generation.use'")
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if b, _, e := r.Files.ReadApprovedTurn(ctx, readScope, task); e == nil || len(b) != 0 {
				t.Fatal("history crossed current boundary", boundary)
			}
		})
	}
	var storage string
	if e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, storage+".part")); e != nil {
		t.Fatal(e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e == nil || len(b) != 0 {
		t.Fatal("history reconstructed deleted bytes")
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e == nil || len(b) != 0 {
		t.Fatal("retirement bypassed current permission")
	}
}

func TestAccountingWorldSealRejectsSecondLiveTurnAndMissingRegisteredWriter(t *testing.T) {
	for _, mode := range []string{"second-live-turn", "unregistered-writer"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, s, turns, qr, req := actualTurnFixture(t, true)
			q, e := turns.QuoteTurn(ctx, s, qr, "first-q")
			if e != nil {
				t.Fatal(e)
			}
			req.QuoteID = q.QuoteID
			a, e := turns.SubmitTurn(ctx, s, req, "first")
			if e != nil {
				t.Fatal(e)
			}
			expireCutoff(t, r.Store.DB, a.TurnID)
			if _, e = r.Store.CutoffHarnessTask(ctx, s, a.TurnID); e != nil {
				t.Fatal(e)
			}
			if mode == "second-live-turn" {
				q, e = turns.QuoteTurn(ctx, s, qr, "second-q")
				if e != nil {
					t.Fatal(e)
				}
				req.QuoteID = q.QuoteID
				b, e := turns.SubmitTurn(ctx, s, req, "second")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.ClaimRegistered(ctx, "hold-second", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: b.TurnID}}); e != nil {
					t.Fatal(e)
				}
			}
			i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), s)
			if e != nil {
				t.Fatal(e)
			}
			i = runtimeControl(t, r, i, "PAUSE")
			target := nextRuntimeRelease(t, r, s.ProductScope, i.Versions.ReleaseID, "expression_style", "blocked complete set")
			if mode == "unregistered-writer" {
				p, e := r.Local.Coordinator.Register(ctx, "incomplete-owner", []maintenance.Writer{maintenance.StoreWriter})
				if e != nil {
					t.Fatal(e)
				}
				defer p.Close(ctx)
				r.Store.WorldAccountingAuthority = p
			}
			_, e = r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), s, generated.MigrationPreviewRequest{ProductID: generated.Identity(s.ProductID), TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "must-block")
			if e == nil {
				t.Fatal("incomplete world/writer set sealed")
			}
		})
	}
}
func TestAccountingSealRejectsAllOriginalCandidateWriters(t *testing.T) {
	ctx := context.Background()
	r, s, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, s, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, s, task); e != nil {
		t.Fatal(e)
	}
	_, req := accountingMigration(t, r, s, "retire")
	if _, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "retire-execute"); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	for _, op := range []string{"validate", "approve", "recompute", "world-commit", "original-result"} {
		t.Run(op, func(t *testing.T) {
			var e error
			switch op {
			case "validate":
				e = r.Store.ValidateOriginalTurn(ctx, lease, worker)
			case "approve":
				e = r.Store.ApproveOriginalTurn(ctx, lease, worker)
			case "recompute":
				_, e = r.Store.PrepareTurnRecovery(ctx, lease, worker)
			case "world-commit":
				_, e = r.Store.CommitOriginalTurn(ctx, lease, worker)
			case "original-result":
				evidence, err := r.Files.ReadOriginalEvidence(ctx, lease, worker)
				e = err
				if evidence != nil {
					evidence.Close()
				}
			}
			if e == nil {
				t.Fatal("retired writer resumed")
			}
		})
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("retired writer changed J")
	}
	var commits, services int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?)", task, task).Scan(&commits, &services); e != nil || commits != 0 || services != 0 {
		t.Fatal(commits, services, e)
	}
}
func TestAccountingMigrationPreservesActualActionMemoryAndLedger(t *testing.T) {
	ctx := context.Background()
	r, s, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, s, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	snapshot := func() string {
		var a, b, c string
		if e := r.Store.DB.QueryRow("SELECT (SELECT GROUP_CONCAT(SHA2(document,256) ORDER BY item_id) FROM items WHERE world_instance_id=?),(SELECT GROUP_CONCAT(SHA2(document,256) ORDER BY aggregate_id) FROM memory_aggregates WHERE world_instance_id=?),(SELECT CONCAT(COUNT(*),':',SUM(amount)) FROM postings WHERE task_id=?)", s.WorldInstanceID, s.WorldInstanceID, task).Scan(&a, &b, &c); e != nil {
			t.Fatal(e)
		}
		return a + b + c
	}
	before := snapshot()
	_, req := accountingMigration(t, r, s, "preserve")
	if _, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "preserve-execute"); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if snapshot() != before {
		t.Fatal("migration rewrote original facts/memory/ledger")
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e != nil || len(b) == 0 {
		t.Fatal("actual action-memory packet lost", e)
	}
	var retired int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE task_id=? AND status='RETIRED' AND lease_owner IS NULL", task).Scan(&retired); e != nil || retired != 1 {
		t.Fatal("unsent delivery not retired", retired, e)
	}
}

func TestAccountingExecuteWaitsForRealWorldLockAndRetiresOriginalSend(t *testing.T) {
	ctx := context.Background()
	r, s, turns, qr, req := actualTurnFixture(t, true)
	q, e := turns.QuoteTurn(ctx, s, qr, "locked-q")
	if e != nil {
		t.Fatal(e)
	}
	req.QuoteID = q.QuoteID
	accepted, e := turns.SubmitTurn(ctx, s, req, "locked-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "held-send", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "held-send")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(d.TaskID, "send")
	d.CommitKey = d.CommandKey
	send, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, accepted.TurnID)
	if _, e = r.Store.CutoffHarnessTask(ctx, s, accepted.TurnID); e != nil {
		t.Fatal(e)
	}
	_, migration := accountingMigration(t, r, s, "locked")
	userCtx := runtimeContextWithParent(t, r, ctx)
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var id string
	if e = blocker.QueryRow("SELECT world_instance_id FROM world_controls WHERE world_instance_id=? FOR UPDATE", s.WorldInstanceID).Scan(&id); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := r.World.ExecuteInstanceMigration(userCtx, s, migration, "locked-execute"); done <- e }()
	select {
	case e := <-done:
		t.Fatal("world lock bypassed", e)
	case <-time.After(150 * time.Millisecond):
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	if e = r.Worker.Turns.Generate(ctx, send.OriginalSend()); e == nil {
		t.Fatal("retired opaque permit sent")
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("retired send mutated journal")
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("old authorization replay allowed")
	}
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("old marker replay resumed")
	}
}

func TestAccountingMigrationRetainsRealExtraOriginalBodyDependency(t *testing.T) {
	ctx := context.Background()
	r, s, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
	lease, worker := currentResultLease(t, task, s, r.Store.DB)
	r.Worker.Turns.Fault = 0
	if e := r.Worker.Turns.ExecuteRecovery(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	before, commit, e := r.Files.ReadApprovedTurn(ctx, s, task)
	if e != nil || commit.OriginalDependency == nil {
		t.Fatal(e)
	}
	_, req := accountingMigration(t, r, s, "extra-history")
	if _, e = r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), s, req, "extra-history-execute"); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if after, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e != nil || !bytes.Equal(before, after) {
		t.Fatal("extra history lost", e)
	}
	var calls, steps int
	if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", task).Scan(&calls, &steps); e != nil || calls != 2 || steps != 14 {
		t.Fatal(calls, steps, e)
	}
	if e = os.Remove(filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, commit.OriginalDependency.BodyStorage+".part")); e != nil {
		t.Fatal(e)
	}
	if b, _, e := r.Files.ReadApprovedTurn(ctx, s, task); e == nil || len(b) != 0 {
		t.Fatal("migration dropped true original0 body dependency")
	}
}
