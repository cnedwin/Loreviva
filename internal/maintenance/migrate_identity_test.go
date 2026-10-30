package maintenance

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	driver "github.com/go-sql-driver/mysql"
)

func identityFixture() []Migration {
	out := []Migration{}
	for i := 1; i <= 10; i++ {
		out = append(out, Migration{Version: i, Hash: fmt.Sprintf("%064x", i), Applied: true})
	}
	return out
}

func TestMigrationIdentityBindsCompleteOrderedCollection(t *testing.T) {
	items := identityFixture()
	base, e := migrationPlanIdentity("owned_database", items)
	if e != nil {
		t.Fatal(e)
	}
	for i := range items {
		changed := append([]Migration(nil), items...)
		changed[i].Hash = strings.Repeat("a", 64)
		id, e := migrationPlanIdentity("owned_database", changed)
		if e != nil || id == base {
			t.Fatal("migration hash omitted", i, e)
		}
	}
	id, e := migrationPlanIdentity("another_database", items)
	if e != nil || id == base {
		t.Fatal("database identity omitted", e)
	}
	changed := append([]Migration(nil), items...)
	changed[0].Hash, changed[9].Hash = changed[9].Hash, changed[0].Hash
	id, e = migrationPlanIdentity("owned_database", changed)
	if e != nil || id == base {
		t.Fatal("hash order omitted", e)
	}
	id, e = migrationPlanIdentity("owned_database", items[:9])
	if e != nil || id == base {
		t.Fatal("removed last migration omitted", e)
	}
	added := append(append([]Migration(nil), items...), Migration{Version: 11, Hash: strings.Repeat("b", 64)})
	id, e = migrationPlanIdentity("owned_database", added)
	if e != nil || id == base {
		t.Fatal("future appended migration omitted", e)
	}
}
func TestMigrationIdentityAppliedProgressDoesNotChangeContent(t *testing.T) {
	items := identityFixture()
	base, e := migrationPlanIdentity("owned_database", items)
	if e != nil {
		t.Fatal(e)
	}
	for i := range items {
		items[i].Applied = false
	}
	id, e := migrationPlanIdentity("owned_database", items)
	if e != nil || id != base {
		t.Fatal("application progress changed confirmation", e)
	}
}
func TestMigrationIdentityRejectsPartialOrMalformedSequence(t *testing.T) {
	for name, change := range map[string]func([]Migration) []Migration{"empty": func(v []Migration) []Migration { return nil }, "version gap": func(v []Migration) []Migration { v[9].Version = 11; return v }, "reordered version": func(v []Migration) []Migration { v[0], v[1] = v[1], v[0]; return v }, "missing hash": func(v []Migration) []Migration { v[9].Hash = ""; return v }, "uppercase hash": func(v []Migration) []Migration { v[9].Hash = strings.Repeat("A", 64); return v }, "nonhash": func(v []Migration) []Migration { v[9].Hash = strings.Repeat("g", 64); return v }} {
		t.Run(name, func(t *testing.T) {
			if id, e := migrationPlanIdentity("owned_database", change(identityFixture())); e == nil || id != "" {
				t.Fatal("invalid sequence acquired ID")
			}
		})
	}
	if id, e := migrationPlanIdentity("", identityFixture()); e == nil || id != "" {
		t.Fatal("missing database acquired ID")
	}
}
func TestMigrationIdentityDistinguishesKnownMissingStatus(t *testing.T) {
	for _, e := range []error{nil, errors.New("unavailable"), &driver.MySQLError{Number: 1044}, &driver.MySQLError{Number: 1142}} {
		if migrationTableMissing(e) {
			t.Fatal("unknown failure treated as fresh table", e)
		}
	}
	if !migrationTableMissing(&driver.MySQLError{Number: 1146}) || !migrationTableMissing(fmt.Errorf("wrapped: %w", &driver.MySQLError{Number: 1146})) {
		t.Fatal("known missing table blocked")
	}
}
