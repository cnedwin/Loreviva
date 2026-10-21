package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestIntegrationCreateHTTPServiceOnly(t *testing.T) {
	r, _, _, request, _ := integrationCreationFixture(t)
	raw, _ := json.Marshal(request)
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {runtimeToken, 401}, {"synthetic-s66-create-service-secret-32", 201}} {
		q := httptest.NewRequest("POST", "/world/v1/instances", bytes.NewReader(raw))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Authorization", "Bearer "+tc.token)
		q.Header.Set("Idempotency-Key", "s66-http")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		if w.Code != tc.status {
			t.Fatalf("SERVICE route status %d want %d: %s", w.Code, tc.status, w.Body.String())
		}
		if w.Code == 201 {
			var out generated.Instance
			if e := generated.Decode("Instance", w.Body.Bytes(), &out); e != nil || out.InstanceState != "CREATED" {
				t.Fatal(out, e)
			}
		}
	}
}
func integrationWorldRows(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	var count int
	e := s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM world_controls WHERE world_instance_id=?)+(SELECT COUNT(*) FROM runtime_instances WHERE world_instance_id=?)+(SELECT COUNT(*) FROM actors WHERE world_instance_id=?)+(SELECT COUNT(*) FROM items WHERE world_instance_id=?)+(SELECT COUNT(*) FROM memory_aggregates WHERE world_instance_id=?)+(SELECT COUNT(*) FROM integration_world_participants WHERE world_instance_id=?)+(SELECT COUNT(*) FROM delivery_streams WHERE world_instance_id=?)+(SELECT COUNT(*) FROM commit_receipts WHERE commit_key=?)+(SELECT COUNT(*) FROM admin_results WHERE result_id=?)`, id, id, id, id, id, id, id, id, id).Scan(&count)
	if e != nil {
		t.Fatal(e)
	}
	return count
}
func TestIntegrationCreateOriginalKeyAndSealedContext(t *testing.T) {
	r, operator, ctx, request, _ := integrationCreationFixture(t)
	for _, c := range []context.Context{context.Background(), operator} {
		if _, e := r.World.CreateIntegrationInstance(c, "s66-missing", request); e == nil {
			t.Fatal("unproved caller accepted")
		}
	}
	fake := &store.IntegrationCreationPlan{}
	if _, e := r.Store.RunIntegrationCreation(ctx, fake); e == nil {
		t.Fatal("zero seal accepted")
	}
	p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-original", request)
	if e != nil {
		t.Fatal(e)
	}
	other := store.New(r.Store.DB)
	if _, e = other.RunIntegrationCreation(ctx, p); e == nil {
		t.Fatal("foreign Store accepted")
	}
	copy := p.Release()
	copy.Input.Objects = nil
	scope := p.Scope()
	scope.SubjectID = "creator"
	out, e := r.Store.RunIntegrationCreation(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RunIntegrationCreation(ctx, p); e == nil {
		t.Fatal("consumed plan reused")
	}
	before := integrationWorldRows(t, r.Store, out.Scope.WorldInstanceID)
	replay, e := r.World.CreateIntegrationInstance(ctx, "s66-original", request)
	if e != nil || replay != out {
		t.Fatal("original replay", e)
	}
	request.ClientCapabilities = []string{"TEXT", "AUDIO"}
	if _, e = r.World.CreateIntegrationInstance(ctx, "s66-original", request); !errors.Is(e, store.ErrConflict) {
		t.Fatal("changed payload", e)
	}
	if integrationWorldRows(t, r.Store, out.Scope.WorldInstanceID) != before {
		t.Fatal("replay changed rows")
	}
	if e = r.World.BindIntegrationCreation(r.Memory); e == nil {
		t.Fatal("owner replacement allowed")
	}
}
func TestIntegrationCreateCommitUnknownAndFinalGate(t *testing.T) {
	for _, mode := range []string{"commit_ack", "final_participant", "final_baseline", "final_source"} {
		t.Run(mode, func(t *testing.T) {
			r, _, ctx, req, _ := integrationCreationFixture(t)
			key := "s66-" + mode
			id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", key)
			if mode == "commit_ack" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic ACK lost after real commit")
				}
			} else {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					switch mode {
					case "final_participant":
						_, e := tx.ExecContext(c, "UPDATE integration_world_participants SET player_subject_id='wrong-player' WHERE world_instance_id=?", id)
						return e
					case "final_baseline":
						_, e := tx.ExecContext(c, "UPDATE delivery_streams SET initialization_receipt='wrong-receipt' WHERE world_instance_id=?", id)
						return e
					default:
						_, e := tx.ExecContext(c, "UPDATE releases SET available=0 WHERE release_id=?", req.ReleaseID)
						return e
					}
				}
			}
			_, e := r.World.CreateIntegrationInstance(ctx, key, req)
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if mode == "commit_ack" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal("not actual UNKNOWN", e)
				}
				out, e := r.World.CreateIntegrationInstance(ctx, key, req)
				if e != nil || out.Scope.WorldInstanceID != id {
					t.Fatal("original recovery", out, e)
				}
			} else {
				if e == nil || integrationWorldRows(t, r.Store, id) != 0 {
					t.Fatal("final gate failed rollback", e)
				}
				if _, e = r.World.CreateIntegrationInstance(ctx, key, req); e != nil {
					t.Fatal("rolled-back original key", e)
				}
			}
		})
	}
}
func TestIntegrationCreateCurrentEntitlementAndCompetition(t *testing.T) {
	t.Run("current_disable", func(t *testing.T) {
		r, op, ctx, req, _ := integrationCreationFixture(t)
		p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-disable", req)
		if e != nil {
			t.Fatal(e)
		}
		cmd := domain.RegistryCommand{Kind: "DISABLE_ENTITLEMENT", ProductID: "coffee", QualifiedTarget: &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}, TargetID: "s66-entitlement-one", ExpectedRevision: 1}
		if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-disable-entitlement", cmd); e != nil {
			t.Fatal("real disable", e)
		}
		if _, e = r.Store.RunIntegrationCreation(ctx, p); e == nil || integrationWorldRows(t, r.Store, p.WorldID()) != 0 {
			t.Fatal("stale precollect accepted", e)
		}
	})
	t.Run("two_keys", func(t *testing.T) {
		r, _, ctx, req, _ := integrationCreationFixture(t)
		plans := make([]*store.IntegrationCreationPlan, 2)
		for i, k := range []string{"s66-race-one", "s66-race-two"} {
			var e error
			plans[i], e = r.Store.PrepareIntegrationCreation(ctx, k, req)
			if e != nil {
				t.Fatal(e)
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range plans {
			wg.Add(1)
			go func(i int) { defer wg.Done(); _, errs[i] = r.Store.RunIntegrationCreation(ctx, plans[i]) }(i)
		}
		wg.Wait()
		pass := 0
		for i, e := range errs {
			if e == nil {
				pass++
			} else if integrationWorldRows(t, r.Store, plans[i].WorldID()) != 0 {
				t.Fatal("loser partial rows", e)
			}
		}
		if pass != 1 {
			t.Fatal("exclusive entitlement", errs)
		}
	})
}
func TestIntegrationRegistryTwoWorldsAndAmbiguity(t *testing.T) {
	r, op, ctx, req, cmd := integrationCreationFixture(t)
	var before []byte
	if e := r.Store.DB.QueryRow("SELECT CAST(JSON_OBJECT('expiry',expires_at,'revision',revision,'receipt',registry_receipt,'external',HEX(external_id),'player',player_subject_id) AS BINARY) FROM integration_external_subjects WHERE customer_id='s66-customer'").Scan(&before); e != nil {
		t.Fatal(e)
	}
	first, e := r.World.CreateIntegrationInstance(ctx, "s66-first-world", req)
	if e != nil {
		t.Fatal(e)
	}
	v := *cmd.Player
	v.EntitlementID = "s66-second"
	cmd.Player = &v
	if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-second-access", cmd); e != nil {
		t.Fatal(e)
	}
	second, e := r.World.CreateIntegrationInstance(ctx, "s66-second-world", req)
	if e != nil || second.Scope.WorldInstanceID == first.Scope.WorldInstanceID {
		t.Fatal(e)
	}
	var after []byte
	_ = r.Store.DB.QueryRow("SELECT CAST(JSON_OBJECT('expiry',expires_at,'revision',revision,'receipt',registry_receipt,'external',HEX(external_id),'player',player_subject_id) AS BINARY) FROM integration_external_subjects WHERE customer_id='s66-customer'").Scan(&after)
	if !bytes.Equal(before, after) {
		t.Fatal("mapping changed")
	}
	for _, id := range []string{"s66-third", "s66-fourth"} {
		v.EntitlementID = id
		cmd.Player = &v
		if _, e = r.Store.ApplyIntegrationRegistry(op, id, cmd); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = r.World.CreateIntegrationInstance(ctx, "s66-ambiguous", req); !errors.Is(e, store.ErrConflict) {
		t.Fatal("ambiguity silently selected", e)
	}
	altered := *cmd.Player
	altered.Actions = []string{"READ", "MEMORY_VIEW"}
	altered.EntitlementID = "s66-new-permission"
	cmd.Player = &altered
	if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-illegal-permission", cmd); e == nil {
		t.Fatal("append minted missing self permission")
	}
	var permissions int
	_ = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='s66-player'").Scan(&permissions)
	if permissions != 0 {
		t.Fatal("permissions changed", permissions)
	}
	if !reflect.DeepEqual(req.InitialParticipants, []string{"s66-player"}) {
		t.Fatal("test replaced player")
	}
}
