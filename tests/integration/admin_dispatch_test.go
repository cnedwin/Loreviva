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
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func candidateDispatchFixture(t *testing.T) (*app.Runtime, domain.TaskLease) {
	return candidateDispatchInputFixture(t, nil)
}
func candidateDispatchInputFixture(t *testing.T, prepare func(*app.Runtime, domain.ProductScope, *generated.DraftInput)) (*app.Runtime, domain.TaskLease) {
	t.Helper()
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	if prepare != nil {
		prepare(r, scope, &published.Input)
	}
	draft, e := r.Content.Create(ctx, scope, "WORLD", "", published.Input, "candidate-original-draft")
	if e != nil {
		t.Fatal("candidate fixture draft", e)
	}
	q := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: published.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	accepted, e := r.Store.AcceptDraftCandidate(ctx, scope, string(draft.DraftID), "dispatch-candidate", q)
	if e != nil {
		t.Fatal("candidate fixture intake", e)
	}
	l, e := r.Store.ClaimRegistered(ctx, "candidate-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
	if e != nil || l.TaskID != string(accepted.TaskID) {
		t.Fatal(l, e)
	}
	return r, l.TaskLease
}
func TestProductCandidateRealAuthorizeAndOriginalMarker(t *testing.T) {
	r, l := candidateDispatchFixture(t)
	ctx := runtimeContext(t, r)
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal("legitimate fixed Product dispatch unavailable", e)
	}
	if d.WorldScope != nil || d.Slot != 0 || d.HarnessQuoteID == "" {
		t.Fatal(d)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal("authorize", e)
	}
	d.CommandKey = content.Identity(l.TaskID, "candidate-send")
	d.CommitKey = d.CommandKey
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || !a.CanSend || a.OriginalSend() == nil {
		t.Fatal("marker", a, e)
	}
	again, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || again.CanSend || !again.QueryOriginal || again.OriginalSend() != nil {
		t.Fatal("existing marker repurchased", again, e)
	}
	var calls, steps, holds, queries int
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_original_queries WHERE task_id=t.task_id) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&calls, &steps, &holds, &queries); e != nil || calls != 1 || steps != 3 || holds != 5 || queries != 1 {
		t.Fatal(calls, steps, holds, queries, e)
	}
}

func TestProductCandidateActualGenerateUsesIndependentJournal(t *testing.T) {
	r, l := candidateDispatchFixture(t)
	ctx := runtimeContext(t, r)
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(l.TaskID, "candidate-send")
	d.CommitKey = d.CommandKey
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal("real Product Generate", e)
	}
	m, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
	if e != nil || m.State != "SUCCEEDED" || !m.BodyAvailable || !m.UsageKnown || !m.NoFurtherCost || m.Identity.Kind != "CANDIDATE" || m.Identity.Scope.WorldInstanceID != "" {
		t.Fatal(m, e)
	}
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e == nil {
		t.Fatal("opaque permit reused")
	}
	if body, e := r.Files.ReadPendingCandidateResult(ctx, d); e != nil || !bytes.Contains(body, []byte(`"fields"`)) {
		t.Fatal("actual governed body unavailable", string(body), e)
	}
}

