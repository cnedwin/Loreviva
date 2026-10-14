package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPermissionWithoutAcquisitionCannotRecall(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "memory-world"), "START")
	var actors generated.AdminActorPage
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"/actors?product_id=coffee", "", nil, 200, &actors)
	alex, mia := "", ""
	for _, a := range actors.Items {
		if a.InitialSlotID == "alex" {
			alex = string(a.ActorInstanceID)
		}
		if a.InitialSlotID == "mia" {
			mia = string(a.ActorInstanceID)
		}
	}
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	svc := memory.New(r.Store, nil)
	ctx := runtimeContext(t, r)
	if e := svc.RecordStatement(ctx, ns, "statement", "intake", alex, "player likes tea", []memory.Use{memory.PrivateResponse, memory.Relationship}); e != nil {
		t.Fatal(e)
	}
	if e := svc.GrantSource(ctx, ns, "statement", mia, []memory.Use{memory.PrivateResponse}, "grant"); e != nil {
		t.Fatal(e)
	}
	scope := domain.RelationshipScope{Scope: ns.Scope, ActorInstanceID: mia, PlayerID: s.SubjectID}
	before, e := svc.AllowedContext(ctx, scope, mia, memory.Private)
	if e != nil {
		t.Fatal(e)
	}
	if countSource(before, "statement") != 0 {
		t.Fatal("grant manufactured acquisition")
	}
	scope.ActorInstanceID = alex
	positive, e := svc.AllowedContext(ctx, scope, alex, memory.Private)
	if e != nil || countSource(positive, "statement") != 1 {
		t.Fatalf("actual statement not recalled: %d %v", len(positive.Records), e)
	}
	if e = svc.Tell(ctx, ns, "statement", alex, mia, "telling"); e != nil {
		t.Fatal(e)
	}
	scope.ActorInstanceID = mia
	after, e := svc.AllowedContext(ctx, scope, mia, memory.Private)
	if e != nil || countSource(after, "statement") != 1 {
		t.Fatalf("same source not learned: %d %v", len(after.Records), e)
	}

	scope.ProductID = "other"
	if _, e = svc.AllowedContext(ctx, scope, mia, memory.Private); e == nil {
		t.Fatal("cross-product query accepted")
	}
}

func TestDeleteBlocksAllAcquiredCopiesActualGovernanceTask(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "govern-world"), "START")
	var actors generated.AdminActorPage
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"/actors?product_id=coffee", "", nil, 200, &actors)
	alex, mia := "", ""
	for _, a := range actors.Items {
		if a.InitialSlotID == "alex" {
			alex = string(a.ActorInstanceID)
		}
		if a.InitialSlotID == "mia" {
			mia = string(a.ActorInstanceID)
		}
	}
	ctx := runtimeContext(t, r)
	l, e := memory.CreateLedger(t.TempDir(), "installation", "ledger", "anchor")
	if e != nil {
		t.Fatal(e)
	}
	svc := memory.New(r.Store, l)
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"memory.govern", "memory.source.govern", "memory.correct"} {
		if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
			t.Fatal(e)
		}
	}
	if e = svc.RecordStatement(ctx, ns, "origin", "intake", alex, "private tea", []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	if e = svc.GrantSource(ctx, ns, "origin", mia, []memory.Use{memory.PrivateResponse}, "grant"); e != nil {
		t.Fatal(e)
	}
	if e = svc.Tell(ctx, ns, "origin", alex, mia, "tell"); e != nil {
		t.Fatal(e)
	}
	scope := domain.RelationshipScope{Scope: ns.Scope, ActorInstanceID: alex, PlayerID: s.SubjectID}
	c := memory.MemoryGovernanceCommand{Key: "erase-source", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{"origin"}, Expected: map[string]uint64{"origin": 1}}}
	receipt, e := svc.Govern(ctx, scope, c)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.Preservation != "CONFIRMED" || receipt.Cleanup != "LOCAL_COMPLETE" {
		t.Fatal(receipt)
	}
	for _, actor := range []string{alex, mia} {
		scope.ActorInstanceID = actor
		b, e := svc.AllowedContext(ctx, scope, actor, memory.Private)
		if e != nil || countSource(b, "origin") != 0 {
			t.Fatal("deleted origin recalls", actor, e, b)
		}
	}
	scope.ActorInstanceID = alex
	again, e := svc.Govern(ctx, scope, c)
	if e != nil || again != receipt {
		t.Fatal("original identity replay failed", again, e)
	}
	if e = svc.Tell(ctx, ns, "origin", alex, mia, "late-tell"); e == nil {
		t.Fatal("late source reacquired")
	}
	var state, kind, operation string
	if e = r.Store.DB.QueryRow("SELECT t.state,t.task_kind,j.operation FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.task_id=?", receipt.TaskID).Scan(&state, &kind, &operation); e != nil {
		t.Fatal(e)
	}
	if state != "COMMITTED" || kind != "GOVERNANCE" || operation != "MEMORY_GOVERNANCE" {
		t.Fatal(state, kind, operation)
	}
	var body string
	if e = r.Store.DB.QueryRow("SELECT CAST(document AS CHAR) FROM memory_aggregates").Scan(&body); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(body, "private tea") {
		t.Fatal("plaintext retained after cleanup")
	}
}

func TestPrivateMemoryHTTPMetadataBodyAndLegacyProjection(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "http-memory"), "START")
	r.Memory = memory.New(r.Store, r.Local.Ledger)
	r.Worker.Memory = r.Memory
	var actors generated.AdminActorPage
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"/actors?product_id=coffee", "", nil, 200, &actors)
	actor := string(actors.Items[0].ActorInstanceID)
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	if e := r.Memory.RecordStatement(runtimeContext(t, r), ns, "private-source", "statement", actor, "private projection text", []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"memory.metadata.read", "memory.private_body.read"} {
		if _, e := r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner',?)", action); e != nil {
			t.Fatal(e)
		}
	}
	query := "?product_id=coffee&data_subject_id=creator&actor_instance_id=" + actor
	root := "/instances/" + i.Scope.WorldInstanceID
	runtimeHTTP(t, r, "GET", root+"/memories"+query, "", nil, 403, nil)
	if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.metadata.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	var meta generated.AdminMemoryMetadataPageV1
	runtimeHTTP(t, r, "GET", root+"/memories"+query, "", nil, 200, &meta)
	if len(meta.Items) == 0 {
		t.Fatal(meta)
	}
	b, e := json.Marshal(meta)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "private projection text") {
		t.Fatal("metadata exposed body")
	}
	path := root + "/memories/" + content.Identity("private-source", actor, "statement") + "/body" + query
	runtimeHTTP(t, r, "GET", path, "", nil, 403, nil)
	if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.private_body.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	var legacy json.RawMessage
	runtimeHTTP(t, r, "GET", path, "", nil, 200, &legacy)
	if e = generated.Validate("Memory", legacy); e != nil {
		t.Fatalf("legacy default wire changed: %s: %v", legacy, e)
	}
	var projected json.RawMessage
	runtimeHTTP(t, r, "GET", path+"&include=provenance_v1", "", nil, 200, &projected)
	if e = generated.Validate("MemoryProvenanceV1", projected); e != nil {
		t.Fatal(string(projected), e)
	}
	var projection generated.MemoryProvenanceV1
	if e = json.Unmarshal(projected, &projection); e != nil {
		t.Fatal(e)
	}
	nested, e := json.Marshal(projection.Memory)
	if e != nil {
		t.Fatal(e)
	}
	var old, new any
	if e = json.Unmarshal(legacy, &old); e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(nested, &new); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(old, new) {
		t.Fatal("opt-in projection changed legacy memory fields")
	}
}

