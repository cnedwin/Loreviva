package integration

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"

	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net"
	"net/http"
	"net/http/httptest"

	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationDeliverySnapshotWatermark(t *testing.T) {
	f := integrationFixSetup(t, true)
	if _, e := f.r.Store.ReadPlayerSelfConsent(f.client); e != nil {
		t.Fatal("real current CLIENT/consent control", e)
	}
	h, e := f.r.Store.BeginIntegrationRead(f.client, f.world.WorldInstanceID, "SNAPSHOT")
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	v, e := h.Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, v.Cursor)
	if len(v.Actors) == 0 || len(v.State) == 0 {
		t.Fatal("lawful PUBLIC actor/item presence is empty")
	}
	if len(v.Relationships) != 0 {
		t.Fatal("unqualified relationship leaked")
	}
	for _, a := range v.Actors {
		if a.ActorInstanceID == "s66-player" || a.CharacterDefinitionID == "" || a.Revision != v.Versions.WorldRevision || len(a.VisibleState) != 0 {
			t.Fatal("presence identity/revision/whitelist")
		}
	}
	b, e := json.Marshal(v)
	if e != nil || generated.Validate("Snapshot", b) != nil {
		t.Fatal("actual complete Snapshot wire invalid")
	}
	if e = h.Finish(f.client); e != nil {
		t.Fatal(e)
	}
	if _, e = h.Snapshot(); e == nil {
		t.Fatal("closed read reused")
	}
	t.Run("holder-actor-whitelist-owned-state-fault", func(t *testing.T) {
		var id string
		var original []byte
		if e := f.r.Store.DB.QueryRow("SELECT item_id,document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? ORDER BY item_id LIMIT 1", f.world.TenantID, f.world.ProductID, f.world.WorldInstanceID).Scan(&id, &original); e != nil {
			t.Fatal(e)
		}
		var item map[string]any
		if e := json.Unmarshal(original, &item); e != nil {
			t.Fatal(e)
		}
		actor, ok := item["CustodianID"].(string)
		if !ok || actor == "" || actor == "s66-player" {
			t.Fatal("real distinct actor prerequisite")
		}
		visible := false
		for _, a := range v.Actors {
			visible = visible || a.ActorInstanceID == actor
		}
		if !visible {
			t.Fatal("real same-scene actor absent")
		}
		write := func(raw []byte) {
			t.Helper()
			res, e := f.r.Store.DB.Exec("UPDATE items SET document=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", raw, f.world.TenantID, f.world.ProductID, f.world.WorldInstanceID, id)
			if e != nil {
				t.Fatal("owned state injection", e)
			}
			if n, e := res.RowsAffected(); e != nil || n != 1 {
				t.Fatal("owned exact item update", n, e)
			}
		}
		defer func() {
			res, e := f.r.Store.DB.Exec("UPDATE items SET document=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", original, f.world.TenantID, f.world.ProductID, f.world.WorldInstanceID, id)
			if e != nil {
				t.Error("restore item", e)
				return
			}
			if n, e := res.RowsAffected(); e != nil || n > 1 {
				t.Error("restore row count", n, e)
			}
			var restored []byte
			if e := f.r.Store.DB.QueryRow("SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", f.world.TenantID, f.world.ProductID, f.world.WorldInstanceID, id).Scan(&restored); e != nil || !bytes.Equal(original, restored) {
				t.Error("original item restoration", e)
			}
		}()
		// These are bounded state faults, not claims that an owning action moved
		// the item. A player identity must never be treated as an actor identity.
		for _, holder := range []string{actor, "s66-player", ""} {
			item["HolderID"] = holder
			raw, e := json.Marshal(item)
			if e != nil {
				t.Fatal(e)
			}
			write(raw)
			snapshot, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
			if e != nil {
				t.Fatal("holder filter stage", e)
			}
			retainSessionScanMaterial(t, snapshot.Cursor)
			items := map[string]map[string]string{}
			if raw, ok := snapshot.State["items"]; ok {
				if e := json.Unmarshal(raw, &items); e != nil {
					t.Fatal(e)
				}
			}
			projected, exists := items[id]
			if holder == "s66-player" {
				if exists {
					t.Fatal("nonactor holder leaked entire item")
				}
				continue
			}
			if !exists || projected["custodian_actor_id"] != actor || projected["holder_actor_id"] != holder {
				t.Fatal("same-scene holder projection")
			}
			want := 2
			if holder != "" {
				want++
			}
			if len(projected) != want {
				t.Fatal("unqualified item attributes escaped")
			}
		}
	})
	t.Run("free-plan-unit-rebind", func(t *testing.T) {
		defer func() {
			if recover() != nil {
				t.Error("free typed plan must reject without panic")
			}
		}()
		if _, e := new(store.IntegrationSnapshotPlan).Facts(nil); e == nil {
			t.Fatal("free plan accepted")
		}
		if _, e := new(store.IntegrationSnapshotUnit).Facts(f.client, f.r.World); e == nil {
			t.Fatal("free unit accepted")
		}
		if _, e := f.r.World.SnapshotTx(f.client, new(store.IntegrationSnapshotUnit)); e == nil {
			t.Fatal("free unit projected")
		}
		if e := f.r.Store.BindDeliverySnapshotOwner(f.r.World); e == nil {
			t.Fatal("owner rebound")
		}
		if _, e := store.New(f.r.Store.DB).BeginIntegrationRead(f.client, f.world.WorldInstanceID, "SNAPSHOT"); e == nil {
			t.Fatal("foreign Store accepted sealed principal")
		}
	})
	for _, mode := range []string{"root-local-public", "actor-scoped", "dependency-pinned-local"} {
		t.Run(mode, func(t *testing.T) {
			r, _, service, request, reg := integrationCreationFixtureActions(t, func(r *app.Runtime, op context.Context, in *generated.DraftInput) {
				if mode == "actor-scoped" {
					for i, raw := range in.Objects {
						var o map[string]any
						json.Unmarshal(raw, &o)
						if o["kind"] == "SCENE" {
							o["visibility"] = "ACTOR_SCOPED"
							in.Objects[i], _ = json.Marshal(o)
						}
					}
				}
				if mode != "dependency-pinned-local" {
					return
				}
				dep := *in
				dep.Objects = []generated.ContentObject{}
				dep.DependencyLocks = []generated.DependencyLock{}
				var scene json.RawMessage
				var sceneID string
				for _, raw := range in.Objects {
					var o map[string]any
					json.Unmarshal(raw, &o)
					if o["kind"] == "PROP" {
						o["rule_refs"] = []any{}
						raw, _ = json.Marshal(o)
					}
					if o["kind"] == "SCENE" {
						// A published acyclic scene supplies real dependency presence. The
						// coffee scene/prop back-reference is not a legal pinned graph.
						o["child_refs"] = []any{}
						raw, _ = json.Marshal(o)
						if sceneID == "" {
							scene = raw
							sceneID = o["object_id"].(string)
						}
					}
					dep.Objects = append(dep.Objects, raw)
				}
				if sceneID == "" {
					t.Fatal("real scene prerequisite")
				}
				pub := withdrawalPublish(t, r, op, "WORLD", dep, "delivery-scene")
				var propRef map[string]any
				for _, raw := range dep.Objects {
					var o map[string]any
					json.Unmarshal(raw, &o)
					if o["object_id"] != sceneID && o["kind"] != "PROP" {
						continue
					}
					hash, e := content.Hash(raw)
					if e != nil {
						t.Fatal(e)
					}
					in.DependencyLocks = append(in.DependencyLocks, generated.DependencyLock{ReleaseID: pub.ReleaseID, ObjectID: generated.Identity(o["object_id"].(string)), Version: generated.ExactUnsigned(o["version"].(string)), ContentHash: hash})
					if o["kind"] == "PROP" {
						propRef = map[string]any{"kind": "PINNED_DEFINITION", "release_id": pub.ReleaseID, "object_id": o["object_id"], "version": o["version"], "content_hash": hash}
					}
				}
				hash, e := content.Hash(scene)
				if e != nil {
					t.Fatal(e)
				}
				ref := map[string]any{"kind": "PINNED_DEFINITION", "release_id": pub.ReleaseID, "object_id": sceneID, "version": "1", "content_hash": hash}
				for i, raw := range in.Objects {
					var o map[string]any
					json.Unmarshal(raw, &o)
					if o["kind"] == "SCENE" {
						if propRef == nil {
							t.Fatal("real pinned prop prerequisite")
						}
						o["child_refs"] = []any{propRef}
						in.Objects[i], _ = json.Marshal(o)
					}
					if o["kind"] == "WORLD" {
						o["initial_scene_ref"] = ref
						for _, a := range o["initial_actor_setups"].([]any) {
							a.(map[string]any)["scene_ref"] = ref
						}
						in.Objects[i], _ = json.Marshal(o)
					}
				}
			}, time.Hour, []string{"READ", "MEMORY_VIEW"})
			world, e := r.World.CreateIntegrationInstance(service, "delivery-qualified-world", request)
			if e != nil {
				t.Fatal("real source graph CREATE", e)
			}
			session, e := r.Store.ExchangeIntegrationSession(service, "delivery-qualified-session", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID, ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: reg.Player.Actions})
			if e != nil {
				t.Fatal(e)
			}
			retainSessionScanMaterial(t, session.AccessToken)
			client, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ApplyPlayerSelfConsent(client, "delivery-qualified-consent", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
				t.Fatal(e)
			}
			snapshot, e := r.Delivery.GetSnapshot(client, world.Scope.WorldInstanceID)
			if e != nil {
				t.Fatal("actual source qualified snapshot", e)
			}
			retainSessionScanMaterial(t, snapshot.Cursor)
			items := map[string]any{}
			if raw, ok := snapshot.State["items"]; ok && json.Unmarshal(raw, &items) != nil {
				t.Fatal("filtered item map")
			}
			if mode == "actor-scoped" {
				if len(snapshot.Actors) != 0 || len(items) != 0 {
					t.Fatal("ACTOR_SCOPED borrowed as player self")
				}
			} else if mode == "dependency-pinned-local" {
				if len(snapshot.Actors) == 0 || len(items) != 1 {
					t.Fatal("pinned actor and dependency-local prop must share one canonical PUBLIC scene")
				}
			} else if len(snapshot.Actors) == 0 || len(items) == 0 {
				t.Fatal("actual PUBLIC scene graph omitted")
			}
		})
	}

	t.Run("snapshot-before-and-after-real-control", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		accepted, e := f.r.World.ControlIntegrationInstance(f.client, f.world.WorldInstanceID, "snapshot-ordered-pause", generated.ControlRequest{Command: "PAUSE", ExpectedControlVersion: "1"})
		if e != nil {
			t.Fatal(e)
		}
		lease, e := f.r.Store.ClaimRegistered(context.Background(), "snapshot-order-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: accepted.TaskID}})
		if e != nil {
			t.Fatal(e)
		}
		h, e := f.r.Store.BeginIntegrationRead(f.client, f.world.WorldInstanceID, "SNAPSHOT")
		if e != nil {
			t.Fatal(e)
		}
		defer h.Close()
		before, e := h.Snapshot()
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, before.Cursor)
		started, finished := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			finished <- f.r.World.Process(context.Background(), lease, "snapshot-order-worker")
		}()
		<-started
		select {
		case e := <-finished:
			t.Fatalf("writer completed while snapshot owns world barrier: %v", e)
		case <-time.After(50 * time.Millisecond):
		}
		if e = h.Finish(f.client); e != nil {
			t.Fatal(e)
		}
		select {
		case e = <-finished:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owned control did not return after reader close")
		}
		after, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, after.Cursor)
		if before.InstanceState != "RUNNING" || after.InstanceState != "PAUSED" {
			t.Fatal("real ordered instance states")
		}
		page, e := f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, before.Cursor)
		if e != nil || len(page.Events) != 1 {
			t.Fatal("snapshot old H must expose precisely subsequent CONTROL", e)
		}
		retainSessionScanMaterial(t, page.NextCursor)
		eof, e := f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, after.Cursor)
		if e != nil || len(eof.Events) != 0 || eof.NextCursor != after.Cursor {
			t.Fatal("writer-first snapshot H must already include CONTROL", e)
		}
	})
	t.Run("bounded-three-replans", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		attempts := 0
		f.r.Store.BeforeReadFinish = func(context.Context, *sql.Tx) error { attempts++; return store.ErrReplan }
		v, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
		f.r.Store.BeforeReadFinish = nil
		if !errors.Is(e, store.ErrReplan) || attempts != 3 || len(v.Actors) != 0 || len(v.State) != 0 || v.Cursor != "" {
			t.Fatal("exact three whole-stage replans/zero escaping snapshot", attempts, e)
		}
	})

}

