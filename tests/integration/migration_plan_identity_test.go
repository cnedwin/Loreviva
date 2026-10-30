package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/migrations"
)

// Independent identity construction uses the actual returned whole ordered
// collection. It does not call the implementation's private identity helper.
func completePlanDigest(t *testing.T, p maintenance.Plan, limit int) string {
	t.Helper()
	hashes := []string{}
	for _, m := range p.Migrations[:limit] {
		hashes = append(hashes, m.Hash)
	}
	raw, e := json.Marshal(struct {
		Database string
		Hashes   []string
	}{p.Database, hashes})
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestMigrationPlanBindsEveryPinnedSource(t *testing.T) {
	r, _, _ := runtimeFixtureWithConsent(t, true)
	ctx := runtimeContext(t, r)
	p, e := maintenance.MigrationPlan(ctx, r.Store)
	if e != nil {
		t.Fatal(e)
	}
	sources := []string{migrations.Core, migrations.Content, migrations.FileCleanup, migrations.Runtime, migrations.MemoryGovernance, migrations.TurnAttempt, migrations.StudioSettings, migrations.DeliveryIdentity, migrations.PackageImport, migrations.PackageTransfer}
	if len(p.Migrations) != len(sources) {
		t.Fatal("actual pinned migration omitted", len(p.Migrations))
	}
	for i, raw := range sources {
		hash := sha256.Sum256([]byte(raw))
		m := p.Migrations[i]
		if m.Version != i+1 || m.Hash != hex.EncodeToString(hash[:]) || !m.Applied {
			t.Fatal("actual raw source or applied marker differs", i, m)
		}
	}
	expected := completePlanDigest(t, p, len(p.Migrations))
	if p.PlanID != expected {
		t.Fatalf("actual ten-migration plan identity omits a source: got %s expected %s", p.PlanID, expected)
	}
	legacy := completePlanDigest(t, p, len(p.Migrations)-1)
	if legacy == p.PlanID {
		t.Fatal("last actual migration does not bind identity")
	}
	t.Run("old incomplete confirmation rejected", func(t *testing.T) {
		if e := maintenance.Up(ctx, r.Store, legacy); e == nil {
			t.Fatal("old partial confirmation upgraded schema")
		}
	})
	t.Run("current already applied plan is noop", func(t *testing.T) {
		if e := maintenance.Up(ctx, r.Store, p.PlanID); e != nil {
			t.Fatal(e)
		}
		again, e := maintenance.MigrationPlan(ctx, r.Store)
		if e != nil || again.PlanID != p.PlanID {
			t.Fatal("current no-op changed plan identity", e)
		}
	})
	// This is the missing-marker policy on an owned synthetic fixture, not proof
	// of a full fresh install, data upgrade or paired restore.
	t.Run("known missing marker table remains plannable", func(t *testing.T) {
		if _, e := r.Store.DB.ExecContext(ctx, "DROP TABLE schema_migrations"); e != nil {
			t.Fatal(e)
		}
		fresh, e := maintenance.MigrationPlan(ctx, r.Store)
		if e != nil || fresh.PlanID != p.PlanID || len(fresh.Migrations) != len(sources) {
			t.Fatal("known missing table plan differs", e)
		}
		for _, m := range fresh.Migrations {
			if m.Applied {
				t.Fatal("missing marker invented applied state")
			}
		}
	})
}
