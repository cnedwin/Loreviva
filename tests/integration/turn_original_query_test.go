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
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func originalResultFixture(t *testing.T, drop bool) (*app.Runtime, domain.Scope, string, string) {
	t.Helper()
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, q, "query-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "query-turn")
	if e != nil {
		t.Fatal(e)
	}
	if drop {
		r.Worker.Turns.Fault = harness.MockDropOriginalReply
	}
	var before, after []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, scope.SubjectID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	e = runOriginalPrefix(ctx, r)
	if drop {
		if !errors.Is(e, harness.ErrMockReplyLost) {
			t.Fatal(e)
		}
	} else if e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, scope.SubjectID).Scan(&after); e != nil || !bytes.Equal(before, after) {
		t.Fatal("pending result changed frozen context", e)
	}
	return r, scope, accepted.TurnID, request.PrimaryActorID
}
func journalBytes(t *testing.T, r *app.Runtime) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "journal.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func queryDue(t *testing.T, r *app.Runtime, task string) {
	t.Helper()
	if _, e := r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
}
func TestOriginalMockLostReplyRestartQueriesOnce(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	before := journalBytes(t, r)
	original, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	// The original bill is metadata for the same request, not another Generate.
	lines := bytes.Split(bytes.TrimSuffix(before, []byte("\n")), []byte("\n"))
	if len(lines) != 3 || bytes.Contains(before, []byte("我听到了")) {
		t.Fatal("journal not metadata-only original begin/completion/bill")
	}
	for n, state := range []string{"UNKNOWN", "SUCCEEDED", "BILL"} {
		var entry struct {
			Result domain.MockResultMetadata
			Bill   *domain.MockBillItem
			Usage  *domain.MockUsageMeasurement
		}
		if e = json.Unmarshal(lines[n], &entry); e != nil || entry.Result.State != state || entry.Result.Identity != original || entry.Usage != nil {
			t.Fatal("journal changed original request identity or lifecycle", n, e)
		}
		if state == "BILL" {
			if entry.Bill == nil || entry.Bill.Identity != original || entry.Bill.ItemID != "original-charge" {
				t.Fatal("missing exact original bill")
			}
		} else if entry.Bill != nil {
			t.Fatal("bill mislabeled as provider request")
		}
	}
	local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Participant.Close(ctx)
	recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
	recovered.BindLocal(local)
	if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	if e = recovered.Worker.Once(ctx); e != nil {
		t.Fatal("restart did not query original", e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("restart generated new provider request")
	}
	view, e := recovered.Turns.GetTurn(ctx, scope, task)
	if e != nil || len(view.Attempts) != 1 || view.Attempts[0].UsageState != "AUTHORITATIVE" || view.Attempts[0].QueryCapability != "SUPPORTED" {
		t.Fatal("actual result still projected as unknown", view.Attempts, e)
	}
	var state string
	var count int
	if e = r.Store.DB.QueryRow("SELECT state,calls_used FROM tasks WHERE task_id=?", task).Scan(&state, &count); e != nil || state != "COMMITTED" || count != 1 {
		t.Fatal(state, count, e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT metadata FROM harness_original_queries WHERE task_id=?", task).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var result domain.MockResultMetadata
	if e = json.Unmarshal(raw, &result); e != nil || result.Identity != id || result.State != "SUCCEEDED" || result.InputUnits == 0 || result.OutputUnits == 0 {
		t.Fatal("missing real usage", result, e)
	}
	if !result.UsageKnown || !result.NoFurtherCost || result.CostAtoms != 0 || result.CostUnit != "MONEY" || result.CostCurrency != "syntheticUSD" {
		t.Fatal("zero cost evidence omitted", result)
	}
}

func TestOriginalMockKnownMarkerNeverGenerates(t *testing.T) {
	for _, lostACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-unsent", true: "marker-ack-lost"}[lostACK], func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, q, request := actualTurnFixture(t, true)
			quote, e := turns.QuoteTurn(ctx, scope, q, "marker-quote")
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = quote.QuoteID
			accepted, e := turns.SubmitTurn(ctx, scope, request, "marker-turn")
			if e != nil {
				t.Fatal(e)
			}
			lease, e := r.Store.ClaimRegistered(ctx, "marked", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "marked")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d.CommandKey = content.Identity(lease.TaskID, "send")
			d.CommitKey = d.CommandKey
			if lostACK {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("lost marker ACK")
				}
			}
			auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
			r.Store.Commit = nil
			if lostACK {
				if !errors.Is(e, store.ErrCommitUnknown) || auth.OriginalSend() != nil {
					t.Fatal("unknown marker permitted Generate", e)
				}
			} else if e != nil || !auth.CanSend {
				t.Fatal(e)
			}
			replay, e := r.Store.StartSend(ctx, d, d.AttemptID)
			if e != nil || replay.CanSend || replay.OriginalSend() != nil || !replay.QueryOriginal {
				t.Fatal("marker replay minted send authority", replay, e)
			}
			queryDue(t, r, accepted.TurnID)
			if e = r.Worker.Turns.Execute(ctx, lease.TaskLease, "marked"); e != nil {
				t.Fatal(e)
			}
			if len(journalBytes(t, r)) != 0 {
				t.Fatal("potentially unsent marker regenerated")
			}
			var state, provider string
			if e = r.Store.DB.QueryRow("SELECT a.state,q.provider_state FROM attempts a JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE a.task_id=?", accepted.TurnID).Scan(&state, &provider); e != nil || state != "SENT" || provider != "UNKNOWN" {
				t.Fatal(state, provider, e)
			}
			if !lostACK {
				if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", accepted.TurnID); e != nil {
					t.Fatal(e)
				}
				stage, e := r.Files.AllocateDerived(scope.ProductScope, "late original body")
				if e != nil {
					t.Fatal(e)
				}
				if e = r.Store.BindOriginalResultBody(ctx, d, r.Files.DerivedStorage(stage), stage.Hash); e == nil {
					t.Fatal("expired execution staged narrative result")
				}
			}
		})
	}
}