func TestIntegrationDeliveryCloseAndAdmission(t *testing.T) {
	r, _, service, request, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ"})
	if _, e := r.World.CreateIntegrationInstance(service, "delivery-admission-control", request); e != nil {
		t.Fatal("legal original CREATE control", e)
	}
	port, ok := any(r.Local.Participant).(interface {
		EnterDelivery(context.Context, *store.Store) (context.Context, func(), error)
		CheckDelivery(context.Context, *store.Store) error
	})
	if !ok {
		t.Fatal("real Local registration/CREATE succeeded: closed twelve-kind delivery admission unavailable")
	}
	ctx, release, e := port.EnterDelivery(context.Background(), r.Store)
	if e != nil {
		t.Fatal("actual complete delivery group", e)
	}
	if e = port.CheckDelivery(ctx, r.Store); e != nil {
		release()
		t.Fatal(e)
	}
	if _, nestedRelease, e := port.EnterDelivery(ctx, r.Store); e == nil {
		nestedRelease()
		release()
		t.Fatal("borrowed standing context accepted as new stage")
	}
	release()
	if e = port.CheckDelivery(ctx, r.Store); e == nil {
		t.Fatal("released permit accepted")
	}
	if e = port.CheckDelivery(context.Background(), r.Store); e == nil {
		t.Fatal("free context accepted")
	}
	writers := []maintenance.Writer{maintenance.APIWriter, maintenance.StoreWriter, maintenance.WorkerWriter, maintenance.FileWriter, maintenance.GovernanceWriter, maintenance.HarnessWriter, maintenance.MockWriter, maintenance.QueryWriter, maintenance.CleanupWriter, maintenance.DeadlineWriter, maintenance.ConfigWriter, maintenance.NotificationWriter}
	for _, mode := range []string{"second-current", "legacy-eleven", "isolated-notification", "missing", "duplicate", "unknown", "replaced-guard", "released", "stale"} {
		t.Run(mode, func(t *testing.T) {
			list := append([]maintenance.Writer(nil), writers...)
			switch mode {
			case "legacy-eleven", "missing":
				list = list[:11]
			case "isolated-notification":
				list = []maintenance.Writer{maintenance.NotificationWriter}
			case "duplicate":
				list[11] = list[10]
			case "unknown":
				list[11] = maintenance.Writer("UNKNOWN")
			}
			participant, e := r.Local.Coordinator.Register(context.Background(), "delivery-"+mode, list)
			if mode == "unknown" {
				if e == nil {
					participant.Close(context.Background())
					t.Fatal("unknown accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer func() {
				if e := participant.Close(context.Background()); e != nil {
					t.Error(e)
				}
			}()
			other := store.New(r.Store.DB)
			other.Guard = participant
			other.ConfigGuard = participant
			other.WorldAccountingAuthority = participant
			if mode == "replaced-guard" {
				other.Guard = r.Local.Participant
			}
			if mode == "stale" {
				if e = participant.Close(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			entered, done, e := participant.EnterDelivery(context.Background(), other)
			if mode == "second-current" || mode == "released" {
				if e != nil {
					t.Fatal("real independent incarnation", e)
				}
				if e = participant.CheckDelivery(entered, other); e != nil {
					done()
					t.Fatal(e)
				}
				done()
				if e = participant.CheckDelivery(entered, other); e == nil {
					t.Fatal("old permit accepted")
				}
				if mode == "second-current" {
					_, _, finish, e := participant.EnterWorldAccounting(context.Background())
					if e != nil {
						t.Fatal("complete group accounting", e)
					}
					finish()
				}
			} else if e == nil {
				done()
				t.Fatal("invalid registration/Store accepted")
			}
			if mode == "legacy-eleven" {
				_, _, finish, e := participant.EnterWorldAccounting(context.Background())
				if e != nil {
					t.Fatal("legacy accounting changed", e)
				}
				finish()
			}
		})
	}
	// Each SQL identity mutation is in the owned synthetic database, restored
	// before any other stage. The original configuration/credential is unchanged.
	for _, col := range []string{"installation_id", "generation", "database_binding", "files_binding", "coordinator_binding"} {
		t.Run("current-"+col, func(t *testing.T) {
			var old any
			if e := r.Store.DB.QueryRow("SELECT " + col + " FROM memory_installation WHERE singleton=1").Scan(&old); e != nil {
				t.Fatal(e)
			}
			var value any = "foreign-binding"
			if col == "generation" {
				value = uint64(999)
			}
			res, e := r.Store.DB.Exec("UPDATE memory_installation SET "+col+"=? WHERE singleton=1", value)
			if e != nil {
				t.Fatal(e)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatal("injection not reached")
			}
			defer func() {
				if _, e := r.Store.DB.Exec("UPDATE memory_installation SET "+col+"=? WHERE singleton=1", old); e != nil {
					t.Error(e)
				}
			}()
			if _, done, e := port.EnterDelivery(context.Background(), r.Store); e == nil {
				done()
				t.Fatal("current binding accepted")
			}
		})
	}
	if e := r.Store.BindDeliveryAdmission(r.Local.Participant); e == nil {
		t.Fatal("second admission bind accepted")
	}
	ctx, release, e = port.EnterDelivery(context.Background(), r.Store)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Local.Coordinator.BeginClose(context.Background(), "delivery-close"); e != nil {
		release()
		t.Fatal(e)
	}
	if _, done, e := port.EnterDelivery(context.Background(), r.Store); e == nil {
		done()
		release()
		t.Fatal("closing admits a fresh stage")
	}
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	e = r.Local.Coordinator.Drain(short, "delivery-close")
	cancel()
	if e == nil {
		release()
		t.Fatal("held stage not drained")
	}
	release()
	if e = r.Local.Coordinator.Drain(context.Background(), "delivery-close"); e != nil {
		t.Fatal(e)
	}

}

func deliveryFragmentFixture(t *testing.T, action bool) (integrationFixFixture, string) {
	t.Helper()
	f := integrationFixSetup(t, true)
	var raw []byte
	var item struct{ CustodianID string }
	if e := f.r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? LIMIT 1", f.world.WorldInstanceID).Scan(&raw); e != nil || json.Unmarshal(raw, &item) != nil {
		t.Fatal("real item prerequisite", e)
	}
	f.input.PrimaryActorID = item.CustodianID
	if action {
		f.input.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
	}
	q, e := f.r.Store.QuoteIntegrationTurn(f.service, "fragment-quote", generated.QuoteRequest{Scope: f.world, ServiceKind: "TURN_TEXT", PrimaryActorID: &f.input.PrimaryActorID, InputUpperBound: "16000", OutputUpperBound: "4000", PurchaseResponsibility: "MOCK"})
	if e != nil {
		t.Fatal(e)
	}
	f.input.QuoteID = q.QuoteID
	a, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "fragment-intake", f.input)
	if e != nil {
		t.Fatal(e)
	}
	l, e := f.r.Store.ClaimRegistered(context.Background(), "fragment-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.r.Worker.Turns.Execute(context.Background(), l.TaskLease, "fragment-worker"); e != nil {
		t.Fatal("original owning COMMIT", e)
	}
	var events, calls, commits int
	if e = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM events WHERE task_id=?),calls_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM tasks WHERE task_id=?", a.TurnID, a.TurnID, a.TurnID).Scan(&events, &calls, &commits); e != nil || events != map[bool]int{false: 0, true: 1}[action] || calls != 1 || commits != 1 {
		t.Fatal("real one event/packet prerequisite", events, calls, commits, e)
	}
	return f, a.TurnID
}