func TestMemoryInitialAndHistorySurviveActualMigration(t *testing.T) {
	// Root's FIX5 ruling uses real INITIAL, EXPERIENCE/INFERRED and an unrelated
	// RecordStatement/TOLD tombstone; it does not claim Mock produced the old trio.
	r, scope, task, item := actionResultFixture(t)
	ctx := runtimeContext(t, r)
	s := scope.ProductScope
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	joint, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	if e != nil || !joint.Committed || joint.TaskID != task || joint.OwnerSubjectID != s.SubjectID {
		t.Fatal("actual managed joint commit missing", joint, e)
	}
	if original, e := r.Store.QueryOriginalTurnCommit(ctx, scope, task); e != nil || original != joint {
		t.Fatal("joint receipt identity changed", original, e)
	}
	var operation, state, envelope string
	var holds, attempts, validations int
	if e = r.Store.DB.QueryRow("SELECT j.operation,t.state,h.state,(SELECT COUNT(*) FROM reservations WHERE task_id=?),(SELECT COUNT(*) FROM attempts WHERE task_id=?),(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?) FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task).Scan(&operation, &state, &envelope, &holds, &attempts, &validations); e != nil || operation != "TURN" || state != "COMMITTED" || envelope != "COMMITTED" || holds != 6 || attempts != 1 || validations != 1 {
		t.Fatal("history lacks real managed TURN closure", operation, state, envelope, holds, attempts, validations, e)
	}
	_, committed, e := r.Files.ReadApprovedTurn(ctx, scope, task)
	if e != nil || len(committed.MemoryIDs) == 0 || len(committed.EventIDs) == 0 || committed.ReceiptKey != joint.Key {
		t.Fatal("actual action and memory history absent", e)
	}
	svc := r.Memory
	ns := memory.Namespace{Scope: scope, DataSubjectID: s.SubjectID}
	const source, statementKey = "unrelated-migration-statement", "migration-statement-intake"
	if e = svc.RecordStatement(ctx, ns, source, statementKey, item.CustodianID, "Unrelated preference for a quiet table.", []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	nodeID := content.Identity(source, item.CustodianID, "statement")
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.govern','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	deleted, e := svc.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: item.CustodianID, PlayerID: s.SubjectID}, memory.MemoryGovernanceCommand{Key: "migrate-tombstone", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{nodeID}, Expected: map[string]uint64{nodeID: 1}}})
	if e != nil || deleted.Preservation != "CONFIRMED" || deleted.Cleanup != "LOCAL_COMPLETE" {
		t.Fatal("actual governed tombstone not complete", deleted, e)
	}
	var before []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, s.SubjectID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	var aggregate memory.Aggregate
	if e = json.Unmarshal(before, &aggregate); e != nil {
		t.Fatal(e)
	}
	tombstone, exists := aggregate.Nodes[nodeID]
	if !exists || !tombstone.Blocked || tombstone.Body != "" || tombstone.Revision != 2 || tombstone.ActorID != item.CustodianID {
		t.Fatal("deleted statement lost its actual cleaned tombstone")
	}
	initial, told := 0, 0
	inferred := map[string]bool{}
	for _, a := range aggregate.Acquisitions {
		if a.EvidenceID == "" || a.ActorID == "" || a.SourceID == "" {
			t.Fatal("acquisition lost its exact source or evidence identity")
		}
		if a.Kind == memory.Initial {
			initial++
		}
		if a.Kind == memory.Told && a.ActorID == item.CustodianID && a.SourceID == source && a.EvidenceID == content.Identity("statement", statementKey) {
			told++
		}
		if a.Kind == memory.Inferred && a.ActorID == item.CustodianID {
			inferred[a.EvidenceID] = true
		}
	}
	if initial == 0 || told != 1 {
		t.Fatal("real INITIAL or independent TOLD evidence absent", initial, told)
	}
	for _, id := range committed.MemoryIDs {
		n, ok := aggregate.Nodes[id]
		if !ok || n.Kind != "EXPERIENCE" || n.Audience != memory.Private || n.Domain != memory.ActorBelief || n.Body == "" || n.Blocked || len(n.Sources) == 0 || !inferred[id] {
			t.Fatal("actual committed private inferred history was not retained")
		}
	}
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
				t.Fatal("missing preserved world documents", spec.table, e)
			}
		}
		b, e := json.Marshal(out)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	i, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	i = runtimeControl(t, r, i, "PAUSE")
	if _, e = r.Auth.Authorize(ctx, "Bearer "+runtimeToken, s.ProductID, "runtime.migrate"); e != nil {
		t.Fatal("fixture lacks current migration authority", e)
	}
	target := nextRuntimeRelease(t, r, s, i.Versions.ReleaseID, "expression_style", "new presentation only")
	versionsBefore, documentsBefore := runtimeVersions(t, i), snapshot()
	var plan generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+scope.WorldInstanceID+"/migration-previews", "memory-preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &plan)
	h, e := content.Hash(plan)
	if e != nil {
		t.Fatal(e)
	}
	var migration generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+scope.WorldInstanceID+"/migrations", "memory-migration", generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: plan.PlanID, PlanHash: h, ExpectedVersions: i.Versions, ResolutionMap: plan.ResolutionMap}, 202, &migration)
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(documentsBefore, snapshot()) {
		t.Fatal("actual compatible migration changed actor/item/memory/acquisition/tombstone bytes")
	}
	after, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	v := runtimeVersions(t, after)
	if after.InstanceState != "PAUSED" || after.Versions.ReleaseID != string(target.ReleaseID) || v.WorldRevision != versionsBefore.WorldRevision+1 || v.ControlVersion != versionsBefore.ControlVersion+1 || v.ViewEpoch != versionsBefore.ViewEpoch+1 || v.PrivacyEpoch != versionsBefore.PrivacyEpoch {
		t.Fatal("migration did not switch the real paused release exactly once")
	}
	if e = r.Store.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", migration.TaskID).Scan(&state); e != nil || state != "COMMITTED" {
		t.Fatal("actual migration worker did not commit", state, e)
	}
	if original, e := r.Store.QueryOriginalTurnCommit(ctx, scope, task); e != nil || original != joint {
		t.Fatal("migration replaced original joint receipt", original, e)
	}
}

