package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 422, nil)
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
	_, e := r.World.Control(ctx, scope, world.ControlCommand{Key: "start", Request: generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: i.Versions}})
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
	after, e := r.World.GetInstance(ctx, scope)
	if e != nil || after.InstanceState != "CREATED" {
		t.Fatal("crashed writes survived", after, e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	after, e = r.World.GetInstance(ctx, scope)
	if e != nil || after.Versions.ControlVersion != "1" {
		t.Fatal("restart duplicated/lost control", after, e)
	}
}

func TestMigrationPreventsResumeAndUnknownUsesOriginalReceipt(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := context.Background()
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	br, q := actionTask(t, r, s, i, "history-turn")
	if _, e := commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &i)
	i = runtimeControl(t, r, i, "PAUSE")
	var itemBefore, actorBefore []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM items").Scan(&itemBefore); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT document FROM actors ORDER BY actor_id LIMIT 1").Scan(&actorBefore); e != nil {
		t.Fatal(e)
	}
	target := nextRuntimeRelease(t, r, s, rel, "expression_style", "quiet and precise")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	p, e := r.World.PreviewInstanceMigration(ctx, scope, generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "preview")
	if e != nil {
		t.Fatal(e)
	}
	hash, e := content.Hash(p)
	if e != nil {
		t.Fatal(e)
	}
	req := generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: p.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: p.ResolutionMap}
	receipt, e := r.World.ExecuteInstanceMigration(ctx, scope, req, "migrate")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "migrator", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "MIGRATION", Operation: "INSTANCE_MIGRATION"}})
	if e != nil {
		t.Fatal(e)
	}
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic final rollback") }
	if e = r.World.Process(ctx, lease, "migrator"); e == nil {
		t.Fatal("rollback ignored")
	}
	r.Store.BeforeCommit = nil
	var active sql.NullString
	if e = r.Store.DB.QueryRow("SELECT active_migration_task FROM runtime_instances").Scan(&active); e != nil || !active.Valid || active.String != string(receipt.TaskID) {
		t.Fatal("rollback cleared active marker", active, e)
	}
	if _, e = r.World.Control(ctx, scope, world.ControlCommand{Key: "forbidden-end", Request: generated.AdminControlRequest{ProductID: "coffee", Command: "END", ExpectedVersions: i.Versions}}); e == nil {
		t.Fatal("END entered active migration")
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic migration ACK lost")
	}
	if e = r.World.Process(ctx, lease, "migrator"); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	if e = world.New(r.Store).Process(ctx, lease, "migrator"); e != nil {
		t.Fatal("original final receipt recovery", e)
	}
	again, e := r.World.ExecuteInstanceMigration(ctx, scope, req, "migrate")
	if e != nil || again.TaskID != receipt.TaskID {
		t.Fatal(again, e)
	}
	var itemAfter, actorAfter []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM items").Scan(&itemAfter); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM actors ORDER BY actor_id LIMIT 1").Scan(&actorAfter); e != nil {
		t.Fatal(e)
	}
	if string(itemAfter) != string(itemBefore) || string(actorAfter) != string(actorBefore) {
		t.Fatal("migration reset actor/fact/history")
	}
	i, e = r.World.GetInstance(ctx, scope)
	if e != nil || i.InstanceState != "PAUSED" || i.Versions.WorldRevision != "2" || i.Versions.ControlVersion != "3" || i.Versions.ViewEpoch != "2" {
		t.Fatal(i, e)
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
	if _, e := world.New(r.Store).CreateInstance(ctx, s, cmd); !errors.Is(e, store.ErrDenied) {
		t.Fatal("incomplete production assembly accepted new creation", e)
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	if _, e := r.World.CreateInstance(ctx, s, cmd); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	if _, e := world.New(r.Store).CreateInstance(ctx, s, cmd); !errors.Is(e, store.ErrDenied) {
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
	done := make(chan result, 2)
	for _, runtime := range recovered {
		go func() { i, e := runtime.World.CreateInstance(ctx, s, cmd); done <- result{i, e} }()
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
	if _, e := r.World.CreateInstance(ctx, s, cmd); e == nil {
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
	task, e := r.World.Control(ctx, scope, cmd)
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
	replay, e := r.World.Control(ctx, scope, cmd)
	if e != nil || replay.TaskID != task.TaskID {
		t.Fatal(replay, e)
	}
	after, e := r.World.GetInstance(ctx, scope)
	if e != nil || after.Versions.ControlVersion != "1" {
		t.Fatal(after, e)
	}
}
func TestMigrationBlocksPendingStateWriters(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	_, _ = actionTask(t, r, s, i, "pending-turn")
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, rel, "expression_style", "short")
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 422, nil)
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