func TestIntegrationDeliveryFilteredCursor(t *testing.T) {
	if !t.Run("complete-stream-gap", deliveryFix1MissingRows) {
		return
	}
	if !t.Run("real-action-event-and-packet-limit-one", func(t *testing.T) {
		f, task := deliveryFragmentFixture(t, true)
		first, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 1)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, first.NextCursor)
		if len(first.Events) != 1 || len(first.Deliveries) != 0 {
			t.Fatal("original CONTROL first")
		}
		second, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, first.NextCursor, 1)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, second.NextCursor)
		if len(second.Events)+len(second.Deliveries) > 1 {
			t.Fatal("real original event plus packet exceeds combined limit1", len(second.Events), len(second.Deliveries))
		}
		if len(second.Events) != 1 || len(second.Deliveries) != 0 {
			t.Fatal("original event fragment")
		}
		third, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, second.NextCursor, 1)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, third.NextCursor)
		if len(third.Events) != 0 || len(third.Deliveries) != 1 || third.NextCursor == second.NextCursor {
			t.Fatal("final packet fragment must advance")
		}
		end, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, third.NextCursor, 1)
		if e != nil || len(end.Events)+len(end.Deliveries) != 0 || end.NextCursor != third.NextCursor {
			t.Fatal("zero-progress EOF", e)
		}
		decode, _ := deliveryCursorCodec(t, f)
		original := decode(first.NextCursor)
		for _, token := range []string{second.NextCursor, third.NextCursor, end.NextCursor} {
			v := decode(token)
			if v["Version"] != float64(2) || v["Issued"] != original["Issued"] || v["Expires"] != original["Expires"] {
				t.Fatal("fragment renewed original expiry")
			}
		}
		partial := decode(second.NextCursor)
		if partial["Position"] != float64(1) || partial["ItemOffset"] != float64(1) || partial["PartialUpdateID"] == "" || partial["PartialManifestHash"] == "" {
			t.Fatal("complete ordinal/partial tuple")
		}
		if first.Events[0].EventKind != "CONTROL" || second.Events[0].EventKind != "ACTION_BUNDLE" || len(first.Events[0].Payload) != 0 || len(second.Events[0].Payload) != 0 {
			t.Fatal("original event kind/minimal payload")
		}
		for _, limit := range []uint16{1, 2, 50, 100} {
			cursor := ""
			gotEvents := []generated.VisibleEvent{}
			gotPackets := []generated.IntegrationDeliveryUpdate{}
			for i := 0; i < 6; i++ {
				page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, cursor, limit)
				if e != nil || len(page.Events)+len(page.Deliveries) > int(limit) {
					t.Fatal("bounded original fragments", e)
				}
				retainSessionScanMaterial(t, page.NextCursor)
				gotEvents = append(gotEvents, page.Events...)
				gotPackets = append(gotPackets, page.Deliveries...)
				if page.NextCursor == cursor {
					break
				}
				cursor = page.NextCursor
			}
			if len(gotEvents) != 2 || gotEvents[0].EventID != first.Events[0].EventID || gotEvents[1].EventID != second.Events[0].EventID || len(gotPackets) != 1 || !bytes.Equal(gotPackets[0], third.Deliveries[0]) {
				t.Fatal("fragment loss/duplicate/core identity")
			}
		}
		// A replayed fragment is a read, never a new send or business update.
		again, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, second.NextCursor, 1)
		if e != nil || len(again.Deliveries) != 1 || !bytes.Equal(again.Deliveries[0], third.Deliveries[0]) {
			t.Fatal("same partial reread", e)
		}
		retainSessionScanMaterial(t, again.NextCursor)
		// CONTROLLED_MEDIA_ROW_FIXTURE: independent existing sidecar state is
		// deliberately not a claim of a real Slice6 producer or playable asset.
		var packet string
		var coreBefore []byte
		if e = f.r.Store.DB.QueryRow("SELECT result_id,document FROM harness_turn_commits WHERE task_id=?", task).Scan(&packet, &coreBefore); e != nil {
			t.Fatal(e)
		}
		res, e := f.r.Store.DB.Exec(`INSERT INTO delivery_media(tenant_id,product_id,asset_id,world_instance_id,packet_id,media_id,state,revision,original_result_ref,source_refs,fence,attempt_count,accepted_at,execute_by,maintenance_generation) VALUES(?,?,'fragment-sidecar',?,?,'fragment-media','PENDING',1,?,JSON_ARRAY(),0,0,UTC_TIMESTAMP(6),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 120 SECOND),1)`, f.world.TenantID, f.world.ProductID, f.world.WorldInstanceID, packet, packet)
		if e != nil {
			t.Fatal("owned independent sidecar insertion", e)
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			t.Fatal("sidecar exact insert", n, e)
		}
		defer func() {
			res, e := f.r.Store.DB.Exec("DELETE FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id='fragment-sidecar'", f.world.TenantID, f.world.ProductID)
			if e != nil {
				t.Error(e)
				return
			}
			if n, e := res.RowsAffected(); e != nil || n != 1 {
				t.Error("sidecar exact cleanup", n, e)
			}
		}()
		for _, state := range []string{"PENDING", "FAILED"} {
			if state == "FAILED" {
				res, e = f.r.Store.DB.Exec("UPDATE delivery_media SET state='FAILED',revision=2 WHERE tenant_id=? AND product_id=? AND asset_id='fragment-sidecar' AND state='PENDING' AND revision=1", f.world.TenantID, f.world.ProductID)
				if e != nil {
					t.Fatal(e)
				}
				if n, e := res.RowsAffected(); e != nil || n != 1 {
					t.Fatal("sidecar exact update", n, e)
				}
			}
			page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, second.NextCursor, 1)
			if e != nil || len(page.Deliveries) != 1 || !bytes.Equal(page.Deliveries[0], third.Deliveries[0]) {
				t.Fatal("sidecar changed original core fragment", e)
			}
			retainSessionScanMaterial(t, page.NextCursor)
			v := decode(page.NextCursor)
			if v["Expires"] != original["Expires"] || v["Position"] != decode(third.NextCursor)["Position"] || v["ItemOffset"] != float64(0) {
				t.Fatal("sidecar changed progress/expiry")
			}
		}
		var coreAfter []byte
		if e = f.r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", task).Scan(&coreAfter); e != nil || !bytes.Equal(coreBefore, coreAfter) {
			t.Fatal("original body hash/core changed", e)
		}
		var h, calls, commits int
		if e = f.r.Store.DB.QueryRow("SELECT update_seq,(SELECT SUM(calls_used) FROM tasks),(SELECT COUNT(*) FROM harness_turn_commits) FROM delivery_streams WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&h, &calls, &commits); e != nil || h != 2 || calls != 1 || commits != 1 {
			t.Fatal("reads produced business effects", h, calls, commits, e)
		}
	}) {
		return
	}
	t.Run("dialogue-only-final-progress", func(t *testing.T) {
		f, _ := deliveryFragmentFixture(t, false)
		first, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 1)
		if e != nil || len(first.Events) != 1 {
			t.Fatal("CONTROL prerequisite", e)
		}
		retainSessionScanMaterial(t, first.NextCursor)
		last, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, first.NextCursor, 1)
		if e != nil || len(last.Events) != 0 || len(last.Deliveries) != 1 || last.NextCursor == first.NextCursor {
			t.Fatal("delivery-only final progress", e)
		}
		retainSessionScanMaterial(t, last.NextCursor)
		end, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, last.NextCursor, 1)
		if e != nil || len(end.Events)+len(end.Deliveries) != 0 || end.NextCursor != last.NextCursor {
			t.Fatal("delivery-only EOF", e)
		}
	})
	f := integrationFixSetup(t, true)
	// BUSINESS_PRODUCER: every row comes from a genuine owning API and COMMIT.
	// The initial fixture committed START. No SQL manufactures a business row.
	for i := 1; i < 100; i++ {
		action := "RESUME"
		if i%2 == 1 {
			action = "PAUSE"
		}
		f.control(t, f.client, action, strconv.Itoa(i), "delivery-page-"+strconv.Itoa(i))
	}
	var h, updates, receipts, notices int
	if e := f.r.Store.DB.QueryRow(`SELECT update_seq,(SELECT COUNT(*) FROM delivery_updates WHERE world_instance_id=?),(SELECT COUNT(*) FROM commit_receipts WHERE world_instance_id=? AND operation='INSTANCE_CONTROL' AND phase='COMMIT'),(SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=?) FROM delivery_streams WHERE world_instance_id=?`, f.world.WorldInstanceID, f.world.WorldInstanceID, f.world.WorldInstanceID, f.world.WorldInstanceID).Scan(&h, &updates, &receipts, &notices); e != nil || h != 100 || updates != 100 || receipts != 100 || notices != 100 {
		t.Fatal("real hundred producer chain", h, updates, receipts, notices, e)
	}
	read := func(cursor string, limit uint16) generated.IntegrationEventPage {
		t.Helper()
		p, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, cursor, limit)
		if e != nil {
			t.Fatal("real filtered reader", e)
		}
		retainSessionScanMaterial(t, p.NextCursor)
		if len(p.Events)+len(p.Deliveries) > int(limit) {
			t.Fatal("combined output bound")
		}
		b, e := json.Marshal(p)
		if e != nil || generated.Validate("IntegrationEventPage", b) != nil {
			t.Fatal("closed page wire")
		}
		return p
	}
	all := read("", 100)
	if len(all.Events) != 100 || len(all.Deliveries) != 0 {
		t.Fatal("visible hundred events")
	}
	seen := map[string]bool{}
	for _, e := range all.Events {
		if seen[e.EventID] {
			t.Fatal("duplicate event")
		}
		seen[e.EventID] = true
	}
	one := read("", 1)
	if len(one.Events) != 1 || one.Events[0].EventID != all.Events[0].EventID {
		t.Fatal("limit one original event")
	}
	first := read("", 50)
	second := read(first.NextCursor, 50)
	if len(first.Events) != 50 || len(second.Events) != 50 {
		t.Fatal("two visible pages")
	}
	for i, e := range append(append([]generated.VisibleEvent(nil), first.Events...), second.Events...) {
		if e.EventID != all.Events[i].EventID {
			t.Fatal("ordered continuation")
		}
	}
	defaults, e := f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, "")
	if e != nil || len(defaults.Events) != 50 {
		t.Fatal("omitted limit50", e)
	}
	retainSessionScanMaterial(t, defaults.NextCursor)
	for i, e := range defaults.Events {
		if e.EventID != first.Events[i].EventID {
			t.Fatal("default/explicit50 differs")
		}
	}
	eof := read(second.NextCursor, 50)
	if len(eof.Events) != 0 || len(eof.Deliveries) != 0 || eof.NextCursor != second.NextCursor {
		t.Fatal("exact no-progress EOF")
	}
	for _, bad := range []uint16{0, 101} {
		if _, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", bad); e == nil {
			t.Fatal("invalid page limit")
		}
	}
	// Hash complete original rows in RAM; no secret or business body is logged.
	// Sort encoded rows rather than assuming a database's unrequested row order.
	digest := func(query string) [32]byte {
		t.Helper()
		rows, e := f.r.Store.DB.Query(query)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		cols, e := rows.Columns()
		if e != nil {
			t.Fatal(e)
		}
		encoded := []string{}
		for rows.Next() {
			values := make([][]byte, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if e = rows.Scan(dest...); e != nil {
				t.Fatal(e)
			}
			raw, e := json.Marshal(values)
			if e != nil {
				t.Fatal(e)
			}
			encoded = append(encoded, string(raw))
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		sort.Strings(encoded)
		raw, e := json.Marshal(encoded)
		if e != nil {
			t.Fatal(e)
		}
		return sha256.Sum256(raw)
	}
	originQueries := []string{"SELECT * FROM tasks", "SELECT * FROM runtime_tasks", "SELECT * FROM events", "SELECT * FROM runtime_events", "SELECT * FROM approved_results", "SELECT * FROM commit_receipts", "SELECT * FROM integration_notifications", "SELECT * FROM outbox", "SELECT * FROM delivery_streams", "SELECT tenant_id,product_id,world_instance_id,update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,lineage_refs FROM delivery_updates"}
	originals := make([][32]byte, len(originQueries))
	for i, q := range originQueries {
		originals[i] = digest(q)
	}
	allUpdates := digest("SELECT * FROM delivery_updates")
	var other string
	if e = f.r.Store.DB.QueryRow("SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id<>'s66-player' ORDER BY subject_id LIMIT 1", f.world.TenantID, f.world.ProductID).Scan(&other); e != nil || other == "" {
		t.Fatal("real existing nonself subject", e)
	}
	var audience []byte
	if e = f.r.Store.DB.QueryRow("SELECT audience_refs FROM delivery_updates WHERE world_instance_id=? ORDER BY update_seq LIMIT 1", f.world.WorldInstanceID).Scan(&audience); e != nil {
		t.Fatal(e)
	}
	var same int
	if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM delivery_updates WHERE world_instance_id=? AND audience_refs=CAST(? AS JSON)", f.world.WorldInstanceID, string(audience)).Scan(&same); e != nil || same != 100 {
		t.Fatal("same original audience prerequisite", same, e)
	}
	fault, _ := json.Marshal([]string{other})
	defer func() {
		res, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET audience_refs=CAST(? AS JSON) WHERE world_instance_id=?", string(audience), f.world.WorldInstanceID)
		if e != nil {
			t.Error("restore audience", e)
			return
		}
		if n, e := res.RowsAffected(); e != nil || n != 100 {
			t.Error("restore exact hundred rows", n, e)
		}
		if digest("SELECT * FROM delivery_updates") != allUpdates {
			t.Error("original complete update bytes not restored")
		}
		for i, q := range originQueries {
			if digest(q) != originals[i] {
				t.Error("original origin rows changed", i)
			}
		}
	}()
	res, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET audience_refs=CAST(? AS JSON) WHERE world_instance_id=?", string(fault), f.world.WorldInstanceID)
	if e != nil {
		t.Fatal("restrictive audience fault", e)
	}
	if n, e := res.RowsAffected(); e != nil || n != 100 {
		t.Fatal("exact hundred audience faults", n, e)
	}
	// CONTROLLED_FILTER_FIXTURE: this is a restrictive data fault, never a
	// claim that the business producer issued another player's invisible rows.
	invisible := read("", 50)
	if len(invisible.Events) != 0 || len(invisible.Deliveries) != 0 || invisible.NextCursor == "" {
		t.Fatal("FILTER_MECHANISM_NOT_REACHED")
	}
	end := read(invisible.NextCursor, 50)
	if end.NextCursor != invisible.NextCursor || len(end.Events) != 0 || len(end.Deliveries) != 0 {
		t.Fatal("hidden scan did not advance to exact EOF")
	}
	for i, q := range originQueries {
		if digest(q) != originals[i] {
			t.Fatal("filter fault altered original origin", i)
		}
	}
}