func TestOriginalQueryServiceAndLeaseBoundaries(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	first, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	var offset int
	if e = r.Store.DB.QueryRow("SELECT TIMESTAMPDIFF(SECOND,a.send_started_at,q.next_query_at) FROM harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE q.task_id=?", task).Scan(&offset); e != nil || offset != 5 {
		t.Fatal("first backoff", offset, e)
	}
	if _, e = r.Store.BeginOriginalQuery(ctx, id); e == nil {
		t.Fatal("concurrent query lease")
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	second, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT TIMESTAMPDIFF(SECOND,a.send_started_at,q.next_query_at) FROM harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE q.task_id=?", task).Scan(&offset); e != nil || offset != 15 {
		t.Fatal("second backoff", offset, e)
	}
	if out, e := r.Store.RecordOriginalQuery(ctx, first); e == nil || out.State != "" {
		t.Fatal("old query fence read result", out, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	if out, e := r.Store.RecordOriginalQuery(ctx, second); e == nil || out.State != "" {
		t.Fatal("disabled service read result", out, e)
	}
	if _, e = r.Store.BeginOriginalQuery(ctx, id); e == nil {
		t.Fatal("disabled service acquired lease")
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=1"); e != nil {
		t.Fatal(e)
	}
	if out, e := r.Store.RecordOriginalQuery(ctx, second); e != nil || out.State != "SUCCEEDED" {
		t.Fatal(out, e)
	}
	queryDue(t, r, task)
	third, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalQuery(ctx, third); e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	if _, e = r.Store.BeginOriginalQuery(ctx, id); e == nil {
		t.Fatal("fourth automatic query")
	}
	var used int
	if e = r.Store.DB.QueryRow("SELECT queries_used FROM harness_original_queries WHERE task_id=?", task).Scan(&used); e != nil || used != 3 {
		t.Fatal(used, e)
	}
	wrong := id
	wrong.RequestKey = "other-request"
	if _, e = r.Store.BeginOriginalQuery(ctx, wrong); e == nil {
		t.Fatal("substituted original")
	}
}

func TestOriginalCostQuerySurvivesNarrativeWithdrawal(t *testing.T) {
	for _, change := range []string{"pause", "owner-disabled", "product-disabled", "generation-revoked", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, true)
			before := journalBytes(t, r)
			var changed sql.Result
			var changeErr error
			switch change {
			case "pause":
				changed, changeErr = r.Store.DB.Exec("UPDATE world_controls SET status='PAUSED',control_version=control_version+1 WHERE world_instance_id=?", scope.WorldInstanceID)
			case "owner-disabled":
				changed, changeErr = r.Store.DB.Exec("UPDATE subject_governance SET enabled=0 WHERE subject_id=?", scope.SubjectID)
			case "product-disabled":
				changed, changeErr = r.Store.DB.Exec("UPDATE product_gates SET enabled=0 WHERE product_id=?", scope.ProductID)
			case "generation-revoked":
				changed, changeErr = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'")
			case "cancelled":
				changed, changeErr = r.Store.DB.Exec("UPDATE tasks SET state='CANCELLED',cancelled=1,fence=fence+1 WHERE task_id=?", task)
			}
			if changeErr != nil {
				t.Fatal(changeErr)
			}
			if n, e := changed.RowsAffected(); e != nil || n == 0 {
				t.Fatal("withdrawal fixture failed", n, e)
			}
			var originalState string
			if e := r.Store.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", task).Scan(&originalState); e != nil {
				t.Fatal(e)
			}
			id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
			if e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, task)
			if e = r.Worker.QueryOriginalOnce(ctx); e != nil {
				t.Fatal("actual cost worker", e)
			}
			queryDue(t, r, task)
			lease, e := r.Store.BeginOriginalQuery(ctx, id)
			if e != nil {
				t.Fatal("necessary cost query denied by ordinary status", e)
			}
			out, e := r.Store.RecordOriginalQuery(ctx, lease)
			if e != nil || out.State != "SUCCEEDED" {
				t.Fatal(out, e)
			}
			var state string
			if e = r.Store.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", task).Scan(&state); e != nil || state != originalState {
				t.Fatal("cost query revived narrative", state, e)
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("cost query generated again")
			}
			var count int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM approved_results WHERE task_id=?", task).Scan(&count); e != nil || count != 0 {
				t.Fatal("query approved narrative", count, e)
			}
		})
	}
}