func TestMemoryCreationAtomicInitialEvidence(t *testing.T) {
	r, _, release := runtimeFixture(t)
	i := createRuntimeWorld(t, r, release, "atomic-initial")
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal("creation omitted atomic INITIAL memory", e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	if len(a.Acquisitions) == 0 {
		t.Fatal("creation omitted acquisition")
	}
	for _, v := range a.Acquisitions {
		if v.Kind != memory.Initial {
			t.Fatal(v)
		}
	}
}

func countSource(b memory.ContextBundle, id string) int {
	count := 0
	for _, n := range b.Records {
		for _, source := range n.Sources {
			if source == id {
				count++
			}
		}
	}
	return count
}

type failingInitialMemory struct{ world.InitialMemoryAdapter }

func (f failingInitialMemory) CreateInitial(ctx context.Context, tx store.Tx, s domain.Scope, i world.Initialization) error {
	if e := f.InitialMemoryAdapter.CreateInitial(ctx, tx, s, i); e != nil {
		return e
	}
	return errors.New("synthetic memory insertion failure")
}
func TestMemoryCreationFailureRollsBackAndReplayPreserves(t *testing.T) {
	r, s, release := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	real := r.World.InitialMemory
	r.World.InitialMemory = failingInitialMemory{real}
	command := world.CreateInstanceCommand{Key: "atomic-fail", Request: generated.AdminCreateInstanceRequest{ProductID: "coffee", ReleaseID: release, Mode: "PRIVATE", InitialParticipants: []string{}, ClientCapabilities: []string{"TEXT"}}}
	if _, e := r.World.CreateInstance(ctx, s, command); e == nil {
		t.Fatal("injected failure accepted")
	}
	id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, "createAdminInstance", command.Key)
	for _, table := range []string{"world_controls", "runtime_instances", "actors", "memory_aggregates"} {
		var count int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE world_instance_id=?", id).Scan(&count); e != nil || count != 0 {
			t.Fatal(table, count, e)
		}
	}
	var receipts int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE commit_key=?", id).Scan(&receipts); e != nil || receipts != 0 {
		t.Fatal("receipt survived failed memory insertion", receipts, e)
	}
	r.World.InitialMemory = real
	i := createRuntimeWorld(t, r, release, command.Key)
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", id).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	for id, node := range a.Nodes {
		node.Blocked = true
		a.Nodes[id] = node
	}
	a.Clean()
	blocked, e := json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", blocked, id); e != nil {
		t.Fatal(e)
	}
	var before []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", id).Scan(&before); e != nil {
		t.Fatal(e)
	}
	if again := createRuntimeWorld(t, r, release, command.Key); again != i {
		t.Fatal("original replay changed")
	}
	var after []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", id).Scan(&after); e != nil {
		t.Fatal(e)
	}
	if string(before) != string(after) {
		t.Fatal("creation replay reacquired blocked initial data")
	}
}