func TestIntegrationProfileWireContract(t *testing.T) {
	if !t.Run("full-legacy-http-reset", deliveryFix1LegacyHTTP) {
		return
	}
	var token string
	r, a := integrationAcceptedTurn(t, func(_ *app.Runtime, v generated.ClientSession) { token = v.AccessToken })
	q := httptest.NewRequest("GET", "/world/v1/instances/"+a.Task.Scope.WorldInstanceID+"/snapshot", nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	req, e := http.NewRequest("GET", server.URL+q.URL.Path, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Header = q.Header
	resp, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	body, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("lawful registered CLIENT snapshot route status %d", resp.StatusCode)
	}
	if generated.Validate("Snapshot", body) != nil {
		t.Fatal("complete snapshot wire")
	}
	var v generated.Snapshot
	if json.Unmarshal(body, &v) != nil {
		t.Fatal("snapshot decode")
	}
	retainSessionScanMaterial(t, v.Cursor)
	lease, e := r.Store.ClaimRegistered(context.Background(), "delivery-http-turn", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "delivery-http-turn"); e != nil {
		t.Fatal("real original TURN", e)
	}
	for _, tc := range []struct {
		path, profile, schema string
		status                int
	}{
		{"/world/v1/turns/" + a.TurnID, "integration_r0_v1", "IntegrationTurnView", 200},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/events", "integration_r0_v1", "IntegrationEventPage", 200},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/snapshot", "unknown", "", 400},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/snapshot", "public_v1", "", 409},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/events?limit=0", "integration_r0_v1", "", 400},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/events?limit=101", "integration_r0_v1", "", 400},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/events?limit=x", "integration_r0_v1", "", 400},
		{"/world/v1/instances/" + a.Task.Scope.WorldInstanceID + "/events?cursor=a&cursor=b", "integration_r0_v1", "", 400},
	} {
		req, _ := http.NewRequest("GET", server.URL+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Loreviva-Client-Contract", tc.profile)
		resp, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("route %s status %d expected %d", tc.path, resp.StatusCode, tc.status)
		}
		if tc.schema != "" && generated.Validate(tc.schema, b) != nil {
			t.Fatal("profile schema", tc.schema)
		}
		if tc.schema == "IntegrationEventPage" {
			var p generated.IntegrationEventPage
			_ = json.Unmarshal(b, &p)
			retainSessionScanMaterial(t, p.NextCursor)
			if len(p.Deliveries) != 1 {
				t.Fatal("original committed packet delivery missing")
			}
		}
		if tc.schema == "IntegrationTurnView" {
			var p generated.IntegrationTurnView
			_ = json.Unmarshal(b, &p)
			if len(p.ApprovedPackets) != 1 || len(p.Deliveries) != 1 || len(p.ApprovedPackets[0].Media) != 0 {
				t.Fatal("packet identity/media projection")
			}
		}
	}
	qhead, _ := http.NewRequest("HEAD", server.URL+q.URL.Path, nil)
	qhead.Header = q.Header
	res, e := server.Client().Do(qhead)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 405 {
		t.Fatal("HEAD gained GET segment")
	}
	duplicate, _ := http.NewRequest("GET", server.URL+q.URL.Path, nil)
	duplicate.Header = q.Header.Clone()
	duplicate.Header.Add("Loreviva-Client-Contract", "integration_r0_v1")
	dup, e := server.Client().Do(duplicate)
	if e != nil {
		t.Fatal(e)
	}
	dup.Body.Close()
	if dup.StatusCode != 400 {
		t.Fatal("duplicate profile accepted")
	}
	unknown, _ := http.NewRequest("GET", server.URL+q.URL.Path+"/unregistered", nil)
	unknown.Header = q.Header.Clone()
	res, e = server.Client().Do(unknown)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("unknown suffix gained exact delivery route")
	}
	t.Run("real-public-session-default-profile", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		ss, e := f.r.Store.ExchangeIntegrationSession(f.service, "delivery-public-profile", "public_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: f.world.WorldInstanceID, ExternalSubjectID: "S66-player-external", EntitlementReference: "s66-entitlement-one", RequestedActions: []string{"READ"}})
		if e != nil {
			t.Fatal("real public profile Session", e)
		}
		retainSessionScanMaterial(t, ss.AccessToken)
		quote, e := f.quote(f.service, "public-profile-quote")
		if e != nil {
			t.Fatal(e)
		}
		f.input.QuoteID = quote.QuoteID
		accepted, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "public-profile-turn", f.input)
		if e != nil {
			t.Fatal(e)
		}
		lease := withdrawalLease(t, f.r, accepted, "public-profile-worker")
		if e = f.r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "public-profile-worker"); e != nil {
			t.Fatal(e)
		}
		srv := httptest.NewServer(f.r.Handler())
		defer srv.Close()
		for _, profile := range []string{"", "public_v1"} {
			for _, tc := range []struct{ path, schema string }{{"/world/v1/instances/" + f.world.WorldInstanceID + "/snapshot", "Snapshot"}, {"/world/v1/instances/" + f.world.WorldInstanceID + "/events", "EventPage"}, {"/world/v1/turns/" + accepted.TurnID, "TurnView"}} {
				req, _ := http.NewRequest("GET", srv.URL+tc.path, nil)
				req.Header.Set("Authorization", "Bearer "+ss.AccessToken)
				if profile != "" {
					req.Header.Set("Loreviva-Client-Contract", profile)
				}
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || generated.Validate(tc.schema, body) != nil {
					t.Fatalf("public/default wire %s status %d error %v", tc.schema, resp.StatusCode, err)
				}
				var v map[string]json.RawMessage
				if json.Unmarshal(body, &v) != nil {
					t.Fatal("wire")
				}
				for _, key := range []string{"cursor", "next_cursor"} {
					if v[key] != nil {
						var token string
						json.Unmarshal(v[key], &token)
						retainSessionScanMaterial(t, token)
					}
				}
			}
		}
		req, _ := http.NewRequest("GET", srv.URL+"/world/v1/instances/"+f.world.WorldInstanceID+"/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+ss.AccessToken)
		resp, e := srv.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		reader := bufio.NewReader(resp.Body)
		line, e := reader.ReadString('\n')
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 || !strings.HasPrefix(line, "event: ") {
			t.Fatal("default public stream actual string frame", resp.StatusCode, e)
		}
	})

}

// Test-only codec reads the existing owned CONFIG key; it never mints one or
// logs plaintext. Authenticated faults exercise semantic rejection after AEAD.
func deliveryCursorCodec(t *testing.T, f integrationFixFixture) (func(string) map[string]any, func(map[string]any) string) {
	t.Helper()
	var e error
	var installation string
	if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	root, e := localstate.Open(filepath.Dir(f.r.Local.Coordinator.Directory()), false)
	if e != nil {
		t.Fatal(e)
	}
	_, identity, keyDone, e := f.r.Local.Participant.EnterConfiguration(context.Background())
	if e != nil || identity.Installation != installation {
		t.Fatal("CONFIG original key read", e)
	}
	material, e := root.SessionHMACKey(installation, false)
	keyDone()
	if e != nil {
		t.Fatal(e)
	}
	mac := hmac.New(sha256.New, material[:])
	mac.Write([]byte("loreviva/delivery-cursor/v1"))
	block, e := aes.NewCipher(mac.Sum(nil))
	if e != nil {
		t.Fatal(e)
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		t.Fatal(e)
	}

	decode := func(token string) map[string]any {
		t.Helper()
		b, e := base64.RawURLEncoding.DecodeString(token)
		if e != nil || len(b) < gcm.NonceSize() {
			t.Fatal("owned cursor encoding")
		}
		plain, e := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte("loreviva/delivery-cursor/v1"))
		if e != nil {
			t.Fatal("owned cursor authentication")
		}
		var v map[string]any
		if json.Unmarshal(plain, &v) != nil {
			t.Fatal("owned cursor record")
		}
		return v
	}
	encode := func(v map[string]any) string {
		t.Helper()
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, e = rand.Read(nonce); e != nil {
			t.Fatal(e)
		}
		token := base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, raw, []byte("loreviva/delivery-cursor/v1")))
		retainSessionScanMaterial(t, token)
		return token
	}
	return decode, encode
}

