package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func TestIntegrationCreateQualifiedKnowledgeAndPrivateIsolation(t *testing.T) {
	var qualifiedRelease string
	r, op, ctx, req, _ := integrationCreationFixturePrepared(t, func(r *app.Runtime, op context.Context, in *generated.DraftInput) {
		createPolicyInput(t, "owned")(in)
		scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
		var char, note json.RawMessage
		for _, raw := range in.Objects {
			var o map[string]any
			_ = json.Unmarshal(raw, &o)
			if o["object_id"] == "alex-character" {
				o["initial_knowledge_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note"}}
				char, _ = json.Marshal(o)
			}
			if o["object_id"] == "s66-note" {
				o["text"] = "Qualified character knowledge stays in its own release."
				note, _ = json.Marshal(o)
			}
		}
		dep := *in
		dep.RootObjectID = "alex-character"
		dep.Objects = []generated.ContentObject{char, note}
		dep.DependencyLocks = []generated.DependencyLock{}
		draft, e := r.Content.Create(op, scope, "CHARACTER", "", dep, "s66-qualified-char")
		if e != nil {
			t.Fatal(e)
		}
		report, e := r.Content.CheckPublish(op, scope, string(draft.DraftID), 1, []string{"MOCK_DEMO"}, "s66-qualified-check")
		if e != nil || !report.Passed {
			t.Fatal(report, e)
		}
		rel, e := r.Content.Publish(op, scope, string(draft.DraftID), 1, string(report.ReportID), []string{"MOCK_DEMO"}, "s66-qualified-publish")
		if e != nil {
			t.Fatal(e)
		}
		qualifiedRelease = string(rel.ReleaseID)
		hash, e := content.Hash(char)
		if e != nil {
			t.Fatal(e)
		}
		in.DependencyLocks = append(in.DependencyLocks, generated.DependencyLock{ReleaseID: rel.ReleaseID, ObjectID: "alex-character", Version: "1", ContentHash: hash})
		for i, raw := range in.Objects {
			var o map[string]any
			_ = json.Unmarshal(raw, &o)
			if o["kind"] == "WORLD" {
				o["initial_actor_setups"].([]any)[0].(map[string]any)["character_ref"] = map[string]any{"kind": "PINNED_DEFINITION", "release_id": rel.ReleaseID, "object_id": "alex-character", "version": "1", "content_hash": hash}
				in.Objects[i], _ = json.Marshal(o)
			}
		}
	}, time.Hour)
	// Seed an actual private creator statement through the existing owning API;
	// no SQL-created memory permission, participant, source or positive use.
	prior := createRuntimeWorld(t, r, req.ReleaseID, "s66-creator-private-world")
	// Only the old Admin-owned fixture is started, as required by its existing
	// memory command barrier. The new integration instance stays CREATED.
	prior = runtimeControl(t, r, prior, "START")
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var actor string
	if e := r.Store.DB.QueryRow("SELECT actor_id FROM actors WHERE world_instance_id=? ORDER BY actor_id LIMIT 1", prior.Scope.WorldInstanceID).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	ns := memory.Namespace{Scope: domain.Scope{ProductScope: scope, WorldInstanceID: prior.Scope.WorldInstanceID}, DataSubjectID: "creator"}
	const privateBody = "Creator-only private experience must never be initial player knowledge."
	if e := r.Memory.RecordStatement(op, ns, "s66-private-creator", "s66-private-intake", actor, privateBody, []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal("real private statement prerequisite", e)
	}
	var before []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", prior.Scope.WorldInstanceID).Scan(&before); e != nil || !bytes.Contains(before, []byte(privateBody)) {
		t.Fatal("private evidence prerequisite", e)
	}
	out, e := r.World.CreateIntegrationInstance(ctx, "s66-qualified-create", req)
	if e != nil {
		t.Fatal(e)
	}
	var raw, init, after []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='s66-player'", out.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(privateBody)) || !bytes.Contains(raw, []byte("Qualified character knowledge stays in its own release.")) || !bytes.Contains(raw, []byte("The fixture library opens at sunrise.")) {
		t.Fatal("qualified knowledge/private isolation")
	}
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", out.Scope.WorldInstanceID).Scan(&init); e != nil || !bytes.Contains(init, []byte(qualifiedRelease)) {
		t.Fatal("qualified lineage", e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", prior.Scope.WorldInstanceID).Scan(&after); e != nil || !bytes.Equal(before, after) {
		t.Fatal("original private history changed", e)
	}
	var use int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE data_subject_id='s66-player' AND action='memory.generation.use'").Scan(&use); e != nil || use != 0 {
		t.Fatal("positive use minted", e)
	}
	// A SERVICE context and an unrelated real Operator cannot become a player
	// merely by choosing a Namespace. CLIENT self reads remain Slice3 NOT_RUN.
	playerNS := memory.Namespace{Scope: domain.Scope{ProductScope: domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "s66-player"}, WorldInstanceID: out.Scope.WorldInstanceID}, DataSubjectID: "s66-player"}
	for _, wrong := range []context.Context{ctx, op} {
		if e = r.Memory.RecordStatement(wrong, playerNS, "forged", "forged", actor, "no", []memory.Use{memory.PrivateResponse}); e == nil {
			t.Fatal("context/namespace gained private self authority")
		}
	}
}

