package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestIntegrationControlTaskMetadata(t *testing.T) {
	r, _, service, req, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "control-metadata-world", req)
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "control-metadata-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil {
		t.Fatal(e)
	}
	v, e := r.Store.ReadIntegrationTask(service, a.TaskID)
	if e != nil || v.Task.TaskKind != "CONTROL" {
		t.Fatal("real SERVICE control metadata", e)
	}
	// The TURN-specific wire is CLIENT-only even for a SERVICE-owned CONTROL.
	h := httptest.NewRequest("POST", "/world/v1/turns/"+a.TaskID+"/cancel", bytes.NewBufferString(`{}`))
	h.Header.Set("Authorization", "Bearer synthetic-s66-create-service-secret-32")
	h.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	h.Header.Set("Content-Type", "application/json")
	h.Header.Set("Idempotency-Key", "control-through-turn-wire")
	hw := httptest.NewRecorder()
	r.Handler().ServeHTTP(hw, h)
	if hw.Code != 401 {
		t.Fatal("SERVICE used CLIENT-only TURN cancel", hw.Code)
	}
	c, e := r.Store.CancelIntegrationTask(service, a.TaskID, "control-metadata-cancel", generated.CancelRequest{})
	if e != nil || c.ExternalCancelState != "NOT_SENT" {
		t.Fatal("real SERVICE control cancel", e)
	}
	if _, e = r.Store.CancelIntegrationTask(service, a.TaskID, "control-metadata-cancel", generated.CancelRequest{}); e != nil {
		t.Fatal("control cancel replay", e)
	}
	l, e := r.Store.ClaimRegistered(context.Background(), "cancelled-control-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: a.TaskID}})
	if e == nil && r.World.Process(context.Background(), l, "cancelled-control-worker") == nil {
		t.Fatal("cancelled control executed")
	}
}

func TestIntegrationTurnResultHTTP(t *testing.T) {
	var token string
	r, a := integrationAcceptedTurn(t, func(_ *app.Runtime, s generated.ClientSession) { token = s.AccessToken })
	l, e := r.Store.ClaimRegistered(context.Background(), "result-http-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "result-http-worker"); e != nil {
		t.Fatal(e)
	}
	q := httptest.NewRequest("GET", "/world/v1/turns/"+a.TurnID, nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	request, e := http.NewRequest("GET", server.URL+q.URL.Path, nil)
	if e != nil {
		t.Fatal(e)
	}
	request.Header = q.Header
	response, e := server.Client().Do(request)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	body, e := io.ReadAll(response.Body)
	if e != nil {
		t.Fatal(e)
	}
	if response.StatusCode != 200 || generated.Validate("TurnView", body) != nil {
		t.Fatal("committed HTTP result", response.StatusCode)
	}
	var out generated.TurnView
	if json.Unmarshal(body, &out) != nil || len(out.ApprovedPackets) != 1 || out.Attempts[0].UsageState != "AUTHORITATIVE" {
		t.Fatal("real approved packet and usage")
	}
}

func TestIntegrationNecessaryQueryAfterWithdrawal(t *testing.T) {
	var client context.Context
	r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
		var e error
		client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+s.AccessToken, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
	})
	l, e := r.Store.ClaimRegistered(context.Background(), "necessary-query-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDropOriginalReply
	if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "necessary-query-worker"); !errors.Is(e, harness.ErrMockReplyLost) {
		t.Fatal("actual UNKNOWN original request", e)
	}
	before := journalBytes(t, r)
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "necessary-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	c, e := r.Store.CancelIntegrationTask(client, a.TurnID, "necessary-cancel", generated.CancelRequest{})
	if e != nil || c.ExternalCancelState != "UNKNOWN" {
		t.Fatal("actual unknown cancellation", e)
	}
	if _, e = r.Turns.PrepareOriginalTurn(context.Background(), l.TaskLease, "necessary-query-worker"); e == nil {
		t.Fatal("withdrawn new generation authorized")
	}
	scope := domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}
	id, e := r.Store.OriginalMockIdentity(context.Background(), scope, a.TurnID)
	if e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, a.TurnID) // Existing query scheduler fault fixture; no natural-expiry clock is changed.
	ql, e := r.Store.BeginOriginalQuery(context.Background(), id)
	if e != nil {
		t.Fatal("necessary original query", e)
	}
	out, e := r.Store.RecordOriginalQuery(context.Background(), ql)
	if e != nil || out.State != "SUCCEEDED" {
		t.Fatal("necessary query result", e)
	}
	if !out.UsageKnown || !out.NoFurtherCost || out.CostAtoms != 0 || out.Identity != id {
		t.Fatal("original exact cost metadata")
	}
	// The bill-posting owner requires COMMITTED or deadline settlement. This
	// cancelled, still-in-deadline task must not bypass that separate gate.
	if _, e = r.Store.RecordOriginalBillItem(context.Background(), id, "original-charge"); e == nil {
		t.Fatal("premature bill posting")
	}
	owner := runtimeContext(t, r)
	cleanup, e := app.QueueCleanup(owner, r.Store, r.Auth.Scope(), "integration-withdrawal-cleanup")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal("original PROGRAM cleanup after credential withdrawal", e)
	}
	var provenance, cleanupState string
	var credential sql.NullString
	if e = r.Store.DB.QueryRow("SELECT j.issuer_provenance,j.issuer_credential_id,t.state FROM registered_jobs j JOIN tasks t USING(tenant_id,product_id,task_id) WHERE t.task_id=?", string(cleanup.TaskID)).Scan(&provenance, &credential, &cleanupState); e != nil || provenance != "PROGRAM_OWNED" || credential.Valid || cleanupState != "COMMITTED" {
		t.Fatal("original cleanup proof", e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("query generated another request")
	}
	var state string
	var calls int
	if e = r.Store.DB.QueryRow("SELECT state,calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&state, &calls); e != nil || state != "CANCELLED" || calls != 1 {
		t.Fatal("narrative revived", state, calls, e)
	}
}