func TestIntegrationDeliveryCursorBinding(t *testing.T) {
	if !t.Run("full-legacy-http-reset", deliveryFix1LegacyHTTP) {
		return
	}
	t.Run("authenticated-partial-and-current", func(t *testing.T) {
		f, task := deliveryFragmentFixture(t, true)
		first, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 1)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, first.NextCursor)
		partial, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, first.NextCursor, 1)
		if e != nil || len(partial.Events) != 1 || len(partial.Deliveries) != 0 {
			t.Fatal("lawful partial", e)
		}
		retainSessionScanMaterial(t, partial.NextCursor)
		decode, encode := deliveryCursorCodec(t, f)
		for _, field := range []string{"old-format", "zero-offset-with-fields", "missing-id", "missing-hash", "wrong-id", "wrong-hash", "offset-at-end", "offset-overflow", "position-at-tail"} {
			t.Run(field, func(t *testing.T) {
				v := decode(partial.NextCursor)
				switch field {
				case "old-format":
					v["Version"] = 1
				case "zero-offset-with-fields":
					v["ItemOffset"] = 0
				case "missing-id":
					v["PartialUpdateID"] = ""
				case "missing-hash":
					v["PartialManifestHash"] = ""
				case "wrong-id":
					v["PartialUpdateID"] = "another-original-update"
				case "wrong-hash":
					v["PartialManifestHash"] = strings.Repeat("0", 64)
				case "offset-at-end":
					v["ItemOffset"] = 2
				case "offset-overflow":
					v["ItemOffset"] = 1000000
				case "position-at-tail":
					v["Position"] = v["HighWater"]
				}
				bad := encode(v)
				p, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, bad, 1)
				if e == nil || len(p.Events)+len(p.Deliveries) != 0 {
					t.Fatal("authenticated partial fault escaped")
				}
			})
		}
		// Each owned fault is restored byte-exactly before the next valid read.
		for _, mode := range []string{"missing-row", "raw-manifest-drift", "other-legal-kind", "current-epoch"} {
			t.Run(mode, func(t *testing.T) {
				var result string
				var original []byte
				if e := f.r.Store.DB.QueryRow("SELECT record_id,document FROM runtime_events WHERE record_id=(SELECT result_ref FROM delivery_updates WHERE task_ref=?)", task).Scan(&result, &original); e != nil {
					t.Fatal(e)
				}
				query, restore := "", ""
				args, back := []any{}, []any{}
				switch mode {
				case "missing-row":
					query = "UPDATE delivery_updates SET update_seq=3 WHERE task_ref=? AND update_seq=2"
					restore = "UPDATE delivery_updates SET update_seq=2 WHERE task_ref=? AND update_seq=3"
					args = []any{task}
					back = args
				case "raw-manifest-drift", "other-legal-kind":
					raw := append(append([]byte(nil), original...), ' ')
					if mode == "other-legal-kind" {
						var v map[string]any
						json.Unmarshal(original, &v)
						v["Kind"] = "CONTROL"
						raw, _ = json.Marshal(v)
					}
					query = "UPDATE runtime_events SET document=? WHERE record_id=?"
					restore = query
					args = []any{raw, result}
					back = []any{original, result}
				case "current-epoch":
					query = "UPDATE world_controls SET view_epoch=view_epoch+1 WHERE world_instance_id=?"
					restore = "UPDATE world_controls SET view_epoch=view_epoch-1 WHERE world_instance_id=?"
					args = []any{f.world.WorldInstanceID}
					back = args
				}
				exact := func(q string, a []any) {
					t.Helper()
					res, e := f.r.Store.DB.Exec(q, a...)
					if e != nil {
						t.Fatal("legal owned fault", e)
					}
					if n, e := res.RowsAffected(); e != nil || n != 1 {
						t.Fatal("exact fault row", n, e)
					}
				}
				exact(query, args)
				defer exact(restore, back)
				cursor := partial.NextCursor
				if mode == "other-legal-kind" {
					cursor = ""
				} // Fresh planning must bind kind to the original producer, not only compare a prior digest.
				page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, cursor, 50)
				if e == nil || len(page.Events)+len(page.Deliveries) != 0 {
					t.Fatal("current partial/original-kind fault escaped")
				}
			})
			valid, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, partial.NextCursor, 1)
			if e != nil || len(valid.Deliveries) != 1 {
				t.Fatal("exact restored positive", e)
			}
			retainSessionScanMaterial(t, valid.NextCursor)
		}
		snapshot, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, snapshot.Cursor)
		op := runtimeContext(t, f.r)
		release, e := f.r.Content.GetRelease(op, f.r.Auth.Scope(), snapshot.Versions.ReleaseID)
		if e != nil {
			t.Fatal(e)
		}
		withdrawalAssert(t, f.r, op, release, "partial-owner-withdraw")
		page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, partial.NextCursor, 1)
		if e == nil || len(page.Events)+len(page.Deliveries) != 0 {
			t.Fatal("withdrawn partial released body")
		}
	})
	f := integrationFixSetup(t, true)
	snapshot, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
	if e != nil {
		t.Fatal("lawful snapshot", e)
	}
	retainSessionScanMaterial(t, snapshot.Cursor)
	if _, e = f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, snapshot.Cursor); e != nil {
		t.Fatal("same original Session cursor", e)
	}
	for _, bad := range []string{"1", snapshot.Cursor[:len(snapshot.Cursor)-1], func() string {
		if snapshot.Cursor[0] == 'A' {
			return "B" + snapshot.Cursor[1:]
		}
		return "A" + snapshot.Cursor[1:]
	}(), snapshot.Cursor + "="} {
		if _, e = f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, bad); e == nil {
			t.Fatal("tamper/raw sequence admitted")
		}
	}
	// Authenticated wrong-field inputs distinguish semantic binding from a mere
	// GCM failure. CONFIG material stays in this owned test's RAM and is not logged.
	var installation string
	if e = f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	root, e := localstate.Open(filepath.Dir(f.r.Local.Coordinator.Directory()), false)
	if e != nil {
		t.Fatal(e)
	}
	_, identity, keyDone, e := f.r.Local.Participant.EnterConfiguration(context.Background())
	if e != nil || identity.Installation != installation {
		t.Fatal("CONFIG original key read", e)
	}
	material, e := root.SessionHMACKey(installation, false)
	keyDone()
	if e != nil {
		t.Fatal(e)
	}
	mac := hmac.New(sha256.New, material[:])
	mac.Write([]byte("loreviva/delivery-cursor/v1"))
	block, e := aes.NewCipher(mac.Sum(nil))
	if e != nil {
		t.Fatal(e)
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := base64.RawURLEncoding.DecodeString(snapshot.Cursor)
	if e != nil {
		t.Fatal(e)
	}
	plain, e := gcm.Open(nil, encoded[:gcm.NonceSize()], encoded[gcm.NonceSize():], []byte("loreviva/delivery-cursor/v1"))
	if e != nil {
		t.Fatal("owned original cursor decode")
	}
	for _, field := range []string{"Installation", "Tenant", "Product", "Customer", "Player", "World", "Session", "Profile", "KeyVersion", "WorldPrivacy", "WorldView", "SubjectPrivacy", "SubjectView", "Consent", "Version", "Position", "HighWater", "expired", "future", "duration", "unknown"} {
		t.Run("authenticated-"+field, func(t *testing.T) {
			var record map[string]any
			if json.Unmarshal(plain, &record) != nil {
				t.Fatal("decode")
			}
			binding := record["Binding"].(map[string]any)
			switch field {
			case "Version":
				record[field] = 1
			case "Position":
				record[field] = record["HighWater"].(float64) + 1
			case "HighWater":
				record[field] = record[field].(float64) + 1
			case "expired":
				n := time.Now().UTC().Add(-16 * time.Minute)
				record["Issued"] = n
				record["Expires"] = n.Add(15 * time.Minute)
			case "future":
				n := time.Now().UTC().Add(time.Minute)
				record["Issued"] = n
				record["Expires"] = n.Add(15 * time.Minute)
			case "duration":
				record["Expires"] = time.Now().UTC().Add(30 * time.Minute)
			case "unknown":
				record["Unexpected"] = true
			default:
				if _, ok := binding[field].(string); ok {
					binding[field] = "different-" + field
				} else {
					binding[field] = binding[field].(float64) + 1
				}
			}
			body, _ := json.Marshal(record)
			nonce := make([]byte, gcm.NonceSize())
			if _, e := rand.Read(nonce); e != nil {
				t.Fatal(e)
			}
			bad := base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, body, []byte("loreviva/delivery-cursor/v1")))
			retainSessionScanMaterial(t, bad)
			if _, e := f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, bad); e == nil {
				t.Fatal("authenticated wrong binding accepted")
			}
		})
	}
	if _, e = f.r.Delivery.GetEvents(f.second, f.world.WorldInstanceID, snapshot.Cursor); e == nil {
		t.Fatal("different lawful Session accepted original cursor")
	}
	// A real same-generation RENEW changes the current choice, not old expiry.
	current, e := f.r.Store.ReadPlayerSelfConsent(f.client)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]string{"command": "RENEW", "expected_revision": string(current.ConsentRevision), "policy_id": "R0_PLAYER_PRIVATE_GENERATION_30D"})
	if _, e = f.r.Store.ApplyPlayerSelfConsent(f.client, "delivery-cursor-renew", raw); e != nil {
		t.Fatal("lawful RENEW", e)
	}
	if _, e = f.r.Delivery.GetEvents(f.client, f.world.WorldInstanceID, snapshot.Cursor); e == nil {
		t.Fatal("old choice cursor admitted")
	}
	fresh, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
	if e != nil {
		t.Fatal(e)
	}
	retainSessionScanMaterial(t, fresh.Cursor)
	if _, e = f.r.Delivery.GetEvents(f.client, "another-world", fresh.Cursor); e == nil {
		t.Fatal("foreign world accepted")
	}
}

