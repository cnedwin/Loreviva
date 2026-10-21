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
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type releaseWithdrawer interface {
	WithdrawRelease(context.Context, domain.ProductScope, string, string, string) (store.CommitReceipt, error)
}

func withdrawalPort(t *testing.T, r *app.Runtime) releaseWithdrawer {
	t.Helper()
	p, ok := any(r.Content).(releaseWithdrawer)
	if !ok {
		t.Fatal("lawful published release/current Operator reached withdrawal boundary: owning port unavailable")
	}
	return p
}
func withdrawalFixture(t *testing.T) (*app.Runtime, context.Context, generated.Release) {
	t.Helper()
	r, scope, id := runtimeFixture(t)
	op := runtimeContext(t, r)
	rel, err := r.Content.GetRelease(op, scope, id)
	if err != nil {
		t.Fatal("real published prerequisite", err)
	}
	return r, op, rel
}
func withdrawalAssert(t *testing.T, r *app.Runtime, op context.Context, rel generated.Release, key string) store.CommitReceipt {
	t.Helper()
	out, err := withdrawalPort(t, r).WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, key)
	if err != nil || !out.Committed || out.TaskID != "" || out.OwnerSubjectID != r.Auth.Scope().SubjectID {
		t.Fatal("owning withdrawal", err)
	}
	var live bool
	if err = r.Store.DB.QueryRow("SELECT available FROM releases WHERE tenant_id=? AND product_id=? AND release_id=?", r.Auth.Scope().TenantID, r.Auth.Scope().ProductID, rel.ReleaseID).Scan(&live); err != nil || live {
		t.Fatal("not withdrawn", err)
	}
	return out
}