func TestMemoryGovernanceFreshRejectionUnknownAndRestoredReservation(t *testing.T) {
	for _, mode := range []string{"fresh", "prior-reservation", "commit-ack-lost"} {
		t.Run(mode, func(t *testing.T) {
			r, s, release := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "govern-proof"), "START")
			ctx := runtimeContext(t, r)
			svc := memory.New(r.Store, r.Local.Ledger)
			n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
			scope := domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: "alex", PlayerID: s.SubjectID}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.govern','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
				t.Fatal(e)
			}
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var a memory.Aggregate
			if e := json.Unmarshal(raw, &a); e != nil {
				t.Fatal(e)
			}
			node := ""
			for id, v := range a.Nodes {
				node = id
				scope.ActorInstanceID = v.ActorID
				break
			}
			c := memory.MemoryGovernanceCommand{Key: "rejection", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node}, Expected: map[string]uint64{node: 1}}}
			id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, i.Scope.WorldInstanceID, "MEMORY_GOVERNANCE", c.Key)
			if mode == "prior-reservation" {
				raw, _ := json.Marshal(struct {
					Namespace memory.Namespace
					ActorID   string
					Command   memory.MemoryGovernanceCommand
				}{n, scope.ActorInstanceID, c})
				hash := sha256.Sum256(raw)
				if e := r.Local.Ledger.ReserveOnce(ctx, memory.Reservation{ID: id, RequestHash: hex.EncodeToString(hash[:]), Scope: []string{s.TenantID, s.ProductID, i.Scope.WorldInstanceID, s.SubjectID, s.SubjectID}}); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "commit-ack-lost" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic ACK lost")
				}
			} else {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic definite rollback") }
			}
			if _, e := svc.Govern(ctx, scope, c); e == nil {
				t.Fatal("failure not surfaced")
			}
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			entry, e := r.Local.Ledger.Lookup(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "fresh" {
				if entry.Kind != "REJECTED" || r.Local.Ledger.Ready(ctx) != nil {
					t.Fatal("fresh definite rollback not sealed", entry.Kind)
				}
				if e = r.Local.Ledger.AppendOnce(ctx, id, memory.Binding{SourceEpoch: "epoch", SelectorHash: "hash"}); e == nil {
					t.Fatal("rejected reservation revived")
				}
			} else {
				if entry.Kind != "RESERVED" || r.Local.Ledger.Ready(ctx) == nil {
					t.Fatal("uncertain/restored absence sealed", entry.Kind)
				}
			}
			if mode == "commit-ack-lost" {
				receipt, e := svc.Govern(ctx, scope, c)
				if e != nil || receipt.Preservation != "CONFIRMED" {
					t.Fatal("original unknown recovery failed", receipt, e)
				}
			}
		})
	}
}
func TestMemoryDenyReplayRequiresDrainAndMissingReplacementStaysClosed(t *testing.T) {
	for _, action := range []string{"DELETE", "CORRECT"} {
		t.Run(action, func(t *testing.T) {
			r, s, release := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "replay-world"), "START")
			ctx := runtimeContext(t, r)
			svc := memory.New(r.Store, r.Local.Ledger)
			n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			for _, right := range []string{"memory.govern", "memory.correct"} {
				if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
					t.Fatal(e)
				}
			}
			var before []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&before); e != nil {
				t.Fatal(e)
			}
			var a memory.Aggregate
			if e := json.Unmarshal(before, &a); e != nil {
				t.Fatal(e)
			}
			node := ""
			actor := ""
			for id, v := range a.Nodes {
				node = id
				actor = v.ActorID
				break
			}
			scope := domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: actor, PlayerID: s.SubjectID}
			c := memory.MemoryGovernanceCommand{Key: "replay-command", Action: action, Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node}, Expected: map[string]uint64{node: 1}}}
			if action == "CORRECT" {
				c.Replacement = "replacement private text"
			}
			receipt, e := svc.Govern(ctx, scope, c)
			if e != nil {
				t.Fatal(e)
			}
			h, e := r.Local.Ledger.LatestHead(ctx, "replay-test")
			if e != nil {
				t.Fatal(e)
			}
			if e = svc.ReplayDeny(ctx, nil, h); e == nil {
				t.Fatal("nil drain proof accepted")
			}
			if e = r.Local.Coordinator.BeginClose(ctx, "restore"); e != nil {
				t.Fatal(e)
			}
			if e = r.Local.Coordinator.Drain(ctx, "restore"); e != nil {
				t.Fatal(e)
			}
			// Synthetic old-B aggregate restore, while the actual coordinator is CLOSED.
			if _, e = r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", before, i.Scope.WorldInstanceID); e != nil {
				t.Fatal(e)
			}
			permit, done, e := r.Local.Coordinator.AcquireReplay(ctx, "restore")
			if e != nil {
				t.Fatal(e)
			}
			defer done()
			e = svc.ReplayDeny(ctx, permit, h)
			var after []byte
			if x := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&after); x != nil {
				t.Fatal(x)
			}
			var restored memory.Aggregate
			if x := json.Unmarshal(after, &restored); x != nil {
				t.Fatal(x)
			}
			if !restored.Nodes[node].Blocked || restored.Nodes[node].Body != "" || restored.Epoch == a.Epoch {
				t.Fatal("old B negative evidence not replayed")
			}
			candidate, sealError := svc.SealReplayOpenCandidate(ctx, permit)
			if action == "CORRECT" {
				if e == nil || sealError == nil || candidate != nil {
					t.Fatal("missing replacement permitted candidate")
				}
				var state string
				if x := r.Store.DB.QueryRow("SELECT state FROM memory_replay WHERE task_id=?", receipt.TaskID).Scan(&state); x != nil || state != "BLOCKED_NEEDS_REPLACEMENT" {
					t.Fatal(state, x)
				}
			} else if e != nil || sealError != nil || candidate == nil {
				t.Fatal(e, sealError)
			} else {
				raw, x := os.ReadFile(filepath.Join(r.Local.Coordinator.Directory(), "coordinator.json"))
				if x != nil {
					t.Fatal(x)
				}
				var durable struct{ Governance *struct{ CandidateID string } }
				if json.Unmarshal(raw, &durable) != nil || durable.Governance == nil || durable.Governance.CandidateID == "" {
					t.Fatal("G candidate was not durable before return")
				}
			}
			if r.Local.Participant.Check(ctx) == nil {
				t.Fatal("G candidate reopened old runtime")
			}
		})
	}
}

func TestDeleteBlocksActualDerivedBytesAndPreservesIndependentAsset(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "actual-media"), "START")
	ctx := runtimeContext(t, r)
	svc := r.Memory
	n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	for _, right := range []string{"memory.private_body.read", "memory.govern"} {
		if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
			t.Fatal(e)
		}
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	var node memory.MemoryRecord
	for _, v := range a.Nodes {
		node = v
		break
	}
	id, e := svc.RenderPrivateText(ctx, n, node.ActorID, node.ID, "derive")
	if e != nil {
		t.Fatal(e)
	}
	opened, e := r.Files.OpenAuthorized(ctx, s, id)
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(opened)
	opened.Close()
	if e != nil || string(body) != node.Body {
		t.Fatal("server-derived bytes mismatch", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.private_body.read'"); e != nil {
		t.Fatal(e)
	}
	if reader, e := r.Files.OpenAuthorized(ctx, s, id); e == nil {
		reader.Close()
		t.Fatal("product asset permission bypassed private-body authority")
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=1 WHERE action='memory.private_body.read'"); e != nil {
		t.Fatal(e)
	}
	independentBody := "lawful independent asset"
	hash := sha256.Sum256([]byte(independentBody))
	meta := generated.AssetUploadMetadata{ProductID: "coffee", Purpose: "KNOWLEDGE", Name: "independent.txt", DeclaredType: "text/plain", ExpectedSize: generated.ExactUnsigned(strconv.Itoa(len(independentBody))), ExpectedSha256: hex.EncodeToString(hash[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "independent", License: "synthetic", AllowedUses: []string{"EDIT"}}}
	independent, e := r.Files.Upload(ctx, s, strings.NewReader(independentBody), meta, "independent")
	if e != nil {
		t.Fatal(e)
	}
	var key string
	if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", id).Scan(&key); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, key+".blob")
	svc.Files = nil
	c := memory.MemoryGovernanceCommand{Key: "delete-media", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node.ID}, Expected: map[string]uint64{node.ID: 1}}}
	scope := domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: node.ActorID, PlayerID: s.SubjectID}
	receipt, e := svc.Govern(ctx, scope, c)
	if !errors.Is(e, memory.ErrPreservation) {
		t.Fatal("missing purge capability did not preserve pending", e)
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("fixture bytes missing before purge", e)
	}
	if reader, e := r.Files.OpenAuthorized(ctx, s, id); e == nil {
		reader.Close()
		t.Fatal("G1 blocked bytes still readable")
	}
	svc.Files = r.Files
	if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", receipt.TaskID); e != nil {
		t.Fatal(e)
	}
	receipt, e = svc.Govern(ctx, scope, c)
	if e != nil || receipt.Cleanup != "LOCAL_COMPLETE" {
		t.Fatal(receipt, e)
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("derived file bytes not removed", e)
	}
	reader, e := r.Files.OpenAuthorized(ctx, s, string(independent.AssetID))
	if e != nil {
		t.Fatal("lawful independent asset purged", e)
	}
	defer reader.Close()
	body, e = io.ReadAll(reader)
	if e != nil || string(body) != independentBody {
		t.Fatal("independent bytes changed", e)
	}
}