func TestIntegrationDeliveryCurrentReadAndIO(t *testing.T) {
	t.Run("complete-stream-gap-final", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		f.control(t, f.client, "PAUSE", "1", "gap-final-pause")
		f.control(t, f.client, "RESUME", "2", "gap-final-resume")
		original := deliveryFix1Digest(t, f)
		for _, seq := range []int{1, 2, 3} {
			t.Run(strconv.Itoa(seq), func(t *testing.T) {
				h, e := f.r.Store.BeginIntegrationPage(f.client, f.world.WorldInstanceID, "", 50)
				if e != nil {
					t.Fatal("lawful complete page", e)
				}
				defer h.Close()
				page, e := h.EventPage("", 50)
				if e != nil || len(page.Events) != 3 {
					t.Fatal("complete positive before final fault", e)
				}
				retainSessionScanMaterial(t, page.NextCursor)
				hits := 0
				f.r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					result, e := tx.ExecContext(ctx, "UPDATE delivery_updates SET update_seq=4 WHERE world_instance_id=? AND update_seq=?", f.world.WorldInstanceID, seq)
					if e != nil {
						t.Fatal("legal final gap prerequisite", e)
					}
					if n, e := result.RowsAffected(); e != nil || n != 1 {
						t.Fatal("final gap affected row", n, e)
					}
					hits++
					return nil
				}
				defer func() { f.r.Store.BeforeReadFinish = nil }()
				e = h.Finish(f.client)
				f.r.Store.BeforeReadFinish = nil
				if e == nil || hits != 1 {
					t.Fatal("complete stream final must reject", hits, e)
				}
				if deliveryFix1Digest(t, f) != original {
					t.Fatal("final gap failed to roll back exact original rows")
				}
				good, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 50)
				if e != nil || len(good.Events) != 3 {
					t.Fatal("positive after rollback", e)
				}
				retainSessionScanMaterial(t, good.NextCursor)
			})
		}
	})
	t.Run("original-event-final-proof", func(t *testing.T) {
		f, task := deliveryFragmentFixture(t, true)
		var result, controlTask, controlOwner, receipt string
		if e := f.r.Store.DB.QueryRow("SELECT result_id,receipt_key FROM harness_turn_commits WHERE task_id=?", task).Scan(&result, &receipt); e != nil {
			t.Fatal(e)
		}
		if e := f.r.Store.DB.QueryRow("SELECT task_id,owner_subject_id FROM tasks WHERE world_instance_id=? AND task_kind='CONTROL' ORDER BY task_id LIMIT 1", f.world.WorldInstanceID).Scan(&controlTask, &controlOwner); e != nil {
			t.Fatal(e)
		}
		for _, mode := range []string{"raw", "kind", "status", "revision", "task-owner", "world-null", "receipt-time", "receipt-operation"} {
			t.Run(mode, func(t *testing.T) {
				h, e := f.r.Store.BeginIntegrationPage(f.client, f.world.WorldInstanceID, "", 50)
				if e != nil {
					t.Fatal("lawful original page", e)
				}
				defer h.Close()
				page, e := h.EventPage("", 50)
				if e != nil || len(page.Events) != 2 || page.Events[1].EventKind != "ACTION_BUNDLE" {
					t.Fatal("original event positive", e)
				}
				retainSessionScanMaterial(t, page.NextCursor)
				hits := 0
				f.r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
					query := ""
					args := []any{result}
					switch mode {
					case "raw":
						query = "UPDATE runtime_events SET document=CONCAT(document,' ') WHERE record_id=?"
					case "kind":
						query = "UPDATE runtime_events SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.Kind','CONTROL') WHERE record_id=?"
					case "status":
						query = "UPDATE events SET status='PENDING' WHERE record_id=?"
					case "revision":
						query = "UPDATE events SET revision=revision+1 WHERE record_id=?"
					case "task-owner":
						query = "UPDATE events SET task_id=?,owner_subject_id=? WHERE record_id=?"
						args = []any{controlTask, controlOwner, result}
					case "world-null":
						query = "UPDATE events SET world_instance_id=NULL WHERE record_id=?"
					case "receipt-time":
						query = "UPDATE commit_receipts SET committed_at=DATE_ADD(committed_at,INTERVAL 1 SECOND) WHERE commit_key=?"
						args = []any{receipt}
					case "receipt-operation":
						query = "UPDATE commit_receipts SET operation='INSTANCE_CONTROL' WHERE commit_key=?"
						args = []any{receipt}
					}
					res, e := tx.ExecContext(ctx, query, args...)
					if e != nil {
						t.Fatal("legal event fault prerequisite", e)
					}
					if n, e := res.RowsAffected(); e != nil || n != 1 {
						t.Fatal("event fault row", n, e)
					}
					hits++
					return nil
				}
				e = h.Finish(f.client)
				f.r.Store.BeforeReadFinish = nil
				if e == nil || hits != 1 {
					t.Fatal("event final stamp did not reject", hits, e)
				}
				ok, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 50)
				if e != nil || len(ok.Events) != 2 {
					t.Fatal("event rollback/current positive", e)
				}
				retainSessionScanMaterial(t, ok.NextCursor)
			})
		}
	})
	for _, mode := range []string{"positive", "H", "world-epoch", "subject-epoch", "release", "definition", "choice", "publication-document", "publication-result", "publication-check", "publication-commit", "scene-source", "copied-public", "closed"} {
		t.Run(mode, func(t *testing.T) {
			f := integrationFixSetup(t, true)
			h, e := f.r.Store.BeginIntegrationRead(f.client, f.world.WorldInstanceID, "SNAPSHOT")
			if e != nil {
				t.Fatal("lawful read", e)
			}
			defer h.Close()
			v, e := h.Snapshot()
			if e != nil {
				t.Fatal(e)
			}
			retainSessionScanMaterial(t, v.Cursor)
			if mode == "copied-public" {
				v.Actors[0].CharacterDefinitionID = "mutated-copy"
				v.State["other"] = json.RawMessage(`"secret-copy"`)
				again, e := h.Snapshot()
				if e != nil || again.Actors[0].CharacterDefinitionID == "mutated-copy" || again.State["other"] != nil {
					t.Fatal("copy modified seal")
				}
				retainSessionScanMaterial(t, again.Cursor)
			}
			if mode == "closed" {
				if e = h.Close(); e != nil {
					t.Fatal(e)
				}
				if e = h.Finish(f.client); e == nil {
					t.Fatal("closed finish accepted")
				}
				return
			}
			f.r.Store.BeforeReadFinish = func(ctx context.Context, tx *sql.Tx) error {
				query := ""
				args := []any{}
				switch mode {
				case "H":
					query = "UPDATE delivery_streams SET update_seq=update_seq+1 WHERE world_instance_id=?"
					args = []any{f.world.WorldInstanceID}
				case "world-epoch":
					query = "UPDATE world_controls SET view_epoch=view_epoch+1 WHERE world_instance_id=?"
					args = []any{f.world.WorldInstanceID}
				case "subject-epoch":
					query = "UPDATE subject_governance SET view_epoch=view_epoch+1 WHERE subject_id='s66-player'"
				case "release":
					query = "UPDATE releases SET available=0 WHERE release_id=?"
					args = []any{v.Versions.ReleaseID}
				case "definition":
					query = "UPDATE actors SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.OriginalCharacter.Name','tampered') WHERE world_instance_id=? AND actor_id=?"
					args = []any{f.world.WorldInstanceID, v.Actors[0].ActorInstanceID}
				case "choice":
					query = "UPDATE integration_self_consent_facts SET principal_ref=? WHERE player_subject_id='s66-player' AND revision=1"
					args = []any{f.secondID}
				case "publication-document":
					query = "UPDATE content_releases SET document=CONCAT(document,' ') WHERE release_id=?"
					args = []any{v.Versions.ReleaseID}
				case "publication-result":
					query = "UPDATE admin_results SET document=CONCAT(document,' ') WHERE schema_name='Release' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(document USING utf8mb4),'$.release_id'))=?"
					args = []any{v.Versions.ReleaseID}
				case "publication-check":
					query = "UPDATE content_checks SET document=CONCAT(document,' ') WHERE report_id=(SELECT JSON_UNQUOTE(JSON_EXTRACT(CONVERT(document USING utf8mb4),'$.check_report_id')) FROM content_releases WHERE release_id=?)"
					args = []any{v.Versions.ReleaseID}
				case "publication-commit":
					query = "UPDATE commit_receipts SET operation='wrongPublish' WHERE commit_key=(SELECT result_id FROM admin_results WHERE schema_name='Release' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(document USING utf8mb4),'$.release_id'))=?)"
					args = []any{v.Versions.ReleaseID}
				case "scene-source":
					query = "UPDATE actors SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.OriginalSetup.scene_ref.object_id','foreign-scene') WHERE world_instance_id=? AND actor_id=?"
					args = []any{f.world.WorldInstanceID, v.Actors[0].ActorInstanceID}
				}
				if query == "" {
					return nil
				}
				res, e := tx.ExecContext(ctx, query, args...)
				if e != nil {
					t.Fatal("legal fault injection", e)
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					t.Fatalf("exact fault rows %d", n)
				}
				return nil
			}
			e = h.Finish(f.client)
			f.r.Store.BeforeReadFinish = nil
			want := mode == "positive" || mode == "copied-public"
			if (e == nil) != want {
				t.Fatalf("final current gate %s accepted=%v", mode, e == nil)
			}
			// Finish rolls the short read transaction back; no injected state survives.
			check, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
			if e != nil {
				t.Fatal("fault rollback/current control", e)
			}
			retainSessionScanMaterial(t, check.Cursor)
		})
	}

	for _, mode := range []string{"file-positive", "file-missing", "file-hash", "owner-withdrawal", "self-revoke", "io-no-sql-lock", "post-io-row-drift"} {
		t.Run(mode, func(t *testing.T) {
			r, accepted, client, _, release := withdrawalAccepted(t)
			lease := withdrawalLease(t, r, accepted, "delivery-current-worker")
			if e := r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "delivery-current-worker"); e != nil {
				t.Fatal(e)
			}
			control, e := r.Delivery.GetTurn(client, accepted.TurnID)
			if e != nil || len(control.ApprovedPackets) != 1 {
				t.Fatal("real committed body control", e)
			}
			page, e := r.Delivery.GetEvents(client, accepted.Task.Scope.WorldInstanceID, "")
			if e != nil || len(page.Deliveries) != 1 {
				t.Fatal("real whole page control", e)
			}
			retainSessionScanMaterial(t, page.NextCursor)
			var raw []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", accepted.TurnID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var committed domain.CandidateCommit
			if json.Unmarshal(raw, &committed) != nil {
				t.Fatal("real candidate commit")
			}
			path := filepath.Join(r.Files.RootPath, string(accepted.Task.Scope.TenantID), string(accepted.Task.Scope.ProductID), committed.Metadata.BodyStorage+".part")
			if mode == "file-missing" || mode == "file-hash" {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal("actual result body prerequisite", err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(path, body, 0600); err != nil {
						t.Error(err)
					}
				})
				if mode == "file-missing" {
					e = os.Remove(path)
				} else {
					e = os.WriteFile(path, append(body, 'x'), 0600)
				}
				if e != nil {
					t.Fatal(e)
				}
			}
			if mode == "owner-withdrawal" {
				withdrawalAssert(t, r, runtimeContext(t, r), release, "delivery-owner-withdrawal")
			}
			if mode == "self-revoke" {
				if _, e = r.Store.ApplyPlayerSelfConsent(client, "delivery-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
					t.Fatal(e)
				}
			}
			original := r.Files.OriginalResultPolicy
			observed := 0
			if mode == "io-no-sql-lock" || mode == "post-io-row-drift" {
				// This observer delegates the real authorization unchanged. Its hook
				// executes only after that real verifier has closed its short SQL Tx.
				r.Files.OriginalResultPolicy = &deliveryPolicyObserver{OriginalResultPolicy: original, after: func(ctx context.Context, count int, c domain.CandidateCommit) error {
					if count != 3 {
						return nil
					}
					observed++
					probe, err := r.Store.DB.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer probe.Rollback()
					var revision uint64
					if err = probe.QueryRowContext(ctx, "SELECT world_revision FROM world_controls WHERE world_instance_id=? FOR UPDATE NOWAIT", accepted.Task.Scope.WorldInstanceID).Scan(&revision); err != nil {
						return err
					}
					if err = probe.Rollback(); err != nil {
						return err
					}
					if mode == "post-io-row-drift" {
						res, err := r.Store.DB.ExecContext(ctx, "UPDATE delivery_updates SET resource_revision=resource_revision+1 WHERE task_ref=? AND kind='TURN'", accepted.TurnID)
						if err != nil {
							return err
						}
						n, err := res.RowsAffected()
						if err != nil || n != 1 {
							return errors.New("post IO exact one fault row")
						}
					}
					return nil
				}}
				t.Cleanup(func() { r.Files.OriginalResultPolicy = original })
			}
			wrote := false
			e = r.Delivery.WriteTurn(client, accepted.TurnID, "integration_r0_v1", func(body []byte) error { wrote = true; return generated.Validate("IntegrationTurnView", body) })
			want := mode == "file-positive" || mode == "io-no-sql-lock"
			if (e == nil) != want || wrote != want {
				t.Fatalf("body write gate %s accepted=%v wrote=%v", mode, e == nil, wrote)
			}
			if mode == "file-missing" || mode == "file-hash" {
				if !errors.Is(e, files.ErrNotFound) {
					t.Fatalf("file failure must retain file category: %v", e)
				}
			}
			if mode == "io-no-sql-lock" || mode == "post-io-row-drift" {
				if observed != 1 {
					t.Fatalf("real IO current observer reached %d", observed)
				}
			}
			if !want {
				p, err := r.Delivery.GetEvents(client, accepted.Task.Scope.WorldInstanceID, "")
				if err == nil || len(p.Events) != 0 || len(p.Deliveries) != 0 {
					t.Fatal("failed whole page escaped partial output", err)
				}
			}
		})
	}

}