func withdrawalLive(t *testing.T, r *app.Runtime, rel generated.Release, want bool) {
	t.Helper()
	var live bool
	if e := r.Store.DB.QueryRow("SELECT available FROM releases WHERE release_id=?", rel.ReleaseID).Scan(&live); e != nil || live != want {
		t.Fatal("availability", live, e)
	}
}
func withdrawalOriginal(t *testing.T, r *app.Runtime, rel generated.Release) []byte {
	t.Helper()
	var raw, check, result, hash []byte
	var key string
	if e := r.Store.DB.QueryRow("SELECT document FROM content_releases WHERE release_id=?", rel.ReleaseID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT document FROM content_checks WHERE report_id=?", rel.CheckReportID).Scan(&check); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT a.document,c.payload_hash,c.commit_key FROM admin_results a JOIN commit_receipts c ON c.commit_key=a.result_id WHERE a.schema_name='Release' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.release_id'))=?", rel.ReleaseID).Scan(&result, &hash, &key); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal([]any{raw, check, result, hash, key})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// The runtime fixture supplies a legacy action, not an explicit grant. Install
// one real row before testing override faults, and reject a missed injection.
func withdrawalExplicitArchive(t *testing.T, r *app.Runtime) {
	t.Helper()
	result, e := r.Store.DB.Exec("INSERT INTO studio_operator_grants VALUES('demo-local','owner','PRODUCT','coffee','P:coffee','content.archive','PRODUCT_ADMIN','ALLOW',1,NULL,1)")
	if e != nil {
		t.Fatal(e)
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		t.Fatal("explicit archive fixture", n, e)
	}
}
func withdrawalCredentialCustody(t *testing.T, value string) {
	t.Helper()
	channel := os.Getenv("LOREVIVA_TEST_CUSTODY_FILE")
	if channel == "" {
		return
	}
	if filepath.Dir(channel) != os.Getenv("TMP") {
		t.Fatal("private credential custody location")
	}
	f, e := os.OpenFile(channel, os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		t.Fatal("private credential custody unavailable")
	}
	e = json.NewEncoder(f).Encode([]string{value})
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		t.Fatal("private credential custody failed")
	}
}
func TestContentReleaseWithdrawalOwningAndReplay(t *testing.T) {
	r, op, rel := withdrawalFixture(t)
	before := withdrawalOriginal(t, r, rel)
	first := withdrawalAssert(t, r, op, rel, "s74-first")
	again, err := withdrawalPort(t, r).WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "s74-first")
	if err != nil || again != first {
		t.Fatal("retained original replay", err)
	}
	if !bytes.Equal(before, withdrawalOriginal(t, r, rel)) {
		t.Fatal("withdrawal mutated original publication")
	}
	for _, key := range []string{"s74-first", "s74-new"} {
		hash := rel.ContentHash
		if key == "s74-first" {
			hash = strings.Repeat("0", 64)
		}
		if _, err = withdrawalPort(t, r).WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), hash, key); err == nil {
			t.Fatal("changed hash/new key at zero accepted")
		}
	}
	// Same-key alternate retained target cannot borrow the original command.
	other := withdrawalPublish(t, r, op, "WORLD", rel.Input, "s74-alternate")
	if _, e := withdrawalPort(t, r).WithdrawRelease(op, r.Auth.Scope(), string(other.ReleaseID), other.ContentHash, "s74-first"); !errors.Is(e, store.ErrConflict) {
		t.Fatal("same key changed target", e)
	}
	withdrawalLive(t, r, other, true)
	t.Run("actual-commit-lost-ack", func(t *testing.T) {
		rr, oo, pub := withdrawalFixture(t)
		rr.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
			if e := tx.Commit(); e != nil {
				return e
			}
			return errors.New("synthetic lost commit ACK")
		}
		_, e := withdrawalPort(t, rr).WithdrawRelease(oo, rr.Auth.Scope(), string(pub.ReleaseID), pub.ContentHash, "s74-unknown")
		rr.Store.Commit = nil
		if !errors.Is(e, store.ErrCommitUnknown) {
			t.Fatal("no original UNKNOWN", e)
		}
		withdrawalLive(t, rr, pub, false)
		recovered := withdrawalAssert(t, rr, oo, pub, "s74-unknown")
		if recovered.Key != content.Identity("demo-local", "coffee", "creator", "withdrawContentRelease", "s74-unknown") {
			t.Fatal("recovery minted another identity")
		}
	})
}
func TestContentReleaseWithdrawalCurrentPublisherAuthority(t *testing.T) {
	r, op, rel := withdrawalFixture(t)
	withdrawalExplicitArchive(t, r)
	port := withdrawalPort(t, r)
	scope := r.Auth.Scope()
	deny := func(t *testing.T, ctx context.Context, p domain.ProductScope) {
		t.Helper()
		if _, e := port.WithdrawRelease(ctx, p, string(rel.ReleaseID), rel.ContentHash, "s74-denied"); e == nil {
			t.Fatal("noncurrent authority accepted")
		}
		withdrawalLive(t, r, rel, true)
	}
	t.Run("plain-scope", func(t *testing.T) { deny(t, context.Background(), scope) })
	t.Run("cross-scope", func(t *testing.T) { p := scope; p.SubjectID = "second"; deny(t, op, p) })
	t.Run("foreign-store", func(t *testing.T) {
		foreign := content.New(store.New(r.Store.DB))
		if _, e := foreign.WithdrawRelease(op, scope, string(rel.ReleaseID), rel.ContentHash, "s74-foreign"); e == nil {
			t.Fatal("foreign Store borrowed seal")
		}
	})
	t.Run("real-other-publisher", func(t *testing.T) {
		other := scope
		other.SubjectID = "s74-other"
		if _, e := r.Store.DB.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES(?,?,?,1,1,1,1)", other.TenantID, other.ProductID, other.SubjectID); e != nil {
			t.Fatal(e)
		}
		const secret = "synthetic-s74-other-publisher-secret-32"
		withdrawalCredentialCustody(t, secret)
		who := studioRegisteredFixture(t, r, other, "s74-other-credential", secret, maintenance.OperatorActions)
		deny(t, who, other)
	})
	t.Run("real-SERVICE", func(t *testing.T) {
		rr, oo, service, req, _ := integrationCreationFixture(t)
		pub, e := rr.Content.GetRelease(oo, rr.Auth.Scope(), req.ReleaseID)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = withdrawalPort(t, rr).WithdrawRelease(service, rr.Auth.Scope(), req.ReleaseID, pub.ContentHash, "s74-service-denied"); e == nil {
			t.Fatal("SERVICE borrowed publisher")
		}
		withdrawalLive(t, rr, pub, true)
	})
	for _, mode := range []string{"DENY", "DISABLED", "EXPIRED"} {
		t.Run(mode, func(t *testing.T) {
			effect := "ALLOW"
			enabled := true
			expiry := "NULL"
			if mode == "DENY" {
				effect = "DENY"
			}
			if mode == "DISABLED" {
				enabled = false
			}
			if mode == "EXPIRED" {
				expiry = "DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND)"
			}
			result, e := r.Store.DB.Exec("UPDATE studio_operator_grants SET effect=?,enabled=?,expires_at="+expiry+" WHERE credential_id='owner' AND action='content.archive'", effect, enabled)
			if e != nil {
				t.Fatal(e)
			}
			if n, e := result.RowsAffected(); e != nil || n != 1 {
				t.Fatal("authority fault prerequisite", n, e)
			}
			deny(t, op, scope)
			if _, e := r.Store.DB.Exec("UPDATE studio_operator_grants SET effect='ALLOW',enabled=1,expires_at=NULL WHERE credential_id='owner' AND action='content.archive'"); e != nil {
				t.Fatal(e)
			}
		})
	}
	t.Run("missing-both", func(t *testing.T) {
		if _, e := r.Store.DB.Exec("DELETE FROM studio_operator_grants WHERE credential_id='owner' AND action='content.archive'"); e != nil {
			t.Fatal(e)
		}
		if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.archive'"); e != nil {
			t.Fatal(e)
		}
		deny(t, op, scope)
		if _, e := r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner','content.archive')"); e != nil {
			t.Fatal(e)
		}
	})
	// A true absent explicit row retains the old action, without granting a row.
	withdrawalAssert(t, r, op, rel, "s74-legacy")
	t.Run("current-replay-deny", func(t *testing.T) {
		if _, e := r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
			t.Fatal(e)
		}
		if _, e := port.WithdrawRelease(op, scope, string(rel.ReleaseID), rel.ContentHash, "s74-legacy"); e == nil {
			t.Fatal("disabled issuer replay")
		}
	})
	t.Run("historical-report-expiry-current-route", func(t *testing.T) {
		rr, oo, pub := withdrawalFixture(t)
		var raw []byte
		if e := rr.Store.DB.QueryRow("SELECT document FROM content_checks WHERE report_id=?", pub.CheckReportID).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var report generated.PublishCheckReport
		if e := json.Unmarshal(raw, &report); e != nil {
			t.Fatal(e)
		}
		report.ExpiresAt = "2020-01-01T00:00:00Z"
		raw, e := json.Marshal(report)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = rr.Store.DB.Exec("UPDATE content_checks SET document=? WHERE report_id=?", raw, pub.CheckReportID); e != nil {
			t.Fatal(e)
		}
		// Historical-time fault, not natural expiry; current publication routing is irrelevant.
		if _, e = rr.Store.DB.Exec("UPDATE operator_actions SET action='unused.publish' WHERE credential_id='owner' AND action='content.publish'"); e != nil {
			t.Fatal(e)
		}
		withdrawalAssert(t, rr, oo, pub, "s74-old-check")
	})
}
func TestContentReleaseWithdrawalPublicationProofAndFinalRollback(t *testing.T) {
	r, op, rel := withdrawalFixture(t)
	withdrawalExplicitArchive(t, r)
	// These are real owning world/task rows. Faults must pass FK checks before
	// they can demonstrate the withdrawal verifier, not merely a DB rejection.
	w := createRuntimeWorld(t, r, string(rel.ReleaseID), "s74-proof-world")
	cleanup, e := app.QueueCleanup(op, r.Store, r.Auth.Scope(), "s74-proof-task")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('demo-local','coffee','other-owner',1,1,1,1)"); e != nil {
		t.Fatal(e)
	}
	port := withdrawalPort(t, r)
	before := withdrawalOriginal(t, r, rel)
	oldID := content.Identity("demo-local", "coffee", "creator", "publishContentDraft", "seed-publish")
	if e := r.Store.DB.QueryRow("SELECT a.result_id FROM admin_results a WHERE a.schema_name='Release' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(a.document USING utf8mb4),'$.release_id'))=?", rel.ReleaseID).Scan(&oldID); e != nil {
		t.Fatal(e)
	}
	faults := []struct {
		name, query string
		args        []any
	}{
		{"available", "UPDATE releases SET available=1 WHERE release_id=?", []any{rel.ReleaseID}},
		{"document", "UPDATE content_releases SET document=CONCAT(document,' ') WHERE release_id=?", []any{rel.ReleaseID}},
		{"hash", "UPDATE content_releases SET content_hash=UNHEX(REPEAT('00',32)) WHERE release_id=?", []any{rel.ReleaseID}},
		{"result", "UPDATE admin_results SET document=CONCAT(document,' ') WHERE result_id=?", []any{oldID}},
		{"report", "UPDATE content_checks SET document=CONCAT(document,' ') WHERE report_id=?", []any{rel.CheckReportID}},
		{"original-operation", "UPDATE commit_receipts SET operation='otherPublish' WHERE commit_key=?", []any{oldID}},
		{"original-hash", "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('00',32)) WHERE commit_key=?", []any{oldID}},
		{"original-time", "UPDATE commit_receipts SET committed_at=DATE_SUB(committed_at,INTERVAL 1 SECOND) WHERE commit_key=?", []any{oldID}},
		{"original-owner", "UPDATE commit_receipts SET owner_subject_id='other-owner' WHERE commit_key=?", []any{oldID}},
		{"original-task", "UPDATE commit_receipts SET task_id=? WHERE commit_key=?", []any{cleanup.TaskID, oldID}},
		{"original-world", "UPDATE commit_receipts SET world_instance_id=?,scope_kind='WORLD' WHERE commit_key=?", []any{w.Scope.WorldInstanceID, oldID}},
		{"original-command-derived-id", "UPDATE commit_receipts SET command_key='other-original-key' WHERE commit_key=?", []any{oldID}},
		{"new-hash", "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('00',32)) WHERE operation='withdrawContentRelease'", nil},
		{"new-operation", "UPDATE commit_receipts SET operation='otherWithdrawal' WHERE operation='withdrawContentRelease'", nil},
		{"new-owner", "UPDATE commit_receipts SET owner_subject_id='other-owner' WHERE operation='withdrawContentRelease'", nil},
		{"new-task", "UPDATE commit_receipts SET task_id=? WHERE operation='withdrawContentRelease'", []any{cleanup.TaskID}},
		{"new-world", "UPDATE commit_receipts SET world_instance_id=?,scope_kind='WORLD' WHERE operation='withdrawContentRelease'", []any{w.Scope.WorldInstanceID}},
		{"current-deny", "UPDATE studio_operator_grants SET effect='DENY' WHERE credential_id='owner' AND action='content.archive'", nil},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			reached := false
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				reached = true
				result, e := tx.ExecContext(ctx, fault.query, fault.args...)
				if e != nil {
					t.Errorf("fault injection failed: %v", e)
					return e
				}
				n, e := result.RowsAffected()
				if e != nil || n != 1 {
					t.Errorf("fault did not reach one row: %d %v", n, e)
					return store.ErrDenied
				}
				return nil
			}
			_, e := port.WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "s74-final-"+fault.name)
			r.Store.BeforeCommit = nil
			if !reached || e == nil {
				t.Fatal("final exact proof failed", reached, e)
			}
			withdrawalLive(t, r, rel, true)
			if !bytes.Equal(before, withdrawalOriginal(t, r, rel)) {
				t.Fatal("rollback changed publication")
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='withdrawContentRelease' OR operation='otherWithdrawal'").Scan(&n); e != nil || n != 0 {
				t.Fatal("partial receipt", n, e)
			}
		})
	}
	withdrawalAssert(t, r, op, rel, "s74-proof-positive")
	// Replay still validates retained proof, not merely a safe-looking receipt.
	if _, e := r.Store.DB.Exec("UPDATE admin_results SET document=CONCAT(document,' ') WHERE result_id=?", oldID); e != nil {
		t.Fatal(e)
	}
	if _, e := port.WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "s74-proof-positive"); e == nil {
		t.Fatal("corrupt retained original replay")
	}
}
func TestIntegrationCreateWithdrawalCommitOrders(t *testing.T) {
	t.Run("withdraw-commits-first", func(t *testing.T) {
		r, op, service, req, _ := integrationCreationFixture(t)
		plan, err := r.Store.PrepareIntegrationCreation(service, "s74-create", req)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := r.Content.GetRelease(op, r.Auth.Scope(), req.ReleaseID)
		if err != nil {
			t.Fatal(err)
		}
		withdrawalAssert(t, r, op, rel, "s74-before-create")
		if _, err = r.Store.RunIntegrationCreation(service, plan); err == nil || integrationWorldRows(t, r.Store, plan.WorldID()) != 0 {
			t.Fatal("withdraw-first left create rows", err)
		}
	})
	t.Run("create-commits-first", func(t *testing.T) {
		r, op, service, req, _ := integrationCreationFixture(t)
		plan, e := r.Store.PrepareIntegrationCreation(service, "s74-first-create", req)
		if e != nil {
			t.Fatal(e)
		}
		rel, e := r.Content.GetRelease(op, r.Auth.Scope(), req.ReleaseID)
		if e != nil {
			t.Fatal(e)
		}
		createHeld, releaseCreate, withdrawHeld, releaseWithdraw := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var createOnce, withdrawOnce sync.Once
		defer createOnce.Do(func() { close(releaseCreate) })
		defer withdrawOnce.Do(func() { close(releaseWithdraw) })
		var mu sync.Mutex
		var order []string
		r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM commit_receipts WHERE operation='withdrawContentRelease'").Scan(&n); e != nil {
				return e
			}
			ready, release := createHeld, releaseCreate
			if n != 0 {
				ready, release = withdrawHeld, releaseWithdraw
			}
			close(ready)
			select {
			case <-release:
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("owning COMMIT ordering fixture timed out")
			}
		}
		r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM commit_receipts WHERE operation='withdrawContentRelease'").Scan(&n); e != nil {
				return e
			}
			if e := tx.Commit(); e != nil {
				return e
			}
			label := "CREATE"
			if n != 0 {
				label = "WITHDRAW"
			}
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
			return nil
		}
		created, withdrawn := make(chan error, 1), make(chan error, 1)
		go func() { _, e := r.Store.RunIntegrationCreation(service, plan); created <- e }()
		select {
		case <-createHeld:
		case <-time.After(10 * time.Second):
			t.Fatal("CREATE did not reach real pre-COMMIT")
		}
		createOnce.Do(func() { close(releaseCreate) })
		if e = <-created; e != nil {
			t.Fatal("real CREATE commit", e)
		}
		// CREATE's owning result-read also reacquires its locks. Let that real
		// API return before starting withdrawal; never block it behind our hook.
		before := withdrawalWorldBytes(t, r, plan.WorldID())
		go func() {
			_, e := withdrawalPort(t, r).WithdrawRelease(op, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "s74-after-create")
			withdrawn <- e
		}()
		select {
		case <-withdrawHeld:
		case <-time.After(10 * time.Second):
			t.Fatal("withdrawal did not reach real pre-COMMIT")
		}
		withdrawOnce.Do(func() { close(releaseWithdraw) })
		if e = <-withdrawn; e != nil {
			t.Fatal("real withdrawal commit", e)
		}
		r.Store.BeforeCommit = nil
		r.Store.Commit = nil
		mu.Lock()
		same := len(order) == 2 && order[0] == "CREATE" && order[1] == "WITHDRAW"
		mu.Unlock()
		if !same || !bytes.Equal(before, withdrawalWorldBytes(t, r, plan.WorldID())) {
			t.Fatal("COMMIT order or retained namespace changed", order)
		}
		withdrawalLive(t, r, rel, false)
	})
}