func TestIntegrationCreatePublicationProofAndPrivateStateFaults(t *testing.T) {
	for _, mode := range []string{"missing_publish_receipt", "pinned_document", "extra_namespace"} {
		t.Run(mode, func(t *testing.T) {
			r, _, ctx, req, _ := integrationCreationFixture(t)
			plan, e := r.Store.PrepareIntegrationCreation(ctx, "s66-proof-fault", req)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "missing_publish_receipt" {
				// Deliberate integrity fault, not a publication-governance owner API.
				result := content.Identity("demo-local", "coffee", "creator", "publishContentDraft", "s66-publish")
				v, e := r.Store.DB.Exec("DELETE FROM commit_receipts WHERE commit_key=?", result)
				if e != nil {
					t.Fatal(e)
				}
				n, _ := v.RowsAffected()
				if n != 1 {
					t.Fatal("fault did not reach real publish receipt", n)
				}
			} else if mode == "pinned_document" {
				v, e := r.Store.DB.Exec("UPDATE content_releases SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.content_hash',?) WHERE release_id=?", "0000000000000000000000000000000000000000000000000000000000000000", req.ReleaseID)
				if e != nil {
					t.Fatal(e)
				}
				n, _ := v.RowsAffected()
				if n != 1 {
					t.Fatal("publication integrity fault", n)
				}
			} else {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					_, e := tx.ExecContext(c, "INSERT INTO memory_aggregates(tenant_id,product_id,world_instance_id,data_subject_id,aggregate_id,revision,document) SELECT tenant_id,product_id,world_instance_id,'wrong-private-subject','wrong-private-aggregate',revision,document FROM memory_aggregates WHERE world_instance_id=?", plan.WorldID())
					return e
				}
			}
			_, e = r.Store.RunIntegrationCreation(ctx, plan)
			r.Store.BeforeCommit = nil
			if e == nil || integrationWorldRows(t, r.Store, plan.WorldID()) != 0 {
				t.Fatal("integrity fault not atomically rejected", e)
			}
			t.Log("explicit fixture integrity fault; no real release-disable owner API claimed")
		})
	}
}

func TestIntegrationCreationForeignTransactionSeal(t *testing.T) {
	r, op, ctx, req, _ := integrationCreationFixture(t)
	p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-seal", req)
	if e != nil {
		t.Fatal(e)
	}
	gate, e := content.Gate(op, r.Store, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"})
	if e != nil {
		t.Fatal(e)
	}
	reached := false
	_, e = r.Store.WithCommandBarrier(op, store.CommandBarrierRequest{ProductScope: domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, OwnerSubjectID: "creator", Operation: "createContentDraft", CommandKey: "s66-foreign-tx", CommitKey: content.Identity("s66-foreign-tx"), GateVersions: gate}, func(tx store.Tx) error {
		reached = true
		if r.Store.CheckIntegrationCreationTx(ctx, tx, p) == nil {
			t.Fatal("ordinary genuine Tx acquired creation seal")
		}
		if _, e := r.Content.InspectIntegrationInitialSourcesTx(ctx, tx, p); e == nil {
			t.Fatal("copied getter entered source owner")
		}
		return store.ErrDenied
	})
	if !reached || e == nil {
		t.Fatal("foreign Tx prerequisite/denial", reached, e)
	}
	if _, e = r.Store.RunIntegrationCreation(ctx, p); e != nil {
		t.Fatal("denied foreign Tx consumed real plan", e)
	}
}

func TestIntegrationCreateFirstReadbackRechecksCurrentRegistry(t *testing.T) {
	r, op, ctx, req, _ := integrationCreationFixture(t)
	committed, revoked := false, false
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		committed = true
		// The real creation Tx has ended. This precise fixture schedules the
		// real registry owner before first readback, without a SQL fake revoke.
		r.Store.Commit = nil
		_, e := r.Store.ApplyIntegrationRegistry(op, "s66-before-first-read", domain.RegistryCommand{Kind: "DISABLE_ENTITLEMENT", ProductID: "coffee", TargetID: "s66-entitlement-one", QualifiedTarget: &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}, ExpectedRevision: 2})
		revoked = e == nil
		return e
	}
	_, e := r.World.CreateIntegrationInstance(ctx, "s66-first-read-revoke", req)
	r.Store.Commit = nil
	if e == nil || !committed || !revoked {
		t.Fatal("first readback did not check current real revocation", committed, revoked, e)
	}
	id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", "s66-first-read-revoke")
	if integrationWorldRows(t, r.Store, id) == 0 {
		t.Fatal("later revocation rolled back committed creation")
	}
	if _, e = r.World.CreateIntegrationInstance(ctx, "s66-first-read-revoke", req); e == nil {
		t.Fatal("original receipt replay bypassed revocation")
	}
}