// These are real segmented IO/maintenance observations, not a mock nested
// release. The sixty-second stream duration is this protocol's own bound.
func TestIntegrationDeliverySSESegments(t *testing.T) {
	t.Run("actual-fifty-item-fragment-frames", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		for i := 1; i < 49; i++ {
			action := "RESUME"
			if i%2 == 1 {
				action = "PAUSE"
			}
			f.control(t, f.client, action, strconv.Itoa(i), "sse-fragment-control-"+strconv.Itoa(i))
		}
		f.input.ExpectedVersions.ControlVersion = "49"
		var raw []byte
		var item struct{ CustodianID string }
		if e := f.r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? LIMIT 1", f.world.WorldInstanceID).Scan(&raw); e != nil || json.Unmarshal(raw, &item) != nil {
			t.Fatal("real item prerequisite", e)
		}
		f.input.PrimaryActorID = item.CustodianID
		if true {
			f.input.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
		}
		q, e := f.r.Store.QuoteIntegrationTurn(f.service, "fragment-quote", generated.QuoteRequest{Scope: f.world, ServiceKind: "TURN_TEXT", PrimaryActorID: &f.input.PrimaryActorID, InputUpperBound: "16000", OutputUpperBound: "4000", PurchaseResponsibility: "MOCK"})
		if e != nil {
			t.Fatal(e)
		}
		f.input.QuoteID = q.QuoteID
		a, e := f.r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "fragment-intake", f.input)
		if e != nil {
			t.Fatal(e)
		}
		l, e := f.r.Store.ClaimRegistered(context.Background(), "fragment-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
		if e != nil {
			t.Fatal(e)
		}
		if e = f.r.Worker.Turns.Execute(context.Background(), l.TaskLease, "fragment-worker"); e != nil {
			t.Fatal("original owning COMMIT", e)
		}
		var events, calls, commits int
		if e = f.r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM events WHERE task_id=?),calls_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM tasks WHERE task_id=?", a.TurnID, a.TurnID, a.TurnID).Scan(&events, &calls, &commits); e != nil || events != 1 || calls != 1 || commits != 1 {
			t.Fatal("real one event/packet prerequisite", events, calls, commits, e)
		}

		ctx, cancel := context.WithCancel(f.client)
		defer cancel()
		pages := []generated.IntegrationEventPage{}
		err := f.r.Delivery.StreamEvents(ctx, f.world.WorldInstanceID, "", func(raw []byte) error {
			if !bytes.HasPrefix(raw, []byte("event: update\n")) {
				return errors.New("unexpected reset during legal fragment stream")
			}
			var page generated.IntegrationEventPage
			id := ""
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "id: ") {
					id = strings.TrimPrefix(line, "id: ")
				}
				if strings.HasPrefix(line, "data: ") {
					if e := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &page); e != nil {
						return e
					}
				}
			}
			if page.NextCursor == "" || id != page.NextCursor || len(page.Events)+len(page.Deliveries) > 50 {
				return errors.New("SSE fragment wire/limit")
			}
			retainSessionScanMaterial(t, page.NextCursor)
			pages = append(pages, page)
			if len(pages) == 2 {
				cancel()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || len(pages) != 2 || len(pages[0].Events) != 50 || len(pages[0].Deliveries) != 0 || len(pages[1].Events) != 0 || len(pages[1].Deliveries) != 1 || pages[0].NextCursor == pages[1].NextCursor {
			t.Fatal("real fragment frames", len(pages), err)
		}
		if pages[0].Events[49].EventKind != "ACTION_BUNDLE" || pages[0].Events[49].TurnID == nil || *pages[0].Events[49].TurnID != a.TurnID {
			t.Fatal("original event identity across frames")
		}
		decode, _ := deliveryCursorCodec(t, f)
		partial, complete := decode(pages[0].NextCursor), decode(pages[1].NextCursor)
		if partial["ItemOffset"] != float64(1) || complete["ItemOffset"] != float64(0) || partial["Expires"] != complete["Expires"] {
			t.Fatal("actual SSE partial tuple/expiry")
		}
		end, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, pages[1].NextCursor, 50)
		if e != nil || end.NextCursor != pages[1].NextCursor || len(end.Events)+len(end.Deliveries) != 0 {
			t.Fatal("SSE resume exact EOF", e)
		}
	})

	t.Run("actual-http-string-heartbeat-timeout", func(t *testing.T) {
		var token string
		r, a := integrationAcceptedTurn(t, func(_ *app.Runtime, v generated.ClientSession) { token = v.AccessToken })
		server := httptest.NewServer(r.Handler())
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		q, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/world/v1/instances/"+a.Task.Scope.WorldInstanceID+"/events/stream", nil)
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		start := time.Now()
		response, e := server.Client().Do(q)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("real SSE response", response.StatusCode)
		}
		reader := bufio.NewReader(response.Body)
		frame := []string{}
		updates, heartbeats := 0, 0
		var firstCursor string
		for {
			line, e := reader.ReadString('\n')
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			if line != "\n" {
				frame = append(frame, strings.TrimSuffix(line, "\n"))
				continue
			}
			if len(frame) == 1 && frame[0] == ": heartbeat" {
				heartbeats++
				if time.Since(start) < 14*time.Second {
					t.Fatal("heartbeat too early")
				}
				frame = nil
				continue
			}
			if len(frame) != 3 || frame[0] != "event: update" || !strings.HasPrefix(frame[1], "id: ") || !strings.HasPrefix(frame[2], "data: ") {
				t.Fatal("SSE string framing")
			}
			id := strings.TrimPrefix(frame[1], "id: ")
			var page generated.IntegrationEventPage
			if json.Unmarshal([]byte(strings.TrimPrefix(frame[2], "data: ")), &page) != nil || page.NextCursor != id {
				t.Fatal("SSE id/data binding")
			}
			data, _ := json.Marshal(map[string]any{"event": "update", "id": id, "data": page})
			if generated.Validate("IntegrationSSEUpdate", data) != nil {
				t.Fatal("typed SSE update")
			}
			retainSessionScanMaterial(t, id)
			firstCursor = id
			updates++
			frame = nil
		}
		if updates != 1 || heartbeats < 3 || time.Since(start) < 59*time.Second || time.Since(start) > 66*time.Second {
			t.Fatal("bounded stream", updates, heartbeats, time.Since(start))
		}
		// Reconnect from the emitted cursor: EOF must not invent another update ID.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		q2, _ := http.NewRequestWithContext(ctx2, "GET", q.URL.String()+"?cursor="+firstCursor, nil)
		q2.Header = q.Header
		resp, e := server.Client().Do(q2)
		if e == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if bytes.Contains(b, []byte("event: update")) {
				t.Fatal("EOF reconnect invented update")
			}
		}
		var calls uint64
		if e := r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", a.TurnID).Scan(&calls); e != nil || calls != 0 {
			t.Fatal("SSE caused original provider call", e)
		}
	})
	t.Run("write-real-parent-and-closed-SQL", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		count := 0
		e := f.r.Delivery.StreamEvents(f.client, f.world.WorldInstanceID, "", func(body []byte) error {
			count++
			if count > 1 {
				return errors.New("reset after fixture transport failure")
			}
			if !bytes.HasPrefix(body, []byte("event: update")) {
				t.Fatal("expected actual update")
			}
			// A distinct SQL transaction can take the original read row NOWAIT:
			// no reader transaction/row lock survives into network IO.
			tx, e := f.r.Store.DB.BeginTx(context.Background(), nil)
			if e != nil {
				t.Fatal(e)
			}
			var h uint64
			e = tx.QueryRow("SELECT update_seq FROM delivery_streams WHERE tenant_id='demo-local' AND product_id='coffee' AND world_instance_id=? FOR UPDATE NOWAIT", f.world.WorldInstanceID).Scan(&h)
			tx.Rollback()
			if e != nil {
				t.Fatal("SQL retained during IO", e)
			}
			if e = f.r.Local.Coordinator.BeginClose(context.Background(), "write-close"); e != nil {
				t.Fatal(e)
			}
			short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			e = f.r.Local.Coordinator.Drain(short, "write-close")
			cancel()
			if e == nil {
				t.Fatal("actual parent released before IO")
			}
			return errors.New("owned transport failure")
		})
		if e == nil {
			t.Fatal("transport error hidden")
		}
		if e = f.r.Local.Coordinator.Drain(context.Background(), "write-close"); e != nil {
			t.Fatal("parent not returned after write", e)
		}
	})
	t.Run("poll-has-no-request-wide-hold", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		snap, e := f.r.Delivery.GetSnapshot(f.client, f.world.WorldInstanceID)
		if e != nil {
			t.Fatal(e)
		}
		retainSessionScanMaterial(t, snap.Cursor)
		completed := make(chan struct{})
		var once sync.Once
		f.r.Store.BeforeReadFinish = func(context.Context, *sql.Tx) error { once.Do(func() { close(completed) }); return nil }
		ctx, cancel := context.WithCancel(f.client)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- f.r.Delivery.StreamEvents(ctx, f.world.WorldInstanceID, snap.Cursor, func([]byte) error { return nil })
		}()
		select {
		case <-completed:
		case <-time.After(3 * time.Second):
			t.Fatal("stage not reached")
		}
		if e = f.r.Local.Coordinator.BeginClose(context.Background(), "poll-close"); e != nil {
			t.Fatal(e)
		}
		short, stop := context.WithTimeout(context.Background(), time.Second)
		e = f.r.Local.Coordinator.Drain(short, "poll-close")
		stop()
		if e != nil {
			t.Fatal("poll retained request-wide handle", e)
		}
		cancel()
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("stream did not return")
		}
		f.r.Store.BeforeReadFinish = nil
	})
	t.Run("revocation-before-next-frame", func(t *testing.T) {
		f := integrationFixSetup(t, true)
		updates, resets := 0, 0
		err := f.r.Delivery.StreamEvents(f.client, f.world.WorldInstanceID, "", func(raw []byte) error {
			if bytes.HasPrefix(raw, []byte("event: update")) {
				updates++
				if updates != 1 {
					return errors.New("update after actual self revoke")
				}
				_, e := f.r.Store.ApplyPlayerSelfConsent(f.client, "stream-real-revoke", json.RawMessage(`{"command":"REVOKE","expected_revision":"1","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`))
				if e != nil {
					t.Fatal("real self revoke", e)
				}
			} else if bytes.HasPrefix(raw, []byte("event: view_reset")) {
				resets++
				if bytes.Contains(raw, []byte("\nid: ")) {
					t.Fatal("reset gained cursor")
				}
			}
			return nil
		})
		if err == nil || updates != 1 || resets != 1 {
			t.Fatal("current frame gate after self revoke", updates, resets, err)
		}
	})
	t.Run("actual-transport-write-deadline", func(t *testing.T) {
		var token string
		r, a := integrationAcceptedTurn(t, func(_ *app.Runtime, s generated.ClientSession) { token = s.AccessToken })
		serverEnd, clientEnd := net.Pipe()
		defer clientEnd.Close()
		deadlines := make(chan time.Time, 1)
		conn := &deliveryDeadlineConn{Conn: serverEnd, deadline: deadlines}
		listener := &deliveryOneConnListener{connections: make(chan net.Conn, 1), closed: make(chan struct{}), address: serverEnd.LocalAddr()}
		listener.connections <- conn
		srv := &http.Server{Handler: r.Handler()}
		served := make(chan error, 1)
		go func() { served <- srv.Serve(listener) }()
		defer func() {
			srv.Close()
			listener.Close()
			select {
			case <-served:
			case <-time.After(time.Second):
				t.Error("owned HTTP server not returned")
			}
		}()
		req, _ := http.NewRequest("GET", "http://owned.test/world/v1/instances/"+a.Task.Scope.WorldInstanceID+"/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		if e := req.Write(clientEnd); e != nil {
			t.Fatal(e)
		}
		// net.Pipe has no output buffer and this peer deliberately never reads.
		// The real HTTP ResponseController must expire its actual Conn write.
		var deadline time.Time
		select {
		case deadline = <-deadlines:
		case <-time.After(3 * time.Second):
			t.Fatal("actual write deadline never set")
		}
		remaining := time.Until(deadline)
		if remaining < 4*time.Second || remaining > 5*time.Second {
			t.Fatal("actual five-second transport deadline", remaining)
		}
		if e := r.Local.Coordinator.BeginClose(context.Background(), "blocked-http-close"); e != nil {
			t.Fatal(e)
		}
		short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		e := r.Local.Coordinator.Drain(short, "blocked-http-close")
		cancel()
		if e == nil {
			t.Fatal("actual parent released while write blocks")
		}
		bounded, stop := context.WithDeadline(context.Background(), deadline.Add(time.Second))
		defer stop()
		if e = r.Local.Coordinator.Drain(bounded, "blocked-http-close"); e != nil {
			t.Fatal("write deadline failed to return actual API handle", e)
		}
		if time.Now().Before(deadline.Add(-100 * time.Millisecond)) {
			t.Fatal("blocked real transport returned before deadline")
		}
	})

}