func productSent(t *testing.T) (*app.Runtime, domain.TaskLease, store.DispatchRequest, store.DispatchAuthorization) {
	t.Helper()
	r, l := candidateDispatchFixture(t)
	ctx := runtimeContext(t, r)
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(l.TaskID, "candidate-send")
	d.CommitKey = d.CommandKey
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	return r, l, d, a
}
func TestProductCandidateRestartMarkerAndUnknownNeverRegenerate(t *testing.T) {
	for _, mode := range []string{"LOST_REPLY", "KNOWN_UNSENT", "MARKER_ACK_UNKNOWN"} {
		t.Run(mode, func(t *testing.T) {
			r, l := candidateDispatchFixture(t)
			ctx := runtimeContext(t, r)
			d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d.CommandKey = content.Identity(l.TaskID, "candidate-send")
			d.CommitKey = d.CommandKey
			if mode == "MARKER_ACK_UNKNOWN" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("marker reply lost")
				}
			}
			a, e := r.Store.StartSend(ctx, d, d.AttemptID)
			r.Store.Commit = nil
			if mode == "MARKER_ACK_UNKNOWN" {
				if !errors.Is(e, store.ErrCommitUnknown) || a.OriginalSend() != nil {
					t.Fatal(a, e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if mode == "LOST_REPLY" {
				if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
					t.Fatal(e)
				}
			}
			before := journalBytes(t, r)
			local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
			if e != nil {
				t.Fatal(e)
			}
			defer local.Participant.Close(ctx)
			fresh := app.NewRuntime(r.Store, r.Files, r.Auth)
			fresh.BindLocal(local)
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", l.TaskID); e != nil {
				t.Fatal(e)
			}
			current, e := r.Store.ClaimRegistered(ctx, "candidate-takeover", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
				t.Fatal("old fence used")
			}
			next, e := r.Store.PrepareDraftCandidateDispatch(ctx, current.TaskLease, "candidate-takeover")
			if e != nil {
				t.Fatal(e)
			}
			next.CommandKey = d.CommandKey
			next.CommitKey = d.CommitKey
			denied, e := r.Store.StartSend(ctx, next, next.AttemptID)
			if e != nil || denied.CanSend || !denied.QueryOriginal || denied.OriginalSend() != nil {
				t.Fatal(denied, e)
			}
			id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, l.TaskID)
			q, e := r.Store.BeginOriginalQuery(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			m, e := r.Store.RecordOriginalQuery(ctx, q)
			if e != nil {
				t.Fatal(e)
			}
			want := "UNKNOWN"
			if mode == "LOST_REPLY" {
				want = "SUCCEEDED"
			}
			if m.State != want || !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("original query mutated journal", m.State)
			}
			if mode == "LOST_REPLY" {
				if _, e = r.Files.ReadPendingCandidateResult(ctx, next); e != nil {
					t.Fatal("takeover body", e)
				}
			}
		})
	}
}
func TestProductCandidatePermissionAndActualBodyDeletion(t *testing.T) {
	r, _, d, a := productSent(t)
	ctx := runtimeContext(t, r)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	m, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
	if e != nil {
		t.Fatal(e)
	}
	body, e := r.Files.ReadPendingCandidateResult(ctx, d)
	if e != nil || len(body) == 0 {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("withdrawn source read")
	}
	if _, e = r.Store.DB.Exec("INSERT INTO operator_actions(credential_id,action) VALUES('owner','content.assist')"); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(r.Files.RootPath, d.ProductScope.TenantID, d.ProductScope.ProductID, m.BodyStorage+".part")
	if e = os.Remove(file); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("missing bytes rebuilt")
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("J rebuilt")
	}
}
func TestProductCandidateNecessaryCostAfterDeadlineAndAssistWithdrawal(t *testing.T) {
	r, l, _, a := productSent(t)
	ctx := runtimeContext(t, r)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", l.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	q, e := r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal("necessary Product cost denied", e)
	}
	m, e := r.Store.RecordNecessaryCostQuery(ctx, q)
	if e != nil || !m.UsageKnown || !m.NoFurtherCost || m.State != "SUCCEEDED" {
		t.Fatal(m, e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cost performed Generate")
	}
	var calls, steps int
	var state string
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,t.state FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&calls, &steps, &state); e != nil || calls != 1 || steps != 3 || state != "RUNNING" {
		t.Fatal(calls, steps, state, e)
	}
}

func TestProductCandidateCleanupKeepsLiveAndPurgesActualDeletedSource(t *testing.T) {
	r, l, d, a := productSent(t)
	ctx := runtimeContext(t, r)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	m, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
	if e != nil {
		t.Fatal(e)
	}
	var upper uint64
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id) FROM file_staging").Scan(&upper); e != nil {
		t.Fatal(e)
	}
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 24 HOUR)").Scan(&cutoff); e != nil {
		t.Fatal(e)
	}
	page, e := r.Files.CleanupPage(ctx, l.ProductScope, cutoff, 0, upper)
	if e != nil || page.Removed != 0 {
		t.Fatal("live candidate was treated as orphan", page, e)
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e != nil {
		t.Fatal(e)
	}
	var draft string
	if e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "delete-candidate-source-impact")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "delete-candidate-source"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("deleted source still readable")
	}
	page, e = r.Files.CleanupPage(ctx, l.ProductScope, cutoff.Add(time.Hour), 0, upper)
	if e != nil || page.Removed != 1 {
		t.Fatal(page, e)
	}
	file := filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, m.BodyStorage+".part")
	if _, e = os.Stat(file); !os.IsNotExist(e) {
		t.Fatal("source-derived bytes remain", e)
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("purged original regenerated")
	}
}