func TestOriginalMockResultFollowsEverySourceDeletion(t *testing.T) {
	for _, kind := range []string{"input", "initial"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, actor := originalResultFixture(t, false)
			var storage string
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT storage_key,document FROM harness_result_sources WHERE task_id=?", task).Scan(&storage, &raw); e != nil {
				t.Fatal(e)
			}
			var binding domain.ResultSourceBinding
			if e := json.Unmarshal(raw, &binding); e != nil || len(binding.Nodes) < 2 {
				t.Fatal("not multi-source", binding, e)
			}
			bodyPath := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")
			body, e := os.ReadFile(bodyPath)
			if e != nil || len(body) == 0 {
				t.Fatal("actual governed body missing", e)
			}
			if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, scope.SubjectID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var graph memory.Aggregate
			if e = json.Unmarshal(raw, &graph); e != nil {
				t.Fatal(e)
			}
			source := ""
			if kind == "input" {
				e = r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", task).Scan(&source)
				if e != nil {
					t.Fatal(e)
				}
			} else {
				for _, ac := range graph.Acquisitions {
					if ac.ActorID == actor && ac.Kind == memory.Initial {
						source = ac.SourceID
						break
					}
				}
			}
			if source == "" {
				t.Fatal("original source absent")
			}
			if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id=?", scope.SubjectID); e != nil {
				t.Fatal(e)
			}
			for _, action := range []string{"memory.govern", "memory.source.govern"} {
				if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: actor, PlayerID: scope.SubjectID}, memory.MemoryGovernanceCommand{Key: "result-source-delete", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: graph.Sources[source].Revision}}}); e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(bodyPath); !os.IsNotExist(e) {
				t.Fatal("derived body survived source deletion", e)
			}
			stage, e := r.Files.RecoverDerived(scope.ProductScope, storage, string(body))
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Files.WriteDerived(ctx, scope.ProductScope, stage, string(body)); e == nil {
				t.Fatal("late writer resurrected governed body")
			}
			id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
			if e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, task)
			lease, e := r.Store.BeginOriginalQuery(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.RecordOriginalQuery(ctx, lease); e != nil {
				t.Fatal("deletion erased necessary original cost", e)
			}
			if _, e = os.Stat(bodyPath); !os.IsNotExist(e) {
				t.Fatal("metadata query revived body")
			}
		})
	}
}

