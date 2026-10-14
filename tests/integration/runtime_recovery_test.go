package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func TestMigrationRejectsCommittedTaskWithUnclosedSentAttempt(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := context.Background()
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	br, q := actionTask(t, r, s, i, "attempt-turn")
	// Synthetic zero-cost budget fixture only; authorization and SENT marker use
	// production ports. No provider is called and no money is spent.
	if _, e := r.Store.DB.Exec("INSERT INTO budget_accounts(kind,scope_key,account_id,balance) VALUES('supplier_global','GLOBAL','synthetic-zero',0)"); e != nil {
		t.Fatal(e)
	}
	d := store.DispatchRequest{BarrierRequest: br, AttemptID: "original-attempt", AuthorizationID: "original-auth", Route: "mock-local", RequestKey: "original-request", RequestHash: sha256.Sum256([]byte("synthetic request")), Reservations: []store.Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "synthetic-zero", ReservationID: "zero-reservation", Amount: 0}}}
	d.Operation = "generate"
	d.CommandKey = "original-generate"
	d.CommitKey = "original-dispatch"
	d.Locks = nil
	if _, e := r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommitKey = "original-send"
	if _, e := r.Store.StartSend(ctx, d, d.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e := commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &i)
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, rel, "expression_style", "short")
	// A legacy Task4 COMMITTED enum plus SENT attempt is deliberately not a
	// registered, fully closed Task6 TURN. AssertWorldAccounting rejects its
	// missing original ownership/hold proof before the old generic 422 gate.
	var missing int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM registered_jobs WHERE task_id=?)+(SELECT COUNT(*) FROM harness_envelopes WHERE task_id=?)", br.TaskID, br.TaskID).Scan(&missing); e != nil || missing != 0 {
		t.Fatal("fixture unexpectedly became a managed TURN", missing, e)
	}
	snapshot := func() []byte {
		t.Helper()
		state := map[string][]string{}
		for _, table := range []string{"tasks", "attempts", "reservations", "commit_receipts", "events", "approved_results", "postings", "outbox", "world_controls", "actors", "items", "runtime_instances", "harness_world_seals"} {
			rows, e := r.Store.DB.Query("SELECT * FROM " + table)
			if e != nil {
				t.Fatal(e)
			}
			columns, e := rows.Columns()
			if e != nil {
				rows.Close()
				t.Fatal(e)
			}
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for n := range values {
					pointers[n] = &values[n]
				}
				if e = rows.Scan(pointers...); e != nil {
					rows.Close()
					t.Fatal(e)
				}
				b, e := json.Marshal(values)
				if e != nil {
					rows.Close()
					t.Fatal(e)
				}
				state[table] = append(state[table], string(b))
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				t.Fatal(e)
			}
			sort.Strings(state[table])
		}
		b, e := json.Marshal(state)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	before, originalJ := snapshot(), journalBytes(t, r)
	request := generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	if _, e := r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), scope, request, "preview"); !errors.Is(e, store.ErrDenied) {
		t.Fatal("incomplete original accounting proof must be denied", e)
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", request, 403, nil)
	if !bytes.Equal(before, snapshot()) || !bytes.Equal(originalJ, journalBytes(t, r)) {
		t.Fatal("denied migration changed original state, facts, holds, receipts or journal")
	}
	var state string
	if e := r.Store.DB.QueryRow("SELECT state FROM attempts WHERE attempt_id='original-attempt'").Scan(&state); e != nil || state != "SENT" {
		t.Fatal("migration erased pending history", state, e)
	}
}

