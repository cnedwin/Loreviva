package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/migrations"
	"strings"
	"unicode/utf8"
)

type Migration struct {
	Version int    `json:"version"`
	Hash    string `json:"sha256"`
	Applied bool   `json:"applied"`
}
type Plan struct {
	PlanID      string      `json:"plan_id"`
	Database    string      `json:"database"`
	Migrations  []Migration `json:"migrations"`
	Destructive bool        `json:"destructive"`
}

// Bind the plan ID to the database name and all pinned migration hashes. Up recomputes this identity before delegating migration; compatibility checks applied markers without applying a plan.
func MigrationPlan(ctx context.Context, s *store.Store) (Plan, error) {
	p := Plan{Migrations: []Migration{}}
	if e := s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&p.Database); e != nil {
		return p, errors.New("database unavailable")
	}
	for i, source := range []string{migrations.Core, migrations.Content, migrations.FileCleanup, migrations.Runtime, migrations.MemoryGovernance, migrations.TurnAttempt, migrations.StudioSettings, migrations.DeliveryIdentity, migrations.PackageImport, migrations.PackageTransfer} {
		h := sha256.Sum256([]byte(source))
		m := Migration{Version: i + 1, Hash: hex.EncodeToString(h[:])}
		var got []byte
		e := s.DB.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version=?", i+1).Scan(&got)
		if e == nil {
			if string(got) != string(h[:]) {
				return p, errors.New("pinned migration bytes differ from applied schema")
			}
			m.Applied = true
		} else if !errors.Is(e, sql.ErrNoRows) {
			var my *driver.MySQLError
			if !errors.As(e, &my) || my.Number != 1146 {
				return p, errors.New("schema status unavailable")
			}
		}
		p.Migrations = append(p.Migrations, m)
	}
	var n int
	e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version NOT IN (1,2,3,4,5,6,7,8,9,10)").Scan(&n)
	if e != nil && !migrationTableMissing(e) {
		return p, errors.New("schema status unavailable")
	}
	if e == nil && n != 0 {
		return p, errors.New("schema newer than this application")
	}
	p.PlanID, e = migrationPlanIdentity(p.Database, p.Migrations)
	if e != nil {
		return p, e
	}
	return p, nil
}
func Up(ctx context.Context, s *store.Store, id string) error {
	p, e := MigrationPlan(ctx, s)
	if e != nil {
		return e
	}
	if id == "" || id != p.PlanID {
		return errors.New("explicit current pinned migration plan_id required")
	}
	return s.Migrate(ctx)
}
func Compatible(ctx context.Context, s *store.Store) error {
	p, e := MigrationPlan(ctx, s)
	if e != nil {
		return e
	}
	for _, m := range p.Migrations {
		if !m.Applied {
			return errors.New("required migration not applied")
		}
	}
	return nil
}

// A missing or lagging watermark is not ready. Only a fresh synthetic installation may use a zero required watermark; restored installations need an independently established governance boundary.
func GovernanceReady(ctx context.Context, s *store.Store) error {
	var applied, required uint64
	var fresh bool
	e := s.DB.QueryRowContext(ctx, "SELECT applied_watermark,required_watermark,fresh_synthetic FROM governance_watermark WHERE singleton=1").Scan(&applied, &required, &fresh)
	if e != nil || applied < required || (!fresh && required == 0) {
		return errors.New("initial governance watermark unavailable; restore requires independent governance gate")
	}
	return nil
}

// The existing confirmation representation binds the database and every pinned
// hash in order. Versions must agree with that sequence; Applied is progress,
// not new content. No caller may confirm only an old hardcoded prefix.
func migrationPlanIdentity(database string, items []Migration) (string, error) {
	if database == "" || !utf8.ValidString(database) || len(items) == 0 {
		return "", errors.New("complete pinned migration plan required")
	}
	hashes := make([]string, len(items))
	for i, m := range items {
		b, e := hex.DecodeString(m.Hash)
		if m.Version != i+1 || e != nil || len(b) != sha256.Size || m.Hash != strings.ToLower(m.Hash) {
			return "", errors.New("ordered pinned migration identity invalid")
		}
		hashes[i] = m.Hash
	}
	identity := struct {
		Database string
		Hashes   []string
	}{database, hashes}
	raw, e := json.Marshal(identity)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// A known absent marker table is supported for planning an uninitialized
// installation. Other status failures must not produce a confirmable plan ID.
func migrationTableMissing(e error) bool {
	var my *driver.MySQLError
	return errors.As(e, &my) && my.Number == 1146
}