func withdrawalWorldBytes(t *testing.T, r *app.Runtime, world string) []byte {
	t.Helper()
	var init []byte
	var owner, mode, release, receipt, player string
	var revision uint64
	if e := r.Store.DB.QueryRow("SELECT i.owner_subject_id,i.mode,i.initial_release_id,i.initialization,p.creation_receipt,p.player_subject_id,p.revision FROM runtime_instances i JOIN integration_world_participants p USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", world).Scan(&owner, &mode, &release, &init, &receipt, &player, &revision); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal([]any{owner, mode, release, init, receipt, player, revision})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func withdrawalAccepted(t *testing.T) (*app.Runtime, generated.TurnAccepted, context.Context, string, generated.Release) {
	t.Helper()
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
	var id string
	if e := r.Store.DB.QueryRow("SELECT release_id FROM world_controls WHERE world_instance_id=?", a.Task.Scope.WorldInstanceID).Scan(&id); e != nil {
		t.Fatal(e)
	}
	rel, e := r.Content.GetRelease(runtimeContext(t, r), r.Auth.Scope(), id)
	if e != nil {
		t.Fatal(e)
	}
	return r, a, client, token, rel
}
func withdrawalLease(t *testing.T, r *app.Runtime, a generated.TurnAccepted, worker string) store.RegisteredLease {
	t.Helper()
	l, e := r.Store.ClaimRegistered(context.Background(), worker, time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func withdrawalPublish(t *testing.T, r *app.Runtime, op context.Context, kind string, input generated.DraftInput, key string) generated.Release {
	t.Helper()
	d, e := r.Content.Create(op, r.Auth.Scope(), kind, "", input, key+"-draft")
	if e != nil {
		t.Fatal(e)
	}
	c, e := r.Content.CheckPublish(op, r.Auth.Scope(), string(d.DraftID), 1, []string{"MOCK_DEMO"}, key+"-check")
	if e != nil || !c.Passed {
		t.Fatal("real publish check", e)
	}
	p, e := r.Content.Publish(op, r.Auth.Scope(), string(d.DraftID), 1, string(c.ReportID), []string{"MOCK_DEMO"}, key+"-publish")
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// The dependency is actually referenced by the initial actor, not an unused
// lock entry. Both releases are produced by their original Content APIs.
func withdrawalDependency(t *testing.T, id *string) func(*app.Runtime, context.Context, *generated.DraftInput) {
	return func(r *app.Runtime, op context.Context, in *generated.DraftInput) {
		createPolicyInput(t, "owned")(in)
		var char, note json.RawMessage
		for _, raw := range in.Objects {
			var o map[string]any
			if e := json.Unmarshal(raw, &o); e != nil {
				t.Fatal(e)
			}
			if o["object_id"] == "alex-character" {
				o["initial_knowledge_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note"}}
				char, _ = json.Marshal(o)
			}
			if o["object_id"] == "s66-note" {
				note = append([]byte(nil), raw...)
			}
		}
		if len(char) == 0 || len(note) == 0 {
			t.Fatal("qualified dependency fixture missing")
		}
		dep := *in
		dep.RootObjectID = "alex-character"
		dep.Objects = []generated.ContentObject{char, note}
		dep.DependencyLocks = []generated.DependencyLock{}
		pub := withdrawalPublish(t, r, op, "CHARACTER", dep, "s74-dependency")
		*id = string(pub.ReleaseID)
		h, e := content.Hash(char)
		if e != nil {
			t.Fatal(e)
		}
		in.DependencyLocks = append(in.DependencyLocks, generated.DependencyLock{ReleaseID: pub.ReleaseID, ObjectID: "alex-character", Version: "1", ContentHash: h})
		for i, raw := range in.Objects {
			var o map[string]any
			if e = json.Unmarshal(raw, &o); e != nil {
				t.Fatal(e)
			}
			if o["kind"] == "WORLD" {
				o["initial_actor_setups"].([]any)[0].(map[string]any)["character_ref"] = map[string]any{"kind": "PINNED_DEFINITION", "release_id": pub.ReleaseID, "object_id": "alex-character", "version": "1", "content_hash": h}
				in.Objects[i], e = json.Marshal(o)
				if e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}
func TestIntegrationReleaseWithdrawalCurrentBodyAndSend(t *testing.T) {
	for _, boundary := range []string{"prepare", "authorize", "start-send", "approved-body"} {
		t.Run(boundary, func(t *testing.T) {
			r, a, client, _, rel := withdrawalAccepted(t)
			ctx := context.Background()
			worker := "s74-" + boundary
			l := withdrawalLease(t, r, a, worker)
			issuer, e := r.Store.EnterIntegrationTaskIssuer(ctx, l.TaskLease, worker)
			if e != nil {
				t.Fatal("real Background issuer", e)
			}
			if _, e = withdrawalPort(t, r).WithdrawRelease(client, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "client-denied"); e == nil {
				t.Fatal("CLIENT withdrew publication")
			}
			if _, e = withdrawalPort(t, r).WithdrawRelease(issuer, r.Auth.Scope(), string(rel.ReleaseID), rel.ContentHash, "background-denied"); e == nil {
				t.Fatal("Background withdrew publication")
			}
			var d store.DispatchRequest
			if boundary == "approved-body" {
				if e = r.Worker.Turns.Execute(ctx, l.TaskLease, worker); e != nil {
					t.Fatal(e)
				}
				b, _, e := r.Files.ReadApprovedTurn(client, domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}, a.TurnID)
				if e != nil || len(b) == 0 {
					t.Fatal("body positive", e)
				}
			} else {
				d, e = r.Turns.PrepareOriginalTurn(ctx, l.TaskLease, worker)
				if e != nil {
					t.Fatal("real Prepare positive", e)
				}
				if boundary == "start-send" {
					if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
						t.Fatal("real Authorize positive", e)
					}
					d.CommandKey = content.Identity(a.TurnID, "send")
					d.CommitKey = d.CommandKey
				}
			}
			before := withdrawalWorldBytes(t, r, a.Task.Scope.WorldInstanceID)
			withdrawalAssert(t, r, runtimeContext(t, r), rel, "s74-body-"+boundary)
			switch boundary {
			case "prepare":
				_, e = r.Turns.PrepareOriginalTurn(ctx, l.TaskLease, worker)
			case "authorize":
				_, e = r.Store.AuthorizeDispatch(ctx, d)
			case "start-send":
				_, e = r.Store.StartSend(ctx, d, d.AttemptID)
			case "approved-body":
				_, _, e = r.Files.ReadApprovedTurn(client, domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}, a.TurnID)
			}
			if e == nil {
				t.Fatal("withdrawn new body/send accepted", boundary)
			}
			if !bytes.Equal(before, withdrawalWorldBytes(t, r, a.Task.Scope.WorldInstanceID)) {
				t.Fatal("withdrawal rewrote world/lineage")
			}
			if boundary != "approved-body" {
				var calls int
				want := 0
				if boundary == "start-send" {
					want = 1
				} // Original AUTHORIZE already owns this call slot.
				if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&calls); e != nil || calls != want {
					t.Fatal("denied boundary used call", calls, e)
				}
			}
		})
	}
	for _, mode := range []string{"memory-root", "memory-dependency", "send-dependency"} {
		t.Run(mode, func(t *testing.T) {
			var dependency string
			var change func(*app.Runtime, context.Context, *generated.DraftInput)
			if mode != "memory-root" {
				change = withdrawalDependency(t, &dependency)
			}
			r, op, service, req, reg := integrationCreationFixtureActions(t, change, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
			w, e := r.World.CreateIntegrationInstance(service, "s74-dep-world", req)
			if e != nil {
				t.Fatal(e)
			}
			s, e := r.Store.ExchangeIntegrationSession(service, "s74-dep-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: w.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
			if e != nil {
				t.Fatal(e)
			}
			retainSessionScanMaterial(t, s.AccessToken)
			client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+s.AccessToken, domain.IntegrationClient)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ApplyPlayerSelfConsent(client, "s74-dep-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
				t.Fatal(e)
			}
			var raw []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='s66-player'", w.Scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var aggregate memory.Aggregate
			if e = json.Unmarshal(raw, &aggregate); e != nil {
				t.Fatal(e)
			}
			memoryID := ""
			for id := range aggregate.Nodes {
				memoryID = id
				break
			}
			if memoryID == "" {
				t.Fatal("no actual memory")
			}
			if _, node, _, e := r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, memoryID); e != nil || node.Body == "" {
				t.Fatal("created initial body positive", e)
			}
			target := req.ReleaseID
			if dependency != "" {
				target = dependency
			}
			pub, e := r.Content.GetRelease(op, r.Auth.Scope(), target)
			if e != nil {
				t.Fatal(e)
			}
			root, e := r.Content.GetRelease(op, r.Auth.Scope(), req.ReleaseID)
			if e != nil {
				t.Fatal(e)
			}
			var dispatch store.DispatchRequest
			if mode == "send-dependency" {
				start, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "s74-dep-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
				if e != nil {
					t.Fatal(e)
				}
				l, e := r.Store.ClaimRegistered(context.Background(), "s74-dep-start-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: start.TaskID}})
				if e != nil {
					t.Fatal(e)
				}
				if e = r.World.Process(context.Background(), l, "s74-dep-start-worker"); e != nil {
					t.Fatal(e)
				}
				var actor string
				var wr, cv uint64
				if e = r.Store.DB.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.Actors[0].ID')),c.world_revision,c.control_version FROM runtime_instances i JOIN world_controls c USING(tenant_id,product_id,world_instance_id) WHERE i.world_instance_id=?", w.Scope.WorldInstanceID).Scan(&actor, &wr, &cv); e != nil {
					t.Fatal(e)
				}
				q, e := r.Store.QuoteIntegrationTurn(service, "s74-dep-quote", generated.QuoteRequest{Scope: w.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"})
				if e != nil {
					t.Fatal(e)
				}
				a, e := r.Store.AcceptIntegrationTurn(client, w.Scope.WorldInstanceID, "s74-dep-intake", generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr, 10), ControlVersion: strconv.FormatUint(cv, 10)}, PrimaryActorID: actor, QuoteID: q.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)})
				if e != nil {
					t.Fatal(e)
				}
				tl := withdrawalLease(t, r, a, "s74-dep-worker")
				dispatch, e = r.Turns.PrepareOriginalTurn(context.Background(), tl.TaskLease, "s74-dep-worker")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.AuthorizeDispatch(context.Background(), dispatch); e != nil {
					t.Fatal(e)
				}
				dispatch.CommandKey = content.Identity(a.TurnID, "send")
				dispatch.CommitKey = dispatch.CommandKey
			}
			before := withdrawalWorldBytes(t, r, w.Scope.WorldInstanceID)
			withdrawalAssert(t, r, op, pub, "s74-dep-withdraw")
			if mode == "send-dependency" {
				if _, e = r.Store.StartSend(context.Background(), dispatch, dispatch.AttemptID); e == nil {
					t.Fatal("actual used dependency withdrawal bypassed StartSend")
				}
			} else {
				if _, _, _, e = r.Memory.ReadIntegrationMemory(client, w.Scope.WorldInstanceID, memoryID); e == nil {
					t.Fatal("withdrawn original source body returned")
				}
			}
			if !bytes.Equal(before, withdrawalWorldBytes(t, r, w.Scope.WorldInstanceID)) {
				t.Fatal("withdrawal rewrote original lineage")
			}
			if dependency != "" {
				withdrawalLive(t, r, root, true)
				withdrawalAssert(t, r, op, root, "s74-root-after-dependency")
			}
		})
	}
}
func TestIntegrationReleaseWithdrawalSafeMetadata(t *testing.T) {
	r, a, client, token, rel := withdrawalAccepted(t)
	world := a.Task.Scope.WorldInstanceID
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='s66-player'", world).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var aggregate memory.Aggregate
	if e := json.Unmarshal(raw, &aggregate); e != nil {
		t.Fatal(e)
	}
	id := ""
	for key := range aggregate.Nodes {
		id = key
		break
	}
	if id == "" {
		t.Fatal("no actual initial memory")
	}
	if _, _, _, e := r.Memory.ReadIntegrationMemoryMetadata(client, world, id); e != nil {
		t.Fatal("live metadata positive", e)
	}
	read := func() {
		t.Helper()
		if _, e := r.Store.ReadIntegrationTask(client, a.TurnID); e != nil {
			t.Fatal("safe task metadata", e)
		}
		if _, e := r.Store.ReadIntegrationIntakeReceipt(client, world, "turn-intake"); e != nil {
			t.Fatal("original intake metadata", e)
		}
	}
	read()
	withdrawalAssert(t, r, runtimeContext(t, r), rel, "s74-metadata")
	read()
	q := httptest.NewRequest("GET", "/world/v1/instances/"+world+"/turn-intake-receipt", nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	q.Header.Set("Idempotency-Key", "turn-intake")
	out := httptest.NewRecorder()
	r.Handler().ServeHTTP(out, q)
	if out.Code != 200 || generated.Validate("OriginalTurnIntakeReceipt", out.Body.Bytes()) != nil {
		t.Fatal("original-key HTTP metadata", out.Code)
	}
	if _, _, _, e := r.Memory.ReadIntegrationMemoryMetadata(client, world, id); e == nil {
		t.Fatal("Memory metadata bypassed live-publication visibility")
	}
	if _, _, _, e := r.Memory.ReadIntegrationMemory(client, world, id); e == nil {
		t.Fatal("withdrawn Memory body returned")
	}
}
func TestIntegrationReleaseWithdrawalNecessaryQueryCostCleanup(t *testing.T) {
	t.Run("sent-unknown-original-query-and-program-cleanup", func(t *testing.T) {
		r, a, client, _, rel := withdrawalAccepted(t)
		ctx := context.Background()
		l := withdrawalLease(t, r, a, "s74-query")
		r.Worker.Turns.Fault = harness.MockDropOriginalReply
		if e := r.Worker.Turns.Execute(ctx, l.TaskLease, "s74-query"); !errors.Is(e, harness.ErrMockReplyLost) {
			t.Fatal("actual SENT UNKNOWN", e)
		}
		before := journalBytes(t, r)
		withdrawalAssert(t, r, runtimeContext(t, r), rel, "s74-query-withdraw")
		cancel, e := r.Store.CancelIntegrationTask(client, a.TurnID, "s74-cancel", generated.CancelRequest{})
		if e != nil || cancel.ExternalCancelState != "UNKNOWN" {
			t.Fatal("original cancel", e)
		}
		if _, e = r.Turns.PrepareOriginalTurn(ctx, l.TaskLease, "s74-query"); e == nil {
			t.Fatal("withdrawn new generation")
		}
		id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}, a.TurnID)
		if e != nil {
			t.Fatal(e)
		}
		queryDue(t, r, a.TurnID) // Existing scheduler-only injection, not a changed natural deadline.
		ql, e := r.Store.BeginOriginalQuery(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		out, e := r.Store.RecordOriginalQuery(ctx, ql)
		if e != nil || out.State != "SUCCEEDED" || !out.UsageKnown || !out.NoFurtherCost || out.CostAtoms != 0 || out.Identity != id {
			t.Fatal("original query/cost", e)
		}
		if _, e = r.Store.RecordOriginalBillItem(ctx, id, "original-charge"); e == nil {
			t.Fatal("cancelled still-in-deadline bill gate bypassed")
		}
		cleanup, e := app.QueueCleanup(runtimeContext(t, r), r.Store, r.Auth.Scope(), "s74-cleanup")
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Worker.Once(ctx); e != nil {
			t.Fatal("PROGRAM cleanup", e)
		}
		var provenance, state string
		var credential sql.NullString
		if e = r.Store.DB.QueryRow("SELECT j.issuer_provenance,j.issuer_credential_id,t.state FROM registered_jobs j JOIN tasks t USING(tenant_id,product_id,task_id) WHERE t.task_id=?", cleanup.TaskID).Scan(&provenance, &credential, &state); e != nil || provenance != "PROGRAM_OWNED" || credential.Valid || state != "COMMITTED" {
			t.Fatal("cleanup identity", e)
		}
		var calls int
		if e = r.Store.DB.QueryRow("SELECT state,calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&state, &calls); e != nil || state != "CANCELLED" || calls != 1 || !bytes.Equal(before, journalBytes(t, r)) {
			t.Fatal("query revived narrative", e)
		}
	})
	t.Run("committed-original-cost", func(t *testing.T) {
		r, a, _, _, rel := withdrawalAccepted(t)
		ctx := context.Background()
		l := withdrawalLease(t, r, a, "s74-cost")
		if e := r.Worker.Turns.Execute(ctx, l.TaskLease, "s74-cost"); e != nil {
			t.Fatal(e)
		}
		id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: l.ProductScope, WorldInstanceID: *l.WorldInstanceID}, a.TurnID)
		if e != nil {
			t.Fatal(e)
		}
		withdrawalAssert(t, r, runtimeContext(t, r), rel, "s74-cost-withdraw")
		first, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
		if e != nil {
			t.Fatal("original committed cost", e)
		}
		again, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
		if e != nil || first != again {
			t.Fatal("cost original replay", e)
		}
	})
}
