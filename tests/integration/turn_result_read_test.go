package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readOriginalResult(ctx context.Context, f *files.Store, lease domain.TaskLease, worker string) ([]byte, domain.MockResultMetadata, error) {
	evidence, e := f.ReadOriginalEvidence(ctx, lease, worker)
	if e != nil {
		return nil, domain.MockResultMetadata{}, e
	}
	defer evidence.Close()
	return evidence.Bytes(), evidence.Metadata(), nil
}

func currentResultLease(t *testing.T, task string, scope domain.Scope, query interface{ QueryRow(string, ...any) *sql.Row }) (domain.TaskLease, string) {
	t.Helper()
	lease := domain.TaskLease{ProductScope: scope.ProductScope, OwnerSubjectID: scope.SubjectID, TaskID: task, WorldInstanceID: &scope.WorldInstanceID}
	var worker string
	if e := query.QueryRow("SELECT fence,lease_owner FROM tasks WHERE task_id=?", task).Scan(&lease.Fence, &worker); e != nil {
		t.Fatal(e)
	}
	return lease, worker
}

func TestOriginalResultRetentionAfterResourceChangesDuringGenerate(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, q, "retain-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "retain-turn")
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDelayResultStage
	done := make(chan error, 1)
	go func() { done <- runOriginalPrefix(ctx, r) }()
	journal := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "journal.jsonl")
	deadline := time.Now().Add(4 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(journal)
		if bytes.Contains(raw, []byte("UNKNOWN")) {
			found = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		<-done
		t.Fatal("actual Generate never reached original journal")
	}
	_, changeErr := r.Store.DB.Exec("UPDATE world_controls SET world_revision=world_revision+1 WHERE world_instance_id=?", scope.WorldInstanceID)
	workerErr := <-done
	if changeErr != nil || workerErr != nil {
		t.Fatal("genuine completed bytes lost on resource revision", changeErr, workerErr)
	}
	lease, worker := currentResultLease(t, accepted.TurnID, scope, r.Store.DB)
	if body, _, e := readOriginalResult(ctx, r.Files, lease, worker); e != nil || len(body) == 0 {
		t.Fatal(e)
	}
	var count int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=?", accepted.TurnID).Scan(&count); e != nil || count != 1 {
		t.Fatal("revision change purchased recompute", count, e)
	}
}

