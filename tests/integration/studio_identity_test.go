package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http/httptest"
	"testing"
)

// These three positive identity fixtures use the real registration/delegation
// ports. They preserve each old scope/action set and add no bootstrap rights.
func studioRegisteredFixture(t *testing.T, r *app.Runtime, scope domain.ProductScope, id, secret string, actions []string) context.Context {
	t.Helper()
	owner := runtimeContext(t, r)
	if e := maintenance.RegisterLocalCredential(owner, r.Store, scope, id, secret); e != nil {
		t.Fatal(e)
	}
	sc := domain.SettingsScope{Kind: "PRODUCT", TenantID: scope.TenantID, ProductID: &scope.ProductID}
	for _, action := range actions {
		page, e := r.Store.ListMembers(owner, sc, 100, "")
		if e != nil {
			t.Fatal(e)
		}
		var revision string
		for _, member := range page.Items {
			if string(member.CredentialID) == id {
				revision = string(member.GrantSetRevision)
			}
		}
		if revision == "" {
			t.Fatal("registered member missing from current snapshot")
		}
		raw, e := json.Marshal(map[string]any{"scope": sc, "kind": "SET_SCOPE_GRANT", "expected_revision": revision, "credential_id": id, "subject_id": scope.SubjectID, "grant": map[string]any{"scope": sc, "action": action, "use": "PRODUCT_ADMIN", "effect": "ALLOW", "enabled": true, "expires_at": nil}})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.ApplyMember(owner, "fixture-grant-"+id+"-"+action, raw); e != nil {
			t.Fatal(e)
		}
	}
	ctx, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+secret)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}

func TestStudioSecondSubjectOwnsRealContentAndCandidate(t *testing.T) {
	r, original, request := candidateHTTPFixture(t)
	ownerCtx := runtimeContext(t, r)
	ownerScope := r.Auth.Scope()
	source, e := r.Content.GetDraft(ownerCtx, ownerScope, original, "")
	if e != nil {
		t.Fatal(e)
	}
	other := ownerScope
	other.SubjectID = "second-creator"
	if _, e = r.Store.DB.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES(?,?,?,1,1,1,1)", other.TenantID, other.ProductID, other.SubjectID); e != nil {
		t.Fatal(e)
	}
	ctx := studioRegisteredFixture(t, r, other, "second-creator-credential", "synthetic-second-creator-credential-0123456789", maintenance.OperatorActions)
	if _, e = r.Content.GetDraft(ctx, other, original, ""); e == nil {
		t.Fatal("other subject read original owner's draft")
	}
	made, e := r.Content.Create(ctx, other, "WORLD", "", source.Input, "second-subject-create")
	if e != nil {
		t.Fatal(e)
	}
	in := made.Input
	in.Name = "second subject own draft"
	saved, e := r.Content.SaveDraft(ctx, other, string(made.DraftID), in, 1, "second-subject-save")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.GetDraft(ownerCtx, ownerScope, string(made.DraftID), ""); e == nil {
		t.Fatal("original credential borrowed second subject ownership")
	}
	request.ExpectedDraftRevision = saved.DraftRevision
	receipt, e := r.Store.AcceptDraftCandidate(ctx, other, string(made.DraftID), "second-subject-candidate", request)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	view, e := r.Store.ReadDraftCandidateTask(ctx, other, string(receipt.TaskID))
	if e != nil || view.TaskState != "COMMITTED" {
		t.Fatal(view, e)
	}
	var rehearsal generated.RehearsalRequest
	if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"2","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &rehearsal); e != nil {
		t.Fatal(e)
	}
	rehearsalReceipt, e := r.Store.AcceptDraftRehearsal(ctx, other, string(made.DraftID), "second-subject-rehearsal", rehearsal)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.QueryRehearsalReportCommit(ctx, other, string(rehearsalReceipt.TaskID)); e != nil {
		t.Fatal(e)
	}
	// The valid private lease-bound rehearsal above must not widen the online
	// owner-only dispatch path. A genuine second principal plus copied public
	// task/world identifiers cannot recreate the sealed rehearsal plan.
	var childTask, childWorld string
	if e = r.Store.DB.QueryRow("SELECT task_id,world_instance_id FROM harness_turns WHERE owner_subject_id=?", other.SubjectID).Scan(&childTask, &childWorld); e != nil {
		t.Fatal(e)
	}
	world := domain.Scope{ProductScope: other, WorldInstanceID: childWorld}
	forged := store.BarrierRequest{ProductScope: other, WorldScope: &world, OwnerSubjectID: other.SubjectID, TaskID: childTask, Purpose: store.ExecutionPurpose, Operation: "TURN_DISPATCH"}
	if _, e = r.Store.OriginalTurnDispatch(ctx, forged); e != store.ErrDenied {
		t.Fatal("second principal/public scope became online owner", e)
	}
	if e = json.Unmarshal([]byte(`{"rehearsalExecution":{"root":"copied","child":"copied"}}`), &forged); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.OriginalTurnDispatch(ctx, forged); e != store.ErrDenied {
		t.Fatal("JSON fabricated private rehearsal plan", e)
	}
	p, e := r.Store.CurrentOperator(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id=?", p.CredentialID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.GetDraft(ctx, other, string(made.DraftID), ""); e == nil {
		t.Fatal("revoked second principal read persisted content")
	}
	if _, e = r.Store.AcceptDraftCandidate(ctx, other, string(made.DraftID), "revoked-second-candidate", request); e == nil {
		t.Fatal("revoked second principal bought new generation")
	}
}