// Test-only wrapper keeps actual admission and pauses Process at PurgeDerived,
// after earlier assets were inspected, without a DB or filesystem lock held.
type cleanupPauseKey struct{}
type cleanupPauseGuard struct {
	store.OperationGuard
	entered, resume chan struct{}
	calls           int
}

func (g *cleanupPauseGuard) EnterStore(ctx context.Context) (context.Context, func(), error) {
	ctx, done, e := g.OperationGuard.EnterStore(ctx)
	if e != nil {
		return ctx, done, e
	}
	if ctx.Value(cleanupPauseKey{}) != nil {
		g.calls++
		if g.calls == 2 {
			close(g.entered)
			select {
			case <-g.resume:
			case <-ctx.Done():
				done()
				return ctx, nil, ctx.Err()
			}
		}
	}
	return ctx, done, nil
}

func TestMemoryCleanupReplansWhenAnotherGovernanceAddsIntent(t *testing.T) {
	for _, mode := range []string{"new-intent", "asset-growth"} {
		t.Run(mode, func(t *testing.T) {
			r, s, release := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "cleanup-overlap"), "START")
			ctx, cancel := context.WithTimeout(runtimeContext(t, r), 30*time.Second)
			defer cancel()
			n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			for _, right := range []string{"memory.private_body.read", "memory.govern"} {
				if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
					t.Fatal(e)
				}
			}
			var actor string
			if e := r.Store.DB.QueryRow("SELECT actor_id FROM actors WHERE world_instance_id=? LIMIT 1", n.WorldInstanceID).Scan(&actor); e != nil {
				t.Fatal(e)
			}
			scope := domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: actor, PlayerID: s.SubjectID}
			type media struct{ node, id, storage, blob string }
			var pair []media
			for _, source := range []string{"independent-a", "independent-b"} {
				if e := r.Memory.RecordStatement(ctx, n, source, source, actor, "private "+source, []memory.Use{memory.PrivateResponse}); e != nil {
					t.Fatal(e)
				}
				bundle, e := r.Memory.AllowedContext(ctx, scope, actor, memory.Private)
				if e != nil {
					t.Fatal(e)
				}
				var node string
				for _, v := range bundle.Records {
					if len(v.Sources) == 1 && v.Sources[0] == source {
						node = v.ID
					}
				}
				if node == "" {
					t.Fatal("missing independent acquired node")
				}
				id, e := r.Memory.RenderPrivateText(ctx, n, actor, node, "render-"+source)
				if e != nil {
					t.Fatal(e)
				}
				m := media{node: node, id: id}
				if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", id).Scan(&m.storage); e != nil {
					t.Fatal(e)
				}
				m.blob = filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, m.storage+".blob")
				pair = append(pair, m)
			}
			// assetLocks sorts storage keys. B is skipped before A enters its purge.
			a, b := pair[0], pair[1]
			if a.storage < b.storage {
				a, b = b, a
			}
			command := func(m media, key string) memory.MemoryGovernanceCommand {
				return memory.MemoryGovernanceCommand{Key: key, Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{m.node}, Expected: map[string]uint64{m.node: 1}}}
			}
			pendingService := memory.New(r.Store, r.Local.Ledger) // real G1, deferred cleanup without Files
			receiptA, e := pendingService.Govern(ctx, scope, command(a, "delete-a"))
			if !errors.Is(e, memory.ErrPreservation) {
				t.Fatal(receiptA, e)
			}
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", receiptA.TaskID); e != nil {
				t.Fatal(e)
			}
			lease, e := r.Store.ClaimRegistered(ctx, "resumed-a", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "GOVERNANCE", Operation: "MEMORY_GOVERNANCE", TaskID: receiptA.TaskID}})
			if e != nil {
				t.Fatal(e)
			}
			guard := &cleanupPauseGuard{OperationGuard: r.Store.Guard, entered: make(chan struct{}), resume: make(chan struct{})}
			r.Store.Guard = guard
			var unblock sync.Once
			defer unblock.Do(func() { close(guard.resume) })
			finished := make(chan error, 1)
			go func() {
				finished <- r.Memory.Process(context.WithValue(ctx, cleanupPauseKey{}, true), lease, "resumed-a")
			}()
			select {
			case <-guard.entered:
			case <-ctx.Done():
				t.Fatal("worker did not pause after skipping B", ctx.Err())
			}
			var receiptB memory.GovernanceReceipt
			if mode == "new-intent" {
				receiptB, e = pendingService.Govern(ctx, scope, command(b, "delete-b"))
				if !errors.Is(e, memory.ErrPreservation) {
					t.Fatal(receiptB, e)
				}
			} else {
				// No competing intent yet: a lawful new lineage alone invalidates the
				// frozen asset set, even though the already-purged A is sufficient so far.
				id, x := r.Memory.RenderPrivateText(ctx, n, actor, b.node, "concurrent-growth")
				if x != nil {
					t.Fatal(x)
				}
				m := media{node: b.node, id: id}
				if x = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", id).Scan(&m.storage); x != nil {
					t.Fatal(x)
				}
				pair = append(pair, m)
			}
			unblock.Do(func() { close(guard.resume) })
			processError := <-finished
			var purged bool
			if e = r.Store.DB.QueryRow("SELECT purged FROM memory_asset_lineage WHERE asset_id=?", b.id).Scan(&purged); e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(b.blob); e != nil {
				t.Fatal("B bytes must remain until its actual purge", e)
			}
			if purged {
				t.Fatal("A acknowledged skipped B after competing G1; private B bytes still exist")
			}
			if !errors.Is(processError, store.ErrReplan) {
				t.Fatal("new cleanup obligation must replan A", processError)
			}
			for _, receipt := range []memory.GovernanceReceipt{receiptA, receiptB} {
				if receipt.TaskID == "" {
					continue
				}
				status, e := r.Memory.Status(ctx, n, receipt.TaskID)
				if e != nil || status.Cleanup == "LOCAL_COMPLETE" {
					t.Fatal("cleanup completed before all current bytes were purged", status, e)
				}
			}
			if receiptB.TaskID == "" {
				receiptB, e = pendingService.Govern(ctx, scope, command(b, "delete-b"))
				if !errors.Is(e, memory.ErrPreservation) {
					t.Fatal(receiptB, e)
				}
			}
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", receiptB.TaskID); e != nil {
				t.Fatal(e)
			}
			complete, e := r.Memory.Govern(ctx, scope, command(b, "delete-b"))
			if e != nil || complete.Cleanup != "LOCAL_COMPLETE" || complete.TaskID != receiptB.TaskID {
				t.Fatal(complete, e)
			}
			for _, m := range pair {
				for _, ext := range []string{".blob", ".part"} {
					if _, e = os.Stat(filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, m.storage+ext)); !os.IsNotExist(e) {
						t.Fatal("completed cleanup retained private bytes", m.id, ext, e)
					}
				}
			}
			if e = r.Memory.Process(ctx, lease, "resumed-a"); e != nil {
				t.Fatal("original A retry failed", e)
			}
			status, e := r.Memory.Status(ctx, n, receiptA.TaskID)
			if e != nil || status.Cleanup != "LOCAL_COMPLETE" {
				t.Fatal("original A failed to finish after confirmed B purge", status, e)
			}
		})
	}
}