func TestIntegrationIssuerFinalAndClose(t *testing.T) {
	for _, mode := range []string{"turn-final", "control-final", "closed-seal"} {
		t.Run(mode, func(t *testing.T) {
			var r *app.Runtime
			var task string
			var lease store.RegisteredLease
			var e error
			if mode == "control-final" {
				var service context.Context
				var req generated.CreateInstanceRequest
				r, _, service, req, _ = integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ"})
				w, err := r.World.CreateIntegrationInstance(service, "issuer-world", req)
				if err != nil {
					t.Fatal(err)
				}
				a, err := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "issuer-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
				if err != nil {
					t.Fatal(err)
				}
				task = a.TaskID
				lease, e = r.Store.ClaimRegistered(context.Background(), "issuer-final-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: task}})
			} else {
				var a generated.TurnAccepted
				r, a = integrationAcceptedTurn(t)
				task = a.TurnID
				lease, e = r.Store.ClaimRegistered(context.Background(), "issuer-final-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: task}})
			}
			if e != nil {
				t.Fatal(e)
			}
			enter := func(ctx context.Context) (context.Context, error) {
				if mode == "control-final" {
					return r.Store.EnterControlTaskIssuer(ctx, lease, "issuer-final-worker")
				}
				return r.Store.EnterIntegrationTaskIssuer(ctx, lease.TaskLease, "issuer-final-worker")
			}
			if mode == "closed-seal" {
				sealed, err := enter(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if e = r.Store.Close(); e != nil {
					t.Fatal(e)
				}
				if _, e = enter(sealed); e == nil {
					t.Fatal("closed Store reissued retained seal")
				}
				return
			}
			injected := false
			r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, "UPDATE integration_task_principals SET customer_id='foreign-customer' WHERE task_id=?", task)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				injected = err == nil && n == 1
				return err
			}
			if _, e = enter(context.Background()); e == nil {
				t.Fatal("issuer returned without same-Tx final gate")
			}
			r.Store.BeforeReadFinish = nil
			if !injected {
				t.Fatal("issuer final fault not reached", e)
			}
			if _, e = enter(context.Background()); e != nil {
				t.Fatal("issuer rollback positive", e)
			}
		})
	}
}

// This is the original 120-second task deadline, not a shortened clock. Cost
// authority after it is independent of withdrawn narrative/body authority.
func TestIntegrationNecessaryCostAfterNaturalDeadline(t *testing.T) {
	var client context.Context
	r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
		var e error
		client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+s.AccessToken, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
	})
	l, e := r.Store.ClaimRegistered(context.Background(), "natural-cost-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDropOriginalReply
	if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "natural-cost-worker"); !errors.Is(e, harness.ErrMockReplyLost) {
		t.Fatal("actual sent unknown", e)
	}
	scope := domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}
	id, e := r.Store.OriginalMockIdentity(context.Background(), scope, a.TurnID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "natural-cost-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BeginNecessaryCostQuery(context.Background(), id); e == nil {
		t.Fatal("cost query before original deadline/exhaustion")
	}
	var accepted, execute, settle, now time.Time
	if e = r.Store.DB.QueryRow("SELECT accepted_at,execute_by,settle_by,UTC_TIMESTAMP(6) FROM harness_envelopes WHERE task_id=?", a.TurnID).Scan(&accepted, &execute, &settle, &now); e != nil {
		t.Fatal(e)
	}
	if execute.Sub(accepted) != 120*time.Second || settle.Sub(accepted) != 180*time.Second {
		t.Fatal("original deadline changed")
	}
	remaining := execute.Sub(now)
	if remaining <= 0 || remaining > 120*time.Second {
		t.Fatal("natural cost precondition", remaining)
	}
	before := journalBytes(t, r)
	time.Sleep(remaining + 50*time.Millisecond)
	lease, e := r.Store.BeginNecessaryCostQuery(context.Background(), id)
	if e != nil {
		t.Fatal("actual post-deadline necessary cost", e)
	}
	if _, e = r.Store.RecordOriginalQuery(context.Background(), lease); e == nil {
		t.Fatal("cost permit gained full query")
	}
	meta, e := r.Store.RecordNecessaryCostQuery(context.Background(), lease)
	if e != nil || meta.Identity != id || !meta.UsageKnown || !meta.NoFurtherCost || meta.CostAtoms != 0 {
		t.Fatal("exact cost-only positive", e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cost bought new generation")
	}
	var afterExecute, afterSettle time.Time
	var calls int
	if e = r.Store.DB.QueryRow("SELECT e.execute_by,e.settle_by,t.calls_used FROM harness_envelopes e JOIN tasks t USING(tenant_id,product_id,task_id) WHERE task_id=?", a.TurnID).Scan(&afterExecute, &afterSettle, &calls); e != nil || !afterExecute.Equal(execute) || !afterSettle.Equal(settle) || calls != 1 {
		t.Fatal("cost changed intake", e)
	}
	if _, _, e = r.Files.ReadApprovedTurn(client, scope, a.TurnID); e == nil {
		t.Fatal("cost authorized narrative")
	}
}

// Quote and intake use the same real registered player and committed world;
// a Service quote does not turn the Service into that player's HTTP principal.
func TestIntegrationTurnIntake(t *testing.T) {
	integrationAcceptedTurn(t)
}

func TestIntegrationRepairAndQuoteProof(t *testing.T) {
	for _, mode := range []string{"repair", "quote-expiry", "quote-receipt"} {
		t.Run(mode, func(t *testing.T) {
			r, a := integrationAcceptedTurn(t)
			if mode == "quote-expiry" {
				res, e := r.Store.DB.Exec("UPDATE harness_turn_quotes SET public_document=JSON_SET(public_document,'$.expires_at','2099-01-01T00:00:00Z') WHERE quote_id=?", a.QuoteID)
				if e != nil {
					t.Fatal("quote fault", e)
				}
				if n, e := res.RowsAffected(); e != nil || n != 1 {
					t.Fatal("quote fault row", n, e)
				}
			}
			if mode == "quote-receipt" {
				res, e := r.Store.DB.Exec("UPDATE commit_receipts SET command_key='different-quote-key' WHERE commit_key=(SELECT quote_receipt FROM integration_quotes WHERE quote_id=?)", a.QuoteID)
				if e != nil {
					t.Fatal("receipt fault", e)
				}
				if n, e := res.RowsAffected(); e != nil || n != 1 {
					t.Fatal(n, e)
				}
			}
			l, e := r.Store.ClaimRegistered(context.Background(), "repair-proof-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			if mode != "repair" {
				if _, e = r.Turns.PrepareOriginalTurn(context.Background(), l.TaskLease, "repair-proof-worker"); e == nil {
					t.Fatal("quote proof mutation escaped", mode)
				}
				return
			}
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "repair-proof-worker"); e != nil {
				t.Fatal("real integration repair", e)
			}
			var state string
			var calls int
			if e = r.Store.DB.QueryRow("SELECT state,calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&state, &calls); e != nil || state != "COMMITTED" || calls != 2 {
				t.Fatal("actual bounded repair", state, calls, e)
			}
		})
	}
}
func integrationAcceptedTurn(t *testing.T, beforeIntake ...func(*app.Runtime, generated.ClientSession)) (*app.Runtime, generated.TurnAccepted) {
	t.Helper()
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "turn-world", request)
	if e != nil {
		t.Fatal(e)
	}
	session, e := r.Store.ExchangeIntegrationSession(service, "turn-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	consent, e := r.Store.ApplyPlayerSelfConsent(client, "turn-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
	if e != nil || !consent.Snapshot.CurrentUseEnabled {
		t.Fatal("real positive use", e)
	}
	start, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "turn-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(context.Background(), "turn-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: start.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.World.Process(context.Background(), lease, "turn-worker"); e != nil {
		t.Fatal(e)
	}
	var actor string
	var wr, cv uint64
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),w.world_revision,w.control_version FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", w.Scope.WorldInstanceID).Scan(&actor, &wr, &cv); e != nil {
		t.Fatal(e)
	}
	for _, hook := range beforeIntake {
		hook(r, session)
	}
	port, ok := any(r.Store).(interface {
		QuoteIntegrationTurn(context.Context, string, generated.QuoteRequest) (generated.Quote, error)
		AcceptIntegrationTurn(context.Context, string, string, generated.TurnRequest) (generated.TurnAccepted, error)
	})
	if !ok {
		t.Fatal("real Registry/CREATE/Session/INITIAL/Background START succeeded: integration TURN owning ports unavailable")
	}
	q, e := port.QuoteIntegrationTurn(service, "turn-quote", generated.QuoteRequest{Scope: w.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"})
	if e != nil {
		t.Fatal("SERVICE quote", e)
	}
	in := generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr, 10), ControlVersion: strconv.FormatUint(cv, 10)}, PrimaryActorID: actor, QuoteID: q.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	a, e := port.AcceptIntegrationTurn(client, w.Scope.WorldInstanceID, "turn-intake", in)
	if e != nil {
		t.Fatal("CLIENT intake", e)
	}
	var valid bool
	if e = r.Store.DB.QueryRow("SELECT j.issuer_provenance='DIRECT_INTEGRATION' AND j.issuer_credential_id IS NULL AND p.accepted_use_generation=1 AND p.accepted_consent_revision=1 AND p.accepted_use_expires_at>p.accepted_at AND p.intake_receipt=h.accepted_commit_key AND p.intake_hash=h.command_hash AND e.execute_by=DATE_ADD(e.accepted_at,INTERVAL 120 SECOND) FROM integration_task_principals p JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE p.task_id=?", a.TurnID).Scan(&valid); e != nil || !valid {
		t.Fatal("atomic original intake proof", e)
	}
	replay, e := port.AcceptIntegrationTurn(client, w.Scope.WorldInstanceID, "turn-intake", in)
	if e != nil || replay.TurnID != a.TurnID {
		t.Fatal("original intake replay", e)
	}
	return r, a
}