func TestRuntimeWorkerCrashRollsBackAndRecovers(t *testing.T) {
	if os.Getenv("LOREVIVA_RUNTIME_CRASH_HELPER") == "1" {
		s := store.New(dbfixture.ReopenHelper(t))
		lease, e := s.ClaimRegistered(context.Background(), "crash-worker", time.Now(), time.Second, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL"}})
		if e != nil {
			t.Fatal(e)
		}
		s.BeforeCommit = func(context.Context, *sql.Tx) error { os.Exit(44); return nil }
		if e = world.New(s).Process(context.Background(), lease, "crash-worker"); e != nil {
			t.Fatal(e)
		}
		t.Fatal("crash hook was not reached")
	}
	r, s, rel := runtimeFixture(t)
	ctx := context.Background()
	i := createRuntimeWorld(t, r, rel, "create")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	_, e := r.World.Control(runtimeContextWithParent(t, r, ctx), scope, world.ControlCommand{Key: "start", Request: generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: i.Versions}})
	if e != nil {
		t.Fatal(e)
	}
	var database string
	if e = r.Store.DB.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeWorkerCrashRollsBackAndRecovers$")
	cmd.Env = append(os.Environ(), "LOREVIVA_RUNTIME_CRASH_HELPER=1", "LOREVIVA_HELPER_DATABASE="+database)
	b, e := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 44 {
		t.Fatalf("crash child did not reach transactional hook: %v %s", e, b)
	}
	time.Sleep(1100 * time.Millisecond)
	after, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil || after.InstanceState != "CREATED" {
		t.Fatal("crashed writes survived", after, e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	after, e = r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil || after.Versions.ControlVersion != "1" {
		t.Fatal("restart duplicated/lost control", after, e)
	}
}

func TestMigrationPreventsResumeAndUnknownUsesOriginalReceipt(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	s := scope.ProductScope
	turnLease, turnWorker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, turnLease, turnWorker); e != nil {
		t.Fatal(e)
	}
	joint, e := r.Store.CommitOriginalTurn(ctx, turnLease, turnWorker)
	if e != nil || !joint.Committed || joint.TaskID != task || joint.OwnerSubjectID != s.SubjectID {
		t.Fatal("managed original joint commit missing", joint, e)
	}
	var operation, taskState, envelope string
	var holds, attempts, validations int
	if e = r.Store.DB.QueryRow("SELECT j.operation,t.state,h.state,(SELECT COUNT(*) FROM reservations WHERE task_id=?),(SELECT COUNT(*) FROM attempts WHERE task_id=?),(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?) FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task).Scan(&operation, &taskState, &envelope, &holds, &attempts, &validations); e != nil || operation != "TURN" || taskState != "COMMITTED" || envelope != "COMMITTED" || holds != 6 || attempts != 1 || validations != 1 {
		t.Fatal("fixture lacks managed original closure", operation, taskState, envelope, holds, attempts, validations, e)
	}
	i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil {
		t.Fatal(e)
	}
	i = runtimeControl(t, r, i, "PAUSE")
	versionsBefore := runtimeVersions(t, i)
	snapshot := func() []byte {
		t.Helper()
		out := map[string][]string{}
		for _, spec := range []struct{ table, key string }{{"actors", "actor_id"}, {"items", "item_id"}, {"memory_aggregates", "aggregate_id"}} {
			rows, e := r.Store.DB.Query("SELECT document FROM "+spec.table+" WHERE world_instance_id=? ORDER BY "+spec.key, scope.WorldInstanceID)
			if e != nil {
				t.Fatal(e)
			}
			for rows.Next() {
				var raw string
				if e = rows.Scan(&raw); e != nil {
					rows.Close()
					t.Fatal(e)
				}
				out[spec.table] = append(out[spec.table], raw)
			}
			e = rows.Err()
			rows.Close()
			if e != nil || len(out[spec.table]) == 0 {
				t.Fatal("missing actual history", spec.table, e)
			}
		}
		b, e := json.Marshal(out)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	documentsBefore, journalBefore := snapshot(), journalBytes(t, r)
	target := nextRuntimeRelease(t, r, s, i.Versions.ReleaseID, "expression_style", "quiet and precise")
	if _, e = r.Auth.Authorize(ctx, "Bearer "+runtimeToken, s.ProductID, "runtime.migrate"); e != nil {
		t.Fatal("fixture lacks migration permission", e)
	}
	var p generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &p)
	hash, e := content.Hash(p)
	if e != nil {
		t.Fatal(e)
	}
	req := generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: p.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: p.ResolutionMap}
	var receipt generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+scope.WorldInstanceID+"/migrations", "migrate", req, 202, &receipt)
	executeKey := content.Identity(s.TenantID, s.ProductID, s.SubjectID, "executeInstanceMigration", "migrate")
	executeReceipt, e := r.Store.QueryCommitScoped(ctx, s, &scope, s.SubjectID, executeKey)
	if e != nil || !executeReceipt.Committed || executeReceipt.Key != executeKey {
		t.Fatal("original migration intake receipt absent", executeReceipt, e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "migrator", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "MIGRATION", Operation: "INSTANCE_MIGRATION"}})
	if e != nil || lease.TaskID != string(receipt.TaskID) {
		t.Fatal("wrong original migration leased", e)
	}
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic final rollback") }
	e = r.World.Process(ctx, lease, "migrator")
	r.Store.BeforeCommit = nil
	if e == nil {
		t.Fatal("rollback ignored")
	}
	var active sql.NullString
	if e = r.Store.DB.QueryRow("SELECT active_migration_task FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&active); e != nil || !active.Valid || active.String != string(receipt.TaskID) {
		t.Fatal("rollback cleared active marker", active, e)
	}
	var finals int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='INSTANCE_MIGRATION'", receipt.TaskID).Scan(&finals); e != nil || finals != 0 {
		t.Fatal("rollback fabricated final commit", finals, e)
	}
	if current, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope); e != nil || current != i || !bytes.Equal(documentsBefore, snapshot()) {
		t.Fatal("rollback changed release, versions or original history", e)
	}
	for _, command := range []string{"RESUME", "END"} {
		if _, e = r.World.Control(runtimeContextWithParent(t, r, ctx), scope, world.ControlCommand{Key: "forbidden-" + command, Request: generated.AdminControlRequest{ProductID: "coffee", Command: command, ExpectedVersions: i.Versions}}); e == nil {
			t.Fatal("control entered active migration", command)
		}
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic migration ACK lost")
	}
	e = r.World.Process(ctx, lease, "migrator")
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("actual committed migration did not lose its ACK", e)
	}
	finalKey := content.Identity(string(receipt.TaskID), "result")
	finalReceipt, e := r.Store.QueryCommitScoped(ctx, s, &scope, s.SubjectID, finalKey)
	if e != nil || !finalReceipt.Committed || finalReceipt.TaskID != string(receipt.TaskID) || finalReceipt.Key != finalKey {
		t.Fatal("lost ACK lacks exact original final receipt", finalReceipt, e)
	}
	var receiptCount int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts").Scan(&receiptCount); e != nil {
		t.Fatal(e)
	}
	if e = world.New(r.Store).Process(ctx, lease, "migrator"); e != nil {
		t.Fatal("original final receipt recovery", e)
	}
	again, e := r.World.ExecuteInstanceMigration(runtimeContextWithParent(t, r, ctx), scope, req, "migrate")
	if e != nil {
		t.Fatal(e)
	}
	wantReceipt, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal(e)
	}
	gotReceipt, e := json.Marshal(again)
	if e != nil || !bytes.Equal(wantReceipt, gotReceipt) {
		t.Fatal("migration retry changed original returned identity", e)
	}
	for key, want := range map[string]store.CommitReceipt{executeKey: executeReceipt, finalKey: finalReceipt, joint.Key: joint} {
		got, e := r.Store.QueryCommitScoped(ctx, s, &scope, s.SubjectID, key)
		if e != nil || got != want {
			t.Fatal("recovery replaced an original receipt", e)
		}
	}
	var tasks, registrations, intakes, commits, receiptsAfter int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks WHERE task_kind='MIGRATION'),(SELECT COUNT(*) FROM registered_jobs WHERE operation='INSTANCE_MIGRATION'),(SELECT COUNT(*) FROM commit_receipts WHERE operation='executeInstanceMigration'),(SELECT COUNT(*) FROM commit_receipts WHERE operation='INSTANCE_MIGRATION'),(SELECT COUNT(*) FROM commit_receipts)").Scan(&tasks, &registrations, &intakes, &commits, &receiptsAfter); e != nil || tasks != 1 || registrations != 1 || intakes != 1 || commits != 1 || receiptsAfter != receiptCount {
		t.Fatal("original recovery duplicated migration task/key/commit", tasks, registrations, intakes, commits, receiptsAfter, e)
	}
	if !bytes.Equal(documentsBefore, snapshot()) || !bytes.Equal(journalBefore, journalBytes(t, r)) {
		t.Fatal("migration recovery changed actor/item/memory history or original provider journal")
	}
	i, e = r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil {
		t.Fatal(e)
	}
	v := runtimeVersions(t, i)
	if i.InstanceState != "PAUSED" || i.Versions.ReleaseID != string(target.ReleaseID) || v.WorldRevision != versionsBefore.WorldRevision+1 || v.ControlVersion != versionsBefore.ControlVersion+1 || v.ViewEpoch != versionsBefore.ViewEpoch+1 || v.PrivacyEpoch != versionsBefore.PrivacyEpoch {
		t.Fatal("recovery did not preserve exact managed version progression")
	}
	if e = r.Store.DB.QueryRow("SELECT active_migration_task FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&active); e != nil || active.Valid {
		t.Fatal("committed migration left an active marker", e)
	}
	runtimeControl(t, r, i, "RESUME")
}