func TestMemoryDerivedFinalizePurgeOSInterleaving(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "file-interleave"), "START")
	ctx := runtimeContext(t, r)
	if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.private_body.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	var node memory.MemoryRecord
	for _, v := range a.Nodes {
		node = v
		break
	}
	n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	id, e := r.Memory.RenderPrivateText(ctx, n, node.ActorID, node.ID, "render-interleave")
	if e != nil {
		t.Fatal(e)
	}
	var storage string
	if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", id).Scan(&storage); e != nil {
		t.Fatal(e)
	}
	blob := filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, storage+".blob")
	part := filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, storage+".part")
	if e = os.Rename(blob, part); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='PENDING' WHERE asset_id=?", id); e != nil {
		t.Fatal(e)
	}
	stage, e := r.Files.RecoverDerived(s, storage, node.Body)
	if e != nil {
		t.Fatal(e)
	}
	lock, e := localstate.Open(filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, ".memory-locks", storage), false)
	if e != nil {
		t.Fatal(e)
	}
	releaseLock, e := lock.Lock(ctx, "operations.lock")
	if e != nil {
		t.Fatal(e)
	}
	defer releaseLock()
	finalized := make(chan error, 1)
	go func() { finalized <- r.Files.FinalizeDerived(ctx, s, id, stage) }()
	select {
	case e := <-finalized:
		t.Fatal("finalizer bypassed OS mutation lock", e)
	case <-time.After(30 * time.Millisecond):
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_asset_lineage SET delete_intent=1 WHERE asset_id=?", id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", id); e != nil {
		t.Fatal(e)
	}
	purged := make(chan error, 1)
	go func() { purged <- r.Files.PurgeDerived(ctx, s, storage) }()
	select {
	case e := <-purged:
		t.Fatal("purger bypassed OS mutation lock", e)
	case <-time.After(30 * time.Millisecond):
	}
	releaseLock()
	if e = <-finalized; e == nil {
		t.Fatal("late finalizer ignored durable intent")
	}
	if e = <-purged; e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{part, blob} {
		if _, e = os.Stat(path); !os.IsNotExist(e) {
			t.Fatal("rename/purge race left private bytes", path, e)
		}
	}
}
func TestMemoryWitnessAndActorRelationshipUseActualCommittedScope(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "actor-pair"), "START")
	ctx := runtimeContext(t, r)
	n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	br, q := actionTask(t, r, s, i, "pair-turn")
	actor, target := q.Intents[0].ActorID, q.Intents[0].TargetID
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	parent := ""
	for id, v := range a.Nodes {
		if v.ActorID == actor {
			parent = id
			break
		}
	}
	br.Locks = append(br.Locks, n.Locks()...)
	_, e := r.Store.WithBarrier(ctx, br, func(tx store.Tx) error {
		if _, e := world.ApplyActions(ctx, tx, q); e != nil {
			return e
		}
		_, e := memory.ApplyUpdates(tx, memory.MemoryUpdateRequest{Namespace: n, TaskID: q.TaskID, Nodes: []memory.MemoryRecord{{ID: "actual-pair", ActorID: actor, TargetKind: "ACTOR", TargetID: target, Kind: "RELATIONSHIP", Domain: memory.ActorBelief, Audience: memory.Private, Uses: []memory.Use{memory.PrivateResponse}, Parents: []string{parent}, Body: "private actor relationship", Revision: 1}}})
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.private_body.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	pair := domain.ActorRelationshipScope{Scope: n.Scope, ActorInstanceID: actor, TargetActorInstanceID: target}
	bundle, e := r.Memory.AllowedActorContext(ctx, pair, s.SubjectID, memory.Private)
	if e != nil || len(bundle.Records) != 1 {
		t.Fatal("actual actor pair unavailable", bundle, e)
	}
	pair.ActorInstanceID, pair.TargetActorInstanceID = target, actor
	bundle, e = r.Memory.AllowedActorContext(ctx, pair, s.SubjectID, memory.Private)
	if e != nil || len(bundle.Records) != 0 {
		t.Fatal("reverse actor pair leaked", bundle, e)
	}
	if e = r.Memory.WitnessCommitted(ctx, n, q.ResultID, actor, "witness"); e != nil {
		t.Fatal(e)
	}
	if e = r.Memory.WitnessCommitted(ctx, n, "invented-event", actor, "invented"); e == nil {
		t.Fatal("invented event became WORLD_FACT")
	}
	if e = r.Memory.WitnessCommitted(ctx, n, q.ResultID, "invented-actor", "outsider"); e == nil {
		t.Fatal("nonparticipant witnessed event")
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	witness, inferred := false, false
	for _, acq := range a.Acquisitions {
		if acq.Kind == memory.Witnessed {
			witness = true
			if a.Sources[acq.SourceID].Domain != memory.WorldFact {
				t.Fatal("committed event domain lost")
			}
		}
		if acq.Kind == memory.Inferred && acq.EvidenceID == "actual-pair" {
			inferred = true
		}
	}
	if !witness || !inferred {
		t.Fatal("typed acquisition missing", witness, inferred)
	}
}

func TestMemoryHTTPUnknownReturnsOriginalRecoveryAndNeverNewKey(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(strconv.FormatBool(policy), func(t *testing.T) {
			r, s, release := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "http-unknown"), "START")
			ctx := runtimeContext(t, r)
			for _, right := range []string{"memory.govern", "memory.policy"} {
				if _, e := r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner',?)", right); e != nil {
					t.Fatal(e)
				}
				if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var a memory.Aggregate
			if e := json.Unmarshal(raw, &a); e != nil {
				t.Fatal(e)
			}
			var node memory.MemoryRecord
			for _, v := range a.Nodes {
				node = v
				break
			}
			path := "/world/v1/admin/instances/" + i.Scope.WorldInstanceID + "/memory-governance"
			request := map[string]any{"product_id": "coffee", "data_subject_id": "creator", "actor_instance_id": node.ActorID, "command": map[string]any{"operation": "DELETE", "memory_ids": []string{node.ID}, "expected_revisions": map[string]string{node.ID: "1"}}}
			if policy {
				path = "/world/v1/admin/instances/" + i.Scope.WorldInstanceID + "/memory-use-policy"
				request = map[string]any{"product_id": "coffee", "data_subject_id": "creator", "actor_instance_id": node.ActorID, "operation": "SET_USE_POLICY", "action": "DISABLE", "uses": []string{"PRIVATE_RESPONSE"}, "expected_policy_revision": "1"}
			}
			invoke := func(method, path, key string) *httptest.ResponseRecorder {
				b, _ := json.Marshal(request)
				q := httptest.NewRequest(method, path, bytes.NewReader(b))
				q.Header.Set("Authorization", "Bearer "+runtimeToken)
				q.Header.Set("Content-Type", "application/json")
				q.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				r.Handler().ServeHTTP(w, q)
				return w
			}
			r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
				if e := tx.Commit(); e != nil {
					return e
				}
				return errors.New("synthetic ACK lost")
			}
			w := invoke("POST", path, "original")
			r.Store.Commit = nil
			if w.Code != 503 {
				t.Fatal("unknown status", w.Code, w.Body.String())
			}
			if e := generated.Validate("Error", w.Body.Bytes()); e != nil {
				t.Fatal("legacy error wire", e)
			}
			location := w.Header().Get("Operation-Location")
			if location == "" || location != w.Header().Get("Governance-Location") || !strings.Contains(w.Body.String(), "QUERY_ORIGINAL_TASK") {
				t.Fatal("original safe recovery locator missing", w.Header(), w.Body.String())
			}
			status := invoke("GET", location, "")
			if status.Code != 200 {
				t.Fatal("original lookup unavailable", status.Code, status.Body.String())
			}
			if w = invoke("POST", path, "new-key"); w.Code == 202 {
				t.Fatal("new intake bypassed unresolved reservation")
			}
			if w = invoke("POST", path, "original"); w.Code != 202 {
				t.Fatal("same command blocked by its own unresolved reservation", w.Code, w.Body.String())
			}
			if e := r.Local.Ledger.Ready(ctx); e != nil {
				t.Fatal(e)
			}
			if e := r.Local.Coordinator.BeginClose(ctx, "close"); e != nil {
				t.Fatal(e)
			}
			if e := r.Local.Coordinator.Drain(ctx, "close"); e != nil {
				t.Fatal(e)
			}
			if w = invoke("POST", path, "original"); w.Code == 202 {
				t.Fatal("recovery bypassed current maintenance closure")
			}
			_ = s
		})
	}
}