// The observer cannot fabricate a candidate or bypass its delegated policy.
type deliveryPolicyObserver struct {
	files.OriginalResultPolicy
	after func(context.Context, int, domain.CandidateCommit) error
	calls int
}

func (p *deliveryPolicyObserver) AuthorizeApprovedTurn(ctx context.Context, scope domain.Scope, task string) (domain.CandidateCommit, error) {
	v, e := p.OriginalResultPolicy.AuthorizeApprovedTurn(ctx, scope, task)
	if e != nil {
		return v, e
	}
	p.calls++
	if e = p.after(ctx, p.calls, v); e != nil {
		return domain.CandidateCommit{}, e
	}
	return v, nil
}

// Owned zero-buffer transport exercises the actual net/http deadline, not a
// ResponseRecorder or a callback that pretends a blocked write completed.
type deliveryDeadlineConn struct {
	net.Conn
	deadline chan time.Time
	once     sync.Once
}

func (c *deliveryDeadlineConn) SetWriteDeadline(v time.Time) error {
	e := c.Conn.SetWriteDeadline(v)
	if e == nil && !v.IsZero() {
		c.once.Do(func() { c.deadline <- v })
	}
	return e
}

type deliveryOneConnListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
	address     net.Addr
}

func (l *deliveryOneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *deliveryOneConnListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *deliveryOneConnListener) Addr() net.Addr { return l.address }

// These controlled faults move one owned row outside H, then restore its exact
// bytes. They do not impersonate a business producer or alter the committed H.
func deliveryFix1Digest(t *testing.T, f integrationFixFixture) [32]byte {
	t.Helper()
	all := []string{}
	for _, table := range []string{"tasks", "runtime_tasks", "events", "runtime_events", "approved_results", "commit_receipts", "integration_notifications", "outbox", "delivery_streams", "delivery_updates", "harness_turn_commits"} {
		rows, e := f.r.Store.DB.Query("SELECT * FROM " + table)
		if e != nil {
			t.Fatal(e)
		}
		cols, e := rows.Columns()
		if e != nil {
			rows.Close()
			t.Fatal(e)
		}
		for rows.Next() {
			values := make([][]byte, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if e = rows.Scan(dest...); e != nil {
				rows.Close()
				t.Fatal(e)
			}
			b, e := json.Marshal(values)
			if e != nil {
				rows.Close()
				t.Fatal(e)
			}
			all = append(all, table+string(b))
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	sort.Strings(all)
	b, e := json.Marshal(all)
	if e != nil {
		t.Fatal(e)
	}
	return sha256.Sum256(b)
}
func deliveryFix1MissingRows(t *testing.T) {
	f := integrationFixSetup(t, true)
	f.control(t, f.client, "PAUSE", "1", "gap-pause")
	f.control(t, f.client, "RESUME", "2", "gap-resume")
	first, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 1)
	if e != nil || len(first.Events) != 1 {
		t.Fatal("lawful first", e)
	}
	retainSessionScanMaterial(t, first.NextCursor)
	second, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, first.NextCursor, 1)
	if e != nil || len(second.Events) != 1 {
		t.Fatal("lawful second", e)
	}
	retainSessionScanMaterial(t, second.NextCursor)
	original := deliveryFix1Digest(t, f)
	for _, tc := range []struct {
		name, cursor string
		seq          int
	}{{"first", "", 1}, {"middle", first.NextCursor, 2}, {"tail", second.NextCursor, 3}, {"tail-after-prefix", "", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			good, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, tc.cursor, 50)
			if e != nil || len(good.Events) == 0 {
				t.Fatal("positive before fault", e)
			}
			retainSessionScanMaterial(t, good.NextCursor)
			result, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET update_seq=4 WHERE world_instance_id=? AND update_seq=?", f.world.WorldInstanceID, tc.seq)
			if e != nil {
				t.Fatal("fault prerequisite", e)
			}
			n, e := result.RowsAffected()
			if e != nil || n != 1 {
				t.Fatal("fault row count", n, e)
			}
			defer func() {
				result, e := f.r.Store.DB.Exec("UPDATE delivery_updates SET update_seq=? WHERE world_instance_id=? AND update_seq=4", tc.seq, f.world.WorldInstanceID)
				if e != nil {
					t.Fatal("restore", e)
				}
				n, e := result.RowsAffected()
				if e != nil || n != 1 {
					t.Fatal("restore count", n, e)
				}
				if deliveryFix1Digest(t, f) != original {
					t.Fatal("original rows changed")
				}
				p, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, tc.cursor, 50)
				if e != nil || len(p.Events) == 0 {
					t.Fatal("restored positive", e)
				}
				retainSessionScanMaterial(t, p.NextCursor)
			}()
			p, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, tc.cursor, 50)
			retainSessionScanMaterial(t, p.NextCursor)
			if e == nil || len(p.Events)+len(p.Deliveries) != 0 {
				t.Fatal("committed complete-stream gap escaped or became EOF")
			}
		})
	}
}

// Full legacy shape is authenticated by the existing CONFIG-derived key. A
// new legitimate CLIENT Session supplies both its binding and HTTP identity.
func deliveryFix1LegacyHTTP(t *testing.T) {
	f := integrationFixSetup(t, true)
	consentClient := f.client
	session, e := f.r.Store.ExchangeIntegrationSession(f.service, "delivery-legacy-http", "integration_r0_v1", generated.ClientSessionRequest{ProductID: "coffee", WorldInstanceID: f.world.WorldInstanceID, ExternalSubjectID: "S66-player-external", EntitlementReference: "s66-entitlement-one", RequestedActions: []string{"READ"}})
	if e != nil {
		t.Fatal("real legacy-bound Session", e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	f.client, _, e = f.r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	page, e := f.r.Delivery.GetEventsPage(f.client, f.world.WorldInstanceID, "", 50)
	if e != nil {
		t.Fatal("current positive", e)
	}
	retainSessionScanMaterial(t, page.NextCursor)
	decode, encode := deliveryCursorCodec(t, f)
	legacy := func() map[string]any {
		v := decode(page.NextCursor)
		v["Version"] = 1
		delete(v, "ItemOffset")
		delete(v, "PartialUpdateID")
		delete(v, "PartialManifestHash")
		return v
	}
	server := httptest.NewServer(f.r.Handler())
	defer server.Close()
	request := func(t *testing.T, route, token string, want int) {
		t.Helper()
		req, e := http.NewRequest("GET", server.URL+"/world/v1/instances/"+f.world.WorldInstanceID+route+"?cursor="+token, nil)
		if e != nil {
			t.Fatal("request construction")
		}
		req.Header.Set("Authorization", "Bearer "+session.AccessToken)
		req.Header.Set("Loreviva-Client-Contract", "integration_r0_v1")
		client := *server.Client()
		client.Timeout = 10 * time.Second
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal("request transport")
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil {
			t.Fatal("response read")
		}
		if resp.StatusCode != want {
			t.Fatalf("legacy route status %d want %d", resp.StatusCode, want)
		}
		if want == 409 {
			var v map[string]any
			if json.Unmarshal(body, &v) != nil || generated.Validate("Error", body) != nil || v["code"] != "VIEW_RESET" || v["recovery_action"] != "FETCH_NEW_SNAPSHOT" || v["retryable"] != false {
				t.Fatal("closed reset error")
			}
		}
	}
	request(t, "/events", page.NextCursor, 200)
	before := deliveryFix1Digest(t, f)
	token := encode(legacy())
	for _, route := range []string{"/events", "/events/stream"} {
		t.Run(route, func(t *testing.T) { request(t, route, token, 409) })
	}
	for _, name := range []string{"unknown-version", "expired", "foreign-player", "foreign-session", "partial-field", "missing-position", "missing-binding-field", "bad-aead"} {
		t.Run(name, func(t *testing.T) {
			v := legacy()
			switch name {
			case "unknown-version":
				v["Version"] = 3
			case "expired":
				issued := time.Now().UTC().Add(-16 * time.Minute)
				v["Issued"] = issued
				v["Expires"] = issued.Add(15 * time.Minute)
			case "foreign-player":
				v["Binding"].(map[string]any)["Player"] = "foreign-player"
			case "foreign-session":
				v["Binding"].(map[string]any)["Session"] = "foreign-session"
			case "partial-field":
				v["ItemOffset"] = 0
			case "missing-position":
				delete(v, "Position")
			case "missing-binding-field":
				delete(v["Binding"].(map[string]any), "WorldView")
			}
			bad := encode(v)
			if name == "bad-aead" {
				b, _ := base64.RawURLEncoding.DecodeString(bad)
				b[len(b)-1] ^= 1
				bad = base64.RawURLEncoding.EncodeToString(b)
				retainSessionScanMaterial(t, bad)
			}
			request(t, "/events", bad, 403)
		})
	}
	if deliveryFix1Digest(t, f) != before {
		t.Fatal("reset replay changed business rows")
	}
	// A formerly valid legacy record must not reset past a current consent
	// change. This is a real owning operation, not a forged cursor permission.
	choice, e := f.r.Store.ReadPlayerSelfConsent(consentClient)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]string{"command": "RENEW", "expected_revision": string(choice.ConsentRevision), "policy_id": "R0_PLAYER_PRIVATE_GENERATION_30D"})
	if _, e = f.r.Store.ApplyPlayerSelfConsent(consentClient, "legacy-current-renew", raw); e != nil {
		t.Fatal("real current choice change", e)
	}
	request(t, "/events", token, 403)
}