func TestStudioRevokedIssuerPreservesOriginalQueryAndAccounting(t *testing.T) {
	r, draft, request := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := r.Auth.Scope()
	receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "original-query-after-revoke", request)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDropOriginalReply
	if e = r.Worker.Once(context.Background()); e == nil {
		t.Fatal("original reply fault not observed")
	}
	r.Worker.Turns.Fault = 0
	if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", receipt.TaskID); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.QueryOriginalOnce(context.Background()); e != nil {
		t.Fatal("issuer revocation blocked original query", e)
	}
	if _, _, e = r.Store.RunOriginalBills(context.Background()); e != nil {
		t.Fatal("issuer revocation blocked original accounting", e)
	}
	var sent int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND send_started_at IS NOT NULL", receipt.TaskID).Scan(&sent); e != nil || sent != 1 {
		t.Fatal("original recovery repurchased", sent, e)
	}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draft, "new-after-revoke", request); e == nil {
		t.Fatal("revoked issuer authorized new intake")
	}
}

func TestStudioSecondCredentialAuthenticatesCurrentRegistry(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := context.Background()
	secret := "synthetic-second-credential-00000000000001"
	studioRegisteredFixture(t, r, scope, "second", secret, []string{"content.read"})
	got, e := r.Auth.Authorize(ctx, "Bearer "+secret, scope.ProductID, "content.read")
	if e != nil || got != scope {
		t.Fatal("actual second registry credential rejected", got, e)
	}
	if _, e = r.Auth.Authorize(ctx, "Bearer "+secret, scope.ProductID, "content.edit"); e != auth.ErrForbidden {
		t.Fatal("second borrowed configured owner actions", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='second'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Auth.Authorize(ctx, "Bearer "+secret, scope.ProductID, "content.read"); e != auth.ErrUnauthenticated {
		t.Fatal("disabled second credential remained current", e)
	}
}

func TestStudioProgramCleanupKeepsOriginalObligation(t *testing.T) {
	for _, mode := range []string{"REVOKED_CREDENTIAL", "MISSING_INTENT"} {
		t.Run(mode, func(t *testing.T) {
			r, scope, _ := runtimeFixture(t)
			ctx, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+runtimeToken)
			if e != nil {
				t.Fatal(e)
			}
			receipt, e := app.QueueCleanup(ctx, r.Store, scope, "program-cleanup")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
				t.Fatal(e)
			}
			if mode == "MISSING_INTENT" {
				if _, e = r.Store.DB.Exec("DELETE FROM admin_results WHERE schema_name='AdminTaskReceipt'"); e != nil {
					t.Fatal(e)
				}
			}
			e = r.Worker.Once(context.Background())
			if mode == "MISSING_INTENT" {
				if e == nil {
					t.Fatal("cleanup without accepted original intent executed")
				}
				return
			}
			if e != nil {
				t.Fatal("credential revocation prevented necessary cleanup", e)
			}
			var state, provenance string
			var credential sql.NullString
			if e = r.Store.DB.QueryRow("SELECT t.state,j.issuer_provenance,j.issuer_credential_id FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.task_id=?", receipt.TaskID).Scan(&state, &provenance, &credential); e != nil || state != "COMMITTED" || provenance != "PROGRAM_OWNED" || credential.Valid {
				t.Fatal(state, provenance, credential, e)
			}
		})
	}
}

func TestStudioDurableIssuerUsesActualCredential(t *testing.T) {
	r, draft, request := candidateHTTPFixture(t)
	scope := r.Auth.Scope()
	secret := "synthetic-non-owner-credential-00000000001"
	ctx := studioRegisteredFixture(t, r, scope, "second", secret, []string{"content.read", "asset.read", "content.assist", "task.read"})
	receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "second-candidate", request)
	if e != nil {
		t.Fatal(e)
	}
	var credential, provenance string
	if e = r.Store.DB.QueryRow("SELECT issuer_credential_id,issuer_provenance FROM registered_jobs WHERE task_id=?", receipt.TaskID).Scan(&credential, &provenance); e != nil || credential != "second" || provenance != "DIRECT_OPERATOR" {
		t.Fatal(credential, provenance, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	q := httptest.NewRequest("GET", "/world/v1/admin/content-drafts/"+draft+"?product_id=coffee", nil)
	q.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, q)
	if w.Code != 200 {
		t.Fatal("second HTTP relies on configured owner readiness", w.Code, w.Body.String())
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal("second issuer borrowed owner", e)
	}
	var view generated.AdminTaskView
	view, e = r.Store.ReadDraftCandidateTask(ctx, scope, string(receipt.TaskID))
	if e != nil || view.TaskState != "COMMITTED" {
		t.Fatal(view, e)
	}
}

func TestStudioOwningSaveRequiresCurrentPrincipal(t *testing.T) {
	r, scope, release := runtimeFixture(t)
	ctx, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+runtimeToken)
	if e != nil {
		t.Fatal(e)
	}
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	input := published.Input
	input.Name = "Synthetic current identity"
	if _, e = r.Content.SaveDraft(context.Background(), scope, string(published.DraftID), input, 1, "missing-identity"); e != store.ErrDenied {
		t.Fatal("missing principal accepted", e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.edit'")
		return e
	}
	if _, e = r.Content.SaveDraft(ctx, scope, string(published.DraftID), input, 1, "withdraw-in-owning-tx"); e != store.ErrDenied {
		t.Fatal("owning revocation not rechecked", e)
	}
	r.Store.BeforeCommit = nil
	saved, e := r.Content.SaveDraft(ctx, scope, string(published.DraftID), input, 1, "actual-principal")
	if e != nil || saved.DraftRevision != "2" {
		t.Fatal(saved, e)
	}
}