func TestMemoryPauseAndGovernanceRaceAdmittedWorker(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "concurrent-worker"), "START")
	ctx := runtimeContext(t, r)
	br, q := actionTask(t, r, s, i, "waiting-worker")
	admitted, releasePermit, e := r.Store.EnterOperation(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer releasePermit()
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.govern','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	var node memory.MemoryRecord
	for _, v := range a.Nodes {
		node = v
		break
	}
	// An admitted worker is blocked outside SQL. PAUSE and source governance must
	// still enter, complete, and invalidate its old versions before late commit.
	i = runtimeControl(t, r, i, "PAUSE")
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	scope := domain.RelationshipScope{Scope: *br.WorldScope, ActorInstanceID: node.ActorID, PlayerID: s.SubjectID}
	receipt, e := r.Memory.Govern(short, scope, memory.MemoryGovernanceCommand{Key: "while-worker", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node.ID}, Expected: map[string]uint64{node.ID: 1}}})
	if e != nil || receipt.Preservation != "CONFIRMED" {
		t.Fatal("governance serialized behind worker", receipt, e)
	}
	_, e = r.Store.WithBarrier(admitted, br, func(tx store.Tx) error { _, e := world.ApplyActions(admitted, tx, q); return e })
	if e == nil {
		t.Fatal("old admitted worker ignored current PAUSE/privacy versions")
	}
	releasePermit()
}