func TestIntegrationTurnBackgroundDispatch(t *testing.T) {
	r, a := integrationAcceptedTurn(t)
	ctx := context.Background()
	l, e := r.Store.ClaimRegistered(ctx, "integration-turn-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.Turns.PrepareOriginalTurn(ctx, l.TaskLease, "integration-turn-worker")
	if e != nil {
		t.Fatal("real Background issuer/Prepare", e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal("real AuthorizeDispatch", e)
	}
	d.CommandKey = content.Identity(a.TurnID, "send")
	d.CommitKey = d.CommandKey
	mark, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || !mark.CanSend || !mark.Receipt.Committed {
		t.Fatal("real StartSend", e)
	}
	again, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || again.CanSend || !again.QueryOriginal {
		t.Fatal("single original send", e)
	}
	var calls int
	if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&calls); e != nil || calls != 1 {
		t.Fatal("original bounded call", calls, e)
	}
}

func TestIntegrationTurnOriginalCommit(t *testing.T) {
	r, a := integrationAcceptedTurn(t)
	ctx := context.Background()
	l, e := r.Store.ClaimRegistered(ctx, "integration-original-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(ctx, l.TaskLease, "integration-original-worker"); e != nil {
		t.Fatal("real original Mock/validation/approval/commit", e)
	}
	var committed bool
	if e = r.Store.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM harness_turn_commits c JOIN commit_receipts r ON r.commit_key=c.receipt_key WHERE c.task_id=? AND r.operation='HARNESS_TURN_COMMIT' AND r.phase='COMMIT')`, a.TurnID).Scan(&committed); e != nil || !committed {
		t.Fatal("original world COMMIT", e)
	}
}

// One original fifteen-minute issuance is allowed to expire naturally. The
// accepted task keeps its original 120-second deadline and original use tuple.
func TestIntegrationSessionExpiryIsNotRevocation(t *testing.T) {
	started := time.Now()
	var token string
	var expires time.Time
	r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
		token = s.AccessToken
		var e error
		expires, e = time.Parse(time.RFC3339Nano, s.ExpiresAt)
		if e != nil {
			t.Fatal(e)
		}
		var issued time.Time
		if e = r.Store.DB.QueryRow("SELECT issued_at FROM integration_principals WHERE kind='CLIENT' AND world_instance_id=?", s.Scope.WorldInstanceID).Scan(&issued); e != nil {
			t.Fatal("PRECONDITION_FAIL original issuance", e)
		}
		if expires.Sub(issued) != 15*time.Minute {
			t.Fatal("PRECONDITION_FAIL original TTL")
		}
		t.Log("actual original 15-minute Session; waiting for the one 60–65-second intake window")
		wait := time.Until(expires.Add(-63 * time.Second))
		if wait <= 0 {
			t.Fatal("PRECONDITION_FAIL missed intake window")
		}
		time.Sleep(wait)
	})
	var accepted, deadline time.Time
	if e := r.Store.DB.QueryRow("SELECT accepted_at,execute_by FROM harness_envelopes WHERE task_id=?", a.TurnID).Scan(&accepted, &deadline); e != nil {
		t.Fatal(e)
	}
	remaining := expires.Sub(accepted)
	if remaining < 60*time.Second || remaining > 65*time.Second || deadline.Sub(accepted) != 120*time.Second {
		t.Fatal("PRECONDITION_FAIL original intake window/deadline", remaining)
	}
	t.Logf("original intake accepted with remaining %.3fs; original task deadline120s", remaining.Seconds())
	time.Sleep(time.Until(expires.Add(250 * time.Millisecond)))
	q := httptest.NewRequest("GET", "/world/v1/self-consent", nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, q)
	if w.Code != 401 {
		t.Fatal("naturally expired bearer must return401", w.Code)
	}
	ctx := context.Background()
	l, e := r.Store.ClaimRegistered(ctx, "natural-session-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(ctx, l.TaskLease, "natural-session-worker"); e != nil {
		t.Fatal("original accepted task after natural Session expiry", e)
	}
	var valid bool
	if e = r.Store.DB.QueryRow("SELECT e.execute_by=? AND e.accepted_at=? AND e.state='COMMITTED' AND p.accepted_use_generation=1 AND p.accepted_consent_revision=1 AND p.issued_session_expires_at=? FROM harness_envelopes e JOIN integration_task_principals p USING(tenant_id,product_id,task_id) WHERE e.task_id=?", deadline, accepted, expires, a.TurnID).Scan(&valid); e != nil || !valid {
		t.Fatal("original tuple and deadline retained", e)
	}
	if time.Since(started) > 1100*time.Second {
		t.Fatal("original natural target budget exceeded")
	}
}

// The complete control chain uses real issuance and a Background worker;
// no Operator context or caller-supplied Scope can authorize the new port.
func TestIntegrationControlOwning(t *testing.T) {
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "CONTROL_PAUSE", "CONTROL_RESUME", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "s4-world", request)
	if e != nil {
		t.Fatal(e)
	}
	session, e := r.Store.ExchangeIntegrationSession(service, "s4-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "s4-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	port, ok := any(r.World).(interface {
		ControlIntegrationInstance(context.Context, string, string, generated.ControlRequest) (generated.TaskReceipt, error)
	})
	if !ok {
		t.Fatal("real Registry/publication/CREATE/Session/INITIAL succeeded: control owning port unavailable")
	}
	for _, step := range []struct {
		ctx                    context.Context
		action, version, state string
	}{{service, "START", "0", "RUNNING"}, {client, "PAUSE", "1", "PAUSED"}, {client, "RESUME", "2", "RUNNING"}, {service, "END", "3", "ENDED"}} {
		input := generated.ControlRequest{Command: step.action, ExpectedControlVersion: step.version}
		key := "s4-control-" + step.action
		out, e := port.ControlIntegrationInstance(step.ctx, w.Scope.WorldInstanceID, key, input)
		if e != nil {
			t.Fatal(step.action, "intake", e)
		}
		if out.TaskKind != "CONTROL" || out.TaskState != "ACCEPTED" || out.TaskID == "" {
			t.Fatal("control receipt", out)
		}
		var provenance string
		var noOperator bool
		var sidecars int
		if e = r.Store.DB.QueryRow("SELECT j.issuer_provenance,j.issuer_credential_id IS NULL,(SELECT COUNT(*) FROM integration_task_principals p WHERE p.task_id=j.task_id) FROM registered_jobs j WHERE j.task_id=?", out.TaskID).Scan(&provenance, &noOperator, &sidecars); e != nil || provenance != "DIRECT_INTEGRATION" || !noOperator || sidecars != 1 {
			t.Fatal("exact durable issuer", provenance, noOperator, sidecars, e)
		}
		lease, e := r.Store.ClaimRegistered(context.Background(), "s4-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: out.TaskID}})
		if e != nil {
			t.Fatal(e)
		}
		if e = r.World.Process(context.Background(), lease, "s4-worker"); e != nil {
			t.Fatal(step.action, "Background execution", e)
		}
		var state string
		if e = r.Store.DB.QueryRow("SELECT status FROM world_controls WHERE world_instance_id=?", w.Scope.WorldInstanceID).Scan(&state); e != nil || state != step.state {
			t.Fatal("world transition", state, e)
		}
		replay, e := port.ControlIntegrationInstance(step.ctx, w.Scope.WorldInstanceID, key, input)
		if e != nil || replay.TaskID != out.TaskID {
			t.Fatal("original key replay", e)
		}
	}
}

// Mutations are explicit owning-Tx fault injection, not invented registration
// APIs. A refused final write must leave neither a task nor its sidecar/COMMIT.
func TestIntegrationControlCurrentAndRollback(t *testing.T) {
	for _, mutation := range []string{"customer", "service-control", "issuer", "hash", "registration"} {
		t.Run(mutation, func(t *testing.T) {
			r, _, service, request, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "CONTROL_PAUSE", "CONTROL_RESUME"})
			w, e := r.World.CreateIntegrationInstance(service, "control-fault-world", request)
			if e != nil {
				t.Fatal(e)
			}
			in := generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"}
			var injected bool
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var result sql.Result
				var err error
				switch mutation {
				case "customer":
					result, err = tx.ExecContext(ctx, "UPDATE integration_customers SET enabled=0")
				case "service-control":
					result, err = tx.ExecContext(ctx, "UPDATE subject_governance g JOIN integration_customers c ON c.tenant_id=g.tenant_id AND c.product_id=g.product_id AND c.service_subject_id=g.subject_id SET g.can_control=0")
				case "issuer":
					result, err = tx.ExecContext(ctx, "UPDATE integration_task_principals SET principal_ref='foreign-service'")
				case "hash":
					result, err = tx.ExecContext(ctx, "UPDATE integration_task_principals SET intake_hash=UNHEX(REPEAT('00',32))")
				case "registration":
					result, err = tx.ExecContext(ctx, "UPDATE integration_task_principals SET registration_revisions=JSON_SET(registration_revisions,'$.Mapping',77)")
				}
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				injected = err == nil && n == 1
				if !injected {
					return store.ErrDenied
				}
				return nil
			}
			_, e = r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "control-fault", in)
			r.Store.BeforeCommit = nil
			if !injected {
				t.Fatal("fault did not reach exact owning row")
			}
			if e == nil {
				t.Fatal("changed final authority committed")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM integration_task_principals)+(SELECT COUNT(*) FROM commit_receipts WHERE operation='controlIntegrationInstance')+(SELECT COUNT(*) FROM registered_jobs WHERE issuer_provenance='DIRECT_INTEGRATION')").Scan(&n); e != nil || n != 0 {
				t.Fatal("partial intake", n, e)
			}
			if _, e = r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "control-fault", in); e != nil {
				t.Fatal("real restored positive", e)
			}
		})
	}
}

// Original-key metadata is independent of new positive generation permission.
// The original COMMIT, customer and player stay bound under a current read Tx.
func TestIntegrationOriginalIntakeMetadata(t *testing.T) {
	var client context.Context
	var token string
	r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
		token = s.AccessToken
		var e error
		client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+token, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
	})
	port, ok := any(r.Store).(interface {
		ReadIntegrationIntakeReceipt(context.Context, string, string) (generated.OriginalTurnIntakeReceipt, error)
	})
	if !ok {
		t.Fatal("real intake COMMIT established: original-key metadata owning port unavailable")
	}
	read, e := port.ReadIntegrationIntakeReceipt(client, a.Task.Scope.WorldInstanceID, "turn-intake")
	if e != nil || read.Acceptance.TurnID != a.TurnID || read.OriginalKey != "turn-intake" || read.OperationID != "submitTurn" {
		t.Fatal("original seven-field metadata", e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "metadata-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	after, e := port.ReadIntegrationIntakeReceipt(client, a.Task.Scope.WorldInstanceID, "turn-intake")
	if e != nil || after.Acceptance.TurnID != a.TurnID || after.IntakeReceiptRef != read.IntakeReceiptRef {
		t.Fatal("READ must survive positive-use revocation", e)
	}
	q := httptest.NewRequest("GET", "/world/v1/instances/"+a.Task.Scope.WorldInstanceID+"/turn-intake-receipt", nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	q.Header.Set("Idempotency-Key", "turn-intake")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, q)
	if w.Code != 200 || generated.Validate("OriginalTurnIntakeReceipt", w.Body.Bytes()) != nil {
		t.Fatal("original-key HTTP metadata", w.Code)
	}
	r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE integration_task_principals SET intake_hash=UNHEX(REPEAT('00',32)) WHERE task_id=?", a.TurnID)
		return e
	}
	_, e = port.ReadIntegrationIntakeReceipt(client, a.Task.Scope.WorldInstanceID, "turn-intake")
	r.Store.BeforeReadFinish = nil
	if e == nil {
		t.Fatal("changed final original COMMIT binding escaped")
	}
	if _, e = port.ReadIntegrationIntakeReceipt(client, a.Task.Scope.WorldInstanceID, "turn-intake"); e != nil {
		t.Fatal("rollback restored original proof", e)
	}
	if _, e = port.ReadIntegrationIntakeReceipt(context.Background(), a.Task.Scope.WorldInstanceID, "turn-intake"); e == nil {
		t.Fatal("unsealed caller accepted")
	}
}

func TestIntegrationTurnHTTPChain(t *testing.T) {
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
	world, e := r.World.CreateIntegrationInstance(service, "http-turn-world", request)
	if e != nil {
		t.Fatal(e)
	}
	call := func(method, path, token, key string, value any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(value)
		q := httptest.NewRequest(method, path, bytes.NewReader(b))
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		q.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		return w
	}
	serviceToken := "synthetic-s66-create-service-secret-32"
	sw := call("POST", "/world/v1/client-sessions", serviceToken, "http-turn-session", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
	var session generated.ClientSession
	if sw.Code != 201 || generated.Decode("ClientSession", sw.Body.Bytes(), &session) != nil {
		t.Fatal("real HTTP exchange", sw.Code)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	cw := call("POST", "/world/v1/self-consent/commands", session.AccessToken, "http-turn-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
	if cw.Code != 200 {
		t.Fatal("real HTTP INITIAL", cw.Code)
	}
	ctrl := call("POST", "/world/v1/instances/"+world.Scope.WorldInstanceID+"/controls", serviceToken, "http-turn-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	var task generated.TaskReceipt
	if ctrl.Code != 202 || generated.Decode("TaskReceipt", ctrl.Body.Bytes(), &task) != nil {
		t.Fatal("real HTTP control", ctrl.Code)
	}
	lease, e := r.Store.ClaimRegistered(context.Background(), "http-control-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: task.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.World.Process(context.Background(), lease, "http-control-worker"); e != nil {
		t.Fatal(e)
	}
	var actor string
	var wr, cv uint64
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),w.world_revision,w.control_version FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", world.Scope.WorldInstanceID).Scan(&actor, &wr, &cv); e != nil {
		t.Fatal(e)
	}
	req := generated.QuoteRequest{Scope: world.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}
	quoteResponse := call("POST", "/world/v1/quotes", serviceToken, "http-turn-quote", req)
	var quote generated.Quote
	if quoteResponse.Code != 201 || generated.Decode("Quote", quoteResponse.Body.Bytes(), &quote) != nil {
		t.Fatal("real SERVICE HTTP quote", quoteResponse.Code)
	}
	if denied := call("POST", "/world/v1/quotes", session.AccessToken, "http-wrong-kind", req); denied.Code != 401 {
		t.Fatal("CLIENT quote kind", denied.Code)
	}
	input := generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr, 10), ControlVersion: strconv.FormatUint(cv, 10)}, PrimaryActorID: actor, QuoteID: quote.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	path := "/world/v1/instances/" + world.Scope.WorldInstanceID + "/turns"
	accepted := call("POST", path, session.AccessToken, "http-turn-intake", input)
	var a generated.TurnAccepted
	if accepted.Code != 202 || generated.Decode("TurnAccepted", accepted.Body.Bytes(), &a) != nil {
		t.Fatal("real CLIENT HTTP intake", accepted.Code)
	}
	replay := call("POST", path, session.AccessToken, "http-turn-intake", input)
	if replay.Code != 202 || !bytes.Equal(replay.Body.Bytes(), accepted.Body.Bytes()) {
		t.Fatal("original HTTP replay", replay.Code)
	}
	if denied := call("POST", path, serviceToken, "http-wrong-service", input); denied.Code != 401 {
		t.Fatal("SERVICE cannot be player", denied.Code)
	}
	l, e := r.Store.ClaimRegistered(context.Background(), "http-turn-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "http-turn-worker"); e != nil {
		t.Fatal("HTTP accepted Background original COMMIT", e)
	}
}

// These two original registrations expire by DB UTC; no UPDATE or shortened
// Session/Task lifetime creates the boundary. Each positive chain runs once.
func TestIntegrationRegistrationNaturalExpiry(t *testing.T) {
	begin := time.Now()
	for _, customerShort := range []bool{true, false} {
		name := "mapping-entitlement"
		customerTTL := 2 * time.Hour
		if customerShort {
			name = "customer-key"
			customerTTL = 90 * time.Second
		}
		t.Run(name, func(t *testing.T) {
			r, operator, service, request, reg := integrationCreationFixtureActions(t, nil, 90*time.Second, []string{"READ", "TURN", "MEMORY_VIEW"}, customerTTL)
			original, e := r.Store.ReadIntegrationRegistryReceipt(operator, "s66-player")
			if e != nil {
				t.Fatal(e)
			}
			originalBytes, _ := json.Marshal(original)
			expires := reg.Player.ExpiresAt
			world, e := r.World.CreateIntegrationInstance(service, "natural-world", request)
			if e != nil {
				t.Fatal("PRECONDITION_FAIL create", e)
			}
			call := func(method, path, token, key string, value any) *httptest.ResponseRecorder {
				b, _ := json.Marshal(value)
				q := httptest.NewRequest(method, path, bytes.NewReader(b))
				q.Header.Set("Authorization", "Bearer "+token)
				q.Header.Set("Content-Type", "application/json")
				q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
				q.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				r.Handler().ServeHTTP(w, q)
				return w
			}
			const serviceToken = "synthetic-s66-create-service-secret-32"
			sw := call("POST", "/world/v1/client-sessions", serviceToken, "natural-session", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
			var session generated.ClientSession
			if sw.Code != 201 || generated.Decode("ClientSession", sw.Body.Bytes(), &session) != nil {
				t.Fatal("PRECONDITION_FAIL HTTP session", sw.Code)
			}
			retainSessionScanMaterial(t, session.AccessToken)
			if w := call("POST", "/world/v1/self-consent/commands", session.AccessToken, "natural-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); w.Code != 200 {
				t.Fatal("PRECONDITION_FAIL INITIAL", w.Code)
			}
			cw := call("POST", "/world/v1/instances/"+world.Scope.WorldInstanceID+"/controls", serviceToken, "natural-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
			var control generated.TaskReceipt
			if cw.Code != 202 || generated.Decode("TaskReceipt", cw.Body.Bytes(), &control) != nil {
				t.Fatal("PRECONDITION_FAIL control", cw.Code)
			}
			l, e := r.Store.ClaimRegistered(context.Background(), "natural-control-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: control.TaskID}})
			if e != nil {
				t.Fatal(e)
			}
			if e = r.World.Process(context.Background(), l, "natural-control-worker"); e != nil {
				t.Fatal("PRECONDITION_FAIL Background control", e)
			}
			var actor string
			var wr, cv uint64
			if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),w.world_revision,w.control_version FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", world.Scope.WorldInstanceID).Scan(&actor, &wr, &cv); e != nil {
				t.Fatal(e)
			}
			qreq := generated.QuoteRequest{Scope: world.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}
			qw := call("POST", "/world/v1/quotes", serviceToken, "natural-quote", qreq)
			var quote generated.Quote
			if qw.Code != 201 || generated.Decode("Quote", qw.Body.Bytes(), &quote) != nil {
				t.Fatal("PRECONDITION_FAIL SERVICE quote", qw.Code)
			}
			input := generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr, 10), ControlVersion: strconv.FormatUint(cv, 10)}, PrimaryActorID: actor, QuoteID: quote.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
			aw := call("POST", "/world/v1/instances/"+world.Scope.WorldInstanceID+"/turns", session.AccessToken, "natural-intake", input)
			var a generated.TurnAccepted
			if aw.Code != 202 || generated.Decode("TurnAccepted", aw.Body.Bytes(), &a) != nil {
				t.Fatal("PRECONDITION_FAIL HTTP intake", aw.Code)
			}
			lease, e := r.Store.ClaimRegistered(context.Background(), "natural-send-worker", time.Now().UTC(), 3*time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			d, e := r.Turns.PrepareOriginalTurn(context.Background(), lease.TaskLease, "natural-send-worker")
			if e != nil {
				t.Fatal("PRECONDITION_FAIL Background Prepare", e)
			}
			if _, e = r.Store.AuthorizeDispatch(context.Background(), d); e != nil {
				t.Fatal("PRECONDITION_FAIL dispatch", e)
			}
			d.CommandKey = content.Identity(a.TurnID, "send")
			d.CommitKey = d.CommandKey
			mark, e := r.Store.StartSend(context.Background(), d, d.AttemptID)
			if e != nil || !mark.CanSend {
				t.Fatal("PRECONDITION_FAIL positive send", e)
			}
			var now time.Time
			if e = r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
				t.Fatal(e)
			}
			remaining := expires.Sub(now)
			if remaining < 30*time.Second || remaining > 90*time.Second {
				t.Fatal("PRECONDITION_FAIL positive registration window", remaining)
			}
			t.Logf("actual HTTP and Background send positive with %.3fs registration remaining", remaining.Seconds())
			time.Sleep(time.Until(expires.Add(250 * time.Millisecond)))
			if w := call("GET", "/world/v1/self-consent", session.AccessToken, "", nil); w.Code != 401 {
				t.Fatal("expired registration CLIENT HTTP", w.Code)
			}
			if _, e = r.Turns.PrepareOriginalTurn(context.Background(), lease.TaskLease, "natural-send-worker"); e == nil {
				t.Fatal("expired registration new execute")
			}
			if m, e := r.Store.StartSend(context.Background(), d, d.AttemptID); e == nil || m.CanSend {
				t.Fatal("expired registration new send", e)
			}
			var cause, customerLive, deadlineLive, positive bool
			if e = r.Store.DB.QueryRow("SELECT c.expires_at>UTC_TIMESTAMP(6),m.expires_at<=UTC_TIMESTAMP(6) AND e.expires_at<=UTC_TIMESTAMP(6),h.execute_by>UTC_TIMESTAMP(6) AND t.lease_expires_at>UTC_TIMESTAMP(6) AND t.cancelled=0,p.enabled AND p.expires_at>UTC_TIMESTAMP(6) FROM integration_customers c JOIN integration_external_subjects m USING(tenant_id,product_id,customer_id) JOIN integration_entitlements e USING(tenant_id,product_id,customer_id) JOIN harness_envelopes h ON h.task_id=? JOIN tasks t ON t.task_id=h.task_id JOIN memory_subject_permissions p ON p.data_subject_id=m.player_subject_id AND p.action='memory.generation.use' WHERE c.customer_id='s66-customer'", a.TurnID).Scan(&customerLive, &cause, &deadlineLive, &positive); e != nil {
				t.Fatal("expiry cause proof", e)
			}
			if customerLive == customerShort || !cause || !deadlineLive || !positive {
				t.Fatal("expiry inner cause must not be task/use/lease/cancel", customerLive, cause, deadlineLive, positive)
			}
			after, e := r.Store.ReadIntegrationRegistryReceipt(operator, "s66-player")
			if e != nil {
				t.Fatal("original historical expiry read", e)
			}
			afterBytes, _ := json.Marshal(after)
			if !bytes.Equal(originalBytes, afterBytes) {
				t.Fatal("original registration receipt changed")
			}
			var calls int
			if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&calls); e != nil || calls != 1 {
				t.Fatal("expired send changed calls", calls, e)
			}
		})
	}
	if time.Since(begin) > 300*time.Second {
		t.Fatal("combined natural registration target exceeded300s")
	}
}

// Task reads and cancellation retain the original task and real accounting.
func TestIntegrationTaskMetadataAndCancel(t *testing.T) {
	for _, phase := range []string{"accepted", "sent", "committed"} {
		t.Run(phase, func(t *testing.T) {
			var client context.Context
			var token string
			r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
				token = s.AccessToken
				var e error
				client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+token, domain.IntegrationClient)
				if e != nil {
					t.Fatal(e)
				}
			})
			port, ok := any(r.Store).(interface {
				ReadIntegrationTask(context.Context, string) (generated.TaskView, error)
				CancelIntegrationTask(context.Context, string, string, generated.CancelRequest) (generated.CancelReceipt, error)
			})
			if !ok {
				t.Fatal("real accepted task: metadata/cancel owning ports unavailable")
			}
			var sentAttempt string
			if phase != "accepted" {
				l, e := r.Store.ClaimRegistered(context.Background(), "task-read-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
				if e != nil {
					t.Fatal(e)
				}
				if phase == "committed" {
					if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "task-read-worker"); e != nil {
						t.Fatal(e)
					}
				} else {
					d, e := r.Turns.PrepareOriginalTurn(context.Background(), l.TaskLease, "task-read-worker")
					if e != nil {
						t.Fatal(e)
					}
					if _, e = r.Store.AuthorizeDispatch(context.Background(), d); e != nil {
						t.Fatal(e)
					}
					d.CommandKey = content.Identity(a.TurnID, "send")
					d.CommitKey = d.CommandKey
					if m, e := r.Store.StartSend(context.Background(), d, d.AttemptID); e != nil || !m.CanSend {
						t.Fatal(e)
					}
					sentAttempt = d.AttemptID
				}
			}
			checkOriginalUnknown := func() {
				t.Helper()
				if phase != "sent" {
					return
				}
				var count, sent int
				var attempt, provider string
				e := r.Store.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(a.send_started_at IS NOT NULL),0),COALESCE(MIN(q.attempt_id),''),COALESCE(MIN(q.provider_state),'') FROM harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE q.tenant_id=? AND q.product_id=? AND q.task_id=?", a.Task.Scope.TenantID, a.Task.Scope.ProductID, a.TurnID).Scan(&count, &sent, &attempt, &provider)
				if e != nil || count != 1 || sent != 1 || attempt != sentAttempt || provider != "UNKNOWN" {
					t.Fatal("original sent attempt and unknown query must remain", e)
				}
			}
			checkOriginalUnknown()
			view, e := port.ReadIntegrationTask(client, a.TurnID)
			if e != nil || view.Task.TaskID != a.TurnID {
				t.Fatal("current task metadata", e)
			}
			if phase == "committed" {
				if view.Task.TaskState != "COMMITTED" || view.Billing.ServiceSettlementState != "SETTLED" {
					t.Fatal("real committed projection", view.Task.TaskState, view.Billing.ServiceSettlementState)
				}
			}
			reason := "player-request"
			req := generated.CancelRequest{Reason: &reason}
			out, e := port.CancelIntegrationTask(client, a.TurnID, "task-cancel", req)
			if phase == "committed" {
				if e == nil {
					t.Fatal("committed cancellation accepted")
				}
				return
			}
			if e != nil {
				t.Fatal("own cancel", e)
			}
			want := "NOT_SENT"
			if phase == "sent" {
				want = "UNKNOWN"
				if out.LocalCancelState != "REQUESTED" {
					t.Fatal("sent cancellation remains locally requested")
				}
				checkOriginalUnknown()
			}
			if out.Task.TaskState != "CANCELLED" || out.ExternalCancelState != want || out.Billing.ServiceSettlementState != "RESERVED" {
				t.Fatal("actual local/external/accounting split", out, e)
			}
			replay, e := port.CancelIntegrationTask(client, a.TurnID, "task-cancel", req)
			b, _ := json.Marshal(out)
			rb, _ := json.Marshal(replay)
			if e != nil || !bytes.Equal(b, rb) {
				t.Fatal("cancel original replay", e)
			}
			different := "different"
			if _, e = port.CancelIntegrationTask(client, a.TurnID, "task-cancel", generated.CancelRequest{Reason: &different}); e == nil {
				t.Fatal("cancel key payload conflict")
			}
			if _, e = r.Store.ApplyPlayerSelfConsent(client, "cancel-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
				t.Fatal(e)
			}
			if _, e = port.ReadIntegrationTask(client, a.TurnID); e != nil {
				t.Fatal("task metadata needs READ, not generation", e)
			}
			r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, "UPDATE integration_task_principals SET customer_id='foreign-customer' WHERE task_id=?", a.TurnID)
				return e
			}
			if _, e = port.ReadIntegrationTask(client, a.TurnID); e == nil {
				t.Fatal("final metadata identity replaced")
			}
			r.Store.BeforeReadFinish = nil
			q := httptest.NewRequest("GET", "/world/v1/tasks/"+a.TurnID, nil)
			q.Header.Set("Authorization", "Bearer "+token)
			q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			if w.Code != 200 || generated.Validate("TaskView", w.Body.Bytes()) != nil {
				t.Fatal("HTTP task projection", w.Code)
			}
		})
	}
}

func TestIntegrationApprovedResultCurrentRead(t *testing.T) {
	var client context.Context
	r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
		var e error
		client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+s.AccessToken, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
	})
	l, e := r.Store.ClaimRegistered(context.Background(), "approved-read-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "approved-read-worker"); e != nil {
		t.Fatal(e)
	}
	scope := domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}
	body, proof, e := r.Files.ReadApprovedTurn(client, scope, a.TurnID)
	if e != nil || len(body) == 0 || proof.Metadata.Identity.TaskID != a.TurnID {
		t.Fatal("real committed CLIENT result owning read", e)
	}
	if _, _, e = r.Files.ReadApprovedTurn(context.Background(), scope, a.TurnID); e == nil {
		t.Fatal("unsealed Scope gained result body")
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(client, "approved-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if _, _, e = r.Files.ReadApprovedTurn(client, scope, a.TurnID); e == nil {
		t.Fatal("revoked result body returned")
	}
}

func TestIntegrationAcceptedTupleAndFinalRollback(t *testing.T) {
	for _, mode := range []string{"active-renew", "revoke-renew", "registration-sidecar", "cancel-final", "result-final"} {
		t.Run(mode, func(t *testing.T) {
			var client context.Context
			r, a := integrationAcceptedTurn(t, func(r *app.Runtime, s generated.ClientSession) {
				var e error
				client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+s.AccessToken, domain.IntegrationClient)
				if e != nil {
					t.Fatal(e)
				}
			})
			if mode == "cancel-final" {
				injected := false
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					res, e := tx.ExecContext(ctx, "UPDATE tasks SET cancelled=0,state='ACCEPTED' WHERE task_id=?", a.TurnID)
					if e != nil {
						return e
					}
					n, e := res.RowsAffected()
					injected = e == nil && n == 1
					return e
				}
				_, e := r.Store.CancelIntegrationTask(client, a.TurnID, "cancel-final", generated.CancelRequest{})
				r.Store.BeforeCommit = nil
				if !injected {
					t.Fatal("legal cancellation fault not reached", e)
				}
				if e == nil {
					t.Fatal("cancel final tuple changed but COMMIT returned")
				}
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM admin_results WHERE schema_name='CancelReceipt'").Scan(&n); e != nil || n != 0 {
					t.Fatal("cancel receipt partial", n, e)
				}
				if _, e = r.Store.CancelIntegrationTask(client, a.TurnID, "cancel-final", generated.CancelRequest{}); e != nil {
					t.Fatal("rollback positive", e)
				}
				return
			}
			if mode == "active-renew" || mode == "revoke-renew" {
				revision := "1"
				if mode == "revoke-renew" {
					if _, e := r.Store.ApplyPlayerSelfConsent(client, "tuple-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
						t.Fatal(e)
					}
					revision = "2"
				}
				body, _ := json.Marshal(map[string]string{"command": "RENEW", "expected_revision": revision, "policy_id": "R0_PLAYER_PRIVATE_GENERATION_30D"})
				if _, e := r.Store.ApplyPlayerSelfConsent(client, "tuple-renew", body); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "registration-sidecar" {
				res, e := r.Store.DB.Exec("UPDATE integration_task_principals SET registration_revisions=JSON_SET(registration_revisions,'$.Mapping',77) WHERE task_id=?", a.TurnID)
				if e != nil {
					t.Fatal(e)
				}
				if n, e := res.RowsAffected(); e != nil || n != 1 {
					t.Fatal("legal exact fault", n, e)
				}
			}
			lease, e := r.Store.ClaimRegistered(context.Background(), "tuple-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			if mode == "revoke-renew" || mode == "registration-sidecar" {
				if _, e = r.Turns.PrepareOriginalTurn(context.Background(), lease.TaskLease, "tuple-worker"); e == nil {
					t.Fatal("original accepted tuple escaped", mode)
				}
				return
			}
			if e = r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "tuple-worker"); e != nil {
				t.Fatal("lawful original execution", e)
			}
			if mode == "result-final" {
				injected := false
				r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					res, e := tx.ExecContext(ctx, "UPDATE memory_subject_permissions SET enabled=0 WHERE data_subject_id='s66-player' AND action='memory.private_body.read'")
					if e != nil {
						return e
					}
					n, e := res.RowsAffected()
					injected = e == nil && n == 1
					return e
				}
				scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
				_, _, e = r.Files.ReadApprovedTurn(client, scope, a.TurnID)
				r.Store.BeforeReadFinish = nil
				if !injected || e == nil {
					t.Fatal("final result permission escaped", injected, e)
				}
				if _, _, e = r.Files.ReadApprovedTurn(client, scope, a.TurnID); e != nil {
					t.Fatal("read rollback restored", e)
				}
			}
		})
	}
}

// These fixtures use real registration, publication, CREATE and Session issuers.
// Fault injection changes only the owned transaction and must reach exactly one row.
type integrationFixFixture struct {
	r                       *app.Runtime
	service, client, second context.Context
	world                   generated.WorldScope
	actor, secondID         string
	secondExpiry            time.Time
	input                   generated.TurnRequest
}

func integrationFixSetup(t *testing.T, start bool) integrationFixFixture {
	t.Helper()
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "CONTROL_PAUSE", "CONTROL_RESUME", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "fix-world", request)
	if e != nil {
		t.Fatal(e)
	}
	f := integrationFixFixture{r: r, service: service, world: w.Scope}
	for i, key := range []string{"fix-session", "fix-second"} {
		ss, e := r.Store.ExchangeIntegrationSession(service, key, "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, ss.AccessToken)
		ctx, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+ss.AccessToken, domain.IntegrationClient)
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			f.client = ctx
		} else {
			f.second = ctx
			f.secondExpiry, e = time.Parse(time.RFC3339Nano, ss.ExpiresAt)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(result_metadata,'$.ID')) FROM integration_receipts WHERE operation='EXCHANGE_SESSION' AND original_key='fix-second'").Scan(&f.secondID); e != nil || f.secondID == "" {
		t.Fatal("actual second issuance", e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(f.client, "fix-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	if start {
		f.control(t, service, "START", "0", "fix-start")
	}
	var wr, cv uint64
	if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),w.world_revision,w.control_version FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", w.Scope.WorldInstanceID).Scan(&f.actor, &wr, &cv); e != nil {
		t.Fatal(e)
	}
	f.input = generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr, 10), ControlVersion: strconv.FormatUint(cv, 10)}, PrimaryActorID: f.actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	return f
}
func (f integrationFixFixture) control(t *testing.T, ctx context.Context, action, version, key string) generated.TaskReceipt {
	t.Helper()
	out, e := f.r.World.ControlIntegrationInstance(ctx, f.world.WorldInstanceID, key, generated.ControlRequest{Command: action, ExpectedControlVersion: version})
	if e != nil {
		t.Fatal("lawful control", action, e)
	}
	lease, e := f.r.Store.ClaimRegistered(context.Background(), "fix-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: out.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.r.World.Process(context.Background(), lease, "fix-worker"); e != nil {
		t.Fatal(e)
	}
	return out
}
func (f integrationFixFixture) quote(ctx context.Context, key string) (generated.Quote, error) {
	return f.r.Store.QuoteIntegrationTurn(ctx, key, generated.QuoteRequest{Scope: f.world, ServiceKind: "TURN_TEXT", PrimaryActorID: &f.actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"})
}
func integrationFixRows(t *testing.T, result sql.Result, e error) {
	t.Helper()
	if e != nil {
		t.Fatal("fault injection SQL prerequisite", e)
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		t.Fatal("fault injection exact row", n, e)
	}
}
func integrationFixChoiceFault(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, mode, key string) (sql.Result, error) {
	if mode == "principal" {
		return q.ExecContext(ctx, "UPDATE integration_self_consent_facts SET principal_ref='missing-original-session' WHERE original_key=?", key)
	}
	return q.ExecContext(ctx, "UPDATE integration_receipts SET result_metadata=JSON_SET(result_metadata,'$.snapshot.consent_revision','999') WHERE operation='PLAYER_SELF_CONSENT' AND original_key=?", key)
}
func TestIntegrationQuoteCurrentConsentIssuance(t *testing.T) {
	for _, caller := range []string{"SERVICE"} {
		for _, mode := range []string{"principal", "receipt"} {
			for _, phase := range []string{"before", "final"} {
				t.Run(caller+"-"+mode+"-"+phase, func(t *testing.T) {
					f := integrationFixSetup(t, true)
					ctx := f.service
					if caller == "CLIENT" {
						ctx = f.client
					}
					if _, e := f.quote(ctx, "quote-positive"); e != nil {
						t.Fatal("lawful QUOTE", e)
					}
					injected := false
					if phase == "before" {
						res, e := integrationFixChoiceFault(context.Background(), f.r.Store.DB, mode, "fix-initial")
						integrationFixRows(t, res, e)
						injected = true
					} else {
						f.r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
							res, e := integrationFixChoiceFault(c, tx, mode, "fix-initial")
							if e != nil {
								return e
							}
							n, e := res.RowsAffected()
							injected = e == nil && n == 1
							return e
						}
					}
					_, e := f.quote(ctx, "quote-fault")
					f.r.Store.BeforeCommit = nil
					if !injected {
						t.Fatal("legal fault not reached", e)
					}
					if e == nil {
						t.Fatal("current choice issuance escaped QUOTE", mode, phase)
					}
					var n int
					if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='TURN_QUOTE' AND command_key='quote-fault'").Scan(&n); e != nil || n != 0 {
						t.Fatal("quote partial commit", n, e)
					}
					if phase == "final" {
						if _, e = f.quote(ctx, "quote-fault"); e != nil {
							t.Fatal("quote rollback positive", e)
						}
					}
				})
			}
		}
	}
}
func TestIntegrationApprovedResultCurrentConsentIssuance(t *testing.T) {
	for _, mode := range []string{"principal", "receipt"} {
		for _, phase := range []string{"before", "final"} {
			t.Run(mode+"-"+phase, func(t *testing.T) {
				f := integrationFixSetup(t, true)
				q, e := f.quote(f.service, "read-quote")
				if e != nil {
					t.Fatal(e)
				}
				in := f.input
				in.QuoteID = q.QuoteID
				out, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "read-intake", in)
				if e != nil {
					t.Fatal(e)
				}
				l, e := f.r.Store.ClaimRegistered(context.Background(), "fix-result", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: out.TurnID}})
				if e != nil {
					t.Fatal(e)
				}
				if e = f.r.Worker.Turns.Execute(context.Background(), l.TaskLease, "fix-result"); e != nil {
					t.Fatal(e)
				}
				if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "fix-renew", json.RawMessage(`{"command":"RENEW","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
					t.Fatal(e)
				}
				scope := domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: f.world.WorldInstanceID}
				oldFact := []byte{}
				if e = f.r.Store.DB.QueryRow("SELECT CONCAT(HEX(payload_hash),principal_ref,receipt_ref,CAST(expires_at AS CHAR)) FROM integration_self_consent_facts WHERE revision=1").Scan(&oldFact); e != nil {
					t.Fatal(e)
				}
				body, _, e := f.r.Files.ReadApprovedTurn(f.client, scope, out.TurnID)
				if e != nil || len(body) == 0 {
					t.Fatal("current renewal body positive", e)
				}
				injected := false
				if phase == "before" {
					res, e := integrationFixChoiceFault(context.Background(), f.r.Store.DB, mode, "fix-renew")
					integrationFixRows(t, res, e)
					injected = true
				} else {
					f.r.Store.BeforeReadFinish = func(c context.Context, tx *sql.Tx) error {
						res, e := integrationFixChoiceFault(c, tx, mode, "fix-renew")
						if e != nil {
							return e
						}
						n, e := res.RowsAffected()
						injected = e == nil && n == 1
						return e
					}
				}
				body, _, e = f.r.Files.ReadApprovedTurn(f.client, scope, out.TurnID)
				f.r.Store.BeforeReadFinish = nil
				if !injected {
					t.Fatal("legal current-choice fault not reached", e)
				}
				if e == nil || len(body) != 0 {
					t.Fatal("new current choice escaped body gate", mode, phase)
				}
				var after []byte
				if e = f.r.Store.DB.QueryRow("SELECT CONCAT(HEX(payload_hash),principal_ref,receipt_ref,CAST(expires_at AS CHAR)) FROM integration_self_consent_facts WHERE revision=1").Scan(&after); e != nil || !bytes.Equal(after, oldFact) {
					t.Fatal("historical accepted fact changed", e)
				}
				if phase == "final" {
					body, _, e = f.r.Files.ReadApprovedTurn(f.client, scope, out.TurnID)
					if e != nil || len(body) == 0 {
						t.Fatal("body rollback positive", e)
					}
				}
			})
		}
	}
}
func TestIntegrationFreshIntakeTupleFinal(t *testing.T) {
	for _, mode := range []string{"older-consent", "other-session"} {
		t.Run(mode, func(t *testing.T) {
			f := integrationFixSetup(t, true)
			if _, e := f.r.Store.ApplyPlayerSelfConsent(f.client, "fix-renew", json.RawMessage(`{"command":"RENEW","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
				t.Fatal(e)
			}
			q, e := f.quote(f.service, "fresh-quote")
			if e != nil {
				t.Fatal(e)
			}
			in := f.input
			in.QuoteID = q.QuoteID
			injected := false
			f.r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
				var res sql.Result
				var e error
				if mode == "older-consent" {
					res, e = tx.ExecContext(c, "UPDATE integration_task_principals i JOIN integration_self_consent_facts f ON f.player_subject_id=i.player_subject_id AND f.revision=1 SET i.accepted_consent_revision=f.revision,i.accepted_use_expires_at=f.expires_at WHERE i.task_kind='TURN'")
				} else {
					res, e = tx.ExecContext(c, "UPDATE integration_task_principals SET principal_ref=?,issued_session_expires_at=? WHERE task_kind='TURN'", f.secondID, f.secondExpiry)
				}
				if e != nil {
					return e
				}
				n, e := res.RowsAffected()
				injected = e == nil && n == 1
				return e
			}
			_, e = f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "fresh-intake", in)
			f.r.Store.BeforeCommit = nil
			if !injected {
				t.Fatal("legal first-writer mutation not reached", e)
			}
			if e == nil {
				t.Fatal("fresh intake accepted alternate historical tuple", mode)
			}
			var n int
			if e = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks WHERE task_kind='TURN')+(SELECT COUNT(*) FROM integration_task_principals WHERE task_kind='TURN')+(SELECT COUNT(*) FROM harness_turns)+(SELECT COUNT(*) FROM harness_envelopes)+(SELECT COUNT(*) FROM reservations)+(SELECT COUNT(*) FROM commit_receipts WHERE operation='HARNESS_INTAKE')").Scan(&n); e != nil || n != 0 {
				t.Fatal("fresh intake partial rows", n, e)
			}
			out, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "fresh-intake", in)
			if e != nil {
				t.Fatal("restored fresh positive", e)
			}
			replay, e := f.r.Store.AcceptIntegrationTurn(f.second, f.world.WorldInstanceID, "fresh-intake", in)
			if e != nil || replay.TurnID != out.TurnID {
				t.Fatal("lawful newer Session replay", e)
			}
			var revision uint64
			var ref string
			if e = f.r.Store.DB.QueryRow("SELECT principal_ref,accepted_consent_revision FROM integration_task_principals WHERE task_id=?", out.TurnID).Scan(&ref, &revision); e != nil || ref == f.secondID || revision != 2 {
				t.Fatal("replay changed original tuple", e)
			}
		})
	}
}
func TestIntegrationFreshControlTupleFinal(t *testing.T) {
	for _, action := range []string{"START", "PAUSE", "RESUME", "END"} {
		t.Run(action, func(t *testing.T) {
			f := integrationFixSetup(t, action != "START")
			ctx, version := f.service, "0"
			if action != "START" {
				version = "1"
			}
			if action == "PAUSE" {
				ctx = f.client
			}
			if action == "RESUME" {
				f.control(t, f.client, "PAUSE", "1", "before-pause")
				ctx = f.client
				version = "2"
			}
			input := generated.ControlRequest{Command: action, ExpectedControlVersion: version}
			injected := false
			var before int
			if e := f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks)+(SELECT COUNT(*) FROM integration_task_principals)+(SELECT COUNT(*) FROM runtime_tasks)+(SELECT COUNT(*) FROM commit_receipts)").Scan(&before); e != nil {
				t.Fatal(e)
			}
			f.r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
				var res sql.Result
				var e error
				if action == "PAUSE" || action == "RESUME" {
					res, e = tx.ExecContext(c, "UPDATE integration_task_principals SET principal_ref=?,issued_session_expires_at=? WHERE task_kind='CONTROL' AND action=?", f.secondID, f.secondExpiry, action)
				} else {
					res, e = tx.ExecContext(c, "UPDATE integration_task_principals SET issued_session_expires_at=DATE_ADD(issued_session_expires_at,INTERVAL 1 SECOND) WHERE task_kind='CONTROL' AND action=?", action)
				}
				if e != nil {
					return e
				}
				n, e := res.RowsAffected()
				injected = e == nil && n == 1
				return e
			}
			_, e := f.r.World.ControlIntegrationInstance(ctx, f.world.WorldInstanceID, "fresh-control", input)
			f.r.Store.BeforeCommit = nil
			if !injected {
				t.Fatal("legal control fault not reached", e)
			}
			if e == nil {
				t.Fatal("fresh control accepted alternate issued tuple", action)
			}
			var after int
			if e = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks)+(SELECT COUNT(*) FROM integration_task_principals)+(SELECT COUNT(*) FROM runtime_tasks)+(SELECT COUNT(*) FROM commit_receipts)").Scan(&after); e != nil || after != before {
				t.Fatal("control partial commit", before, after, e)
			}
			out, e := f.r.World.ControlIntegrationInstance(ctx, f.world.WorldInstanceID, "fresh-control", input)
			if e != nil {
				t.Fatal("restored control positive", e)
			}
			replayCtx := ctx
			if action == "PAUSE" || action == "RESUME" {
				replayCtx = f.second
			}
			replay, e := f.r.World.ControlIntegrationInstance(replayCtx, f.world.WorldInstanceID, "fresh-control", input)
			if e != nil || replay.TaskID != out.TaskID {
				t.Fatal("historical control replay", e)
			}
		})
	}
}