func TestProductCandidateBothSendBoundariesAndConsumedPermit(t *testing.T) {
	for _, phase := range []string{"AUTHORIZE", "SEND", "CONSUME"} {
		for _, change := range []string{"ASSIST", "REVISION", "CANCEL", "QUERY_SERVICE"} {
			t.Run(phase+"/"+change, func(t *testing.T) {

				r, l := candidateDispatchFixture(t)
				ctx := runtimeContext(t, r)
				d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
				if e != nil {
					t.Fatal(e)
				}
				if phase != "AUTHORIZE" {
					if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
						t.Fatal(e)
					}
				}
				d.CommandKey = content.Identity(l.TaskID, "candidate-send")
				d.CommitKey = d.CommandKey
				var permit *store.OriginalSendPermit
				if phase == "CONSUME" {
					a, e := r.Store.StartSend(ctx, d, d.AttemptID)
					if e != nil {
						t.Fatal(e)
					}
					permit = a.OriginalSend()
				}
				switch change {
				case "ASSIST":
					_, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
				case "QUERY_SERVICE":
					if phase == "AUTHORIZE" {
						_, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=0")
					} else {
						_, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0")
					}
				case "CANCEL":
					runtimeHTTP(t, r, "POST", "/tasks/"+l.TaskID+"/cancel", "cancel-product-candidate", generated.AdminCancelRequest{ProductID: "coffee", Reason: "Cancel authoring candidate"}, 202, nil)
				case "REVISION":
					var draft string
					e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft)
					if e == nil {
						v, err := r.Content.GetDraft(ctx, l.ProductScope, draft, "")
						e = err
						if e == nil {
							_, e = r.Content.SaveDraft(ctx, l.ProductScope, draft, v.Input, 1, "revise-product-before-send")
						}
					}
				}
				if e != nil {
					t.Fatal("fixture current change", e)
				}
				before := journalBytes(t, r)
				switch phase {
				case "AUTHORIZE":
					_, e = r.Store.AuthorizeDispatch(ctx, d)
				case "SEND":
					_, e = r.Store.StartSend(ctx, d, d.AttemptID)
				case "CONSUME":
					e = r.Worker.Turns.GenerateDraftCandidate(ctx, permit)
				}
				if e == nil {
					t.Fatal("current eligibility lost but purchase/send accepted")
				}
				if !bytes.Equal(before, journalBytes(t, r)) {
					t.Fatal("denied purchase touched provider")
				}
			})
		}
	}
}

func TestProductCandidateCanonicalLimitRejectsBeforeIntake(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	p, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	in := p.Input
	for i, b := range in.Objects {
		var object map[string]any
		if json.Unmarshal(b, &object) != nil {
			t.Fatal("bad fixture")
		}
		if object["kind"] == "CHARACTER" {
			in.RootObjectID = generated.Identity(object["object_id"].(string))
			lines := make([]string, 20)
			for n := range lines {
				lines[n] = strings.Repeat("\\", 500)
			}
			object["example_lines"] = lines
			in.Objects[i], _ = json.Marshal(object)
			break
		}
	}
	d, e := r.Content.Create(ctx, scope, "CHARACTER", "", in, "oversized-candidate-input")
	if e != nil {
		t.Fatal("large valid authoring field fixture", e)
	}
	q := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "INTERACTION", ObjectID: in.RootObjectID, SelectedFields: []string{"example_lines"}, ApprovedCostUnits: "0"}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, string(d.DraftID), "oversized-candidate", q); e == nil {
		t.Fatal("oversized actual canonical accepted")
	}
	var tasks, holds int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_admin_candidates),(SELECT COUNT(*) FROM reservations)").Scan(&tasks, &holds); e != nil || tasks != 0 || holds != 0 {
		t.Fatal(tasks, holds, e)
	}
}

func TestProductCandidatePendingReadRequiresCompleteFrozenBindings(t *testing.T) {
	for _, table := range []string{"harness_candidate_attempts", "harness_candidate_results"} {
		t.Run(table, func(t *testing.T) {
			r, _, d, permit := productSent(t)
			ctx := runtimeContext(t, r)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, permit.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Files.ReadPendingCandidateResult(ctx, d); e != nil {
				t.Fatal("original frozen body", e)
			}
			var e error
			// Corrupt one immutable metadata field while preserving the original
			// request/route/draft hashes. A bare hash is not the frozen binding.
			if table == "harness_candidate_attempts" {
				_, e = r.Store.DB.Exec("UPDATE harness_candidate_attempts SET document=JSON_SET(document,'$.Selection.Fields',JSON_ARRAY('background')) WHERE task_id=?", d.TaskID)
			} else {
				_, e = r.Store.DB.Exec("UPDATE harness_candidate_results SET source_binding=JSON_SET(source_binding,'$.Selection.Fields',JSON_ARRAY('background')) WHERE task_id=?", d.TaskID)
			}
			if e != nil {
				t.Fatal("corruption fixture", e)
			}
			before := journalBytes(t, r)
			if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
				t.Fatal("changed immutable binding still authorized original bytes")
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("inspection changed provider evidence")
			}
		})
	}
}