func TestMemoryInitialKnowledgeUsesQualifiedCharacterAndSetupReleases(t *testing.T) {
	r, s, seed := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	base, e := r.Content.GetRelease(ctx, s, seed)
	if e != nil {
		t.Fatal(e)
	}
	var raw json.RawMessage
	for _, b := range base.Input.Objects {
		var h struct {
			ObjectID string `json:"object_id"`
		}
		_ = json.Unmarshal(b, &h)
		if h.ObjectID == "alex-character" {
			raw = b
		}
	}
	knowledge := func(body string) generated.ContentObject {
		var common generated.KnowledgeDefinition
		if e := json.Unmarshal(raw, &common); e != nil {
			t.Fatal(e)
		}
		common.Kind = "KNOWLEDGE"
		common.ObjectID = "shared-knowledge"
		common.KnowledgeType = "NOTE"
		common.Text = body
		common.BindingRefs = []generated.DefinitionRef{}
		b, e := json.Marshal(common)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	refs := []json.RawMessage{}
	locks := []generated.DependencyLock{}
	for _, version := range []string{"1", "2"} {
		var char map[string]any
		_ = json.Unmarshal(raw, &char)
		char["version"] = version
		char["initial_knowledge_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "shared-knowledge"}}
		char["expression_style"] = "version " + version
		object, e := json.Marshal(char)
		if e != nil {
			t.Fatal(e)
		}
		in := base.Input
		in.RootObjectID = "alex-character"
		in.Objects = []generated.ContentObject{object, knowledge("character release " + version)}
		in.DependencyLocks = []generated.DependencyLock{}
		d, e := r.Content.Create(ctx, s, "CHARACTER", "", in, "char-"+version)
		if e != nil {
			t.Fatal(e)
		}
		check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "char-check-"+version)
		if e != nil || !check.Passed {
			t.Fatal(check, e)
		}
		rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "char-pub-"+version)
		if e != nil {
			t.Fatal(e)
		}
		hash, e := content.Hash(char)
		if e != nil {
			t.Fatal(e)
		}
		ref, e := json.Marshal(map[string]any{"kind": "PINNED_DEFINITION", "release_id": rel.ReleaseID, "object_id": "alex-character", "version": version, "content_hash": hash})
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
		locks = append(locks, generated.DependencyLock{ReleaseID: rel.ReleaseID, ObjectID: "alex-character", Version: generated.ExactUnsigned(version), ContentHash: hash})
	}
	in := base.Input
	in.Objects = []generated.ContentObject{knowledge("world setup local")}
	in.DependencyLocks = locks
	for _, b := range base.Input.Objects {
		var h struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(b, &h)
		if h.Kind == "CHARACTER" {
			continue
		}
		if h.Kind == "WORLD" {
			var w generated.WorldDefinition
			if e = generated.Decode("WorldDefinition", b, &w); e != nil {
				t.Fatal(e)
			}
			w.InitialActorSetups[0].KnowledgeRefs = []generated.DefinitionRef{json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"shared-knowledge"}`)}
			w.InitialActorSetups[1].KnowledgeRefs = w.InitialActorSetups[0].KnowledgeRefs
			w.InitialActorSetups[0].CharacterRef = refs[0]
			w.InitialActorSetups[1].CharacterRef = refs[1]
			b, e = json.Marshal(w)
			if e != nil {
				t.Fatal(e)
			}
		}
		in.Objects = append(in.Objects, b)
	}
	d, e := r.Content.Create(ctx, s, "WORLD", "", in, "pinned-world")
	if e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "pinned-world-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-world-pub")
	if e != nil {
		t.Fatal(e)
	}
	i := createRuntimeWorld(t, r, string(rel.ReleaseID), "pinned-create")
	var initial []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&initial); e != nil {
		t.Fatal(e)
	}
	var init world.Initialization
	if e = json.Unmarshal(initial, &init); e != nil {
		t.Fatal(e)
	}
	if len(init.Actors) != 2 || init.Actors[0].ID == init.Actors[1].ID || string(init.Actors[0].OwnerReference) == string(init.Actors[1].OwnerReference) || init.Actors[0].OriginalCharacter.Version != "1" || init.Actors[1].OriginalCharacter.Version != "2" {
		t.Fatal("pinned versions collapsed", init.Actors)
	}
	var memoryRaw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&memoryRaw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e = json.Unmarshal(memoryRaw, &a); e != nil {
		t.Fatal(e)
	}
	for _, actor := range init.Actors {
		bodies := map[string]bool{}
		sources := map[string]bool{}
		for _, v := range a.Allowed(actor.ID, memory.Private, memory.PrivateResponse, time.Now()) {
			bodies[v.Body] = true
			for _, source := range v.Sources {
				sources[source] = true
			}
		}
		if !bodies["world setup local"] || !bodies["character release "+string(actor.OriginalCharacter.Version)] {
			t.Fatal("LOCAL_DRAFT knowledge resolved under wrong release", actor.Slot, bodies)
		}
		other := "1"
		if actor.OriginalCharacter.Version == "1" {
			other = "2"
		}
		if bodies["character release "+other] {
			t.Fatal("same object ID crossed character releases")
		}
	}
}

func TestMemoryOldDatabaseAndPrivateFileReplayPurgesBeforeSeal(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "file-restore"), "START")
	ctx := runtimeContext(t, r)
	n := memory.Namespace{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, DataSubjectID: s.SubjectID}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	for _, right := range []string{"memory.govern", "memory.private_body.read"} {
		if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", right); e != nil {
			t.Fatal(e)
		}
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	var node memory.MemoryRecord
	for _, v := range a.Nodes {
		node = v
		break
	}
	id, e := r.Memory.RenderPrivateText(ctx, n, node.ActorID, node.ID, "restore-render")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var key string
	if e = r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", id).Scan(&key); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(r.Files.RootPath, s.TenantID, s.ProductID, key+".blob")
	oldBytes, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	scope := domain.RelationshipScope{Scope: n.Scope, ActorInstanceID: node.ActorID, PlayerID: s.SubjectID}
	if _, e = r.Memory.Govern(ctx, scope, memory.MemoryGovernanceCommand{Key: "erase-before-restore", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node.ID}, Expected: map[string]uint64{node.ID: 1}}}); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Coordinator.BeginClose(ctx, "file-restore-plan"); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Coordinator.Drain(ctx, "file-restore-plan"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, i.Scope.WorldInstanceID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_asset_lineage SET delete_intent=0,purged=0 WHERE asset_id=?", id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='READY' WHERE asset_id=?", id); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, oldBytes, 0600); e != nil {
		t.Fatal(e)
	}
	p, done, e := r.Local.Coordinator.AcquireReplay(ctx, "file-restore-plan")
	if e != nil {
		t.Fatal(e)
	}
	defer done()
	h, e := r.Local.Ledger.LatestHead(ctx, "restore-files")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Memory.ReplayDeny(ctx, p, h); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("restored private bytes survived deny replay", e)
	}
	candidate, e := r.Memory.SealReplayOpenCandidate(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Memory.BindReplayOpenCandidate(ctx, p, candidate)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Memory.ConfirmGovernanceReady(ctx, p, candidate, receipt); e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Ledger.ReserveOnce(ctx, memory.Reservation{ID: "changed-head", RequestHash: "hash", Scope: []string{s.TenantID, s.ProductID, i.Scope.WorldInstanceID, s.SubjectID, s.SubjectID}}); !errors.Is(e, maintenance.ErrMaintenance) {
		t.Fatal("ordinary append must reject CLOSED replay admission", e)
	}
	if e = r.Memory.ConfirmGovernanceReady(ctx, p, candidate, receipt); e != nil {
		t.Fatal("rejected ordinary append changed retained seal", e)
	}
}

func TestMemorySameOriginalIdentityOverlapAcceptsOnce(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "overlap"), "START")
	ctx := runtimeContext(t, r)
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.govern','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e := json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	var node memory.MemoryRecord
	for _, v := range a.Nodes {
		node = v
		break
	}
	scope := domain.RelationshipScope{Scope: domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}, ActorInstanceID: node.ActorID, PlayerID: s.SubjectID}
	command := memory.MemoryGovernanceCommand{Key: "same-original", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{node.ID}, Expected: map[string]uint64{node.ID: 1}}}
	entered, continueCommit := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { once.Do(func() { close(entered); <-continueCommit }); return nil }
	type outcome struct {
		receipt memory.GovernanceReceipt
		e       error
	}
	done := make(chan outcome, 2)
	go func() { v, e := r.Memory.Govern(ctx, scope, command); done <- outcome{v, e} }()
	<-entered
	go func() { v, e := r.Memory.Govern(ctx, scope, command); done <- outcome{v, e} }()
	select {
	case result := <-done:
		t.Fatal("overlap bypassed original intake lock", result)
	case <-time.After(40 * time.Millisecond):
	}
	close(continueCommit)
	first, second := <-done, <-done
	r.Store.BeforeCommit = nil
	if first.e != nil || second.e != nil || first.receipt != second.receipt {
		t.Fatal(first, second)
	}
	h, e := r.Local.Ledger.LatestHead(ctx, "same-identity")
	if e != nil || h.Sequence != 2 {
		t.Fatal("same command reserved/appended more than once", h, e)
	}
	var tasks int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_kind='GOVERNANCE'").Scan(&tasks); e != nil || tasks != 1 {
		t.Fatal(tasks, e)
	}
}