func TestOriginalResultReaderRejectsChangedAuthorityAndMissingBytes(t *testing.T) {
	for _, kind := range []string{"old-fence", "privacy", "view", "paused", "cancelled", "expired-consent", "route", "actor-core", "actor-route", "actor-settings", "missing-context", "missing-body", "changed-body", "missing-anchor", "input-deleted", "wrong-installation", "disabled-service"} {
		t.Run(kind, func(t *testing.T) {
			r, scope, task, actor := originalResultFixture(t, false)
			ctx := runtimeContext(t, r)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			var storage string
			if e := r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
				t.Fatal(e)
			}
			bodyPath := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")
			var e error
			switch kind {
			case "actor-core", "actor-route", "actor-settings":
				var raw []byte
				var current world.ActorState
				if e = r.Store.DB.QueryRow("SELECT document FROM actors WHERE world_instance_id=? AND actor_id=?", scope.WorldInstanceID, actor).Scan(&raw); e != nil {
					t.Fatal(e)
				}
				if e = json.Unmarshal(raw, &current); e != nil {
					t.Fatal(e)
				}
				switch kind {
				case "actor-core":
					current.OriginalCharacter.Values = []string{"changed stable core"}
				case "actor-route":
					current.OwnerReference = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"another-character"}`)
				case "actor-settings":
					connection := generated.Identity("another-connection")
					current.OriginalCharacter.Generation.ConnectionRef = &connection
				}
				raw, e = json.Marshal(current)
				if e == nil {
					_, e = r.Store.DB.Exec("UPDATE actors SET document=? WHERE world_instance_id=? AND actor_id=?", raw, scope.WorldInstanceID, actor)
				}
			case "old-fence":
				_, e = r.Store.DB.Exec("UPDATE tasks SET fence=fence+1 WHERE task_id=?", task)
			case "wrong-installation":
				_, e = r.Store.DB.Exec("UPDATE memory_installation SET generation=generation+1")
			case "disabled-service":
				_, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0")
			case "privacy":
				_, e = r.Store.DB.Exec("UPDATE world_controls SET privacy_epoch=privacy_epoch+1 WHERE world_instance_id=?", scope.WorldInstanceID)
			case "view":
				_, e = r.Store.DB.Exec("UPDATE world_controls SET view_epoch=view_epoch+1 WHERE world_instance_id=?", scope.WorldInstanceID)
			case "paused":
				_, e = r.Store.DB.Exec("UPDATE world_controls SET status='PAUSED' WHERE world_instance_id=?", scope.WorldInstanceID)
			case "cancelled":
				_, e = r.Store.DB.Exec("UPDATE tasks SET cancelled=1 WHERE task_id=?", task)
			case "expired-consent":
				_, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE action='memory.generation.use'")
			case "route":
				_, e = r.Store.DB.Exec("UPDATE releases SET available=0")
			case "missing-context":
				_, e = r.Store.DB.Exec("DELETE FROM harness_attempt_contexts WHERE task_id=?", task)
			case "missing-body":
				e = os.Remove(bodyPath)
			case "changed-body":
				e = os.WriteFile(bodyPath, []byte("different bytes"), 0600)
			case "missing-anchor":
				e = os.Remove(filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "anchor.json"))
			case "input-deleted":
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
				if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id=?", scope.SubjectID); e != nil {
					t.Fatal(e)
				}
				for _, action := range []string{"memory.govern", "memory.source.govern"} {
					if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
						t.Fatal(e)
					}
				}
				_, e = r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: actor, PlayerID: scope.SubjectID}, memory.MemoryGovernanceCommand{Key: "reader-source-delete", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: a.Sources[source].Revision}}})
			}
			if e != nil {
				t.Fatal("negative boundary fixture", e)
			}
			if body, _, e := readOriginalResult(ctx, r.Files, lease, worker); e == nil || len(body) != 0 {
				t.Fatal("ineligible original result returned bytes", e)
			}
		})
	}
}

func TestOriginalResultReaderRequiresCurrentNarrativeAuthority(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	var beforeReceipts, beforeSteps int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),steps_used FROM harness_envelopes WHERE task_id=?", task).Scan(&beforeReceipts, &beforeSteps); e != nil {
		t.Fatal(e)
	}
	body, meta, e := readOriginalResult(ctx, r.Files, lease, worker)
	var result struct {
		Dialogue string `json:"dialogue"`
	}
	if e != nil || json.Unmarshal(body, &result) != nil || result.Dialogue != "我听到了。" || meta.Identity.TaskID != task {
		t.Fatal("real governed original bytes unavailable", string(body), e)
	}
	var afterReceipts, afterSteps int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),steps_used FROM harness_envelopes WHERE task_id=?", task).Scan(&afterReceipts, &afterSteps); e != nil || beforeReceipts != afterReceipts || beforeSteps != afterSteps {
		t.Fatal("body inspection mutated receipt/FSM", beforeReceipts, afterReceipts, beforeSteps, afterSteps, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	body, _, e = readOriginalResult(ctx, r.Files, lease, worker)
	if e == nil || len(body) != 0 {
		t.Fatal("withdrawn generation permission leaked original bytes", e)
	}
}

func TestOriginalResultReaderAllowsOnlyWorldResourceRevisionDrift(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if _, e := r.Store.DB.Exec("UPDATE world_controls SET world_revision=world_revision+1 WHERE world_instance_id=?", scope.WorldInstanceID); e != nil {
		t.Fatal(e)
	}
	if body, _, e := readOriginalResult(ctx, r.Files, lease, worker); e != nil || len(body) == 0 {
		t.Fatal("resource-only world revision lost genuine original evidence", e)
	}
	if _, e := r.Store.DB.Exec("UPDATE world_controls SET control_version=control_version+1 WHERE world_instance_id=?", scope.WorldInstanceID); e != nil {
		t.Fatal(e)
	}
	if body, _, e := readOriginalResult(ctx, r.Files, lease, worker); e == nil || len(body) != 0 {
		t.Fatal("control change treated as resource revision", e)
	}
}