type replayInitialMemory struct {
	world.InitialMemoryAdapter
	calls atomic.Int32
}

func (m *replayInitialMemory) CreateInitial(context.Context, store.Tx, domain.Scope, world.Initialization) error {
	m.calls.Add(1)
	return errors.New("original creation replay invoked INITIAL body")
}

func TestRuntimeCreationLostACKConcurrentReplayAndClosedMissingWorld(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := context.Background()
	cmd := world.CreateInstanceCommand{Key: "unknown-create", Request: generated.AdminCreateInstanceRequest{ProductID: "coffee", ReleaseID: rel, Mode: "PRIVATE", InitialParticipants: []string{}, ClientCapabilities: []string{"TEXT"}}}
	if _, e := world.New(r.Store).CreateInstance(runtimeContextWithParent(t, r, ctx), s, cmd); !errors.Is(e, store.ErrDenied) {
		t.Fatal("incomplete production assembly accepted new creation", e)
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	if _, e := r.World.CreateInstance(runtimeContextWithParent(t, r, ctx), s, cmd); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	if _, e := world.New(r.Store).CreateInstance(runtimeContextWithParent(t, r, ctx), s, cmd); !errors.Is(e, store.ErrDenied) {
		t.Fatal("incomplete production assembly accepted original replay", e)
	}
	// Reconstruct the production adapter assembly before starting either replay.
	// Both retain the existing installation's current admission, files and ledger.
	// The sentinel keeps real lock collection but fails if replay runs INITIAL.
	var recovered [2]*app.Runtime
	var initial [2]*replayInitialMemory
	for n := range recovered {
		recovered[n] = app.NewRuntime(r.Store, r.Files, r.Auth)
		recovered[n].Local = r.Local
		recovered[n].Memory.Ledger = r.Local.Ledger
		initial[n] = &replayInitialMemory{InitialMemoryAdapter: recovered[n].World.InitialMemory}
		recovered[n].World.InitialMemory = initial[n]
	}
	type result struct {
		i generated.Instance
		e error
	}
	userCtx := runtimeContextWithParent(t, r, ctx)
	done := make(chan result, 2)
	for _, runtime := range recovered {
		go func() { i, e := runtime.World.CreateInstance(userCtx, s, cmd); done <- result{i, e} }()
	}
	a, b := <-done, <-done
	if a.e != nil || b.e != nil || a.i != b.i {
		t.Fatal(a, b)
	}
	for _, adapter := range initial {
		if adapter.calls.Load() != 0 {
			t.Fatal("original receipt replay reinitialized memory")
		}
	}
	var receipts int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND operation='createAdminInstance' AND command_key=? AND commit_key=?", s.TenantID, s.ProductID, s.SubjectID, cmd.Key, a.i.Scope.WorldInstanceID).Scan(&receipts); e != nil || receipts != 1 {
		t.Fatal("concurrent replay did not preserve the one original creation receipt", receipts, e)
	}
	cmd.Request.Mode = "SHARED"
	if _, e := r.World.CreateInstance(runtimeContextWithParent(t, r, ctx), s, cmd); e == nil {
		t.Fatal("shared silently downgraded")
	}
	v := runtimeVersions(t, a.i)
	v.ControlVersion = 1
	missing := domain.Scope{ProductScope: s, WorldInstanceID: "absent"}
	g, e := content.Gate(ctx, r.Store, s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: s, WorldScope: &missing, Versions: &v, OwnerSubjectID: s.SubjectID, GateVersions: g, Operation: "turn", CommandKey: "absent", CommitKey: "absent"}, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("ordinary missing world acquired creation exception")
	}
}
func TestRuntimeControlLeaseTakeoverAndOriginalCommitUnknown(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := context.Background()
	i := createRuntimeWorld(t, r, rel, "create")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	cmd := world.ControlCommand{Key: "start", Request: generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: i.Versions}}
	task, e := r.World.Control(runtimeContextWithParent(t, r, ctx), scope, cmd)
	if e != nil {
		t.Fatal(e)
	}
	old, e := r.Store.ClaimRegistered(ctx, "old-worker", time.Now(), time.Millisecond, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL"}})
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	current, e := r.Store.ClaimRegistered(ctx, "new-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL"}})
	if e != nil || current.Fence <= old.Fence {
		t.Fatal(current, e)
	}
	if e = r.World.Process(ctx, old, "old-worker"); e == nil {
		t.Fatal("stale worker committed")
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	if e = r.World.Process(ctx, current, "new-worker"); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	if e = world.New(r.Store).Process(ctx, current, "new-worker"); e != nil {
		t.Fatal("original execution recovery", e)
	}
	replay, e := r.World.Control(runtimeContextWithParent(t, r, ctx), scope, cmd)
	if e != nil || replay.TaskID != task.TaskID {
		t.Fatal(replay, e)
	}
	after, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil || after.Versions.ControlVersion != "1" {
		t.Fatal(after, e)
	}
}
func TestMigrationBlocksPendingStateWriters(t *testing.T) {
	ctx := context.Background()
	// Use a real accepted/sent TURN: its reply is lost but its original result
	// can still resume a world write. An unmanaged legacy TURN proves no closure.
	r, scope, task, _ := originalResultFixture(t, true)
	i, e := r.World.GetInstance(runtimeContextWithParent(t, r, ctx), scope)
	if e != nil {
		t.Fatal(e)
	}
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, scope.ProductScope, i.Versions.ReleaseID, "expression_style", "short")
	if _, e = r.Auth.Authorize(ctx, "Bearer "+runtimeToken, scope.ProductID, "runtime.migrate"); e != nil {
		t.Fatal("fixture lacks current migration permission", e)
	}
	var operation, taskState, envelope, attemptState string
	var holds int
	if e = r.Store.DB.QueryRow("SELECT j.operation,t.state,h.state,a.state,(SELECT COUNT(*) FROM reservations WHERE task_id=?) FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) JOIN attempts a USING(tenant_id,product_id,task_id) WHERE t.task_id=? AND a.slot=0", task, task).Scan(&operation, &taskState, &envelope, &attemptState, &holds); e != nil || operation != "TURN" || taskState != "RUNNING" || envelope != "ACTIVE" || attemptState != "SENT" || holds != 6 {
		t.Fatal("fixture is not a registered live original state writer", operation, taskState, envelope, attemptState, holds, e)
	}
	snapshot := func() []byte {
		t.Helper()
		state := map[string][]string{}
		for _, table := range []string{"tasks", "registered_jobs", "harness_envelopes", "attempts", "reservations", "commit_receipts", "events", "approved_results", "postings", "outbox", "world_controls", "actors", "items", "runtime_instances", "migration_plans", "harness_world_seals"} {
			rows, e := r.Store.DB.Query("SELECT * FROM " + table)
			if e != nil {
				t.Fatal(e)
			}
			columns, e := rows.Columns()
			if e != nil {
				rows.Close()
				t.Fatal(e)
			}
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for n := range values {
					pointers[n] = &values[n]
				}
				if e = rows.Scan(pointers...); e != nil {
					rows.Close()
					t.Fatal(e)
				}
				b, e := json.Marshal(values)
				if e != nil {
					rows.Close()
					t.Fatal(e)
				}
				state[table] = append(state[table], string(b))
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				t.Fatal(e)
			}
			sort.Strings(state[table])
		}
		b, e := json.Marshal(state)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	before, originalJ := snapshot(), journalBytes(t, r)
	request := generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}
	if _, e = r.World.PreviewInstanceMigration(runtimeContextWithParent(t, r, ctx), scope, request, "preview"); !errors.Is(e, store.ErrDenied) {
		t.Fatal("live original state writer must block migration", e)
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", request, 403, nil)
	if !bytes.Equal(before, snapshot()) || !bytes.Equal(originalJ, journalBytes(t, r)) {
		t.Fatal("blocked migration changed original world, tasks, holds, receipts or journal")
	}
	var plans, migrations, seals int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM migration_plans),(SELECT COUNT(*) FROM tasks WHERE task_kind='MIGRATION'),(SELECT COUNT(*) FROM harness_world_seals)").Scan(&plans, &migrations, &seals); e != nil || plans != 0 || migrations != 0 || seals != 0 {
		t.Fatal("live writer admitted migration side effects", plans, migrations, seals, e)
	}
}
func TestMigrationCancelReleasesOnlyOwnedActiveMarker(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, rel, "expression_style", "short")
	var p generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &p)
	hash, e := content.Hash(p)
	if e != nil {
		t.Fatal(e)
	}
	var task generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migrations", "migrate", generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: p.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: p.ResolutionMap}, 202, &task)
	runtimeHTTP(t, r, "POST", "/tasks/"+string(task.TaskID)+"/cancel", "cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "Cancel paused migration"}, 202, nil)
	runtimeControl(t, r, i, "RESUME")
}