func TestMockIndependentHeadRejectsKnownRollback(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	root := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock")
	oldAnchor, e := os.ReadFile(filepath.Join(root, "anchor.json"))
	if e != nil {
		t.Fatal(e)
	}
	quote, e := turns.QuoteTurn(ctx, scope, q, "head-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	if _, e = turns.SubmitTurn(ctx, scope, request, "head-turn"); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	head, e := os.ReadFile(filepath.Join(root, "anchor.json"))
	if e != nil {
		t.Fatal(e)
	}
	journal := journalBytes(t, r)
	if e = os.WriteFile(filepath.Join(root, "anchor.json"), oldAnchor, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "journal.jsonl"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Mock.Ready(ctx); e == nil {
		t.Fatal("self-consistent old J accepted against newer coordinator anchor")
	}
	_ = os.WriteFile(filepath.Join(root, "anchor.json"), head, 0600)
	_ = os.WriteFile(filepath.Join(root, "journal.jsonl"), journal, 0600)
	if e = r.Local.Mock.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(root, "anchor.json"), filepath.Join(root, "anchor.missing")); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Mock.Ready(ctx); e == nil {
		t.Fatal("missing original identity accepted")
	}
	if e = os.Rename(filepath.Join(root, "anchor.missing"), filepath.Join(root, "anchor.json")); e != nil {
		t.Fatal(e)
	}
}

func TestMockFreshServiceRegistrationLostACK(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	root, files := t.TempDir(), t.TempDir()
	s.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("lost fresh registration ACK")
	}
	if e := app.InitializeLocal(ctx, s, root, files); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	s.Commit = nil
	if e := app.InitializeLocal(ctx, s, root, files); e != nil {
		t.Fatal("original registration recovery failed", e)
	}
	var count int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM harness_query_services").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	if _, e := s.DB.Exec("DELETE FROM harness_query_services"); e != nil {
		t.Fatal(e)
	}
	if _, e := app.OpenLocal(ctx, s, root, files); e == nil {
		t.Fatal("ordinary open backfilled missing service")
	}
	if e := app.InitializeLocal(ctx, s, root, files); e == nil {
		t.Fatal("existing initializer backfilled missing service")
	}
}

func TestOriginalQueryRejectsIdentityGenerationAndDeadlineGaps(t *testing.T) {
	for _, gap := range []string{"installation", "generation", "binding", "deadline", "admission"} {
		t.Run(gap, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, true)
			id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
			if e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, task)
			switch gap {
			case "installation":
				_, e = r.Store.DB.Exec("UPDATE memory_installation SET installation_id='wrong-installation'")
			case "generation":
				_, e = r.Store.DB.Exec("UPDATE memory_installation SET generation=generation+1")
			case "binding":
				_, e = r.Store.DB.Exec("DELETE FROM harness_original_queries WHERE task_id=?", task)
			case "deadline":
				_, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", task)
			case "admission":
				e = r.Local.Coordinator.BeginClose(ctx, "close-query-admission")
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.BeginOriginalQuery(ctx, id); e == nil {
				t.Fatal("query crossed missing current authority", gap)
			}
		})
	}
}

func TestLiveResultStagingSurvivesOrdinaryCleanup(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	var storage string
	if e := r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE file_staging SET created_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 2 HOUR) WHERE storage_key=?", storage); e != nil {
		t.Fatal(e)
	}
	if _, e := app.QueueCleanup(ctx, r.Store, scope.ProductScope, "live-result-cleanup"); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if body, e := os.ReadFile(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")); e != nil || len(body) == 0 {
		t.Fatal("live governed result treated as abandoned staging", e)
	}
}
